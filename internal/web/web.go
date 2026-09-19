// Package web serves labview's browser application.
//
// The whole UI is embedded in the binary, because labview is one binary with
// one inventory directory (design preamble) and a lab is not expected to
// reach a CDN.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var content embed.FS

// Handler serves the application: static assets by path, and index.html for
// a route so that a deep link like /machine/el9-build/logs loads the app
// rather than 404ing.
//
// The fallback applies to a navigation and to nothing else. A missing module
// answered with index.html and a 200 fails inside a parser as a syntax error
// on the wrong file, which hides the one fact worth having: the file is not
// there.
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
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			serveIndex(w, index)
			return
		}
		if _, err := fs.Stat(root, path); err != nil {
			if !isNavigation(r) {
				http.NotFound(w, r)
				return
			}
			// A route: hand it to the app.
			serveIndex(w, index)
			return
		}
		// The UI is embedded and versioned with the binary, so a long
		// cache would serve a stale app after an upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}), nil
}

// isNavigation reports whether a request could be one of the app's routes.
//
// Two rules, and they answer the two ways the question arrives. A browser
// says what it is fetching: Sec-Fetch-Mode is "navigate" for a page and
// something else for a module, a stylesheet or an image, and that alone
// settles every request the application itself makes. Anything else is judged
// by the path: a route names no file, so a path with an extension is a miss
// rather than a route. That keeps a deep link working for a client that sends
// no fetch headers, such as curl.
func isNavigation(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return path.Ext(r.URL.Path) == ""
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
