package host

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

type ManifestFactory struct {
	descriptor plugin.Descriptor
	manifest   presentation.ClientManifest
	payload    []byte
}

func NewManifestFactory(manifest presentation.ClientManifest) (*ManifestFactory, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	payload, err := presentation.MarshalManifest(manifest)
	if err != nil {
		return nil, err
	}
	return &ManifestFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.presentation.host.client-manifest", Revision: 1,
			Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
			Provides: []plugin.Contract{presentation.ClientManifestContract},
			Requires: []plugin.Requirement{
				{Contract: presentation.HTTPRoutesContract},
				{Contract: presentation.ModuleCatalogContract},
			},
		},
		manifest: manifest.Clone(), payload: slices.Clone(payload),
	}, nil
}

func (factory *ManifestFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *ManifestFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	catalog, err := lookupModuleCatalog(mount.Services)
	if err != nil {
		return err
	}
	for _, asset := range factory.manifest.Assets {
		if !catalog.Has(asset.Digest) {
			return errors.New("client manifest references an asset absent from the mounted module store")
		}
	}
	payload := slices.Clone(factory.payload)
	fingerprint := factory.manifest.Fingerprint
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("ETag", `"`+fingerprint+`"`)
		if request.Header.Get("If-None-Match") == `"`+fingerprint+`"` {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		if request.Method != http.MethodHead {
			_, _ = writer.Write(payload)
		}
	})
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/manifest", Handler: handler,
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.ClientManifestContract, factory.manifest.Clone())
}
