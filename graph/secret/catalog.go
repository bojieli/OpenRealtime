// Package secret resolves secret:// references at mount time without placing
// credential values in topology, Graph IR, diagrams, or evidence artifacts.
package secret

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "openrealtime.ai/secrets/v1alpha1"

	maximumArtifactBytes = 4 << 20
	maximumEntries       = 65_536
	maximumStringBytes   = 64 << 10
	maximumSecretBytes   = 1 << 20
)

var referencePattern = regexp.MustCompile(`^secret://[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

type Document struct {
	APIVersion string             `json:"apiVersion" yaml:"apiVersion"`
	Catalog    string             `json:"catalog" yaml:"catalog"`
	Secrets    map[string]Binding `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

// Binding identifies a provider and provider-specific non-secret locator.
// There is intentionally no inline/value/data field.
type Binding struct {
	Provider string `json:"provider" yaml:"provider"`
	Locator  string `json:"locator" yaml:"locator"`
}

// Provider returns newly allocated secret bytes owned by the caller. It must
// honor cancellation and must not cache values past its own documented
// lifecycle. Providers can wrap an environment, file-descriptor broker,
// operating-system keychain, KMS, or remote secret manager.
type Provider interface {
	Resolve(context.Context, string) ([]byte, error)
}

type registration struct {
	provider Provider
	artifact inspect.ArtifactIdentity
}

// Registry is mutable deployment assembly state. NewStore snapshots every
// selected registration so later registration cannot change a mounted graph.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]registration
}

func NewRegistry() *Registry { return &Registry{providers: make(map[string]registration)} }

