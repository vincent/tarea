// Package agenttest provides in-memory fakes for the agent's injected
// dependencies. They are used by agent, runner and scheduler tests.
package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/vincent/tarea/internal/llm"
)

// Step is one scripted provider reply.
type Step struct {
	Resp llm.Response
	Err  error
}

// Text builds a final-answer step.
func Text(content string, cost float64) Step {
	return Step{Resp: llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: content},
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: cost},
		Model:   "fake/model",
	}}
}

// Calls builds a step that requests tool calls (id, name, argsJSON triples).
func Calls(cost float64, calls ...llm.ToolCall) Step {
	return Step{Resp: llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls},
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: cost},
		Model:   "fake/model",
	}}
}

// Provider replays Steps in order and records every request it receives.
type Provider struct {
	mu       sync.Mutex
	Steps    []Step
	Requests []llm.Request
}

// Chat implements agent.Provider.
func (p *Provider) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Snapshot messages: the agent appends to the same slice afterwards.
	req.Messages = append([]llm.Message(nil), req.Messages...)
	p.Requests = append(p.Requests, req)

	if err := ctx.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("fake provider: %w", err)
	}
	if len(p.Steps) == 0 {
		return llm.Response{}, errors.New("fake provider: script exhausted")
	}
	s := p.Steps[0]
	p.Steps = p.Steps[1:]
	return s.Resp, s.Err
}

// ToolCall records one invocation of Tools.
type ToolCall struct {
	Name string
	Args string
}

// Tools is a scriptable agent.ToolHost.
type Tools struct {
	mu      sync.Mutex
	Defs    []llm.ToolDef
	Handler func(name string, args json.RawMessage) (string, error)
	Calls   []ToolCall
}

// Tools implements agent.ToolHost.
func (t *Tools) Tools() []llm.ToolDef { return t.Defs }

// Call implements agent.ToolHost.
func (t *Tools) Call(_ context.Context, name string, args json.RawMessage) (string, error) {
	t.mu.Lock()
	t.Calls = append(t.Calls, ToolCall{Name: name, Args: string(args)})
	h := t.Handler
	t.mu.Unlock()
	if h == nil {
		return "", errors.New("no handler")
	}
	return h(name, args)
}

// Memory is an in-memory agent.Memory with an optional byte cap.
type Memory struct {
	mu      sync.Mutex
	Content string
	Cap     int
}

// ErrFull is returned when Cap would be exceeded.
var ErrFull = errors.New("memory full")

// Read implements agent.Memory.
func (m *Memory) Read() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Content, nil
}

// Append implements agent.Memory.
func (m *Memory) Append(note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.Content + "- " + note + "\n"
	if m.Cap > 0 && len(next) > m.Cap {
		return ErrFull
	}
	m.Content = next
	return nil
}

// Replace implements agent.Memory.
func (m *Memory) Replace(content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Cap > 0 && len(content) > m.Cap {
		return ErrFull
	}
	m.Content = content
	return nil
}

// Seen is an in-memory agent.Seen.
type Seen struct {
	mu   sync.Mutex
	Keys map[string]bool
}

// Has implements agent.Seen.
func (s *Seen) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Keys[key]
}

// Add implements agent.Seen.
func (s *Seen) Add(keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Keys == nil {
		s.Keys = make(map[string]bool)
	}
	for _, k := range keys {
		s.Keys[k] = true
	}
	return nil
}
