package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/rocry/smolllm-server/internal/apierr"
	"github.com/rocry/smolllm-server/internal/config"
)

type handlers struct {
	cfg    *config.Config
	logger *slog.Logger
}

func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// badRequest writes an OpenAI-style 400.
func badRequest(w http.ResponseWriter, message string) {
	apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", message)
}

// upstreamError writes an OpenAI-style 502 — provider call failed.
func upstreamError(w http.ResponseWriter, err error) {
	apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
}
