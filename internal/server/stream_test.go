package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rocry/smolllm-server/internal/llm"
	"github.com/stretchr/testify/require"
)

// readStream decodes every frame of a streamed completion and requires the
// stream to end with [DONE] and nothing after it.
func readStream(t *testing.T, resp *http.Response) []llm.ChatCompletionChunk {
	t.Helper()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var chunks []llm.ChatCompletionChunk
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload, found := strings.CutPrefix(scanner.Text(), "data: ")
		if !found {
			continue
		}
		if payload == "[DONE]" {
			rest, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Empty(t, strings.TrimSpace(string(rest)), "nothing follows [DONE]")
			return chunks
		}
		var chunk llm.ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk), payload)
		chunks = append(chunks, chunk)
	}
	require.NoError(t, scanner.Err())
	t.Fatal("stream ended without [DONE]")
	return nil
}

// streamedText joins the answer text every frame carried.
func streamedText(chunks []llm.ChatCompletionChunk) string {
	var text strings.Builder
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
	return text.String()
}

// failsAfter streams the given deltas, then reports an in-band error, which on
// its own makes the chain fail over.
func failsAfter(deltas ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range deltas {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}],"+
			"\"error\":{\"message\":\"upstream died\"}}\n\ndata: [DONE]\n\n")
	}
}

// requireCommittedFailure checks a stream whose first leg failed after emitting
// answer output: the stream ends in the error frame, nothing from the fallback
// leg ever reaches the client, and the request log reports the failure.
func requireCommittedFailure(
	t *testing.T, chunks []llm.ChatCompletionChunk, logs *syncBuffer, upstreamCalls int,
) llm.ChatCompletionChunk {
	t.Helper()
	require.Equal(t, 1, upstreamCalls, "the fallback leg is never started")
	terminal := chunks[len(chunks)-1]
	require.NotNil(t, terminal.Error)
	require.Equal(t, "api_error", terminal.Error.Type)
	require.Contains(t, terminal.Error.Message, "output already emitted")
	require.Len(t, terminal.Choices, 1)
	require.NotNil(t, terminal.Choices[0].FinishReason)
	require.Equal(t, "error", *terminal.Choices[0].FinishReason)
	for _, chunk := range chunks {
		require.Equal(t, "smolayer/alpha", chunk.Model, "every frame names the committed leg")
	}
	require.NotContains(t, streamedText(chunks), "beta answer")

	line := logs.requestLine(t)
	require.Contains(t, line, "status=502")
	require.Contains(t, line, "model=smolayer/alpha")
	require.Contains(t, logs.String(), "stream leg failed after partial output")
	return terminal
}

// Bytes already on the wire cannot be recalled, so a leg that fails after
// streaming answer text ends the stream rather than splicing in a second answer.
func TestChatCompletions_StreamEndsWhenLegFailsAfterText(t *testing.T) {
	ts, logs, bodies := newCacheRig(t, failsAfter(`{"content":"partial"}`))

	resp := postChat(t, ts, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	chunks := readStream(t, resp)

	require.Equal(t, "partial", streamedText(chunks))
	terminal := requireCommittedFailure(t, chunks, logs, len(bodies()))
	require.Contains(t, terminal.Error.Message, "upstream died")
}

// A tool-call fragment commits the stream the same way answer text does.
func TestChatCompletions_StreamEndsWhenLegFailsAfterToolCallFragment(t *testing.T) {
	ts, logs, bodies := newCacheRig(t, failsAfter(
		`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\""}}]}`,
	))

	resp := postChat(t, ts, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	chunks := readStream(t, resp)

	var fragments []llm.ToolCallDelta
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			fragments = append(fragments, choice.Delta.ToolCalls...)
		}
	}
	require.NotEmpty(t, fragments)
	require.Equal(t, "lookup", fragments[0].Function.Name)
	requireCommittedFailure(t, chunks, logs, len(bodies()))
}

// Reasoning is not answer output: a leg that only thought before failing still
// falls back, and its reasoning precedes the winner's answer.
func TestChatCompletions_StreamFallsBackWhenLegFailsAfterReasoningOnly(t *testing.T) {
	ts, logs, bodies := newCacheRig(t, failsAfter(`{"reasoning_content":"thinking"}`))

	resp := postChat(t, ts, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	chunks := readStream(t, resp)

	require.Len(t, bodies(), 2)
	require.Equal(t, "beta answer", streamedText(chunks))
	var reasoning strings.Builder
	for _, chunk := range chunks {
		require.Nil(t, chunk.Error)
		reasoning.WriteString(chunk.Choices[0].Delta.ReasoningContent)
	}
	require.Equal(t, "thinking", reasoning.String())

	terminal := chunks[len(chunks)-1]
	require.Equal(t, "stop", *terminal.Choices[0].FinishReason)
	require.Equal(t, "mock/beta", terminal.Model, "the finish frame names the leg that answered")

	line := logs.requestLine(t)
	require.Contains(t, line, "status=200")
	require.Contains(t, line, "model=mock/beta")
	require.Contains(t, logs.String(), "stream leg failed before output")
}
