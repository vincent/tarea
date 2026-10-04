// Package fsx provides the small filesystem primitives tarea relies on:
// atomic writes, append-only lines, advisory lock files and traversal-safe
// path joins. It is stdlib-only and safe to cross-compile.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrLocked is returned by TryLock when another holder owns the lock.
	ErrLocked = errors.New("fsx: lock held")
	// ErrUnsafePath is returned by SafeJoin for absolute or escaping paths.
	ErrUnsafePath = errors.New("fsx: unsafe path")
)

const (
	dirPerm  fs.FileMode = 0o750
	filePerm fs.FileMode = 0o600
)

// WriteFileAtomic writes data to path through a temp file and a rename, so
// readers only ever see the old or the new content, never a partial file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("fsx: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("fsx: create temp: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("fsx: write temp: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("fsx: sync temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("fsx: close temp: %w", err)
	}
	if err = os.Chmod(tmp.Name(), perm); err != nil {
		return fmt.Errorf("fsx: chmod temp: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("fsx: rename: %w", err)
	}
	return nil
}

// AppendLine appends line plus a newline to path using a single write call,
// creating the file and its parent directory when needed.
func AppendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("fsx: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("fsx: open %s: %w", path, err)
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err = f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsx: append %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("fsx: close %s: %w", path, err)
	}
	return nil
}

// TryLock creates an exclusive lock file at path. It never blocks: if the lock
// is held it returns ErrLocked. While held, the lock's mtime is refreshed every
// staleAfter/4 (heartbeat), so a lock whose mtime is older than staleAfter was
// left by a crashed process and is taken over.
// The returned function stops the heartbeat and releases the lock; it is safe
// to call more than once.
func TryLock(path string, staleAfter time.Duration, now func() time.Time) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("fsx: mkdir: %w", err)
	}

	for range 3 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return startHeartbeat(path, staleAfter, now), nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("fsx: lock %s: %w", path, err)
		}

		info, statErr := os.Stat(path)
		if statErr != nil {
			continue // vanished between open and stat: retry.
		}
		if now().Sub(info.ModTime()) <= staleAfter {
			return nil, ErrLocked
		}
		if err = takeOverStale(path, staleAfter, now); err != nil {
			return nil, err
		}
	}
	return nil, ErrLocked
}

// takeOverStale removes a lock judged stale. The lock is renamed away first:
// rename is atomic, so of several concurrent takers only one gets the file. If
// the file turns out fresh (a taker re-created it between our stat and rename)
// it is linked back and ErrLocked is returned. A tiny window remains where the
// link-back loses to yet another taker.
func takeOverStale(path string, staleAfter time.Duration, now func() time.Time) error {
	grave := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, grave); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // another taker won; the caller retries.
		}
		return fmt.Errorf("fsx: remove stale lock: %w", err)
	}
	if info, err := os.Stat(grave); err == nil && now().Sub(info.ModTime()) <= staleAfter {
		_ = os.Link(grave, path)
		_ = os.Remove(grave)
		return ErrLocked
	}
	_ = os.Remove(grave)
	return nil
}

// startHeartbeat keeps the lock's mtime fresh until the returned release
// function is called. Release stops the heartbeat before removing the file so
// a late tick can never touch a successor's lock.
func startHeartbeat(path string, staleAfter time.Duration, now func() time.Time) func() error {
	interval := max(staleAfter/4, time.Millisecond)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				ts := now()
				if err := os.Chtimes(path, ts, ts); errors.Is(err, fs.ErrNotExist) {
					return // lock removed under us: nothing left to refresh.
				}
			}
		}
	}()

	var once sync.Once
	return func() (err error) {
		once.Do(func() {
			close(done)
			<-stopped
			err = os.Remove(path)
		})
		return err
	}
}

// SafeJoin joins rel under base and rejects absolute paths and any path that
// escapes base after cleaning.
func SafeJoin(base, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, rel)
	}
	return filepath.Join(base, clean), nil
}

// ValidName reports whether s is usable as a single path element (a job name
// or run id): non-empty, no separators, no dot-prefix.
func ValidName(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") {
		return false
	}
	return !strings.ContainsAny(s, `/\`+"\x00")
}