func (registry *Registry) Register(
	name string, artifact inspect.ArtifactIdentity, provider Provider,
) error {
	if registry == nil {
		return errors.New("register secret provider: nil registry")
	}
	if err := canonicalString("secret provider name", name); err != nil {
		return err
	}
	if provider == nil {
		return fmt.Errorf("register secret provider %q: nil provider", name)
	}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("register secret provider %q artifact: %w", name, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.providers == nil {
		registry.providers = make(map[string]registration)
	}
	if _, duplicate := registry.providers[name]; duplicate {
		return fmt.Errorf("secret provider %q is already registered", name)
	}
	registry.providers[name] = registration{provider: provider, artifact: artifact}
	return nil
}

func (registry *Registry) lookup(name string) (registration, bool) {
	if registry == nil {
		return registration{}, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	value, found := registry.providers[name]
	return value, found
}

type entry struct {
	binding  Binding
	provider Provider
	artifact inspect.ArtifactIdentity
	digest   string
}

// Store is an immutable catalog of provider selections. Fingerprint covers
// references and non-secret provider locators, never the resolved values.
type Store struct {
	id          string
	fingerprint string
	entries     map[string]entry
}

func NewStore(document Document, registry *Registry) (*Store, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	if registry == nil {
		return nil, errors.New("create secret store: provider registry is required")
	}
	store := &Store{
		id: normalized.Catalog, entries: make(map[string]entry, len(normalized.Secrets)),
	}
	for reference, binding := range normalized.Secrets {
		registration, found := registry.lookup(binding.Provider)
		if !found {
			return nil, fmt.Errorf("secret %s selects unregistered provider %q", reference, binding.Provider)
		}
		digest, digestErr := bindingDigest(binding)
		if digestErr != nil {
			return nil, fmt.Errorf("secret %s: %w", reference, digestErr)
		}
		store.entries[reference] = entry{
			binding: binding, provider: registration.provider,
			artifact: registration.artifact, digest: digest,
		}
	}
	store.fingerprint, err = documentDigest(normalized)
	if err != nil {
		return nil, err
	}
	return store, nil
}

func (store *Store) Identity() inspect.ArtifactIdentity {
	if store == nil {
		return inspect.ArtifactIdentity{}
	}
	return inspect.ArtifactIdentity{
		ID: "secrets://" + store.id, Revision: APIVersion, Digest: store.fingerprint,
	}
}

// Resolution is safe management-plane evidence: it proves which catalog
// binding and provider implementation were selected, without exposing the
// locator or a digest of potentially low-entropy secret bytes.
type Resolution struct {
	Reference       string                   `json:"reference"`
	BindingDigest   string                   `json:"binding_digest"`
	Provider        string                   `json:"provider"`
	ProviderRuntime inspect.ArtifactIdentity `json:"provider_runtime"`
}

// Resolve obtains one fresh value. The returned handle must be closed. A
// provider error is wrapped without adding the locator to the message.
func (store *Store) Resolve(ctx context.Context, reference string) (*Value, Resolution, error) {
	if store == nil {
		return nil, Resolution{}, errors.New("resolve secret: nil store")
	}
	if ctx == nil {
		return nil, Resolution{}, errors.New("resolve secret: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, Resolution{}, err
	}
	if err := ValidateReference(reference); err != nil {
		return nil, Resolution{}, err
	}
	selected, found := store.entries[reference]
	if !found {
		return nil, Resolution{}, fmt.Errorf("secret reference %q is not in catalog %q", reference, store.id)
	}
	raw, err := selected.provider.Resolve(ctx, selected.binding.Locator)
	if err != nil {
		zero(raw)
		return nil, Resolution{}, fmt.Errorf("resolve secret %s through provider %q: %w",
			reference, selected.binding.Provider, err)
	}
	defer zero(raw)
	if len(raw) == 0 {
		return nil, Resolution{}, fmt.Errorf("resolve secret %s through provider %q: empty value",
			reference, selected.binding.Provider)
	}
	if len(raw) > maximumSecretBytes {
		return nil, Resolution{}, fmt.Errorf("resolve secret %s through provider %q: value exceeds %d bytes",
			reference, selected.binding.Provider, maximumSecretBytes)
	}
	value := &Value{bytes: append([]byte(nil), raw...)}
	resolution := Resolution{
		Reference: reference, BindingDigest: selected.digest,
		Provider: selected.binding.Provider, ProviderRuntime: selected.artifact,
	}
	return value, resolution, nil
}

// Value owns an erasable secret buffer. Bytes returns a caller-owned copy;
// callers are responsible for minimizing and erasing any copies they retain.
type Value struct {
	mu     sync.Mutex
	bytes  []byte
	closed bool
}

func (value *Value) Bytes() ([]byte, error) {
	if value == nil {
		return nil, errors.New("read secret value: nil handle")
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if value.closed {
		return nil, errors.New("read secret value: handle is closed")
	}
	return append([]byte(nil), value.bytes...), nil
}

func (value *Value) Close() error {
	if value == nil {
		return nil
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if !value.closed {
		zero(value.bytes)
		value.bytes = nil
		value.closed = true
	}
	return nil
}

// EnvironmentProvider is opt-in. It receives an injected lookup function so
// tests and embedders need not mutate process-global environment state.
type EnvironmentProvider struct {
	Lookup func(string) (string, bool)
}

func (provider EnvironmentProvider) Resolve(ctx context.Context, locator string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("resolve environment secret: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if provider.Lookup == nil {
		return nil, errors.New("environment secret provider has no lookup function")
	}
	if err := canonicalString("environment variable name", locator); err != nil {
		return nil, err
	}
	value, found := provider.Lookup(locator)
	if !found {
		return nil, fmt.Errorf("environment variable %q is not set", locator)
	}
	return []byte(value), nil
}

func ParseJSON(path string, source []byte) (Document, error) {
	if len(source) > maximumArtifactBytes {
		return Document{}, fmt.Errorf("parse secrets %s: artifact exceeds %d bytes", path, maximumArtifactBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return Document{}, fmt.Errorf("parse secrets %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("parse secrets %s: %w", path, err)
	}
	if err := requireEOF(decoder); err != nil {
		return Document{}, fmt.Errorf("parse secrets %s: %w", path, err)
	}
	return normalize(document)
}

func ParseYAML(path string, source []byte) (Document, error) {
	if len(source) > maximumArtifactBytes {
		return Document{}, fmt.Errorf("parse secrets %s: artifact exceeds %d bytes", path, maximumArtifactBytes)
	}
	var document Document
	if _, err := strictyaml.Decode(path, source, &document); err != nil {
		return Document{}, err
	}
	return normalize(document)
}

func MarshalJSON(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode secrets JSON: %w", err)
	}
	return append(payload, '\n'), nil
}

func MarshalYAML(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(normalized); err != nil {
		return nil, fmt.Errorf("encode secrets YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close secrets YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

// Validate checks a programmatically constructed document without resolving
// or reading any secret. ParseJSON and ParseYAML already perform this check.
func Validate(document Document) error {
	_, err := normalize(document)
	return err
}

func ValidateReference(reference string) error {
	if len(reference) > maximumStringBytes || !referencePattern.MatchString(reference) {
		return fmt.Errorf("secret reference %q must match secret://name[/name...]", reference)
	}
	return nil
}

func normalize(document Document) (Document, error) {
	if document.APIVersion != APIVersion {
		return Document{}, fmt.Errorf("secrets apiVersion must be %q, got %q", APIVersion, document.APIVersion)
	}
	if err := canonicalString("secret catalog ID", document.Catalog); err != nil {
		return Document{}, err
	}
	if len(document.Secrets) > maximumEntries {
		return Document{}, fmt.Errorf("secret catalog contains %d entries; maximum is %d",
			len(document.Secrets), maximumEntries)
	}
	result := Document{
		APIVersion: APIVersion, Catalog: document.Catalog,
		Secrets: make(map[string]Binding, len(document.Secrets)),
	}
	for reference, binding := range document.Secrets {
		if err := ValidateReference(reference); err != nil {
			return Document{}, err
		}
		if err := canonicalString("secret "+reference+" provider", binding.Provider); err != nil {
			return Document{}, err
		}
		if err := canonicalString("secret "+reference+" locator", binding.Locator); err != nil {
			return Document{}, err
		}
		result.Secrets[reference] = binding
	}
	return result, nil
}

func bindingDigest(binding Binding) (string, error) {
	payload, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func documentDigest(document Document) (string, error) {
	references := make([]string, 0, len(document.Secrets))
	for reference := range document.Secrets {
		references = append(references, reference)
	}
	sort.Strings(references)
	type item struct {
		Reference string  `json:"reference"`
		Binding   Binding `json:"binding"`
	}
	canonical := struct {
		APIVersion string `json:"apiVersion"`
		Catalog    string `json:"catalog"`
		Secrets    []item `json:"secrets"`
	}{APIVersion: APIVersion, Catalog: document.Catalog, Secrets: make([]item, 0, len(references))}
	for _, reference := range references {
		canonical.Secrets = append(canonical.Secrets, item{Reference: reference, Binding: document.Secrets[reference]})
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical secret catalog: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func canonicalString(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s cannot be empty", label)
	}
	if value != strings.TrimSpace(value) || len(value) > maximumStringBytes ||
		strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s is not canonical", label)
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
