package handlers

import (
	"caller/internal/config"
	"caller/internal/elevenlabs"
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

// Toggle: when true = pure echo (no ElevenLabs).
// When false = full Twilio <-> ElevenLabs bridge.
const EchoMode = false

// ConversationData represents a call session with identifiers/metadata.
type ConversationData struct {
	StreamSid      string
	CallSid        string
	ConversationID string
	CallerPhone    string
	UserData       map[string]interface{}
	Direction      string // "inbound" or "outbound"
}

var conversations sync.Map

// HandleIncomingCall processes webhook requests from Twilio when someone calls our number.
func HandleIncomingCall() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form data", http.StatusBadRequest)
			return
		}

		callerPhone := r.FormValue("From")
		log.Printf("[Twilio] Incoming call from: %s", callerPhone)

		// For now we skip auth and always connect to media stream.
		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
	<Connect>
		<Stream url="wss://%s/media-stream">
			<Parameter name="caller_phone" value="%s" />
			<Parameter name="direction" value="inbound" />
		</Stream>
	</Connect>
</Response>`, r.Host, callerPhone)

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// HandleMediaStream upgrades HTTP -> WebSocket and routes audio
// between Twilio and ElevenLabs (or echo, depending on EchoMode).
func HandleMediaStream(upgrader websocket.Upgrader, cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WebSocket] Error upgrading connection: %v", err)
			return
		}
		defer conn.Close()

		log.Println("[Server] Twilio connected to media stream")
		if EchoMode {
			log.Println("[Server] EchoMode ENABLED (no ElevenLabs)")
		} else {
			log.Println("[Server] ElevenLabs bridge ENABLED")
		}

		var (
			streamSid       string
			callSid         string
			callerPhone     = "Unknown"
			direction       = "inbound"
			conversation    *ConversationData
			elevenLabsWs    *websocket.Conn
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

			// Close ElevenLabs WS if present
			if elevenLabsWs != nil {
				_ = elevenLabsWs.WriteJSON(map[string]interface{}{
					"type": "end_of_conversation",
				})
				_ = elevenLabsWs.Close()
			}

			if conversation != nil && conversation.StreamSid != "" {
				conversations.Delete(conversation.StreamSid)
			}

			if streamSid != "" {
				// mark_done + hangup; omit "clear" to avoid Twilio warnings.
				messages := []map[string]interface{}{
					{"event": "mark_done", "streamSid": streamSid},
					{"event": "twiml", "streamSid": streamSid, "twiml": "<Response><Hangup/></Response>"},
				}
				for _, msg := range messages {
					if err := conn.WriteJSON(msg); err != nil {
						log.Printf("[Twilio] Error sending disconnect message: %v", err)
					}
				}
			}

			time.AfterFunc(1*time.Second, func() {
				_ = conn.Close()
			})
		}

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[Twilio] WebSocket error: %v", err)
				break
			}

			if messageType != websocket.TextMessage {
				continue
			}

            // Twilio frames are JSON text
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

				if sid, ok := startData["streamSid"].(string); ok {
					streamSid = sid
				}
				if cs, ok := startData["callSid"].(string); ok {
					callSid = cs
				}

				customParams, _ := startData["customParameters"].(map[string]interface{})

				if phone, ok := customParams["caller_phone"].(string); ok {
					callerPhone = phone
				}
				if dir, ok := customParams["direction"].(string); ok {
					direction = dir
				}

				conversation = &ConversationData{
					StreamSid:   streamSid,
					CallSid:     callSid,
					CallerPhone: callerPhone,
					Direction:   direction,
				}
				conversations.Store(streamSid, conversation)

				log.Printf("[Twilio] Stream started - SID: %s, Phone: %s, Direction: %s", streamSid, callerPhone, direction)

				// If EchoMode, we don't touch ElevenLabs.
				if EchoMode {
					continue
				}

				// ---- ElevenLabs ConvAI setup ----

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
				log.Println("[ElevenLabs] WebSocket connected")

				isInbound := direction == "inbound"
				// We don't have rich userData here yet, so pass nil.
				elConfig := elevenlabs.GenerateElevenLabsConfig(nil, callerPhone, isInbound)

				if err := elevenLabsWs.WriteJSON(elConfig); err != nil {
					log.Printf("[ElevenLabs] Error sending config: %v", err)
					disconnectCall()
					continue
				}

				go handleElevenLabsMessages(elevenLabsWs, conn, conversation, disconnectCall)

			case "media":
				mediaData, ok := data["media"].(map[string]interface{})
				if !ok {
					log.Println("[Twilio] Invalid media data")
					continue
				}
				payload, ok := mediaData["payload"].(string)
				if !ok || payload == "" {
					log.Println("[Twilio] Invalid media payload")
					continue
				}

				// Always: if EchoMode, send echo back to Twilio.
				if EchoMode && streamSid != "" {
					echoMsg := map[string]interface{}{
						"event":     "media",
						"streamSid": streamSid,
						"media": map[string]string{
							"payload": payload,
						},
					}
					if err := conn.WriteJSON(echoMsg); err != nil {
						log.Printf("[Echo] Error sending echo media: %v", err)
					}
				}

				// When not EchoMode, forward user audio to ElevenLabs.
				if !EchoMode && elevenLabsWs != nil {
					audioMessage := map[string]string{
						"user_audio_chunk": payload, // ConvAI mulaw_8000 base64 chunk
					}
					if err := elevenLabsWs.WriteJSON(audioMessage); err != nil {
						log.Printf("[ElevenLabs] Error sending audio: %v", err)
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

// HandleOutboundCall initiates calls from our system to specified phone numbers.
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

// HandleOutboundCallTwiml generates TwiML instructions for Twilio when setting up outbound calls.
func HandleOutboundCallTwiml() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := r.URL.Query().Get("number")

		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Connect>
        <Stream url="wss://%s/media-stream">
            <Parameter name="caller_phone" value="%s" />
            <Parameter name="direction" value="outbound" />
        </Stream>
    </Connect>
</Response>`, r.Host, number)

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}

