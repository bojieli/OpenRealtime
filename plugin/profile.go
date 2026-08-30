package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	ProfileFormatVersion = 1
	LockFormatVersion    = 1
)

// ProfileScope is a service visibility boundary. A child sees services in its
// own scope and ancestors unless an isolation boundary names that service.
type ProfileScope struct {
	Path    string   `json:"path" yaml:"path"`
	Isolate []string `json:"isolate,omitempty" yaml:"isolate,omitempty"`
}

// ProfileEntry mounts one symbolic plugin in a scope. Values and implementation
// bindings are deliberately absent and live in separate artifacts.
type ProfileEntry struct {
	ID     string `json:"id" yaml:"id"`
	Plugin string `json:"plugin" yaml:"plugin"`
	Scope  string `json:"scope" yaml:"scope"`
}

// ProfileExport exposes one provider service as a profile boundary. Application
// launchers use exports instead of reaching into a plugin implementation.
type ProfileExport struct {
	Name     string `json:"name" yaml:"name"`
	Provider string `json:"provider" yaml:"provider"`
	Service  string `json:"service" yaml:"service"`
}

// Profile is one realm's ordered, patchable plugin tree.
type Profile struct {
	FormatVersion uint64          `json:"format_version" yaml:"format_version"`
	Name          string          `json:"name" yaml:"name"`
	Revision      uint64          `json:"revision" yaml:"revision"`
	Realm         Realm           `json:"realm" yaml:"realm"`
	Fingerprint   string          `json:"fingerprint" yaml:"fingerprint"`
	Scopes        []ProfileScope  `json:"scopes" yaml:"scopes"`
	Entries       []ProfileEntry  `json:"entries" yaml:"entries"`
	Exports       []ProfileExport `json:"exports,omitempty" yaml:"exports,omitempty"`
}

// LayerRow replaces a complete row by stable ID, appends a new row, or removes
// a row. Partial mutation is intentionally unsupported: it makes the effective
// composition printable and reviewable.
type LayerRow struct {
	ID     string        `json:"id" yaml:"id"`
	Remove bool          `json:"remove,omitempty" yaml:"remove,omitempty"`
	Entry  *ProfileEntry `json:"entry,omitempty" yaml:"entry,omitempty"`
}

// Layer is one ordered profile patch.
type Layer struct {
	Name string     `json:"name" yaml:"name"`
	Rows []LayerRow `json:"rows" yaml:"rows"`
}

// LockedEntry binds a stable profile row to an exact plugin descriptor.
type LockedEntry struct {
	ID     string   `json:"id" yaml:"id"`
	Plugin Identity `json:"plugin" yaml:"plugin"`
}

// Lock is the complete descriptor resolution for one frozen profile.
type Lock struct {
	FormatVersion      uint64        `json:"format_version" yaml:"format_version"`
	ProfileFingerprint string        `json:"profile_fingerprint" yaml:"profile_fingerprint"`
	Fingerprint        string        `json:"fingerprint" yaml:"fingerprint"`
	Entries            []LockedEntry `json:"entries" yaml:"entries"`
}

// DependencyBinding is the exact provider selected for one consumer service.
type DependencyBinding struct {
	Service  Contract `json:"service"`
	Provider string   `json:"provider"`
	Optional bool     `json:"optional,omitempty"`
}

// PlannedEntry is one mount in deterministic dependency order.
type PlannedEntry struct {
	Entry        ProfileEntry        `json:"entry"`
	Identity     Identity            `json:"identity"`
	Descriptor   Descriptor          `json:"descriptor"`
	Dependencies []DependencyBinding `json:"dependencies,omitempty"`
}

// PlannedExport is an exact externally visible service boundary.
type PlannedExport struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	Service  Contract `json:"service"`
}

