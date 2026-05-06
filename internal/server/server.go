package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/rocry/smolllm-server/internal/auth"
	"github.com/rocry/smolllm-server/internal/config"
)

// Server bundles the HTTP server with its dependencies for graceful shutdown.
type Server struct {
	HTTP   *http.Server
	Cfg    *config.Config
	Logger *slog.Logger
}

// New builds the HTTP server, mux, and middleware chain.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	h := &handlers{cfg: cfg, logger: logger}

	// Public routes (no auth)
	mux.HandleFunc("GET /healthz", h.health)

	// Protected routes
	authMW := auth.Middleware(cfg.Server.AccessKey)
	mux.Handle("POST /v1/chat/completions", authMW(http.HandlerFunc(h.chat)))
	mux.Handle("POST /v1/embeddings", authMW(http.HandlerFunc(h.embeddings)))
	mux.Handle("GET /v1/models", authMW(http.HandlerFunc(h.models)))

	wrapped := chain(mux, recoverMW(logger), logMW(logger))

	return &Server{
		HTTP: &http.Server{
			Addr:              cfg.Server.Bind,
			Handler:           wrapped,
			ReadHeaderTimeout: 10 * time.Second,
			// No write timeout: streaming responses can run for minutes.
		},
		Cfg:    cfg,
		Logger: logger,
	}
}

// Run starts the HTTP server and blocks until ctx is canceled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.Logger.Info("server listening", "bind", s.Cfg.Server.Bind, "aliases", len(s.Cfg.Aliases))
		if err := s.HTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Logger.Info("shutting down")
		return s.HTTP.Shutdown(shutdownCtx)
	}
}

func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
