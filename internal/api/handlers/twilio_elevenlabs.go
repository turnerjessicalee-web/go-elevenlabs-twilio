package handlers

import (
	"caller/internal/config"
	"caller/internal/elevenlabs"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// -----------------------------------------------------------------------------
// GLOBALS / TYPES
// -----------------------------------------------------------------------------

// EchoMode:
//   - true  => just echo caller audio back (no ElevenLabs, good for testing Twilio <-> Go)
//   - false => full Twilio <-> ElevenLabs bridge
const EchoMode = false

// EnableLatencyDebug:
//   - true  => log timestamps and deltas for user->agent round-trip
//   - false => minimal logging
const EnableLatencyDebug = true

// ConversationData represents a call session
type ConversationData struct {
	StreamSid      string
	CallSid        string
	ConversationID string
	CallerPhone    string
	UserData       map[string]interface{}
	Direction      string // "inbound" or "outbound"
}

var conversations sync.Map

// -----------------------------------------------------------------------------
// INBOUND CALL HANDLER
// -----------------------------------------------------------------------------

// HandleIncomingCall processes webhook requests from Twilio when someone calls
func HandleIncomingCall() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form data", http.StatusBadRequest)
			return
		}

		callerPhone := r.FormValue("From")
		log.Printf("[Twilio] Incoming call from: %s", callerPhone)

		userData, err := checkUserExists(callerPhone)
		if err != nil || userData == nil {
			twiml := `<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Say>Sorry, you are not authorized to make this call.</Say>
    <Hangup />
</Response>`
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(twiml))
			return
		}

		userDataJSON, _ := json.Marshal(userData)

		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
	<Connect>
		<Stream url="wss://%s/media-stream">
			<Parameter name="caller_phone" value="%s" />
			<Parameter name="user_data" value="%s" />
			<Parameter name="direction" value="inbound" />
		</Stream>
	</Connect>