// Plan is the immutable static plugin composition consumed by a realm runtime.
type Plan struct {
	ProfileFingerprint string          `json:"profile_fingerprint"`
	LockFingerprint    string          `json:"lock_fingerprint"`
	Realm              Realm           `json:"realm"`
	Fingerprint        string          `json:"fingerprint"`
	Entries            []PlannedEntry  `json:"entries"`
	Exports            []PlannedExport `json:"exports,omitempty"`
}

type resolvedProfileEntry struct {
	entry      ProfileEntry
	identity   Identity
	descriptor Descriptor
	index      int
	bindings   []DependencyBinding
}

// FreezeProfile canonicalizes non-semantic collections and computes a profile
// fingerprint. Entry order is semantic and is preserved.
func FreezeProfile(profile Profile) (Profile, error) {
	result := profile.Clone()
	result.Fingerprint = ""
	result.canonicalize()
	if err := result.validateStructure(); err != nil {
		return Profile{}, err
	}
	fingerprint, err := digestJSON(result)
	if err != nil {
		return Profile{}, fmt.Errorf("fingerprint plugin profile %s: %w", profile.Name, err)
	}
	result.Fingerprint = fingerprint
	return result, nil
}

// Validate verifies structure and the stored fingerprint.
func (profile Profile) Validate() error {
	want := profile.Clone()
	want.Fingerprint = ""
	want.canonicalize()
	if err := want.validateStructure(); err != nil {
		return err
	}
	fingerprint, err := digestJSON(want)
	if err != nil {
		return err
	}
	if profile.Fingerprint != fingerprint {
		return fmt.Errorf("plugin profile %s fingerprint is %q, want %q",
			profile.Name, profile.Fingerprint, fingerprint)
	}
	return nil
}

func (profile Profile) validateStructure() error {
	if profile.FormatVersion != ProfileFormatVersion {
		return fmt.Errorf("plugin profile %q uses format %d, want %d",
			profile.Name, profile.FormatVersion, ProfileFormatVersion)
	}
	if !contractNamePattern.MatchString(profile.Name) {
		return fmt.Errorf("invalid plugin profile name %q", profile.Name)
	}
	if profile.Revision == 0 {
		return fmt.Errorf("plugin profile %s revision must be positive", profile.Name)
	}
	switch profile.Realm {
	case ServerRealm, PresentationHostRealm, ClientRealm:
	default:
		return fmt.Errorf("plugin profile %s has invalid realm %q", profile.Name, profile.Realm)
	}
	if len(profile.Scopes) == 0 {
		return fmt.Errorf("plugin profile %s declares no scopes", profile.Name)
	}
	scopes := make(map[string]ProfileScope, len(profile.Scopes))
	for _, scope := range profile.Scopes {
		if err := validateScopePath(scope.Path); err != nil {
			return fmt.Errorf("plugin profile %s: %w", profile.Name, err)
		}
		if _, duplicate := scopes[scope.Path]; duplicate {
			return fmt.Errorf("plugin profile %s repeats scope %q", profile.Name, scope.Path)
		}
		if err := validateUniqueStrings(profile.Name, "isolated service", scope.Isolate, contractNamePattern); err != nil {
			return err
		}
		scopes[scope.Path] = scope
	}
	if _, root := scopes["root"]; !root {
		return fmt.Errorf("plugin profile %s has no root scope", profile.Name)
	}
	for path := range scopes {
		if path == "root" {
			continue
		}
		if _, found := scopes[parentScope(path)]; !found {
			return fmt.Errorf("plugin profile %s scope %q has no parent scope %q",
				profile.Name, path, parentScope(path))
		}
	}
	if len(profile.Entries) == 0 {
		return fmt.Errorf("plugin profile %s declares no entries", profile.Name)
	}
	entries := make(map[string]struct{}, len(profile.Entries))
	for _, entry := range profile.Entries {
		if err := validateEntry(entry, scopes); err != nil {
			return fmt.Errorf("plugin profile %s: %w", profile.Name, err)
		}
		if _, duplicate := entries[entry.ID]; duplicate {
			return fmt.Errorf("plugin profile %s repeats entry %q", profile.Name, entry.ID)
		}
		entries[entry.ID] = struct{}{}
	}
	exports := make(map[string]struct{}, len(profile.Exports))
	for _, boundary := range profile.Exports {
		if !contractNamePattern.MatchString(boundary.Name) {
			return fmt.Errorf("plugin profile %s has invalid export name %q", profile.Name, boundary.Name)
		}
		if _, duplicate := exports[boundary.Name]; duplicate {
			return fmt.Errorf("plugin profile %s repeats export %q", profile.Name, boundary.Name)
		}
		exports[boundary.Name] = struct{}{}
		if _, found := entries[boundary.Provider]; !found {
			return fmt.Errorf("plugin profile %s export %s names absent provider %s",
				profile.Name, boundary.Name, boundary.Provider)
		}
		if !contractNamePattern.MatchString(boundary.Service) {
			return fmt.Errorf("plugin profile %s export %s has invalid service %q",
				profile.Name, boundary.Name, boundary.Service)
		}
	}
	return nil
}

