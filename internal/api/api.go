// Package api serves the JSON API consumed by the Svelte panel and the
// embedded static UI. It depends only on small interfaces.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/improve"
	"github.com/vincent/tarea/internal/runlog"
	"github.com/vincent/tarea/internal/scheduler"
)

// Jobs lists and resolves jobs (config.Dir satisfies it).
type Jobs interface {
	List() ([]config.Job, error)
	Get(name string) (config.Job, error)
}

// Runs reads run history (runlog.Store satisfies it).
type Runs interface {
	List(job string, limit int) ([]runlog.Summary, error)
	Get(job, id string) (runlog.Record, error)
	CostSince(job string, since time.Time) (float64, error)
}

// Control drives the scheduler (scheduler.Scheduler satisfies it).
type Control interface {
	RunNow(name string) error
	Next(name string) time.Time
	Running(name string) bool
}

// Improver proposes job improvements (improve.Improver satisfies it).
type Improver interface {
	Propose(ctx context.Context, name string) (improve.Proposal, error)
	List(job string) ([]improve.Proposal, error)
	Accept(job, id string) (improve.Proposal, error)
	Reject(job, id string) (improve.Proposal, error)
}

// Deps are the handler's collaborators. UI and Improve may be nil.
type Deps struct {
	Jobs    Jobs
	Runs    Runs
	Control Control
	Improve Improver
	Memory  func(job config.Job) (string, error)
	UI      fs.FS
	Version string
	Started time.Time
	Now     func() time.Time
	Log     *slog.Logger
}

const (
	defaultLimit = 50
	maxLimit     = 500
	costWindow   = 7 * 24 * time.Hour
)

type server struct{ Deps }

// New builds the HTTP handler.
func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &server{d}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("GET /api/jobs/{name}", s.getJob)
	mux.HandleFunc("GET /api/jobs/{name}/runs", s.jobRuns)
	mux.HandleFunc("GET /api/jobs/{name}/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/jobs/{name}/memory", s.getMemory)
	mux.HandleFunc("POST /api/jobs/{name}/run", s.runNow)
	mux.HandleFunc("POST /api/jobs/{name}/improve", s.improve)
	mux.HandleFunc("GET /api/jobs/{name}/proposals", s.proposals)
	mux.HandleFunc("POST /api/jobs/{name}/proposals/{id}/accept", s.decide(true))
	mux.HandleFunc("POST /api/jobs/{name}/proposals/{id}/reject", s.decide(false))
	mux.HandleFunc("GET /api/runs", s.allRuns)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { s.fail(w, http.StatusNotFound, "not found") })
	mux.Handle("/", s.ui())
	return mux
}

// --- views -----------------------------------------------------------------

// JobView is the overview row for one job.
type JobView struct {
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Schedule  string          `json:"schedule"`
	Model     string          `json:"model"`
	BudgetUSD float64         `json:"budget_usd"`
	Running   bool            `json:"running"`
	NextRun   *time.Time      `json:"next_run"`
	LastRun   *runlog.Summary `json:"last_run"`
	Cost7d    float64         `json:"cost_7d"`
}

// JobDetail is a job's configuration with secrets removed.
type JobDetail struct {
	JobView
	Fallbacks []string   `json:"fallbacks"`
	MaxSteps  int        `json:"max_steps"`
	Prompt    string     `json:"prompt"`
	MCP       []MCPView  `json:"mcp"`
	Sinks     []SinkView `json:"sinks"`
	Memory    MemoryView `json:"memory"`
}

// MemoryView is the memory configuration.
type MemoryView struct {
	File  string `json:"file"`
	MaxKB int    `json:"max_kb"`
}

// MCPView lists an MCP server without credentials: only header/env key names.
type MCPView struct {
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	Target     string   `json:"target"`
	Allow      []string `json:"allow"`
	HeaderKeys []string `json:"header_keys"`
	EnvKeys    []string `json:"env_keys"`
}

