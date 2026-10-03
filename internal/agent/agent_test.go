package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincent/agentd/internal/agent"
	"github.com/vincent/agentd/internal/agent/agenttest"
	"github.com/vincent/agentd/internal/llm"
)

func spec() agent.Spec {
	return agent.Spec{Job: "gigs", Prompt: "do it", Model: "m", MaxSteps: 4, BudgetUSD: 1}
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: args}
}

func toolMessages(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			out = append(out, m)
		}
	}
	return out
}

func TestRun_NoTools(t *testing.T) {
	t.Parallel()
	p := &agenttest.Provider{Steps: []agenttest.Step{agenttest.Text("  hello  ", 0.01)}}

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "hello" || res.Stop != agent.StopDone || res.Steps != 1 || res.Usage.CostUSD != 0.01 || res.Model != "fake/model" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(p.Requests[0].Tools) != 0 {
		t.Fatal("no tools should be advertised without hosts, memory or seen")
	}
}

func TestRun_MultiStepToolUse(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{
		Defs:    []llm.ToolDef{{Name: "events__search"}},
		Handler: func(string, json.RawMessage) (string, error) { return "3 gigs", nil },
	}
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0.01, call("c1", "events__search", `{"artist":"x"}`)),
		agenttest.Text("digest", 0.02),
	}}

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "digest" || res.Steps != 2 || res.ToolCalls != 1 || res.Usage.CostUSD != 0.03 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(tools.Calls) != 1 || tools.Calls[0].Args != `{"artist":"x"}` {
		t.Fatalf("tool calls: %+v", tools.Calls)
	}
	// Step 2 request must contain the tool result and still advertise the tool.
	second := p.Requests[1]
	tm := toolMessages(second.Messages)
	if len(tm) != 1 || tm[0].Content != "3 gigs" || tm[0].ToolCallID != "c1" {
		t.Fatalf("tool message not fed back: %+v", tm)
	}
	if len(second.Tools) != 1 {
		t.Fatalf("tools missing on step 2: %+v", second.Tools)
	}
}

func TestRun_MaxStepsForcesTextOnLastStep(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{
		Defs:    []llm.ToolDef{{Name: "t__x"}},
		Handler: func(string, json.RawMessage) (string, error) { return "r", nil },
	}
	s := spec()
	s.MaxSteps = 2
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0, call("1", "t__x", "{}")),
		agenttest.Text("forced summary", 0),
	}}

	res, err := agent.Run(context.Background(), s, agent.Deps{Provider: p, Tools: tools})
	if err != nil || res.Stop != agent.StopDone || res.Final != "forced summary" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(p.Requests[1].Tools) != 0 {
		t.Fatal("last step must not advertise tools")
	}
}

func TestRun_MaxStepsWhenModelIgnoresNoTools(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{Defs: []llm.ToolDef{{Name: "t__x"}}, Handler: func(string, json.RawMessage) (string, error) { return "r", nil }}
	s := spec()
	s.MaxSteps = 2
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0, call("1", "t__x", "{}")),
		agenttest.Calls(0, call("2", "t__x", "{}")),
	}}

	res, err := agent.Run(context.Background(), s, agent.Deps{Provider: p, Tools: tools})
	if err != nil || res.Stop != agent.StopMaxSteps || len(tools.Calls) != 1 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, len(tools.Calls))
	}
}

func TestRun_BudgetStopsBeforeExecutingTools(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{Defs: []llm.ToolDef{{Name: "t__x"}}, Handler: func(string, json.RawMessage) (string, error) { return "r", nil }}
	s := spec()
	s.BudgetUSD = 0.05
	p := &agenttest.Provider{Steps: []agenttest.Step{agenttest.Calls(0.06, call("1", "t__x", "{}"))}}

	res, err := agent.Run(context.Background(), s, agent.Deps{Provider: p, Tools: tools})
	if err != nil || res.Stop != agent.StopBudget || len(tools.Calls) != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, len(tools.Calls))
	}
}

