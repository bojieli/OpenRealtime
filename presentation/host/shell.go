package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

// BrowserShellFactory serves only the mechanics needed to load a locked
// client manifest. All visible product behavior comes from client plugins.
type BrowserShellFactory struct {
	descriptor      plugin.Descriptor
	bootstrapDigest string
	page            []byte
}

func NewBrowserShellFactory(revision uint64, bootstrapDigest string) (*BrowserShellFactory, error) {
	if revision == 0 {
		return nil, errors.New("browser shell revision must be positive")
	}
	if len(strings.TrimPrefix(bootstrapDigest, "sha256:")) != sha256.Size*2 ||
		!strings.HasPrefix(bootstrapDigest, "sha256:") {
		return nil, errors.New("browser shell requires a SHA-256 bootstrap digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(bootstrapDigest, "sha256:")); err != nil {
		return nil, fmt.Errorf("browser shell bootstrap digest: %w", err)
	}
	bootstrapPath := "/client/v1/modules/" + strings.TrimPrefix(bootstrapDigest, "sha256:")
	page := []byte("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>OpenRealtime</title></head><body><main id=\"openrealtime-root\" " +
		"aria-live=\"polite\">Loading client profile…</main><script type=\"module\" src=\"" +
		template.HTMLEscapeString(bootstrapPath) + "\"></script></body></html>\n")
	pageDigest := sha256.Sum256(page)
	factory := &BrowserShellFactory{
		bootstrapDigest: bootstrapDigest, page: page,
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.presentation.host.browser-shell", Revision: revision,
			Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
			Requires: []plugin.Requirement{
				{Contract: presentation.HTTPRoutesContract},
				{Contract: presentation.ModuleCatalogContract},
				{Contract: presentation.ClientManifestContract},
			},
			Assets: []plugin.Asset{{
				Name: "index.html", MediaType: "text/html",
				Digest: "sha256:" + hex.EncodeToString(pageDigest[:]),
			}},
		},
	}
	return factory, nil
}

func (factory *BrowserShellFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *BrowserShellFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	catalog, err := lookupModuleCatalog(mount.Services)
	if err != nil {
		return err
	}
	if !catalog.Has(factory.bootstrapDigest) {
		return errors.New("browser shell bootstrap is absent from the mounted module store")
	}
	page := append([]byte(nil), factory.page...)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' blob:; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self' ws: wss:; img-src 'self' blob: data:; media-src 'self' blob:; "+
				"frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'self'")
		if request.Method != http.MethodHead {
			_, _ = writer.Write(page)
		}
	})
	return registerRoutes(mount, []Route{
		{Pattern: "GET /{$}", Handler: handler},
		{Pattern: "GET /index.html", Handler: handler},
	})
}
