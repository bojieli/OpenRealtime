package console

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The console is plain ES modules with no build step, served straight from the
// binary.
//
// That is a deliberate constraint rather than an absence of ambition. This page
// is the most complete worked example of the protocol anyone will read - it
// speaks both transports, negotiates the extension, sends video, and executes
// tools - and a bundled artefact is not something you can read. Someone
// implementing a client in another language should be able to open one file
// per concern and see exactly which events go in which order.
//
//go:embed assets
var assets embed.FS

// assetHandler serves the console's files.
func assetHandler() http.Handler {
	root, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("console assets are missing from the binary: " + err.Error())
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Everything the page loads comes from this origin, and the only
		// places it connects to are this origin's sockets and, for WebRTC,
		// whatever ICE negotiates. Media is blob: because captured video is
		// drawn through a canvas.
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' blob: data:; media-src 'self' blob:; "+
				"connect-src 'self' ws: wss:; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		// A development tool that serves a cached copy of a page you just
		// changed is a development tool that wastes an afternoon. The whole
		// page is a few tens of kilobytes from memory, so there is nothing to
		// be gained by caching it.
		writer.Header().Set("Cache-Control", "no-store")
		// The embedded filesystem reports .js as text/javascript already on
		// most platforms, but not all, and a module served as text/plain is
		// refused by the browser rather than merely rendered oddly.
		if strings.HasSuffix(request.URL.Path, ".js") {
			writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		files.ServeHTTP(writer, request)
	})
}
