package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/rocry/smolllm-server/internal/apierr"
)

// Middleware verifies the Authorization header against the access key.
// Accepts either "Bearer <token>" or a bare "<token>".
func Middleware(accessKey string) func(http.Handler) http.Handler {
	expected := []byte(accessKey)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r.Header.Get("Authorization"))
			if token == "" {
				apierr.Write(w, http.StatusUnauthorized, "missing_api_key", "invalid_request_error", "Missing API key in Authorization header")
				return
			}
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
