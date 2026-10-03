package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/vincent/agentd/internal/runner"
)

func cmdRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := dataFlag(fs)
	dry := fs.Bool("dry-run", false, "print the result instead of sending it to the sinks")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() < 1 {
		return errors.New("usage: agentd run [--data DIR] [--dry-run] JOB")
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil { // allow flags after the job name.
		return fmt.Errorf("parse flags: %w", err)
	}

	a, err := newApp(*data, stderr)
	if err != nil {
		return err
	}
	var out io.Writer
	if *dry {
		out = stdout
	}
	r, err := a.runner(out)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sum, err := r.Run(ctx, name, runner.TriggerCLI)
	if sum.ID != "" {
		_, _ = fmt.Fprintf(stderr, "run %s: status=%s steps=%d tools=%d tokens=%d/%d cost=$%.4f\n",
			sum.ID, sum.Status, sum.Steps, sum.ToolCalls, sum.PromptTokens, sum.CompletionTokens, sum.CostUSD)
	}
	if err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}
