// Package runlog persists run history as plain files:
//
//	<root>/runs.jsonl                       one Summary line per run (index)
//	<root>/state/<job>/runs/<id>.json       full Record (transcript, output)
package runlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vincent/tarea/internal/fsx"
	"github.com/vincent/tarea/internal/llm"
)

// ErrNotFound is returned by Get for an unknown run.
var ErrNotFound = errors.New("runlog: run not found")

// Status is the outcome of a run.
type Status string

// Run statuses.
const (
	StatusOK      Status = "ok"
	StatusPartial Status = "partial" // stopped by budget/steps/tool errors.
	StatusError   Status = "error"
)

const previewLen = 200

// Summary is the one-line index entry shown in the panel.
type Summary struct {
	ID               string    `json:"id"`
	Job              string    `json:"job"`
	Trigger          string    `json:"trigger"`
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at"`
	Status           Status    `json:"status"`
	Stop             string    `json:"stop,omitempty"`
	Model            string    `json:"model,omitempty"`
	Steps            int       `json:"steps"`
	ToolCalls        int       `json:"tool_calls"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Delivered        bool      `json:"delivered"`
	Error            string    `json:"error,omitempty"`
	Preview          string    `json:"preview,omitempty"`
}

// Record is the full on-disk run.
type Record struct {
	Summary
	Output   string        `json:"output"`
	Messages []llm.Message `json:"messages"`
}

// Store reads and writes run history under Root.
type Store struct {
	Root string
	mu   sync.Mutex
}

// NewID derives a sortable, filesystem-safe run id from the start time.
func NewID(t time.Time) string { return t.UTC().Format("20060102T150405.000Z") }

// Preview shortens text for the index.
func Preview(s string) string {
	r := []rune(s)
	if len(r) <= previewLen {
		return s
	}
	return string(r[:previewLen]) + "..."
}

func (s *Store) indexPath() string { return filepath.Join(s.Root, "runs.jsonl") }

func (s *Store) runPath(job, id string) (string, error) {
	if !fsx.ValidName(job) || !fsx.ValidName(id) {
		return "", fmt.Errorf("runlog: invalid job %q or id %q", job, id)
	}
	return filepath.Join(s.Root, "state", job, "runs", id+".json"), nil
}

// Write stores the full record then appends its summary to the index.
// The record is written first so the index never points at a missing file.
func (s *Store) Write(rec Record) error {
	p, err := s.runPath(rec.Job, rec.ID)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("runlog: encode record: %w", err)
	}
	line, err := json.Marshal(rec.Summary)
	if err != nil {
		return fmt.Errorf("runlog: encode summary: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err = fsx.WriteFileAtomic(p, raw, 0o600); err != nil {
		return err
	}
	return fsx.AppendLine(s.indexPath(), line)
}

// Get returns a full record.
func (s *Store) Get(job, id string) (Record, error) {
	p, err := s.runPath(job, id)
	if err != nil {
		return Record{}, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("%w: %s/%s", ErrNotFound, job, id)
	}
	if err != nil {
		return Record{}, fmt.Errorf("runlog: read %s: %w", p, err)
	}
	var rec Record
	if err = json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("runlog: decode %s: %w", p, err)
	}
	return rec, nil
}

// List returns summaries newest first. An empty job matches every job;
// limit <= 0 means no limit.
func (s *Store) List(job string, limit int) ([]Summary, error) {
	all, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	var out []Summary
	for i := len(all) - 1; i >= 0; i-- {
		if job != "" && all[i].Job != job {
			continue
		}
		out = append(out, all[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CostSince sums the cost of runs of job (all jobs if empty) started at or after since.
func (s *Store) CostSince(job string, since time.Time) (float64, error) {
	all, err := s.readIndex()
	if err != nil {
		return 0, err
	}
	var total float64
	for _, r := range all {
		if (job == "" || r.Job == job) && !r.StartedAt.Before(since) {
			total += r.CostUSD
		}
	}
	return total, nil
}

func (s *Store) readIndex() ([]Summary, error) {
	f, err := os.Open(s.indexPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runlog: open index: %w", err)
	}
	defer func() { _ = f.Close() }()

	var out []Summary
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var sum Summary
		if json.Unmarshal(sc.Bytes(), &sum) == nil && sum.ID != "" {
			out = append(out, sum)
		}
	}
	if err = sc.Err(); err != nil {
		return nil, fmt.Errorf("runlog: scan index: %w", err)
	}
	return out, nil
}

// Prune keeps the newest keepPerJob runs of every job, deleting older run
// files and rewriting the index atomically.
func (s *Store) Prune(keepPerJob int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readIndex()
	if err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].StartedAt.Before(all[j].StartedAt) })

	counts := make(map[string]int)
	keep := make([]bool, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		counts[all[i].Job]++
		keep[i] = counts[all[i].Job] <= keepPerJob
	}

	var buf []byte
	for i, sum := range all {
		if keep[i] {
			line, merr := json.Marshal(sum)
			if merr != nil {
				return fmt.Errorf("runlog: encode summary: %w", merr)
			}
			buf = append(append(buf, line...), '\n')
			continue
		}
		if p, perr := s.runPath(sum.Job, sum.ID); perr == nil {
			_ = os.Remove(p)
		}
	}
	return fsx.WriteFileAtomic(s.indexPath(), buf, 0o600)
}
