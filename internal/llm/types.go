package llm

import (
	"encoding/json"

	openai "github.com/openai/openai-go/v3"
)

// ChatRequest mirrors the OpenAI Chat Completions request body. Only the fields
// we actually forward are explicit; the rest is captured in Extras for future
// pass-through but currently ignored.
type ChatRequest struct {
	Model           string                                  `json:"model"`
	Messages        []openai.ChatCompletionMessageParamUnion `json:"messages"`
	Stream          bool                                    `json:"stream"`
	Temperature     *float64                                `json:"temperature,omitempty"`
	TopP            *float64                                `json:"top_p,omitempty"`
	ReasoningEffort *string                                 `json:"reasoning_effort,omitempty"`
	MaxTokens       *int                                    `json:"max_tokens,omitempty"`
	N               *int                                    `json:"n,omitempty"`

	// Unsupported in v1; presence triggers 400.
	Tools          json.RawMessage `json:"tools,omitempty"`
	Functions      json.RawMessage `json:"functions,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
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
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []ChatChoice   `json:"choices"`
	Usage   CompletionUsage `json:"usage"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type CompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionChunk is a single SSE frame for streaming chat.
type ChatCompletionChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []ChatChoiceDelta  `json:"choices"`
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