// SinkView lists a sink without option values.
type SinkView struct {
	Type       string   `json:"type"`
	OptionKeys []string `json:"option_keys"`
}

func (s *server) view(j config.Job) (JobView, error) {
	v := JobView{
		Name: j.Name, Enabled: j.IsEnabled(), Schedule: j.Schedule, Model: j.Model,
		BudgetUSD: j.BudgetUSD, Running: s.Control.Running(j.Name),
	}
	if next := s.Control.Next(j.Name); !next.IsZero() {
		v.NextRun = &next
	}
	last, err := s.Runs.List(j.Name, 1)
	if err != nil {
		return v, err
	}
	if len(last) > 0 {
		v.LastRun = &last[0]
	}
	if v.Cost7d, err = s.Runs.CostSince(j.Name, s.Now().Add(-costWindow)); err != nil {
		return v, err
	}
	return v, nil
}

func redact(j config.Job, v JobView) JobDetail {
	d := JobDetail{JobView: v, Fallbacks: j.Fallbacks, MaxSteps: j.MaxSteps, Prompt: j.Prompt, Memory: MemoryView{File: j.Memory.File, MaxKB: j.Memory.MaxKB},
		MCP: []MCPView{}, Sinks: []SinkView{}}
	for _, m := range j.MCP {
		mv := MCPView{Name: m.Name, Allow: m.Allow, HeaderKeys: keys(m.Headers), EnvKeys: keys(m.Env)}
		if len(m.Command) > 0 {
			mv.Transport, mv.Target = "stdio", m.Command[0] // arguments may carry secrets: show the binary only.
		} else {
			mv.Transport, mv.Target = "http", hostOnly(m.URL)
		}
		d.MCP = append(d.MCP, mv)
	}
	for _, sk := range j.Sinks {
		d.Sinks = append(d.Sinks, SinkView{Type: sk.Type, OptionKeys: keys(sk.Options)})
	}
	if d.Fallbacks == nil {
		d.Fallbacks = []string{}
	}
	return d
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hostOnly strips path and query, which may embed tokens.
func hostOnly(raw string) string {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	return rest
}

// --- handlers --------------------------------------------------------------

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	s.json(w, http.StatusOK, map[string]any{
		"version":        s.Version,
		"uptime_seconds": int(s.Now().Sub(s.Started).Seconds()),
	})
}

func (s *server) listJobs(w http.ResponseWriter, _ *http.Request) {
	jobs, err := s.Jobs.List()
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]JobView, 0, len(jobs))
	for _, j := range jobs {
		v, verr := s.view(j)
		if verr != nil {
			s.internal(w, verr)
			return
		}
		out = append(out, v)
	}
	s.json(w, http.StatusOK, out)
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	v, err := s.view(j)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.json(w, http.StatusOK, redact(j, v))
}

func (s *server) jobRuns(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	s.runs(w, r, j.Name)
}

func (s *server) allRuns(w http.ResponseWriter, r *http.Request) { s.runs(w, r, "") }

func (s *server) runs(w http.ResponseWriter, r *http.Request, job string) {
	list, err := s.Runs.List(job, limit(r))
	if err != nil {
		s.internal(w, err)
		return
	}
	if list == nil {
		list = []runlog.Summary{}
	}
	s.json(w, http.StatusOK, list)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	rec, err := s.Runs.Get(j.Name, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	s.json(w, http.StatusOK, rec)
}

func (s *server) getMemory(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	text, err := s.Memory(j)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.json(w, http.StatusOK, map[string]string{"file": j.Memory.File, "content": text})
}

func (s *server) runNow(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	if err := s.Control.RunNow(j.Name); err != nil {
		s.mapErr(w, err)
		return
	}
	s.json(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *server) job(w http.ResponseWriter, r *http.Request) (config.Job, bool) {
	j, err := s.Jobs.Get(r.PathValue("name"))
	if err != nil {
		s.mapErr(w, err)
		return config.Job{}, false
	}
	return j, true
}

func limit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	switch {
	case err != nil || n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	}
	return n
}

// --- responses -------------------------------------------------------------

func (s *server) mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrJobNotFound), errors.Is(err, runlog.ErrNotFound):
		s.fail(w, http.StatusNotFound, "not found")
	case errors.Is(err, improve.ErrProposalNotFound):
		s.fail(w, http.StatusNotFound, "not found")
	case errors.Is(err, improve.ErrNoChanges):
		s.fail(w, http.StatusUnprocessableEntity, "nothing to improve")
	case errors.Is(err, improve.ErrStale), errors.Is(err, improve.ErrNotPending):
		s.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, scheduler.ErrBusy):
		s.fail(w, http.StatusConflict, "job already running")
	case errors.Is(err, scheduler.ErrStopping):
		s.fail(w, http.StatusServiceUnavailable, "shutting down")
	default:
		// A job file that exists but is invalid surfaces as a 422 with the validation text.
		if strings.HasPrefix(err.Error(), "job ") {
			s.fail(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.internal(w, err)
	}
}

