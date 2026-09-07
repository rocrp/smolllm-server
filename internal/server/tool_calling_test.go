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

	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/llm"
	"github.com/stretchr/testify/require"
)

// newToolCallRig fakes a provider answering with one tool call and no text. The
// frames mirror a real Gemini reply: fragmented argument JSON, a thought
// signature the provider expects echoed back, and finish_reason "stop" rather
// than "tool_calls".
func newToolCallRig(t *testing.T, finishReason string, inspect func(map[string]any)) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if inspect != nil {
			inspect(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[` +
				`{"index":0,"id":"call_1","type":"function",` +
				`"function":{"name":"get_weather","arguments":""},` +
				`"extra_content":{"google":{"thought_signature":"sig-abc"}}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Paris\"}"}}]}}]}`,
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}],`+
				`"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}`, finishReason),
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
		Server: config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "warn"},
		Aliases: map[string]string{
			"agent": "mock/marvin-7b",
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(config.NewStore("", cfg), logger)
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func postChat(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

const toolsField = `"tools":[{"type":"function","function":{"name":"get_weather"}}]`

func TestChatCompletions_BlockingReturnsToolCalls(t *testing.T) {
	ts := newToolCallRig(t, "tool_calls", nil)

	resp := postChat(t, ts, `{"model":"agent","messages":[{"role":"user","content":"weather?"}],`+toolsField+`}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out llm.ChatCompletion
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Choices, 1)
	require.Nil(t, out.Choices[0].Message.Content, "content must be null on a tool-only turn")
	require.Equal(t, "tool_calls", out.Choices[0].FinishReason)
	require.Len(t, out.Choices[0].Message.ToolCalls, 1)

	call := out.Choices[0].Message.ToolCalls[0]
	require.Equal(t, "call_1", call.ID)
	require.Equal(t, "get_weather", call.Function.Name)
	require.JSONEq(t, `{"city":"Paris"}`, call.Function.Arguments)
	require.JSONEq(t, `{"google":{"thought_signature":"sig-abc"}}`, string(call.Extra["extra_content"]))
}

func TestChatCompletions_StreamingEmitsToolCallFragments(t *testing.T) {
	ts := newToolCallRig(t, "stop", nil)

	resp := postChat(t, ts,
		`{"model":"agent","stream":true,"messages":[{"role":"user","content":"weather?"}],`+toolsField+`}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Each frame carrying tool calls, decoded loosely: only the opening frame
	// sets id, type and name, so a typed struct would hide which frame did what.
	type rawFrame struct {
		Choices []struct {
			Delta struct {
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}

	var (
		toolFrames []map[string]any
		lastFrame  rawFrame
		doneSeen   bool
	)
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
		var frame rawFrame
		require.NoError(t, json.Unmarshal([]byte(payload), &frame))
		require.Len(t, frame.Choices, 1)
		toolFrames = append(toolFrames, frame.Choices[0].Delta.ToolCalls...)
		lastFrame = frame
	}
	require.NoError(t, scanner.Err())
	require.True(t, doneSeen)

	// The upstream fragments its arguments across two frames, so the client sees
	// the same shape a provider would send: a header frame, then argument text.
	require.Len(t, toolFrames, 3)

	header := toolFrames[0]
	require.Equal(t, float64(0), header["index"])
	require.Equal(t, "call_1", header["id"])
	require.Equal(t, "function", header["type"])
	require.Contains(t, header, "extra_content",
		"Gemini expects its thought signature echoed back on the next turn")
	require.Equal(t,
		map[string]any{"name": "get_weather", "arguments": ""},
		header["function"])

	var arguments strings.Builder
	for _, frame := range toolFrames[1:] {
		require.Equal(t, float64(0), frame["index"])
		require.NotContains(t, frame, "id", "only the opening frame identifies the call")
		function, ok := frame["function"].(map[string]any)
		require.True(t, ok)
		text, ok := function["arguments"].(string)
		require.True(t, ok)
		arguments.WriteString(text)
	}
	require.JSONEq(t, `{"city":"Paris"}`, arguments.String())

	// The terminal frame reports the OpenAI-shaped reason and carries no call of
	// its own - Gemini says "stop" while returning tool calls.
	require.NotNil(t, lastFrame.Choices[0].FinishReason)
	require.Equal(t, "tool_calls", *lastFrame.Choices[0].FinishReason,
		"agent loops branch on this field")
	require.Empty(t, lastFrame.Choices[0].Delta.ToolCalls)
}

func TestChatCompletions_ReplaysToolConversationUpstream(t *testing.T) {
	var upstreamBody map[string]any
	ts := newToolCallRig(t, "tool_calls", func(body map[string]any) { upstreamBody = body })

	body := `{"model":"agent","messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",` +
		`"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"},` +
		`"extra_content":{"google":{"thought_signature":"sig-abc"}}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"temp_c\":18}"}],` + toolsField + `}`

	resp := postChat(t, ts, body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	messages, ok := upstreamBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 3)

	assistant, ok := messages[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "assistant", assistant["role"])
	calls, ok := assistant["tool_calls"].([]any)
	require.True(t, ok)
	require.Len(t, calls, 1)

	// Gemini expects its thought signature echoed back; a replayed turn that
	// loses it breaks the next hop of the caller's tool loop.
	call, ok := calls[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t,
		map[string]any{"google": map[string]any{"thought_signature": "sig-abc"}},
		call["extra_content"],
	)

	toolMsg, ok := messages[2].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool", toolMsg["role"])
	require.Equal(t, "call_1", toolMsg["tool_call_id"])
}

func TestChatCompletions_ReportsToolCallsFinishReasonWhenProviderSaysStop(t *testing.T) {
	ts := newToolCallRig(t, "stop", nil)

	resp := postChat(t, ts, `{"model":"agent","messages":[{"role":"user","content":"weather?"}],`+toolsField+`}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out llm.ChatCompletion
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Choices[0].Message.ToolCalls, 1)
	require.Equal(t, "tool_calls", out.Choices[0].FinishReason)
}
