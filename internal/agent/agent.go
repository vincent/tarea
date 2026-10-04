// Package agent implements the LLM <-> tools loop for one job run.
//
// It performs no I/O of its own: the provider, tools, memory and seen-set are
// injected, which keeps this package (the critical block) fully unit-testable.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vincent/tarea/internal/llm"
)

// NothingNew is the sentinel a model returns when there is nothing to report;
// the runner skips sinks for it so quiet days stay quiet.
const NothingNew = "NOTHING_NEW"

const (
	maxToolOutputBytes = 16 << 10
	maxConsecToolErrs  = 3

	finishLength    = "length"
	truncatedMarker = "\n\n[truncated response]"
)

// Provider is the LLM backend.
type Provider interface {
	Chat(ctx context.Context, req llm.Request) (llm.Response, error)
}

// ToolHost exposes external tools (MCP) to the model.
type ToolHost interface {
	Tools() []llm.ToolDef
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// Memory is the long-lived notes file the model may read (via the prompt) and edit.
type Memory interface {
	Read() (string, error)
	Append(note string) error
	Replace(content string) error
}

// Seen is the code-managed dedupe set, never dependent on the model's recall.
type Seen interface {
	Has(key string) bool
	Add(keys ...string) error
}

// Spec is the immutable description of what to run.
type Spec struct {
	Job       string
	Prompt    string
	Model     string
	Fallbacks []string
	BudgetUSD float64 // <= 0 means unlimited.
	MaxSteps  int
	MaxTokens int // <= 0 means provider default.
}

// Deps are the injected collaborators. Tools, Memory, Seen and Now are optional.
type Deps struct {
	Provider Provider
	Tools    ToolHost
	Memory   Memory
	Seen     Seen
	Now      func() time.Time
	Log      *slog.Logger
}

// StopReason says why the loop ended.
type StopReason string

// Stop reasons.
const (
	StopDone       StopReason = "done"
	StopMaxSteps   StopReason = "max_steps"
	StopBudget     StopReason = "budget"
	StopCancelled  StopReason = "cancelled"
	StopToolErrors StopReason = "tool_errors"
	StopTruncated  StopReason = "truncated"
)

// Result is always returned, even alongside an error, so a failed run keeps its transcript.
type Result struct {
	Final     string
	Messages  []llm.Message
	Usage     llm.Usage
	Steps     int
	ToolCalls int
	Stop      StopReason
	Model     string
}

// Run executes the loop. Unrecoverable provider errors and cancellation are
// returned as errors together with the partial Result.
func Run(ctx context.Context, spec Spec, deps Deps) (Result, error) {
	if err := validate(spec, deps); err != nil {
		return Result{}, err
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	l := &loop{spec: spec, deps: deps, builtin: builtins{mem: deps.Memory, seen: deps.Seen}}
	if err := l.init(); err != nil {
		return Result{}, err
	}
	return l.run(ctx)
}

func validate(spec Spec, deps Deps) error {
	switch {
	case deps.Provider == nil:
		return errors.New("agent: provider is required")
	case strings.TrimSpace(spec.Model) == "":
		return errors.New("agent: model is required")
	case strings.TrimSpace(spec.Prompt) == "":
		return errors.New("agent: prompt is required")
	case spec.MaxSteps < 1:
		return errors.New("agent: max_steps must be >= 1")
	}
	return nil
}

type loop struct {
	spec       Spec
	deps       Deps
	builtin    builtins
	tools      []llm.ToolDef
	res        Result
	consecErrs int
}

func (l *loop) init() error {
	l.tools = l.builtin.defs()
	if l.deps.Tools != nil {
		l.tools = append(l.tools, l.deps.Tools.Tools()...)
	}

	system, err := l.systemPrompt()
	if err != nil {
		return err
	}
	l.res.Messages = []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: l.spec.Prompt},
	}
	return nil
}

