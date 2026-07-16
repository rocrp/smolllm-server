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
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/rocry/smolllm-server/internal/llm"
	"github.com/stretchr/testify/require"
)

// Sets up a fake OpenAI-compatible upstream provider, points smolllm-go at it
// via env vars, and returns an *httptest.Server hosting our smolllm-server
// handlers. Caller closes both servers via t.Cleanup.
//
// `streaming` controls the response fixture shape.
func newTestRig(t *testing.T, streaming bool, inspectRequest ...func(map[string]any)) (*httptest.Server, *config.Config) {
	t.Helper()
	return newTestRigWithFinishReason(t, streaming, "stop", inspectRequest...)
}

func newTestRigWithFinishReason(
	t *testing.T,
	streaming bool,
	providerFinishReason string,
	inspectRequest ...func(map[string]any),
) (*httptest.Server, *config.Config) {
	t.Helper()
	terminalChoice := `{"index":0,"delta":{}}`
	if providerFinishReason != "" {
		terminalChoice = fmt.Sprintf(`{"index":0,"delta":{},"finish_reason":%q}`, providerFinishReason)
	}

	// smolllm-go always sends `stream: true` upstream and reassembles into a
	// Response when the caller used Ask(). So our mock provider always emits SSE.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer secret-mock-key", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "marvin-7b", body["model"])
		for _, inspect := range inspectRequest {
			if inspect != nil {
				inspect(body)
			}
		}
		if r.URL.Path == "/v1/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"data":  []map[string]any{{"index": 0, "embedding": []float64{0.1, 0.2}}},
				"model": "marvin-7b",
				"usage": map[string]int{"prompt_tokens": 2, "total_tokens": 2},
			}))
			return
		}
		require.Equal(t, "/v1/chat/completions", r.URL.Path)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		var frames []string
		if !streaming {
			// Single content chunk → reassembles to "42" via Ask.
			frames = []string{
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{"role":"assistant","content":"42"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
				fmt.Sprintf(`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[%s]}`, terminalChoice),
			}
		} else {
			frames = []string{
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`,
				`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
				fmt.Sprintf(`{"id":"x","object":"chat.completion.chunk","model":"marvin-7b","choices":[%s],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, terminalChoice),
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
	store := config.NewStore("", cfg)
	srv := New(store, logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)
	return ts, cfg
}

func TestChatCompletions_Blocking(t *testing.T) {
	ts, _ := newTestRig(t, false, func(body map[string]any) {
		require.NotContains(t, body, "max_tokens")
	})

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

func TestChatCompletions_AcceptsNullishOptionalFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "stop null",
			body: `{"model":"fast","stop":null,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "stop empty array",
			body: `{"model":"fast","stop":[],"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "tools null",
			body: `{"model":"fast","tools":null,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newTestRig(t, false)
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer rocry")
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestChatCompletions_RejectsEmptyStopString(t *testing.T) {
	ts, _ := newTestRig(t, false)

	body := `{"model":"fast","stop":"","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestChatCompletions_Streaming(t *testing.T) {
	ts, _ := newTestRig(t, true, nil)

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

func TestChatCompletions_ReportsProviderFinishReason(t *testing.T) {
	tests := []struct {
		name           string
		stream         bool
		providerReason string
		want           string
	}{
		{name: "blocking length", providerReason: "length", want: "length"},
		{name: "streaming length", stream: true, providerReason: "length", want: "length"},
		{name: "blocking omitted", want: "stop"},
		{name: "streaming omitted", stream: true, want: "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := newTestRigWithFinishReason(t, tt.stream, tt.providerReason)
			body := fmt.Sprintf(
				`{"model":"fast","stream":%t,"messages":[{"role":"user","content":"hi"}]}`,
				tt.stream,
			)
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer rocry")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			if !tt.stream {
				var out llm.ChatCompletion
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
				require.Len(t, out.Choices, 1)
				require.Equal(t, tt.want, out.Choices[0].FinishReason)
				return
			}

			var got string
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				payload := strings.TrimPrefix(scanner.Text(), "data: ")
				if payload == "" || payload == "[DONE]" {
					continue
				}
				var chunk llm.ChatCompletionChunk
				require.NoError(t, json.Unmarshal([]byte(payload), &chunk))
				if len(chunk.Choices) == 1 && chunk.Choices[0].FinishReason != nil {
					got = *chunk.Choices[0].FinishReason
				}
			}
			require.NoError(t, scanner.Err())
			require.Equal(t, tt.want, got)
		})
	}
}

func TestChatCompletions_RejectsTools(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)

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
	ts, _ := newTestRig(t, false, nil)

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHealthz_NoAuth(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestModels_ListsAliases(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)
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

func TestChatCompletions_RejectsNonPositiveMaxTokens(t *testing.T) {
	tests := []struct {
		name      string
		maxTokens int
	}{
		{name: "zero", maxTokens: 0},
		{name: "negative", maxTokens: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := newTestRig(t, false, nil)
			body := fmt.Sprintf(`{"model":"fast","max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, tt.maxTokens)
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer rocry")
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)

			var env struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
			require.Equal(t, "invalid_request_error", env.Error.Type)
			require.Contains(t, env.Error.Message, "max_tokens must be positive")
		})
	}
}

func TestStats_AggregatesChatAndEmbeddingAttempts(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)

	requests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/chat/completions", body: `{"model":"mock/marvin-7b","messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/embeddings", body: `{"model":"fast","input":"hi"}`},
	}
	for _, request := range requests {
		req, err := http.NewRequest(http.MethodPost, ts.URL+request.path, strings.NewReader(request.body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer rocry")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
	}

	unauthorized, err := http.Get(ts.URL + "/stats")
	require.NoError(t, err)
	defer unauthorized.Body.Close()
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/stats", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Buckets []ledger.Bucket `json:"buckets"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Buckets, 2)
	day := out.Buckets[0].Day
	require.Equal(t, []ledger.Bucket{
		{
			Day:          day,
			Alias:        "fast",
			Provider:     "mock",
			Model:        "marvin-7b",
			Requests:     3,
			InputTokens:  8,
			OutputTokens: 2,
		},
		{
			Day:          day,
			Alias:        "mock/marvin-7b",
			Provider:     "mock",
			Model:        "marvin-7b",
			Requests:     1,
			InputTokens:  3,
			OutputTokens: 1,
		},
	}, out.Buckets)
}

func TestStats_RecordsFailedAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "warn"},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(config.NewStore("", cfg), logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)

	requests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`},
		{path: "/v1/embeddings", body: `{"model":"fast","input":"hi"}`},
	}
	for _, request := range requests {
		failedReq, err := http.NewRequest(http.MethodPost, ts.URL+request.path, strings.NewReader(request.body))
		require.NoError(t, err)
		failedReq.Header.Set("Authorization", "Bearer rocry")
		failedReq.Header.Set("Content-Type", "application/json")
		failedResp, err := http.DefaultClient.Do(failedReq)
		require.NoError(t, err)
		defer failedResp.Body.Close()
		require.Equal(t, http.StatusBadGateway, failedResp.StatusCode)
	}

	statsReq, err := http.NewRequest(http.MethodGet, ts.URL+"/stats", nil)
	require.NoError(t, err)
	statsReq.Header.Set("Authorization", "Bearer rocry")
	statsResp, err := http.DefaultClient.Do(statsReq)
	require.NoError(t, err)
	defer statsResp.Body.Close()
	require.Equal(t, http.StatusOK, statsResp.StatusCode)

	var out struct {
		Buckets []ledger.Bucket `json:"buckets"`
	}
	require.NoError(t, json.NewDecoder(statsResp.Body).Decode(&out))
	require.Len(t, out.Buckets, 1)
	require.Equal(t, "fast", out.Buckets[0].Alias)
	require.Equal(t, "mock", out.Buckets[0].Provider)
	require.Equal(t, "marvin-7b", out.Buckets[0].Model)
	require.Equal(t, 2, out.Buckets[0].Requests)
	require.Equal(t, 2, out.Buckets[0].Failures)
	require.Equal(t, 2, out.Buckets[0].EstimatedRequests)
}
