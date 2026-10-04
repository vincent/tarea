// Package memory persists the two kinds of per-job state: free-form notes the
// model maintains (memory.md) and a code-managed dedupe set (seen.jsonl).
package memory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vincent/tarea/internal/fsx"
)

// ErrOverCap is returned when a write would push memory past its size cap.
var ErrOverCap = errors.New("memory over size cap")

// Store is a size-capped notes file. It is safe for concurrent use.
type Store struct {
	path     string
	maxBytes int
	mu       sync.Mutex
}

// New returns a Store backed by path, capped at maxKB kilobytes.
func New(path string, maxKB int) *Store {
	return &Store{path: path, maxBytes: maxKB * 1024}
}

// Read returns the file content; a missing file reads as empty.
func (s *Store) Read() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

func (s *Store) read() (string, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("memory: read: %w", err)
	}
	return string(b), nil
}

// Append adds a bullet note. It fails with ErrOverCap when the result would
// exceed the cap, which tells the model to compact via Replace.
func (s *Store) Append(note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.read()
	if err != nil {
		return err
	}
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	return s.write(cur + "- " + strings.TrimSpace(note) + "\n")
}

// Replace overwrites the file, subject to the same cap.
func (s *Store) Replace(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(content)
}

func (s *Store) write(content string) error {
	if len(content) > s.maxBytes {
		return fmt.Errorf("%w: %d > %d bytes; rewrite memory more compactly with memory_replace", ErrOverCap, len(content), s.maxBytes)
	}
	return fsx.WriteFileAtomic(s.path, []byte(content), 0o600)
}

// Seen is an append-only dedupe set persisted as JSON lines.
// It is safe for concurrent use.
type Seen struct {
	path string
	mu   sync.Mutex
	keys map[string]struct{}
	now  func() time.Time
}

type seenLine struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}

// OpenSeen loads the set from path (missing file = empty set). Corrupt lines
// are skipped so a torn final write never bricks a job.
func OpenSeen(path string, now func() time.Time) (*Seen, error) {
	s := &Seen{path: path, keys: make(map[string]struct{}), now: now}

	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: open seen: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var l seenLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Key != "" {
			s.keys[l.Key] = struct{}{}
		}
	}
	if err = sc.Err(); err != nil {
		return nil, fmt.Errorf("memory: scan seen: %w", err)
	}
	return s, nil
}

// Has reports whether key was recorded.
func (s *Seen) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.keys[key]
	return ok
}

// Add records keys not yet present, persisting each as one line.
func (s *Seen) Add(keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, ok := s.keys[k]; ok {
			continue
		}
		line, err := json.Marshal(seenLine{Key: k, At: s.now().UTC()})
		if err != nil {
			return fmt.Errorf("memory: encode seen: %w", err)
		}
		if err = fsx.AppendLine(s.path, line); err != nil {
			return err
		}
		s.keys[k] = struct{}{}
	}
	return nil
}

// Len returns the number of recorded keys.
func (s *Seen) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// StagedStore buffers memory edits in RAM so they only reach disk on Commit.
// It applies the same size cap as Store. Not safe for concurrent use by itself
// beyond its own mutex: one run owns one StagedStore.
type StagedStore struct {
	base    *Store
	mu      sync.Mutex
	pending string
	dirty   bool
}

// Stage returns a buffered view of the store.
func (s *Store) Stage() *StagedStore { return &StagedStore{base: s} }

// Read returns the staged content when edited, else the file content.
func (g *StagedStore) Read() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.read()
}

func (g *StagedStore) read() (string, error) {
	if g.dirty {
		return g.pending, nil
	}
	return g.base.Read()
}

// Append stages a bullet note, failing with ErrOverCap past the cap.
func (g *StagedStore) Append(note string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	cur, err := g.read()
	if err != nil {
		return err
	}
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	return g.stage(cur + "- " + strings.TrimSpace(note) + "\n")
}

// Replace stages new content, subject to the same cap.
func (g *StagedStore) Replace(content string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stage(content)
}

func (g *StagedStore) stage(content string) error {
	if len(content) > g.base.maxBytes {
		return fmt.Errorf("%w: %d > %d bytes; rewrite memory more compactly with memory_replace", ErrOverCap, len(content), g.base.maxBytes)
	}
	g.pending, g.dirty = content, true
	return nil
}

// Commit writes the staged content to disk, if any edit happened.
func (g *StagedStore) Commit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.dirty {
		return nil
	}
	// Keep one previous generation so a poisoned or wiped memory can be restored.
	prev, err := g.base.Read()
	if err != nil {
		return err
	}
	if prev != "" {
		if err := fsx.WriteFileAtomic(g.base.path+".bak", []byte(prev), 0o600); err != nil {
			return fmt.Errorf("memory: backup: %w", err)
		}
	}
	return g.base.Replace(g.pending)
}

// StagedSeen buffers new keys in RAM so they only reach disk on Commit.
type StagedSeen struct {
	base    *Seen
	mu      sync.Mutex
	pending map[string]struct{}
	order   []string
}

// Stage returns a buffered view of the set.
func (s *Seen) Stage() *StagedSeen {
	return &StagedSeen{base: s, pending: make(map[string]struct{})}
}

// Has reports whether key is recorded or staged.
func (g *StagedSeen) Has(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[strings.TrimSpace(key)]; ok {
		return true
	}
	return g.base.Has(key)
}

// Add stages keys; nothing is persisted until Commit.
func (g *StagedSeen) Add(keys ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, ok := g.pending[k]; ok || g.base.Has(k) {
			continue
		}
		g.pending[k] = struct{}{}
		g.order = append(g.order, k)
	}
	return nil
}

// Commit persists the staged keys.
func (g *StagedSeen) Commit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.base.Add(g.order...)
}
