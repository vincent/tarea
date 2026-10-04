package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vincent/tarea/internal/fsx"
)

// ErrJobNotFound is returned by Dir.Get for an unknown job.
var ErrJobNotFound = errors.New("config: job not found")

// Dir is a directory of job files (<name>.yaml / <name>.yml).
type Dir struct {
	Path   string
	Lookup Lookup
}

// Load parses every job file. Invalid jobs are reported in the joined error
// while valid ones are still returned, so one typo never stops the others.
func (d Dir) Load() ([]Job, error) {
	files, err := d.files()
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(files))
	var errs []error
	for _, f := range files {
		j, jerr := d.parseFile(f)
		if jerr != nil {
			errs = append(errs, jerr)
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, errors.Join(errs...)
}

// Get loads a single job by name.
func (d Dir) Get(name string) (Job, error) {
	if !fsx.ValidName(name) {
		return Job{}, fmt.Errorf("%w: %q", ErrJobNotFound, name)
	}
	for _, ext := range []string{".yaml", ".yml"} {
		p := filepath.Join(d.Path, name+ext)
		if _, err := os.Stat(p); err == nil {
			return d.parseFile(p)
		}
	}
	return Job{}, fmt.Errorf("%w: %q", ErrJobNotFound, name)
}

// List satisfies read-only consumers (the API) and ignores invalid jobs.
func (d Dir) List() ([]Job, error) {
	jobs, err := d.Load()
	if len(jobs) > 0 {
		return jobs, nil
	}
	return jobs, err
}

// Fingerprint changes whenever a job file is added, removed or modified.
func (d Dir) Fingerprint() (string, error) {
	files, err := d.files()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range files {
		info, statErr := os.Stat(f)
		if statErr != nil {
			continue
		}
		_, _ = fmt.Fprintf(h, "%s|%d|%d\n", filepath.Base(f), info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (d Dir) files() ([]string, error) {
	entries, err := os.ReadDir(d.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read jobs dir: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(d.Path, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func (d Dir) parseFile(path string) (Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, fmt.Errorf("read %s: %w", path, err)
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return Parse(name, data, d.Lookup)
}
