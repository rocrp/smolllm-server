package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/stretchr/testify/require"
)

// A provider's request-size or TPM ceiling does not constrain the next provider.
func TestChatCompletions_PayloadTooLargeFallsBack(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var mu sync.Mutex
			var attempted []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid JSON", 400)
					return
				}
				mu.Lock()
				attempted = append(attempted, body.Model)
				mu.Unlock()
				if body.Model == "alpha" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					fmt.Fprint(w, `{"error":{"message":"Request too large: TPM Limit 8000, Requested 8732","type":"tokens","code":"rate_limit_exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"model\":\"beta\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fallback answer\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"model\":\"beta\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(upstream.Close)
			t.Setenv("MOCK_BASE_URL", upstream.URL)
			t.Setenv("MOCK_API_KEY", "test-key")
			cfg := &config.Config{Server: config.ServerConfig{AccessKey: "rocry"}, Aliases: map[string]string{"fast": "mock/alpha,mock/beta"}}
			ts := httptest.NewServer(New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(io.Discard, nil))).HTTP.Handler)
			t.Cleanup(ts.Close)
			resp := postChat(t, ts, fmt.Sprintf(`{"model":"fast","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, streaming))
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			mu.Lock()
			calls := append([]string(nil), attempted...)
			mu.Unlock()
			require.Equal(t, []string{"alpha", "beta"}, calls)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(data), "fallback answer")
			require.NotContains(t, string(data), `"error":`)
			require.Contains(t, string(data), `"finish_reason":"stop"`)
			if streaming {
				require.True(t, strings.HasSuffix(string(data), "data: [DONE]\n\n"))
			}
		})
	}
}
