package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_VersionUsageAndUnknown(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer

	if code := run([]string{"version"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: code=%d out=%q", code, out.String())
	}
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("no args: code=%d", code)
	}
	if code := run([]string{"bogus"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("unknown: code=%d err=%q", code, errOut.String())
	}
}

func writeJob(t *testing.T, dir, name, body string) {
	t.Helper()
	jobs := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, name+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeJob(t, dir, "gigs", "schedule: '0 8 * * *'\nmodel: m\nprompt: p\nsinks: [{type: telegram, chat_id: '1'}]")
		var out, errOut bytes.Buffer
		if code := run([]string{"validate", "--data", dir}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "gigs") {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
		}
	})

	t.Run("invalid and unknown sink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeJob(t, dir, "bad", "schedule: nope")
		writeJob(t, dir, "weird", "schedule: '0 8 * * *'\nmodel: m\nprompt: p\nsinks: [{type: carrier-pigeon}]")
		var out, errOut bytes.Buffer
		code := run([]string{"validate", "--data", dir}, &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "carrier-pigeon") || !strings.Contains(errOut.String(), "schedule") {
			t.Fatalf("code=%d err=%q", code, errOut.String())
		}
	})

	t.Run("missing data dir", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		if code := run([]string{"validate", "--data", filepath.Join(t.TempDir(), "nope")}, &out, &errOut); code != 1 {
			t.Fatalf("code=%d", code)
		}
	})
}

func TestRunCommand_RequiresKeyAndJobName(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	dir := t.TempDir()
	var out, errOut bytes.Buffer

	if code := run([]string{"run", "--data", dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("no job name: code=%d err=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"run", "gigs", "--data", dir, "--dry-run"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("no key: code=%d err=%q", code, errOut.String())
	}
}

func TestServe_RefusesOpenListenerWithoutToken(t *testing.T) {
	t.Setenv("TAREA_TOKEN", "")
	dir := t.TempDir()
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.0.2.1:0"} {
		var errOut bytes.Buffer
		if code := run([]string{"serve", "--data", dir, "--addr", addr}, &bytes.Buffer{}, &errOut); code == 0 || !strings.Contains(errOut.String(), "TAREA_TOKEN") {
			t.Errorf("addr %s: code=%d err=%q", addr, code, errOut.String())
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:8080": true, "[::1]:8080": true,
		"0.0.0.0:8080": false, ":8080": false, "[::]:8080": false, "10.0.0.5:8080": false, "bad": false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
