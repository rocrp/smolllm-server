package llm

import (
	"encoding/json"
	"fmt"

	openai "github.com/openai/openai-go/v3"
	"github.com/rocry/smolllm-go/smolllm"
)

// ChatRequest mirrors the OpenAI Chat Completions request body. Only the fields
// we actually forward are explicit; the rest is captured in Extras for future
// pass-through but currently ignored.
type ChatRequest struct {
	Model           string                                   `json:"model"`
	Messages        []openai.ChatCompletionMessageParamUnion `json:"messages"`
	Stream          bool                                     `json:"stream"`
	Temperature     *float64                                 `json:"temperature,omitempty"`
	TopP            *float64                                 `json:"top_p,omitempty"`
	ReasoningEffort *string                                  `json:"reasoning_effort,omitempty"`
	MaxTokens       *int                                     `json:"max_tokens,omitempty"`
	Stop            json.RawMessage                          `json:"stop,omitempty"`
	Seed            *int                                     `json:"seed,omitempty"`
	N               *int                                     `json:"n,omitempty"`
	// Timeout in seconds. 0 disables the timeout (relies on the request context).
	// When omitted, smolllm-go's default applies.
	Timeout *float64 `json:"timeout,omitempty"`

	// Pass-through fields: forwarded to the provider verbatim, never modeled.
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls json.RawMessage `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`

	// Unsupported; presence triggers 400. The legacy functions API is deprecated
	// upstream and superseded by tools.
	Functions json.RawMessage `json:"functions,omitempty"`

	// rawMessages keeps the messages exactly as the client sent them. The openai
	// param union drops keys it does not model, which would silently strip
	// provider extras (e.g. Gemini thought signatures) from a replayed turn.
	rawMessages []json.RawMessage
}

// UnmarshalJSON decodes the request and keeps the raw messages alongside it.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	type plain ChatRequest // avoid recursing into this method
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	*r = ChatRequest(decoded)
	r.rawMessages = envelope.Messages
	return nil
}

// EmbeddingRequest mirrors POST /v1/embeddings.
// Input may be a single string or an array of strings.
type EmbeddingRequest struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Dimensions *int            `json:"dimensions,omitempty"`
}

// ChatCompletion is the OpenAI non-streaming response shape.
type ChatCompletion struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []ChatChoice    `json:"choices"`
	Usage   CompletionUsage `json:"usage"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatMessage struct {
	Role string `json:"role"`
	// Content is null on an assistant turn that only requests tool calls.
	Content          *string            `json:"content"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []smolllm.ToolCall `json:"tool_calls,omitempty"`
}

// CompletionUsage is the OpenAI usage block. smolllm-go reports Input with the
// cached tokens taken out, so PromptTokens adds them back: OpenAI clients read
// prompt_tokens as everything the prompt cost, cached or not, and report the
// cached share separately under the details.
type CompletionUsage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// UsageFrom converts a smolllm Usage into the OpenAI block. The details appear
// only when the provider reported them, so a provider that meters neither cache
// reads nor reasoning produces the same three fields it always did.
func UsageFrom(usage smolllm.Usage) CompletionUsage {
	out := CompletionUsage{
		PromptTokens:            usage.Input + usage.CacheRead,
		CompletionTokens:        usage.Output,
		TotalTokens:             usage.Total,
		PromptTokensDetails:     nil,
		CompletionTokensDetails: nil,
	}
	if usage.CacheRead > 0 {
		out.PromptTokensDetails = &PromptTokensDetails{CachedTokens: usage.CacheRead}
	}
	if usage.Reasoning > 0 {
		out.CompletionTokensDetails = &CompletionTokensDetails{ReasoningTokens: usage.Reasoning}
	}
	return out
}

// ChatCompletionChunk is a single SSE frame for streaming chat.
type ChatCompletionChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []ChatChoiceDelta `json:"choices"`
	Error   *ChatStreamError  `json:"error,omitempty"`
}

type ChatStreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type ChatChoiceDelta struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type ChatDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ToolCalls stream as they arrive: one frame opens the slot with its id and
	// function name, later frames carry argument text only.
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is one streamed fragment of a tool call, in the shape OpenAI
// clients reassemble: Index identifies the slot, and every other field is
// present only in the frames that carry it.
type ToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function *ToolCallFunctionDelta `json:"function,omitempty"`
	// Extra carries the provider keys smolllm-go preserved on the call, e.g.
	// Gemini's thought signature, which the provider expects echoed back.
	Extra map[string]json.RawMessage `json:"-"`
}

// ToolCallFunctionDelta names the function and carries whatever argument text
// this frame contributed. Arguments is always written, empty included, because
// clients open their own accumulator on the frame that first sets it.
type ToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// OpenToolCall builds the frame that opens a tool-call slot, carrying everything
// about the call except its arguments.
func OpenToolCall(index int, call smolllm.ToolCall) ToolCallDelta {
	return ToolCallDelta{
		Index:    index,
		ID:       call.ID,
		Type:     call.Type,
		Function: &ToolCallFunctionDelta{Name: call.Function.Name, Arguments: ""},
		Extra:    call.Extra,
	}
}

// AppendToolCallArguments builds the frame that adds argument text to an open slot.
func AppendToolCallArguments(index int, arguments string) ToolCallDelta {
	return ToolCallDelta{
		Index:    index,
		ID:       "",
		Type:     "",
		Function: &ToolCallFunctionDelta{Name: "", Arguments: arguments},
		Extra:    nil,
	}
}

// MarshalJSON writes the modelled fields plus any preserved provider extras.
func (d ToolCallDelta) MarshalJSON() ([]byte, error) {
	type plain ToolCallDelta // avoid recursing into this method
	encoded, err := json.Marshal(plain(d))
	if err != nil {
		return nil, fmt.Errorf("encode tool call delta: %w", err)
	}
	if len(d.Extra) == 0 {
		return encoded, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("encode tool call delta extras: %w", err)
	}
	for key, value := range d.Extra {
		fields[key] = value
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode tool call delta extras: %w", err)
	}
	return out, nil
}

// EmbeddingResponse is the OpenAI /v1/embeddings response shape.
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingItem `json:"data"`
	Model  string          `json:"model"`
	Usage  EmbeddingUsage  `json:"usage"`
}

type EmbeddingItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ModelsResponse is the OpenAI /v1/models response shape.
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Created int64  `json:"created"`
}
