package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent/tarea/internal/agent"
	"github.com/vincent/tarea/internal/agent/agenttest"
	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/llm"
	"github.com/vincent/tarea/internal/mcpx"
	"github.com/vincent/tarea/internal/runlog"
	"github.com/vincent/tarea/internal/runner"
	"github.com/vincent/tarea/internal/sink"
)

type jobs map[string]config.Job

func (j jobs) Get(name string) (config.Job, error) {
	if job, ok := j[name]; ok {
		return job, nil
	}
	return config.Job{}, config.ErrJobNotFound
}

type recordingSink struct {
	mu   sync.Mutex
	msgs []sink.Message
	err  error
}

func (s *recordingSink) Send(_ context.Context, m sink.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.msgs = append(s.msgs, m)
	return nil
}

type builder struct{ s *recordingSink }

func (b builder) Build(typ string, _ map[string]string) (sink.Sink, error) {
	if typ != "fake" {
		return nil, errors.New("unknown sink")
	}
	return b.s, nil
}

type fakeSession struct{ closed bool }

func (f *fakeSession) ListTools(context.Context) ([]mcpx.Tool, error) {
	return []mcpx.Tool{{Name: "top_artists"}}, nil
}

func (f *fakeSession) CallTool(context.Context, string, json.RawMessage) (string, error) {
	return "Bonobo, Four Tet", nil
}
func (f *fakeSession) Close() error { f.closed = true; return nil }

type env struct {
	dir  string
	snk  *recordingSink
	sess *fakeSession
	r    *runner.Runner
}

func newEnv(t *testing.T, steps ...agenttest.Step) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{dir: dir, snk: &recordingSink{}, sess: &fakeSession{}}
	job := config.Job{
		Name: "gigs", Schedule: "0 8 * * *", Model: "m", Prompt: "find gigs", BudgetUSD: 1, MaxSteps: 5,
		Memory: config.Memory{File: "memory.md", MaxKB: 4},
		MCP:    []config.MCPServer{{Name: "library", Command: []string{"x"}, Allow: []string{"top_artists"}}},
		Sinks:  []config.Sink{{Type: "fake"}},
	}
	clock := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	e.r = &runner.Runner{
		DataDir:  dir,
		Jobs:     jobs{"gigs": job},
		Provider: &agenttest.Provider{Steps: steps},
		Dial:     func(context.Context, config.MCPServer) (mcpx.Session, error) { return e.sess, nil },
		Sinks:    builder{e.snk},
		Runs:     &runlog.Store{Root: dir},
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
	}
	return e
}

func TestRun_EndToEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t,
		agenttest.Calls(0.01,
			llm.ToolCall{ID: "1", Name: "library__top_artists", Arguments: "{}"},
			llm.ToolCall{ID: "2", Name: "memory_append", Arguments: `{"note":"top: Bonobo"}`},
			llm.ToolCall{ID: "3", Name: "seen_add", Arguments: `{"keys":["gig-1"]}`},
		),
		agenttest.Text("Bonobo plays Lyon on Nov 3", 0.02),
	)

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != runlog.StatusOK || !sum.Delivered || sum.ToolCalls != 3 || sum.CostUSD != 0.03 || sum.Trigger != "manual" {
		t.Fatalf("summary: %+v", sum)
	}
	if len(e.snk.msgs) != 1 || e.snk.msgs[0].Text != "Bonobo plays Lyon on Nov 3" || e.snk.msgs[0].Job != "gigs" {
		t.Fatalf("sink: %+v", e.snk.msgs)
	}
	if !e.sess.closed {
		t.Fatal("MCP session leaked")
	}

	mem, _ := os.ReadFile(filepath.Join(e.dir, "state", "gigs", "memory.md"))
	if string(mem) != "- top: Bonobo\n" {
		t.Fatalf("memory = %q", mem)
	}
	seen, _ := os.ReadFile(filepath.Join(e.dir, "state", "gigs", "seen.jsonl"))
	if !strings.Contains(string(seen), "gig-1") {
		t.Fatalf("seen = %q", seen)
	}

	store := &runlog.Store{Root: e.dir}
	rec, err := store.Get("gigs", sum.ID)
	if err != nil || rec.Output != "Bonobo plays Lyon on Nov 3" || len(rec.Messages) < 4 {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if _, statErr := os.Stat(filepath.Join(e.dir, "state", "gigs", ".lock")); !os.IsNotExist(statErr) {
		t.Fatal("lock not released")
	}
}

