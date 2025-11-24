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

// NumberStats tracks per-number usage per day
type NumberStats struct {
	Count int
	Date  string // YYYY-MM-DD
}

var (
	conversations sync.Map

	// Guardrails: global daily cap
	dailyCallCountMu sync.Mutex
	dailyCallCount   int
	dailyCallDate    = time.Now().Format("2006-01-02")

	// Guardrails: per-number daily cap (max 4 calls / number / day)
	numberStatsMu sync.Mutex
	numberStats   = make(map[string]NumberStats)
)

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
			callStartTime     time.Time
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

				// Track call start + initial audio time for guardrails
				mu.Lock()
				callStartTime = time.Now()
				lastUserAudioTime = callStartTime
				mu.Unlock()

				// Silence timeout watcher (e.g. 2 minutes of no user audio)
				go func() {
					ticker := time.NewTicker(30 * time.Second)
					defer ticker.Stop()

					for range ticker.C {
						mu.Lock()
						if isDisconnecting {
							mu.Unlock()
							return
						}
						lt := lastUserAudioTime
						mu.Unlock()

						if !lt.IsZero() && time.Since(lt) > 2*time.Minute {
							log.Printf("[Guard] No user audio for >2m, disconnecting call (streamSid=%s)", streamSid)
							disconnectCall()
							return
						}
					}
				}()

				// Max call duration watcher (e.g. hard 5 min cap)
				go func() {
					ticker := time.NewTicker(30 * time.Second)
					defer ticker.Stop()

					for range ticker.C {
						mu.Lock()
						if isDisconnecting {
							mu.Unlock()
							return
						}
						cs := callStartTime
						mu.Unlock()

						if !cs.IsZero() && time.Since(cs) > 5*time.Minute {
							log.Printf("[Guard] Call exceeded 5 minutes, disconnecting (streamSid=%s)", streamSid)
							disconnectCall()
							return
						}
					}
				}()

				go handleElevenLabsMessages(
					elevenConn,
					twilioConn,
					conversation,
					disconnectCall,
					&lastUserAudioTime,
					&mu,
					&isDisconnecting,
				)

			case "media":
				media, ok := data["media"].(map[string]interface{})
				if !ok {
					continue
				}
				payload, _ := media["payload"].(string)
				if payload == "" {
					continue
				}

				now := time.Now()
				mu.Lock()
				lastUserAudioTime = now
				mu.Unlock()

				if EnableLatencyDebug {
					log.Printf("[Latency] TWILIO_IN media at %s (len=%d bytes base64)", now.Format(time.RFC3339Nano), len(payload))
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

		// 🚫 Block non-AU numbers for auto-calls
		if !strings.HasPrefix(normalizedNumber, "+61") {
			log.Printf("[Guard] Blocked non-AU number from auto-call: raw=%s normalized=%s", req.Number, normalizedNumber)
			http.Error(w, "Only Australian numbers are auto-called. Your request has been recorded for manual review.", http.StatusBadRequest)
			return
		}

		today := time.Now().Format("2006-01-02")

		// Global daily cap with simple reset
		dailyCallCountMu.Lock()
		if today != dailyCallDate {
			dailyCallDate = today
			dailyCallCount = 0
		}
		if dailyCallCount >= 50 { // global cap (tweak as needed)
			dailyCallCountMu.Unlock()
			log.Printf("[Guard] Daily call cap reached (%d). Blocking auto-call.", dailyCallCount)
			http.Error(w, "Daily call limit reached. Your request has been recorded for manual review.", http.StatusTooManyRequests)
			return
		}
		dailyCallCountMu.Unlock()

		// Per-number cap: max 4 calls per number per day
		numberStatsMu.Lock()
		ns, ok := numberStats[normalizedNumber]
		if !ok || ns.Date != today {
			ns = NumberStats{Count: 0, Date: today}
		}
		if ns.Count >= 4 {
			numberStatsMu.Unlock()
			log.Printf("[Guard] Per-number cap reached for %s: %d calls today", normalizedNumber, ns.Count)
			http.Error(w, "We have already called this number several times today. Please try again tomorrow.", http.StatusTooManyRequests)
			return
		}
		numberStatsMu.Unlock()

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

		// Increment global daily call count only on successful Twilio call creation
		dailyCallCountMu.Lock()
		dailyCallCount++
		dailyCallCountMu.Unlock()

		// Increment per-number count (now that the call was actually created)
		numberStatsMu.Lock()
		ns, ok = numberStats[normalizedNumber]
		if !ok || ns.Date != today {
			ns = NumberStats{Count: 0, Date: today}
		}
		ns.Count++
		ns.Date = today
		numberStats[normalizedNumber] = ns
		numberStatsMu.Unlock()

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

		// Build user_data JSON from quer_
