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