func validateEntry(entry ProfileEntry, scopes map[string]ProfileScope) error {
	if !contractNamePattern.MatchString(entry.ID) {
		return fmt.Errorf("invalid profile entry ID %q", entry.ID)
	}
	if !pluginNamePattern.MatchString(entry.Plugin) {
		return fmt.Errorf("profile entry %s has invalid plugin name %q", entry.ID, entry.Plugin)
	}
	if _, found := scopes[entry.Scope]; !found {
		return fmt.Errorf("profile entry %s uses unknown scope %q", entry.ID, entry.Scope)
	}
	return nil
}

func validateScopePath(path string) error {
	if path == "" || path != strings.TrimSpace(path) || len(path) > 512 {
		return fmt.Errorf("invalid plugin scope %q", path)
	}
	parts := strings.Split(path, "/")
	if parts[0] != "root" {
		return fmt.Errorf("plugin scope %q is not rooted at root", path)
	}
	for _, part := range parts {
		if !platformPattern.MatchString(part) {
			return fmt.Errorf("invalid plugin scope %q", path)
		}
	}
	return nil
}

func parentScope(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return ""
	}
	return path[:index]
}

func (profile *Profile) canonicalize() {
	sort.Slice(profile.Scopes, func(left, right int) bool {
		return profile.Scopes[left].Path < profile.Scopes[right].Path
	})
	for index := range profile.Scopes {
		sort.Strings(profile.Scopes[index].Isolate)
	}
	sort.Slice(profile.Exports, func(left, right int) bool {
		return profile.Exports[left].Name < profile.Exports[right].Name
	})
}

// Clone returns a recursively independent profile.
func (profile Profile) Clone() Profile {
	result := profile
	result.Scopes = slices.Clone(profile.Scopes)
	for index := range result.Scopes {
		result.Scopes[index].Isolate = slices.Clone(profile.Scopes[index].Isolate)
	}
	result.Entries = slices.Clone(profile.Entries)
	result.Exports = slices.Clone(profile.Exports)
	return result
}

