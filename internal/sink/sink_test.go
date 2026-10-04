package sink_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent/tarea/internal/sink"
)

type nop struct{}

func (nop) Send(context.Context, sink.Message) error { return nil }

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := sink.NewRegistry()
	r.Register("b", func(map[string]string) (sink.Sink, error) { return nop{}, nil })
	r.Register("a", func(map[string]string) (sink.Sink, error) { return nil, errors.New("bad opts") })

	if got := r.Types(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("types = %v", got)
	}
	if s, err := r.Build("b", nil); err != nil || s == nil {
		t.Fatalf("build b: %v", err)
	}
	if _, err := r.Build("a", nil); err == nil || !strings.Contains(err.Error(), "bad opts") {
		t.Fatalf("factory error not propagated: %v", err)
	}
	if _, err := r.Build("zzz", nil); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("unknown type: %v", err)
	}
}
