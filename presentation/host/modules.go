package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

// ModuleSource is one immutable browser resource bundled by a module-store
// implementation. Content is copied during construction.
type ModuleSource struct {
	Name      string
	MediaType string
	Content   []byte
}

// ServedModule is the public, payload-free module catalog row.
type ServedModule struct {
	Asset plugin.Asset `json:"asset"`
	Path  string       `json:"path"`
}

// ModuleCatalog exposes only immutable identities and same-origin paths.
type ModuleCatalog interface {
	Modules() []ServedModule
	Has(digest string) bool
}

type moduleStore struct {
	modules map[string][]byte
	rows    []ServedModule
}

func (store *moduleStore) Modules() []ServedModule { return slices.Clone(store.rows) }

func (store *moduleStore) Has(digest string) bool {
	_, found := store.modules[digest]
	return found
}

type ModuleStoreFactory struct {
	descriptor plugin.Descriptor
	modules    map[string][]byte
	rows       []ServedModule
}

func NewModuleStoreFactory(revision uint64, sources []ModuleSource) (*ModuleStoreFactory, error) {
	if revision == 0 {
		return nil, errors.New("presentation module store revision must be positive")
	}
	if len(sources) == 0 {
		return nil, errors.New("presentation module store requires at least one module")
	}
	factory := &ModuleStoreFactory{modules: make(map[string][]byte)}
	seenNames := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if source.Name == "" || source.Name != strings.TrimSpace(source.Name) || strings.Contains(source.Name, "..") {
			return nil, fmt.Errorf("invalid presentation module name %q", source.Name)
		}
		if _, duplicate := seenNames[source.Name]; duplicate {
			return nil, fmt.Errorf("presentation module store repeats name %q", source.Name)
		}
		seenNames[source.Name] = struct{}{}
		if len(source.Content) == 0 {
			return nil, fmt.Errorf("presentation module %s is empty", source.Name)
		}
		digestBytes := sha256.Sum256(source.Content)
		digest := "sha256:" + hex.EncodeToString(digestBytes[:])
		if _, duplicate := factory.modules[digest]; duplicate {
			return nil, fmt.Errorf("presentation module %s duplicates content digest %s", source.Name, digest)
		}
		asset := plugin.Asset{Name: source.Name, MediaType: source.MediaType, Digest: digest}
		factory.modules[digest] = slices.Clone(source.Content)
		factory.rows = append(factory.rows, ServedModule{
			Asset: asset, Path: "/client/v1/modules/" + strings.TrimPrefix(digest, "sha256:"),
		})
	}
	factory.descriptor = plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.module-store", Revision: revision,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{presentation.ModuleCatalogContract},
		Requires: []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
		Assets:   make([]plugin.Asset, 0, len(factory.rows)),
	}
	for _, row := range factory.rows {
		factory.descriptor.Assets = append(factory.descriptor.Assets, row.Asset)
	}
	canonical, err := factory.descriptor.Canonical()
	if err != nil {
		return nil, err
	}
	factory.descriptor = canonical
	slices.SortFunc(factory.rows, func(left, right ServedModule) int {
		return strings.Compare(left.Asset.Name, right.Asset.Name)
	})
	return factory, nil
}

func (factory *ModuleStoreFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *ModuleStoreFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	store := &moduleStore{modules: make(map[string][]byte, len(factory.modules)), rows: slices.Clone(factory.rows)}
	for digest, content := range factory.modules {
		store.modules[digest] = slices.Clone(content)
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hexDigest := request.PathValue("digest")
		if len(hexDigest) != sha256.Size*2 {
			http.NotFound(writer, request)
			return
		}
		digest := "sha256:" + strings.ToLower(hexDigest)
		content, found := store.modules[digest]
		if !found || strings.TrimPrefix(digest, "sha256:") != hexDigest {
			http.NotFound(writer, request)
			return
		}
		var asset plugin.Asset
		for _, row := range store.rows {
			if row.Asset.Digest == digest {
				asset = row.Asset
				break
			}
		}
		writer.Header().Set("Content-Type", asset.MediaType)
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		writer.Header().Set("ETag", `"`+digest+`"`)
		if request.Method != http.MethodHead {
			_, _ = writer.Write(content)
		}
	})
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/modules/{digest}", Handler: handler,
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.ModuleCatalogContract, ModuleCatalog(store))
}

func lookupModuleCatalog(services pluginruntime.Services) (ModuleCatalog, error) {
	value, contract, _, _, found := services.Lookup(presentation.ModuleCatalogContract.Name)
	if !found || contract != presentation.ModuleCatalogContract {
		return nil, errors.New("presentation module catalog is unavailable")
	}
	catalog, ok := value.(ModuleCatalog)
	if !ok || catalog == nil {
		return nil, errors.New("presentation module catalog has the wrong Go type")
	}
	return catalog, nil
}
