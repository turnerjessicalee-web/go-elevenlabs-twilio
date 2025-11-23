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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request*