func TestRun_NothingNewSkipsSinksButIsLogged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Text(agent.NothingNew, 0.01))

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron)
	if err != nil || sum.Status != runlog.StatusOK || sum.Delivered {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
	if len(e.snk.msgs) != 0 {
		t.Fatalf("sink must stay quiet: %+v", e.snk.msgs)
	}
	if runs, _ := (&runlog.Store{Root: e.dir}).List("gigs", 0); len(runs) != 1 {
		t.Fatal("quiet run must still be logged")
	}
}

func TestRun_ProviderErrorIsLoggedAndNotDelivered(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Step{Err: errors.New("provider down")})

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron)
	if err == nil || sum.Status != runlog.StatusError || !strings.Contains(sum.Error, "provider down") {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
	// The only message is the failure alert, never a digest.
	if len(e.snk.msgs) != 1 || !strings.Contains(e.snk.msgs[0].Text, "failed") || !e.sess.closed {
		t.Fatalf("msgs=%+v closed=%v", e.snk.msgs, e.sess.closed)
	}
	if runs, _ := (&runlog.Store{Root: e.dir}).List("gigs", 0); len(runs) != 1 {
		t.Fatal("failed run must be logged")
	}
}

func TestRun_PartialRunIsDeliveredWithNotice(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Step{Resp: llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: "half done", ToolCalls: []llm.ToolCall{{ID: "1", Name: "library__top_artists", Arguments: "{}"}}},
		Usage:   llm.Usage{CostUSD: 5}, // far over the 1 USD budget.
	}})

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron)
	if err != nil || sum.Status != runlog.StatusPartial || sum.Stop != "budget" {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
	if len(e.snk.msgs) != 1 || !strings.HasPrefix(e.snk.msgs[0].Text, "(partial run, stopped: budget)") {
		t.Fatalf("msgs = %+v", e.snk.msgs)
	}
}

func TestRun_SinkFailureMarksRunAsError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Text("hello", 0))
	e.snk.err = errors.New("telegram 400")

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron)
	if err == nil || sum.Status != runlog.StatusError || sum.Delivered || !strings.Contains(sum.Error, "telegram 400") {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
}

func TestRun_DryRunWritesToWriterNotSinks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Text("hello", 0))
	var out bytes.Buffer
	e.r.DryRun = &out

	if _, err := e.r.Run(context.Background(), "gigs", runner.TriggerCLI); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--- gigs ---\nhello") || len(e.snk.msgs) != 0 {
		t.Fatalf("out=%q msgs=%v", out.String(), e.snk.msgs)
	}
}

func TestRun_BusyAndUnknownJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Text("x", 0))
	lock := filepath.Join(e.dir, "state", "gigs", ".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.r.Now = time.Now

	if _, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron); !errors.Is(err, runner.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if _, err := e.r.Run(context.Background(), "nope", runner.TriggerCron); !errors.Is(err, config.ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestRun_MCPConnectFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Text("x", 0))
	e.r.Dial = func(context.Context, config.MCPServer) (mcpx.Session, error) { return nil, errors.New("spawn failed") }

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron)
	if err == nil || sum.Status != runlog.StatusError || !strings.Contains(sum.Error, "spawn failed") {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
}

func readState(t *testing.T, e *env, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "state", "gigs", file))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func stateCalls() agenttest.Step {
	return agenttest.Calls(0,
		llm.ToolCall{ID: "1", Name: "memory_append", Arguments: `{"note":"n"}`},
		llm.ToolCall{ID: "2", Name: "seen_add", Arguments: `{"keys":["gig-1"]}`},
	)
}

