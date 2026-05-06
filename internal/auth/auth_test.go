package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMiddleware(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(func() string { return "rocry" })(ok)

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"correct token with bearer", "Bearer rocry", http.StatusOK},
		{"correct token without bearer", "rocry", http.StatusOK},
		{"correct token lowercase bearer", "bearer rocry", http.StatusOK},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			require.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestMiddleware_KeyFnHotRotation(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	current := "old"
	mw := Middleware(func() string { return current })(ok)

	doReq := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusOK, doReq("old"))
	require.Equal(t, http.StatusUnauthorized, doReq("new"))

	current = "new" // simulate SIGHUP reload
	require.Equal(t, http.StatusUnauthorized, doReq("old"))
	require.Equal(t, http.StatusOK, doReq("new"))
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
