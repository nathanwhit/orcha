package api

import (
	"net/http"
	"net/url"
	"strings"
)

// exe.dev's authenticating proxy injects these headers on every authenticated
// request it forwards (browser page loads, XHR, and WebSocket upgrades alike).
// Because exe.dev VMs are not reachable from the internet except through that
// proxy, a request that carries the header provably came through authentication
// — the header cannot be forged by reaching the VM directly. See
// https://exe.dev/docs/login-with-exe and /docs/proxy.
const (
	exeEmailHeader = "X-ExeDev-Email"
	exeUserHeader  = "X-ExeDev-UserID"
)

// ExeAuth gates next behind exe.dev identity. A request without an identity did
// not pass through the authenticating proxy: real page navigations are bounced
// to exe.dev's login (which returns here once signed in), everything else is
// refused. When allow is non-empty, only those emails (case-insensitive) pass —
// authentication alone is not enough.
//
// It deliberately does NOT guard the MCP surfaces: agent CLIs reach those over
// the localhost reverse tunnel and never carry exe headers.
func ExeAuth(allow []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allow))
	for _, e := range allow {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			allowed[e] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			email := strings.ToLower(strings.TrimSpace(r.Header.Get(exeEmailHeader)))
			if email == "" {
				if isPageNav(r) {
					// Relative path: the proxy serves /__exe.dev/* on this same
					// origin and redirects back when authentication completes.
					http.Redirect(w, r,
						"/__exe.dev/login?redirect="+url.QueryEscape(r.URL.RequestURI()),
						http.StatusFound)
					return
				}
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			if len(allowed) > 0 && !allowed[email] {
				http.Error(w, "forbidden: "+email+" is not on the allowlist", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isPageNav reports whether r is a top-level browser navigation (worth bouncing
// to login) rather than an API/XHR/WebSocket call (which should just get a 401).
func isPageNav(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		r.Header.Get("Upgrade") == "" &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// whoami reports the exe.dev-authenticated identity to the UI, so it can show who
// is signed in and offer a logout link. Empty fields mean auth is not enabled
// (local dev) — the UI then simply omits the affordance.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"email":  r.Header.Get(exeEmailHeader),
		"userId": r.Header.Get(exeUserHeader),
	})
}

// sameOrigin guards WebSocket upgrades against cross-site hijacking: a browser
// WS handshake carries an Origin, which for a legitimate same-site request
// matches the public host the proxy saw (X-Forwarded-Host). A request with no
// Origin is a non-browser client and is left to header auth. The forwarded host
// is trustworthy here precisely because the VM is only reachable via the proxy.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return strings.EqualFold(u.Host, host)
}
