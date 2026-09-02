package element

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const DescriptorFormatVersion = 1

// Direction is the direction of a port relative to its element.
type Direction string

const (
	Input  Direction = "input"
	Output Direction = "output"
)

// Cardinality says whether a port is singular or is a homogeneous variadic
// group whose lanes are materialized from graph edges.
type Cardinality string

const (
	One      Cardinality = "one"
	Variadic Cardinality = "variadic"
)

// Port is one typed observation or action boundary of an element.
type Port struct {
	Name           string      `json:"name" yaml:"name"`
	Direction      Direction   `json:"direction" yaml:"direction"`
	Type           Type        `json:"type" yaml:"type"`
	Cardinality    Cardinality `json:"cardinality" yaml:"cardinality"`
	Required       bool        `json:"required,omitempty" yaml:"required,omitempty"`
	MinConnections int         `json:"min_connections,omitempty" yaml:"min_connections,omitempty"`
	LossAllowed    bool        `json:"loss_allowed,omitempty" yaml:"loss_allowed,omitempty"`
	DefaultDepth   int         `json:"default_depth,omitempty" yaml:"default_depth,omitempty"`
}

// Reaction describes activation independently of payload type. Context/state
// ports can update without appearing in Triggers; an interrupt is visible and
// addressed rather than a hidden method call.
type Reaction struct {
	Triggers       []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	SampledState   []string `json:"sampled_state,omitempty" yaml:"sampled_state,omitempty"`
	Interrupts     []string `json:"interrupts,omitempty" yaml:"interrupts,omitempty"`
	Outcomes       []string `json:"outcomes,omitempty" yaml:"outcomes,omitempty"`
	MaxConcurrency int      `json:"max_concurrency,omitempty" yaml:"max_concurrency,omitempty"`
	// BreaksCycles declares that this reaction provides an explicit causal
	// break (for example a seeded State or Delay) and can therefore make
	// progress without first consuming the graph cycle it participates in.
	BreaksCycles bool `json:"breaks_cycles,omitempty" yaml:"breaks_cycles,omitempty"`
}

// Dependency is a service or coeffect required while an element is mounted.
type Dependency struct {
	Name     string `json:"name" yaml:"name"`
	Optional bool   `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// Effect is a lifecycle-owned registration/resource or an external authority
// the element may request. External effects are not claimed to be reversible.
type Effect struct {
	Name       string `json:"name" yaml:"name"`
	External   bool   `json:"external,omitempty" yaml:"external,omitempty"`
	Authority  string `json:"authority,omitempty" yaml:"authority,omitempty"`
	Reversible bool   `json:"reversible,omitempty" yaml:"reversible,omitempty"`
}

// StateTransferCapabilities declares which state-transfer lifecycle operations
// an element implementation supports. It is intentionally separate from
// StateSchema: a schema describes state bytes, but does not imply that an
// implementation can safely capture, restore, or quiesce that state.
//
// Snapshot and Restore are independent because a predecessor and candidate may
// have asymmetric roles in an explicitly migrated transition. Quiesce requires
// Snapshot because stopping mutation without capturing state is not a state
// transfer capability.
type StateTransferCapabilities struct {
	Snapshot bool `json:"snapshot,omitempty" yaml:"snapshot,omitempty"`
	Restore  bool `json:"restore,omitempty" yaml:"restore,omitempty"`
	Quiesce  bool `json:"quiesce,omitempty" yaml:"quiesce,omitempty"`
}

// Validate rejects an empty or internally contradictory capability contract.
// The owning descriptor or Graph IR node separately verifies StateSchema.
func (capabilities StateTransferCapabilities) Validate() error {
	if !capabilities.Snapshot && !capabilities.Restore && !capabilities.Quiesce {
		return errors.New("state transfer declares no capabilities")
	}
	if capabilities.Quiesce && !capabilities.Snapshot {
		return errors.New("state transfer quiescence requires snapshot capability")
	}
	return nil
}

// Clone returns an independently owned optional capability contract.
func (capabilities *StateTransferCapabilities) Clone() *StateTransferCapabilities {
	if capabilities == nil {
		return nil
	}
	result := *capabilities
	return &result
}

// Descriptor is the immutable, language-neutral composition contract of an
// element implementation. Human graph source refers to Name only; a generated
// lockfile and Graph IR retain Revision and Digest.
type Descriptor struct {
	FormatVersion uint64   `json:"format_version" yaml:"format_version"`
	Name          string   `json:"name" yaml:"name"`
	Revision      uint64   `json:"revision" yaml:"revision"`
	Generics      []string `json:"generics,omitempty" yaml:"generics,omitempty"`
	Ports         []Port   `json:"ports" yaml:"ports"`
	Reaction      Reaction `json:"reaction,omitempty" yaml:"reaction,omitempty"`
	StateSchema   string   `json:"state_schema,omitempty" yaml:"state_schema,omitempty"`
	// StateTransfer is nil for schema-only state. Its optional representation
	// keeps descriptors authored before this contract byte-for-byte and
	// digest-compatible while making transfer support an explicit opt-in.
	StateTransfer *StateTransferCapabilities `json:"state_transfer,omitempty" yaml:"state_transfer,omitempty"`
	ConfigSchema  string                     `json:"config_schema,omitempty" yaml:"config_schema,omitempty"`
	Dependencies  []Dependency               `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Effects       []Effect                   `json:"effects,omitempty" yaml:"effects,omitempty"`
	// CompositeFingerprint is set only for a subgraph descriptor. It binds the
	// exported contract identity to the exact frozen child Graph IR rather than
	// allowing a body change behind an unchanged boundary signature.
	CompositeFingerprint string `json:"composite_fingerprint,omitempty" yaml:"composite_fingerprint,omitempty"`
}

