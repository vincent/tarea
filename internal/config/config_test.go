package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent/tarea/internal/config"
)

func lookup(m map[string]string) config.Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const validJob = `
schedule: "0 8 * * *"
model: anthropic/claude-sonnet-4.5
fallbacks: [openai/gpt-5-mini]
prompt: |
  Find gigs. Keep $HOME untouched.
mcp:
  - name: events
    url: https://example.com/mcp
    headers: {Authorization: "Bearer ${KEY}"}
    allow: ["*"]
sinks:
  - type: telegram
    chat_id: "${CHAT}"
`

func TestParse_ValidAppliesDefaultsAndExpansion(t *testing.T) {
	t.Parallel()
	j, err := config.Parse("gigs", []byte(validJob), lookup(map[string]string{"KEY": "k1", "CHAT": "42"}))
	if err != nil {
		t.Fatal(err)
	}
	if j.MaxSteps != config.DefaultMaxSteps || j.BudgetUSD != config.DefaultBudgetUSD || j.Memory.MaxKB != config.DefaultMemoryKB {
		t.Fatalf("defaults not applied: %+v", j)
	}
	if got := j.MCP[0].Headers["Authorization"]; got != "Bearer k1" {
		t.Fatalf("header = %q", got)
	}
	if got := j.Sinks[0].Options["chat_id"]; got != "42" {
		t.Fatalf("chat_id = %q", got)
	}
	if !strings.Contains(j.Prompt, "$HOME") {
		t.Fatal("prompt must not be expanded")
	}
	if !j.IsEnabled() {
		t.Fatal("jobs are enabled by default")
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, yaml, want string
	}{
		{"bad cron", "schedule: nope\nmodel: m\nprompt: p\nsinks: [{type: telegram}]", "schedule"},
		{"missing model", "schedule: '* * * * *'\nprompt: p\nsinks: [{type: telegram}]", "model is required"},
		{"missing prompt", "schedule: '* * * * *'\nmodel: m\nsinks: [{type: telegram}]", "prompt is required"},
		{"no sink", "schedule: '* * * * *'\nmodel: m\nprompt: p", "at least one sink"},
		{"unknown key", "schedule: '* * * * *'\nmodel: m\nprompt: p\nsinks: [{type: t}]\nbogus: 1", "bogus"},
		{"mcp both", "schedule: '* * * * *'\nmodel: m\nprompt: p\nsinks: [{type: t}]\nmcp: [{name: a, command: [x], url: http://x, allow: ['*']}]", "exactly one of command or url"},
		{"mcp no allow", "schedule: '* * * * *'\nmodel: m\nprompt: p\nsinks: [{type: t}]\nmcp: [{name: a, command: [x]}]", "allow is required"},
		{"mcp bad name", "schedule: '* * * * *'\nmodel: m\nprompt: p\nsinks: [{type: t}]\nmcp: [{name: a__b, command: [x], allow: ['*']}]", "invalid name"},
		{"undefined var", "schedule: '* * * * *'\nmodel: m\nprompt: p\nsinks: [{type: t, chat_id: '${NOPE}'}]", "NOPE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse("j", []byte(tt.yaml), lookup(nil))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParse_ReportsAllProblemsAtOnce(t *testing.T) {
	t.Parallel()
	_, err := config.Parse("j", []byte("schedule: nope"), lookup(nil))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"schedule", "model is required", "prompt is required", "sink"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestDir_LoadKeepsValidJobsAndFingerprintChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("good.yaml", validJob)
	write("bad.yaml", "schedule: nope")
	write("notes.txt", "ignored")

	d := config.Dir{Path: dir, Lookup: lookup(map[string]string{"KEY": "k", "CHAT": "1"})}
	fp1, err := d.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := d.Load()
	if len(jobs) != 1 || jobs[0].Name != "good" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("expected error mentioning bad job, got %v", err)
	}

	write("good.yaml", validJob+"\n# changed\n")
	fp2, _ := d.Fingerprint()
	if fp1 == fp2 {
		t.Fatal("fingerprint must change when a file changes")
	}

	if _, err = d.Get("missing"); err == nil {
		t.Fatal("expected not found")
	}
	if _, err = d.Get("../x"); err == nil {
		t.Fatal("expected rejection of traversal")
	}
}

func TestExpand(t *testing.T) {
	t.Parallel()
	got, err := config.Expand("a ${X} b ${Y}", lookup(map[string]string{"X": "1", "Y": "2"}))
	if err != nil || got != "a 1 b 2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err = config.Expand("${MISSING}", lookup(nil)); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadDotEnv(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), ".env")
	body := "# c\nA=1\nexport B=\"two words\"\nC='x'\n\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadDotEnv(p)
	if err != nil || got["A"] != "1" || got["B"] != "two words" || got["C"] != "x" {
		t.Fatalf("got %v, %v", got, err)
	}
	if m, err2 := config.LoadDotEnv(filepath.Join(t.TempDir(), "none")); err2 != nil || len(m) != 0 {
		t.Fatalf("missing file must yield empty map: %v %v", m, err2)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("nokeyvalue"), 0o600)
	if _, err = config.LoadDotEnv(bad); err == nil {
		t.Fatal("expected parse error")
	}
}
