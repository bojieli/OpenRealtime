package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
)

const (
	maximumProviderRegistrations = 1024
	maximumProviderRegistryBytes = 64 << 20
)

// ProviderCapabilities is the provider-neutral media admission contract. It
// describes what a selected evaluator can actually review before any network
// side effect occurs; presentation clients and benchmark runners do not need
// provider-specific type assertions.
type ProviderCapabilities struct {
	MediaTypes        []string `json:"media_types"`
	MaximumMediaCount int      `json:"maximum_media_count"`
	MaximumMediaBytes int64    `json:"maximum_media_bytes"`
}

func (capabilities ProviderCapabilities) Clone() ProviderCapabilities {
	capabilities.MediaTypes = slices.Clone(capabilities.MediaTypes)
	return capabilities
}

func (capabilities ProviderCapabilities) Validate() error {
	if len(capabilities.MediaTypes) == 0 || len(capabilities.MediaTypes) > 32 ||
		capabilities.MaximumMediaCount <= 0 || capabilities.MaximumMediaCount > maximumMediaCount ||
		capabilities.MaximumMediaBytes <= 0 || capabilities.MaximumMediaBytes > maximumMediaBytes {
		return errors.New("review provider capabilities have invalid media bounds")
	}
	for index, mediaType := range capabilities.MediaTypes {
		separator := strings.IndexByte(mediaType, '/')
		if separator <= 0 || !supportedReviewMedia(mediaType[:separator], mediaType) {
			return fmt.Errorf("review provider capability media type %q is unsupported", mediaType)
		}
		if index > 0 && capabilities.MediaTypes[index-1] >= mediaType {
			return errors.New("review provider capability media types must be sorted and unique")
		}
	}
	return nil
}

// SHA256 is the canonical identity of the complete media-admission contract.
// Provider descriptors and retained records bind this digest so a capability
// change cannot masquerade as another run of the same evaluator provenance.
func (capabilities ProviderCapabilities) SHA256() (string, error) {
	if err := capabilities.Validate(); err != nil {
		return "", err
	}
	payload, err := marshalCanonicalCompact(capabilities, 16<<10)
	if err != nil {
		return "", err
	}
	return digest(payload), nil
}

func (capabilities ProviderCapabilities) supports(media []PreparedMedia) error {
	if err := capabilities.Validate(); err != nil {
		return err
	}
	if len(media) == 0 || len(media) > capabilities.MaximumMediaCount {
		return errors.New("prepared review media count exceeds provider capabilities")
	}
	total := int64(0)
	for index, item := range media {
		position, found := slices.BinarySearch(capabilities.MediaTypes, item.MediaType)
		if !found || position < 0 {
			return fmt.Errorf("prepared review media %d type %q is unsupported by the provider", index, item.MediaType)
		}
		if int64(len(item.Bytes)) > capabilities.MaximumMediaBytes-total {
			return errors.New("prepared review media bytes exceed provider capabilities")
		}
		total += int64(len(item.Bytes))
	}
	return nil
}

// ProviderFactory opens one selected offline review provider. Registry
// construction never calls factories, so credentials and network resources
// stay behind explicit selection. Until the returned provider successfully
// accepts Claim, the factory retains ownership. A factory returning an error
// must clean up any provider it constructed and should return nil; Registry
// deliberately does not close an unclaimed value because it may be a
// singleton already owned by another lease.
type ProviderFactory func(context.Context) (Provider, error)

// Registration is one immutable provider-plugin contribution. Descriptor is
// checked again against the opened provider to prevent a symbolic selection
// from silently drifting to another model or wire configuration.
type Registration struct {
	Name           string
	Descriptor     ProviderDescriptor
	Capabilities   ProviderCapabilities
	Implementation []byte
	Configuration  []byte
	Factory        ProviderFactory
}