func TestRun_ZeroBudgetMeansUnlimited(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{Defs: []llm.ToolDef{{Name: "t__x"}}, Handler: func(string, json.RawMessage) (string, error) { return "r", nil }}
	s := spec()
	s.BudgetUSD = 0
	p := &agenttest.Provider{Steps: []agenttest.Step{agenttest.Calls(99, call("1", "t__x", "{}")), agenttest.Text("ok", 99)}}

	res, err := agent.Run(context.Background(), s, agent.Deps{Provider: p, Tools: tools})
	if err != nil || res.Stop != agent.StopDone {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &agenttest.Provider{Steps: []agenttest.Step{agenttest.Text("never", 0)}}

	res, err := agent.Run(ctx, spec(), agent.Deps{Provider: p})
	if !errors.Is(err, context.Canceled) || res.Stop != agent.StopCancelled || len(p.Requests) != 0 {
		t.Fatalf("res=%+v err=%v requests=%d", res, err, len(p.Requests))
	}
}

func TestRun_CancelDuringProviderCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	p := &agenttest.Provider{Steps: []agenttest.Step{{Err: context.Canceled}}}
	cancel()

	res, err := agent.Run(ctx, spec(), agent.Deps{Provider: p})
	if !errors.Is(err, context.Canceled) || res.Stop != agent.StopCancelled {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRun_ProviderErrorKeepsPartialResult(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tools := &agenttest.Tools{Defs: []llm.ToolDef{{Name: "t__x"}}, Handler: func(string, json.RawMessage) (string, error) { return "r", nil }}
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0.01, call("1", "t__x", "{}")),
		{Err: boom},
	}}

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p, Tools: tools})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if res.Usage.CostUSD != 0.01 || len(res.Messages) < 4 {
		t.Fatalf("partial result lost: %+v", res)
	}
}

func TestRun_ToolErrorIsFedBackThenAbortsAfterRepeatedFailures(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{
		Defs:    []llm.ToolDef{{Name: "t__x"}},
		Handler: func(string, json.RawMessage) (string, error) { return "", errors.New("upstream down") },
	}
	s := spec()
	s.MaxSteps = 10
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0, call("1", "t__x", "{}")),
		agenttest.Calls(0, call("2", "t__x", "{}")),
		agenttest.Calls(0, call("3", "t__x", "{}")),
	}}

	res, err := agent.Run(context.Background(), s, agent.Deps{Provider: p, Tools: tools})
	if err != nil || res.Stop != agent.StopToolErrors {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	tm := toolMessages(res.Messages)
	if len(tm) != 3 || !strings.HasPrefix(tm[0].Content, "error: ") || !strings.Contains(tm[0].Content, "upstream down") {
		t.Fatalf("tool errors not fed back: %+v", tm)
	}
}

func TestRun_ToolErrorCounterResetsOnSuccess(t *testing.T) {
	t.Parallel()
	n := 0
	tools := &agenttest.Tools{
		Defs: []llm.ToolDef{{Name: "t__x"}},
		Handler: func(string, json.RawMessage) (string, error) {
			n++
			if n%2 == 1 {
				return "", errors.New("flaky")
			}
			return "fine", nil
		},
	}
	s := spec()
	s.MaxSteps = 8
	steps := make([]agenttest.Step, 0, 7)
	for i := range 6 {
		steps = append(steps, agenttest.Calls(0, call(string(rune('a'+i)), "t__x", "{}")))
	}
	steps = append(steps, agenttest.Text("done", 0))
	p := &agenttest.Provider{Steps: steps}

	res, err := agent.Run(context.Background(), s, agent.Deps{Provider: p, Tools: tools})
	if err != nil || res.Stop != agent.StopDone {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRun_UnknownToolAndMalformedArgs(t *testing.T) {
	t.Parallel()
	mem := &agenttest.Memory{}
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0, call("1", "nope", "{}"), call("2", "memory_append", "{not json")),
		agenttest.Text("ok", 0),
	}}

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p, Memory: mem})
	if err != nil || res.Stop != agent.StopDone {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	tm := toolMessages(res.Messages)
	if len(tm) != 2 || !strings.Contains(tm[0].Content, "unknown tool") || !strings.Contains(tm[1].Content, "invalid arguments") {
		t.Fatalf("tool messages: %+v", tm)
	}
}

