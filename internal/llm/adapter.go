package llm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rocry/smolllm-go/smolllm"
	"github.com/rocry/smolllm-server/internal/modelspec"
)

// BuildOptions converts an incoming ChatRequest into a smolllm Request plus the
// per-call options for it. `aliasResolve` maps an alias name to a comma-separated
// chain, or returns its input unchanged when no alias applies (typically
// *config.Config.ResolveModel).
//
// Routing and sampling live in the options; the conversation, and only the
// conversation, lives in the Request.
func BuildOptions(req *ChatRequest, aliasResolve func(string) string) (smolllm.Request, []smolllm.Option, error) {
	var empty smolllm.Request
	if req == nil {
		return empty, nil, errors.New("request must not be nil")
	}
	if strings.TrimSpace(req.Model) == "" {
		return empty, nil, errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return empty, nil, errors.New("messages must not be empty")
	}

	if !isJSONNullOrEmpty(req.Functions) {
		return empty, nil, errors.New(
			"functions are not supported by smolllm-server (the legacy API is superseded by tools)")
	}
	if req.N != nil && *req.N != 1 {
		return empty, nil, fmt.Errorf("n=%d is not supported (only n=1)", *req.N)
	}

	model := req.Model
	if aliasResolve != nil {
		model = aliasResolve(model)
	}
	routing, err := ModelOptions(model)
	if err != nil {
		return empty, nil, err
	}

	turn := smolllm.RequestFromMessages(req.Messages)
	if err := restoreToolCallExtras(turn.Messages, req.rawMessages); err != nil {
		return empty, nil, err
	}
	if err := turn.Validate(); err != nil {
		return empty, nil, err
	}

	opts := routing
	if req.Temperature != nil {
		opts = append(opts, smolllm.WithTemperature(*req.Temperature))
	}
	if req.TopP != nil {
		opts = append(opts, smolllm.WithTopP(*req.TopP))
	}
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			return empty, nil, fmt.Errorf("max_tokens must be positive (got %d)", *req.MaxTokens)
		}
		opts = append(opts, smolllm.WithMaxTokens(*req.MaxTokens))
	}
	if !isJSONNullOrEmpty(req.Stop) {
		stops, err := decodeStop(req.Stop)
		if err != nil {
			return empty, nil, err
		}
		opts = append(opts, smolllm.WithStop(stops...))
	}
	if req.Seed != nil {
		opts = append(opts, smolllm.WithSeed(*req.Seed))
	}
	// The request field sets the effort for the chain as a whole. A leg that named
	// its own in the config keeps it: smolllm-go reads the per-leg override first
	// and falls back to this one, so the two never depend on option order.
	if req.ReasoningEffort != nil && strings.TrimSpace(*req.ReasoningEffort) != "" {
		opts = append(opts, smolllm.WithReasoningEffort(*req.ReasoningEffort))
	}
	// Pass-through fields reach the provider verbatim; the server models none of
	// them, so a provider error about one surfaces unchanged. `tools` stays here
	// rather than moving to the typed Request.Tools, which models only name,
	// description and parameters and would drop every other key a caller sent.
	if extra := passThroughFields(req); len(extra) > 0 {
		opts = append(opts, smolllm.WithExtraBody(extra))
	}
	if req.Timeout != nil {
		if *req.Timeout < 0 {
			return empty, nil, fmt.Errorf("timeout must be >= 0 (got %g)", *req.Timeout)
		}
		// Since v0.3 this bounds the whole call - every fallback leg, every retry
		// and the consumption of the stream - not one upstream request.
		opts = append(opts, smolllm.WithTimeout(time.Duration(*req.Timeout*float64(time.Second))))
	}
	return turn, opts, nil
}

// ModelOptions turns a resolved model chain into the routing options for it:
// WithModel over the bare specs, plus one per-leg reasoning-effort override for
// each leg whose config entry carried an `!effort` suffix.
//
// The suffix is smolllm-server config syntax. smolllm-go dropped it in v0.3, so
// it never reaches the library as part of a model string.
func ModelOptions(model string) ([]smolllm.Option, error) {
	chain, err := modelspec.Parse(model)
	if err != nil {
		return nil, err
	}
	opts := make([]smolllm.Option, 0, len(chain.Legs)+1)
	opts = append(opts, smolllm.WithModel(chain.Model()))
	for _, leg := range chain.Legs {
		if leg.Effort != "" {
			opts = append(opts, smolllm.WithLegReasoningEffort(leg.Spec, leg.Effort))
		}
	}
	return opts, nil
}

// restoreToolCallExtras puts back the tool-call keys the openai param union drops
// while decoding. A caller replaying an assistant turn must reach the provider
// with it intact — Gemini expects its thought signature echoed back.
func restoreToolCallExtras(messages []smolllm.Message, raw []json.RawMessage) error {
	if len(raw) != len(messages) {
		return nil // nothing to align against; leave the decoded messages alone
	}
	for i, rawMessage := range raw {
		assistant := messages[i].OfAssistant
		if assistant == nil || len(assistant.ToolCalls) == 0 {
			continue
		}
		var envelope struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(rawMessage, &envelope); err != nil {
			return fmt.Errorf("decode message #%d tool calls: %w", i, err)
		}
		if len(envelope.ToolCalls) != len(assistant.ToolCalls) {
			continue
		}
		for j, rawCall := range envelope.ToolCalls {
			extra := map[string]any{}
			for key, value := range rawCall {
				switch key {
				case "id", "type", "function", "index":
				default:
					extra[key] = value
				}
			}
			if len(extra) == 0 {
				continue
			}
			if fn := assistant.ToolCalls[j].OfFunction; fn != nil {
				fn.SetExtraFields(extra)
			}
		}
	}
	return nil
}

// passThroughFields collects the request fields the server forwards verbatim.
func passThroughFields(req *ChatRequest) map[string]any {
	fields := map[string]any{}
	for name, raw := range map[string]json.RawMessage{
		"tools":               req.Tools,
		"tool_choice":         req.ToolChoice,
		"parallel_tool_calls": req.ParallelToolCalls,
		"response_format":     req.ResponseFormat,
	} {
		if !isJSONNullOrEmpty(raw) {
			fields[name] = raw
		}
	}
	return fields
}

func isJSONNullOrEmpty(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return true
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	switch decoded := value.(type) {
	case nil:
		return true
	case []any:
		return len(decoded) == 0
	case map[string]any:
		return len(decoded) == 0
	default:
		return false
	}
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	if isJSONNullOrEmpty(raw) {
		return nil, nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil, errors.New("stop must not be empty")
		}
		return []string{single}, nil
	}

	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, errors.New("stop must be a string or array of strings")
	}
	if len(multiple) == 0 {
		return nil, errors.New("stop must contain at least one entry")
	}
	for _, stop := range multiple {
		if strings.TrimSpace(stop) == "" {
			return nil, errors.New("stop entries must not be empty")
		}
	}
	return multiple, nil
}

// NewID returns an OpenAI-style chat completion ID, e.g. "chatcmpl-1f2a...".
func NewID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}
