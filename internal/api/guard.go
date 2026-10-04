package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// GuardOpts configures Guard.
type GuardOpts struct {
	// Token, when set, makes /api/* (except GET /api/health) require
	// "Authorization: Bearer <token>". A request with a valid token skips the
	// Host/Origin checks: it is not ambient browser authority.
	Token string
	// AllowedHosts are extra hostnames (no port) accepted in loopback mode, on top of
	// localhost, 127.0.0.1 and ::1.
	AllowedHosts []string
}

// Guard protects the API against browser-driven attacks and, optionally, remote
// access without credentials.
//
// With a token, the API needs the bearer token. Without one (loopback use), it
// rejects DNS rebinding (unknown Host) and cross-site requests (CSRF) by checking
// Host, Sec-Fetch-Site and Origin. Plain clients that send none of those headers
// (curl, scripts) are let through: a local process is already trusted.
func Guard(next http.Handler, o GuardOpts) http.Handler {
	hosts := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range o.AllowedHosts {
		hosts[strings.ToLower(strings.Trim(h, "[]"))] = true
	}
	deny := func(w http.ResponseWriter, status int, msg string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + msg + `"}` + "\n"))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isAPI := r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")

		if o.Token != "" {
			health := r.Method == http.MethodGet && r.URL.Path == "/api/health"
			if isAPI && !health && !validBearer(r.Header.Get("Authorization"), o.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="tarea"`)
				deny(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if !hosts[hostname(r.Host)] {
			deny(w, http.StatusForbidden, "forbidden host")
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			deny(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				deny(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func validBearer(header, token string) bool {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) == 1
}

// hostname returns the lower-cased host of a Host header, without port or brackets.
func hostname(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport // no port
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}
