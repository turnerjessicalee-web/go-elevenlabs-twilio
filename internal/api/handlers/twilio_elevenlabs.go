package handlers

import (
	"caller/internal/config"
	"caller/internal/elevenlabs"
	"encoding/base64"
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

var conversations sync.Map

// ConversationData represents a call session with all necessary identifiers and metadata
type ConversationData struct {
	StreamSid      string
	CallSid        string
	ConversationID string
	CallerPhone    string
	UserData       map[string]interface{}
	Direction      string // "inbound" or "outbound"
}

// handleIncomingCall processes webhook requests from twilio when someone calls our number
func HandleIncomingCall() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form data", http.StatusBadRequest)
			return
		}

		// get caller's phone number
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

		// generate twiml
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
</Response>`, r.Host, callerPhone, url.QueryEscape(string(userDataJSON)))

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// HandleMediaStream manages bidirectional audio between Twilio and ElevenLabs
// Upgrades HTTP to WebSocket and routes audio between caller and AI
func HandleMediaStream(upgrader websocket.Upgrader, cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WebSocket] Error upgrading connection: %v", err)
			return
		}
		defer conn.Close()

		log.Println("[Server] Twilio connected to media stream")

		var (
			streamSid       string
			callSid         string
			elevenLabsWs    *websocket.Conn
			callerPhone     = "Unknown"
			userData        map[string]interface{}
			direction       = "inbound" // Default to inbound
			conversation    *ConversationData
			isDisconnecting = false
			disconnectMutex sync.Mutex
		)

		disconnectCall := func() {
			disconnectMutex.Lock()
			defer disconnectMutex.Unlock()

			if isDisconnecting {
				return
			}
			isDisconnecting = true
			log.Println("[Twilio] Initiating call disconnect")

			if conversation != nil && conversation.ConversationID != "" {
				payload := map[string]interface{}{
					"conversation_id": conversation.ConversationID,
					"phone_number":    conversation.CallerPhone,
					"call_sid":        conversation.StreamSid,
					"direction":       conversation.Direction,
				}

				endpoint := "inbound-calls"
				if conversation.Direction == "outbound" {
					endpoint = "outbound-calls"
				}

				if err := sendConversationWebhook(payload, endpoint, "123123"); err != nil {
					log.Printf("[Webhook] Error sending webhook: %v", err)
				}

				conversations.Delete(streamSid)
			}

			// send commands to twilio to disconnect
			messages := []map[string]interface{}{
				{"event": "mark_done", "streamSid": streamSid},
				{"event": "clear", "streamSid": streamSid},
				{"event": "twiml", "streamSid": streamSid, "twiml": "<Response><Hangup/></Response>"},
			}

			for _, msg := range messages {
				if err := conn.WriteJSON(msg); err != nil {
					log.Printf("[Twilio] Error sending disconnect message: %v", err)
				}
			}

			time.AfterFunc(1*time.Second, func() {
				_ = conn.Close()
			})
		}

		// main websocket message loop
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[Twilio] WebSocket error: %v", err)
				break
			}

			if messageType != websocket.TextMessage {
				continue
			}

			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Printf("[Twilio] Error parsing message: %v", err)
				continue
			}

			event, ok := data["event"].(string)
			if !ok {
				continue
			}

			log.Printf("[Twilio] Received event: %s", event)

			disconnectMutex.Lock()
			localIsDisconnecting := isDisconnecting
			disconnectMutex.Unlock()

			if localIsDisconnecting && event != "stop" {
				continue
			}

			switch event {
			case "start":
				startData, ok := data["start"].(map[string]interface{})
				if !ok {
					log.Println("[Twilio] Invalid start data")
					continue
				}

				streamSid, _ = startData["streamSid"].(string)
				if callSidVal, ok := startData["callSid"]; ok {
					callSid, _ = callSidVal.(string)
				}

				customParams, ok := startData["customParameters"].(map[string]interface{})
				if !ok {
					log.Println("[Twilio] No custom parameters")
					continue
				}

				if phone, ok := customParams["caller_phone"].(string); ok {
					callerPhone = phone
				}

				if dir, ok := customParams["direction"].(string); ok {
					direction = dir
				}

				if userDataStr, ok := customParams["user_data"].(string); ok {
					decodedStr, err := url.QueryUnescape(userDataStr)
					if err != nil {
						log.Printf("[Twilio] Error decoding user data: %v", err)
					} else {
						if err := json.Unmarshal([]byte(decodedStr), &userData); err != nil {
							log.Printf("[Twilio] Error parsing user data: %v", err)
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

				log.Printf("[Twilio] Stream started - SID: %s, Phone: %s", streamSid, callerPhone)

				signedURL, err := elevenlabs.GetSignedElevenLabsURL(cfg.ElevenLabsAgentID, cfg.ElevenLabsAPIKey)
				if err != nil {
					log.Printf("[ElevenLabs] Error getting signed URL: %v", err)
					disconnectCall()
					continue
				}

				elevenLabsWs, _, err = websocket.DefaultDialer.Dial(signedURL, nil)
				if err != nil {
					log.Printf("[ElevenLabs] Error connecting: %v", err)
					disconnectCall()
					continue
				}

				isInbound := direction == "inbound"
				elConfig := elevenlabs.GenerateElevenLabsConfig(userData, callerPhone, isInbound)

				if err := elevenLabsWs.WriteJSON(elConfig); err != nil {
					log.Printf("[ElevenLabs] Error sending config: %v", err)
					disconnectCall()
					continue
				}

				go handleElevenLabsMessages(elevenLabsWs, conn, conversation, disconnectCall)

			case "media":
				if elevenLabsWs != nil && !localIsDisconnecting {
					mediaData, ok := data["media"].(map[string]interface{})
					if !ok {
						log.Println("[Twilio] Invalid media data")
						continue
					}

					payload, ok := mediaData["payload"].(string)
					if !ok {
						log.Println("[Twilio] Invalid media payload")
						continue
					}

					// Twilio -> μ-law 8k base64
					muBytes, err := base64.StdEncoding.DecodeString(payload)
					if err != nil {
						log.Printf("[Audio] Failed to decode Twilio μ-law base64: %v", err)
						continue
					}

					// Convert μ-law 8k -> PCM 16k (16-bit LE)
					pcm16k := muLaw8kToPCM16k(muBytes)

					pcmB64 := base64.StdEncoding.EncodeToString(pcm16k)

					audioMessage := map[string]string{
						"user_audio_chunk": pcmB64,
					}

					if err := elevenLabsWs.WriteJSON(audioMessage); err != nil {
						log.Printf("[ElevenLabs] Error sending audio: %v", err)
					}
				}

			case "stop":
				if elevenLabsWs != nil {
					_ = elevenLabsWs.WriteJSON(map[string]string{
						"type": "end_conversation",
					})
					_ = elevenLabsWs.Close()
				}

				disconnectCall()
				return
			}
		}
	})
}

// HandleOutboundCall initiates calls from our system to specified phone numbers
func HandleOutboundCall(cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Number string `json:"number"`
			Prompt string `json:"prompt"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Number == "" {
			http.Error(w, "Phone number is required", http.StatusBadRequest)
			return
		}

		callURL := fmt.Sprintf("https://%s/outbound-call-twiml?prompt=%s&number=%s",
			r.Host, url.QueryEscape(req.Prompt), url.QueryEscape(req.Number))

		params := map[string]string{
			"To":   req.Number,
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

// HandleOutboundCallTwiml generates xml instructions for twilio when setting up outbound calls
func HandleOutboundCallTwiml() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompt := r.URL.Query().Get("prompt")
		number := r.URL.Query().Get("number")

		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Connect>
        <Stream url="wss://%s/media-stream">
            <Parameter name="caller_phone" value="%s" />
            <Parameter name="prompt" value="%s" />
            <Parameter name="direction" value="outbound" />
        </Stream>
    </Connect>
</Response>`, r.Host, number, url.QueryEscape(prompt))

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// private //

// handleElevenLabsMessages processes responses from elevenlabs ai and forwards audio to twilio
func handleElevenLabsMessages(
	elevenLabsWs, twilioWs *websocket.Conn,
	conversation *ConversationData,
	disconnectFunc func(),
) {
	for {
		_, message, err := elevenLabsWs.ReadMessage()
		if err != nil {
			log.Printf("[ElevenLabs] WebSocket error: %v", err)

			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Println("[ElevenLabs] Connection closed normally")
			} else {
				log.Printf("[ElevenLabs] Abnormal connection closure: %v", err)
			}

			disconnectFunc()
			break
		}

		var data map[string]interface{}
		if err := json.Unmarshal(message, &data); err != nil {
			log.Printf("[ElevenLabs] Error parsing message: %v", err)
			continue
		}

		messageType, _ := data["type"].(string)
		log.Printf("[ElevenLabs] Received message type: %s", messageType)

		switch messageType {
		case "conversation_initiation_metadata":
			if metadataEvent, ok := data["conversation_initiation_metadata_event"].(map[string]interface{}); ok {
				if conversationID, ok := metadataEvent["conversation_id"].(string); ok {
					conversation.ConversationID = conversationID
					log.Printf("[ElevenLabs] Stored conversation ID: %s", conversationID)
					conversations.Store(conversation.StreamSid, conversation)
				}
				log.Printf("[ElevenLabs] Metadata: %+v", metadataEvent)
			}

		case "audio":
			var audioBase64 string
			if audioEvent, ok := data["audio_event"].(map[string]interface{}); ok {
				audioBase64, _ = audioEvent["audio_base_64"].(string)
			} else if audio, ok := data["audio"].(map[string]interface{}); ok {
				audioBase64, _ = audio["chunk"].(string)
			}

			if audioBase64 != "" {
				pcmBytes, err := base64.StdEncoding.DecodeString(audioBase64)
				if err != nil {
					log.Printf("[Audio] Failed to decode ElevenLabs PCM base64: %v", err)
					continue
				}

				muBytes := pcm16kToMuLaw8k(pcmBytes)
				muB64 := base64.StdEncoding.EncodeToString(muBytes)

				audioData := map[string]interface{}{
					"event":     "media",
					"streamSid": conversation.StreamSid,
					"media": map[string]string{
						"payload": muB64,
					},
				}

				if err := twilioWs.WriteJSON(audioData); err != nil {
					log.Printf("[Twilio] Error sending audio: %v", err)
				}
			}

		case "interruption":
			clearMsg := map[string]interface{}{
				"event":     "clear",
				"streamSid": conversation.StreamSid,
			}

			if err := twilioWs.WriteJSON(clearMsg); err != nil {
				log.Printf("[Twilio] Error sending clear: %v", err)
			}

		case "ping":
			if pingEvent, ok := data["ping_event"].(map[string]interface{}); ok {
				if eventID, ok := pingEvent["event_id"].(string); ok {
					pongResponse := map[string]interface{}{
						"type":     "pong",
						"event_id": eventID,
					}

					if err := elevenLabsWs.WriteJSON(pongResponse); err != nil {
						log.Printf("[ElevenLabs] Error sending pong: %v", err)
					}
				}
			}

		case "end_of_conversation":
			log.Println("[ElevenLabs] End of conversation received")
			disconnectFunc()
			return
		}
	}
}

// createTwilioCall sends api request to twilio to initiate an outbound call
func createTwilioCall(params map[string]string, accountSid, authToken string) (map[string]interface{}, error) {
	client := &http.Client{}

	form := url.Values{}
	for key, value := range params {
		form.Add(key, value)
	}

	req, err := http.NewRequest("POST",
		fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Calls.json", accountSid),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(accountSid, authToken)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("twilio API error: %s", resp.Status)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}

// checkUserExists verifies caller authorization based on phone number
func checkUserExists(phone string) (map[string]interface{}, error) {
	log.Printf("[Mock] Checking if user exists with phone: %s", phone)

	return map[string]interface{}{
		"first_name": "John",
		"last_name":  "Doe",
		"phone":      phone,
	}, nil
}

// sendConversationWebhook notifies external systems about call events
func sendConversationWebhook(payload map[string]interface{}, endpoint string, authToken string) error {
	log.Printf("[Mock Webhook] Sending payload to %s: %+v", endpoint, payload)
	log.Printf("[Mock Webhook] Using auth token: %s", authToken)
	log.Printf("[Mock Webhook] Successfully sent to %s endpoint", endpoint)
	return nil
}

//////////////////////
// AUDIO CONVERSION //
//////////////////////

const (
	muLawBias = 0x84
	muLawMax  = 0x1FFF
)

// μ-law 8k → PCM 16k (16-bit LE) by decoding and duplicating each sample
func muLaw8kToPCM16k(mu []byte) []byte {
	if len(mu) == 0 {
		return nil
	}

	out := make([]byte, len(mu)*4) // 1 μ-law sample -> 2 PCM samples -> 4 bytes
	outIdx := 0

	for _, b := range mu {
		s := muLawDecode(b)

		// sample 1
		out[outIdx] = byte(s & 0xff)
		out[outIdx+1] = byte((s >> 8) & 0xff)
		// sample 2 (duplicate for 16k)
		out[outIdx+2] = out[outIdx]
		out[outIdx+3] = out[outIdx+1]

		outIdx += 4
	}

	return out
}

// PCM 16k (16-bit LE) → μ-law 8k by dropping every second sample and encoding
func pcm16kToMuLaw8k(pcm []byte) []byte {
	if len(pcm) < 4 {
		return nil
	}

	numOut := len(pcm) / 4 // we take one 16-bit sample for every 2 samples (4 bytes)
	out := make([]byte, numOut)

	outIdx := 0
	for i := 0; i < numOut; i++ {
		src := i * 4
		if src+1 >= len(pcm) {
			break
		}
		s := int16(pcm[src]) | int16(pcm[src+1])<<8
		out[outIdx] = muLawEncode(s)
		outIdx++
	}

	return out[:outIdx]
}

func muLawDecode(b byte) int16 {
	// standard μ-law decode
	b = ^b
	sign := b & 0x80
	exponent := (b & 0x70) >> 4
	mantissa := b & 0x0F

	sample := ((int16(mantissa) << 4) + 0x08) << exponent
	sample -= muLawBias

	if sign != 0 {
		sample = -sample
	}

	return sample
}

func muLawEncode(sample int16) byte {
	sign := byte(0)
	if sample < 0 {
		sign = 0x80
		sample = -sample
		if sample < 0 {
			sample = 0
		}
	}

	if sample > muLawMax {
		sample = muLawMax
	}
	sample += muLawBias

	exponent := byte(7)
	for expMask := 0x4000; (int(sample)&expMask) == 0 && exponent > 0; exponent-- {
		expMask >>= 1
	}

	mantissa := (sample >> (exponent + 3)) & 0x0F
	mu := ^(sign | (exponent << 4) | byte(mantissa))
	return mu
}
