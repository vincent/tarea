package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
)

func cmdValidate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := dataFlag(fs)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	a, err := newApp(*data, io.Discard)
	if err != nil {
		return err
	}

	jobs, loadErr := a.jobs.Load()
	known := a.sinks.Types()
	var errs []error
	for _, j := range jobs {
		for _, s := range j.Sinks {
			if !slices.Contains(known, s.Type) {
				errs = append(errs, fmt.Errorf("job %s: unknown sink type %q (known: %v)", j.Name, s.Type, known))
			}
		}
		_, _ = fmt.Fprintf(stdout, "ok  %-20s %-14s %s\n", j.Name, j.Schedule, j.Model)
	}
	if err = errors.Join(append(errs, loadErr)...); err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	if len(jobs) == 0 {
		return errors.New("no jobs found in " + a.jobs.Path)
	}
	return nil
}
