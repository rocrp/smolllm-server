package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/require"
)

// A WorkBuddy per-Model credit limit advances to the next configured leg.
func TestChatCompletions_WorkBuddy429Advances(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[streaming], func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var calls []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				calls = append(calls, body.Model)
				mu.Unlock()
				if body.Model == "workbuddy/glm-5.3-flash" {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"model credit limit","type":"rate_limit_error","code":"6004"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fallback answer\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(upstream.Close)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			provider := smolllm.ProviderConfig{BaseURL: upstream.URL, APIKey: "test-key"}
			cfg := &config.Config{
				Server: config.ServerConfig{AccessKey: "test-client-key"},
				Aliases: map[string]string{
					"explain": "smolayer/workbuddy/glm-5.3-flash!low,smolayer/codex/gpt-6-luna!low",
				},
			}
			h := &handlers{
				store:  config.NewStore("", cfg),
				logger: logger,
				ledger: ledger.New(),
				client: smolllm.New(
					smolllm.WithLogger(logger),
					smolllm.WithProvider("smolayer", provider),
				),
			}
			server := httptest.NewServer(http.HandlerFunc(h.chat))
			t.Cleanup(server.Close)
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions",
				strings.NewReader(map[bool]string{
					false: `{"model":"explain","messages":[{"role":"user","content":"hi"}]}`,
					true:  `{"model":"explain","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
				}[streaming]))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer test-client-key")
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			answer, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(answer))
			require.Contains(t, string(answer), "fallback answer")
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, []string{"workbuddy/glm-5.3-flash", "codex/gpt-6-luna"}, calls)
		})
	}
}