</Response>`,
			r.Host,
			callerPhone,
			url.QueryEscape(string(userDataJSON)),
		)

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// -----------------------------------------------------------------------------
// MEDIA STREAM HANDLER (Twilio <-> ElevenLabs bridge)
// -----------------------------------------------------------------------------

func HandleMediaStream(upgrader websocket.Upgrader, cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		twilioConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WebSocket] Error upgrading connection: %v", err)
			return
		}
		defer twilioConn.Close()

		log.Println("[Server] Twilio connected to media stream")

		var (
			streamSid       string
			callSid         string
			elevenConn      *websocket.Conn
			callerPhone     = "Unknown"
			userData        map[string]interface{}
			direction       = "inbound"
			conversation    *ConversationData
			isDisconnecting = false
			mu              sync.Mutex

			lastUserAudioTime time.Time
		)

		disconnectCall := func() {
			mu.Lock()
			if isDisconnecting {
				mu.Unlock()
				return
			}
			isDisconnecting = true
			mu.Unlock()

			log.Println("[Twilio] Initiating call disconnect")

			if elevenConn != nil {
				_ = elevenConn.WriteJSON(map[string]string{"type": "end_of_conversation"})
				_ = elevenConn.Close()
			}

			if conversation != nil && conversation.ConversationID != "" {
				payload := map[string]interface{}{
					"conversation_id": conversation.ConversationID,
					"phone_number":    conversation.CallerPhone,
					"call_sid":        conversation.CallSid,
					"direction":       conversation.Direction,
				}
				_ = sendConversationWebhook(payload, conversation.Direction+"-calls", "123123")
				conversations.Delete(streamSid)
			}

			msgs := []map[string]interface{}{
				{"event": "mark_done", "streamSid": streamSid},
				{"event": "clear", "streamSid": streamSid},
				{"event": "twiml", "streamSid": streamSid, "twiml": "<Response><Hangup/></Response>"},
			}
			for _, m := range msgs {
				if err := twilioConn.WriteJSON(m); err != nil {
					log.Printf("[Twilio] Error sending disconnect message: %v", err)
				}
			}

			time.AfterFunc(time.Second, func() {
				_ = twilioConn.Close()
			})
		}

		for {
			msgType, raw, err := twilioConn.ReadMessage()
			if err != nil {
				log.Printf("[Twilio] WebSocket error: %v", err)
				break
			}
			if msgType != websocket.TextMessage {
				continue
			}

			var data map[string]interface{}
			if err := json.Unmarshal(raw, &data); err != nil {
				log.Printf("[Twilio] Error parsing message: %v", err)
				continue
			}

			event, _ := data["event"].(string)
			if event == "" {
				continue
			}

			mu.Lock()
			localDisconnecting := isDisconnecting
			mu.Unlock()
			if localDisconnecting && event != "stop" {
				continue
			}

			switch event {
			case "start":
				start, ok := data["start"].(map[string]interface{})
				if !ok {
					log.Println("[Twilio] Invalid start event")
					continue
				}

				streamSid, _ = start["streamSid"].(string)
				if cs, ok := start["callSid"].(string); ok {
					callSid = cs
				}

				custom, _ := start["customParameters"].(map[string]interface{})
				if phone, ok := custom["caller_phone"].(string); ok {
					callerPhone = phone
				}
				if dir, ok := custom["direction"].(string); ok {
					direction = dir
				}

				if userStr, ok := custom["user_data"].(string); ok && userStr != "" {
					if decoded, err := url.QueryUnescape(userStr); err == nil {
						if err := json.Unmarshal([]byte(decoded), &userData); err != nil {
							log.Printf("[Twilio] Error parsing user_data: %v", err)
						}
					}
				}

				conversation = &ConversationData{
					StreamSid:   streamSid,
					CallSid:     callSid,
					CallerPhone: callerPhone,
					UserData:    userData,
					Direction:   direction,
				}
				conversations.Store(streamSid, conversation)

				if EchoMode {
					log.Println("[Server] EchoMode ENABLED (no ElevenLabs)")
					continue
				}

				log.Println("[Server] ElevenLabs bridge ENABLED")

				wsURL := elevenlabs.GetRealtimeURL(cfg.ElevenLabsAgentID, cfg.ElevenLabsAPIKey)
				elevenConn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
				if err != nil {
					log.Printf("[ElevenLabs] Error connecting: %v", err)
					disconnectCall()
					continue
				}
				log.Println("[ElevenLabs] WebSocket connected")

				isInbound := direction == "inbound"
				configMsg := elevenlabs.GenerateElevenLabsConfig(userData, callerPhone, isInbound)

				if err := elevenConn.WriteJSON(configMsg); err != nil {
					log.Printf("[ElevenLabs] Error sending config: %v", err)
					disconnectCall()
					continue
				}

				go handleElevenLabsMessages(elevenConn, twilioConn, conversation, disconnectCall, &lastUserAudioTime)

			case "media":
				media, ok := data["media"].(map[string]interface{})
				if !ok {
					continue
				}
				payload, _ := media["payload"].(string)
				if payload == "" {
					continue
				}

				if EnableLatencyDebug {
					lastUserAudioTime = time.Now()
					log.Printf("[Latency] TWILIO_IN media at %s (len=%d bytes base64)", lastUserAudioTime.Format(time.RFC3339Nano), len(payload))
				}

				if EchoMode {
					resp := map[string]interface{}{
						"event":     "media",
						"streamSid": streamSid,
						"media": map[string]string{
							"payload": payload,
						},
					}
					if err := twilioConn.WriteJSON(resp); err != nil {
						log.Printf("[Twilio] Error sending echo: %v", err)
					}
					continue
				}

				if elevenConn != nil {
					msg := map[string]string{
						"user_audio_chunk": payload,
					}
					if err := elevenConn.WriteJSON(msg); err != nil {
						log.Printf("[ElevenLabs] Error sending audio: %v", err)
					} else if EnableLatencyDebug {
						log.Printf("[Latency] EL_OUT user_audio_chunk at %s", time.Now().Format(time.RFC3339Nano))
					}
				}

			case "stop":
				log.Println("[Twilio] Stop event received")
				disconnectCall()
				return
			}
		}
	})
}

// -----------------------------------------------------------------------------
// OUTBOUND CALL HANDLERS
// -----------------------------------------------------------------------------

func HandleOutboundCall(cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Number    string `json:"number"`
			FirstName string `json:"first_name"`
			Email     string `json:"email"`
			Prompt    string `json:"prompt"` // optional, kept for compatibility
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Number == "" {
			http.Error(w, "Phone number is required", http.StatusBadRequest)
			return
		}

		normalizedNumber := normalizeAUNumber(req.Number)
		if normalizedNumber == "" {
			http.Error(w, "Invalid phone number", http.StatusBadRequest)
			return
		}

		// Build TwiML URL (no user_data in query; we build it in the TwiML handler)
		callURL := fmt.Sprintf(
			"https://%s/outbound-call-twiml?number=%s&first_name=%s&email=%s&prompt=%s",
			r.Host,
			url.QueryEscape(normalizedNumber),
			url.QueryEscape(req.FirstName),
			url.QueryEscape(req.Email),
			url.QueryEscape(req.Prompt),
		)

		params := map[string]string{
			"To":   normalizedNumber,
			"From": cfg.TwilioPhoneNumber,
			"Url":  callURL,
		}

		call, err := createTwilioCall(params, cfg.TwilioAccountSID, cfg.TwilioAuthToken)
		if err != nil {
			log.Printf("[Twilio] Error creating call: %v", err)
			http.Error(w, "Failed to initiate call", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Call initiated",
			"callSid": call["sid"],
		})
	})
}

func HandleOutboundCallTwiml() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompt := r.URL.Query().Get("prompt")
		number := r.URL.Query().Get("number") // already normalized
		firstName := r.URL.Query().Get("first_name")
		email := r.URL.Query().Get("email")

		// Build user_data JSON from query params (so ElevenLabs can see name/email)
		userData := map[string]interface{}{}
		if firstName != "" {
			userData["first_name"] = firstName
		}
		if email != "" {
			userData["email"] = email
		}
		userDataJSON, _ := json.Marshal(userData)
		escapedUserData := url.QueryEscape(string(userDataJSON))

		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Connect>
        <Stream url="wss://%s/media-stream">
            <Parameter name="caller_phone" value="%s" />
            <Parameter name="user_data" value="%s" />
            <Parameter name="prompt" value="%s" />
            <Parameter name="direction" value="outbound" />
        </Stream>
    </Connect>
</Response>`,
			r.Host,
			number,
			escapedUserData,
			url.QueryEscape(prompt),
		)

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// -----------------------------------------------------------------------------
// ElevenLabs → Twilio bridge (with PCM16 -> µ-law 8k)
// -----------------------------------------------------------------------------

