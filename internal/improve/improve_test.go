package improve_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/improve"
	"github.com/vincent/tarea/internal/llm"
	"github.com/vincent/tarea/internal/runlog"
)

const jobYAML = `# my job
schedule: "0 * * * *"
model: m1
fallbacks: [m2]
max_tokens: 1000
max_steps: 8
prompt: |
  Do the thing.
mcp:
  - name: web
    builtin: http
    hosts: ["example.com"]
    headers:
      Authorization: "Bearer ${SECRET}"
    allow: ["*"]
sinks:
  - type: telegram
    chat_id: "${CHAT}"
`

type fakeLLM struct {
	reply string
	err   error
	got   llm.Request
}

func (f *fakeLLM) Chat(_ context.Context, r llm.Request) (llm.Response, error) {
	f.got = r
	return llm.Response{Message: llm.Message{Content: f.reply}, Usage: llm.Usage{CostUSD: 0.01}}, f.err
}

func setup(t *testing.T, reply string, sums []runlog.Summary) (*improve.Improver, *fakeLLM, string) {
	t.Helper()
	root := t.TempDir()
	jobs := filepath.Join(root, "jobs")
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, "j.yaml"), []byte(jobYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &runlog.Store{Root: root}
	for _, s := range sums {
		s.Job = "j"
		if err := store.Write(runlog.Record{Summary: s, Messages: []llm.Message{
			{Role: llm.RoleTool, Content: "x" + "...[truncated 5 bytes]"},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeLLM{reply: reply}
	lookup := func(string) (string, bool) { return "v", true }
	return &improve.Improver{
		DataDir: root, Jobs: config.Dir{Path: jobs, Lookup: lookup}, Runs: store, Provider: f,
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}, f, jobs
}

func truncRuns(n int) []runlog.Summary {
	out := make([]runlog.Summary, 0, n)
	for i := range n {
		out = append(out, runlog.Summary{
			ID: runlog.NewID(time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC)), Status: runlog.StatusPartial,
			Stop: "truncated", StopDetail: "final answer cut", Model: "m1", Steps: 3,
			PromptTokens: 9000, CompletionTokens: 100,
		})
	}
	return out
}

func TestPropose_RulesAndLLMMerged(t *testing.T) {
	t.Parallel()
	im, f, jobs := setup(t, "```json\n"+`{"rationale":"cut it","changes":{"prompt":"Do it.\nBriefly.","model":"m2"}}`+"\n```", truncRuns(3))
	p, err := im.Propose(context.Background(), "j")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, c := range p.Changes {
		got[c.Field] = c.To
	}
	if got["max_tokens"] != 2000 || got["model"] != "m2" || !strings.Contains(got["prompt"].(string), "Briefly.") {
		t.Fatalf("changes = %v", got)
	}
	if p.Rationale != "cut it" || p.Status != improve.StatusPending || p.CostUSD != 0.01 {
		t.Fatalf("proposal = %+v", p)
	}
	for _, want := range []string{"${SECRET}", "${CHAT}", "# my job"} {
		if !strings.Contains(p.NewYAML, want) {
			t.Errorf("new yaml lost %q:\n%s", want, p.NewYAML)
		}
	}
	if strings.Contains(f.got.Messages[1].Content, "Bearer") || strings.Contains(f.got.Messages[1].Content, "SECRET") {
		t.Error("secrets leaked into the meta prompt")
	}
	if p.Stats.ToolCutRuns != 3 || p.Stats.Truncated != 3 {
		t.Errorf("stats = %+v", p.Stats)
	}

	// Accept: history kept, job replaced, stale/replay refused.
	if _, err = im.Accept("j", p.ID); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(filepath.Join(jobs, "j.yaml"))
	if string(cur) != p.NewYAML {
		t.Error("job file not replaced")
	}
	hist, _ := filepath.Glob(filepath.Join(jobs, ".history", "j.*.yaml"))
	if len(hist) != 1 {
		t.Fatalf("history = %v", hist)
	}
	if old, _ := os.ReadFile(hist[0]); string(old) != jobYAML {
		t.Error("history copy differs from the original")
	}
	if _, err = im.Accept("j", p.ID); !errors.Is(err, improve.ErrNotPending) {
		t.Errorf("replay err = %v", err)
	}
}

func TestPropose_RejectsNonWhitelistedAndBadValues(t *testing.T) {
	t.Parallel()
	reply := `{"rationale":"x","changes":{"schedule":"* * * * *","mcp":[],"model":"gpt-unknown","max_steps":999,"max_tokens":512}}`
	im, _, _ := setup(t, reply, []runlog.Summary{{ID: "a", Status: runlog.StatusOK, Model: "m1", Steps: 2}})
	p, err := im.Propose(context.Background(), "j")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].Field != "max_tokens" {
		t.Fatalf("changes = %+v", p.Changes)
	}
	if strings.Contains(p.NewYAML, "* * * * *") {
		t.Error("schedule was edited")
	}
}

func TestPropose_NoChanges(t *testing.T) {
	t.Parallel()
	im, _, _ := setup(t, `{"rationale":"fine as is","changes":{}}`, []runlog.Summary{{ID: "a", Status: runlog.StatusOK, Model: "m1", Steps: 7}})
	p, err := im.Propose(context.Background(), "j")
	if !errors.Is(err, improve.ErrNoChanges) || p.Rationale != "fine as is" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestPropose_LLMFailureFallsBackToRules(t *testing.T) {
	t.Parallel()
	im, f, _ := setup(t, "", truncRuns(2))
	f.err = errors.New("boom")
	p, err := im.Propose(context.Background(), "j")
	if err != nil || len(p.Changes) != 1 || !strings.Contains(p.Rationale, "boom") {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestAccept_StaleJobFile(t *testing.T) {
	t.Parallel()
	im, _, jobs := setup(t, `{"rationale":"r","changes":{"max_steps":5}}`, []runlog.Summary{{ID: "a", Status: runlog.StatusOK, Model: "m1", Steps: 2}})
	p, err := im.Propose(context.Background(), "j")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(jobs, "j.yaml"), []byte(jobYAML+"\n# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = im.Accept("j", p.ID); !errors.Is(err, improve.ErrStale) {
		t.Fatalf("err = %v", err)
	}
}

func TestReject(t *testing.T) {
	t.Parallel()
	im, _, _ := setup(t, `{"rationale":"r","changes":{"max_steps":5}}`, []runlog.Summary{{ID: "a", Status: runlog.StatusOK, Model: "m1", Steps: 2}})
	p, _ := im.Propose(context.Background(), "j")
	if _, err := im.Reject("j", p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := im.List("j")
	if len(got) != 1 || got[0].Status != improve.StatusRejected {
		t.Fatalf("list = %+v", got)
	}
}
