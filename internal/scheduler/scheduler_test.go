package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/runlog"
	"github.com/vincent/tarea/internal/runner"
	"github.com/vincent/tarea/internal/scheduler"
)

type fakeRunner struct {
	mu      sync.Mutex
	runs    []string
	trigger []runner.Trigger
	block   chan struct{} // when non-nil, Run waits for it to be closed.
	started chan string
}

func (f *fakeRunner) Run(_ context.Context, job string, trig runner.Trigger) (runlog.Summary, error) {
	f.mu.Lock()
	f.runs = append(f.runs, job)
	f.trigger = append(f.trigger, trig)
	block, started := f.block, f.started
	f.mu.Unlock()
	if started != nil {
		started <- job
	}
	if block != nil {
		<-block
	}
	return runlog.Summary{}, nil
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func job(name, sched string, enabled bool) config.Job {
	return config.Job{Name: name, Schedule: sched, Enabled: &enabled}
}

func TestSync_AddsReplacesAndRemoves(t *testing.T) {
	t.Parallel()
	s := scheduler.New(&fakeRunner{}, nil)
	s.Start()
	defer func() { _ = s.Stop(context.Background()) }()

	s.Sync([]config.Job{job("a", "0 8 * * *", true), job("b", "*/5 * * * *", true), job("off", "0 8 * * *", false)})
	if s.Next("a").IsZero() || s.Next("b").IsZero() {
		t.Fatal("enabled jobs must be scheduled")
	}
	if !s.Next("off").IsZero() {
		t.Fatal("disabled jobs must not be scheduled")
	}

	nextA := s.Next("a")
	s.Sync([]config.Job{job("a", "0 9 * * *", true)}) // a re-scheduled, b removed.
	if s.Next("a").Equal(nextA) {
		t.Fatal("schedule change must replace the entry")
	}
	if !s.Next("b").IsZero() {
		t.Fatal("removed job must be unscheduled")
	}
}

func TestRunNow_RunsAndRejectsOverlap(t *testing.T) {
	t.Parallel()
	fr := &fakeRunner{block: make(chan struct{}), started: make(chan string, 1)}
	s := scheduler.New(fr, nil)
	s.Start()

	if err := s.RunNow("a"); err != nil {
		t.Fatal(err)
	}
	<-fr.started
	if !s.Running("a") {
		t.Fatal("job should be marked running")
	}
	if err := s.RunNow("a"); !errors.Is(err, scheduler.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if err := s.RunNow("other"); err != nil { // other jobs are independent.
		t.Fatal(err)
	}

	close(fr.block)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Running("a") || fr.count() != 2 {
		t.Fatalf("running=%v count=%d", s.Running("a"), fr.count())
	}
	if fr.trigger[0] != runner.TriggerManual {
		t.Fatalf("trigger = %v", fr.trigger)
	}
}

func TestStop_DeadlineWithRunInFlight(t *testing.T) {
	t.Parallel()
	fr := &fakeRunner{block: make(chan struct{}), started: make(chan string, 1)}
	s := scheduler.New(fr, nil)
	s.Start()
	if err := s.RunNow("a"); err != nil {
		t.Fatal(err)
	}
	<-fr.started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	close(fr.block) // release the goroutine so the test does not leak it.
}

type fakeSource struct {
	mu   sync.Mutex
	jobs []config.Job
	fp   string
}

func (f *fakeSource) Load() ([]config.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs, nil
}

func (f *fakeSource) Fingerprint() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fp, nil
}

func (f *fakeSource) set(fp string, jobs ...config.Job) {
	f.mu.Lock()
	f.fp, f.jobs = fp, jobs
	f.mu.Unlock()
}

func TestWatch_PicksUpChanges(t *testing.T) {
	t.Parallel()
	src := &fakeSource{}
	src.set("v1", job("a", "0 8 * * *", true))
	s := scheduler.New(&fakeRunner{}, nil)
	s.Start()
	defer func() { _ = s.Stop(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Watch(ctx, 5*time.Millisecond, src); close(done) }()

	waitFor(t, func() bool { return !s.Next("a").IsZero() })
	src.set("v2", job("b", "0 8 * * *", true))
	waitFor(t, func() bool { return !s.Next("b").IsZero() && s.Next("a").IsZero() })

	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// ctxRunner blocks until released or until its run context is cancelled.
type ctxRunner struct {
	started   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func (c *ctxRunner) Run(ctx context.Context, _ string, _ runner.Trigger) (runlog.Summary, error) {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		close(c.cancelled)
	}
	return runlog.Summary{}, nil
}

func newCtxRunner() *ctxRunner {
	return &ctxRunner{started: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{})}
}

func TestStop_LetsInFlightRunFinishWithinGrace(t *testing.T) {
	t.Parallel()
	cr := newCtxRunner()
	s := scheduler.New(cr, nil)
	s.Start()
	if err := s.RunNow("a"); err != nil {
		t.Fatal(err)
	}
	<-cr.started

	go func() {
		time.Sleep(30 * time.Millisecond)
		close(cr.release)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("run finishing inside the grace must not error: %v", err)
	}
	select {
	case <-cr.cancelled:
		t.Fatal("run was cancelled although it finished within the grace")
	default:
	}
	if err := s.RunNow("a"); !errors.Is(err, scheduler.ErrStopping) {
		t.Fatalf("want ErrStopping after Stop, got %v", err)
	}
}

func TestStop_CancelsRunsWhenGraceExpires(t *testing.T) {
	t.Parallel()
	cr := newCtxRunner()
	s := scheduler.New(cr, nil)
	s.Start()
	if err := s.RunNow("a"); err != nil {
		t.Fatal(err)
	}
	<-cr.started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	select {
	case <-cr.cancelled:
	case <-time.After(time.Second):
		t.Fatal("run must be cancelled once the grace expires")
	}
}
