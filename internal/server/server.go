package server

import (
	"caller/internal/api/handlers"
	"caller/internal/config"
	"context"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

// Server wraps the underlying HTTP server.
type Server struct {
	httpServer *http.Server
}

// New constructs a new Server with routes wired up.
func New(cfg *config.Config) (*Server, error) {
	// WebSocket upgrader for Twilio media stream
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// Twilio connects from various IPs; typically allow all origins here.
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

	// Simple health check (useful for Render)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	httpSrv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	log.Printf("[Server] Initialised on :%s", port)

	return &Server{
		httpServer: httpSrv,
	}, nil
}

// Start begins serving HTTP requests.
func (s *Server) Start() error {
	log.Printf("[Server] Listening on %s", s.httpServer.Addr)
	// ListenAndServe returns http.ErrServerClosed on graceful shutdown,
	// which we usually don't treat as a fatal error.
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("[Server] Shutting down...")
	return s.httpServer.Shutdown(ctx)
}
