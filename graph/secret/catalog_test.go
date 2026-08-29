package secret

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestStoreResolvesFreshErasableValuesWithoutAttestingBytes(t *testing.T) {
	document, err := ParseYAML("agent.secrets.yaml", []byte(`apiVersion: openrealtime.ai/secrets/v1alpha1
catalog: production
secrets:
  secret://providers/example/api-key:
    provider: environment
    locator: EXAMPLE_API_KEY
`))
	if err != nil {
		t.Fatalf("parse secrets: %v", err)
	}
	raw := []byte("highly-sensitive-value")
	provider := &recordingProvider{value: raw}
	registry := NewRegistry()
	if err := registry.Register("environment", testArtifact('1'), provider); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(document, registry)
	if err != nil {
		t.Fatal(err)
	}
	identity := store.Identity()
	if err := identity.Validate(); err != nil || identity.ID != "secrets://production" ||
		!strings.HasPrefix(identity.Digest, "sha256:") {
		t.Fatalf("store identity = %+v, err=%v", identity, err)
	}
	handle, resolution, err := store.Resolve(context.Background(), "secret://providers/example/api-key")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Reference != "secret://providers/example/api-key" ||
		resolution.Provider != "environment" || resolution.ProviderRuntime != testArtifact('1') ||
		!strings.HasPrefix(resolution.BindingDigest, "sha256:") {
		t.Fatalf("resolution = %+v", resolution)
	}
	got, err := handle.Bytes()
	if err != nil || string(got) != "highly-sensitive-value" {
		t.Fatalf("resolved bytes = %q, err=%v", got, err)
	}
	got[0] = 'X'
	again, err := handle.Bytes()
	if err != nil || string(again) != "highly-sensitive-value" {
		t.Fatal("Bytes returned an aliased secret buffer")
	}
	if provider.last == nil || !allZero(provider.last) {
		t.Fatal("provider-owned transfer buffer was not erased after copying")
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Bytes(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("read after close error = %v", err)
	}
	// Neither management-plane identity contains a value or a digest of it.
	if strings.Contains(identity.ID+identity.Digest+resolution.BindingDigest, "sensitive") {
		t.Fatal("secret bytes leaked into management-plane evidence")
	}
}

func TestCatalogIdentityIsFormatAndMapOrderStable(t *testing.T) {
	yamlDocument, err := ParseYAML("secrets.yaml", []byte(`apiVersion: openrealtime.ai/secrets/v1alpha1
catalog: test
secrets:
  secret://z/key:
    provider: env
    locator: Z_KEY
  secret://a/key:
    provider: env
    locator: A_KEY
`))
	if err != nil {
		t.Fatal(err)
	}
	jsonDocument, err := ParseJSON("secrets.json", []byte(`{
  "secrets": {
    "secret://a/key": {"locator":"A_KEY", "provider":"env"},
    "secret://z/key": {"provider":"env", "locator":"Z_KEY"}
  },
  "catalog": "test",
  "apiVersion": "openrealtime.ai/secrets/v1alpha1"
}`))
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.Register("env", testArtifact('2'), EnvironmentProvider{
		Lookup: func(string) (string, bool) { return "value", true },
	}); err != nil {
		t.Fatal(err)
	}
	left, err := NewStore(yamlDocument, registry)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewStore(jsonDocument, registry)
	if err != nil {
		t.Fatal(err)
	}
	if left.Identity() != right.Identity() {
		t.Fatalf("format-dependent secret catalog identity: %+v != %+v", left.Identity(), right.Identity())
	}
	changed := jsonDocument
	changed.Secrets = map[string]Binding{}
	for reference, binding := range jsonDocument.Secrets {
		changed.Secrets[reference] = binding
	}
	binding := changed.Secrets["secret://a/key"]
	binding.Locator = "A_KEY_ROTATED"
	changed.Secrets["secret://a/key"] = binding
	rotated, err := NewStore(changed, registry)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Identity().Digest == right.Identity().Digest {
		t.Fatal("secret binding change did not change private catalog identity")
	}
}

func TestSecretParsersRefuseInlineAndAmbiguousDocuments(t *testing.T) {
	tests := []struct {
		name   string
		parse  func(string, []byte) (Document, error)
		source string
		want   string
	}{
		{
			name: "inline field", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/secrets/v1alpha1\ncatalog: c\nsecrets:\n  secret://x/key:\n    provider: env\n    locator: X\n    value: plaintext\n",
			want:   "field value not found",
		},
		{
			name: "duplicate JSON", parse: ParseJSON,
			source: `{"apiVersion":"openrealtime.ai/secrets/v1alpha1","catalog":"a","catalog":"b"}`,
			want:   "duplicate",
		},
		{
			name: "alias", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/secrets/v1alpha1\ncatalog: c\nsecrets: &s {}\ncopy: *s\n",
			want:   "anchors and aliases",
		},
		{
			name: "invalid reference", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/secrets/v1alpha1\ncatalog: c\nsecrets:\n  https://x/key:\n    provider: env\n    locator: X\n",
			want:   "secret://",
		},
		{
			name: "path traversal", parse: ParseYAML,
			source: "apiVersion: openrealtime.ai/secrets/v1alpha1\ncatalog: c\nsecrets:\n  secret://x/../key:\n    provider: env\n    locator: X\n",
			want:   "secret://",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.parse("secrets", []byte(test.source)); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRegistryAndStoreRejectMutableOrMissingProviders(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register("env", inspect.ArtifactIdentity{ID: "latest", Revision: "latest"}, &recordingProvider{}); err == nil {
		t.Fatal("registered mutable provider identity")
	}
	if err := registry.Register("env", testArtifact('3'), &recordingProvider{value: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("env", testArtifact('3'), &recordingProvider{value: []byte("x")}); err == nil {
		t.Fatal("registered duplicate provider")
	}
	document := Document{APIVersion: APIVersion, Catalog: "c", Secrets: map[string]Binding{
		"secret://x/key": {Provider: "missing", Locator: "X"},
	}}
	if _, err := NewStore(document, registry); err == nil || !strings.Contains(err.Error(), "unregistered") {
		t.Fatalf("missing provider error = %v", err)
	}
}

func TestResolveBoundsCancellationEmptyAndOversizedValues(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider Provider
		ctx      func() context.Context
		want     string
	}{
		{"canceled", &recordingProvider{value: []byte("x")}, canceledContext, "canceled"},
		{"provider error", &recordingProvider{err: errors.New("unavailable")}, context.Background, "unavailable"},
		{"empty", &recordingProvider{}, context.Background, "empty value"},
		{"oversized", &recordingProvider{value: make([]byte, maximumSecretBytes+1)}, context.Background, "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry()
			if err := registry.Register("test", testArtifact('4'), test.provider); err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(Document{
				APIVersion: APIVersion, Catalog: "c", Secrets: map[string]Binding{
					"secret://x/key": {Provider: "test", Locator: "X"},
				},
			}, registry)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Resolve(test.ctx(), "secret://x/key"); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolve error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValueCloseAndReadAreRaceSafe(t *testing.T) {
	value := &Value{bytes: []byte("secret")}
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				_, _ = value.Bytes()
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		_ = value.Close()
	}()
	wait.Wait()
	if _, err := value.Bytes(); err == nil {
		t.Fatal("closed value remained readable")
	}
}

func TestEnvironmentProviderUsesInjectedLookup(t *testing.T) {
	provider := EnvironmentProvider{Lookup: func(name string) (string, bool) {
		return "from-" + name, name == "TOKEN"
	}}
	got, err := provider.Resolve(context.Background(), "TOKEN")
	if err != nil || string(got) != "from-TOKEN" {
		t.Fatalf("environment resolve = %q, err=%v", got, err)
	}
	if _, err := provider.Resolve(context.Background(), "ABSENT"); err == nil ||
		!strings.Contains(err.Error(), "not set") {
		t.Fatalf("absent environment error = %v", err)
	}
}

type recordingProvider struct {
	value []byte
	err   error
	last  []byte
}

func (provider *recordingProvider) Resolve(context.Context, string) ([]byte, error) {
	if provider.err != nil {
		return nil, provider.err
	}
	provider.last = append([]byte(nil), provider.value...)
	return provider.last, nil
}

func testArtifact(fill byte) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "provider://test/environment", Digest: "sha256:" + strings.Repeat(string(fill), 64),
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
