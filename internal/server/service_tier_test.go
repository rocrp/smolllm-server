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

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/stretchr/testify/require"
)

func TestChatCompletions_ServiceTierFollowsWinningCodexLeg(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		chain     string
		failModel string
		wantModel string
		wantTier  string
		wantCalls []string
	}{
		{
			name:      "fast",
			chain:     "smolayer/codex/gpt-6-luna@fast!low",
			wantModel: "smolayer/codex/gpt-6-luna@fast",
			wantTier:  "fast",
			wantCalls: []string{"codex/gpt-6-luna@fast"},
		},
		{
			name:      "Groq fails then fast Codex wins",
			chain:     "groq/openai/gpt-oss-120b!low,smolayer/codex/gpt-6-luna@fast!low",
			failModel: "openai/gpt-oss-120b",
			wantModel: "smolayer/codex/gpt-6-luna@fast",
			wantTier:  "fast",
			wantCalls: []string{"openai/gpt-oss-120b", "codex/gpt-6-luna@fast"},
		},
		{
			name:      "default",
			chain:     "smolayer/codex/gpt-6-luna",
			wantModel: "smolayer/codex/gpt-6-luna",
			wantTier:  "default",
			wantCalls: []string{"codex/gpt-6-luna"},
		},
		{
			name:      "fallback to other provider",
			chain:     "smolayer/codex/gpt-6-luna@fast!low,mock/other",
			failModel: "codex/gpt-6-luna@fast",
			wantModel: "mock/other",
			wantCalls: []string{"codex/gpt-6-luna@fast", "other"},
		},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				t.Parallel()
				var mu sync.Mutex
				var calls []string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Model           string `json:"model"`
						ReasoningEffort string `json:"reasoning_effort"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					mu.Lock()
					calls = append(calls, body.Model)
					mu.Unlock()
					if (body.Model == "codex/gpt-6-luna@fast" || body.Model == "openai/gpt-oss-120b") && body.ReasoningEffort != "low" {
						http.Error(w, "leg lost !low", http.StatusBadRequest)
						return
					}
					if body.Model == tc.failModel {
						w.WriteHeader(http.StatusRequestEntityTooLarge)
						_, _ = fmt.Fprint(w, `{"error":{"message":"TPM Limit 8000, Requested 8732","type":"tokens","code":"rate_limit_exceeded"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
					_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
					_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				}))
				t.Cleanup(upstream.Close)
				cfg := &config.Config{
					Server:  config.ServerConfig{AccessKey: "test-client-key"},
					Aliases: map[string]string{"alias": tc.chain},
				}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				provider := smolllm.ProviderConfig{BaseURL: upstream.URL, APIKey: "test-key"}
				h := &handlers{
					store:  config.NewStore("", cfg),
					logger: logger,
					ledger: ledger.New(),
					client: smolllm.New(
						smolllm.WithLogger(logger),
						smolllm.WithProvider("smolayer", provider),
						smolllm.WithProvider("groq", provider),
						smolllm.WithProvider("mock", provider),
					),
				}
				ts := httptest.NewServer(http.HandlerFunc(h.chat))
				t.Cleanup(ts.Close)
				req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
					strings.NewReader(fmt.Sprintf(`{"model":"alias","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, streaming)))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer test-client-key")
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				payload, err := io.ReadAll(resp.Body)
				require.NoError(t, err)

				var frames []string
				if streaming {
					for _, line := range strings.Split(string(payload), "\n") {
						if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
							frames = append(frames, strings.TrimPrefix(line, "data: "))
						}
					}
					require.Contains(t, string(payload), "data: [DONE]")
				} else {
					frames = []string{string(payload)}
				}
				require.NotEmpty(t, frames)
				if streaming {
					require.GreaterOrEqual(t, len(frames), 2)
				}
				for _, frame := range frames {
					var decoded map[string]any
					require.NoError(t, json.Unmarshal([]byte(frame), &decoded))
					require.Equal(t, tc.wantModel, decoded["model"])
					if tc.wantTier == "" {
						require.NotContains(t, decoded, "service_tier")
					} else {
						require.Equal(t, tc.wantTier, decoded["service_tier"])
					}
				}
				mu.Lock()
				defer mu.Unlock()
				require.Equal(t, tc.wantCalls, calls)
			})
		}
	}
}
