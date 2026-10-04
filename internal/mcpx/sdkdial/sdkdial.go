// Package sdkdial adapts the official MCP Go SDK
// (github.com/modelcontextprotocol/go-sdk) to mcpx.Session.
//
// Keeping the SDK behind this one file means mcpx stays testable offline and
// an SDK API change only ever touches this adapter.
package sdkdial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/mcpx"
)

// Version is reported to servers; set via -ldflags.
var Version = "dev"

// inheritedEnv is the only parent environment stdio servers receive, so job
// secrets never leak to third-party MCP processes. Add more via the job's env.
var inheritedEnv = []string{"PATH", "HOME", "USER", "LANG", "TMPDIR", "SYSTEMROOT"}

// Dial connects to an MCP server over stdio (command) or streamable HTTP (url).
func Dial(ctx context.Context, srv config.MCPServer) (mcpx.Session, error) {
	transport, err := newTransport(ctx, srv)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "tarea", Version: Version}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &session{cs: cs}, nil
}

func newTransport(ctx context.Context, srv config.MCPServer) (mcp.Transport, error) {
	switch {
	case len(srv.Command) > 0:
		cmd := exec.CommandContext(ctx, srv.Command[0], srv.Command[1:]...) //nolint:gosec // command comes from operator-owned job config; env is scrubbed by buildEnv
		cmd.Env = buildEnv(srv.Env)
		return &mcp.CommandTransport{Command: cmd}, nil
	case srv.URL != "":
		return &mcp.StreamableClientTransport{
			Endpoint:   srv.URL,
			HTTPClient: &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: srv.Headers}},
		}, nil
	default:
		return nil, errors.New("server needs a command or a url")
	}
}

func buildEnv(extra map[string]string) []string {
	var env []string
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req) //nolint:wrapcheck // transparent RoundTripper decorator; http.Client wraps the error
}

type session struct{ cs *mcp.ClientSession }

func (s *session) ListTools(ctx context.Context) ([]mcpx.Tool, error) {
	var (
		out    []mcpx.Tool
		cursor string
	)
	for {
		res, err := s.cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		for _, t := range res.Tools {
			schema, merr := json.Marshal(t.InputSchema)
			if merr != nil {
				return nil, fmt.Errorf("encode schema of %s: %w", t.Name, merr)
			}
			out = append(out, mcpx.Tool{Name: t.Name, Description: t.Description, Schema: schema})
		}
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

func (s *session) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var arguments map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", fmt.Errorf("arguments must be a JSON object: %w", err)
		}
	}
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return "", fmt.Errorf("call tool: %w", err)
	}

	text := flatten(res)
	if res.IsError {
		return "", fmt.Errorf("tool reported an error: %s", text)
	}
	return text, nil
}

func (s *session) Close() error {
	if err := s.cs.Close(); err != nil {
		return fmt.Errorf("close session: %w", err)
	}
	return nil
}

// flatten renders a tool result as text: text parts verbatim, anything else as
// a short placeholder (binary payloads are useless to the model).
func flatten(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
			continue
		}
		parts = append(parts, fmt.Sprintf("[%T content omitted]", c))
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			return string(raw)
		}
	}
	return strings.Join(parts, "\n")
}
