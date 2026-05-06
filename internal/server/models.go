package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/rocry/smolllm-server/internal/llm"
)

func (h *handlers) models(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	cfg := h.cfg()
	out := llm.ModelsResponse{
		Object: "list",
		Data:   make([]llm.ModelInfo, 0, len(cfg.Aliases)),
	}
	names := make([]string, 0, len(cfg.Aliases))
	for name := range cfg.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out.Data = append(out.Data, llm.ModelInfo{
			ID:      name,
			Object:  "model",
			OwnedBy: "smolllm-alias",
			Created: now,
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("encode models response failed", "error", err)
	}
}
