package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/plugin"
)

type Limits struct {
	MaxSources               uint16 `json:"max_sources"`
	MaxSeries                uint16 `json:"max_series"`
	MaxDimensionsPerSeries   uint8  `json:"max_dimensions_per_series"`
	MaxPendingSpans          uint16 `json:"max_pending_spans"`
	MaxObservationsPerSeries uint32 `json:"max_observations_per_series"`
	MaxArtifactBytes         uint32 `json:"max_artifact_bytes"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxSources: MaxSources, MaxSeries: MaxSeries,
		MaxDimensionsPerSeries:   MaxDimensionsPerSeries,
		MaxPendingSpans:          MaxPendingSpans,
		MaxObservationsPerSeries: MaxObservationsPerSeries,
		MaxArtifactBytes:         MaxSnapshotBytes,
	}
}

func (limits Limits) validate() error {
	checks := []struct {
		name string
		got  uint64
		max  uint64
	}{
		{"max_sources", uint64(limits.MaxSources), uint64(MaxSources)},
		{"max_series", uint64(limits.MaxSeries), uint64(MaxSeries)},
		{"max_dimensions_per_series", uint64(limits.MaxDimensionsPerSeries), uint64(MaxDimensionsPerSeries)},
		{"max_pending_spans", uint64(limits.MaxPendingSpans), uint64(MaxPendingSpans)},
		{"max_observations_per_series", uint64(limits.MaxObservationsPerSeries), uint64(MaxObservationsPerSeries)},
		{"max_artifact_bytes", uint64(limits.MaxArtifactBytes), uint64(MaxSnapshotBytes)},
	}
	for _, check := range checks {
		if check.got == 0 || check.got > check.max {
			return fmt.Errorf("performance limit %s is %d, want 1..%d", check.name, check.got, check.max)
		}
	}
	return nil
}

type Sampling struct {
	Numerator   uint32 `json:"numerator"`
	Denominator uint32 `json:"denominator"`
	Seed        uint64 `json:"seed"`
}

type Configuration struct {
	FormatVersion uint64            `json:"format_version"`
	Mode          ObservabilityMode `json:"mode"`
	Sampling      Sampling          `json:"sampling"`
	Limits        Limits            `json:"limits"`
	Fingerprint   string            `json:"fingerprint"`
}

func DefaultConfiguration(mode ObservabilityMode) (Configuration, error) {
	return FreezeConfiguration(Configuration{
		FormatVersion: ConfigurationVersion,
		Mode:          mode,
		Sampling:      Sampling{Numerator: 1, Denominator: 1},
		Limits:        DefaultLimits(),
	})
}

func FreezeConfiguration(source Configuration) (Configuration, error) {
	result := source
	result.Fingerprint = ""
	if err := result.validateStructure(); err != nil {
		return Configuration{}, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return Configuration{}, fmt.Errorf("encode performance configuration: %w", err)
	}
	result.Fingerprint = digest(payload)
	return result, nil
}

func (configuration Configuration) Validate() error {
	want, err := FreezeConfiguration(configuration)
	if err != nil {
		return err
	}
	if configuration.Fingerprint != want.Fingerprint {
		return fmt.Errorf("performance configuration fingerprint is %q, want %q", configuration.Fingerprint, want.Fingerprint)
	}
	return nil
}

func (configuration Configuration) validateStructure() error {
	if configuration.FormatVersion != ConfigurationVersion {
		return fmt.Errorf("unsupported performance configuration format %d", configuration.FormatVersion)
	}
	switch configuration.Mode {
	case ObservabilitySampled, ObservabilityRelease:
	case ObservabilityDisabled:
		return errors.New("disabled observability must omit the collector and probes")
	default:
		return fmt.Errorf("unknown observability mode %q", configuration.Mode)
	}
	if configuration.Sampling.Denominator == 0 || configuration.Sampling.Denominator > 1<<20 ||
		configuration.Sampling.Numerator == 0 || configuration.Sampling.Numerator > configuration.Sampling.Denominator {
		return fmt.Errorf("invalid deterministic sampling ratio %d/%d",
			configuration.Sampling.Numerator, configuration.Sampling.Denominator)
	}
	return configuration.Limits.validate()
}

type ClockIdentity struct {
	Domain                ClockDomain     `json:"domain"`
	ResolutionNS          uint64          `json:"resolution_ns"`
	Provenance            ClockProvenance `json:"provenance"`
	CrossHostSynchronized bool            `json:"cross_host_synchronized"`
	CrossHostErrorBoundNS uint64          `json:"cross_host_error_bound_ns,omitempty"`
}

type SourceIdentity struct {
	EntryFingerprint     string          `json:"entry_fingerprint"`
	Descriptor           plugin.Identity `json:"descriptor"`
	ImplementationDigest string          `json:"implementation_digest"`
	ConfigurationDigest  string          `json:"configuration_digest"`
}

// RuntimeBinding is supplied by the client composition runtime. Generation is
// intentionally absent: MountScope injects it, and recording callers never
// receive a setter for any field in the resulting identity.
type RuntimeBinding struct {
	EvidenceClass        EvidenceClass    `json:"evidence_class"`
	Platform             ClientPlatform   `json:"platform"`
	Clock                ClockIdentity    `json:"clock"`
	ProfileFingerprint   string           `json:"profile_fingerprint"`
	LockFingerprint      string           `json:"lock_fingerprint"`
	PlanFingerprint      string           `json:"plan_fingerprint"`
	ManifestFingerprint  string           `json:"manifest_fingerprint"`
	Collector            plugin.Identity  `json:"collector"`
	ImplementationDigest string           `json:"implementation_digest"`
	ConfigurationDigest  string           `json:"configuration_digest"`
	FixtureDigest        string           `json:"fixture_digest,omitempty"`
	Sources              []SourceIdentity `json:"sources"`
}

type RuntimeIdentity struct {
	EvidenceClass        EvidenceClass     `json:"evidence_class"`
	Platform             ClientPlatform    `json:"platform"`
	Clock                ClockIdentity     `json:"clock"`
	ProfileFingerprint   string            `json:"profile_fingerprint"`
	LockFingerprint      string            `json:"lock_fingerprint"`
	PlanFingerprint      string            `json:"plan_fingerprint"`
	ManifestFingerprint  string            `json:"manifest_fingerprint"`
	Collector            plugin.Identity   `json:"collector"`
	ImplementationDigest string            `json:"implementation_digest"`
	ConfigurationDigest  string            `json:"configuration_digest"`
	FixtureDigest        string            `json:"fixture_digest,omitempty"`
	ObservabilityMode    ObservabilityMode `json:"observability_mode"`
	Generation           uint64            `json:"generation"`
	Sources              []SourceIdentity  `json:"sources"`
}

func (identity RuntimeIdentity) Clone() RuntimeIdentity {
	identity.Sources = slices.Clone(identity.Sources)
	return identity
}

func canonicalizeBinding(source RuntimeBinding) RuntimeBinding {
	result := source
	result.Sources = slices.Clone(source.Sources)
	sort.Slice(result.Sources, func(left, right int) bool {
		return compareSources(result.Sources[left], result.Sources[right]) < 0
	})
	return result
}

func compareSources(left, right SourceIdentity) int {
	leftKey := left.EntryFingerprint + "\x00" + left.Descriptor.Name + "\x00" + left.Descriptor.Digest + "\x00" +
		left.ImplementationDigest + "\x00" + left.ConfigurationDigest
	rightKey := right.EntryFingerprint + "\x00" + right.Descriptor.Name + "\x00" + right.Descriptor.Digest + "\x00" +
		right.ImplementationDigest + "\x00" + right.ConfigurationDigest
	return strings.Compare(leftKey, rightKey)
}

func validateBinding(binding RuntimeBinding, configuration Configuration) error {
	switch binding.EvidenceClass {
	case EvidenceDeterministicFixture:
		if binding.FixtureDigest == "" {
			return errors.New("deterministic fixture evidence requires a fixture digest")
		}
	case EvidenceBrowserRuntime:
		if binding.Platform != PlatformBrowser {
			return errors.New("browser runtime evidence requires the browser platform")
		}
		if binding.FixtureDigest != "" {
			return errors.New("browser runtime evidence cannot claim a fixture identity")
		}
	case EvidenceMacOSNative:
		if binding.Platform != PlatformMacOS {
			return errors.New("macos native evidence requires the macos platform")
		}
		if binding.FixtureDigest != "" {
			return errors.New("macos native evidence cannot claim a fixture identity")
		}
	default:
		return fmt.Errorf("unknown evidence class %q", binding.EvidenceClass)
	}
	switch binding.Platform {
	case PlatformHeadless, PlatformBrowser, PlatformMacOS:
	default:
		return fmt.Errorf("unknown client platform %q", binding.Platform)
	}
	if err := validateClock(binding.Clock, binding.EvidenceClass); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"profile fingerprint":   binding.ProfileFingerprint,
		"lock fingerprint":      binding.LockFingerprint,
		"plan fingerprint":      binding.PlanFingerprint,
		"manifest fingerprint":  binding.ManifestFingerprint,
		"implementation digest": binding.ImplementationDigest,
		"configuration digest":  binding.ConfigurationDigest,
	} {
		if err := validateDigest(name, value); err != nil {
			return err
		}
	}
	if binding.FixtureDigest != "" {
		if err := validateDigest("fixture digest", binding.FixtureDigest); err != nil {
			return err
		}
	}
	if err := plugin.ValidateIdentity(binding.Collector); err != nil {
		return fmt.Errorf("collector identity: %w", err)
	}
	if binding.ConfigurationDigest != configuration.Fingerprint {
		return fmt.Errorf("runtime configuration identity is %s, selected configuration is %s",
			binding.ConfigurationDigest, configuration.Fingerprint)
	}
	if len(binding.Sources) == 0 || len(binding.Sources) > int(configuration.Limits.MaxSources) {
		return fmt.Errorf("runtime binding has %d sources, want 1..%d",
			len(binding.Sources), configuration.Limits.MaxSources)
	}
	for index, source := range binding.Sources {
		if err := validateSource(source); err != nil {
			return fmt.Errorf("runtime source %d: %w", index, err)
		}
		if index > 0 && compareSources(binding.Sources[index-1], source) >= 0 {
			return errors.New("runtime sources are duplicate or not in canonical order")
		}
	}
	return nil
}

func validateClock(clock ClockIdentity, class EvidenceClass) error {
	switch clock.Domain {
	case ClockVirtualInteger, ClockProcessMonotonic, ClockBrowserMonotonic, ClockMachContinuous:
	default:
		return fmt.Errorf("unknown clock domain %q", clock.Domain)
	}
	switch clock.Provenance {
	case ClockFromFixture, ClockFromGoMonotonic, ClockFromBrowserAPI, ClockFromMachClock, ClockFromSynchronized:
	default:
		return fmt.Errorf("unknown clock provenance %q", clock.Provenance)
	}
	if clock.ResolutionNS == 0 || clock.ResolutionNS > MaxDurationNS {
		return fmt.Errorf("clock resolution is %d, want 1..%d", clock.ResolutionNS, MaxDurationNS)
	}
	if clock.CrossHostErrorBoundNS > MaxDurationNS {
		return fmt.Errorf("cross-host clock error bound exceeds %d", MaxDurationNS)
	}
	if !clock.CrossHostSynchronized && clock.CrossHostErrorBoundNS != 0 {
		return errors.New("cross-host clock error bound requires synchronized clocks")
	}
	if clock.Provenance == ClockFromSynchronized && !clock.CrossHostSynchronized {
		return errors.New("synchronized clock provenance requires synchronized clocks")
	}
	if clock.CrossHostSynchronized && clock.Provenance != ClockFromSynchronized &&
		clock.Provenance != ClockFromFixture {
		return errors.New("cross-host synchronization requires synchronized or fixture provenance")
	}
	if class == EvidenceDeterministicFixture &&
		(clock.Domain != ClockVirtualInteger || clock.Provenance != ClockFromFixture) {
		return errors.New("deterministic fixture evidence requires the virtual fixture clock")
	}
	return nil
}

func validateSource(source SourceIdentity) error {
	if err := validateDigest("source entry fingerprint", source.EntryFingerprint); err != nil {
		return err
	}
	if err := plugin.ValidateIdentity(source.Descriptor); err != nil {
		return fmt.Errorf("source descriptor: %w", err)
	}
	if err := validateDigest("source implementation digest", source.ImplementationDigest); err != nil {
		return err
	}
	return validateDigest("source configuration digest", source.ConfigurationDigest)
}

func validateRuntimeIdentity(identity RuntimeIdentity, configuration Configuration) error {
	if identity.Generation == 0 {
		return errors.New("runtime generation must be positive")
	}
	if identity.ObservabilityMode != configuration.Mode {
		return errors.New("runtime observability mode does not match selected configuration")
	}
	return validateBinding(RuntimeBinding{
		EvidenceClass: identity.EvidenceClass, Platform: identity.Platform, Clock: identity.Clock,
		ProfileFingerprint: identity.ProfileFingerprint, LockFingerprint: identity.LockFingerprint,
		PlanFingerprint: identity.PlanFingerprint, ManifestFingerprint: identity.ManifestFingerprint,
		Collector: identity.Collector, ImplementationDigest: identity.ImplementationDigest,
		ConfigurationDigest: identity.ConfigurationDigest, FixtureDigest: identity.FixtureDigest,
		Sources: identity.Sources,
	}, configuration)
}

func validateDigest(name, value string) error {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") ||
		value != strings.ToLower(value) {
		return fmt.Errorf("%s has invalid SHA-256 digest %q", name, value)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil {
		return fmt.Errorf("%s has invalid SHA-256 digest %q", name, value)
	}
	return nil
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type MountScope struct {
	mu         sync.Mutex
	generation uint64
	active     *Provider
}

func NewMountScope() *MountScope { return &MountScope{} }

func (scope *MountScope) Mount(binding RuntimeBinding, configuration Configuration) (*Provider, error) {
	if scope == nil {
		return nil, errors.New("performance mount scope is nil")
	}
	if err := configuration.Validate(); err != nil {
		return nil, fmt.Errorf("mount performance collector: %w", err)
	}
	binding = canonicalizeBinding(binding)
	if err := validateBinding(binding, configuration); err != nil {
		return nil, fmt.Errorf("mount performance collector: %w", err)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.active != nil && scope.active.isActive() {
		return nil, errors.New("performance collector is already mounted in this scope")
	}
	if scope.generation == math.MaxUint64 {
		return nil, errors.New("performance collector generation overflow")
	}
	scope.generation++
	identity := RuntimeIdentity{
		EvidenceClass: binding.EvidenceClass, Platform: binding.Platform, Clock: binding.Clock,
		ProfileFingerprint: binding.ProfileFingerprint, LockFingerprint: binding.LockFingerprint,
		PlanFingerprint: binding.PlanFingerprint, ManifestFingerprint: binding.ManifestFingerprint,
		Collector: binding.Collector, ImplementationDigest: binding.ImplementationDigest,
		ConfigurationDigest: binding.ConfigurationDigest, FixtureDigest: binding.FixtureDigest,
		ObservabilityMode: configuration.Mode, Generation: scope.generation,
		Sources: slices.Clone(binding.Sources),
	}
	provider := newProvider(identity, configuration)
	if err := provider.enforceArtifactBoundLocked(); err != nil {
		return nil, fmt.Errorf("mount performance collector: %w", err)
	}
	if _, err := provider.snapshot(); err != nil {
		return nil, fmt.Errorf("mount performance collector: %w", err)
	}
	scope.active = provider
	return provider, nil
}

// Lose invalidates every recorder and pending span created by provider. It is
// idempotent for the provider that was most recently lost. A later Mount gets
// a fresh generation and an entirely separate aggregate.
func (scope *MountScope) Lose(provider *Provider) error {
	if scope == nil {
		return errors.New("performance mount scope is nil")
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if provider == nil {
		return errors.New("performance provider is nil")
	}
	// A disposal callback from an older generation may run after a remount. It
	// is a no-op and must never reach the new provider.
	if !provider.isActive() {
		return nil
	}
	if scope.active == nil {
		return errors.New("performance provider does not belong to this mount scope")
	}
	if scope.active != provider {
		return errors.New("performance provider does not match the active generation")
	}
	provider.lose()
	scope.active = nil
	return nil
}
