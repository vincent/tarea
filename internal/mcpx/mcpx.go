// Package mcpx manages a job's MCP connections: it dials the configured
// servers, applies the per-server allow-list, namespaces tool names and routes
// calls. The wire protocol lives behind the Session interface (see sdkdial for
// the official-SDK implementation), which keeps this logic testable offline.
package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/vincent/agentd/internal/config"
	"github.com/vincent/agentd/internal/llm"
)

// Separator joins server and tool names: "events__search".
const Separator = "__"

// DefaultCallTimeout bounds a single tool call.
const DefaultCallTimeout = 60 * time.Second

const maxToolNameLen = 64

// Tool is a tool offered by an MCP server.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Session is one live connection to an MCP server.
type Session interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (string, error)
	Close() error
}

// Dialer connects to the described server.
type Dialer func(ctx context.Context, srv config.MCPServer) (Session, error)

type route struct {
	session Session
	tool    string
}

// Host is the set of open sessions for one run. It implements agent.ToolHost.
type Host struct {
	CallTimeout time.Duration
	defs        []llm.ToolDef
	routes      map[string]route
	sessions    []Session
}

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// Open dials every server and builds the allowed tool set. On any failure the
// sessions opened so far are closed, so callers never leak subprocesses.
func Open(ctx context.Context, servers []config.MCPServer, dial Dialer) (*Host, error) {
	h := &Host{CallTimeout: DefaultCallTimeout, routes: make(map[string]route)}

	for _, srv := range servers {
		if err := h.addServer(ctx, srv, dial); err != nil {
			return nil, errors.Join(fmt.Errorf("mcp %s: %w", srv.Name, err), h.Close())
		}
	}
	sort.Slice(h.defs, func(i, j int) bool { return h.defs[i].Name < h.defs[j].Name })
	return h, nil
}

func (h *Host) addServer(ctx context.Context, srv config.MCPServer, dial Dialer) error {
	sess, err := dial(ctx, srv)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	h.sessions = append(h.sessions, sess)

	tools, err := sess.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	allowed, err := filter(tools, srv.Allow)
	if err != nil {
		return err
	}

	for _, t := range allowed {
		name := safeName(srv.Name + Separator + t.Name)
		if _, dup := h.routes[name]; dup {
			return fmt.Errorf("tool name collision on %q", name)
		}
		h.routes[name] = route{session: sess, tool: t.Name}
		h.defs = append(h.defs, llm.ToolDef{Name: name, Description: t.Description, Parameters: t.Schema})
	}
	return nil
}

// filter applies the allow-list. "*" allows everything; naming a tool the
// server does not offer is an error so a typo never silently disables a tool.
func filter(tools []Tool, allow []string) ([]Tool, error) {
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	for _, a := range allow {
		if a == "*" {
			return tools, nil
		}
	}
	out := make([]Tool, 0, len(allow))
	for _, a := range allow {
		t, ok := byName[a]
		if !ok {
			return nil, fmt.Errorf("allowed tool %q not offered by server", a)
		}
		out = append(out, t)
	}
	return out, nil
}

func safeName(s string) string {
	s = invalidNameChars.ReplaceAllString(s, "_")
	if len(s) > maxToolNameLen {
		s = s[:maxToolNameLen]
	}
	return s
}

// Tools returns the namespaced, allow-listed tool definitions.
func (h *Host) Tools() []llm.ToolDef { return h.defs }

// Call routes a namespaced tool call to its server with a per-call timeout.
func (h *Host) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	r, ok := h.routes[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if h.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.CallTimeout)
		defer cancel()
	}
	out, err := r.session.CallTool(ctx, r.tool, args)
	if err != nil {
		return "", fmt.Errorf("call %s: %w", name, err)
	}
	return out, nil
}

// Close closes every session and joins their errors.
func (h *Host) Close() error {
	var errs []error
	for _, s := range h.sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	h.sessions = nil
	return errors.Join(errs...)
}