// Compose applies ordered whole-row layers and freezes the resulting revision.
func Compose(base Profile, revision uint64, layers ...Layer) (Profile, error) {
	if err := base.Validate(); err != nil {
		return Profile{}, fmt.Errorf("compose plugin profile: %w", err)
	}
	if revision <= base.Revision {
		return Profile{}, fmt.Errorf("composed profile revision %d must exceed base revision %d",
			revision, base.Revision)
	}
	result := base.Clone()
	result.Revision = revision
	result.Fingerprint = ""
	for _, layer := range layers {
		if layer.Name == "" || layer.Name != strings.TrimSpace(layer.Name) {
			return Profile{}, errors.New("plugin profile layer requires a canonical name")
		}
		seen := make(map[string]struct{}, len(layer.Rows))
		for _, row := range layer.Rows {
			if !contractNamePattern.MatchString(row.ID) {
				return Profile{}, fmt.Errorf("plugin profile layer %s has invalid row ID %q", layer.Name, row.ID)
			}
			if _, duplicate := seen[row.ID]; duplicate {
				return Profile{}, fmt.Errorf("plugin profile layer %s repeats row %q", layer.Name, row.ID)
			}
			seen[row.ID] = struct{}{}
			if row.Remove == (row.Entry != nil) {
				return Profile{}, fmt.Errorf("plugin profile layer %s row %s must select exactly one of remove or entry",
					layer.Name, row.ID)
			}
			index := profileEntryIndex(result.Entries, row.ID)
			if row.Remove {
				if index < 0 {
					return Profile{}, fmt.Errorf("plugin profile layer %s cannot remove absent row %s", layer.Name, row.ID)
				}
				result.Entries = append(result.Entries[:index], result.Entries[index+1:]...)
				continue
			}
			if row.Entry.ID != row.ID {
				return Profile{}, fmt.Errorf("plugin profile layer %s row %s contains entry ID %s",
					layer.Name, row.ID, row.Entry.ID)
			}
			if index >= 0 {
				result.Entries[index] = *row.Entry
			} else {
				result.Entries = append(result.Entries, *row.Entry)
			}
		}
	}
	return FreezeProfile(result)
}

func profileEntryIndex(entries []ProfileEntry, id string) int {
	for index := range entries {
		if entries[index].ID == id {
			return index
		}
	}
	return -1
}

// ResolveProfile deliberately creates a new exact lock using the latest
// registered descriptor for every symbolic row.
func ResolveProfile(profile Profile, catalog *Catalog) (Lock, error) {
	if err := profile.Validate(); err != nil {
		return Lock{}, err
	}
	if catalog == nil {
		return Lock{}, errors.New("resolve plugin profile: nil catalog")
	}
	lock := Lock{
		FormatVersion: LockFormatVersion, ProfileFingerprint: profile.Fingerprint,
		Entries: make([]LockedEntry, 0, len(profile.Entries)),
	}
	for _, entry := range profile.Entries {
		descriptor, identity, err := catalog.Latest(entry.Plugin)
		if err != nil {
			return Lock{}, fmt.Errorf("resolve plugin profile entry %s: %w", entry.ID, err)
		}
		if descriptor.Realm != profile.Realm {
			return Lock{}, fmt.Errorf("plugin profile entry %s selects %s realm plugin %s in %s profile",
				entry.ID, descriptor.Realm, descriptor.Name, profile.Realm)
		}
		lock.Entries = append(lock.Entries, LockedEntry{ID: entry.ID, Plugin: identity})
	}
	return freezeLock(lock)
}

func freezeLock(lock Lock) (Lock, error) {
	result := lock.Clone()
	result.Fingerprint = ""
	if err := result.validateStructure(); err != nil {
		return Lock{}, err
	}
	fingerprint, err := digestJSON(result)
	if err != nil {
		return Lock{}, err
	}
	result.Fingerprint = fingerprint
	return result, nil
}

// Validate verifies lock structure, profile binding, and fingerprint.
func (lock Lock) Validate() error {
	want := lock.Clone()
	want.Fingerprint = ""
	if err := want.validateStructure(); err != nil {
		return err
	}
	fingerprint, err := digestJSON(want)
	if err != nil {
		return err
	}
	if lock.Fingerprint != fingerprint {
		return fmt.Errorf("plugin profile lock fingerprint is %q, want %q", lock.Fingerprint, fingerprint)
	}
	return nil
}

