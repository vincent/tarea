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

	"github.com/vincent/tarea/internal/agent"
	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/fsx"
	"github.com/vincent/tarea/internal/mcpx"
	"github.com/vincent/tarea/internal/memory"
	"github.com/vincent/tarea/internal/runlog"
	"github.com/vincent/tarea/internal/sink"
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

// Speaker turns text into audio (llm.OpenRouter satisfies it).
type Speaker interface {
	Speech(ctx context.Context, text string) ([]byte, error)
}

// maxSpeechRunes bounds the text sent to text-to-speech.
const maxSpeechRunes = 4000

// speechTimeout bounds text-to-speech and audio upload, independent of the run
// deadline (the text has already been delivered by then).
const speechTimeout = 2 * time.Minute

// Runner executes jobs. Construct it with a struct literal; all fields except
// DryRun, Speaker, Log, Now, LockStale and RunTimeout are required.
type Runner struct {
	DataDir  string
	Jobs     Jobs
	Provider agent.Provider
	Dial     mcpx.Dialer
	Sinks    SinkBuilder
	Runs     RunStore

	// Speaker, when set, makes each delivered final message also go out as audio
	// to sinks that support it. Failures are logged, never fatal.
	Speaker Speaker
	// DryRun, when set, receives the final message instead of the sinks.
	DryRun io.Writer
	Log    *slog.Logger
	Now    func() time.Time
	// LockStale is how long a lock may go without a heartbeat before it is
	// considered abandoned by a crashed process.
	LockStale time.Duration
	// RunTimeout bounds the agent loop of one run.
	RunTimeout time.Duration
}

const (
	defaultLockStale  = 2 * time.Minute
	defaultRunTimeout = 30 * time.Minute
)

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

	ectx, cancel := context.WithTimeout(ctx, r.RunTimeout)
	res, st, runErr := r.execute(ectx, job, stateDir)
	cancel()
	r.fill(&rec, res, runErr)

	if runErr == nil {
		runErr = r.deliver(ctx, job, &rec)
	}
	// Seen keys and memory edits reach disk only once the user has the digest,
	// so a failed delivery is retried with the same items on the next run.
	if runErr == nil && r.DryRun == nil && st != nil {
		runErr = st.commit()
	}
	if runErr != nil {
		rec.Status = runlog.StatusError
		rec.Error = runErr.Error()
	}

	rec.EndedAt = r.Now().UTC()
	if werr := r.Runs.Write(rec); werr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("runner: write run log: %w", werr))
	}
	r.alert(ctx, job, stateDir, rec.Summary)
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
	if r.RunTimeout == 0 {
		r.RunTimeout = defaultRunTimeout
	}
}

// staged holds the run's buffered state writes.
type staged struct {
	mem  *memory.StagedStore
	seen *memory.StagedSeen
}

func (s *staged) commit() error {
	return errors.Join(s.seen.Commit(), s.mem.Commit())
}

func (r *Runner) execute(ctx context.Context, job config.Job, stateDir string) (agent.Result, *staged, error) {
	seenSet, err := memory.OpenSeen(filepath.Join(stateDir, "seen.jsonl"), r.Now)
	if err != nil {
		return agent.Result{}, nil, fmt.Errorf("runner: %w", err)
	}
	st := &staged{
		mem:  memory.New(filepath.Join(stateDir, job.Memory.File), job.Memory.MaxKB).Stage(),
		seen: seenSet.Stage(),
	}

	deps := agent.Deps{Provider: r.Provider, Memory: st.mem, Seen: st.seen, Now: r.Now, Log: r.Log.With("job", job.Name)}

	if len(job.MCP) > 0 {
		host, herr := mcpx.Open(ctx, job.MCP, r.Dial)
		if herr != nil {
			return agent.Result{}, nil, fmt.Errorf("runner: %w", herr)
		}
		defer func() {
			if cerr := host.Close(); cerr != nil {
				r.Log.Warn("close mcp", "job", job.Name, "err", cerr)
			}
		}()
		deps.Tools = host
	}

	res, err := agent.Run(ctx, agent.Spec{
		Job: job.Name, Prompt: job.Prompt, Model: job.Model, Fallbacks: job.Fallbacks,
		BudgetUSD: job.BudgetUSD, MaxSteps: job.MaxSteps, MaxTokens: job.MaxTokens,
	}, deps)
	return res, st, err
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

// deliver sends the final message unless there is nothing to say. A partial
// run is always announced, even when the model produced no final text.
func (r *Runner) deliver(ctx context.Context, job config.Job, rec *runlog.Record) error {
	text := strings.TrimSpace(rec.Output)
	if rec.Status == runlog.StatusPartial {
		if text == "" {
			text = "(no final message was produced)"
		}
		text = fmt.Sprintf("(partial run, stopped: %s)\n\n%s", rec.Stop, text)
	} else if text == "" || text == agent.NothingNew {
		return nil
	}

	if r.DryRun != nil {
		if _, err := fmt.Fprintf(r.DryRun, "--- %s ---\n%s\n", job.Name, text); err != nil {
			return fmt.Errorf("runner: dry-run output: %w", err)
		}
		return nil
	}

	sent, err := r.sendAll(ctx, job, text, rec.Status == runlog.StatusOK)
	rec.Delivered = err == nil && sent > 0
	return err
}

// sendAll sends text to every sink of the job and reports how many accepted it.
// With speak set, audio follows to the sinks that took the text and opted in
// with `audio: true`.
func (r *Runner) sendAll(ctx context.Context, job config.Job, text string, speak bool) (int, error) {
	msg := sink.Message{Job: job.Name, Text: text}
	var (
		errs      []error
		sent      int
		audioSink []sink.AudioSink
	)
	for _, sc := range job.Sinks {
		s, err := r.Sinks.Build(sc.Type, sc.Options)
		if err == nil {
			err = s.Send(ctx, msg)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("deliver via %s: %w", sc.Type, err))
			continue
		}
		sent++
		if !sc.WantsAudio() {
			continue
		}
		if as, ok := s.(sink.AudioSink); ok {
			audioSink = append(audioSink, as)
		} else if speak {
			r.Log.Warn("audio: true ignored, sink cannot send audio", "job", job.Name, "sink", sc.Type)
		}
	}
	if speak && r.Speaker != nil && len(audioSink) > 0 {
		r.sendAudio(ctx, job.Name, text, audioSink)
	}
	return sent, errors.Join(errs...)
}

// sendAudio synthesizes text once and sends it to each sink. Errors are only
// logged: the text already reached the user.
func (r *Runner) sendAudio(ctx context.Context, job, text string, sinks []sink.AudioSink) {
	if rs := []rune(text); len(rs) > maxSpeechRunes {
		text = string(rs[:maxSpeechRunes])
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), speechTimeout)
	defer cancel()
	audio, err := r.Speaker.Speech(ctx, text)
	if err != nil {
		r.Log.Warn("tts failed", "job", job, "err", err)
		return
	}
	msg := sink.Message{Job: job, Audio: audio}
	for _, as := range sinks {
		if err = as.SendAudio(ctx, msg); err != nil {
			r.Log.Warn("deliver audio", "job", job, "err", err)
		}
	}
}