func TestRun_FailedDeliveryDoesNotCommitSeenOrMemory(t *testing.T) {
	t.Parallel()
	e := newEnv(t, stateCalls(), agenttest.Text("digest", 0))
	e.snk.err = errors.New("telegram down")

	if _, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron); err == nil {
		t.Fatal("want delivery error")
	}
	if got := readState(t, e, "seen.jsonl"); got != "" {
		t.Fatalf("seen committed despite failed delivery: %q", got)
	}
	if got := readState(t, e, "memory.md"); got != "" {
		t.Fatalf("memory committed despite failed delivery: %q", got)
	}
}

func TestRun_DryRunDoesNotCommitState(t *testing.T) {
	t.Parallel()
	e := newEnv(t, stateCalls(), agenttest.Text("digest", 0))
	e.r.DryRun = &bytes.Buffer{}

	if _, err := e.r.Run(context.Background(), "gigs", runner.TriggerCLI); err != nil {
		t.Fatal(err)
	}
	if readState(t, e, "seen.jsonl") != "" || readState(t, e, "memory.md") != "" {
		t.Fatal("dry run must not write state")
	}
}

func TestRun_NothingNewStillCommitsState(t *testing.T) {
	t.Parallel()
	e := newEnv(t, stateCalls(), agenttest.Text(agent.NothingNew, 0))

	if _, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readState(t, e, "seen.jsonl"), "gig-1") {
		t.Fatal("state must be committed when there is nothing to deliver")
	}
}

func TestRun_PartialRunWithoutTextIsStillAnnounced(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Step{Resp: llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "library__top_artists", Arguments: "{}"}}},
		Usage:   llm.Usage{CostUSD: 5},
	}})

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerCron)
	if err != nil || sum.Status != runlog.StatusPartial || !sum.Delivered {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
	if len(e.snk.msgs) != 1 || !strings.Contains(e.snk.msgs[0].Text, "stopped: budget") {
		t.Fatalf("msgs = %+v", e.snk.msgs)
	}
}

func TestRun_FailureAlertIsRateLimitedAndResets(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Step{Err: errors.New("bad key")}, agenttest.Step{Err: errors.New("bad key")},
		agenttest.Text("fine", 0), agenttest.Step{Err: errors.New("bad key")})
	var now = time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	e.r.Now = func() time.Time { return now }
	run := func() { _, _ = e.r.Run(context.Background(), "gigs", runner.TriggerCron) }
	alerts := func() int {
		n := 0
		for _, m := range e.snk.msgs {
			if strings.Contains(m.Text, "failed") {
				n++
			}
		}
		return n
	}

	run()
	if alerts() != 1 || !strings.Contains(e.snk.msgs[0].Text, "bad key") {
		t.Fatalf("first failure must alert: %+v", e.snk.msgs)
	}
	now = now.Add(time.Hour)
	run() // still failing, within 6h.
	if alerts() != 1 {
		t.Fatalf("repeat failure must be rate-limited: %+v", e.snk.msgs)
	}
	now = now.Add(time.Hour)
	run() // success resets the streak.
	now = now.Add(time.Minute)
	run() // new failure after success alerts immediately.
	if alerts() != 2 {
		t.Fatalf("failure after recovery must alert again: %+v", e.snk.msgs)
	}
}

func TestRun_NoAlertWhenCancelled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, agenttest.Text("x", 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := e.r.Run(ctx, "gigs", runner.TriggerCron); err == nil {
		t.Fatal("want cancellation error")
	}
	if len(e.snk.msgs) != 0 {
		t.Fatalf("shutdown cancel must not alert: %+v", e.snk.msgs)
	}
}

type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

func TestRun_TimeoutStopsRunAndReleasesLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Provider = blockingProvider{}
	e.r.RunTimeout = 50 * time.Millisecond

	sum, err := e.r.Run(context.Background(), "gigs", runner.TriggerManual)
	if err == nil || sum.Status != runlog.StatusError {
		t.Fatalf("want error run, got %+v, %v", sum, err)
	}
	if _, statErr := os.Stat(filepath.Join(e.dir, "state", "gigs", ".lock")); !os.IsNotExist(statErr) {
		t.Fatalf("lock not released: %v", statErr)
	}
}