func (lock Lock) validateStructure() error {
	if lock.FormatVersion != LockFormatVersion {
		return fmt.Errorf("plugin profile lock uses format %d, want %d", lock.FormatVersion, LockFormatVersion)
	}
	if err := validateDigest(lock.ProfileFingerprint); err != nil {
		return fmt.Errorf("plugin profile lock profile fingerprint: %w", err)
	}
	if len(lock.Entries) == 0 {
		return errors.New("plugin profile lock has no entries")
	}
	seen := make(map[string]struct{}, len(lock.Entries))
	for _, entry := range lock.Entries {
		if !contractNamePattern.MatchString(entry.ID) {
			return fmt.Errorf("plugin profile lock has invalid entry ID %q", entry.ID)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return fmt.Errorf("plugin profile lock repeats entry %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		if err := ValidateIdentity(entry.Plugin); err != nil {
			return fmt.Errorf("plugin profile lock entry %s: %w", entry.ID, err)
		}
	}
	return nil
}

// Clone returns a recursively independent lock.
func (lock Lock) Clone() Lock {
	result := lock
	result.Entries = slices.Clone(lock.Entries)
	return result
}

// Compile verifies a frozen profile and lock, binds every visible service to
// one exact provider, rejects dependency cycles, and emits deterministic mount
// order. It never falls back to a newer catalog revision.
func Compile(profile Profile, lock Lock, catalog *Catalog) (Plan, error) {
	if err := profile.Validate(); err != nil {
		return Plan{}, err
	}
	if err := lock.Validate(); err != nil {
		return Plan{}, err
	}
	if lock.ProfileFingerprint != profile.Fingerprint {
		return Plan{}, fmt.Errorf("plugin profile lock targets %s, profile is %s",
			lock.ProfileFingerprint, profile.Fingerprint)
	}
	if catalog == nil {
		return Plan{}, errors.New("compile plugin profile: nil catalog")
	}
	if len(lock.Entries) != len(profile.Entries) {
		return Plan{}, fmt.Errorf("plugin profile lock has %d entries, profile has %d",
			len(lock.Entries), len(profile.Entries))
	}
	locked := make(map[string]Identity, len(lock.Entries))
	for _, entry := range lock.Entries {
		locked[entry.ID] = entry.Plugin
	}

	resolved := make(map[string]*resolvedProfileEntry, len(profile.Entries))
	providers := make(map[string]map[string][]*resolvedProfileEntry)
	for index, entry := range profile.Entries {
		identity, found := locked[entry.ID]
		if !found || identity.Name != entry.Plugin {
			return Plan{}, fmt.Errorf("plugin profile lock has no exact resolution for entry %s", entry.ID)
		}
		descriptor, err := catalog.Resolve(identity)
		if err != nil {
			return Plan{}, fmt.Errorf("compile plugin profile entry %s: %w", entry.ID, err)
		}
		if descriptor.Realm != profile.Realm {
			return Plan{}, fmt.Errorf("plugin profile entry %s selects %s realm plugin in %s profile",
				entry.ID, descriptor.Realm, profile.Realm)
		}
		item := &resolvedProfileEntry{entry: entry, identity: identity, descriptor: descriptor, index: index}
		resolved[entry.ID] = item
		if providers[entry.Scope] == nil {
			providers[entry.Scope] = make(map[string][]*resolvedProfileEntry)
		}
		for _, service := range descriptor.Provides {
			providers[entry.Scope][service.Name] = append(providers[entry.Scope][service.Name], item)
		}
	}

	scopes := make(map[string]ProfileScope, len(profile.Scopes))
	for _, scope := range profile.Scopes {
		scopes[scope.Path] = scope
	}
	requiredEdges := make(map[string]map[string]struct{}, len(resolved))
	indegree := make(map[string]int, len(resolved))
	for id := range resolved {
		requiredEdges[id] = make(map[string]struct{})
		indegree[id] = 0
	}
	for _, consumer := range resolved {
		for _, requirement := range consumer.descriptor.Requires {
			provider, found, err := visibleProvider(consumer, requirement, providers, scopes)
			if err != nil {
				return Plan{}, err
			}
			if !found {
				if requirement.Optional {
					continue
				}
				return Plan{}, fmt.Errorf("plugin profile entry %s requires unavailable service %s",
					consumer.entry.ID, requirement.Contract.Name)
			}
			consumer.bindings = append(consumer.bindings, DependencyBinding{
				Service: requirement.Contract, Provider: provider.entry.ID, Optional: requirement.Optional,
			})
			if requirement.Optional {
				continue
			}
			if _, duplicate := requiredEdges[provider.entry.ID][consumer.entry.ID]; !duplicate {
				requiredEdges[provider.entry.ID][consumer.entry.ID] = struct{}{}
				indegree[consumer.entry.ID]++
			}
		}
		sort.Slice(consumer.bindings, func(left, right int) bool {
			return consumer.bindings[left].Service.Name < consumer.bindings[right].Service.Name
		})
	}

	ready := make([]*resolvedProfileEntry, 0, len(resolved))
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, resolved[id])
		}
	}
	sortResolved(ready)
	ordered := make([]*resolvedProfileEntry, 0, len(resolved))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		ordered = append(ordered, current)
		for consumerID := range requiredEdges[current.entry.ID] {
			indegree[consumerID]--
			if indegree[consumerID] == 0 {
				ready = append(ready, resolved[consumerID])
				sortResolved(ready)
			}
		}
	}
	if len(ordered) != len(resolved) {
		var cycle []string
		for id, degree := range indegree {
			if degree > 0 {
				cycle = append(cycle, id)
			}
		}
		sort.Strings(cycle)
		return Plan{}, fmt.Errorf("plugin profile has required-service dependency cycle among [%s]",
			strings.Join(cycle, ", "))
	}

	plan := Plan{
		ProfileFingerprint: profile.Fingerprint, LockFingerprint: lock.Fingerprint,
		Realm: profile.Realm, Entries: make([]PlannedEntry, 0, len(ordered)),
	}
	for _, item := range ordered {
		plan.Entries = append(plan.Entries, PlannedEntry{
			Entry: item.entry, Identity: item.identity, Descriptor: item.descriptor.Clone(),
			Dependencies: slices.Clone(item.bindings),
		})
	}
	for _, boundary := range profile.Exports {
		provider := resolved[boundary.Provider]
		var service Contract
		for _, candidate := range provider.descriptor.Provides {
			if candidate.Name == boundary.Service {
				service = candidate
				break
			}
		}
		if service.Name == "" {
			return Plan{}, fmt.Errorf("plugin profile export %s provider %s does not provide service %s",
				boundary.Name, boundary.Provider, boundary.Service)
		}
		plan.Exports = append(plan.Exports, PlannedExport{
			Name: boundary.Name, Provider: boundary.Provider, Service: service,
		})
	}
	sort.Slice(plan.Exports, func(left, right int) bool { return plan.Exports[left].Name < plan.Exports[right].Name })
	fingerprint, err := planDigest(plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Fingerprint = fingerprint
	if err := plan.Validate(); err != nil {
		return Plan{}, fmt.Errorf("compile plugin profile produced invalid plan: %w", err)
	}
	return plan, nil
}

