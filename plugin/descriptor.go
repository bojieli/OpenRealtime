// Package plugin defines immutable, language-neutral contracts for server,
// presentation-host, and client plugins.
//
// Product behavior lives behind these descriptors. A descriptor says what a
// plugin provides, requires, publishes, consumes, and is permitted to access;
// implementation selection, configuration values, deployment, and secrets
// remain separate artifacts.
package plugin

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

// Realm is the trust and lifecycle plane in which a plugin runs.
type Realm string

const (
	ServerRealm           Realm = "server"
	PresentationHostRealm Realm = "presentation_host"
	ClientRealm           Realm = "client"
)

// Identity pins one immutable plugin descriptor revision.
type Identity struct {
	Name     string `json:"name" yaml:"name"`
	Revision uint64 `json:"revision" yaml:"revision"`
	Digest   string `json:"digest" yaml:"digest"`
}

// Contract identifies one immutable service, event, or schema vocabulary.
// Name alone is used for dependency lookup; revision and digest prevent two
// implementations from silently assigning different meanings to that name.
type Contract struct {
	Name     string `json:"name" yaml:"name"`
	Revision uint64 `json:"revision" yaml:"revision"`
	Digest   string `json:"digest" yaml:"digest"`
}

// Requirement is one service contract a plugin consumes. Optional
// requirements do not delay mounting, but if a matching service is present it
// must still match this exact contract.
type Requirement struct {
	Contract Contract `json:"contract" yaml:"contract"`
	Optional bool     `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// EventDirection is relative to a plugin.
type EventDirection string

const (
	Publishes EventDirection = "publishes"
	Consumes  EventDirection = "consumes"
)

// Event declares a typed event extension point.
type Event struct {
	Contract  Contract       `json:"contract" yaml:"contract"`
	Direction EventDirection `json:"direction" yaml:"direction"`
}

// Permission is a ceiling, not a grant. Deployment policy and the authority
// host may narrow it further. Operations are declaration-order-insensitive.
type Permission struct {
	Kind       string   `json:"kind" yaml:"kind"`
	Resource   string   `json:"resource" yaml:"resource"`
	Operations []string `json:"operations" yaml:"operations"`
	// Authority names the typed authority contract required at the effect
	// boundary. It is empty for capabilities that cannot perform an external
	// effect, such as rendering an already-redacted snapshot.
	Authority string `json:"authority,omitempty" yaml:"authority,omitempty"`
}

// Asset pins one implementation-independent module or presentation resource.
// The deployment chooses where the bytes are served from; the descriptor
// records what bytes are part of this revision.
type Asset struct {
	Name      string `json:"name" yaml:"name"`
	MediaType string `json:"media_type" yaml:"media_type"`
	Digest    string `json:"digest" yaml:"digest"`
}

// Lifecycle declares bounded transition support. Zero timeouts select the
// runtime's bounded defaults rather than meaning unbounded.
type Lifecycle struct {
	QuiesceTimeoutMS uint64 `json:"quiesce_timeout_ms,omitempty" yaml:"quiesce_timeout_ms,omitempty"`
	DisposeTimeoutMS uint64 `json:"dispose_timeout_ms,omitempty" yaml:"dispose_timeout_ms,omitempty"`
	Snapshot         bool   `json:"snapshot,omitempty" yaml:"snapshot,omitempty"`
	Restore          bool   `json:"restore,omitempty" yaml:"restore,omitempty"`
}

// Descriptor is the complete immutable composition and permission contract of
// a plugin revision.
type Descriptor struct {
	FormatVersion uint64        `json:"format_version" yaml:"format_version"`
	Name          string        `json:"name" yaml:"name"`
	Revision      uint64        `json:"revision" yaml:"revision"`
	Realm         Realm         `json:"realm" yaml:"realm"`
	Platforms     []string      `json:"platforms" yaml:"platforms"`
	Provides      []Contract    `json:"provides,omitempty" yaml:"provides,omitempty"`
	Requires      []Requirement `json:"requires,omitempty" yaml:"requires,omitempty"`
	Events        []Event       `json:"events,omitempty" yaml:"events,omitempty"`
	ConfigSchema  *Contract     `json:"config_schema,omitempty" yaml:"config_schema,omitempty"`
	StateSchema   *Contract     `json:"state_schema,omitempty" yaml:"state_schema,omitempty"`
	Permissions   []Permission  `json:"permissions,omitempty" yaml:"permissions,omitempty"`
	Assets        []Asset       `json:"assets,omitempty" yaml:"assets,omitempty"`
	Lifecycle     Lifecycle     `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"`
}

