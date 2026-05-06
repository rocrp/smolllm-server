package llm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/rocry/smolllm-go/smolllm"
)

// BuildOptions converts an incoming ChatRequest into smolllm options.
// `aliasResolve` should map an alias name to a comma-separated chain or return
// the input unchanged if no alias applies (typically *config.Config.ResolveModel).
func BuildOptions(req *ChatRequest, aliasResolve func(string) string) (smolllm.Prompt, []smolllm.Option, error) {
	if req == nil {
		return smolllm.Prompt{}, nil, errors.New("request must not be nil")
	}
	if strings.TrimSpace(req.Model) == "" {
		return smolllm.Prompt{}, nil, errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return smolllm.Prompt{}, nil, errors.New("messages must not be empty")
	}

	if len(req.Tools) > 0 {
		return smolllm.Prompt{}, nil, errors.New("tools are not yet supported by smolllm-server")
	}
	if len(req.Functions) > 0 {
		return smolllm.Prompt{}, nil, errors.New("functions are not yet supported by smolllm-server")
	}
	if len(req.ResponseFormat) > 0 {
		return smolllm.Prompt{}, nil, errors.New("response_format is not yet supported by smolllm-server")
	}
	if req.N != nil && *req.N != 1 {
		return smolllm.Prompt{}, nil, fmt.Errorf("n=%d is not supported (only n=1)", *req.N)
	}

	model := req.Model
	if aliasResolve != nil {
		model = aliasResolve(model)
	}

	prompt := smolllm.PromptFromMessages(req.Messages)
	if err := prompt.Validate(); err != nil {
		return smolllm.Prompt{}, nil, err
	}

	opts := []smolllm.Option{smolllm.WithModel(model)}
	if req.Temperature != nil {
		opts = append(opts, smolllm.WithTemperature(*req.Temperature))
	}
	if req.TopP != nil {
		opts = append(opts, smolllm.WithTopP(*req.TopP))
	}
	if req.ReasoningEffort != nil && strings.TrimSpace(*req.ReasoningEffort) != "" {
		opts = append(opts, smolllm.WithReasoningEffort(*req.ReasoningEffort))
	}
	return prompt, opts, nil
}

// NewID returns an OpenAI-style chat completion ID, e.g. "chatcmpl-1f2a...".
func NewID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}