func visibleProvider(
	consumer *resolvedProfileEntry,
	requirement Requirement,
	providers map[string]map[string][]*resolvedProfileEntry,
	scopes map[string]ProfileScope,
) (*resolvedProfileEntry, bool, error) {
	service := requirement.Contract.Name
	for scopePath := consumer.entry.Scope; scopePath != ""; scopePath = parentScope(scopePath) {
		candidates := providers[scopePath][service]
		if len(candidates) > 0 {
			if len(candidates) != 1 {
				ids := make([]string, 0, len(candidates))
				for _, candidate := range candidates {
					ids = append(ids, candidate.entry.ID)
				}
				sort.Strings(ids)
				return nil, false, fmt.Errorf(
					"plugin profile entry %s sees ambiguous service %s providers [%s] in scope %s",
					consumer.entry.ID, service, strings.Join(ids, ", "), scopePath,
				)
			}
			provider := candidates[0]
			var provided Contract
			for _, contract := range provider.descriptor.Provides {
				if contract.Name == service {
					provided = contract
					break
				}
			}
			if !requirement.Contract.Compatible(provided) {
				return nil, false, fmt.Errorf(
					"%w: plugin profile entry %s requires %s@%d %s, provider %s declares %s@%d %s",
					ErrContractMismatch, consumer.entry.ID,
					requirement.Contract.Name, requirement.Contract.Revision, requirement.Contract.Digest,
					provider.entry.ID, provided.Name, provided.Revision, provided.Digest,
				)
			}
			return provider, true, nil
		}
		scope := scopes[scopePath]
		if scopePath == "root" || slices.Contains(scope.Isolate, service) {
			return nil, false, nil
		}
	}
	return nil, false, nil
}

