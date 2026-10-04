package mcpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/mcpx"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type fakeSession struct {
	mu        sync.Mutex
	tools     []mcpx.Tool
	listErr   error
	callFn    func(ctx context.Context, name string, args json.RawMessage) (string, error)
	closed    bool
	closeErr  error
	callCount int
}

func (f *fakeSession) ListTools(context.Context) ([]mcpx.Tool, error) { return f.tools, f.listErr }

func (f *fakeSession) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	f.mu.Lock()
	f.callCount++
	f.mu.Unlock()
	return f.callFn(ctx, name, args)
}

func (f *fakeSession) Close() error { f.closed = true; return f.closeErr }

func dialer(sessions map[string]*fakeSession, dialErr map[string]error) mcpx.Dialer {
	return func(_ context.Context, srv config.MCPServer) (mcpx.Session, error) {
		if err := dialErr[srv.Name]; err != nil {
			return nil, err
		}
		return sessions[srv.Name], nil
	}
}

func echo(_ context.Context, name string, args json.RawMessage) (string, error) {
	return name + ":" + string(args), nil
}

func TestOpen_AllowListNamespacingAndRouting(t *testing.T) {
	t.Parallel()
	lib := &fakeSession{callFn: echo, tools: []mcpx.Tool{{Name: "top_artists", Description: "d"}, {Name: "delete_all"}}}
	ev := &fakeSession{callFn: echo, tools: []mcpx.Tool{{Name: "search.events"}, {Name: "top_artists"}}}
	servers := []config.MCPServer{
		{Name: "library", Command: []string{"x"}, Allow: []string{"top_artists"}},
		{Name: "events", URL: "http://x", Allow: []string{"*"}},
	}

	h, err := mcpx.Open(context.Background(), servers, dialer(map[string]*fakeSession{"library": lib, "events": ev}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	names := make([]string, 0, len(h.Tools()))
	for _, d := range h.Tools() {
		names = append(names, d.Name)
	}
	want := "events__search_events,events__top_artists,library__top_artists" // sanitized, sorted, delete_all filtered out.
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v", names)
	}

	out, err := h.Call(context.Background(), "library__top_artists", json.RawMessage(`{"n":3}`))
	if err != nil || out != `top_artists:{"n":3}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, err = h.Call(context.Background(), "events__search_events", json.RawMessage(`{}`))
	if err != nil || !strings.HasPrefix(out, "search.events:") { // original (unsanitized) name reaches the server.
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, err = h.Call(context.Background(), "library__delete_all", nil); err == nil {
		t.Fatal("filtered tool must not be callable")
	}
}

func TestOpen_AllowedToolMissingFailsFastAndCleansUp(t *testing.T) {
	t.Parallel()
	good := &fakeSession{callFn: echo, tools: []mcpx.Tool{{Name: "a"}}}
	bad := &fakeSession{callFn: echo, tools: []mcpx.Tool{{Name: "a"}}}
	servers := []config.MCPServer{
		{Name: "good", Command: []string{"x"}, Allow: []string{"a"}},
		{Name: "bad", Command: []string{"x"}, Allow: []string{"typo"}},
	}

	_, err := mcpx.Open(context.Background(), servers, dialer(map[string]*fakeSession{"good": good, "bad": bad}, nil))
	if err == nil || !strings.Contains(err.Error(), `"typo"`) || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v", err)
	}
	if !good.closed || !bad.closed {
		t.Fatalf("sessions leaked: good=%v bad=%v", good.closed, bad.closed)
	}
}

func TestOpen_DialAndListErrors(t *testing.T) {
	t.Parallel()
	first := &fakeSession{callFn: echo, tools: []mcpx.Tool{{Name: "a"}}}
	servers := []config.MCPServer{
		{Name: "first", Command: []string{"x"}, Allow: []string{"*"}},
		{Name: "second", Command: []string{"x"}, Allow: []string{"*"}},
	}
	_, err := mcpx.Open(context.Background(), servers, dialer(map[string]*fakeSession{"first": first}, map[string]error{"second": errors.New("refused")}))
	if err == nil || !strings.Contains(err.Error(), "refused") || !first.closed {
		t.Fatalf("err=%v first.closed=%v", err, first.closed)
	}

	listFail := &fakeSession{listErr: errors.New("boom")}
	_, err = mcpx.Open(context.Background(), servers[:1], dialer(map[string]*fakeSession{"first": listFail}, nil))
	if err == nil || !strings.Contains(err.Error(), "list tools") || !listFail.closed {
		t.Fatalf("err=%v closed=%v", err, listFail.closed)
	}
}

func TestOpen_NameCollision(t *testing.T) {
	t.Parallel()
	s := &fakeSession{callFn: echo, tools: []mcpx.Tool{{Name: "a.b"}, {Name: "a_b"}}}
	_, err := mcpx.Open(context.Background(), []config.MCPServer{{Name: "s", Command: []string{"x"}, Allow: []string{"*"}}}, dialer(map[string]*fakeSession{"s": s}, nil))
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("err = %v", err)
	}
}

func TestCall_TimeoutAndError(t *testing.T) {
	t.Parallel()
	s := &fakeSession{tools: []mcpx.Tool{{Name: "slow"}, {Name: "fail"}}}
	s.callFn = func(ctx context.Context, name string, _ json.RawMessage) (string, error) {
		if name == "fail" {
			return "", errors.New("upstream 500")
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	h, err := mcpx.Open(context.Background(), []config.MCPServer{{Name: "s", Command: []string{"x"}, Allow: []string{"*"}}}, dialer(map[string]*fakeSession{"s": s}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	h.CallTimeout = 20 * time.Millisecond

	if _, err = h.Call(context.Background(), "s__slow", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	if _, err = h.Call(context.Background(), "s__fail", nil); err == nil || !strings.Contains(err.Error(), "upstream 500") {
		t.Fatalf("err = %v", err)
	}
}

func TestClose_JoinsErrorsAndClosesAll(t *testing.T) {
	t.Parallel()
	a := &fakeSession{callFn: echo, closeErr: errors.New("a failed")}
	b := &fakeSession{callFn: echo, closeErr: errors.New("b failed")}
	servers := []config.MCPServer{{Name: "a", Command: []string{"x"}, Allow: []string{"*"}}, {Name: "b", Command: []string{"x"}, Allow: []string{"*"}}}
	h, err := mcpx.Open(context.Background(), servers, dialer(map[string]*fakeSession{"a": a, "b": b}, nil))
	if err != nil {
		t.Fatal(err)
	}
	err = h.Close()
	if err == nil || !strings.Contains(err.Error(), "a failed") || !strings.Contains(err.Error(), "b failed") || !a.closed || !b.closed {
		t.Fatalf("err=%v", err)
	}
	if err = h.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

func TestOpen_NoServers(t *testing.T) {
	t.Parallel()
	h, err := mcpx.Open(context.Background(), nil, nil)
	if err != nil || len(h.Tools()) != 0 {
		t.Fatalf("h=%v err=%v", h, err)
	}
}
