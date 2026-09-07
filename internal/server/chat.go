package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/apierr"
	"github.com/rocry/smolllm-server/internal/llm"
)

func (h *handlers) chat(w http.ResponseWriter, r *http.Request) {
	var req llm.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	turn, opts, err := llm.BuildOptions(&req, h.cfg().ResolveModel)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	opts = append(opts, smolllm.WithHook(h.ledger.Hook(req.Model)))

	if req.Stream {
		h.chatStream(w, r, turn, opts, req.Model)
		return
	}
	h.chatBlocking(w, r, turn, opts, req.Model)
}

func (h *handlers) chatBlocking(
	w http.ResponseWriter, r *http.Request, turn smolllm.Request, opts []smolllm.Option, requestedModel string,
) {
	// Ask never reports an operational failure as a Go error: the turn it hands
	// back says how the call ended.
	msg := h.client.Ask(r.Context(), turn, opts...)
	if msg.StopReason == smolllm.StopReasonError || msg.StopReason == smolllm.StopReasonAborted {
		writeFailure(w, msg)
		return
	}

	// OpenAI reports content as null on an assistant turn that only requested
	// tool calls; clients key on tool_calls, not on the empty string.
	var content *string
	if msg.Content != "" || len(msg.ToolCalls) == 0 {
		text := msg.Content
		content = &text
	}

	out := llm.ChatCompletion{
		ID:      llm.NewID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resolvedModel(msg.Model, requestedModel),
		Choices: []llm.ChatChoice{{
			Index: 0,
			Message: llm.ChatMessage{
				Role:             "assistant",
				Content:          content,
				ReasoningContent: msg.Reasoning,
				ToolCalls:        msg.ToolCalls,
			},
			FinishReason: finishReasonForTurn(msg.FinishReason, len(msg.ToolCalls)),
		}},
		Usage: llm.UsageFrom(msg.Usage),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("encode response failed", "error", err)
	}
}

func (h *handlers) chatStream(
	w http.ResponseWriter, r *http.Request, turn smolllm.Request, opts []smolllm.Option, requestedModel string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		apierr.Write(w, http.StatusInternalServerError, "internal_error", "server_error",
			"streaming not supported by this server")
		return
	}

	stream := h.client.Stream(r.Context(), turn, opts...)
	// The request context already aborts the call when the client hangs up; Close
	// releases the pump for every other way out of this handler.
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	frames := &chunkWriter{
		w:       w,
		flusher: flusher,
		id:      llm.NewID(),
		created: time.Now().Unix(),
		// Replaced by the served model as soon as a leg wins; only a stream that
		// fails before any leg answers ever names the alias the client asked for.
		model: requestedModel,
	}

	var slots toolCallSlots
	for event := range stream.Events() {
		if event.Message != nil && event.Message.Model != "" {
			frames.model = event.Message.Model
		}
		switch event.Kind {
		case smolllm.EventTextDelta:
			frames.write(llm.ChatDelta{Content: event.Delta}, nil)
		case smolllm.EventReasoningDelta:
			frames.write(llm.ChatDelta{ReasoningContent: event.Delta}, nil)
		case smolllm.EventToolCallStart:
			if call, found := slots.open(event.Index, event.Message); found {
				frames.write(llm.ChatDelta{ToolCalls: []llm.ToolCallDelta{
					llm.OpenToolCall(event.Index, call),
				}}, nil)
			}
		case smolllm.EventToolCallDelta:
			frames.write(llm.ChatDelta{ToolCalls: []llm.ToolCallDelta{
				llm.AppendToolCallArguments(event.Index, event.Delta),
			}}, nil)
		case smolllm.EventLegFailed:
			// The chain discards the failed leg's text and starts the next one
			// from an empty turn, but whatever already reached the client cannot
			// be recalled. Worth a log line when a stream reads oddly.
			h.logger.Warn("stream leg failed after partial output",
				"model", event.Attempt.Model, "error", event.Attempt.Err)
			slots = nil
		case smolllm.EventDone:
			reason := finishReasonForTurn(event.Message.FinishReason, len(event.Message.ToolCalls))
			frames.write(llm.ChatDelta{}, &reason)
			frames.done()
			return
		case smolllm.EventError:
			h.streamFailure(frames, event.Message)
			return
		case smolllm.EventStart, smolllm.EventToolCallEnd:
			// Start carries no payload, and the completed call was already sent
			// fragment by fragment.
		}
	}

	// The channel closed without a terminal event, which the library does not do.
	// Fail loudly rather than leaving the client waiting on a stream that ended.
	h.streamFailure(frames, stream.Result())
}

