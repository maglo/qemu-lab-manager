package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func handler(t *testing.T) http.Handler {
	t.Helper()
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

func get(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A deep link is a route of the application, so it loads the application.
func TestNavigationGetsTheApp(t *testing.T) {
	h := handler(t)

	for _, path := range []string{"/", "/machine/el9-build", "/machine/el9-build/serial"} {
		rec := get(t, h, path, map[string]string{
			"Accept":         "text/html,application/xhtml+xml",
			"Sec-Fetch-Mode": "navigate",
		})
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("%s: body is not the application", path)
		}
	}
}

// An asset that is not there is not a route. Answering it with the
// application hides the missing file: a module then fails as a syntax error
// inside a parser rather than as a 404 on a named file
// (https://github.com/maglo/qemu-lab-manager/issues/53).
func TestMissingAssetIs404(t *testing.T) {
	h := handler(t)

	assets := []string{
		"/vendor/xterm/xterm.js.map",
		"/app/does-not-exist.js",
		"/vendor/novnc/core/nope.js",
		"/app.css.map",
	}
	for _, path := range assets {
		rec := get(t, h, path, map[string]string{
			"Accept":         "*/*",
			"Sec-Fetch-Mode": "no-cors",
		})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

// A client that sends no fetch headers, such as curl, still reaches a deep
// link, and still does not get the application for something that names a
// file.
func TestPathDecidesWithoutFetchHeaders(t *testing.T) {
	h := handler(t)

	rec := get(t, h, "/app/does-not-exist.js", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: status = %d, want 404", rec.Code)
	}
	rec = get(t, h, "/machine/el9-build/serial", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("deep link: status = %d, want 200", rec.Code)
	}
}

// The application asks for its own modules with fetch headers, and a missing
// one is a miss whatever the path looks like. This is the request that used to
// answer with an HTML page and a 200
// (https://github.com/maglo/qemu-lab-manager/issues/53).
func TestBrowserAssetRequestIsNeverARoute(t *testing.T) {
	h := handler(t)

	rec := get(t, h, "/vendor/novnc/core/nope", map[string]string{
		"Accept":         "*/*",
		"Sec-Fetch-Mode": "cors",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The assets the application loads are served, with the headers that keep an
// upgrade from serving a stale app.
func TestAssetsAreServed(t *testing.T) {
	h := handler(t)

	for _, path := range []string{"/app/main.js", "/app.css", "/favicon.svg", "/vendor/novnc/core/rfb.js", "/vendor/xterm/xterm.js"} {
		rec := get(t, h, path, map[string]string{"Accept": "*/*", "Sec-Fetch-Mode": "cors"})
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q", path, cc)
		}
	}
}

// The vendored xterm bundle names no source map, because the map is
// deliberately not vendored (see its PROVENANCE.md).
func TestVendoredBundlesNameNoSourceMap(t *testing.T) {
	h := handler(t)

	for _, path := range []string{"/vendor/xterm/xterm.js", "/vendor/xterm/xterm.css"} {
		rec := get(t, h, path, map[string]string{"Accept": "*/*"})
		if strings.Contains(rec.Body.String(), "sourceMappingURL") {
			t.Errorf("%s names a source map that is not served", path)
		}
	}
}
