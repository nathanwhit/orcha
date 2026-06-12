// Package webui serves the embedded dashboard single-page app. The app is
// built from ui/ (npm run build there writes into dist/ here) and compiled
// into the orcha binary, so the served UI always matches the binary's API.
//
// dist/ is build output and not committed — only a .gitkeep placeholder keeps
// the embed pattern valid on a fresh checkout. A binary built without the UI
// serves a pointer to the build command instead of a blank 404.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the built UI. The app routes client-side with URL hashes, so
// the server only ever sees requests for "/" and real asset files — no SPA
// path fallback is needed.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("webui: embedded dist missing: " + err.Error())
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(notBuilt)
	}
	return http.FileServerFS(sub)
}

func notBuilt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>orcha</title>
<body style="font:14px/1.6 ui-monospace,monospace;background:#0a0d13;color:#e3e9f1;display:grid;place-items:center;min-height:100vh;margin:0">
<div><p>This orcha binary was built without the dashboard.</p>
<p>Build it with <code style="color:#7c93ff">cd ui &amp;&amp; npm install &amp;&amp; npm run build</code>, then rebuild orcha.</p>
<p>The API is still up at <code style="color:#7c93ff">/api</code>.</p></div>`))
}
