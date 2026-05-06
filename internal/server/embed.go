package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/llm"
)

func (h *handlers) embeddings(w http.ResponseWriter, r *http.Request) {
	var req llm.EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		badRequest(w, "model is required")
		return
	}
	inputs, err := decodeEmbeddingInput(req.Input)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	model := h.cfg.ResolveModel(req.Model)
	opts := []smolllm.Option{
		smolllm.WithModel(model),
		smolllm.WithLogger(h.logger),
	}
	if req.Dimensions != nil && *req.Dimensions > 0 {
		opts = append(opts, smolllm.WithDimensions(*req.Dimensions))
	}

	resp, err := smolllm.Embed(r.Context(), inputs, opts...)
	if err != nil {
		upstreamError(w, err)
		return
	}

	out := llm.EmbeddingResponse{
		Object: "list",
		Data:   make([]llm.EmbeddingItem, len(resp.Embeddings)),
		Model:  resolvedModel(resp.Model, req.Model),
		Usage: llm.EmbeddingUsage{
			PromptTokens: resp.Usage.InputTokens,
			TotalTokens:  resp.Usage.InputTokens,
		},
	}
	for i, vec := range resp.Embeddings {
		out.Data[i] = llm.EmbeddingItem{Object: "embedding", Index: i, Embedding: vec}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("encode embeddings response failed", "error", err)
	}
}

// decodeEmbeddingInput accepts either a JSON string or an array of strings,
// matching the OpenAI /v1/embeddings spec.
func decodeEmbeddingInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	// Try string first.
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, fmt.Errorf("input must not be empty")
		}
		return []string{single}, nil
	}
	// Then array of strings.
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("input must be a string or array of strings")
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("input array must not be empty")
	}
	for i, s := range arr {
		if s == "" {
			return nil, fmt.Errorf("input[%d] must not be empty", i)
		}
	}
	return arr, nil
}
