package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/llm"
	"github.com/vincent/tarea/internal/mcpx/sdkdial"
	"github.com/vincent/tarea/internal/memory"
	"github.com/vincent/tarea/internal/runlog"
	"github.com/vincent/tarea/internal/runner"
	"github.com/vincent/tarea/internal/sink"
	"github.com/vincent/tarea/internal/sink/telegram"
)

// app holds the pieces shared by every command.
type app struct {
	dataDir string
	lookup  config.Lookup
	jobs    config.Dir
	runs    *runlog.Store
	sinks   *sink.Registry
	log     *slog.Logger
}

func newApp(dataDir string, stderr io.Writer) (*app, error) {
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("data directory %q not found (use --data or $TAREA_DATA)", dataDir)
	}
	dotenv, err := config.LoadDotEnv(filepath.Join(dataDir, ".env"))
	if err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	lookup := config.EnvLookup(dotenv)

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("TAREA_LOG"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	token, _ := lookup("TELEGRAM_BOT_TOKEN")
	reg := sink.NewRegistry()
	reg.Register("telegram", telegram.Factory(token, nil))

	sdkdial.Version = version
	return &app{
		dataDir: dataDir,
		lookup:  lookup,
		jobs:    config.Dir{Path: filepath.Join(dataDir, "jobs"), Lookup: lookup},
		runs:    &runlog.Store{Root: dataDir},
		sinks:   reg,
		log:     log,
	}, nil
}

// runner builds a Runner; dry (optional) receives output instead of the sinks.
func (a *app) runner(dry io.Writer) (*runner.Runner, error) {
	key, ok := a.lookup("OPENROUTER_API_KEY")
	if !ok || key == "" {
		return nil, errors.New("OPENROUTER_API_KEY is not set (environment or <data>/.env)")
	}
	provider := llm.NewOpenRouter(key)
	provider.HTTP = &http.Client{Timeout: 3 * time.Minute}

	return &runner.Runner{
		DataDir:  a.dataDir,
		Jobs:     a.jobs,
		Provider: provider,
		Dial:     sdkdial.Dial,
		Sinks:    a.sinks,
		Runs:     a.runs,
		DryRun:   dry,
		Log:      a.log,
		Now:      time.Now,
	}, nil
}

// readMemory loads a job's memory file for the panel.
func (a *app) readMemory(j config.Job) (string, error) {
	p := filepath.Join(a.dataDir, "state", j.Name, j.Memory.File)
	text, err := memory.New(p, j.Memory.MaxKB).Read()
	if err != nil {
		return "", fmt.Errorf("read memory: %w", err)
	}
	return text, nil
}
