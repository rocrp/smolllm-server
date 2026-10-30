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

// smolayer's answer to any response_format on a WorkBuddy leg, captured from
// smolayer 67f8bec on 2026-10-30. Qoder's wire.rs rejects it the same way.
const smolayerFormatRefusal = `{"error":{"message":"workbuddy does not promise JSON schema output",` +
	`"type":"invalid_request_error","param":"response_format","code":null}}`

// A JSON caller on an alias that leads with a formatless smolayer leg is served
// by the next leg instead of failing on that leg's 400.
func TestChatCompletions_ResponseFormatSkipsFormatlessLegs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		format string
		want   []string
	}{
		{"json_object", `,"response_format":{"type":"json_object"}`, []string{"codex/gpt-6-luna"}},
		{"text", `,"response_format":{"type":"text"}`, []string{"workbuddy/hy3"}},
		{"absent", ``, []string{"workbuddy/hy3"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var calls []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model          string         `json:"model"`
					ResponseFormat map[string]any `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				calls = append(calls, body.Model)
				mu.Unlock()
				formatless := strings.HasPrefix(body.Model, "workbuddy/") || strings.HasPrefix(body.Model, "qoder/")
				if formatless && body.ResponseFormat != nil && body.ResponseFormat["type"] != "text" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, smolayerFormatRefusal)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{}\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(upstream.Close)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			h := &handlers{
				store: config.NewStore("", &config.Config{
					Server: config.ServerConfig{AccessKey: "rocry"},
					Aliases: map[string]string{
						"json": "smolayer/workbuddy/hy3!low,smolayer/qoder/qwen3-coder,smolayer/codex/gpt-6-luna!low",
					},
				}),
				logger: logger,
				ledger: ledger.New(),
				client: smolllm.New(
					smolllm.WithLogger(logger),
					smolllm.WithProvider("smolayer", smolllm.ProviderConfig{BaseURL: upstream.URL, APIKey: "k"}),
				),
			}
			ts := httptest.NewServer(http.HandlerFunc(h.chat))
			t.Cleanup(ts.Close)

			resp := postChat(t, ts, `{"model":"json","messages":[{"role":"user","content":"reply JSON"}]`+tt.format+`}`)
			defer resp.Body.Close()
			answer, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(answer))
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tt.want, calls)
		})
	}
}
