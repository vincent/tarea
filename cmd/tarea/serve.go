package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vincent/tarea/internal/api"
	"github.com/vincent/tarea/internal/scheduler"
	"github.com/vincent/tarea/internal/webui"
)

const (
	shutdownGrace = 2 * time.Minute
	pruneEvery    = 24 * time.Hour
)

func cmdServe(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := dataFlag(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "panel listen address (non-loopback requires $TAREA_TOKEN)")
	var hosts stringList
	fs.Var(&hosts, "allowed-host", "extra Host name accepted without a token, e.g. behind a local proxy (repeatable)")
	reload := fs.Duration("reload", 5*time.Second, "how often to check the jobs directory for changes")
	keep := fs.Int("keep-runs", 500, "run history kept per job (older runs are pruned daily)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	a, err := newApp(*data, stderr)
	if err != nil {
		return err
	}
	token, _ := a.lookup("TAREA_TOKEN")
	if token == "" && !isLoopbackAddr(*addr) {
		return fmt.Errorf("refusing to listen on %q without authentication: set TAREA_TOKEN or bind to loopback", *addr)
	}
	r, err := a.runner(nil)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched := scheduler.New(r, a.log)
	sched.Start()
	go sched.Watch(ctx, *reload, a.jobs)
	go prune(ctx, a, *keep)

	handler := api.New(api.Deps{
		Jobs: a.jobs, Runs: a.runs, Control: sched, Memory: a.readMemory,
		UI: webui.FS(), Version: version, Started: time.Now(), Log: a.log,
	})
	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.Guard(handler, api.GuardOpts{Token: token, AllowedHosts: hosts}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	a.log.Info("tarea listening", "addr", *addr, "data", a.dataDir, "version", version, "auth", token != "")

	select {
	case err = <-errc:
		stop()
	case <-ctx.Done():
		stop() // restore default signal handling: a second Ctrl-C kills the process.
		a.log.Info("shutting down: waiting for in-flight runs", "grace", shutdownGrace)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	httpErr := srv.Shutdown(shutdownCtx)
	schedErr := sched.Stop(shutdownCtx)

	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, httpErr, schedErr)
}

func prune(ctx context.Context, a *app, keep int) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		if err := a.runs.Prune(keep); err != nil {
			a.log.Warn("prune run history", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// isLoopbackAddr reports whether a listen address only accepts local connections.
// An empty or wildcard host (":8080", "0.0.0.0") listens on every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
