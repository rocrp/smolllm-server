package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// serve runs one request through mw and reports the status and the key name
// the handler saw ("" when it never ran).
func serve(mw func(http.Handler) http.Handler, seen *string, header string) int {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	*seen = ""
	mw(ok).ServeHTTP(rec, req)
	return rec.Code
}

func TestMiddleware(t *testing.T) {
	t.Parallel()
	keys := map[string]string{"laptop": "k-laptop", "rctlsr": "k-rctlsr"}

	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantKey    string
	}{
		{"missing header", "", http.StatusUnauthorized, ""},
		{"third string", "Bearer k-other", http.StatusUnauthorized, ""},
		{"prefix of a key", "Bearer k-rct", http.StatusUnauthorized, ""},
		{"first key", "Bearer k-laptop", http.StatusOK, "laptop"},
		{"second key", "Bearer k-rctlsr", http.StatusOK, "rctlsr"},
		{"without bearer", "k-rctlsr", http.StatusOK, "rctlsr"},
		{"lowercase bearer", "bearer k-laptop", http.StatusOK, "laptop"},
		{"empty bearer", "Bearer ", http.StatusUnauthorized, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			mw := Middleware(func() map[string]string { return keys },
				func(_ *http.Request, name string) { seen = name })
			require.Equal(t, tc.wantStatus, serve(mw, &seen, tc.header))
			require.Equal(t, tc.wantKey, seen)
		})
	}
}

func TestMiddleware_RevokedKeyIs401(t *testing.T) {
	t.Parallel()
	current := map[string]string{"laptop": "k-laptop", "rctlsr": "k-rctlsr"}
	var seen string
	mw := Middleware(func() map[string]string { return current },
		func(_ *http.Request, name string) { seen = name })

	require.Equal(t, http.StatusOK, serve(mw, &seen, "Bearer k-rctlsr"))
	current = map[string]string{"laptop": "k-laptop"} // a reload dropped rctlsr
	require.Equal(t, http.StatusUnauthorized, serve(mw, &seen, "Bearer k-rctlsr"))
	require.Equal(t, http.StatusOK, serve(mw, &seen, "Bearer k-laptop"))
	require.Equal(t, "laptop", seen)
}

func TestExtractToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, out string
	}{
		{"", ""},
		{"   ", ""},
		{"rocry", "rocry"},
		{"Bearer rocry", "rocry"},
		{"bearer rocry", "rocry"},
		{"  Bearer  rocry  ", "rocry"},
	}
	for _, tc := range tests {
		require.Equal(t, tc.out, extractToken(tc.in), "input=%q", tc.in)
	}
}
