package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/auth"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
)

// legBudget caps how long one leg may wait for its response to start, across
// its retries, before the chain moves on. A dead provider cost ~95s before
// (three 30s connect failures); healthy legs start well inside this, and a leg
// that is already streaming is never cut by it.
const legBudget = 45 * time.Second

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
	// One client for the whole process. It owns the key/endpoint balancer, and
	// resolves credentials per leg at call time, so hot-reloading the env file
	// still reaches it. Everything that varies per request - the model chain, the
	// sampling knobs, the ledger hook - is a per-call option.
	//
	// WithTimeout(0): smolllm-go's own 600s whole-call default would kill a
	// longer call although its client holds a bigger budget. The client's deadline
	// and hang-up already bound a call, and a request's `timeout` field still
	// overrides this per call.
	h := &handlers{
		store:  store,
		logger: logger,
		ledger: ledger.New(),
		client: smolllm.New(smolllm.WithLogger(logger), smolllm.WithLegBudget(legBudget), smolllm.WithTimeout(0)),
	}

	// Public routes (no auth)
	mux.HandleFunc("GET /healthz", h.health)

	// Protected routes — auth middleware reads the keys from the store per
	// request, so a reload that adds or revokes a key takes effect immediately.
	// The key's name rides the request note into the log line and the ledger.
	authMW := auth.Middleware(
		func() map[string]string { return store.Get().Keys() },
		func(r *http.Request, name string) { noteFor(r).key = name },
	)
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
