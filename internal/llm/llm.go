// Package llm abstracts chat-completion providers. The only wire protocol is
// the OpenAI-compatible Chat Completions API (corporate AI gateway, Kong AI
// Gateway, OpenRouter, Gemini, Groq, Ollama...), plus a deterministic demo
// provider for tests and zero-cost demos.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// Roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON
}

// Message is one chat message in provider-neutral form.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	Name       string     `json:"name,omitempty"` // tool name for role=tool
}

// ToolDef declares a tool the model may call.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request is a chat completion request.
type Request struct {
	Model       string
	Messages    []Message
	Tools       []ToolDef
	Temperature *float64
	MaxTokens   int
	JSONMode    bool
	// Purpose tags the call for accounting ("interpreter", "router", "subagent", "summary", "deck").
	Purpose string
}

// Usage is token accounting for one call.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Add accumulates usage.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
}

// Response is the assembled result of a (possibly streamed) completion.
type Response struct {
	Message      Message
	Usage        Usage
	FinishReason string
	Model        string
}

// Provider produces completions. onText receives text deltas as they stream
// (it may be nil); the returned Response holds the full message.
type Provider interface {
	Chat(ctx context.Context, req Request, onText func(delta string)) (*Response, error)
	// Name identifies the provider for logs and the UI ("openai", "demo").
	Name() string
}

// ErrUnavailable signals a definitive provider failure after retries.
var ErrUnavailable = errors.New("serviço de IA indisponível")

// EstimateTokens is a cheap heuristic (≈4 chars per token) for budgeting.
func EstimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			n += (len(tc.Name) + len(tc.Arguments)) / 4
		}
		n += 4
	}
	return n
}