var (
	pluginNamePattern = regexp.MustCompile(
		`^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$`,
	)
	contractNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]*$`)
	platformPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	assetNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@/-]*$`)
	mediaTypePattern    = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
)

// Validate checks a complete descriptor without consulting a catalog.
func (descriptor Descriptor) Validate() error {
	if descriptor.FormatVersion != DescriptorFormatVersion {
		return fmt.Errorf("plugin %q uses descriptor format %d, want %d",
			descriptor.Name, descriptor.FormatVersion, DescriptorFormatVersion)
	}
	if !pluginNamePattern.MatchString(descriptor.Name) {
		return fmt.Errorf("invalid plugin name %q", descriptor.Name)
	}
	if descriptor.Revision == 0 {
		return fmt.Errorf("plugin %s revision must be positive", descriptor.Name)
	}
	switch descriptor.Realm {
	case ServerRealm, PresentationHostRealm, ClientRealm:
	default:
		return fmt.Errorf("plugin %s has invalid realm %q", descriptor.Name, descriptor.Realm)
	}
	if len(descriptor.Platforms) == 0 {
		return fmt.Errorf("plugin %s declares no platforms", descriptor.Name)
	}
	if err := validateUniqueStrings(descriptor.Name, "platform", descriptor.Platforms, platformPattern); err != nil {
		return err
	}

	provided := make(map[string]Contract, len(descriptor.Provides))
	for _, contract := range descriptor.Provides {
		if err := validateContract(contract); err != nil {
			return fmt.Errorf("plugin %s provided service: %w", descriptor.Name, err)
		}
		if _, duplicate := provided[contract.Name]; duplicate {
			return fmt.Errorf("plugin %s provides service %q more than once", descriptor.Name, contract.Name)
		}
		provided[contract.Name] = contract
	}
	required := make(map[string]Requirement, len(descriptor.Requires))
	for _, requirement := range descriptor.Requires {
		if err := validateContract(requirement.Contract); err != nil {
			return fmt.Errorf("plugin %s required service: %w", descriptor.Name, err)
		}
		name := requirement.Contract.Name
		if _, duplicate := required[name]; duplicate {
			return fmt.Errorf("plugin %s requires service %q more than once", descriptor.Name, name)
		}
		if _, selfProvided := provided[name]; selfProvided {
			return fmt.Errorf("plugin %s both provides and requires service %q", descriptor.Name, name)
		}
		required[name] = requirement
	}

	events := make(map[string]struct{}, len(descriptor.Events))
	for _, event := range descriptor.Events {
		if err := validateContract(event.Contract); err != nil {
			return fmt.Errorf("plugin %s event: %w", descriptor.Name, err)
		}
		switch event.Direction {
		case Publishes, Consumes:
		default:
			return fmt.Errorf("plugin %s event %s has invalid direction %q",
				descriptor.Name, event.Contract.Name, event.Direction)
		}
		key := string(event.Direction) + "\x00" + event.Contract.Name
		if _, duplicate := events[key]; duplicate {
			return fmt.Errorf("plugin %s repeats %s event %q",
				descriptor.Name, event.Direction, event.Contract.Name)
		}
		events[key] = struct{}{}
	}

	if descriptor.ConfigSchema != nil {
		if err := validateContract(*descriptor.ConfigSchema); err != nil {
			return fmt.Errorf("plugin %s config schema: %w", descriptor.Name, err)
		}
	}
	if descriptor.StateSchema != nil {
		if err := validateContract(*descriptor.StateSchema); err != nil {
			return fmt.Errorf("plugin %s state schema: %w", descriptor.Name, err)
		}
	}
	if (descriptor.Lifecycle.Snapshot || descriptor.Lifecycle.Restore) && descriptor.StateSchema == nil {
		return fmt.Errorf("plugin %s declares state transitions without a state schema", descriptor.Name)
	}
	if descriptor.Lifecycle.Restore && !descriptor.Lifecycle.Snapshot {
		return fmt.Errorf("plugin %s declares restore without snapshot", descriptor.Name)
	}

	permissions := make(map[string]struct{}, len(descriptor.Permissions))
	for _, permission := range descriptor.Permissions {
		if !contractNamePattern.MatchString(permission.Kind) {
			return fmt.Errorf("plugin %s has invalid permission kind %q", descriptor.Name, permission.Kind)
		}
		if permission.Resource == "" || permission.Resource != strings.TrimSpace(permission.Resource) ||
			len(permission.Resource) > 1024 {
			return fmt.Errorf("plugin %s permission %s has invalid resource", descriptor.Name, permission.Kind)
		}
		if len(permission.Operations) == 0 {
			return fmt.Errorf("plugin %s permission %s/%s declares no operations",
				descriptor.Name, permission.Kind, permission.Resource)
		}
		if err := validateUniqueStrings(
			descriptor.Name, "permission operation", permission.Operations, contractNamePattern,
		); err != nil {
			return err
		}
		if permission.Authority != "" && !contractNamePattern.MatchString(permission.Authority) {
			return fmt.Errorf("plugin %s permission %s/%s has invalid authority %q",
				descriptor.Name, permission.Kind, permission.Resource, permission.Authority)
		}
		if permission.Authority != "" {
			requirement, declared := required[permission.Authority]
			if !declared {
				return fmt.Errorf("plugin %s permission %s/%s names undeclared authority service %q",
					descriptor.Name, permission.Kind, permission.Resource, permission.Authority)
			}
			if requirement.Optional {
				return fmt.Errorf("plugin %s permission %s/%s names optional authority service %q",
					descriptor.Name, permission.Kind, permission.Resource, permission.Authority)
			}
		}
		key := permission.Kind + "\x00" + permission.Resource
		if _, duplicate := permissions[key]; duplicate {
			return fmt.Errorf("plugin %s repeats permission %s/%s",
				descriptor.Name, permission.Kind, permission.Resource)
		}
		permissions[key] = struct{}{}
	}

	assets := make(map[string]struct{}, len(descriptor.Assets))
	for _, asset := range descriptor.Assets {
		if !assetNamePattern.MatchString(asset.Name) || strings.Contains(asset.Name, "..") {
			return fmt.Errorf("plugin %s has invalid asset name %q", descriptor.Name, asset.Name)
		}
		if !mediaTypePattern.MatchString(asset.MediaType) {
			return fmt.Errorf("plugin %s asset %s has invalid media type %q",
				descriptor.Name, asset.Name, asset.MediaType)
		}
		if err := validateDigest(asset.Digest); err != nil {
			return fmt.Errorf("plugin %s asset %s: %w", descriptor.Name, asset.Name, err)
		}
		if _, duplicate := assets[asset.Name]; duplicate {
			return fmt.Errorf("plugin %s repeats asset %q", descriptor.Name, asset.Name)
		}
		assets[asset.Name] = struct{}{}
	}
	if len(descriptor.Provides) == 0 && len(descriptor.Requires) == 0 && len(descriptor.Events) == 0 &&
		len(descriptor.Permissions) == 0 && len(descriptor.Assets) == 0 {
		return fmt.Errorf("plugin %s declares no capability, event, permission, or asset", descriptor.Name)
	}
	return nil
}

