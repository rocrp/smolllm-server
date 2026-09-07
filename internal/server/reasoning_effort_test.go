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

// upstreamCall is what one leg actually sent on the wire.
type upstreamCall struct {
	Model  string
	Effort any // nil when the leg sent no reasoning_effort at all
}

// newEffortRig fakes a provider that records every call and fails each model in
// `fail` with a 401, which smolllm-go classifies as leg-local so the chain
// advances to the next candidate.
func newEffortRig(t *testing.T, alias string, fail ...string) (*httptest.Server, func() []upstreamCall) {
	t.Helper()

	var (
		mu    sync.Mutex
		calls []upstreamCall
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		model, _ := body["model"].(string)

		mu.Lock()
		calls = append(calls, upstreamCall{Model: model, Effort: body["reasoning_effort"]})
		mu.Unlock()

		for _, failing := range fail {
			if model == failing {
				http.Error(w, "denied", http.StatusUnauthorized)
				return
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"42"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "error"},
		Aliases: map[string]string{"fast": alias},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(config.NewStore("", cfg), logger).HTTP.Handler)
	t.Cleanup(ts.Close)

	return ts, func() []upstreamCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamCall(nil), calls...)
	}
}

// The reason `!effort` survives in the server's config grammar: each leg of one
// chain can carry its own reasoning budget, which smolllm-go's chain-wide
// WithReasoningEffort cannot express.
func TestChatCompletions_SendsPerLegReasoningEffort(t *testing.T) {
	ts, recorded := newEffortRig(t, "mock/alpha!none,mock/beta!low,mock/gamma", "alpha", "beta")

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, []upstreamCall{
		{Model: "alpha", Effort: "none"},
		{Model: "beta", Effort: "low"},
		{Model: "gamma", Effort: nil},
	}, recorded(), "each leg carries its own effort, and a leg with no suffix sends none")
}

// The suffix never reaches the wire model name: smolllm-go v0.3 sends everything
// after the first "/" verbatim, so a leaked "!low" would 404.
func TestChatCompletions_StripsEffortFromWireModel(t *testing.T) {
	ts, recorded := newEffortRig(t, "mock/marvin-7b!high")

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	calls := recorded()
	require.Len(t, calls, 1)
	require.Equal(t, "marvin-7b", calls[0].Model)
	require.Equal(t, "high", calls[0].Effort)
}

// A request-level reasoning_effort sets the chain-wide value; a leg that named
// its own in config keeps it. The two live in different smolllm-go fields, so
// the precedence does not depend on the order the options are applied.
func TestChatCompletions_PerLegEffortWinsOverRequestField(t *testing.T) {
	ts, recorded := newEffortRig(t, "mock/alpha!none,mock/beta", "alpha")

	resp := postChat(t, ts,
		`{"model":"fast","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, []upstreamCall{
		{Model: "alpha", Effort: "none"},
		{Model: "beta", Effort: "high"},
	}, recorded(), "the suffixed leg keeps its own effort; the bare leg takes the request's")
}

// A model string sent straight through, bypassing aliases, uses the same grammar.
func TestChatCompletions_AcceptsEffortSuffixOnDirectModel(t *testing.T) {
	ts, recorded := newEffortRig(t, "mock/unused")

	resp := postChat(t, ts,
		`{"model":"mock/alpha!medium","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	calls := recorded()
	require.Len(t, calls, 1)
	require.Equal(t, "alpha", calls[0].Model)
	require.Equal(t, "medium", calls[0].Effort)
}

func TestEmbeddings_SendsPerLegReasoningEffort(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"index": 0, "embedding": []float64{0.1, 0.2}}},
			"model": "embed-1",
			"usage": map[string]int{"prompt_tokens": 2, "total_tokens": 2},
		}))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "error"},
		Aliases: map[string]string{"vectors": "mock/embed-1!none"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(config.NewStore("", cfg), logger).HTTP.Handler)
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/embeddings",
		strings.NewReader(`{"model":"vectors","input":"hi"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, "embed-1", captured["model"], "the suffix must not reach the wire model name")
	require.Equal(t, "none", captured["reasoning_effort"])
}
