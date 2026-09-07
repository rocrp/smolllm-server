package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/llm"
	"github.com/stretchr/testify/require"
)

// newFailingRig points the server at an upstream that always answers with the
// given status, which is what decides the status the client sees.
func newFailingRig(t *testing.T, upstreamStatus int, body string) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, upstreamStatus)
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "error"},
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(config.NewStore("", cfg), logger).HTTP.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func decodeAPIError(t *testing.T, resp *http.Response) (message, kind string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
	return envelope.Error.Message, envelope.Error.Type
}

// smolllm-go v0.3 classifies each leg failure, and the disposition decides the
// status: an aborting 4xx is the caller's problem, an advancing one is ours.
func TestChatCompletions_MapsUpstreamFailureToStatus(t *testing.T) {
	tests := []struct {
		name           string
		upstreamStatus int
		wantStatus     int
		wantKind       string
	}{
		{
			name:           "bad request aborts the chain and passes through",
			upstreamStatus: http.StatusBadRequest,
			wantStatus:     http.StatusBadRequest,
			wantKind:       "invalid_request_error",
		},
		{
			name:           "unprocessable entity aborts the chain and passes through",
			upstreamStatus: http.StatusUnprocessableEntity,
			wantStatus:     http.StatusUnprocessableEntity,
			wantKind:       "invalid_request_error",
		},
		{
			name:           "unauthorized advances until the chain is exhausted",
			upstreamStatus: http.StatusUnauthorized,
			wantStatus:     http.StatusBadGateway,
			wantKind:       "api_error",
		},
		{
			name:           "rate limit advances until the chain is exhausted",
			upstreamStatus: http.StatusTooManyRequests,
			wantStatus:     http.StatusBadGateway,
			wantKind:       "api_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newFailingRig(t, tt.upstreamStatus, "upstream said no")

			resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
			defer resp.Body.Close()
			require.Equal(t, tt.wantStatus, resp.StatusCode)

			message, kind := decodeAPIError(t, resp)
			require.Equal(t, tt.wantKind, kind)
			// ErrorMessage names every failed leg, so the served model is in there.
			require.Contains(t, message, "mock/marvin-7b")
		})
	}
}

// The error message joins every leg, so a chain that fails everywhere tells the
// caller which candidates were tried rather than only the last one.
func TestChatCompletions_FailureNamesEveryLeg(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "error"},
		Aliases: map[string]string{"fast": "mock/alpha,mock/beta"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(config.NewStore("", cfg), logger).HTTP.Handler)
	t.Cleanup(ts.Close)

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	message, _ := decodeAPIError(t, resp)
	require.Contains(t, message, "mock/alpha")
	require.Contains(t, message, "mock/beta")
}

// A streamed failure arrives after the status line, so it travels as a terminal
// frame rather than an HTTP status.
func TestChatCompletions_StreamingReportsFailureInTerminalFrame(t *testing.T) {
	ts := newFailingRig(t, http.StatusBadRequest, "bad tool schema")

	resp := postChat(t, ts, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var (
		terminal llm.ChatCompletionChunk
		doneSeen bool
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload := strings.TrimPrefix(scanner.Text(), "data: ")
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			doneSeen = true
			break
		}
		var chunk llm.ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk))
		if chunk.Error != nil {
			terminal = chunk
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, doneSeen)
	require.NotNil(t, terminal.Error)
	require.Equal(t, "invalid_request_error", terminal.Error.Type)
	require.Contains(t, terminal.Error.Message, "bad tool schema")
	require.Len(t, terminal.Choices, 1)
	require.NotNil(t, terminal.Choices[0].FinishReason)
	require.Equal(t, "error", *terminal.Choices[0].FinishReason)
}

// WithTimeout now bounds the whole call, fallback legs included, so an expired
// budget is a gateway timeout rather than an upstream error.
func TestChatCompletions_WholeCallTimeoutReports504(t *testing.T) {
	// Never answers, but always returns: httptest.Server.Close waits on its
	// handlers, so a handler that blocks on the client alone can deadlock the test.
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "error"},
		Aliases: map[string]string{"fast": "mock/alpha,mock/beta"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(config.NewStore("", cfg), logger).HTTP.Handler)
	t.Cleanup(ts.Close)

	resp := postChat(t, ts,
		`{"model":"fast","timeout":0.25,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)

	_, kind := decodeAPIError(t, resp)
	require.Equal(t, "api_error", kind)
}

// One spec cannot carry two efforts in a chain, and a request naming such a
// chain directly is rejected the same way a config would be.
func TestChatCompletions_RejectsOneSpecWithTwoEfforts(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)

	resp := postChat(t, ts,
		`{"model":"mock/marvin-7b!low,mock/marvin-7b!high","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	message, kind := decodeAPIError(t, resp)
	require.Equal(t, "invalid_request_error", kind)
	require.Contains(t, message, "two different efforts")
}

func TestEmbeddings_RejectsMalformedChain(t *testing.T) {
	ts, _ := newTestRig(t, false, nil)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/embeddings",
		strings.NewReader(`{"model":"mock/embed-1!","input":"hi"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	message, _ := decodeAPIError(t, resp)
	require.Contains(t, message, "no effort after it")
}

// smolllm-go reports Input with cache reads taken out; the OpenAI surface adds
// them back and reports the cached share under prompt_tokens_details.
func TestChatCompletions_ReportsCachedAndReasoningTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"42"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1000,"completion_tokens":40,"total_tokens":1040,` +
				`"prompt_tokens_details":{"cached_tokens":900},` +
				`"completion_tokens_details":{"reasoning_tokens":25}}}`,
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
		Aliases: map[string]string{"fast": "mock/marvin-7b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(config.NewStore("", cfg), logger).HTTP.Handler)
	t.Cleanup(ts.Close)

	resp := postChat(t, ts, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out llm.ChatCompletion
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, 1000, out.Usage.PromptTokens)
	require.Equal(t, 40, out.Usage.CompletionTokens)
	require.Equal(t, 1040, out.Usage.TotalTokens)
	require.NotNil(t, out.Usage.PromptTokensDetails)
	require.Equal(t, 900, out.Usage.PromptTokensDetails.CachedTokens)
	require.NotNil(t, out.Usage.CompletionTokensDetails)
	require.Equal(t, 25, out.Usage.CompletionTokensDetails.ReasoningTokens)
}
