package handlers

import (
	"caller/internal/config"
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

// EchoMode: when true we ignore ElevenLabs and simply echo audio
// back to the caller to verify Twilio <-> Go audio is clean.
const EchoMode = true

// ConversationData is kept for possible future use, but not necessary
// for pure echo mode.
type ConversationData struct {
	StreamSid      string
	CallSid        string
	ConversationID string
	CallerPhone    string
	UserData       map[string]interface{}
	Direction      string // "inbound" or "outbound"
}

var conversations sync.Map

// HandleIncomingCall processes webhook requests from Twilio when someone calls our number
func HandleIncomingCall() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Failed to parse form data", http.StatusBadRequest)
			return
		}

		// Caller phone number from Twilio
		callerPhone := r.FormValue("From")
		log.Printf("[Twilio] Incoming call from: %s", callerPhone)

		// In echo mode we don't care about auth – just connect to stream
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

// HandleMediaStream manages bidirectional audio between Twilio and our server.
// In EchoMode it just sends the caller's audio straight back to them.
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
			log.Println("[Server] EchoMode ENABLED (no ElevenLabs, pure Twilio echo)")
		}

		var (
			streamSid       string
			callSid         string
			callerPhone     = "Unknown"
			direction       = "inbound"
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

			// Optional: clean up conversation map
			if conversation != nil {
				conversations.Delete(conversation.StreamSid)
			}

			// Tell Twilio to hang up
			if streamSid != "" {
				messages := []map[string]interface{}{
					{"event": "mark_done", "streamSid": streamSid},
					// "clear" is optional; Twilio warns if malformed, so we omit it in echo mode
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

				// ECHO-ONLY: send the same payload back as outbound media
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

			case "stop":
				log.Println("[Twilio] Stop event received")
				disconnectCall()
				return
			}
		}
	})
}

// HandleOutboundCall initiates calls from our system to specified phone numbers
// Accepts number and prompt in JSON and returns call status
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

		// Outbound TwiML URL, we ignore prompt in echo mode
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

// HandleOutboundCallTwiml generates TwiML for outbound calls
// It simply connects the call to the media-stream WebSocket.
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

// ---- Private helpers ----

// createTwilioCall sends API request to Twilio to initiate an outbound call
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
