// Command agentd runs scheduled LLM jobs with per-job MCP tools and serves an
// overview panel.
//
//	agentd serve    [--data DIR] [--addr HOST:PORT]
//	agentd run      [--data DIR] [--dry-run] JOB
//	agentd validate [--data DIR]
//	agentd version
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// version is overridden at build time: -ldflags "-X main.version=...".
var version = "dev"

const usage = `agentd - scheduled LLM jobs with MCP tools

Usage:
  agentd serve    [--data DIR] [--addr HOST:PORT]   run the scheduler and the panel
  agentd run      [--data DIR] [--dry-run] JOB      run one job now
  agentd validate [--data DIR]                      check every job file
  agentd version

The data directory defaults to $AGENTD_DATA or ./data.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}

	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		err = cmdServe(rest, stderr)
	case "run":
		err = cmdRun(rest, stdout, stderr)
	case "validate":
		err = cmdValidate(rest, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "agentd: %v\n", err)
		return 1
	}
}

func dataFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("AGENTD_DATA")
	if def == "" {
		def = "data"
	}
	return fs.String("data", def, "data directory (jobs/, state/, runs.jsonl, .env)")
}
