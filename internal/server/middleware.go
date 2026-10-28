package server

import (
	"context"
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

// requestNote is what a handler tells the request log line beyond the status
// line: which access key authenticated it (by name), which alias was asked for,
// which model served it, and how a stream that already sent 200 actually ended.
type requestNote struct {
	key   string
	alias string
	model string
	// status overrides the wire status when a stream failed after its 200.
	status int
}

type requestNoteKey struct{}

// noteFor returns the request's note. Every request passing logMW has one; a
// handler run without it (a unit test) gets a throwaway.
func noteFor(r *http.Request) *requestNote {
	if note, ok := r.Context().Value(requestNoteKey{}).(*requestNote); ok {
		return note
	}
	return &requestNote{key: "", alias: "", model: "", status: 0}
}

func logMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			note := &requestNote{key: "", alias: "", model: "", status: 0}
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestNoteKey{}, note)))
			status := rec.status
			if note.status != 0 {
				status = note.status
			}
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			}
			if id := r.Header.Get("X-Request-ID"); id != "" {
				// The caller's own id for the call (Zelna: msg-<tenant>-<thread>-<seq>),
				// so one search finds the call in both services' logs.
				attrs = append(attrs, "request_id", id)
			}
			if note.key != "" {
				attrs = append(attrs, "key", note.key)
			}
			if note.alias != "" {
				attrs = append(attrs, "alias", note.alias, "model", note.model)
			}
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelWarn
			}
			logger.Log(r.Context(), level, "request", attrs...)
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
