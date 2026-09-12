// Package web serves labview's browser application.
//
// The whole UI is embedded in the binary, because labview is one binary with
// one inventory file (design preamble) and a lab is not expected to reach a
// CDN.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:static
var content embed.FS

// Handler serves the application: static assets by path, and index.html for
// anything else so that a deep link like /machine/el9-build/logs loads the
// app rather than 404ing.
func Handler() (http.Handler, error) {
	root, err := fs.Sub(content, "static")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(root))

	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, err
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			serveIndex(w, index)
			return
		}
		if _, err := fs.Stat(root, path); err != nil {
			// Not an asset: hand the route to the app.
			serveIndex(w, index)
			return
		}
		// The UI is embedded and versioned with the binary, so a long
		// cache would serve a stale app after an upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}), nil
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// The app loads only its own assets, so a policy this tight costs
	// nothing and rules out an injected script reaching a console.
	//
	// style-src allows inline styles because both vendored renderers need
	// them: xterm injects a stylesheet for its theme and noVNC sets style
	// attributes while laying out the framebuffer, and neither accepts a
	// nonce. Keeping script-src strict is what actually matters here --
	// inline styles cannot reach a console, an injected script could.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data: blob:; "+
			"style-src 'self' 'unsafe-inline'; script-src 'self'; "+
			"connect-src 'self' ws: wss:; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(index)
}
