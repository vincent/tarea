package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/fsx"
	"github.com/vincent/tarea/internal/runlog"
)

const (
	alertFile     = ".alert.json"
	alertEvery    = 6 * time.Hour
	alertMaxError = 500
)

// alertState is persisted so a restart neither re-alerts nor forgets a streak.
type alertState struct {
	LastAlert time.Time `json:"last_alert"`
}

// alert tells the job's sinks that a run failed, so a broken job is not
// mistaken for a quiet day. It alerts on the first failure after a success and
// then at most every alertEvery while the job keeps failing. Recovery resets it.
func (r *Runner) alert(ctx context.Context, job config.Job, stateDir string, sum runlog.Summary) {
	path := filepath.Join(stateDir, alertFile)

	if sum.Status != runlog.StatusError {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.Log.Warn("reset failure alert", "job", job.Name, "err", err)
		}
		return
	}
	// Shutdown cancels runs; that is not a job failure worth a message.
	if r.DryRun != nil || ctx.Err() != nil {
		return
	}

	var st alertState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st) // unreadable state = alert again.
	}
	if !st.LastAlert.IsZero() && r.Now().Sub(st.LastAlert) < alertEvery {
		return
	}

	text := fmt.Sprintf("tarea: job %q failed (run %s): %s", job.Name, sum.ID, clip(sum.Error, alertMaxError))
	sent, err := r.sendAll(ctx, job, text, false)
	if err != nil {
		r.Log.Warn("failure alert not fully delivered", "job", job.Name, "err", err)
	}
	if sent == 0 {
		return // retry on the next failure.
	}
	st.LastAlert = r.Now().UTC()
	b, err := json.Marshal(st)
	if err == nil {
		err = fsx.WriteFileAtomic(path, b, 0o600)
	}
	if err != nil {
		r.Log.Warn("persist failure alert state", "job", job.Name, "err", err)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // do not cut inside a UTF-8 rune.
		n--
	}
	return s[:n] + "..."
}
