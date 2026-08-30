package review

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
)

const maximumProviderRegistrations = 65_536

// ProviderFactory opens one selected offline review provider. Registry
// construction never calls factories, so credentials and network resources
// stay behind explicit selection.
type ProviderFactory func(context.Context) (Provider, error)

// Registration is one immutable provider-plugin contribution. Descriptor is
// checked again against the opened provider to prevent a symbolic selection
// from silently drifting to another model or wire configuration.
type Registration struct {
	Name       string
	Descriptor ProviderDescriptor
	Factory    ProviderFactory
}

// CatalogEntry is the resource-free inventory safe to expose in plans and
// review metadata. It deliberately contains no factory or credential source.
type CatalogEntry struct {
	Name       string             `json:"name"`
	Descriptor ProviderDescriptor `json:"descriptor"`
}

// Registry is an immutable, concurrency-safe catalog of offline review
// plugins. A benchmark runner chooses a symbolic entry and calls Open only
// after deterministic evidence has been retained.
type Registry struct {
	entries map[string]Registration
	catalog []CatalogEntry
}

func NewRegistry(source []Registration) (*Registry, error) {
	if len(source) == 0 || len(source) > maximumProviderRegistrations {
		return nil, fmt.Errorf(
			"review provider registry needs 1..%d registrations", maximumProviderRegistrations)
	}
	entries := make(map[string]Registration, len(source))
	catalog := make([]CatalogEntry, 0, len(source))
	for index, registration := range source {
		if err := validateMachineIdentifier("review provider registration name", registration.Name, 256); err != nil {
			return nil, fmt.Errorf("review provider registration %d: %w", index, err)
		}
		if err := registration.Descriptor.Validate(); err != nil {
			return nil, fmt.Errorf("review provider registration %q: %w", registration.Name, err)
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf("review provider registration %q has no factory", registration.Name)
		}
		if _, duplicate := entries[registration.Name]; duplicate {
			return nil, fmt.Errorf("review provider registration %q is duplicated", registration.Name)
		}
		entries[registration.Name] = registration
		catalog = append(catalog, CatalogEntry{
			Name: registration.Name, Descriptor: registration.Descriptor,
		})
	}
	sort.Slice(catalog, func(left, right int) bool { return catalog[left].Name < catalog[right].Name })
	return &Registry{entries: entries, catalog: catalog}, nil
}

// Catalog returns a sorted owned snapshot without exposing factories.
func (registry *Registry) Catalog() []CatalogEntry {
	if registry == nil {
		return nil
	}
	result := make([]CatalogEntry, len(registry.catalog))
	copy(result, registry.catalog)
	return result
}

// Open invokes only the selected plugin and verifies its exact descriptor.
func (registry *Registry) Open(ctx context.Context, name string) (Provider, error) {
	if registry == nil {
		return nil, errors.New("review provider registry is nil")
	}
	if ctx == nil {
		return nil, errors.New("open review provider: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := validateMachineIdentifier("review provider selection", name, 256); err != nil {
		return nil, err
	}
	registration, exists := registry.entries[name]
	if !exists {
		return nil, fmt.Errorf("review provider %q is not registered", name)
	}
	provider, openErr := registration.Factory(ctx)
	if cause := context.Cause(ctx); cause != nil {
		return nil, errors.Join(cause, openErr)
	}
	if openErr != nil {
		return nil, fmt.Errorf("open review provider %q: %w", name, openErr)
	}
	if nilInterface(provider) {
		return nil, fmt.Errorf("open review provider %q: factory returned nil", name)
	}
	if descriptor := provider.Descriptor(); descriptor != registration.Descriptor {
		return nil, fmt.Errorf("open review provider %q: descriptor drifted", name)
	}
	return provider, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
