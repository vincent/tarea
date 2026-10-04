// Package httptool is an in-process MCP session offering the "request", "text"
// and "jq" tools, which perform HTTP calls restricted to a per-job host allow-list. Configured
// headers (typically secrets) are injected here and never shown to the model.
package httptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/itchyny/gojq"
	"github.com/k3a/html2text"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/mcpx"
)

// maxBodyBytes caps how much of a response body is returned to the model.
const maxBodyBytes = 64 << 10

// maxPageBytes caps the raw HTML fetched by the text tool before conversion.
const maxPageBytes = 1 << 20

// nonContent matches elements whose body is never page text. html2text
// returns nothing at all for pages with large inline scripts, so they are
// removed before conversion.
var nonContent = regexp.MustCompile(`(?is)<!--.*?-->|<(script|style|noscript|template)\b.*?</(script|style|noscript|template)\s*>`)

// maxJSONBytes caps the JSON document fetched by the jq tool.
const maxJSONBytes = 10 << 20

// maxJQOutputBytes caps the rendered jq output kept in memory.
const maxJQOutputBytes = 1 << 20

const (
	toolRequest = "request"
	toolText    = "text"
	toolJQ      = "jq"
)

const jqSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Absolute http(s) URL returning JSON"},
    "query": {"type": "string", "description": "jq filter, e.g. '.[] | .id'"}
  },
  "required": ["url", "query"]
}`

const textSchema = `{
  "type": "object",
  "properties": {"url": {"type": "string", "description": "Absolute http(s) URL of the page"}},
  "required": ["url"]
}`

const requestSchema = `{
  "type": "object",
  "properties": {
    "method": {"type": "string", "enum": ["GET", "POST"], "description": "HTTP method (default GET)"},
    "url": {"type": "string", "description": "Absolute http(s) URL"},
    "body": {"type": "string", "description": "Request body for POST (sent as JSON)"}
  },
  "required": ["url"]
}`

// Session implements mcpx.Session.
type Session struct {
	hosts   map[string]bool
	headers map[string]string
	client  *http.Client
}

// New builds a session for srv. A nil client uses a copy of the default one.
func New(srv config.MCPServer, client *http.Client) *Session {
	s := &Session{hosts: make(map[string]bool, len(srv.Hosts)), headers: srv.Headers}
	for _, h := range srv.Hosts {
		s.hosts[strings.ToLower(h)] = true
	}
	c := http.Client{}
	if client != nil {
		c = *client
	}
	// Never follow a redirect off the allow-list.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return s.checkHost(req.URL)
	}
	s.client = &c
	return s
}

// Wrap returns a Dialer serving the http builtin in-process and delegating
// every other server to dial.
func Wrap(dial mcpx.Dialer, client *http.Client) mcpx.Dialer {
	return func(ctx context.Context, srv config.MCPServer) (mcpx.Session, error) {
		if srv.Builtin == config.BuiltinHTTP {
			return New(srv, client), nil
		}
		return dial(ctx, srv)
	}
}

// ListTools returns the request, text and jq tools.
func (s *Session) ListTools(context.Context) ([]mcpx.Tool, error) {
	return []mcpx.Tool{
		{
			Name:        toolRequest,
			Description: "Make an HTTP request to an allowed host. Returns the status line then the response body.",
			Schema:      json.RawMessage(requestSchema),
		},
		{
			Name:        toolText,
			Description: "GET a web page from an allowed host and return it as plain text (HTML converted). Returns the status line then the text.",
			Schema:      json.RawMessage(textSchema),
		},
		{
			Name:        toolJQ,
			Description: "GET a JSON document from an allowed host (up to 10 MB) and apply a jq filter to it. Returns one compact JSON value per line. Prefer this over request for large JSON.",
			Schema:      json.RawMessage(jqSchema),
		},
	}, nil
}

// CallTool dispatches to the named tool.
func (s *Session) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var in struct {
		Method string `json:"method"`
		URL    string `json:"url"`
		Body   string `json:"body"`
		Query  string `json:"query"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	switch name {
	case toolRequest:
		return s.request(ctx, in.Method, in.URL, in.Body)
	case toolText:
		return s.text(ctx, in.URL)
	case toolJQ:
		return s.jq(ctx, in.URL, in.Query)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func (s *Session) request(ctx context.Context, method, rawURL, body string) (string, error) {
	method = strings.ToUpper(method)
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodPost:
	default:
		return "", fmt.Errorf("method %q not allowed (GET, POST)", method)
	}
	res, err := s.do(ctx, method, rawURL, body, maxBodyBytes)
	if err != nil {
		return "", err
	}
	return format(res.status, res.body, maxBodyBytes), nil
}

