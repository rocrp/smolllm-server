package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rocry/smolllm-server/internal/meter"
)

func (h *handlers) stats(w http.ResponseWriter, r *http.Request) {
	days := 7
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			badRequest(w, fmt.Sprintf("days must be a positive integer (got %q)", raw))
			return
		}
		days = parsed
	}

	path, err := h.cfg().UsagePath()
	if err != nil {
		upstreamError(w, err)
		return
	}
	out, err := meter.ReadStats(path, days, time.Now())
	if err != nil {
		upstreamError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("encode stats response failed", "error", err)
	}
}