func TestRun_OversizeToolOutputIsTruncated(t *testing.T) {
	t.Parallel()
	tools := &agenttest.Tools{
		Defs:    []llm.ToolDef{{Name: "t__x"}},
		Handler: func(string, json.RawMessage) (string, error) { return strings.Repeat("é", 20000), nil },
	}
	p := &agenttest.Provider{Steps: []agenttest.Step{agenttest.Calls(0, call("1", "t__x", "")), agenttest.Text("ok", 0)}}

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	out := toolMessages(res.Messages)[0].Content
	if len(out) > 16<<10+64 || !strings.Contains(out, "[truncated") {
		t.Fatalf("not truncated: len=%d", len(out))
	}
	if strings.ContainsRune(out, '\uFFFD') {
		t.Fatal("truncation split a multi-byte rune")
	}
	if tools.Calls[0].Args != "{}" {
		t.Fatalf("empty args must default to {}: %q", tools.Calls[0].Args)
	}
}

func TestRun_MemoryInPromptAndMemoryTools(t *testing.T) {
	t.Parallel()
	mem := &agenttest.Memory{Content: "likes Bonobo\n", Cap: 40}
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0,
			call("1", "memory_append", `{"note":"moved to Lyon"}`),
			call("2", "memory_append", `{"note":"this note is far too long to fit in the cap"}`),
			call("3", "memory_replace", `{"content":"Bonobo fan, Lyon\n"}`),
		),
		agenttest.Text("ok", 0),
	}}
	now := func() time.Time { return time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC) }

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p, Memory: mem, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	system := p.Requests[0].Messages[0].Content
	if !strings.Contains(system, "likes Bonobo") || !strings.Contains(system, "2026-10-04T08:00:00Z") || !strings.Contains(system, agent.NothingNew) {
		t.Fatalf("system prompt incomplete:\n%s", system)
	}
	tm := toolMessages(res.Messages)
	if tm[0].Content != "ok" || !strings.HasPrefix(tm[1].Content, "error: ") {
		t.Fatalf("append results: %+v", tm)
	}
	if mem.Content != "Bonobo fan, Lyon\n" {
		t.Fatalf("memory = %q", mem.Content)
	}
	names := map[string]bool{}
	for _, d := range p.Requests[0].Tools {
		names[d.Name] = true
	}
	if !names["memory_append"] || !names["memory_replace"] || names["seen_check"] {
		t.Fatalf("advertised tools: %v", names)
	}
}

func TestRun_SeenTools(t *testing.T) {
	t.Parallel()
	seen := &agenttest.Seen{Keys: map[string]bool{"a": true}}
	p := &agenttest.Provider{Steps: []agenttest.Step{
		agenttest.Calls(0, call("1", "seen_check", `{"keys":["a","b"]}`), call("2", "seen_add", `{"keys":["b"]}`), call("3", "seen_check", `{"keys":[]}`)),
		agenttest.Text("ok", 0),
	}}

	res, err := agent.Run(context.Background(), spec(), agent.Deps{Provider: p, Seen: seen})
	if err != nil {
		t.Fatal(err)
	}
	tm := toolMessages(res.Messages)
	if tm[0].Content != `{"seen":["a"],"unseen":["b"]}` || tm[1].Content != "added 1 key(s)" || !strings.Contains(tm[2].Content, "keys must not be empty") {
		t.Fatalf("tool messages: %+v", tm)
	}
	if !seen.Has("b") {
		t.Fatal("seen_add did not persist")
	}
}

func TestRun_Validation(t *testing.T) {
	t.Parallel()
	good := spec()
	p := &agenttest.Provider{}
	tests := map[string]struct {
		mut  func(*agent.Spec)
		deps agent.Deps
	}{
		"no provider": {func(*agent.Spec) {}, agent.Deps{}},
		"no model":    {func(s *agent.Spec) { s.Model = "" }, agent.Deps{Provider: p}},
		"no prompt":   {func(s *agent.Spec) { s.Prompt = " " }, agent.Deps{Provider: p}},
		"no steps":    {func(s *agent.Spec) { s.MaxSteps = 0 }, agent.Deps{Provider: p}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := good
			tt.mut(&s)
			if _, err := agent.Run(context.Background(), s, tt.deps); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
