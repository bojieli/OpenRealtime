// Package browser embeds the browser demo.
//
// The demo is one HTML file with no build step, and embedding it lets the
// server offer it directly - so trying the system out is one command rather
// than one command plus a static file server. It is off by default: a
// production server has no business serving a page, and a demo that appears
// on every deployment is surface nobody asked for.
package browser

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var page []byte

// Page is the demo's HTML.
var Page = page

// Handler serves the demo.
//
// It serves one file and nothing else. A directory handler would turn a
// convenience into a way to read whatever happened to be next to it.
func Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page talks to a WebSocket and a peer connection on the same
		// origin and loads nothing from anywhere else, so it says so.
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
				"connect-src 'self' ws: wss:; media-src 'self' blob:")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		if request.Method == http.MethodHead {
			return
		}
		_, _ = writer.Write(page)
	})
}
