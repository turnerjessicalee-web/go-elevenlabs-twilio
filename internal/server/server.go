package server

import (
    "context"
    "fmt"
    "net/http"
    "os"

    "caller/internal/api"
    "caller/internal/config"

    "github.com/gorilla/websocket"
)

type Server struct {
    cfg      *config.Config
    srv      *http.Server
    upgrader websocket.Upgrader
}

func New(cfg *config.Config) (*Server, error) {
    s := &Server{
        cfg: cfg,
        upgrader: websocket.Upgrader{
            CheckOrigin: func(r *http.Request) bool {
                return true // allow all origins for now
            },
        },
    }

    router := api.NewRouter(s.cfg, s.upgrader)

    // Use Render's PORT if present, otherwise fall back to cfg.Port (10000 for local dev)
    port := os.Getenv("PORT")
    if port == "" {
        port = cfg.Port
    }

    s.srv = &http.Server{
        Addr:    "0.0.0.0:" + port,
        Handler: router,
    }

    return s, nil
}

func (s *Server) Start() error {
    fmt.Printf("[Server] Listening on port %s\n", s.srv.Addr)
    return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
    return s.srv.Shutdown(ctx)
}
