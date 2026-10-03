package runlog_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent/agentd/internal/llm"
	"github.com/vincent/agentd/internal/runlog"
)

var t0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

func rec(job string, day int, cost float64) runlog.Record {
	start := t0.AddDate(0, 0, day)
	return runlog.Record{
		Summary: runlog.Summary{
			ID: runlog.NewID(start), Job: job, Trigger: "cron", StartedAt: start, EndedAt: start.Add(time.Second),
			Status: runlog.StatusOK, CostUSD: cost, Preview: "p",
		},
		Output:   "out " + job,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}
}

func TestStore_WriteGetListCost(t *testing.T) {
	t.Parallel()
	s := &runlog.Store{Root: t.TempDir()}
	for i, r := range []runlog.Record{rec("gigs", 0, 0.01), rec("news", 1, 0.02), rec("gigs", 2, 0.03)} {
		if err := s.Write(r); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	got, err := s.Get("gigs", runlog.NewID(t0))
	if err != nil || got.Output != "out gigs" || len(got.Messages) != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err = s.Get("gigs", "nope"); !errors.Is(err, runlog.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err = s.Get("../x", "y"); err == nil {
		t.Fatal("traversal must be rejected")
	}

	all, _ := s.List("", 0)
	if len(all) != 3 || all[0].StartedAt.Before(all[2].StartedAt) {
		t.Fatalf("expected newest first: %+v", all)
	}
	gigs, _ := s.List("gigs", 1)
	if len(gigs) != 1 || gigs[0].CostUSD != 0.03 {
		t.Fatalf("limit/filter: %+v", gigs)
	}

	cost, _ := s.CostSince("gigs", t0.AddDate(0, 0, 1))
	if cost != 0.03 {
		t.Fatalf("cost = %v", cost)
	}
	total, _ := s.CostSince("", t0)
	if fmt.Sprintf("%.2f", total) != "0.06" {
		t.Fatalf("total = %v", total)
	}
}

func TestStore_ListOnEmptyRoot(t *testing.T) {
	t.Parallel()
	s := &runlog.Store{Root: t.TempDir()}
	got, err := s.List("", 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestStore_Prune(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := &runlog.Store{Root: root}
	for day := range 5 {
		if err := s.Write(rec("gigs", day, 0.01)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Write(rec("news", 0, 0.01)); err != nil {
		t.Fatal(err)
	}

	if err := s.Prune(2); err != nil {
		t.Fatal(err)
	}

	gigs, _ := s.List("gigs", 0)
	if len(gigs) != 2 || !gigs[0].StartedAt.Equal(t0.AddDate(0, 0, 4)) {
		t.Fatalf("kept wrong runs: %+v", gigs)
	}
	if news, _ := s.List("news", 0); len(news) != 1 {
		t.Fatalf("other job affected: %+v", news)
	}
	files, _ := os.ReadDir(filepath.Join(root, "state", "gigs", "runs"))
	if len(files) != 2 {
		t.Fatalf("old run files not deleted: %d", len(files))
	}
}

func TestPreview(t *testing.T) {
	t.Parallel()
	if runlog.Preview("short") != "short" {
		t.Fatal("short strings are untouched")
	}
	got := runlog.Preview(strings.Repeat("é", 500))
	if r := []rune(got); len(r) != 203 || !strings.HasSuffix(got, "...") {
		t.Fatalf("len=%d", len(r))
	}
}
