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
	"path/filepath"
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
func newTestRig(t *testing.T, streaming bool, inspectRequest ...func(map[string]any)) (*httptest.Server, *config.Config) {
	t.Helper()

	// smolllm-go always sends `stream: true` upstream and reassembles into a
	// Response when the caller used Ask(). So our mock provider always emits SSE.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer secret-mock-key", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "marvin-7b", body["model"])
		for _, inspect := range inspectRequest {
			inspect(body)
		}

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
			UsagePath: filepath.Join(t.TempDir(), "usage.jsonl"),
		},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", cfg)
	srv := New(store, logger)
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
	require.Equal(t, 3, out.Usage.PromptTokens)
	require.Equal(t, 1, out.Usage.CompletionTokens)
}

func TestChatCompletions_ForwardsCommonGenerationParams(t *testing.T) {
	var captured map[string]any
	ts, _ := newTestRig(t, false, func(body map[string]any) {
		captured = body
	})

	body := `{"model":"fast","max_tokens":128,"stop":["END","STOP"],"seed":42,"messages":[{"role":"user","content":"what is the answer?"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.NotNil(t, captured)
	require.InDelta(t, 128, captured["max_tokens"], 1e-9)
	require.Equal(t, []any{"END", "STOP"}, captured["stop"])
	require.InDelta(t, 42, captured["seed"], 1e-9)
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

func TestChatCompletions_StreamingWaitErrorUsesTerminalChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: {bad-json}\n\n")
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
			UsagePath: filepath.Join(t.TempDir(), "usage.jsonl"),
		},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(config.NewStore("", cfg), logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)

	body := `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var terminal struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	var doneSeen bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			doneSeen = true
			break
		}
		var frame struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &frame))
		if frame.Error != nil {
			terminal = frame
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, doneSeen)
	require.NotNil(t, terminal.Error)
	require.Contains(t, terminal.Error.Message, "malformed streaming chunk")
	require.Len(t, terminal.Choices, 1)
	require.NotNil(t, terminal.Choices[0].FinishReason)
	require.Equal(t, "error", *terminal.Choices[0].FinishReason)
}

func TestStats_AggregatesUsageMeter(t *testing.T) {
	ts, cfg := newTestRig(t, false)

	body := `{"model":"fast","messages":[{"role":"user","content":"what is the answer?"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/v1/stats?days=7", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Alias        string `json:"alias"`
			Provider     string `json:"provider"`
			Requests     int    `json:"requests"`
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "usage_stats", out.Object)
	require.Len(t, out.Data, 1)
	require.Equal(t, "fast", out.Data[0].Alias)
	require.Equal(t, "mock", out.Data[0].Provider)
	require.Equal(t, 1, out.Data[0].Requests)
	require.Equal(t, 3, out.Data[0].InputTokens)
	require.Equal(t, 1, out.Data[0].OutputTokens)
	require.FileExists(t, cfg.Server.UsagePath)
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
