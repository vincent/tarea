// Package sink defines where a job's final message goes. Concrete sinks live
// in subpackages (telegram) and are registered by type name.
package sink

import (
	"context"
	"fmt"
	"sort"
)

// Message is one deliverable.
type Message struct {
	Job  string
	Text string
}

// Sink delivers a message somewhere.
type Sink interface {
	Send(ctx context.Context, m Message) error
}

// Factory builds a Sink from the job's per-sink YAML options.
type Factory func(opts map[string]string) (Sink, error)

// Registry maps a sink type ("telegram") to its factory.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: make(map[string]Factory)} }

// Register adds or replaces a factory.
func (r *Registry) Register(typ string, f Factory) { r.factories[typ] = f }

// Build instantiates a sink by type.
func (r *Registry) Build(typ string, opts map[string]string) (Sink, error) {
	f, ok := r.factories[typ]
	if !ok {
		return nil, fmt.Errorf("sink: unknown type %q (known: %v)", typ, r.Types())
	}
	s, err := f(opts)
	if err != nil {
		return nil, fmt.Errorf("sink %s: %w", typ, err)
	}
	return s, nil
}

// Types lists registered types, sorted.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
