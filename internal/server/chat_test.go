package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/llm"
	"github.com/stretchr/testify/require"
)

// Sets up a fake OpenAI-compatible upstream provider, points smolllm-go at it
// via env vars, and returns an *httptest.Server hosting our smolllm-server
// handlers. Caller closes both servers via t.Cleanup.
//
// `streaming` controls whether the upstream returns SSE.
func newTestRig(t *testing.T, streaming bool) (*httptest.Server, *config.Config) {
	t.Helper()

	// smolllm-go always sends `stream: true` upstream and reassembles into a
	// Response when the caller used Ask(). So our mock provider always emits SSE.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer secret-mock-key", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "marvin-7b", body["model"])

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		var frames []string
		if !streaming {
			// Single content chunk → reassembles to "42" via Ask.
			frames = []string{
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{"role":"assistant","content":"42"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			}
		} else {
			frames = []string{
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`,
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			}
		}
		for _, f := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:      "127.0.0.1:0",
			AccessKey: "rocry",
			LogLevel:  "warn",
		},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)
	return ts, cfg
}

func TestChatCompletions_Blocking(t *testing.T) {
	ts, _ := newTestRig(t, false)

	body := `{"model":"fast","messages":[{"role":"user","content":"what is the answer?"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out llm.ChatCompletion
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "chat.completion", out.Object)
	require.Len(t, out.Choices, 1)
	require.Equal(t, "assistant", out.Choices[0].Message.Role)
	require.Equal(t, "42", out.Choices[0].Message.Content)
	require.Equal(t, "stop", out.Choices[0].FinishReason)
}

func TestChatCompletions_Streaming(t *testing.T) {
	ts, _ := newTestRig(t, true)

	body := `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	var collected bytes.Buffer
	var doneSeen bool
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		require.True(t, strings.HasPrefix(line, "data: "), "line=%q", line)
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			doneSeen = true
			break
		}
		var chunk llm.ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk))
		require.Len(t, chunk.Choices, 1)
		collected.WriteString(chunk.Choices[0].Delta.Content)
	}
	require.NoError(t, scanner.Err())
	require.True(t, doneSeen, "expected [DONE] frame")
	require.Equal(t, "hello", collected.String())
}

func TestChatCompletions_RejectsTools(t *testing.T) {
	ts, _ := newTestRig(t, false)

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Contains(t, env.Error.Message, "tools are not yet supported")
}

func TestChatCompletions_AuthRequired(t *testing.T) {
	ts, _ := newTestRig(t, false)

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHealthz_NoAuth(t *testing.T) {
	ts, _ := newTestRig(t, false)
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestModels_ListsAliases(t *testing.T) {
	ts, _ := newTestRig(t, false)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer rocry")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out llm.ModelsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "list", out.Object)
	require.Len(t, out.Data, 1)
	require.Equal(t, "fast", out.Data[0].ID)
	require.Equal(t, "smolllm-alias", out.Data[0].OwnedBy)
}