// streamFailure reports a failed turn inside the stream. The status line is long
// gone by now, so the failure travels as a terminal frame instead.
func (h *handlers) streamFailure(frames *chunkWriter, msg *smolllm.AssistantMessage) {
	failure := llm.FailureFor(msg)
	h.logger.Warn("stream failed", "status", failure.Status, "error", failure.Message)
	reason := "error"
	frames.write(llm.ChatDelta{}, &reason, &llm.ChatStreamError{
		Message: failure.Message,
		Type:    failure.Kind,
	})
	frames.done()
}

// chunkWriter emits the SSE frames of one streamed completion, holding the
// identity every frame repeats.
//
// The opening `role: assistant` frame is not written until the first real frame
// is due. Clients read the served model off the first frame they receive, and
// the leg that serves the turn is only known once it produces something; an
// eager prelude would name the alias instead and be believed.
type chunkWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	id      string
	created int64
	model   string
	opened  bool
}

func (c *chunkWriter) write(delta llm.ChatDelta, finishReason *string, streamErr ...*llm.ChatStreamError) {
	if !c.opened {
		c.opened = true
		c.write(llm.ChatDelta{Role: "assistant"}, nil)
	}
	chunk := llm.ChatCompletionChunk{
		ID:      c.id,
		Object:  "chat.completion.chunk",
		Created: c.created,
		Model:   c.model,
		Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: delta, FinishReason: finishReason}},
		Error:   nil,
	}
	if len(streamErr) > 0 {
		chunk.Error = streamErr[0]
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	c.raw(string(encoded))
}

func (c *chunkWriter) done() { c.raw("[DONE]") }

func (c *chunkWriter) raw(payload string) {
	_, _ = fmt.Fprintf(c.w, "data: %s\n\n", payload)
	c.flusher.Flush()
}

// toolCallSlots tracks the provider slot indexes opened so far, in order.
//
// A tool-call event names its slot by the provider's index, but the message
// snapshot it carries lists the calls sorted by that index and nothing else. The
// sorted position of a newly opened index among the open ones is therefore
// exactly its position in the snapshot.
type toolCallSlots []int

func (s *toolCallSlots) open(index int, msg *smolllm.AssistantMessage) (smolllm.ToolCall, bool) {
	position, _ := slices.BinarySearch(*s, index)
	*s = slices.Insert(*s, position, index)
	if msg == nil || position >= len(msg.ToolCalls) {
		return smolllm.ToolCall{}, false
	}
	return msg.ToolCalls[position], true
}

func resolvedModel(actual, requested string) string {
	if actual != "" {
		return actual
	}
	return requested
}

// finishReasonForTurn reports the OpenAI-shaped reason for a completed turn.
//
// The provider's string passes through untouched except in two cases this
// endpoint's clients depend on: an absent reason becomes "stop", and a turn that
// carries tool calls reports "tool_calls" whatever the provider said. Gemini
// says "stop" while returning tool calls, and an agent loop branching on this
// field would treat that turn as a final answer and never run the tool. The
// libraries still surface the provider's reason verbatim; only this
// OpenAI-compatible surface normalizes it.
func finishReasonForTurn(reason string, toolCalls int) string {
	if toolCalls > 0 {
		return "tool_calls"
	}
	if reason == "" {
		return "stop"
	}
	return reason
}
