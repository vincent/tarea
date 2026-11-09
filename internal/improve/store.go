package improve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/fsx"
)

// ErrProposalNotFound is returned for an unknown proposal id.
var ErrProposalNotFound = errors.New("improve: proposal not found")

func (im *Improver) dir(job string) string {
	return filepath.Join(im.DataDir, "state", job, "proposals")
}

func (im *Improver) save(p Proposal) error {
	if !fsx.ValidName(p.Job) || !fsx.ValidName(p.ID) {
		return fmt.Errorf("improve: invalid job %q or id %q", p.Job, p.ID)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("improve: encode proposal: %w", err)
	}
	return fsx.WriteFileAtomic(filepath.Join(im.dir(p.Job), p.ID+".json"), raw, 0o600)
}

// Get returns one proposal.
func (im *Improver) Get(job, id string) (Proposal, error) {
	if !fsx.ValidName(job) || !fsx.ValidName(id) {
		return Proposal{}, fmt.Errorf("%w: %s/%s", ErrProposalNotFound, job, id)
	}
	raw, err := os.ReadFile(filepath.Join(im.dir(job), id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Proposal{}, fmt.Errorf("%w: %s/%s", ErrProposalNotFound, job, id)
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("improve: read proposal: %w", err)
	}
	var p Proposal
	if err = json.Unmarshal(raw, &p); err != nil {
		return Proposal{}, fmt.Errorf("improve: decode proposal: %w", err)
	}
	return p, nil
}

// List returns a job's proposals, newest first.
func (im *Improver) List(job string) ([]Proposal, error) {
	if !fsx.ValidName(job) {
		return nil, fmt.Errorf("%w: %q", config.ErrJobNotFound, job)
	}
	entries, err := os.ReadDir(im.dir(job))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("improve: list proposals: %w", err)
	}
	var out []Proposal
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if p, gerr := im.Get(job, id); gerr == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID > out[k].ID })
	return out, nil
}

// Accept applies a pending proposal: the current job file is kept under
// <jobs>/.history/, then replaced atomically. The scheduler picks it up on its
// next reload.
func (im *Improver) Accept(job, id string) (Proposal, error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	p, err := im.Get(job, id)
	if err != nil {
		return Proposal{}, err
	}
	if p.Status != StatusPending {
		return Proposal{}, ErrNotPending
	}
	path, raw, err := im.readJob(job)
	if err != nil {
		return Proposal{}, err
	}
	if sha(raw) != p.BaseSHA {
		return Proposal{}, ErrStale
	}
	if _, err = config.Parse(job, []byte(p.NewYAML), im.Jobs.Lookup); err != nil {
		return Proposal{}, fmt.Errorf("improve: proposed job is invalid: %w", err)
	}

	hist := filepath.Join(im.Jobs.Path, ".history", fmt.Sprintf("%s.%s%s", job, p.ID, filepath.Ext(path)))
	if err = fsx.WriteFileAtomic(hist, raw, 0o600); err != nil {
		return Proposal{}, fmt.Errorf("improve: save history: %w", err)
	}
	mode := fs.FileMode(0o600)
	if info, serr := os.Stat(path); serr == nil {
		mode = info.Mode().Perm()
	}
	if err = fsx.WriteFileAtomic(path, []byte(p.NewYAML), mode); err != nil {
		return Proposal{}, fmt.Errorf("improve: write job: %w", err)
	}
	p.Status = StatusAccepted
	return p, im.save(p)
}

// Reject marks a pending proposal as rejected.
func (im *Improver) Reject(job, id string) (Proposal, error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	p, err := im.Get(job, id)
	if err != nil {
		return Proposal{}, err
	}
	if p.Status != StatusPending {
		return Proposal{}, ErrNotPending
	}
	p.Status = StatusRejected
	return p, im.save(p)
}
