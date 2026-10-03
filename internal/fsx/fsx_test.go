package fsx_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vincent/agentd/internal/fsx"
)

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a", "b.txt")

	if err := fsx.WriteFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("got %q, %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestWriteFileAtomic_FailureLeavesNoTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Renaming a file over a non-empty directory fails: no temp file may leak.
	blocker := filepath.Join(dir, "blocker")
	if err := os.MkdirAll(filepath.Join(blocker, "child"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(blocker, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "blocker" {
		t.Fatalf("unexpected directory content: %v", entries)
	}
}

func TestAppendLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x", "log.jsonl")
	for _, l := range []string{"a", "b"} {
		if err := fsx.AppendLine(path, []byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a\nb\n" {
		t.Fatalf("got %q", got)
	}
}

func TestTryLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".lock")

	unlock, err := fsx.TryLock(path, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fsx.TryLock(path, time.Hour, time.Now); !errors.Is(err, fsx.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if err = unlock(); err != nil {
		t.Fatal(err)
	}
	unlock2, err := fsx.TryLock(path, time.Hour, time.Now)
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	_ = unlock2()
}

func TestTryLock_StaleIsTakenOver(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".lock")
	if _, err := fsx.TryLock(path, time.Hour, time.Now); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := fsx.TryLock(path, time.Hour, time.Now)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	_ = unlock()
}

func TestSafeJoin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rel     string
		wantErr bool
	}{
		{"a/b", false},
		{"./a", false},
		{"", true},
		{"/etc/passwd", true},
		{"../x", true},
		{"a/../../x", true},
		{"..", true},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()
			_, err := fsx.SafeJoin("/base", tt.rel)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, fsx.ErrUnsafePath) {
				t.Fatalf("wrong error type: %v", err)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"gigs": true, "a-b_c": true, "": false, ".hidden": false, "a/b": false, `a\b`: false} {
		if got := fsx.ValidName(name); got != want {
			t.Errorf("ValidName(%q)=%v want %v", name, got, want)
		}
	}
}
