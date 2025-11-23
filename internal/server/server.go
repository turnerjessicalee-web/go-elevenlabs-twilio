package server

import (
	"caller/internal/api/handlers"
	"caller/internal/config"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

// Run starts the HTTP server and wires up all routes.
func Run() {
	// Load config (adjust if your Config loader is different)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("[Server] Failed to load config: %v", err)
	}

	// WebSocket upgrader for Twilio media stream
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// Twilio will connect from various IPs, so we typically allow all origins here.
			return true
		},
	}

	mux := http.NewServeMux()

	// Inbound call webhook (Twilio hits this when someone calls your Twilio number)
	mux.Handle("/incoming-call", handlers.HandleIncomingCall())

	// Twilio <-> Go <-> ElevenLabs media WebSocket bridge
	mux.Handle("/media-stream", handlers.HandleMediaStream(upgrader, cfg))

	// Outbound call initiation (your app hits this to start a call via Twilio)
	mux.Handle("/outbound-call", handlers.HandleOutboundCall(cfg))

	// TwiML endpoint for outbound calls (Twilio calls this to get <Connect><Stream> TwiML)
	mux.Handle("/outbound-call-twiml", handlers.HandleOutboundCallTwiml())

	// Simple health check (optional but handy for Render)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("[Server] Listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("[Server] ListenAndServe error: %v", err)
	}
}
