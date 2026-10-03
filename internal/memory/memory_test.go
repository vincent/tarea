package memory_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vincent/agentd/internal/memory"
)

func TestStore_ReadAppendReplaceAndCap(t *testing.T) {
	t.Parallel()
	s := memory.New(filepath.Join(t.TempDir(), "memory.md"), 1) // 1 KiB cap.

	if got, err := s.Read(); err != nil || got != "" {
		t.Fatalf("missing file must read empty: %q %v", got, err)
	}
	if err := s.Append("first"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("  second  "); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Read(); got != "- first\n- second\n" {
		t.Fatalf("got %q", got)
	}

	big := make([]byte, 2048)
	if err := s.Replace(string(big)); !errors.Is(err, memory.ErrOverCap) {
		t.Fatalf("want ErrOverCap, got %v", err)
	}
	if got, _ := s.Read(); got != "- first\n- second\n" {
		t.Fatalf("failed write must not alter file: %q", got)
	}
	if err := s.Replace("compact\n"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Read(); got != "compact\n" {
		t.Fatalf("got %q", got)
	}
}

func TestStore_ConcurrentAppends(t *testing.T) {
	t.Parallel()
	s := memory.New(filepath.Join(t.TempDir(), "m.md"), 64)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Append(fmt.Sprintf("note %d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := s.Read()
	if n := countLines(got); n != 20 {
		t.Fatalf("lost appends: %d lines\n%s", n, got)
	}
}

func countLines(s string) int {
	n := 0
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}

func TestSeen_PersistsAcrossOpenAndSkipsCorruptLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "seen.jsonl")
	now := func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }

	s, err := memory.OpenSeen(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Add("a", "b", "a", " ", "c"); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 || !s.Has("a") || s.Has("z") {
		t.Fatalf("len=%d", s.Len())
	}

	// Simulate a torn write at the end of the file.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"key":"torn`)
	_ = f.Close()

	s2, err := memory.OpenSeen(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 3 || !s2.Has("b") {
		t.Fatalf("reload len=%d", s2.Len())
	}
}

func TestSeen_ConcurrentAdds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "seen.jsonl")
	s, _ := memory.OpenSeen(path, time.Now)
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Add(fmt.Sprintf("k%d", i%10))
		}()
	}
	wg.Wait()
	if s.Len() != 10 {
		t.Fatalf("len = %d", s.Len())
	}
	reloaded, _ := memory.OpenSeen(path, time.Now)
	if reloaded.Len() != 10 {
		t.Fatalf("persisted len = %d (duplicate lines written?)", reloaded.Len())
	}
}
