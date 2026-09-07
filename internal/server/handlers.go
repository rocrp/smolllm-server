package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/apierr"
	"github.com/rocry/smolllm-server/internal/config"
	"github.com/rocry/smolllm-server/internal/ledger"
	"github.com/rocry/smolllm-server/internal/llm"
)

type handlers struct {
	store  *config.Store
	logger *slog.Logger
	ledger *ledger.Ledger
	// client is built once and shared: it owns the key/endpoint balancer, so a
	// per-request client would restart key rotation on every call.
	client *smolllm.Client
}

// cfg returns the current config snapshot. Always call this — never cache
// the result across request boundaries — so SIGHUP reloads take effect.
func (h *handlers) cfg() *config.Config { return h.store.Get() }

func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// badRequest writes an OpenAI-style 400.
func badRequest(w http.ResponseWriter, message string) {
	apierr.Write(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", message)
}

// writeFailure reports a turn that produced no answer. smolllm-go classifies
// each failed leg, so the status says whether the caller or the provider has to
// change something.
func writeFailure(w http.ResponseWriter, msg *smolllm.AssistantMessage) {
	failure := llm.FailureFor(msg)
	apierr.Write(w, failure.Status, failure.Code, failure.Kind, failure.Message)
}

// upstreamError writes an OpenAI-style 502 for a call that still reports failure
// as a Go error, which since v0.3 means embeddings only.
func upstreamError(w http.ResponseWriter, err error) {
	apierr.Write(w, http.StatusBadGateway, "upstream_error", "api_error", err.Error())
}