func sortResolved(entries []*resolvedProfileEntry) {
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].index < entries[right].index
	})
}

func planDigest(plan Plan) (string, error) {
	copy := plan
	copy.Fingerprint = ""
	return digestJSON(copy)
}

// Validate proves that a plan is self-contained, exact, dependency ordered,
// and unmodified since compilation.
func (plan Plan) Validate() error {
	if err := validateDigest(plan.ProfileFingerprint); err != nil {
		return fmt.Errorf("plugin plan profile fingerprint: %w", err)
	}
	if err := validateDigest(plan.LockFingerprint); err != nil {
		return fmt.Errorf("plugin plan lock fingerprint: %w", err)
	}
	switch plan.Realm {
	case ServerRealm, PresentationHostRealm, ClientRealm:
	default:
		return fmt.Errorf("plugin plan has invalid realm %q", plan.Realm)
	}
	if len(plan.Entries) == 0 {
		return errors.New("plugin plan has no entries")
	}
	entries := make(map[string]int, len(plan.Entries))
	for index, entry := range plan.Entries {
		if _, duplicate := entries[entry.Entry.ID]; duplicate {
			return fmt.Errorf("plugin plan repeats entry %q", entry.Entry.ID)
		}
		entries[entry.Entry.ID] = index
		if entry.Entry.Plugin != entry.Identity.Name {
			return fmt.Errorf("plugin plan entry %s names %s but pins %s",
				entry.Entry.ID, entry.Entry.Plugin, entry.Identity.Name)
		}
		if err := ValidateIdentity(entry.Identity); err != nil {
			return fmt.Errorf("plugin plan entry %s: %w", entry.Entry.ID, err)
		}
		if err := entry.Descriptor.Validate(); err != nil {
			return fmt.Errorf("plugin plan entry %s descriptor: %w", entry.Entry.ID, err)
		}
		identity, err := entry.Descriptor.Identity()
		if err != nil || identity != entry.Identity {
			return fmt.Errorf("plugin plan entry %s descriptor identity is %#v, want %#v",
				entry.Entry.ID, identity, entry.Identity)
		}
		if entry.Descriptor.Realm != plan.Realm {
			return fmt.Errorf("plugin plan entry %s has realm %s, plan has realm %s",
				entry.Entry.ID, entry.Descriptor.Realm, plan.Realm)
		}
	}
	for consumerIndex, entry := range plan.Entries {
		requirements := make(map[string]Requirement, len(entry.Descriptor.Requires))
		for _, requirement := range entry.Descriptor.Requires {
			requirements[requirement.Contract.Name] = requirement
		}
		bound := make(map[string]struct{}, len(entry.Dependencies))
		for _, binding := range entry.Dependencies {
			requirement, declared := requirements[binding.Service.Name]
			if !declared || requirement.Contract != binding.Service || requirement.Optional != binding.Optional {
				return fmt.Errorf("plugin plan entry %s has undeclared or changed dependency %s",
					entry.Entry.ID, binding.Service.Name)
			}
			if _, duplicate := bound[binding.Service.Name]; duplicate {
				return fmt.Errorf("plugin plan entry %s repeats dependency %s",
					entry.Entry.ID, binding.Service.Name)
			}
			bound[binding.Service.Name] = struct{}{}
			providerIndex, found := entries[binding.Provider]
			if !found {
				return fmt.Errorf("plugin plan entry %s names absent provider %s",
					entry.Entry.ID, binding.Provider)
			}
			if !binding.Optional && providerIndex >= consumerIndex {
				return fmt.Errorf("plugin plan entry %s is ordered before required provider %s",
					entry.Entry.ID, binding.Provider)
			}
			provided := false
			for _, service := range plan.Entries[providerIndex].Descriptor.Provides {
				if service == binding.Service {
					provided = true
					break
				}
			}
			if !provided {
				return fmt.Errorf("plugin plan provider %s does not provide exact service %s",
					binding.Provider, binding.Service.Name)
			}
		}
		for _, requirement := range entry.Descriptor.Requires {
			_, exists := bound[requirement.Contract.Name]
			if !requirement.Optional && !exists {
				return fmt.Errorf("plugin plan entry %s has no binding for required service %s",
					entry.Entry.ID, requirement.Contract.Name)
			}
		}
	}
	exports := make(map[string]struct{}, len(plan.Exports))
	for _, boundary := range plan.Exports {
		if !contractNamePattern.MatchString(boundary.Name) {
			return fmt.Errorf("plugin plan has invalid export name %q", boundary.Name)
		}
		if _, duplicate := exports[boundary.Name]; duplicate {
			return fmt.Errorf("plugin plan repeats export %q", boundary.Name)
		}
		exports[boundary.Name] = struct{}{}
		providerIndex, found := entries[boundary.Provider]
		if !found {
			return fmt.Errorf("plugin plan export %s names absent provider %s", boundary.Name, boundary.Provider)
		}
		provided := false
		for _, service := range plan.Entries[providerIndex].Descriptor.Provides {
			if service == boundary.Service {
				provided = true
				break
			}
		}
		if !provided {
			return fmt.Errorf("plugin plan export %s provider %s does not provide exact service %s",
				boundary.Name, boundary.Provider, boundary.Service.Name)
		}
	}
	want, err := planDigest(plan)
	if err != nil {
		return err
	}
	if plan.Fingerprint != want {
		return fmt.Errorf("plugin plan fingerprint is %q, want %q", plan.Fingerprint, want)
	}
	return nil
}

// Clone returns a recursively independent plan.
func (plan Plan) Clone() Plan {
	result := plan
	result.Entries = make([]PlannedEntry, len(plan.Entries))
	for index, entry := range plan.Entries {
		result.Entries[index] = PlannedEntry{
			Entry: entry.Entry, Identity: entry.Identity, Descriptor: entry.Descriptor.Clone(),
			Dependencies: slices.Clone(entry.Dependencies),
		}
	}
	result.Exports = slices.Clone(plan.Exports)
	return result
}

func digestJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// MarshalProfile returns deterministic, newline-terminated strict JSON.
func MarshalProfile(profile Profile) ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	canonical := profile.Clone()
	canonical.canonicalize()
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// ParseProfile decodes strict JSON and verifies the fingerprint.
func ParseProfile(source []byte) (Profile, error) {
	if err := strictjson.Validate(source); err != nil {
		return Profile{}, fmt.Errorf("decode plugin profile: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var profile Profile
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, fmt.Errorf("decode plugin profile: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Profile{}, errors.New("decode plugin profile: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Profile{}, fmt.Errorf("decode plugin profile trailing data: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	profile.canonicalize()
	return profile, nil
}
