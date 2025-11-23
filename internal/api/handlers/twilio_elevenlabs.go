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
			firstName       string
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
				if fn, ok := custom["first_name"].(string); ok {
					firstName = fn
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
				configMsg := elevenlabs.GenerateElevenLabsConfig(userData, callerPhone, firstName, isInbound)

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
			Prompt    string `json:"prompt"`
			FirstName string `json:"first_name"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Number == "" {
			http.Error(w, "Phone number is required", http.StatusBadRequest)
			return
		}

		callURL := fmt.Sprintf(
			"https://%s/outbound-call-twiml?prompt=%s&number=%s&first_name=%s",
			r.Host,
			url.QueryEscape(req.Prompt),
			url.QueryEscape(req.Number),
			url.QueryEscape(req.FirstName),
		)

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

func HandleOutboundCallTwiml() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompt := r.URL.Query().Get("prompt")
		number := r.URL.Query().Get("number")
		firstName := r.URL.Query().Get("first_name")

		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Connect>
        <Stream url="wss://%s/media-stream">
            <Parameter name="caller_phone" value="%s" />
            <Parameter name="prompt" value="%s" />
            <Parameter name="first_name" value="%s" />
            <Parameter name="direction" value="outbound" />
        </Stream>
    </Connect>
</Response>`,
			r.Host,
			number,
			url.QueryEscape(prompt),
			firstName,
		)

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// -----------------------------------------------------------------------------
// ElevenLabs → Twilio bridge (with PCM16 -> µ-law 8k transcoding)
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
