// Package llm holds the provider-neutral chat types and the OpenRouter client.
package llm

import "encoding/json"

// Role is a chat message role.
type Role string

// Chat roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one chat message. Tool results use RoleTool with ToolCallID.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a model-requested tool invocation; Arguments is raw JSON text.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef advertises a callable tool; Parameters is a JSON Schema object.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request is a single chat completion request.
type Request struct {
	Model     string
	Fallbacks []string
	Messages  []Message
	Tools     []ToolDef
}

// Usage reports tokens and the provider-reported cost for one call.
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + o.PromptTokens,
		CompletionTokens: u.CompletionTokens + o.CompletionTokens,
		CostUSD:          u.CostUSD + o.CostUSD,
	}
}

// Response is the assistant's reply.
type Response struct {
	Message      Message
	Usage        Usage
	Model        string // model that actually served the call (after fallbacks).
	FinishReason string
}
