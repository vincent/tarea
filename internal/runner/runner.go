// Package runner wires everything needed for ONE job run: lock, memory, MCP
// connections, the agent loop, delivery to sinks and the run log.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent/agentd/internal/agent"
	"github.com/vincent/agentd/internal/config"
	"github.com/vincent/agentd/internal/fsx"
	"github.com/vincent/agentd/internal/mcpx"
	"github.com/vincent/agentd/internal/memory"
	"github.com/vincent/agentd/internal/runlog"
	"github.com/vincent/agentd/internal/sink"
)

// Trigger records what started a run.
type Trigger string

// Triggers.
const (
	TriggerCron   Trigger = "cron"
	TriggerManual Trigger = "manual"
	TriggerCLI    Trigger = "cli"
)

// ErrBusy is returned when the job already has a run in progress.
var ErrBusy = errors.New("runner: job already running")

// Jobs resolves job definitions (config.Dir satisfies it).
type Jobs interface {
	Get(name string) (config.Job, error)
}

// SinkBuilder instantiates sinks (sink.Registry satisfies it).
type SinkBuilder interface {
	Build(typ string, opts map[string]string) (sink.Sink, error)
}

// RunStore persists finished runs (runlog.Store satisfies it).
type RunStore interface {
	Write(rec runlog.Record) error
}

// Runner executes jobs. Construct it with a struct literal; all fields except
// DryRun, Log, Now and LockStale are required.
type Runner struct {
	DataDir  string
	Jobs     Jobs
	Provider agent.Provider
	Dial     mcpx.Dialer
	Sinks    SinkBuilder
	Runs     RunStore

	// DryRun, when set, receives the final message instead of the sinks.
	DryRun io.Writer
	Log    *slog.Logger
	Now    func() time.Time
	// LockStale is how long a lock file may live before it is considered abandoned.
	LockStale time.Duration
}

const defaultLockStale = 2 * time.Hour

// Run executes one job. A Summary is returned whenever the run started, even
// together with an error; failed runs are logged to the run store too.
func (r *Runner) Run(ctx context.Context, name string, trig Trigger) (runlog.Summary, error) {
	r.defaults()

	job, err := r.Jobs.Get(name)
	if err != nil {
		return runlog.Summary{}, fmt.Errorf("runner: %w", err)
	}

	stateDir, err := fsx.SafeJoin(r.DataDir, filepath.Join("state", job.Name))
	if err != nil {
		return runlog.Summary{}, fmt.Errorf("runner: %w", err)
	}
	unlock, err := fsx.TryLock(filepath.Join(stateDir, ".lock"), r.LockStale, r.Now)
	if errors.Is(err, fsx.ErrLocked) {
		return runlog.Summary{}, ErrBusy
	}
	if err != nil {
		return runlog.Summary{}, fmt.Errorf("runner: %w", err)
	}
	defer func() {
		if uerr := unlock(); uerr != nil {
			r.Log.Warn("release lock", "job", name, "err", uerr)
		}
	}()

	started := r.Now()
	rec := runlog.Record{Summary: runlog.Summary{
		ID: runlog.NewID(started), Job: job.Name, Trigger: string(trig), StartedAt: started.UTC(),
	}}
	r.Log.Info("run started", "job", job.Name, "run", rec.ID, "trigger", trig)

	res, runErr := r.execute(ctx, job, stateDir)
	r.fill(&rec, res, runErr)

	if runErr == nil {
		runErr = r.deliver(ctx, job, &rec)
		if runErr != nil {
			rec.Status = runlog.StatusError
			rec.Error = runErr.Error()
		}
	}

	rec.EndedAt = r.Now().UTC()
	if werr := r.Runs.Write(rec); werr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("runner: write run log: %w", werr))
	}
	r.Log.Info("run finished", "job", job.Name, "run", rec.ID, "status", rec.Status, "cost_usd", rec.CostUSD)
	return rec.Summary, runErr
}

func (r *Runner) defaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Log == nil {
		r.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.LockStale == 0 {
		r.LockStale = defaultLockStale
	}
}

func (r *Runner) execute(ctx context.Context, job config.Job, stateDir string) (agent.Result, error) {
	mem := memory.New(filepath.Join(stateDir, job.Memory.File), job.Memory.MaxKB)
	seen, err := memory.OpenSeen(filepath.Join(stateDir, "seen.jsonl"), r.Now)
	if err != nil {
		return agent.Result{}, fmt.Errorf("runner: %w", err)
	}

	deps := agent.Deps{Provider: r.Provider, Memory: mem, Seen: seen, Now: r.Now, Log: r.Log.With("job", job.Name)}

	if len(job.MCP) > 0 {
		host, herr := mcpx.Open(ctx, job.MCP, r.Dial)
		if herr != nil {
			return agent.Result{}, fmt.Errorf("runner: %w", herr)
		}
		defer func() {
			if cerr := host.Close(); cerr != nil {
				r.Log.Warn("close mcp", "job", job.Name, "err", cerr)
			}
		}()
		deps.Tools = host
	}

	return agent.Run(ctx, agent.Spec{
		Job: job.Name, Prompt: job.Prompt, Model: job.Model, Fallbacks: job.Fallbacks,
		BudgetUSD: job.BudgetUSD, MaxSteps: job.MaxSteps,
	}, deps)
}

func (r *Runner) fill(rec *runlog.Record, res agent.Result, runErr error) {
	rec.Model = res.Model
	rec.Steps = res.Steps
	rec.ToolCalls = res.ToolCalls
	rec.PromptTokens = res.Usage.PromptTokens
	rec.CompletionTokens = res.Usage.CompletionTokens
	rec.CostUSD = res.Usage.CostUSD
	rec.Stop = string(res.Stop)
	rec.Output = res.Final
	rec.Messages = res.Messages
	rec.Preview = runlog.Preview(res.Final)

	switch {
	case runErr != nil:
		rec.Status = runlog.StatusError
		rec.Error = runErr.Error()
	case res.Stop == agent.StopDone:
		rec.Status = runlog.StatusOK
	default:
		rec.Status = runlog.StatusPartial
	}
}

// deliver sends the final message unless there is nothing to say.
func (r *Runner) deliver(ctx context.Context, job config.Job, rec *runlog.Record) error {
	text := strings.TrimSpace(rec.Output)
	if text == "" || text == agent.NothingNew {
		return nil
	}
	if rec.Status == runlog.StatusPartial {
		text = fmt.Sprintf("(partial run, stopped: %s)\n\n%s", rec.Stop, text)
	}
	msg := sink.Message{Job: job.Name, Text: text}

	if r.DryRun != nil {
		_, err := fmt.Fprintf(r.DryRun, "--- %s ---\n%s\n", job.Name, text)
		if err != nil {
			return fmt.Errorf("runner: dry-run output: %w", err)
		}
		return nil
	}

	var errs []error
	for _, sc := range job.Sinks {
		s, err := r.Sinks.Build(sc.Type, sc.Options)
		if err == nil {
			err = s.Send(ctx, msg)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("deliver via %s: %w", sc.Type, err))
		}
	}
	rec.Delivered = len(errs) == 0
	return errors.Join(errs...)
}