func handleElevenLabsMessages(
	elevenConn, twilioConn *websocket.Conn,
	conversation *ConversationData,
	disconnectFunc func(),
	lastUserAudioTime *time.Time,
) {
	for {
		_, message, err := elevenConn.ReadMessage()
		if err != nil {
			log.Printf("[ElevenLabs] WebSocket error: %v", err)
			disconnectFunc()
			return
		}

		now := time.Now()

		var data map[string]interface{}
		if err := json.Unmarshal(message, &data); err != nil {
			log.Printf("[ElevenLabs] Error parsing message: %v", err)
			continue
		}

		msgType, _ := data["type"].(string)
		if msgType == "" {
			continue
		}
		log.Printf("[ElevenLabs] Received message type: %s", msgType)

		switch msgType {
		case "conversation_initiation_metadata":
			if meta, ok := data["conversation_initiation_metadata_event"].(map[string]interface{}); ok {
				if id, ok := meta["conversation_id"].(string); ok {
					conversation.ConversationID = id
					conversations.Store(conversation.StreamSid, conversation)
					log.Printf("[ElevenLabs] Stored conversation ID: %s", id)
				}
			}

		case "audio":
			var audioB64 string

			if ev, ok := data["audio_event"].(map[string]interface{}); ok {
				audioB64, _ = ev["audio_base_64"].(string)
			} else if audio, ok := data["audio"].(map[string]interface{}); ok {
				audioB64, _ = audio["chunk"].(string)
			}

			if audioB64 == "" {
				continue
			}

			if EnableLatencyDebug {
				if lastUserAudioTime != nil && !lastUserAudioTime.IsZero() {
					delta := now.Sub(*lastUserAudioTime)
					log.Printf("[Latency] EL_IN audio at %s; time since last TWILIO_IN media: %s", now.Format(time.RFC3339Nano), delta.String())
				} else {
					log.Printf("[Latency] EL_IN audio at %s; no last TWILIO_IN timestamp recorded", now.Format(time.RFC3339Nano))
				}
			}

			pcmBytes, err := base64.StdEncoding.DecodeString(audioB64)
			if err != nil {
				log.Printf("[ElevenLabs] Error decoding audio base64: %v", err)
				continue
			}

			ulawB64, err := pcm16ToULaw8kBase64(pcmBytes)
			if err != nil {
				log.Printf("[Transcode] Error transcoding ElevenLabs audio: %v", err)
				continue
			}

			resp := map[string]interface{}{
				"event":     "media",
				"streamSid": conversation.StreamSid,
				"media": map[string]string{
					"payload": ulawB64,
				},
			}

			sendStart := time.Now()
			if err := twilioConn.WriteJSON(resp); err != nil {
				log.Printf("[Twilio] Error sending media: %v", err)
			} else if EnableLatencyDebug {
				log.Printf("[Latency] TWILIO_OUT media at %s (send duration: %s)", time.Now().Format(time.RFC3339Nano), time.Since(sendStart).String())
			}

		case "interruption":
			clearMsg := map[string]interface{}{
				"event":     "clear",
				"streamSid": conversation.StreamSid,
			}
			if err := twilioConn.WriteJSON(clearMsg); err != nil {
				log.Printf("[Twilio] Error sending clear: %v", err)
			}

		case "ping":
			if ev, ok := data["ping_event"].(map[string]interface{}); ok {
				if id, ok := ev["event_id"].(string); ok {
					pong := map[string]interface{}{
						"type":     "pong",
						"event_id": id,
					}
					if err := elevenConn.WriteJSON(pong); err != nil {
						log.Printf("[ElevenLabs] Error sending pong: %v", err)
					}
				}
			}

		case "end_of_conversation":
			log.Println("[ElevenLabs] End of conversation")
			disconnectFunc()
			return
		}
	}
}

