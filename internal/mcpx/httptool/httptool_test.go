package httptool_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/mcpx"
	"github.com/vincent/tarea/internal/mcpx/httptool"
)

func newSession(t *testing.T, h http.Handler) (sess mcpx.Session, baseURL string) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv := config.MCPServer{Name: "http", Builtin: "http", Hosts: []string{u.Host}, Headers: map[string]string{"X-Api-Key": "secret"}}
	return httptool.New(srv, nil), ts.URL
}

func call(s mcpx.Session, args string) (string, error) {
	return s.CallTool(context.Background(), "request", json.RawMessage(args))
}

func TestRequest_GetInjectsHeader(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Method + " " + r.Header.Get("X-Api-Key")))
	}))
	out, err := call(s, `{"url":"`+base+`/x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "200 OK") || !strings.HasSuffix(out, "GET secret") {
		t.Fatalf("got %q", out)
	}
}

func TestRequest_RejectsDisallowedHostMethodAndScheme(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.NotFoundHandler())
	for _, args := range []string{
		`{"url":"http://example.invalid/"}`,
		`{"url":"` + base + `","method":"DELETE"}`,
		`{"url":"file:///etc/passwd"}`,
	} {
		if _, err := call(s, args); err == nil {
			t.Errorf("%s: want error", args)
		}
	}
}

func TestRequest_BlocksRedirectOffAllowList(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
	}))
	if _, err := call(s, `{"url":"`+base+`"}`); err == nil {
		t.Fatal("want redirect error")
	}
}

func TestRequest_TruncatesLargeBody(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 100<<10)))
	}))
	out, err := call(s, `{"url":"`+base+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "[truncated]") || len(out) > 70<<10 {
		t.Fatalf("len %d", len(out))
	}
}

func TestWrap_RoutesBuiltinAndDelegatesRest(t *testing.T) {
	t.Parallel()
	called := false
	dial := httptool.Wrap(func(context.Context, config.MCPServer) (mcpx.Session, error) {
		called = true
		return nil, nil //nolint:nilnil // sentinel
	}, nil)
	sess, err := dial(context.Background(), config.MCPServer{Builtin: "http", Hosts: []string{"x"}})
	if err != nil || sess == nil || called {
		t.Fatalf("builtin: sess=%v err=%v called=%v", sess, err, called)
	}
	if _, _ = dial(context.Background(), config.MCPServer{URL: "http://x"}); !called {
		t.Fatal("non-builtin not delegated")
	}
}

func TestText_ConvertsHTMLAndPassesJSON(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/j" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a":"<b>"}`))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><h1>Gig</h1><p>Tue <a href="http://x/t">tickets</a></p></body></html>`))
	}))
	out, err := s.CallTool(context.Background(), "text", json.RawMessage(`{"url":"`+base+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<h1>") || !strings.Contains(out, "Gig") || !strings.Contains(out, "http://x/t") || strings.Contains(out, "\r") {
		t.Fatalf("got %q", out)
	}
	out, err = s.CallTool(context.Background(), "text", json.RawMessage(`{"url":"`+base+`/j"}`))
	if err != nil || !strings.HasSuffix(out, `{"a":"<b>"}`) {
		t.Fatalf("json: %q %v", out, err)
	}
}

func TestText_RejectsDisallowedHost(t *testing.T) {
	t.Parallel()
	s, _ := newSession(t, http.NotFoundHandler())
	if _, err := s.CallTool(context.Background(), "text", json.RawMessage(`{"url":"http://example.invalid/"}`)); err == nil {
		t.Fatal("want error")
	}
}

func TestListTools_OffersRequestAndText(t *testing.T) {
	t.Parallel()
	s, _ := newSession(t, http.NotFoundHandler())
	tools, err := s.ListTools(context.Background())
	if err != nil || len(tools) != 2 || tools[0].Name != "request" || tools[1].Name != "text" {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
}

func TestText_IgnoresInlineScripts(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><script>var a = '<b>';" + strings.Repeat("x", 1000) +
			"</script></head><body><ul><li>Warhaus at Razzmatazz</li></ul><script>track()</script></body></html>"))
	}))
	out, err := s.CallTool(context.Background(), "text", json.RawMessage(`{"url":"`+base+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Warhaus at Razzmatazz") || strings.Contains(out, "track()") || strings.Contains(out, "xxx") {
		t.Fatalf("got %q", out)
	}
}

func TestText_ErrorsOnEmptyConversion(t *testing.T) {
	t.Parallel()
	s, base := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><script>render()</script></body></html>"))
	}))
	if _, err := s.CallTool(context.Background(), "text", json.RawMessage(`{"url":"`+base+`"}`)); err == nil {
		t.Fatal("want error")
	}
}
