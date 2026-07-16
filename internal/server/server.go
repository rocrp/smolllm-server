package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/rocry/smolllm-server/internal/auth"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
)

// Server bundles the HTTP server with its dependencies for graceful shutdown.
// Bind is captured at listen time and is NOT hot-reloadable; everything else
// is read fresh from Store on every request.
type Server struct {
	HTTP   *http.Server
	Store  *config.Store
	Logger *slog.Logger
	bind   string // captured at construction
}

// New builds the HTTP server, mux, and middleware chain. The store is the
// single source of truth for live config; pass the same store to the SIGHUP
// reloader in main so handlers see updates without a restart.
func New(store *config.Store, logger *slog.Logger) *Server {
	cfg := store.Get()

	mux := http.NewServeMux()
	h := &handlers{store: store, logger: logger, ledger: ledger.New()}

	// Public routes (no auth)
	mux.HandleFunc("GET /healthz", h.health)

	// Protected routes — auth middleware reads the access key from the store
	// per request, so SIGHUP-driven access_key rotation takes effect immediately.
	authMW := auth.Middleware(func() string { return store.Get().Server.AccessKey })
	mux.Handle("POST /v1/chat/completions", authMW(http.HandlerFunc(h.chat)))
	mux.Handle("POST /v1/embeddings", authMW(http.HandlerFunc(h.embeddings)))
	mux.Handle("GET /v1/models", authMW(http.HandlerFunc(h.models)))
	mux.Handle("GET /stats", authMW(http.HandlerFunc(h.stats)))

	wrapped := chain(mux, recoverMW(logger), logMW(logger))

	return &Server{
		HTTP: &http.Server{
			Addr:              cfg.Server.Bind,
			Handler:           wrapped,
			ReadHeaderTimeout: 10 * time.Second,
			// No write timeout: streaming responses can run for minutes.
		},
		Store:  store,
		Logger: logger,
		bind:   cfg.Server.Bind,
	}
}

// Bind returns the address the server is listening on. Use this to detect
// post-reload bind drift (which requires a process restart).
func (s *Server) Bind() string { return s.bind }

// Run starts the HTTP server and blocks until ctx is canceled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		cfg := s.Store.Get()
		s.Logger.Info("server listening", "bind", s.bind, "aliases", len(cfg.Aliases))
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
