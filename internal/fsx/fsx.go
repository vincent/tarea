// Package fsx provides the small filesystem primitives agentd relies on:
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
// is held it returns ErrLocked. A lock older than staleAfter (by file mtime) is
// considered abandoned by a crashed process and is taken over.
// The returned function releases the lock and is safe to call once.
func TryLock(path string, staleAfter time.Duration, now func() time.Time) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("fsx: mkdir: %w", err)
	}

	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return func() error { return os.Remove(path) }, nil
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
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("fsx: remove stale lock: %w", rmErr)
		}
	}
	return nil, ErrLocked
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
