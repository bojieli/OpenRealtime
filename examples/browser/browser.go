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
		// The page loads nothing from anywhere else, so it says so - but it
		// does connect elsewhere, and that is the whole of what it does. The
		// SDP offer goes to an adapter that is a different origin in every
		// documented configuration: the README points it with
		// ?adapter=http://host:port/v1/realtime/calls, and `serve -demo
		// -webrtc-listen 127.0.0.1:8766` puts the page on one port and the
		// adapter on another. connect-src 'self' forbade exactly that request,
		// so the demo could not reach an adapter anywhere.
		//
		// http: and https: rather than a fixed origin, because the adapter's
		// address is the one thing this page takes as a parameter. It loads no
		// code from there and can do nothing with the answer but hand it to a
		// peer connection.
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
				"connect-src 'self' http: https: ws: wss:; media-src 'self' blob:")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		if request.Method == http.MethodHead {
			return
		}
		_, _ = writer.Write(page)
	})
}
