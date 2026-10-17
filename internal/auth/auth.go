package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/rocry/smolllm-server/internal/apierr"
)

// Middleware verifies the Authorization header against the named keys returned
// by keysFn, then hands the matching key's name to onAuth before the handler
// runs. keysFn is invoked per request so keys can be added or revoked via
// config reload without restarting the server.
// Accepts either "Bearer <token>" or a bare "<token>".
func Middleware(keysFn func() map[string]string, onAuth func(r *http.Request, name string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r.Header.Get("Authorization"))
			if token == "" {
				apierr.Write(w, http.StatusUnauthorized, "missing_api_key", "invalid_request_error", "Missing API key in Authorization header")
				return
			}
			name, ok := match(token, keysFn())
			if !ok {
				apierr.Write(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "Invalid API key")
				return
			}
			onAuth(r, name)
			next.ServeHTTP(w, r)
		})
	}
}

// match compares the token with every key, never stopping early, so the time
// taken says nothing about which key came close. Config validation keeps keys
// unique, so at most one matches.
func match(token string, keys map[string]string) (string, bool) {
	found := ""
	for name, key := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
			found = name
		}
	}
	return found, found != ""
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
