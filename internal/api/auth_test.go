package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestExeAuth(t *testing.T) {
	cases := []struct {
		name     string
		allow    []string
		method   string
		accept   string
		upgrade  string
		email    string
		wantCode int
		wantLoc  string // expected Location prefix on a redirect
	}{
		{
			name: "authenticated passes", method: "GET", email: "me@deno.com",
			wantCode: http.StatusOK,
		},
		{
			name: "xhr without identity is 401", method: "GET", accept: "application/json",
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "page nav without identity redirects to login",
			method: "GET", accept: "text/html",
			wantCode: http.StatusFound, wantLoc: "/__exe.dev/login?redirect=",
		},
		{
			name: "websocket without identity is 401 not redirect",
			method: "GET", accept: "text/html", upgrade: "websocket",
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "allowlisted email passes", allow: []string{"Me@Deno.com"},
			method: "GET", email: "me@deno.com", wantCode: http.StatusOK,
		},
		{
			name: "off-allowlist email is forbidden", allow: []string{"boss@deno.com"},
			method: "GET", email: "me@deno.com", wantCode: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ExeAuth(tc.allow)(okHandler())
			r := httptest.NewRequest(tc.method, "/dash", nil)
			if tc.accept != "" {
				r.Header.Set("Accept", tc.accept)
			}
			if tc.upgrade != "" {
				r.Header.Set("Upgrade", tc.upgrade)
			}
			if tc.email != "" {
				r.Header.Set(exeEmailHeader, tc.email)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if tc.wantLoc != "" && !strings.HasPrefix(w.Header().Get("Location"), tc.wantLoc) {
				t.Fatalf("Location = %q, want prefix %q", w.Header().Get("Location"), tc.wantLoc)
			}
		})
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name      string
		origin    string
		fwdHost   string
		host      string
		wantAllow bool
	}{
		{name: "no origin (non-browser) allowed", wantAllow: true},
		{name: "origin matches forwarded host", origin: "https://vm.exe.xyz", fwdHost: "vm.exe.xyz", wantAllow: true},
		{name: "origin matches host when no forwarded host", origin: "https://vm.exe.xyz", host: "vm.exe.xyz", wantAllow: true},
		{name: "cross-site origin rejected", origin: "https://evil.example", fwdHost: "vm.exe.xyz", wantAllow: false},
		{name: "garbage origin rejected", origin: "://nope", fwdHost: "vm.exe.xyz", wantAllow: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/sessions/x/pty", nil)
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.fwdHost != "" {
				r.Header.Set("X-Forwarded-Host", tc.fwdHost)
			}
			if got := sameOrigin(r); got != tc.wantAllow {
				t.Fatalf("sameOrigin = %v, want %v", got, tc.wantAllow)
			}
		})
	}
}
