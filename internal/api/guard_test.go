package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vincent/tarea/internal/api"
)

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	cases := []struct {
		name    string
		opts    api.GuardOpts
		method  string
		path    string
		host    string
		headers map[string]string
		want    int
	}{
		{"loopback plain GET", api.GuardOpts{}, "GET", "/api/jobs", "127.0.0.1:8080", nil, 204},
		{"localhost curl POST", api.GuardOpts{}, "POST", "/api/jobs/x/run", "localhost:8080", nil, 204},
		{"ipv6 loopback", api.GuardOpts{}, "GET", "/api/jobs", "[::1]:8080", nil, 204},
		{"rebinding host", api.GuardOpts{}, "GET", "/api/jobs", "evil.example:8080", nil, 403},
		{"allowed extra host", api.GuardOpts{AllowedHosts: []string{"Tarea.Lan"}}, "GET", "/", "tarea.lan:8080", nil, 204},
		{"same-origin POST", api.GuardOpts{}, "POST", "/api/jobs/x/run", "localhost:8080",
			map[string]string{"Origin": "http://localhost:8080", "Sec-Fetch-Site": "same-origin"}, 204},
		{"cross-origin POST", api.GuardOpts{}, "POST", "/api/jobs/x/run", "localhost:8080",
			map[string]string{"Origin": "http://evil.example"}, 403},
		{"cross-site fetch metadata", api.GuardOpts{}, "POST", "/api/jobs/x/run", "localhost:8080",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site (other port) refused", api.GuardOpts{}, "GET", "/api/jobs", "localhost:8080",
			map[string]string{"Sec-Fetch-Site": "same-site"}, 403},

		{"token missing", api.GuardOpts{Token: "s3cret"}, "GET", "/api/jobs", "tarea.lan", nil, 401},
		{"token wrong", api.GuardOpts{Token: "s3cret"}, "GET", "/api/jobs", "tarea.lan",
			map[string]string{"Authorization": "Bearer nope"}, 401},
		{"token wrong scheme", api.GuardOpts{Token: "s3cret"}, "GET", "/api/jobs", "tarea.lan",
			map[string]string{"Authorization": "Basic s3cret"}, 401},
		{"token ok, foreign host", api.GuardOpts{Token: "s3cret"}, "POST", "/api/jobs/x/run", "tarea.lan",
			map[string]string{"Authorization": "Bearer s3cret"}, 204},
		{"token ok, case-insensitive scheme", api.GuardOpts{Token: "s3cret"}, "GET", "/api/jobs", "tarea.lan",
			map[string]string{"Authorization": "bearer s3cret"}, 204},
		{"health exempt", api.GuardOpts{Token: "s3cret"}, "GET", "/api/health", "tarea.lan", nil, 204},
		{"UI assets public", api.GuardOpts{Token: "s3cret"}, "GET", "/", "tarea.lan", nil, 204},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			req.Host = c.host
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			api.Guard(ok, c.opts).ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
			if c.want == 401 && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
		})
	}
}