// CatalogEntry is the resource-free inventory safe to expose in plans and
// review metadata. It deliberately contains no factory or credential source.
type CatalogEntry struct {
	Name         string               `json:"name"`
	Descriptor   ProviderDescriptor   `json:"descriptor"`
	Capabilities ProviderCapabilities `json:"capabilities"`
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
	aggregateBytes := 0
	for index, registration := range source {
		if err := validateMachineIdentifier("review provider registration name", registration.Name, 256); err != nil {
			return nil, fmt.Errorf("review provider registration %d: %w", index, err)
		}
		if err := registration.Descriptor.Validate(); err != nil {
			return nil, fmt.Errorf("review provider registration %q: %w", registration.Name, err)
		}
		if err := registration.Capabilities.Validate(); err != nil {
			return nil, fmt.Errorf("review provider registration %q: %w", registration.Name, err)
		}
		capabilitiesSHA256, err := registration.Capabilities.SHA256()
		if err != nil || capabilitiesSHA256 != registration.Descriptor.CapabilitiesSHA256 {
			return nil, fmt.Errorf(
				"review provider registration %q capabilities identity drifted", registration.Name)
		}
		if len(registration.Implementation) == 0 || len(registration.Implementation) > 1<<20 ||
			digest(registration.Implementation) != registration.Descriptor.Implementation.SHA256 {
			return nil, fmt.Errorf("review provider registration %q implementation bytes drifted", registration.Name)
		}
		canonicalConfiguration, err := canonicalJSON(registration.Configuration, 1<<20)
		if err != nil || !bytes.Equal(canonicalConfiguration, registration.Configuration) ||
			digest(registration.Configuration) != registration.Descriptor.ConfigurationSHA256 {
			return nil, fmt.Errorf("review provider registration %q configuration bytes drifted", registration.Name)
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf("review provider registration %q has no factory", registration.Name)
		}
		if _, duplicate := entries[registration.Name]; duplicate {
			return nil, fmt.Errorf("review provider registration %q is duplicated", registration.Name)
		}
		entryBytes := len(registration.Name) + len(registration.Implementation) + len(registration.Configuration)
		for _, mediaType := range registration.Capabilities.MediaTypes {
			entryBytes += len(mediaType)
		}
		if entryBytes > maximumProviderRegistryBytes-aggregateBytes {
			return nil, fmt.Errorf(
				"review provider registry exceeds the %d-byte aggregate limit", maximumProviderRegistryBytes)
		}
		aggregateBytes += entryBytes
		registration.Implementation = slices.Clone(registration.Implementation)
		registration.Configuration = slices.Clone(registration.Configuration)
		registration.Capabilities = registration.Capabilities.Clone()
		entries[registration.Name] = registration
		catalog = append(catalog, CatalogEntry{
			Name: registration.Name, Descriptor: registration.Descriptor,
			Capabilities: registration.Capabilities.Clone(),
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
	for index := range result {
		result[index].Capabilities = result[index].Capabilities.Clone()
	}
	return result
}

// Open invokes only the selected plugin and verifies its exact descriptor.
func (registry *Registry) Open(ctx context.Context, name string) (*ProviderLease, error) {
	if registry == nil {
		return nil, errors.New("review provider registry is nil")
	}
	if ctx == nil {
		return nil, errors.New("open review provider: nil context")
	}
	if err := ctx.Err(); err != nil {
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
	if openErr != nil {
		if cause := ctx.Err(); cause != nil {
			return nil, errors.Join(cause, fmt.Errorf("open review provider %q: %w", name, openErr))
		}
		return nil, fmt.Errorf("open review provider %q: %w", name, openErr)
	}
	if nilInterface(provider) {
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		return nil, fmt.Errorf("open review provider %q: factory returned nil", name)
	}
	if err := provider.Claim(); err != nil {
		return nil, fmt.Errorf("open review provider %q: exclusive ownership claim failed", name)
	}
	if cause := ctx.Err(); cause != nil {
		return nil, errors.Join(cause, closeProvider(provider))
	}
	descriptor := provider.Descriptor()
	if cause := ctx.Err(); cause != nil {
		return nil, errors.Join(cause, closeProvider(provider))
	}
	if descriptor != registration.Descriptor {
		return nil, errors.Join(
			fmt.Errorf("open review provider %q: descriptor drifted", name), closeProvider(provider))
	}
	capabilities := provider.Capabilities()
	if cause := ctx.Err(); cause != nil {
		return nil, errors.Join(cause, closeProvider(provider))
	}
	if err := capabilities.Validate(); err != nil ||
		!reflect.DeepEqual(capabilities, registration.Capabilities) {
		return nil, errors.Join(
			fmt.Errorf("open review provider %q: capabilities drifted", name), closeProvider(provider))
	}
	implementation := provider.Implementation()
	if cause := ctx.Err(); cause != nil {
		return nil, errors.Join(cause, closeProvider(provider))
	}
	if !bytes.Equal(implementation, registration.Implementation) {
		return nil, errors.Join(
			fmt.Errorf("open review provider %q: implementation drifted", name), closeProvider(provider))
	}
	configuration := provider.Configuration()
	if cause := ctx.Err(); cause != nil {
		return nil, errors.Join(cause, closeProvider(provider))
	}
	if !bytes.Equal(configuration, registration.Configuration) {
		return nil, errors.Join(
			fmt.Errorf("open review provider %q: configuration drifted", name), closeProvider(provider))
	}
	lease := &ProviderLease{
		provider: provider, descriptor: registration.Descriptor,
		capabilities:   registration.Capabilities.Clone(),
		implementation: slices.Clone(registration.Implementation),
		configuration:  slices.Clone(registration.Configuration),
	}
	lease.condition = sync.NewCond(&lease.mu)
	return lease, nil
}

// ProviderLease is the only provider handle accepted by Evaluate. It must not
// be copied after first use because it owns synchronization and provider
// lifetime state. Its unexported construction binds a selected registration's
// exact implementation and configuration bytes to the live provider and
// guarantees cleanup.
type ProviderLease struct {
	mu             sync.Mutex
	condition      *sync.Cond
	provider       Provider
	descriptor     ProviderDescriptor
	capabilities   ProviderCapabilities
	implementation []byte
	configuration  []byte
	active         int
	closing        bool
	closed         bool
	closeErr       error
}

func (lease *ProviderLease) provenance() (ProviderDescriptor, []byte, []byte, error) {
	if lease == nil {
		return ProviderDescriptor{}, nil, nil, errors.New("review provider lease is nil")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.condition == nil || lease.closing || lease.closed || nilInterface(lease.provider) {
		return ProviderDescriptor{}, nil, nil, errors.New("review provider lease is closed")
	}
	if err := validateLeaseProvenance(
		lease.descriptor, lease.implementation, lease.configuration,
	); err != nil {
		return ProviderDescriptor{}, nil, nil, err
	}
	return lease.descriptor, slices.Clone(lease.implementation), slices.Clone(lease.configuration), nil
}

// Descriptor returns the immutable descriptor bound to an open lease. A nil,
// closed, or malformed zero-value lease returns the zero descriptor.
func (lease *ProviderLease) Descriptor() ProviderDescriptor {
	if lease == nil {
		return ProviderDescriptor{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.condition == nil || lease.closing || lease.closed || nilInterface(lease.provider) {
		return ProviderDescriptor{}
	}
	return lease.descriptor
}

// Capabilities returns an owned snapshot of the exact provider-neutral media
// contract bound when this lease was opened.
func (lease *ProviderLease) Capabilities() ProviderCapabilities {
	if lease == nil {
		return ProviderCapabilities{}
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.condition == nil || lease.closing || lease.closed || nilInterface(lease.provider) {
		return ProviderCapabilities{}
	}
	return lease.capabilities.Clone()
}

func (lease *ProviderLease) validateCapabilities(media []PreparedMedia) error {
	if lease == nil {
		return errors.New("review provider lease is nil")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.condition == nil || lease.closing || lease.closed || nilInterface(lease.provider) {
		return errors.New("review provider lease is closed")
	}
	return lease.capabilities.supports(media)
}

func (lease *ProviderLease) acquire() (Provider, error) {
	if lease == nil {
		return nil, errors.New("review provider lease is nil")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.condition == nil || lease.closing || lease.closed || nilInterface(lease.provider) {
		return nil, errors.New("review provider lease is closed")
	}
	lease.active++
	return lease.provider, nil
}

func (lease *ProviderLease) release() {
	lease.mu.Lock()
	if lease.condition == nil || lease.active <= 0 {
		lease.mu.Unlock()
		return
	}
	lease.active--
	if lease.active == 0 && lease.closing {
		lease.condition.Broadcast()
	}
	lease.mu.Unlock()
}

// Close prevents new evaluations, waits for active calls, and closes the
// provider exactly once. Concurrent Close calls receive the same result.
func (lease *ProviderLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	if lease.condition == nil {
		lease.condition = sync.NewCond(&lease.mu)
	}
	if lease.closed {
		err := lease.closeErr
		lease.mu.Unlock()
		return err
	}
	if lease.closing {
		for !lease.closed {
			lease.condition.Wait()
		}
		err := lease.closeErr
		lease.mu.Unlock()
		return err
	}
	lease.closing = true
	for lease.active > 0 {
		lease.condition.Wait()
	}
	provider := lease.provider
	lease.mu.Unlock()

	closeErr := closeProvider(provider)
	lease.mu.Lock()
	lease.provider = nil
	lease.closeErr = closeErr
	lease.closed = true
	lease.condition.Broadcast()
	lease.mu.Unlock()
	return closeErr
}

func validateLeaseProvenance(
	descriptor ProviderDescriptor, implementation, configuration []byte,
) error {
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("review provider lease descriptor: %w", err)
	}
	if len(implementation) == 0 || len(implementation) > 1<<20 ||
		digest(implementation) != descriptor.Implementation.SHA256 {
		return errors.New("review provider lease implementation bytes drifted")
	}
	canonicalConfiguration, err := canonicalJSON(configuration, 1<<20)
	if err != nil || !bytes.Equal(canonicalConfiguration, configuration) ||
		digest(configuration) != descriptor.ConfigurationSHA256 {
		return errors.New("review provider lease configuration bytes drifted")
	}
	return nil
}

func closeProvider(provider Provider) error {
	if nilInterface(provider) {
		return nil
	}
	return provider.Close()
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
