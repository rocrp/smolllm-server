package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/rocry/smolllm-server/internal/apierr"
)

// statusRecorder captures status code for logging without buffering the body —
// streaming endpoints rely on the body passing through unchanged.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

func recoverMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"path", r.URL.Path,
						"panic", rec,
						"stack", string(debug.Stack()),
					)
					apierr.Write(w, http.StatusInternalServerError, "internal_error", "server_error", "Internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
