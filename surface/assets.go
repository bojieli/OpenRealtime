package surface

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The surface is plain ES modules with no build step, served straight from the
// binary.
//
// Same constraint as the console next door, and for the same reason: a page
// that demonstrates a protocol should be readable as the thing it
// demonstrates. One file per channel, no bundle, nothing to compile before you
// can see which events go in which order.
//
//go:embed assets
var assets embed.FS

// assetHandler serves the surface's files.
func assetHandler() http.Handler {
	root, err := fs.Sub(assets, "assets")
	if err != nil {
		panic("surface assets are missing from the binary: " + err.Error())
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Everything the page loads comes from this origin, and the only
		// places it connects to are this origin's sockets and, for WebRTC,
		// whatever ICE negotiates. Media is blob: because captured video is
		// drawn through a canvas, and frames arriving from the browser channel
		// are data: because they arrive as base64 in a JSON body.
		//
		// frame-src is this origin only, which is the artifact route and
		// nothing else. An artifact carries its own far stricter policy in its
		// own response, and the frame element withholds the origin on top of
		// that; what this line prevents is a frame pointed anywhere else.
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' blob: data:; media-src 'self' blob:; "+
				"connect-src 'self' ws: wss:; frame-src 'self'; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		// A development tool that serves a cached copy of a page you just
		// changed is a development tool that wastes an afternoon.
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
