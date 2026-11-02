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

// Opaque tokens in smolayer's shape (`rsn_` + base64): the server must never
// look inside one, so a `+`, `/` or `=` has to survive untouched.
const (
	reasoningItemA = "rsn_eyJpZCI6InJzXzEifQ+/AAA="
	reasoningItemB = "rsn_eyJpZCI6InJzXzIifQ+/BBB="
)

// newReasoningItemsRig fakes smolayer answering one Codex turn: each Reasoning
// Item arrives as its own `delta.reasoning_items` frame ahead of the answer, as
// src/openai/from_turn.rs streams them. upstreamMessages receives the raw
// `messages` array of every request the fake gets.
func newReasoningItemsRig(t *testing.T, upstreamMessages *json.RawMessage) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		*upstreamMessages = body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_items":["` + reasoningItemA + `"]}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_items":["` + reasoningItemB + `"]}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("MOCK_BASE_URL", upstream.URL)
	t.Setenv("MOCK_API_KEY", "secret-mock-key")

	cfg := &config.Config{
		Server:  config.ServerConfig{Bind: "127.0.0.1:0", AccessKey: "rocry", LogLevel: "warn"},
		Aliases: map[string]string{"haas-agent": "mock/codex"},
	}
	srv := New(config.NewStore("", cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.HTTP.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestChatCompletions_BlockingReturnsReasoningItems(t *testing.T) {
	var upstream json.RawMessage
	ts := newReasoningItemsRig(t, &upstream)

	resp := postChat(t, ts, `{"model":"haas-agent","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out llm.ChatCompletion
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Choices, 1)
	require.Equal(t, []string{reasoningItemA, reasoningItemB}, out.Choices[0].Message.ReasoningItems)
}

func TestChatCompletions_StreamingReturnsReasoningItems(t *testing.T) {
	var upstream json.RawMessage
	ts := newReasoningItemsRig(t, &upstream)

	resp := postChat(t, ts, `{"model":"haas-agent","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// A client merges reasoning_items deltas by pushing, as smolayer documents.
	var merged []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					ReasoningItems []string `json:"reasoning_items"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &frame))
		for _, choice := range frame.Choices {
			merged = append(merged, choice.Delta.ReasoningItems...)
		}
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, []string{reasoningItemA, reasoningItemB}, merged)
}

// The next step of a tool loop echoes the items back on the assistant message;
// smolayer needs them verbatim or the model reasons from scratch.
func TestChatCompletions_ForwardsReasoningItemsOnReplayedAssistantMessage(t *testing.T) {
	var upstream json.RawMessage
	ts := newReasoningItemsRig(t, &upstream)

	resp := postChat(t, ts, `{"model":"haas-agent","messages":[`+
		`{"role":"user","content":"hi"},`+
		`{"role":"assistant","content":"ok","reasoning_items":["`+reasoningItemA+`","`+reasoningItemB+`"]},`+
		`{"role":"user","content":"again"}]}`)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.JSONEq(t, `[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"ok","reasoning_items":["`+reasoningItemA+`","`+reasoningItemB+`"]},
		{"role":"user","content":"again"}
	]`, string(upstream))
}