func validateUniqueStrings(owner, label string, values []string, pattern *regexp.Regexp) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !pattern.MatchString(value) {
			return fmt.Errorf("plugin %s has invalid %s %q", owner, label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("plugin %s repeats %s %q", owner, label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateContract(contract Contract) error {
	if !contractNamePattern.MatchString(contract.Name) {
		return fmt.Errorf("invalid contract name %q", contract.Name)
	}
	if contract.Revision == 0 {
		return fmt.Errorf("contract %s revision must be positive", contract.Name)
	}
	if err := validateDigest(contract.Digest); err != nil {
		return fmt.Errorf("contract %s: %w", contract.Name, err)
	}
	return nil
}

func validateDigest(digest string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 ||
		digest != strings.ToLower(digest) {
		return fmt.Errorf("invalid digest %q", digest)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, prefix)); err != nil {
		return fmt.Errorf("invalid digest %q: %w", digest, err)
	}
	return nil
}

// ValidateIdentity verifies an immutable descriptor identity.
func ValidateIdentity(identity Identity) error {
	if !pluginNamePattern.MatchString(identity.Name) {
		return fmt.Errorf("invalid locked plugin name %q", identity.Name)
	}
	if identity.Revision == 0 {
		return fmt.Errorf("locked plugin %s revision must be positive", identity.Name)
	}
	if err := validateDigest(identity.Digest); err != nil {
		return fmt.Errorf("locked plugin %s: %w", identity.Name, err)
	}
	return nil
}

// Clone returns a recursively independent descriptor.
func (descriptor Descriptor) Clone() Descriptor {
	result := descriptor
	result.Platforms = slices.Clone(descriptor.Platforms)
	result.Provides = slices.Clone(descriptor.Provides)
	result.Requires = slices.Clone(descriptor.Requires)
	result.Events = slices.Clone(descriptor.Events)
	result.Permissions = slices.Clone(descriptor.Permissions)
	for index := range result.Permissions {
		result.Permissions[index].Operations = slices.Clone(descriptor.Permissions[index].Operations)
	}
	result.Assets = slices.Clone(descriptor.Assets)
	if descriptor.ConfigSchema != nil {
		copy := *descriptor.ConfigSchema
		result.ConfigSchema = &copy
	}
	if descriptor.StateSchema != nil {
		copy := *descriptor.StateSchema
		result.StateSchema = &copy
	}
	return result
}

// Canonical validates and sorts all declaration-order-insensitive fields.
func (descriptor Descriptor) Canonical() (Descriptor, error) {
	if err := descriptor.Validate(); err != nil {
		return Descriptor{}, err
	}
	result := descriptor.Clone()
	sort.Strings(result.Platforms)
	sort.Slice(result.Provides, func(left, right int) bool {
		return result.Provides[left].Name < result.Provides[right].Name
	})
	sort.Slice(result.Requires, func(left, right int) bool {
		return result.Requires[left].Contract.Name < result.Requires[right].Contract.Name
	})
	sort.Slice(result.Events, func(left, right int) bool {
		if result.Events[left].Contract.Name != result.Events[right].Contract.Name {
			return result.Events[left].Contract.Name < result.Events[right].Contract.Name
		}
		return result.Events[left].Direction < result.Events[right].Direction
	})
	for index := range result.Permissions {
		sort.Strings(result.Permissions[index].Operations)
	}
	sort.Slice(result.Permissions, func(left, right int) bool {
		if result.Permissions[left].Kind != result.Permissions[right].Kind {
			return result.Permissions[left].Kind < result.Permissions[right].Kind
		}
		return result.Permissions[left].Resource < result.Permissions[right].Resource
	})
	sort.Slice(result.Assets, func(left, right int) bool {
		return result.Assets[left].Name < result.Assets[right].Name
	})
	return result, nil
}

// Digest computes the canonical content digest.
func (descriptor Descriptor) Digest() (string, error) {
	canonical, err := descriptor.Canonical()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode plugin %s descriptor: %w", descriptor.Name, err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// Identity returns the exact content-addressed descriptor identity.
func (descriptor Descriptor) Identity() (Identity, error) {
	digest, err := descriptor.Digest()
	if err != nil {
		return Identity{}, err
	}
	return Identity{Name: descriptor.Name, Revision: descriptor.Revision, Digest: digest}, nil
}

// Compatible proves that two exact service contracts have the same meaning.
func (contract Contract) Compatible(other Contract) bool {
	return contract == other
}

// ValidateContract is exported for profile/deployment decoders that admit a
// contract independently of a complete descriptor.
func ValidateContract(contract Contract) error { return validateContract(contract) }

// ErrContractMismatch describes an exact service contract disagreement.
var ErrContractMismatch = errors.New("plugin service contract mismatch")
