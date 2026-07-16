package server

import (
	"encoding/json"
	"net/http"

	"github.com/rocry/smolllm-server/internal/ledger"
)

type statsResponse struct {
	Buckets []ledger.Bucket `json:"buckets"`
}

func (h *handlers) stats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(statsResponse{Buckets: h.ledger.Snapshot()}); err != nil {
		h.logger.Warn("encode stats response failed", "error", err)
	}
}
