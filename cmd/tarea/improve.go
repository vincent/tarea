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
	"time"

	"github.com/vincent/tarea/internal/improve"
)

func cmdImprove(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("improve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := dataFlag(fs)
	model := fs.String("model", "", "analysis model (default: $TAREA_IMPROVE_MODEL, then the job's model)")
	window := fs.Int("window", improve.DefaultWindow, "number of recent runs to analyse")
	apply := fs.String("apply", "", "accept proposal ID: replaces the job file (old copy kept in jobs/.history)")
	reject := fs.String("reject", "", "reject proposal ID")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() < 1 {
		return errors.New("usage: tarea improve [--data DIR] [--model M] [--window N] [--apply ID | --reject ID] JOB")
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil { // allow flags after the job name.
		return fmt.Errorf("parse flags: %w", err)
	}

	a, err := newApp(*data, stderr)
	if err != nil {
		return err
	}
	im, err := a.improver()
	if err != nil {
		return err
	}
	if *model != "" {
		im.Model = *model
	}
	im.Window = *window

	switch {
	case *apply != "":
		if _, err = im.Accept(name, *apply); err != nil {
			return fmt.Errorf("apply %s: %w", *apply, err)
		}
		_, _ = fmt.Fprintf(stdout, "applied %s to jobs/%s (previous version in jobs/.history)\n", *apply, name)
		return nil
	case *reject != "":
		if _, err = im.Reject(name, *reject); err != nil {
			return fmt.Errorf("reject %s: %w", *reject, err)
		}
		_, _ = fmt.Fprintf(stdout, "rejected %s\n", *reject)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	p, err := im.Propose(ctx, name)
	if errors.Is(err, improve.ErrNoChanges) {
		_, _ = fmt.Fprintf(stdout, "nothing to improve: %s\n", p.Rationale)
		return nil
	}
	if err != nil {
		return err
	}
	printProposal(stdout, p)
	_, _ = fmt.Fprintf(stdout, "\naccept: tarea improve --apply %s %s\nreject: tarea improve --reject %s %s\n", p.ID, name, p.ID, name)
	return nil
}

func printProposal(w io.Writer, p improve.Proposal) {
	st := p.Stats
	_, _ = fmt.Fprintf(w, "proposal %s for %s (analysed by %s, $%.4f)\n", p.ID, p.Job, p.AnalysedBy, p.CostUSD)
	_, _ = fmt.Fprintf(w, "runs=%d ok=%d partial=%d error=%d truncated=%d avg cost=$%.4f avg tokens=%.0f/%.0f\n\n",
		st.Runs, st.OK, st.Partial, st.Errors, st.Truncated, st.AvgCostUSD, st.AvgPrompt, st.AvgCompletion)
	_, _ = fmt.Fprintf(w, "%s\n\n", p.Rationale)
	for _, c := range p.Changes {
		if c.Field == improve.FieldPrompt {
			_, _ = fmt.Fprintf(w, "~ prompt:\n--- old\n%v--- new\n%v\n", c.From, c.To)
			continue
		}
		_, _ = fmt.Fprintf(w, "~ %s: %v -> %v\n", c.Field, c.From, c.To)
	}
}
