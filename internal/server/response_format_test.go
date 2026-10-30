package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/require"
)

// 400 bodies captured from the live smolayer (67f8bec) on 2026-10-30.
const (
	// Any non-text response_format on a WorkBuddy leg.
	smolayerFormatRefusal = `{"error":{"message":"workbuddy does not promise JSON schema output",` +
		`"type":"invalid_request_error","param":"response_format","code":null}}`
	// Any qoder leg while smolayer holds no qoder login.
	smolayerQoderLoggedOut = "{\"error\":{\"message\":\"unknown provider `qoder`; known providers: codex, " +
		"antigravity, workbuddy, workbuddy-global; `qoder` is not logged in (run `smolayer login qoder`)\"," +
		`"type":"invalid_request_error","param":"model","code":null}}`
)

// smolayer's leg-local 400s, a logged-out qoder and a WorkBuddy that cannot
// honour json_object, hand the call to the next leg instead of failing it.
func TestChatCompletions_LegLocalRefusalsFailOver(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		format string
		want   []string
	}{
		{"json_object", `,"response_format":{"type":"json_object"}`,
			[]string{"qoder/qwen3-coder", "workbuddy/hy3", "codex/gpt-6-luna"}},
		{"text", `,"response_format":{"type":"text"}`, []string{"qoder/qwen3-coder", "workbuddy/hy3"}},
		{"absent", ``, []string{"qoder/qwen3-coder", "workbuddy/hy3"}},
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
				refusal := ""
				switch {
				case strings.HasPrefix(body.Model, "qoder/"):
					refusal = smolayerQoderLoggedOut
				case strings.HasPrefix(body.Model, "workbuddy/") &&
					body.ResponseFormat != nil && body.ResponseFormat["type"] != "text":
					refusal = smolayerFormatRefusal
				}
				if refusal != "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, refusal)
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
						"json": "smolayer/qoder/qwen3-coder,smolayer/workbuddy/hy3!low,smolayer/codex/gpt-6-luna!low",
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
			// smolllm-go re-sends a 400ed leg once without stream_options, so
			// each refusing leg is asked twice in a row; only the order matters.
			require.Equal(t, tt.want, slices.Compact(calls))
		})
	}
}