func (s *Session) text(ctx context.Context, rawURL string) (string, error) {
	res, err := s.do(ctx, http.MethodGet, rawURL, "", maxPageBytes)
	if err != nil {
		return "", err
	}
	out := string(res.body)
	if strings.Contains(strings.ToLower(res.contentType), "html") {
		out = strings.ReplaceAll(html2text.HTML2Text(nonContent.ReplaceAllString(out, "")), "\r\n", "\n")
		if strings.TrimSpace(out) == "" && len(res.body) > 0 {
			return "", errors.New("page has no text content (rendered client-side?); try the request tool")
		}
	}
	return format(res.status, []byte(out), maxBodyBytes), nil
}

func (s *Session) jq(ctx context.Context, rawURL, query string) (string, error) {
	q, err := gojq.Parse(query)
	if err != nil {
		return "", fmt.Errorf("invalid jq query: %w", err)
	}
	code, err := gojq.Compile(q)
	if err != nil {
		return "", fmt.Errorf("invalid jq query: %w", err)
	}
	res, err := s.do(ctx, http.MethodGet, rawURL, "", maxJSONBytes)
	if err != nil {
		return "", err
	}
	if res.code < 200 || res.code > 299 {
		return "", fmt.Errorf("unexpected status: %s", res.status)
	}
	if len(res.body) > maxJSONBytes {
		return "", fmt.Errorf("response exceeds %d MB", maxJSONBytes>>20)
	}
	var doc any
	if err := json.Unmarshal(res.body, &doc); err != nil {
		return "", fmt.Errorf("response is not valid JSON: %w", err)
	}
	var out strings.Builder
	iter := code.RunWithContext(ctx, doc)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			return "", fmt.Errorf("jq: %w", err)
		}
		b, err := gojq.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("jq: marshal: %w", err)
		}
		if out.Len()+len(b) > maxJQOutputBytes {
			out.WriteString("[truncated]")
			return out.String(), nil
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	if out.Len() == 0 {
		return "(no output)", nil
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

// response is what is left of an http.Response once its body is consumed.
type response struct {
	status      string
	code        int
	contentType string
	body        []byte
}

// do performs one allow-listed request and reads at most limit+1 body bytes
// (the extra byte lets format detect truncation).
func (s *Session) do(ctx context.Context, method, rawURL, body string, limit int64) (response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return response{}, fmt.Errorf("invalid url: %w", err)
	}
	if err := s.checkHost(u); err != nil {
		return response{}, err
	}
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return response{}, fmt.Errorf("build request: %w", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return response{}, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return response{}, fmt.Errorf("read body: %w", err)
	}
	return response{status: resp.Status, code: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: data}, nil
}

// format renders "status\n\nbody", truncating body to limit bytes.
func format(status string, body []byte, limit int) string {
	suffix := ""
	if len(body) > limit {
		body, suffix = body[:limit], "\n[truncated]"
	}
	return fmt.Sprintf("%s\n\n%s%s", status, body, suffix)
}

// Close is a no-op.
func (s *Session) Close() error { return nil }

func (s *Session) checkHost(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme %q not allowed", u.Scheme)
	}
	if !s.hosts[strings.ToLower(u.Host)] {
		return fmt.Errorf("host %q not in allowed hosts", u.Host)
	}
	return nil
}