func (s *server) internal(w http.ResponseWriter, err error) {
	s.Log.Error("api error", "err", err)
	s.fail(w, http.StatusInternalServerError, "internal error")
}

func (s *server) fail(w http.ResponseWriter, status int, msg string) {
	s.json(w, status, map[string]string{"error": msg})
}

func (s *server) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Log.Warn("encode response", "err", err)
	}
}

// --- static UI -------------------------------------------------------------

// ui serves the embedded SPA: real files as-is, anything else falls back to index.html.
func (s *server) ui() http.Handler {
	if s.UI == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			s.fail(w, http.StatusNotFound, "UI not embedded; build it with `make web`")
		})
	}
	files := http.FileServerFS(s.UI)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if info, err := fs.Stat(s.UI, p); err != nil || info.IsDir() {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r2)
			return
		}
		if strings.HasPrefix(p, "_app/immutable/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// --- improvement proposals --------------------------------------------------

// improveTimeout bounds one LLM analysis; the server's write timeout is shorter.
const improveTimeout = 3 * time.Minute

func (s *server) improve(w http.ResponseWriter, r *http.Request) {
	if s.Improve == nil {
		s.fail(w, http.StatusNotFound, "improve is not enabled")
		return
	}
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(improveTimeout + 10*time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), improveTimeout)
	defer cancel()
	p, err := s.Improve.Propose(ctx, j.Name)
	if errors.Is(err, improve.ErrNoChanges) {
		// Not an error for the panel: show the model's reasoning.
		s.json(w, http.StatusOK, map[string]any{"proposal": nil, "message": p.Rationale})
		return
	}
	if err != nil {
		s.mapErr(w, err)
		return
	}
	s.json(w, http.StatusCreated, map[string]any{"proposal": proposalView(p)})
}

func (s *server) proposals(w http.ResponseWriter, r *http.Request) {
	if s.Improve == nil {
		s.json(w, http.StatusOK, []improve.Proposal{})
		return
	}
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	list, err := s.Improve.List(j.Name)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	if list == nil {
		list = []improve.Proposal{}
	}
	for i := range list {
		list[i] = proposalView(list[i])
	}
	s.json(w, http.StatusOK, list)
}

func (s *server) decide(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Improve == nil {
			s.fail(w, http.StatusNotFound, "improve is not enabled")
			return
		}
		j, ok := s.job(w, r)
		if !ok {
			return
		}
		var p improve.Proposal
		var err error
		if accept {
			p, err = s.Improve.Accept(j.Name, r.PathValue("id"))
		} else {
			p, err = s.Improve.Reject(j.Name, r.PathValue("id"))
		}
		if err != nil {
			s.mapErr(w, err)
			return
		}
		s.json(w, http.StatusOK, proposalView(p))
	}
}

// proposalView drops the full YAML: the job file may hold literal credentials the panel never shows.
func proposalView(p improve.Proposal) improve.Proposal {
	p.NewYAML, p.BaseSHA = "", ""
	return p
}
