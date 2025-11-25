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
			if err != n
