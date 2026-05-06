package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/rocry/smolllm-server/internal/apierr"
)

// Middleware verifies the Authorization header against the access key
// returned by keyFn. keyFn is invoked per request so the access key can be
// hot-rotated via config reload without restarting the server.
// Accepts either "Bearer <token>" or a bare "<token>".
func Middleware(keyFn func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r.Header.Get("Authorization"))
			if token == "" {
				apierr.Write(w, http.StatusUnauthorized, "missing_api_key", "invalid_request_error", "Missing API key in Authorization header")
				return
			}
			expected := []byte(keyFn())
			if subtle.ConstantTimeCompare([]byte(token), expected) != 1 {
				apierr.Write(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "Invalid API key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[len("bearer "):])
	}
	return header
}