func (l *loop) systemPrompt() (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are an unattended automation agent running the scheduled job %q. ", l.spec.Job)
	b.WriteString("Nobody can answer questions during the run: decide and act.\n")
	if l.deps.Now != nil {
		fmt.Fprintf(&b, "Current time: %s\n", l.deps.Now().UTC().Format(time.RFC3339))
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- Use tools to gather facts. Never invent data.\n")
	b.WriteString("- Your final message (the one without tool calls) is delivered verbatim to the user as a short plain-text digest.\n")
	fmt.Fprintf(&b, "- If there is nothing new to report, reply with exactly %s.\n", NothingNew)

	if l.deps.Memory != nil {
		mem, err := l.deps.Memory.Read()
		if err != nil {
			return "", fmt.Errorf("agent: read memory: %w", err)
		}
		if strings.TrimSpace(mem) == "" {
			mem = "(empty)"
		}
		b.WriteString("- Use memory_append for durable facts worth remembering; use memory_replace to rewrite memory more compactly when it is full.\n")
		b.WriteString("\n## Memory (notes you maintain across runs)\n")
		b.WriteString(mem)
		b.WriteString("\n")
	}
	if l.deps.Seen != nil {
		b.WriteString("\nUse seen_check before reporting an item and seen_add after reporting it, so the user never gets the same item twice.\n")
	}
	return b.String(), nil
}

func (l *loop) request(lastStep bool) llm.Request {
	req := llm.Request{
		Model:     l.spec.Model,
		Fallbacks: l.spec.Fallbacks,
		Messages:  l.res.Messages,
		MaxTokens: l.spec.MaxTokens,
	}
	if !lastStep { // on the last step force a text answer so the run always concludes.
		req.Tools = l.tools
	}
	return req
}

func (l *loop) run(ctx context.Context) (Result, error) {
	for step := 1; step <= l.spec.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return l.cancelled(err)
		}
		last := step == l.spec.MaxSteps

		resp, err := l.deps.Provider.Chat(ctx, l.request(last))
		l.res.Steps = step
		if err != nil {
			if ctx.Err() != nil {
				return l.cancelled(ctx.Err())
			}
			return l.res, fmt.Errorf("agent: step %d: %w", step, err)
		}
		l.record(resp)

		calls := resp.Message.ToolCalls
		truncated := resp.FinishReason == finishLength
		switch {
		case len(calls) == 0 && truncated:
			return l.finish(StopTruncated, resp.Message.Content+truncatedMarker), nil
		case len(calls) == 0:
			return l.finish(StopDone, resp.Message.Content), nil
		case last:
			return l.finish(StopMaxSteps, resp.Message.Content), nil
		case l.overBudget():
			return l.finish(StopBudget, resp.Message.Content), nil
		}

		if truncated { // the arguments are probably cut mid-JSON: do not run them.
			l.rejectTruncated(calls)
		} else {
			l.execTools(ctx, calls)
		}
		if l.consecErrs >= maxConsecToolErrs {
			return l.finish(StopToolErrors, ""), nil
		}
	}
	return l.finish(StopMaxSteps, ""), nil
}

func (l *loop) record(resp llm.Response) {
	l.res.Usage = l.res.Usage.Add(resp.Usage)
	if resp.Model != "" {
		l.res.Model = resp.Model
	}
	l.res.Messages = append(l.res.Messages, resp.Message)
	l.deps.Log.Debug("agent step", "step", l.res.Steps, "tool_calls", len(resp.Message.ToolCalls), "cost_usd", l.res.Usage.CostUSD)
}

func (l *loop) overBudget() bool {
	return l.spec.BudgetUSD > 0 && l.res.Usage.CostUSD >= l.spec.BudgetUSD
}

func (l *loop) finish(reason StopReason, final string) Result {
	l.res.Stop = reason
	l.res.Final = strings.TrimSpace(final)
	return l.res
}

func (l *loop) cancelled(err error) (Result, error) {
	l.res.Stop = StopCancelled
	return l.res, fmt.Errorf("agent: %w", err)
}

func (l *loop) execTools(ctx context.Context, calls []llm.ToolCall) {
	for _, call := range calls {
		l.res.ToolCalls++
		out, err := l.dispatch(ctx, call)
		if err != nil {
			l.consecErrs++
			out = "error: " + err.Error()
			l.deps.Log.Warn("tool call failed", "tool", call.Name, "err", err)
		} else {
			l.consecErrs = 0
		}
		l.res.Messages = append(l.res.Messages, llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: call.ID,
			Content:    truncate(out, maxToolOutputBytes),
		})
	}
}

// rejectTruncated answers every call of a cut-off turn with an error so the
// transcript stays well formed and the model can retry with shorter output.
func (l *loop) rejectTruncated(calls []llm.ToolCall) {
	l.consecErrs++
	l.deps.Log.Warn("model reply truncated by output limit, tool calls skipped", "calls", len(calls))
	for _, call := range calls {
		l.res.Messages = append(l.res.Messages, llm.Message{
			Role:       llm.RoleTool,
			ToolCallID: call.ID,
			Content:    "error: response truncated (output token limit); retry with shorter output",
		})
	}
}

func (l *loop) dispatch(ctx context.Context, call llm.ToolCall) (string, error) {
	args := json.RawMessage(call.Arguments)
	if strings.TrimSpace(call.Arguments) == "" {
		args = json.RawMessage("{}")
	}
	if out, handled, err := l.builtin.handle(call.Name, args); handled {
		return out, err
	}
	if l.deps.Tools == nil {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	out, err := l.deps.Tools.Call(ctx, call.Name, args)
	if err != nil {
		return "", fmt.Errorf("tool %s: %w", call.Name, err)
	}
	return out, nil
}

func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-cut)
}