// ---- ElevenLabs message handling ----

// handleElevenLabsMessages reads from ElevenLabs ConvAI WS and forwards audio to Twilio.
func handleElevenLabsMessages(
	elevenLabsWs *websocket.Conn,
	twilioWs *websocket.Conn,
	conversation *ConversationData,
	disconnectFunc func(),
) {
	for {
		_, message, err := elevenLabsWs.ReadMessage()
		if err != nil {
			log.Printf("[ElevenLabs] WebSocket error: %v", err)
			disconnectFunc()
			return
		}

		var data map[string]interface{}
		if err := json.Unmarshal(message, &data); err != nil {
			log.Printf("[ElevenLabs] Error parsing message: %v", err)
			continue
		}

		msgType, _ := data["type"].(string)
		log.Printf("[ElevenLabs] Received message type: %s", msgType)

		switch msgType {
		case "conversation_initiation_metadata":
			if meta, ok := data["conversation_initiation_metadata_event"].(map[string]interface{}); ok {
				if convID, ok := meta["conversation_id"].(string); ok {
					conversation.ConversationID = convID
					conversations.Store(conversation.StreamSid, conversation)
					log.Printf("[ElevenLabs] Stored conversation ID: %s", convID)
				}
			}

		case "audio":
			var audioBase64 string

			if audioEvent, ok := data["audio_event"].(map[string]interface{}); ok {
				if chunk, ok := audioEvent["audio_base_64"].(string); ok {
					audioBase64 = chunk
				}
			} else if audio, ok := data["audio"].(map[string]interface{}); ok {
				if chunk, ok := audio["chunk"].(string); ok {
					audioBase64 = chunk
				}
			}

			if audioBase64 != "" && conversation.StreamSid != "" {
				audioData := map[string]interface{}{
					"event":     "media",
					"streamSid": conversation.StreamSid,
					"media": map[string]string{
						"payload": audioBase64,
					},
				}

				if err := twilioWs.WriteJSON(audioData); err != nil {
					log.Printf("[Twilio] Error sending audio to Twilio: %v", err)
				}
			}

		case "ping":
			if pingEvent, ok := data["ping_event"].(map[string]interface{}); ok {
				if eventID, ok := pingEvent["event_id"].(string); ok {
					pong := map[string]interface{}{
						"type":     "pong",
						"event_id": eventID,
					}
					if err := elevenLabsWs.WriteJSON(pong); err != nil {
						log.Printf("[ElevenLabs] Error sending pong: %v", err)
					}
				}
			}

		case "end_of_conversation":
			log.Println("[ElevenLabs] End of conversation received")
			disconnectFunc()
			return

		default:
			// ignore other types
		}
	}
}

// ---- Twilio helper ----

// createTwilioCall sends API request to Twilio to initiate an outbound call.
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
