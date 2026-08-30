package review

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestProviderCapabilitiesAreExactOwnedAndEnforcedBeforeReview(t *testing.T) {
	for name, capabilities := range map[string]ProviderCapabilities{
		"unsorted": {
			MediaTypes:        []string{"image/png", "audio/wav"},
			MaximumMediaCount: 1, MaximumMediaBytes: 1024,
		},
		"duplicate": {
			MediaTypes:        []string{"audio/wav", "audio/wav"},
			MaximumMediaCount: 1, MaximumMediaBytes: 1024,
		},
		"unsupported": {
			MediaTypes:        []string{"audio/mpeg"},
			MaximumMediaCount: 1, MaximumMediaBytes: 1024,
		},
		"oversized count": {
			MediaTypes:        []string{"audio/wav"},
			MaximumMediaCount: maximumMediaCount + 1, MaximumMediaBytes: 1024,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := capabilities.Validate(); err == nil {
				t.Fatal("invalid provider capabilities passed validation")
			}
		})
	}

	request, _ := testRequest(t)
	descriptor := testDescriptor("capability-model")
	capabilities := ProviderCapabilities{
		MediaTypes: []string{"image/png"}, MaximumMediaCount: 1,
		MaximumMediaBytes: maximumMediaBytes,
	}
	capabilitiesSHA256, err := capabilities.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	descriptor.CapabilitiesSHA256 = capabilitiesSHA256
	provider := &testProvider{descriptor: descriptor, capabilities: capabilities}
	registration := testRegistration(
		"provider", descriptor, func(context.Context) (Provider, error) { return provider, nil },
	)
	registration.Capabilities = capabilities.Clone()
	registry, err := NewRegistry([]Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	catalog := registry.Catalog()
	if len(catalog) != 1 || len(catalog[0].Capabilities.MediaTypes) != 1 {
		t.Fatalf("provider capability catalog = %+v", catalog)
	}
	catalog[0].Capabilities.MediaTypes[0] = "audio/wav"
	if got := registry.Catalog()[0].Capabilities.MediaTypes[0]; got != "image/png" {
		t.Fatalf("catalog capability aliases caller storage: %q", got)
	}
	lease, err := registry.Open(t.Context(), "provider")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	snapshot := lease.Capabilities()
	snapshot.MediaTypes[0] = "audio/wav"
	if got := lease.Capabilities().MediaTypes[0]; got != "image/png" {
		t.Fatalf("lease capability aliases caller storage: %q", got)
	}
	if _, err := Evaluate(t.Context(), lease, request); err == nil ||
		!strings.Contains(err.Error(), "unsupported by the provider") {
		t.Fatalf("Evaluate() capability error = %v", err)
	}
	if provider.reviewCalls.Load() != 0 {
		t.Fatalf("unsupported media crossed the provider boundary %d times", provider.reviewCalls.Load())
	}
}

func TestRegistryBindsCapabilitiesToDescriptorIdentity(t *testing.T) {
	descriptor := testDescriptor("capability-identity-model")
	registration := testRegistration(
		"provider", descriptor,
		func(context.Context) (Provider, error) {
			return &testProvider{descriptor: descriptor}, nil
		},
	)
	registration.Capabilities.MaximumMediaBytes--
	if _, err := NewRegistry([]Registration{registration}); err == nil ||
		!strings.Contains(err.Error(), "capabilities identity drifted") {
		t.Fatalf("NewRegistry() capability identity error = %v", err)
	}
}

func TestRegistryBoundsCountAggregateAndLiveCapabilityDrift(t *testing.T) {
	tooMany := make([]Registration, maximumProviderRegistrations+1)
	if _, err := NewRegistry(tooMany); err == nil || !strings.Contains(err.Error(), "registrations") {
		t.Fatalf("NewRegistry() registration-count error = %v", err)
	}

	descriptor := testDescriptor("registry-aggregate-model")
	largeImplementation := []byte(strings.Repeat("i", maximumProviderImplementationBytes))
	descriptor.Implementation.SHA256 = digest(largeImplementation)
	registrations := make([]Registration, 0, maximumProviderRegistryBytes/len(largeImplementation)+2)
	for index := 0; index < cap(registrations); index++ {
		registrations = append(registrations, Registration{
			Name: fmt.Sprintf("provider-%03d", index), Descriptor: descriptor,
			Capabilities: testProviderCapabilities(), Implementation: largeImplementation,
			Configuration: []byte(`{"name":"registry-aggregate-model"}`),
			Factory: func(context.Context) (Provider, error) {
				return &testProvider{descriptor: descriptor}, nil
			},
		})
	}
	if _, err := NewRegistry(registrations); err == nil ||
		!strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("NewRegistry() aggregate-bound error = %v", err)
	}
}

func TestRegistryRejectsLiveCapabilityDriftBeforeEvaluation(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("capability-drift-model")
	provider := &testProvider{descriptor: descriptor}
	registry, err := NewRegistry([]Registration{testRegistration(
		"provider", descriptor, func(context.Context) (Provider, error) { return provider, nil },
	)})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(t.Context(), "provider")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	drift := testProviderCapabilities()
	drift.MaximumMediaBytes--
	provider.nextCapabilities = &drift
	if _, err := Evaluate(t.Context(), lease, request); err == nil ||
		!strings.Contains(err.Error(), "capabilities changed before evaluation") {
		t.Fatalf("Evaluate() live capability drift error = %v", err)
	}
	if provider.reviewCalls.Load() != 0 {
		t.Fatalf("capability drift crossed review %d times", provider.reviewCalls.Load())
	}
}
