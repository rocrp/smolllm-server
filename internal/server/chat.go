package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

	note := noteFor(r)
	note.alias = req.Model

	turn, opts, err := llm.BuildOptions(&req, h.cfg().ResolveModel)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	opts = append(opts, smolllm.WithHook(h.ledger.Hook(req.Model)))

	if req.Stream {
		h.chatStream(w, r, turn, opts, req.Model, req.IncludeUsage())
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
	noteFor(r).model = servedModel(msg)
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
		ID:          llm.NewID(),
		Object:      "chat.completion",
		Created:     time.Now().Unix(),
		Model:       resolvedModel(msg.Model, requestedModel),
		ServiceTier: codexServiceTier(msg.Model),
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

// chatStream relays one streamed call. smolllm-go commits a Stream to the first
// leg that emits answer text or a tool-call fragment, so every answer delta
// sent here comes from one leg: a committed leg that fails ends the stream in
// the error frame instead of splicing in the next leg's answer.
func (h *handlers) chatStream(
	w http.ResponseWriter, r *http.Request, turn smolllm.Request, opts []smolllm.Option, requestedModel string,
	includeUsage bool,
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
		// Follows the leg behind each event, so the finish frame names the leg
		// that answered; only a stream that fails before any leg is tried names
		// the alias the client asked for.
		model: requestedModel,
	}

	note := noteFor(r)
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
			h.logLegFailure(event.Attempt)
		case smolllm.EventDone:
			note.model = servedModel(event.Message)
			reason := finishReasonForTurn(event.Message.FinishReason, len(event.Message.ToolCalls))
			frames.write(llm.ChatDelta{}, &reason)
			if includeUsage {
				frames.usage(llm.UsageFrom(event.Message.Usage))
			}
			frames.done()
			return
		case smolllm.EventError:
			h.streamFailure(note, frames, event.Message)
			return
		case smolllm.EventStart, smolllm.EventToolCallEnd:
			// Start carries no payload, and the completed call was already sent
			// fragment by fragment.
		}
	}

	// The channel closed without a terminal event, which the library does not do.
	// Fail loudly rather than leaving the client waiting on a stream that ended.
	h.streamFailure(note, frames, stream.Result())
}

// logLegFailure tells apart the two ways a leg fails mid-stream. Before answer
// output the chain retries or advances (unless the request itself was rejected)
// and the client sees only the next try. After it the leg is committed: the
// stream ends in the error frame, and the client must retry.
func (h *handlers) logLegFailure(attempt *smolllm.Attempt) {
	if attempt.Emitted {
		h.logger.Warn("stream leg failed after partial output; ending the stream with an error",
			"model", attempt.Model, "error", attempt.Err)
		return
	}
	h.logger.Warn("stream leg failed before output",
		"model", attempt.Model, "disposition", attempt.Err.Disposition.String(), "error", attempt.Err)
}

// streamFailure reports a failed turn inside the stream. The status line is long
// gone by now, so the failure travels as a terminal frame instead.
func (h *handlers) streamFailure(note *requestNote, frames *chunkWriter, msg *smolllm.AssistantMessage) {
	failure := llm.FailureFor(msg)
	note.model = servedModel(msg)
	note.status = failure.Status
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
	chunk := c.chunk([]llm.ChatChoiceDelta{{Index: 0, Delta: delta, FinishReason: finishReason}})
	if len(streamErr) > 0 {
		chunk.Error = streamErr[0]
	}
	c.send(chunk)
}

// usage writes the frame stream_options.include_usage asks for: empty choices
// and the usage, after the finish frame and before [DONE].
func (c *chunkWriter) usage(usage llm.CompletionUsage) {
	chunk := c.chunk([]llm.ChatChoiceDelta{})
	chunk.Usage = &usage
	c.send(chunk)
}

func (c *chunkWriter) chunk(choices []llm.ChatChoiceDelta) llm.ChatCompletionChunk {
	return llm.ChatCompletionChunk{
		ID:          c.id,
		Object:      "chat.completion.chunk",
		Created:     c.created,
		Model:       c.model,
		ServiceTier: codexServiceTier(c.model),
		Choices:     choices,
		Usage:       nil,
		Error:       nil,
	}
}

func (c *chunkWriter) send(chunk llm.ChatCompletionChunk) {
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

// toolCallSlots tracks the provider slot indexes opened so far, in order. They
// never need resetting: opening a slot commits the stream to its leg, so no
// retry or later leg ever follows one.
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

// servedModel names the leg that answered, or for a failed turn the last leg
// tried, which smolllm-go v0.4 puts on the message itself; "-" when that leg
// never resolved to a model (or no leg ran at all).
func servedModel(msg *smolllm.AssistantMessage) string {
	if msg == nil || msg.Model == "" {
		return "-"
	}
	return msg.Model
}

func resolvedModel(actual, requested string) string {
	if actual != "" {
		return actual
	}
	return requested
}

// The Gateway echoes the requested Service Tier. smolllm-go retains the
// winning leg's Model Spec but not that response field, so recover it from the
// winning Codex spec. Never label a fallback to another Provider as Codex Fast.
func codexServiceTier(model string) string {
	name, codex := strings.CutPrefix(model, "smolayer/codex/")
	if !codex || name == "" {
		return ""
	}
	_, tier, explicit := strings.Cut(name, "@")
	if !explicit {
		return "default"
	}
	if tier == "fast" || tier == "default" {
		return tier
	}
	return ""
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
