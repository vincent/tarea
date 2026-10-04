package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vincent/tarea/internal/api"
	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/runlog"
	"github.com/vincent/tarea/internal/scheduler"
)

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type fakeJobs struct{ jobs map[string]config.Job }

func (f fakeJobs) List() ([]config.Job, error) {
	out := make([]config.Job, 0, len(f.jobs))
	for _, j := range f.jobs {
		out = append(out, j)
	}
	return out, nil
}

func (f fakeJobs) Get(name string) (config.Job, error) {
	if j, ok := f.jobs[name]; ok {
		return j, nil
	}
	return config.Job{}, config.ErrJobNotFound
}

type fakeRuns struct{}

func (fakeRuns) List(job string, _ int) ([]runlog.Summary, error) {
	if job == "empty" {
		return nil, nil
	}
	return []runlog.Summary{{ID: "r1", Job: "gigs", Status: runlog.StatusOK, CostUSD: 0.02}}, nil
}

func (fakeRuns) Get(job, id string) (runlog.Record, error) {
	if id != "r1" {
		return runlog.Record{}, runlog.ErrNotFound
	}
	return runlog.Record{Summary: runlog.Summary{ID: "r1", Job: job}, Output: "digest"}, nil
}

func (fakeRuns) CostSince(string, time.Time) (float64, error) { return 0.14, nil }

type fakeControl struct{ err error }

func (f fakeControl) RunNow(string) error    { return f.err }
func (fakeControl) Next(string) time.Time    { return now.Add(time.Hour) }
func (fakeControl) Running(name string) bool { return name == "gigs" }

func newHandler(ctl fakeControl, ui bool) http.Handler {
	job := config.Job{
		Name: "gigs", Schedule: "0 8 * * *", Model: "m", BudgetUSD: 0.1, MaxSteps: 8, Prompt: "p",
		MCP: []config.MCPServer{
			{Name: "events", URL: "https://user:pw@example.com/mcp/SECRETPATH?token=SECRETQ", Headers: map[string]string{"Authorization": "Bearer SECRETHDR"}, Allow: []string{"*"}},
			{Name: "lib", Command: []string{"./mcp-lib", "--token=SECRETARG"}, Env: map[string]string{"KEY": "SECRETENV"}, Allow: []string{"x"}},
		},
		Sinks: []config.Sink{{Type: "telegram", Options: map[string]string{"chat_id": "SECRETCHAT"}}},
	}
	d := api.Deps{
		Jobs: fakeJobs{map[string]config.Job{"gigs": job}}, Runs: fakeRuns{}, Control: ctl,
		Memory:  func(config.Job) (string, error) { return "- notes\n", nil },
		Version: "test", Started: now.Add(-90 * time.Second), Now: func() time.Time { return now },
	}
	if ui {
		d.UI = fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}, "_app/immutable/a.js": {Data: []byte("js")}}
	}
	return api.New(d)
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, http.NoBody))
	return rec
}

func TestListJobsAndHealth(t *testing.T) {
	t.Parallel()
	h := newHandler(fakeControl{}, false)

	rec := do(h, http.MethodGet, "/api/jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var views []api.JobView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	v := views[0]
	if v.Name != "gigs" || !v.Running || v.NextRun == nil || v.LastRun == nil || v.LastRun.ID != "r1" || v.Cost7d != 0.14 {
		t.Fatalf("view: %+v", v)
	}

	var health map[string]any
	_ = json.Unmarshal(do(h, http.MethodGet, "/api/health").Body.Bytes(), &health)
	if health["version"] != "test" || health["uptime_seconds"] != float64(90) {
		t.Fatalf("health: %v", health)
	}
}

func TestJobDetailNeverLeaksSecrets(t *testing.T) {
	t.Parallel()
	rec := do(newHandler(fakeControl{}, false), http.MethodGet, "/api/jobs/gigs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"SECRETPATH", "SECRETQ", "SECRETHDR", "SECRETARG", "SECRETENV", "SECRETCHAT", "user:pw"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q:\n%s", secret, body)
		}
	}
	for _, want := range []string{"example.com", "Authorization", "./mcp-lib", "chat_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("response lacks %q", want)
		}
	}
}

func TestRunsAndRunDetail(t *testing.T) {
	t.Parallel()
	h := newHandler(fakeControl{}, false)

	if rec := do(h, http.MethodGet, "/api/jobs/gigs/runs?limit=5"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"r1"`) {
		t.Fatalf("runs: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodGet, "/api/runs"); rec.Code != http.StatusOK {
		t.Fatalf("all runs: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/jobs/gigs/runs/r1"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "digest") {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodGet, "/api/jobs/gigs/runs/zzz"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing run: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/jobs/gigs/memory"); !strings.Contains(rec.Body.String(), "notes") {
		t.Fatalf("memory: %s", rec.Body)
	}
}

func TestRunNowStatuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		err  error
		want int
	}{
		{"started", "/api/jobs/gigs/run", nil, http.StatusAccepted},
		{"busy", "/api/jobs/gigs/run", scheduler.ErrBusy, http.StatusConflict},
		{"unknown job", "/api/jobs/nope/run", nil, http.StatusNotFound},
		{"failure", "/api/jobs/gigs/run", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if rec := do(newHandler(fakeControl{err: tt.err}, false), http.MethodPost, tt.path); rec.Code != tt.want {
				t.Fatalf("status %d want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
	// A wrong method falls through to the JSON catch-all rather than triggering a run.
	if rec := do(newHandler(fakeControl{}, false), http.MethodGet, "/api/jobs/gigs/run"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET on run must not start a run, got %d", rec.Code)
	}
}

func TestUnknownAPIRouteIsJSON404(t *testing.T) {
	t.Parallel()
	rec := do(newHandler(fakeControl{}, true), http.MethodGet, "/api/nope")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("%d %s", rec.Code, rec.Header())
	}
}

func TestStaticUI(t *testing.T) {
	t.Parallel()
	h := newHandler(fakeControl{}, true)

	if rec := do(h, http.MethodGet, "/"); !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("index: %s", rec.Body)
	}
	rec := do(h, http.MethodGet, "/jobs/gigs") // client-side route: SPA fallback.
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html>app") {
		t.Fatalf("fallback: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, http.MethodGet, "/_app/immutable/a.js")
	if rec.Body.String() != "js" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %q %v", rec.Body, rec.Header())
	}

	if rec = do(newHandler(fakeControl{}, false), http.MethodGet, "/"); rec.Code != http.StatusNotFound {
		t.Fatalf("no UI must 404, got %d", rec.Code)
	}
}