var (
	elementNamePattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$`)
	portNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	contractNamePattern = regexp.MustCompile(
		`^[A-Za-z][A-Za-z0-9_.:/-]*$`,
	)
)

// Identity is the exact descriptor selected by resolution.
type Identity struct {
	Name     string `json:"name" yaml:"name"`
	Revision uint64 `json:"revision" yaml:"revision"`
	Digest   string `json:"digest" yaml:"digest"`
}

// Validate checks a complete descriptor without consulting a registry.
func (descriptor Descriptor) Validate() error {
	if descriptor.FormatVersion != DescriptorFormatVersion {
		return fmt.Errorf("element %q uses descriptor format %d, want %d",
			descriptor.Name, descriptor.FormatVersion, DescriptorFormatVersion)
	}
	if !elementNamePattern.MatchString(descriptor.Name) {
		return fmt.Errorf("invalid element name %q", descriptor.Name)
	}
	if descriptor.Revision == 0 {
		return fmt.Errorf("element %s revision must be positive", descriptor.Name)
	}
	genericSet := make(map[string]struct{}, len(descriptor.Generics))
	for _, generic := range descriptor.Generics {
		if !variablePattern.MatchString(generic) {
			return fmt.Errorf("element %s has invalid generic %q", descriptor.Name, generic)
		}
		if _, exists := genericSet[generic]; exists {
			return fmt.Errorf("element %s repeats generic %q", descriptor.Name, generic)
		}
		genericSet[generic] = struct{}{}
	}
	ports := make(map[string]Port, len(descriptor.Ports))
	for _, port := range descriptor.Ports {
		if !portNamePattern.MatchString(port.Name) {
			return fmt.Errorf("element %s has invalid port name %q", descriptor.Name, port.Name)
		}
		if _, exists := ports[port.Name]; exists {
			return fmt.Errorf("element %s repeats port %q", descriptor.Name, port.Name)
		}
		ports[port.Name] = port
		switch port.Direction {
		case Input, Output:
		default:
			return fmt.Errorf("element %s port %s has invalid direction %q",
				descriptor.Name, port.Name, port.Direction)
		}
		switch port.Cardinality {
		case One:
			if port.MinConnections > 1 {
				return fmt.Errorf("element %s singular port %s cannot require %d connections",
					descriptor.Name, port.Name, port.MinConnections)
			}
		case Variadic:
			if port.MinConnections < 0 {
				return fmt.Errorf("element %s variadic port %s has negative minimum connections",
					descriptor.Name, port.Name)
			}
		default:
			return fmt.Errorf("element %s port %s has invalid cardinality %q",
				descriptor.Name, port.Name, port.Cardinality)
		}
		if port.DefaultDepth < 0 {
			return fmt.Errorf("element %s port %s has negative default depth",
				descriptor.Name, port.Name)
		}
		if err := port.Type.ValidatePort(genericSet); err != nil {
			return fmt.Errorf("element %s port %s: %w", descriptor.Name, port.Name, err)
		}
	}
	if len(ports) == 0 {
		return fmt.Errorf("element %s has no ports", descriptor.Name)
	}
	if err := validateReaction(descriptor.Name, descriptor.Reaction, ports); err != nil {
		return err
	}
	if err := validateNamedContracts(descriptor.Name, descriptor.Dependencies, descriptor.Effects); err != nil {
		return err
	}
	if descriptor.StateTransfer != nil {
		if err := descriptor.StateTransfer.Validate(); err != nil {
			return fmt.Errorf("element %s: %w", descriptor.Name, err)
		}
		if descriptor.StateSchema == "" {
			return fmt.Errorf("element %s state transfer requires a state schema", descriptor.Name)
		}
	}
	if descriptor.CompositeFingerprint != "" {
		if !strings.HasPrefix(descriptor.CompositeFingerprint, "sha256:") ||
			len(descriptor.CompositeFingerprint) != len("sha256:")+sha256.Size*2 {
			return fmt.Errorf("element %s has invalid composite fingerprint %q",
				descriptor.Name, descriptor.CompositeFingerprint)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(descriptor.CompositeFingerprint, "sha256:")); err != nil {
			return fmt.Errorf("element %s has invalid composite fingerprint: %w", descriptor.Name, err)
		}
	}
	return nil
}

func validateReaction(elementName string, reaction Reaction, ports map[string]Port) error {
	if reaction.MaxConcurrency < 0 {
		return fmt.Errorf("element %s has negative max concurrency", elementName)
	}
	groups := []struct {
		label string
		names []string
		want  Direction
	}{
		{label: "trigger", names: reaction.Triggers, want: Input},
		{label: "sampled state", names: reaction.SampledState, want: Input},
		{label: "interrupt", names: reaction.Interrupts, want: Input},
		{label: "outcome", names: reaction.Outcomes, want: Output},
	}
	seen := map[string]string{}
	for _, group := range groups {
		for _, name := range group.names {
			port, exists := ports[name]
			if !exists {
				return fmt.Errorf("element %s reaction names unknown %s port %q",
					elementName, group.label, name)
			}
			if port.Direction != group.want {
				return fmt.Errorf("element %s reaction %s port %q is %s, want %s",
					elementName, group.label, name, port.Direction, group.want)
			}
			if previous, duplicate := seen[name]; duplicate {
				return fmt.Errorf("element %s reaction port %q is both %s and %s",
					elementName, name, previous, group.label)
			}
			seen[name] = group.label
		}
	}
	return nil
}

func validateNamedContracts(elementName string, dependencies []Dependency, effects []Effect) error {
	seenDependencies := map[string]struct{}{}
	for _, dependency := range dependencies {
		if !contractNamePattern.MatchString(dependency.Name) {
			return fmt.Errorf("element %s has invalid dependency %q", elementName, dependency.Name)
		}
		if _, exists := seenDependencies[dependency.Name]; exists {
			return fmt.Errorf("element %s repeats dependency %q", elementName, dependency.Name)
		}
		seenDependencies[dependency.Name] = struct{}{}
	}
	seenEffects := map[string]struct{}{}
	for _, effect := range effects {
		if !contractNamePattern.MatchString(effect.Name) {
			return fmt.Errorf("element %s has invalid effect %q", elementName, effect.Name)
		}
		if _, exists := seenEffects[effect.Name]; exists {
			return fmt.Errorf("element %s repeats effect %q", elementName, effect.Name)
		}
		seenEffects[effect.Name] = struct{}{}
		if effect.Reversible && effect.External {
			return fmt.Errorf("element %s effect %q cannot claim an external effect is reversible",
				elementName, effect.Name)
		}
		if effect.External && effect.Authority == "" {
			return fmt.Errorf("element %s external effect %q must name its required authority type",
				elementName, effect.Name)
		}
		if effect.Authority != "" && !contractNamePattern.MatchString(effect.Authority) {
			return fmt.Errorf("element %s effect %q has invalid authority %q",
				elementName, effect.Name, effect.Authority)
		}
	}
	return nil
}

// Port returns the named port.
func (descriptor Descriptor) Port(name string) (Port, bool) {
	for _, port := range descriptor.Ports {
		if port.Name == name {
			return port, true
		}
	}
	return Port{}, false
}

// Clone returns a recursively independent descriptor. Catalogs use this to
// preserve the immutability promised by descriptor identities even when a
// caller later reuses or mutates its construction slices.
func (descriptor Descriptor) Clone() Descriptor {
	result := descriptor
	result.Generics = slices.Clone(descriptor.Generics)
	result.Ports = slices.Clone(descriptor.Ports)
	for index := range result.Ports {
		result.Ports[index].Type = result.Ports[index].Type.Clone()
	}
	result.Reaction.Triggers = slices.Clone(descriptor.Reaction.Triggers)
	result.Reaction.SampledState = slices.Clone(descriptor.Reaction.SampledState)
	result.Reaction.Interrupts = slices.Clone(descriptor.Reaction.Interrupts)
	result.Reaction.Outcomes = slices.Clone(descriptor.Reaction.Outcomes)
	result.StateTransfer = descriptor.StateTransfer.Clone()
	result.Dependencies = slices.Clone(descriptor.Dependencies)
	result.Effects = slices.Clone(descriptor.Effects)
	return result
}

// Digest returns a deterministic content digest for a valid descriptor.
func (descriptor Descriptor) Digest() (string, error) {
	if err := descriptor.Validate(); err != nil {
		return "", err
	}
	normalized := descriptor.normalized()
	payload, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode element %s descriptor: %w", descriptor.Name, err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// Identity returns the exact, content-addressed descriptor identity.
func (descriptor Descriptor) Identity() (Identity, error) {
	digest, err := descriptor.Digest()
	if err != nil {
		return Identity{}, err
	}
	return Identity{Name: descriptor.Name, Revision: descriptor.Revision, Digest: digest}, nil
}

// Canonical returns a validated, recursively independent descriptor with all
// declaration-order-insensitive collections sorted. It is useful when a
// runtime verifies that Graph IR did not alter the contract behind a digest.
func (descriptor Descriptor) Canonical() (Descriptor, error) {
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	return descriptor.normalized(), nil
}

func (descriptor Descriptor) normalized() Descriptor {
	result := descriptor.Clone()
	sort.Strings(result.Generics)
	sort.Slice(result.Ports, func(left, right int) bool {
		return result.Ports[left].Name < result.Ports[right].Name
	})
	result.Reaction.Triggers = sortedStrings(descriptor.Reaction.Triggers)
	result.Reaction.SampledState = sortedStrings(descriptor.Reaction.SampledState)
	result.Reaction.Interrupts = sortedStrings(descriptor.Reaction.Interrupts)
	result.Reaction.Outcomes = sortedStrings(descriptor.Reaction.Outcomes)
	sort.Slice(result.Dependencies, func(left, right int) bool {
		return result.Dependencies[left].Name < result.Dependencies[right].Name
	})
	sort.Slice(result.Effects, func(left, right int) bool {
		return result.Effects[left].Name < result.Effects[right].Name
	})
	return result
}

func sortedStrings(values []string) []string {
	result := slices.Clone(values)
	sort.Strings(result)
	return result
}

// ValidateIdentity checks the immutable fields supplied by a lockfile.
func ValidateIdentity(identity Identity) error {
	if !elementNamePattern.MatchString(identity.Name) {
		return fmt.Errorf("invalid locked element name %q", identity.Name)
	}
	if identity.Revision == 0 {
		return fmt.Errorf("locked element %s revision must be positive", identity.Name)
	}
	if !strings.HasPrefix(identity.Digest, "sha256:") || len(identity.Digest) != len("sha256:")+sha256.Size*2 {
		return fmt.Errorf("locked element %s has invalid digest %q", identity.Name, identity.Digest)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(identity.Digest, "sha256:")); err != nil {
		return fmt.Errorf("locked element %s has invalid digest: %w", identity.Name, err)
	}
	return nil
}
