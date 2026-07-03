package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/llm"
)

func (h *handlers) chat(w http.ResponseWriter, r *http.Request) {
	var req llm.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	prompt, opts, err := llm.BuildOptions(&req, h.cfg().ResolveModel)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	opts = append(opts, smolllm.WithLogger(h.logger))
	opts = h.appendUsageHook(opts, req.Model, req.Stream)

	if req.Stream {
		h.chatStream(w, r, prompt, opts)
		return
	}
	h.chatBlocking(w, r, prompt, opts, req.Model)
}

func (h *handlers) chatBlocking(w http.ResponseWriter, r *http.Request, prompt smolllm.Prompt, opts []smolllm.Option, requestedModel string) {
	resp, err := smolllm.Ask(r.Context(), prompt, opts...)
	if err != nil {
		upstreamError(w, err)
		return
	}

	out := llm.ChatCompletion{
		ID:      llm.NewID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resolvedModel(resp.Model, requestedModel),
		Choices: []llm.ChatChoice{{
			Index: 0,
			Message: llm.ChatMessage{
				Role:             "assistant",
				Content:          resp.Text,
				ReasoningContent: resp.Reasoning,
			},
			FinishReason: "stop",
		}},
		Usage: llm.CompletionUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("encode response failed", "error", err)
	}
}

func (h *handlers) chatStream(w http.ResponseWriter, r *http.Request, prompt smolllm.Prompt, opts []smolllm.Option) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		upstreamError(w, errors.New("streaming not supported by this server"))
		return
	}

	stream, err := smolllm.Stream(r.Context(), prompt, opts...)
	if err != nil {
		upstreamError(w, err)
		return
	}
	defer stream.Stream.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := llm.NewID()
	created := time.Now().Unix()
	model := stream.Model

	writeChunk(w, flusher, llm.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{Role: "assistant"}}},
	})

	ctx := r.Context()
streamLoop:
	for {
		select {
		case <-ctx.Done():
			stream.Stream.Close()
			_ = stream.Stream.Wait()
			return
		case chunk, ok := <-stream.Stream.Chan():
			if !ok {
				break streamLoop
			}
			if chunk.Content == "" && chunk.Reasoning == "" {
				continue
			}
			writeChunk(w, flusher, llm.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []llm.ChatChoiceDelta{{
					Index: 0,
					Delta: llm.ChatDelta{
						Content:          chunk.Content,
						ReasoningContent: chunk.Reasoning,
					},
				}},
			})
		}
	}

	if err := stream.Stream.Wait(); err != nil {
		reason := "error"
		writeChunk(w, flusher, llm.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{}, FinishReason: &reason}},
			Error:   &llm.ChatStreamError{Message: err.Error(), Type: "api_error"},
		})
		writeRaw(w, flusher, "[DONE]")
		return
	}

	stop := "stop"
	writeChunk(w, flusher, llm.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []llm.ChatChoiceDelta{{Index: 0, Delta: llm.ChatDelta{}, FinishReason: &stop}},
	})
	writeRaw(w, flusher, "[DONE]")
}

func writeChunk(w http.ResponseWriter, f http.Flusher, c llm.ChatCompletionChunk) {
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	writeRaw(w, f, string(b))
}

func writeRaw(w http.ResponseWriter, f http.Flusher, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	f.Flush()
}

func resolvedModel(actual, requested string) string {
	if actual != "" {
		return actual
	}
	return requested
}