// -----------------------------------------------------------------------------
// AUDIO TRANSCODING HELPERS (PCM16 16k -> µ-law 8k)
// -----------------------------------------------------------------------------

// pcm16ToULaw8kBase64 takes raw little-endian PCM16 at 16kHz and returns base64 µ-law at 8kHz
func pcm16ToULaw8kBase64(pcm []byte) (string, error) {
	if len(pcm)%2 != 0 {
		pcm = pcm[:len(pcm)-1]
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}

	down := make([]int16, len(samples)/2)
	for i := range down {
		down[i] = samples[i*2]
	}

	ulaw := make([]byte, len(down))
	for i, s := range down {
		ulaw[i] = linearToMuLaw(s)
	}

	return base64.StdEncoding.EncodeToString(ulaw), nil
}

// standard G.711 µ-law encoder (all int math, then cast to byte)
func linearToMuLaw(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)

	s := int(sample)

	sign := 0
	if s < 0 {
		s = -s
		sign = 0x80
	}

	if s > clip {
		s = clip
	}
	s += bias

	exponent := 7
	for expMask := 0x4000; (s & expMask) == 0 && exponent > 0; exponent-- {
		expMask >>= 1
	}

	mantissa := (s >> (exponent + 3)) & 0x0F
	mu := byte(sign | (exponent << 4) | mantissa)

	return ^mu
}

// -----------------------------------------------------------------------------
// PHONE NORMALISATION (AU -> +61...)
// -----------------------------------------------------------------------------

func normalizeAUNumber(input string) string {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return ""
	}

	// Remove common formatting
	replacer := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
	raw = replacer.Replace(raw)

	if raw == "" {
		return ""
	}

	// Already in + format
	if strings.HasPrefix(raw, "+") {
		if strings.HasPrefix(raw, "+61") {
			rest := raw[3:]
			if strings.HasPrefix(rest, "0") {
				rest = rest[1:]
			}
			return "+61" + rest
		}
		return raw
	}

	// 00 international prefix
	if strings.HasPrefix(raw, "00") {
		tmp := raw[2:]
		if strings.HasPrefix(tmp, "61") {
			tmp = tmp[2:]
			if strings.HasPrefix(tmp, "0") {
				tmp = tmp[1:]
			}
			return "+61" + tmp
		}
		return "+" + tmp
	}

	// Assume AU local formats from here

	// Mobile: 04xxxxxxxx
	if len(raw) == 10 && strings.HasPrefix(raw, "04") {
		return "+61" + raw[1:]
	}

	// Mobile without leading 0: 4xxxxxxxx
	if len(raw) == 9 && strings.HasPrefix(raw, "4") {
		return "+61" + raw
	}

	// Landline: 0[2,3,7,8]xxxxxxx
	if len(raw) == 10 && raw[0] == '0' && strings.ContainsRune("2378", rune(raw[1])) {
		return "+61" + raw[1:]
	}

	// 13 / 1300 / 1800 / 180x etc
	if raw[0] == '1' {
		return "+61" + raw
	}

	// Fallback: just prepend +61
	return "+61" + raw
}

// -----------------------------------------------------------------------------
// TWILIO API + MOCK HELPERS
// -----------------------------------------------------------------------------

func createTwilioCall(params map[string]string, accountSid, authToken string) (map[string]interface{}, error) {
	client := &http.Client{}
	form := url.Values{}
	for k, v := range params {
		form.Add(k, v)
	}

	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls.json", accountSid),
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(accountSid, authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	respClose := resp.Body.Close
	defer respClose()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("twilio API error: %s", resp.Status)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func checkUserExists(phone string) (map[string]interface{}, error) {
	log.Printf("[Mock] Checking if user exists with phone: %s", phone)
	return map[string]interface{}{
		"first_name": "John",
		"last_name":  "Doe",
		"phone":      phone,
	}, nil
}

func sendConversationWebhook(payload map[string]interface{}, endpoint string, authToken string) error {
	log.Printf("[Mock Webhook] Sending payload to %s: %+v", endpoint, payload)
	log.Printf("[Mock Webhook] Using auth token: %s", authToken)
	log.Printf("[Mock Webhook] Successfully sent to %s endpoint", endpoint)
	return nil
}
