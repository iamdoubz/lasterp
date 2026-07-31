// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// webRoot is the directory holding the built web client. It defaults to the
// path the container image copies the bundle to (deploy/Dockerfile) and is
// overridable for local runs, where the client is usually served by the Vite
// dev server instead.
func webRoot() string {
	if dir := os.Getenv("LASTERP_WEB_ROOT"); dir != "" {
		return dir
	}
	return "/srv/web"
}

// withStatic serves the built web client from dir alongside the API. Requests
// under the API prefixes always go to the gateway; everything else falls back
// to index.html so client-side routes survive a reload or a deep link.
//
// An absent dir is not an error: an API-only deployment (or any `go test`
// against the handler) simply has no bundle, and every path keeps hitting the
// gateway — which answers unknown paths with problem+json, as it did before.
func withStatic(gw http.Handler, dir string) http.Handler {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return gw
	}
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			gw.ServeHTTP(w, r)
			return
		}
		// A request for a real file (hashed JS/CSS bundle, favicon) is served
		// as-is; anything else is a client route and gets the shell.
		if path := filepath.Join(dir, filepath.Clean(r.URL.Path)); r.URL.Path != "/" && fileExists(path) {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

// isAPIPath reports whether the gateway, not the bundle, owns a path. Keeping
// this an explicit prefix list (rather than "does a file exist?") means a
// mistyped API route returns the API's problem+json 404, not a 200 of the SPA
// shell that a client would try to parse as JSON.
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/healthz"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
