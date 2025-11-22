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

// conversations tracks active calls by StreamSid
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

// -------------------- INBOUND CALL HANDLER --------------------

// HandleIncomingCall processes webhook requests from Twilio when someone calls our number
func HandleIncomingCall() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form data", http.StatusBadRequest)
			return
		}

		// Get caller's phone number
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

		// Generate TwiML
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

// -------------------- MEDIA STREAM HANDLER --------------------

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
			direction       = "inbound" // default
			conversation    *ConversationData
			isDisconnecting = false
			disconnectMutex sync.Mutex
		)

		// Safely terminate the call: tell Twilio to hang up and clean up resources
		disconnectCall := func() {
			disconnectMutex.Lock()
			defer disconnectMutex.Unlock()

			if isDisconnecting {
				return
			}
			isDisconnecting = true

			log.Println("[Twilio] Initiating call disconnect")

			// Send webhook about conversation end
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

			// Tell Twilio to stop streaming and hang up
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

			// Close WebSocket after a small delay
			time.AfterFunc(1*time.Second, func() {
				_ = conn.Close()
			})
		}

		// Main WebSocket loop: handle events from Twilio
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[Twilio] WebSocket error: %v", err)
				break
			}

			// Twilio sends text frames with JSON payloads
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

			// Check if we're already tearing down
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

				// Custom parameters (caller phone, direction, user data, etc.)
				customParams, ok := startData["customParameters"].(map[string]interface{})
				if !ok {
					log.Println("[Twilio] No custom parameters")
					customParams = map[string]interface{}{}
				}

				if phone, ok := customParams["caller_phone"].(string); ok {
					callerPhone = phone
				}

				if dir, ok := customParams["direction"].(string); ok {
					direction = dir
				}

				// Parse user data (for inbound calls)
				if userDataStr, ok := customParams["user_data"].(string); ok && userDataStr != "" {
					decodedStr, err := url.QueryUnescape(userDataStr)
					if err != nil {
						log.Printf("[Twilio] Error decoding user data: %v", err)
					} else {
						if err := json.Unmarshal([]byte(decodedStr), &userData); err != nil {
							log.Printf("[Twilio] Error parsing user data: %v", err)
						}
					}
				}

				// For outbound calls we might also have a "prompt"
				if promptStr, ok := customParams["prompt"].(string); ok && promptStr != "" {
					log.Printf("[Outbound] Prompt from query: %s", promptStr)
					if userData == nil {
						userData = make(map[string]interface{})
					}
					userData["prompt"] = promptStr
				}

				// Create conversation context
				conversation = &ConversationData{
					StreamSid:   streamSid,
					CallSid:     callSid,
					CallerPhone: callerPhone,
					UserData:    userData,
					Direction:   direction,
				}

				conversations.Store(streamSid, conversation)

				log.Printf("[Twilio] Stream started - SID: %s, Phone: %s, Direction: %s", streamSid, callerPhone, direction)

				// Get signed WS URL for ElevenLabs agent
				signedURL, err := elevenlabs.GetSignedElevenLabsURL(cfg.ElevenLabsAgentID, cfg.ElevenLabsAPIKey)
				if err != nil {
					log.Printf("[ElevenLabs] Error getting signed URL: %v", err)
					disconnectCall()
					continue
				}

				// Connect to ElevenLabs WebSocket
				elevenLabsWs, _, err = websocket.DefaultDialer.Dial(signedURL, nil)
				if err != nil {
					log.Printf("[ElevenLabs] Error connecting: %v", err)
					disconnectCall()
					continue
				}

				// Generate configuration based on direction
				isInbound := direction == "inbound"
				initConfig := elevenlabs.GenerateElevenLabsConfig(userData, callerPhone, isInbound)

				// Send configuration (conversation initiation)
				if err := elevenLabsWs.WriteJSON(initConfig); err != nil {
					log.Printf("[ElevenLabs] Error sending config: %v", err)
					disconnectCall()
					continue
				}

				// Handle ElevenLabs messages (audio back to Twilio etc.)
				go handleElevenLabsMessages(elevenLabsWs, conn, conversation, disconnectCall)

			case "media":
				// Forward caller audio to ElevenLabs
				if elevenLabsWs != nil && !localIsDisconnecting {
					mediaData, ok := data["media"].(map[string]interface{})
					if !ok {
						log.Println("[Twilio] Invalid media data")
						continue
					}

					payload, ok := mediaData["payload"].(string)
					if !ok || payload == "" {
						log.Println("[Twilio] Invalid or empty media payload")
						continue
					}

					// Twilio payload is base64-encoded PCMU (µ-law 8kHz)
					// ElevenLabs expects the same when the agent is configured for telephony / mulaw_8000
					audioMessage := map[string]string{
						"user_audio_chunk": payload,
					}

					if err := elevenLabsWs.WriteJSON(audioMessage); err != nil {
						log.Printf("[ElevenLabs] Error sending audio: %v", err)
					}
				}

			case "stop":
				// Twilio is ending the stream
				if elevenLabsWs != nil {
					_ =
