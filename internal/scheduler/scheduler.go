// Package scheduler turns job definitions into cron entries, prevents a job
// from overlapping itself, supports run-now, hot-reloads the jobs directory
// and shuts down gracefully.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/vincent/agentd/internal/config"
	"github.com/vincent/agentd/internal/runlog"
	"github.com/vincent/agentd/internal/runner"
)

// ErrBusy is returned by RunNow when the job is already running.
var ErrBusy = errors.New("scheduler: job already running")

// Runner executes a job (runner.Runner satisfies it).
type Runner interface {
	Run(ctx context.Context, job string, trig runner.Trigger) (runlog.Summary, error)
}

// Source provides jobs and cheap change detection (config.Dir satisfies it).
type Source interface {
	Load() ([]config.Job, error)
	Fingerprint() (string, error)
}

type entry struct {
	id       cron.EntryID
	schedule string
}

// Scheduler owns the cron loop. Create it with New.
type Scheduler struct {
	runner Runner
	log    *slog.Logger
	cron   *cron.Cron

	mu      sync.Mutex
	base    context.Context
	entries map[string]entry
	running map[string]bool
	wg      sync.WaitGroup
}

// New returns a stopped scheduler. A nil log discards output.
func New(r Runner, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Scheduler{
		runner:  r,
		log:     log,
		cron:    cron.New(),
		base:    context.Background(),
		entries: make(map[string]entry),
		running: make(map[string]bool),
	}
}

// Start begins firing jobs. ctx bounds every run started from now on.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	s.cron.Start()
}

// Stop stops scheduling and waits for in-flight runs until ctx expires.
func (s *Scheduler) Stop(ctx context.Context) error {
	stopped := s.cron.Stop()
	done := make(chan struct{})
	go func() {
		<-stopped.Done()
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("scheduler: shutdown deadline hit with runs in flight: %w", ctx.Err())
	}
}

// Sync makes the cron table match jobs: adds new, replaces re-scheduled and
// removes missing or disabled ones. Running jobs are never interrupted.
func (s *Scheduler) Sync(jobs []config.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	desired := make(map[string]config.Job, len(jobs))
	for _, j := range jobs {
		if j.IsEnabled() {
			desired[j.Name] = j
		}
	}

	for name, e := range s.entries {
		if j, ok := desired[name]; !ok || j.Schedule != e.schedule {
			s.cron.Remove(e.id)
			delete(s.entries, name)
		}
	}
	for name, j := range desired {
		if _, ok := s.entries[name]; ok {
			continue
		}
		id, err := s.cron.AddFunc(j.Schedule, func() { s.fire(name, runner.TriggerCron) })
		if err != nil {
			s.log.Error("invalid schedule", "job", name, "schedule", j.Schedule, "err", err)
			continue
		}
		s.entries[name] = entry{id: id, schedule: j.Schedule}
		s.log.Info("job scheduled", "job", name, "schedule", j.Schedule)
	}
}

// RunNow starts a manual run in the background.
func (s *Scheduler) RunNow(name string) error {
	if !s.begin(name) {
		return ErrBusy
	}
	s.spawn(name, runner.TriggerManual)
	return nil
}

func (s *Scheduler) fire(name string, trig runner.Trigger) {
	if !s.begin(name) {
		s.log.Warn("skipping run: previous run still in progress", "job", name)
		return
	}
	s.spawn(name, trig)
}

func (s *Scheduler) begin(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[name] {
		return false
	}
	s.running[name] = true
	return true
}

func (s *Scheduler) spawn(name string, trig runner.Trigger) {
	s.mu.Lock()
	ctx := s.base
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, name)
			s.mu.Unlock()
		}()
		if _, err := s.runner.Run(ctx, name, trig); err != nil {
			s.log.Error("run failed", "job", name, "trigger", trig, "err", err)
		}
	}()
}

// Next returns the next scheduled time, or the zero time when unscheduled.
func (s *Scheduler) Next(name string) time.Time {
	s.mu.Lock()
	e, ok := s.entries[name]
	s.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	return s.cron.Entry(e.id).Next
}

// Running reports whether the job has a run in flight.
func (s *Scheduler) Running(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[name]
}

// Watch syncs immediately, then re-syncs whenever the source fingerprint
// changes, until ctx is done. A job whose file becomes invalid is unscheduled
// (and the error logged) until it is fixed.
func (s *Scheduler) Watch(ctx context.Context, interval time.Duration, src Source) {
	last := s.reload(src, "")
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fp, err := src.Fingerprint()
			if err != nil {
				s.log.Warn("fingerprint jobs", "err", err)
				continue
			}
			if fp != last {
				last = s.reload(src, fp)
			}
		}
	}
}

func (s *Scheduler) reload(src Source, known string) string {
	jobs, err := src.Load()
	if err != nil {
		s.log.Warn("some jobs are invalid and were skipped", "err", err)
	}
	s.Sync(jobs)

	if known != "" {
		return known
	}
	fp, ferr := src.Fingerprint()
	if ferr != nil {
		s.log.Warn("fingerprint jobs", "err", ferr)
	}
	return fp
}
