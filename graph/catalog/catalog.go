// Package catalog publishes immutable graph-plan metadata for discovery. It
// derives exported contracts and requirements from an already validated
// config.Plan; it never reconstructs topology from legacy ownership flags and
// never retains values, deployment secret references, or secret locators.
package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	FormatVersion        uint64 = 1
	maximumCatalogBytes         = 32 << 20
	maximumEntries              = 65_536
	maximumQueryResults         = 1_000
	maximumJSONDepth            = 128
	maximumJSONTokens           = 2_000_000
	maximumMetadataItems        = 65_536
	maximumStringBytes          = 64 << 10
)

type Stage string

const (
	Experimental Stage = "experimental"
	Candidate    Stage = "candidate"
	Stable       Stage = "stable"
	Retired      Stage = "retired"
)

type Metadata struct {
	Stage    Stage
	Summary  string
	Change   string
	Tags     []string
	Profiles []inspect.ArtifactIdentity
}

type Boundary struct {
	Name      string               `json:"name"`
	Direction ir.BoundaryDirection `json:"direction"`
	Type      string               `json:"type"`
}

type Node struct {
	ID             string                               `json:"id"`
	Element        element.Identity                     `json:"element"`
	Implementation graphconfig.ImplementationResolution `json:"implementation"`
	ConfigSchema   string                               `json:"config_schema,omitempty"`
	StateSchema    string                               `json:"state_schema,omitempty"`
}

type Dependency struct {
	Name     string                      `json:"name"`
	Optional bool                        `json:"optional,omitempty"`
	Artifact *inspect.ArtifactIdentity   `json:"artifact,omitempty"`
	Scope    graphconfig.DependencyScope `json:"scope,omitempty"`
}

type ChannelSummary struct {
	Count    int `json:"count"`
	Lossy    int `json:"lossy"`
	MaxDepth int `json:"max_depth"`
}

// Entry is one exact discoverable plan revision. Fingerprint covers metadata,
// the public plan identity, exact boundary contracts, registered
// implementations, dependencies, effects, and evidence profiles.
type Entry struct {
	GraphID      string                     `json:"graph_id"`
	Revision     uint64                     `json:"revision"`
	Fingerprint  string                     `json:"fingerprint"`
	Plan         graphconfig.Identity       `json:"plan"`
	Graph        ir.Graph                   `json:"graph"`
	Stage        Stage                      `json:"stage"`
	Summary      string                     `json:"summary"`
	Change       string                     `json:"change"`
	Tags         []string                   `json:"tags,omitempty"`
	Lineage      []string                   `json:"lineage,omitempty"`
	Boundaries   []Boundary                 `json:"boundaries,omitempty"`
	Nodes        []Node                     `json:"nodes"`
	Dependencies []Dependency               `json:"dependencies,omitempty"`
	Effects      []element.Effect           `json:"effects,omitempty"`
	Channels     ChannelSummary             `json:"channels"`
	Profiles     []inspect.ArtifactIdentity `json:"profiles,omitempty"`
}

func (entry Entry) Ref() string {
	return fmt.Sprintf("%s@%d", entry.GraphID, entry.Revision)
}

// Validate verifies both derived metadata and the stored immutable entry
// fingerprint. Freeze is the constructor that assigns a new fingerprint.
func (entry Entry) Validate() error {
	frozen, err := freezeEntry(entry)
	if err != nil {
		return err
	}
	if entry.Fingerprint != frozen.Fingerprint {
		return fmt.Errorf("catalog entry %s fingerprint is %q, want %q",
			entry.Ref(), entry.Fingerprint, frozen.Fingerprint)
	}
	return nil
}

// NewEntry snapshots one Plan. Private deployment identity and all raw
// artifacts are intentionally inaccessible through this API.
func NewEntry(plan *graphconfig.Plan, metadata Metadata) (Entry, error) {
	if plan == nil {
		return Entry{}, errors.New("catalog entry requires a plan")
	}
	if err := plan.Validate(); err != nil {
		return Entry{}, fmt.Errorf("catalog entry plan: %w", err)
	}
	identity := plan.Identity()
	graph := plan.Graph()
	resolution := plan.Resolution()
	byNode := make(map[string]graphconfig.ImplementationResolution, len(resolution.Nodes))
	for _, node := range resolution.Nodes {
		if _, duplicate := byNode[node.NodeID]; duplicate {
			return Entry{}, fmt.Errorf("catalog entry resolution repeats node %s", node.NodeID)
		}
		byNode[node.NodeID] = node.Implementation
	}
	dependencyResolutions := make(map[string]graphconfig.DependencyResolution, len(resolution.Dependencies))
	for _, dependency := range resolution.Dependencies {
		dependencyResolutions[dependency.Name] = dependency
	}

	entry := Entry{
		GraphID: graph.ID, Revision: graph.Revision, Plan: identity, Graph: graph,
		Stage: metadata.Stage, Summary: metadata.Summary, Change: metadata.Change,
		Tags: slices.Clone(metadata.Tags), Lineage: slices.Clone(graph.Lineage),
		Profiles: slices.Clone(metadata.Profiles),
		Nodes:    make([]Node, 0, len(graph.Nodes)),
	}
	for _, boundary := range graph.Boundaries {
		entry.Boundaries = append(entry.Boundaries, Boundary{
			Name: boundary.Name, Direction: boundary.Direction, Type: boundary.Type.String(),
		})
	}
	dependencies := make(map[string]Dependency)
	effects := make(map[string]element.Effect)
	for _, node := range graph.Nodes {
		implementation, found := byNode[node.ID]
		if !found {
			return Entry{}, fmt.Errorf("catalog entry resolution is missing node %s", node.ID)
		}
		entry.Nodes = append(entry.Nodes, Node{
			ID: node.ID, Element: node.Element, Implementation: implementation,
			ConfigSchema: node.ConfigSchema, StateSchema: node.StateSchema,
		})
		for _, dependency := range node.Dependencies {
			current, found := dependencies[dependency.Name]
			if !found || current.Optional && !dependency.Optional {
				current = Dependency{Name: dependency.Name, Optional: dependency.Optional}
				if resolved, selected := dependencyResolutions[dependency.Name]; selected {
					artifact := resolved.Artifact
					current.Artifact = &artifact
					current.Scope = resolved.Scope
				}
				dependencies[dependency.Name] = current
			}
		}
		for _, effect := range node.Effects {
			effects[effect.Name] = effect
		}
	}
	for _, dependency := range dependencies {
		entry.Dependencies = append(entry.Dependencies, dependency)
	}
	for _, effect := range effects {
		entry.Effects = append(entry.Effects, effect)
	}
	for _, edge := range graph.Edges {
		entry.Channels.Count++
		if edge.Delivery == ir.Lossy {
			entry.Channels.Lossy++
		}
		entry.Channels.MaxDepth = max(entry.Channels.MaxDepth, edge.Depth)
	}
	canonical, err := freezeEntry(entry)
	if err != nil {
		return Entry{}, err
	}
	return canonical, nil
}

type Document struct {
	FormatVersion uint64  `json:"format_version"`
	Fingerprint   string  `json:"fingerprint"`
	Entries       []Entry `json:"entries"`
}

// Freeze creates a deterministic immutable catalog snapshot.
func Freeze(entries []Entry) (Document, error) {
	document := Document{FormatVersion: FormatVersion, Entries: make([]Entry, len(entries))}
	for index, entry := range entries {
		canonical, err := freezeEntry(entry)
		if err != nil {
			return Document{}, fmt.Errorf("catalog entry %d: %w", index, err)
		}
		document.Entries[index] = canonical
	}
	canonicalizeDocument(&document)
	if len(document.Entries) > maximumEntries {
		return Document{}, fmt.Errorf("catalog contains %d entries; maximum is %d", len(document.Entries), maximumEntries)
	}
	seen := make(map[string]struct{}, len(document.Entries))
	for _, entry := range document.Entries {
		if _, duplicate := seen[entry.Ref()]; duplicate {
			return Document{}, fmt.Errorf("catalog repeats immutable graph revision %s", entry.Ref())
		}
		seen[entry.Ref()] = struct{}{}
	}
	fingerprint, err := documentFingerprint(document)
	if err != nil {
		return Document{}, err
	}
	document.Fingerprint = fingerprint
	return document, nil
}

func (document Document) Validate() error {
	if document.FormatVersion != FormatVersion {
		return fmt.Errorf("catalog format %d is unsupported; want %d", document.FormatVersion, FormatVersion)
	}
	if len(document.Entries) > maximumEntries {
		return fmt.Errorf("catalog contains %d entries; maximum is %d", len(document.Entries), maximumEntries)
	}
	storedFingerprints := make(map[string]string, len(document.Entries))
	for _, entry := range document.Entries {
		if _, duplicate := storedFingerprints[entry.Ref()]; duplicate {
			return fmt.Errorf("catalog repeats immutable graph revision %s", entry.Ref())
		}
		storedFingerprints[entry.Ref()] = entry.Fingerprint
	}
	frozen, err := Freeze(document.Entries)
	if err != nil {
		return err
	}
	for _, entry := range frozen.Entries {
		if storedFingerprints[entry.Ref()] != entry.Fingerprint {
			return fmt.Errorf("catalog entry %s fingerprint is %q, want %q",
				entry.Ref(), storedFingerprints[entry.Ref()], entry.Fingerprint)
		}
	}
	if document.Fingerprint != frozen.Fingerprint {
		return fmt.Errorf("catalog fingerprint is %q, want %q", document.Fingerprint, frozen.Fingerprint)
	}
	return nil
}

func (document Document) Marshal() ([]byte, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	canonical, err := Freeze(document.Entries)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode graph catalog: %w", err)
	}
	return append(payload, '\n'), nil
}

func Parse(source []byte) (Document, error) {
	if len(source) > maximumCatalogBytes {
		return Document{}, fmt.Errorf("graph catalog has %d bytes; maximum is %d", len(source), maximumCatalogBytes)
	}
	if err := preflightJSON(source); err != nil {
		return Document{}, err
	}
	if err := strictjson.Validate(source); err != nil {
		return Document{}, fmt.Errorf("decode graph catalog: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode graph catalog: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Document{}, errors.New("decode graph catalog: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Document{}, fmt.Errorf("decode graph catalog trailing data: %w", err)
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	frozen, err := Freeze(document.Entries)
	if err != nil {
		return Document{}, err
	}
	return frozen, nil
}

// Lookup requires the exact entry fingerprint so mutable graph@revision
// selectors cannot silently reinterpret a deployment.
func (document Document) Lookup(graphID string, revision uint64, fingerprint string) (Entry, error) {
	if err := document.Validate(); err != nil {
		return Entry{}, err
	}
	for _, entry := range document.Entries {
		if entry.GraphID == graphID && entry.Revision == revision {
			if entry.Fingerprint != fingerprint {
				return Entry{}, fmt.Errorf("catalog entry %s fingerprint differs", entry.Ref())
			}
			return cloneEntry(entry), nil
		}
	}
	return Entry{}, fmt.Errorf("catalog has no graph %s@%d", graphID, revision)
}

type Query struct {
	Stages      []Stage
	Tags        []string
	InputTypes  []string
	OutputTypes []string
	Effects     []string
	Limit       int
}

// Discover returns entries matching every requested constraint. Type filters
// are exact canonical protocol/payload strings, not modality booleans.
func (document Document) Discover(query Query) ([]Entry, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	if query.Limit < 0 || query.Limit > maximumQueryResults {
		return nil, fmt.Errorf("catalog discovery limit must be between 0 and %d", maximumQueryResults)
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	stages := make(map[Stage]struct{}, len(query.Stages))
	for _, stage := range query.Stages {
		if !validStage(stage) {
			return nil, fmt.Errorf("catalog discovery has invalid stage %q", stage)
		}
		stages[stage] = struct{}{}
	}
	for _, values := range [][]string{query.Tags, query.InputTypes, query.OutputTypes, query.Effects} {
		for _, value := range values {
			if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") ||
				len(value) > maximumStringBytes {
				return nil, fmt.Errorf("catalog discovery contains non-canonical filter %q", value)
			}
		}
	}
	result := make([]Entry, 0, min(query.Limit, len(document.Entries)))
	for _, entry := range document.Entries {
		if len(stages) != 0 {
			if _, found := stages[entry.Stage]; !found {
				continue
			}
		}
		if !containsAll(entry.Tags, query.Tags) || !entryHasBoundaryTypes(entry, query.InputTypes, query.OutputTypes) ||
			!entryHasEffects(entry, query.Effects) {
			continue
		}
		result = append(result, cloneEntry(entry))
		if len(result) == query.Limit {
			break
		}
	}
	return result, nil
}

func freezeEntry(source Entry) (Entry, error) {
	entry := cloneEntry(source)
	entry.Fingerprint = ""
	canonicalizeEntry(&entry)
	for index := range entry.Nodes {
		canonical, err := canonicalImplementation(entry.Nodes[index].Implementation)
		if err != nil {
			return Entry{}, fmt.Errorf("node %s implementation: %w", entry.Nodes[index].ID, err)
		}
		entry.Nodes[index].Implementation = canonical
	}
	if err := validateEntry(entry); err != nil {
		return Entry{}, err
	}
	fingerprint, err := entryFingerprint(entry)
	if err != nil {
		return Entry{}, err
	}
	entry.Fingerprint = fingerprint
	return entry, nil
}

func validateEntry(entry Entry) error {
	if entry.GraphID == "" || entry.GraphID != strings.TrimSpace(entry.GraphID) || strings.ContainsAny(entry.GraphID, "\x00\r\n") {
		return errors.New("catalog entry requires a canonical graph ID")
	}
	if entry.Revision == 0 {
		return fmt.Errorf("catalog entry %s revision must be positive", entry.GraphID)
	}
	if err := entry.Plan.Validate(); err != nil {
		return fmt.Errorf("catalog entry %s plan: %w", entry.Ref(), err)
	}
	if entry.Plan.GraphID != entry.GraphID || entry.Plan.GraphRevision != entry.Revision {
		return fmt.Errorf("catalog entry %s disagrees with plan identity", entry.Ref())
	}
	if err := entry.Graph.Validate(); err != nil {
		return fmt.Errorf("catalog entry %s Graph IR: %w", entry.Ref(), err)
	}
	if entry.Graph.ID != entry.GraphID || entry.Graph.Revision != entry.Revision ||
		entry.Graph.Fingerprint != entry.Plan.GraphFingerprint {
		return fmt.Errorf("catalog entry %s Graph IR disagrees with plan identity", entry.Ref())
	}
	if !validStage(entry.Stage) {
		return fmt.Errorf("catalog entry %s has invalid stage %q", entry.Ref(), entry.Stage)
	}
	for label, value := range map[string]string{"summary": entry.Summary, "change": entry.Change} {
		if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r") || len(value) > 64<<10 {
			return fmt.Errorf("catalog entry %s has non-canonical %s", entry.Ref(), label)
		}
	}
	if len(entry.Tags) > maximumMetadataItems || len(entry.Profiles) > maximumMetadataItems ||
		len(entry.Lineage) > maximumMetadataItems {
		return fmt.Errorf("catalog entry %s exceeds metadata item limits", entry.Ref())
	}
	seenTags := make(map[string]struct{}, len(entry.Tags))
	for _, tag := range entry.Tags {
		if !tagPattern.MatchString(tag) {
			return fmt.Errorf("catalog entry %s has invalid tag %q", entry.Ref(), tag)
		}
		if _, duplicate := seenTags[tag]; duplicate {
			return fmt.Errorf("catalog entry %s repeats tag %q", entry.Ref(), tag)
		}
		seenTags[tag] = struct{}{}
	}
	if !slices.Equal(entry.Lineage, entry.Graph.Lineage) {
		return fmt.Errorf("catalog entry %s lineage differs from Graph IR", entry.Ref())
	}
	seenBoundaries := make(map[string]struct{}, len(entry.Boundaries))
	for _, boundary := range entry.Boundaries {
		if boundary.Name == "" || boundary.Type == "" {
			return fmt.Errorf("catalog entry %s has incomplete boundary metadata", entry.Ref())
		}
		switch boundary.Direction {
		case ir.InputBoundary, ir.OutputBoundary:
		default:
			return fmt.Errorf("catalog entry %s boundary %s has invalid direction", entry.Ref(), boundary.Name)
		}
		key := string(boundary.Direction) + ":" + boundary.Name
		if _, duplicate := seenBoundaries[key]; duplicate {
			return fmt.Errorf("catalog entry %s repeats boundary %s", entry.Ref(), key)
		}
		seenBoundaries[key] = struct{}{}
	}
	seenNodes := make(map[string]struct{}, len(entry.Nodes))
	graphNodes := make(map[string]ir.Node, len(entry.Graph.Nodes))
	for _, node := range entry.Graph.Nodes {
		graphNodes[node.ID] = node
	}
	for _, node := range entry.Nodes {
		if node.ID == "" {
			return fmt.Errorf("catalog entry %s has empty node ID", entry.Ref())
		}
		if _, duplicate := seenNodes[node.ID]; duplicate {
			return fmt.Errorf("catalog entry %s repeats node %s", entry.Ref(), node.ID)
		}
		seenNodes[node.ID] = struct{}{}
		if node.Element != node.Implementation.Contract {
			return fmt.Errorf("catalog entry %s node %s implementation contract differs", entry.Ref(), node.ID)
		}
		graphNode, found := graphNodes[node.ID]
		if !found || graphNode.Element != node.Element || graphNode.Implementation != node.Implementation.Reference ||
			graphNode.ConfigSchema != node.ConfigSchema || graphNode.StateSchema != node.StateSchema {
			return fmt.Errorf("catalog entry %s node %s metadata differs from Graph IR", entry.Ref(), node.ID)
		}
		if err := node.Implementation.Artifact.Validate(); err != nil {
			return fmt.Errorf("catalog entry %s node %s artifact: %w", entry.Ref(), node.ID, err)
		}
	}
	if len(seenNodes) != len(graphNodes) {
		return fmt.Errorf("catalog entry %s node metadata does not cover Graph IR", entry.Ref())
	}
	seenDependencies := make(map[string]struct{}, len(entry.Dependencies))
	for _, dependency := range entry.Dependencies {
		if dependency.Name == "" {
			return fmt.Errorf("catalog entry %s has empty dependency", entry.Ref())
		}
		if _, duplicate := seenDependencies[dependency.Name]; duplicate {
			return fmt.Errorf("catalog entry %s repeats dependency %s", entry.Ref(), dependency.Name)
		}
		seenDependencies[dependency.Name] = struct{}{}
		if !dependency.Optional && dependency.Artifact == nil {
			return fmt.Errorf("catalog entry %s required dependency %s lacks preflight artifact", entry.Ref(), dependency.Name)
		}
		if dependency.Artifact == nil {
			if dependency.Scope != "" {
				return fmt.Errorf("catalog entry %s unresolved dependency %s has a scope", entry.Ref(), dependency.Name)
			}
		} else {
			if err := dependency.Artifact.Validate(); err != nil {
				return fmt.Errorf("catalog entry %s dependency %s: %w", entry.Ref(), dependency.Name, err)
			}
			if err := dependency.Scope.Validate(); err != nil {
				return fmt.Errorf("catalog entry %s dependency %s: %w", entry.Ref(), dependency.Name, err)
			}
		}
	}
	seenEffects := make(map[string]struct{}, len(entry.Effects))
	for _, effect := range entry.Effects {
		if _, duplicate := seenEffects[effect.Name]; duplicate {
			return fmt.Errorf("catalog entry %s repeats effect %s", entry.Ref(), effect.Name)
		}
		seenEffects[effect.Name] = struct{}{}
	}
	if entry.Channels.Count < 0 || entry.Channels.Lossy < 0 || entry.Channels.Lossy > entry.Channels.Count ||
		entry.Channels.MaxDepth < 0 {
		return fmt.Errorf("catalog entry %s has invalid channel summary", entry.Ref())
	}
	seenProfiles := make(map[inspect.ArtifactIdentity]struct{}, len(entry.Profiles))
	for _, profile := range entry.Profiles {
		if err := profile.Validate(); err != nil {
			return fmt.Errorf("catalog entry %s profile: %w", entry.Ref(), err)
		}
		if _, duplicate := seenProfiles[profile]; duplicate {
			return fmt.Errorf("catalog entry %s repeats profile %+v", entry.Ref(), profile)
		}
		seenProfiles[profile] = struct{}{}
	}
	resolution := graphconfig.Resolution{
		Nodes: make([]graphconfig.NodeResolution, 0, len(entry.Nodes)),
	}
	for _, node := range entry.Nodes {
		resolution.Nodes = append(resolution.Nodes, graphconfig.NodeResolution{
			NodeID: node.ID, Implementation: node.Implementation,
		})
	}
	for _, dependency := range entry.Dependencies {
		if dependency.Artifact != nil {
			resolution.Dependencies = append(resolution.Dependencies, graphconfig.DependencyResolution{
				Name: dependency.Name, Artifact: *dependency.Artifact, Scope: dependency.Scope,
			})
		}
	}
	resolutionFingerprint, err := resolution.Fingerprint()
	if err != nil {
		return fmt.Errorf("catalog entry %s resolution: %w", entry.Ref(), err)
	}
	if resolutionFingerprint != entry.Plan.ResolutionDigest {
		return fmt.Errorf("catalog entry %s resolution metadata differs from plan identity", entry.Ref())
	}
	if err := validateDerivedMetadata(entry); err != nil {
		return fmt.Errorf("catalog entry %s: %w", entry.Ref(), err)
	}
	return nil
}

func canonicalizeEntry(entry *Entry) {
	if frozen, err := ir.Freeze(entry.Graph); err == nil {
		entry.Graph = frozen
	}
	sort.Strings(entry.Tags)
	sort.Strings(entry.Lineage)
	sort.Slice(entry.Boundaries, func(left, right int) bool {
		if entry.Boundaries[left].Direction != entry.Boundaries[right].Direction {
			return entry.Boundaries[left].Direction < entry.Boundaries[right].Direction
		}
		return entry.Boundaries[left].Name < entry.Boundaries[right].Name
	})
	sort.Slice(entry.Nodes, func(left, right int) bool { return entry.Nodes[left].ID < entry.Nodes[right].ID })
	sort.Slice(entry.Dependencies, func(left, right int) bool {
		return entry.Dependencies[left].Name < entry.Dependencies[right].Name
	})
	sort.Slice(entry.Effects, func(left, right int) bool { return entry.Effects[left].Name < entry.Effects[right].Name })
	sort.Slice(entry.Profiles, func(left, right int) bool {
		if entry.Profiles[left].ID != entry.Profiles[right].ID {
			return entry.Profiles[left].ID < entry.Profiles[right].ID
		}
		if entry.Profiles[left].Revision != entry.Profiles[right].Revision {
			return entry.Profiles[left].Revision < entry.Profiles[right].Revision
		}
		return entry.Profiles[left].Digest < entry.Profiles[right].Digest
	})
}

func canonicalizeDocument(document *Document) {
	sort.Slice(document.Entries, func(left, right int) bool {
		if document.Entries[left].GraphID != document.Entries[right].GraphID {
			return document.Entries[left].GraphID < document.Entries[right].GraphID
		}
		return document.Entries[left].Revision < document.Entries[right].Revision
	})
}

func entryFingerprint(entry Entry) (string, error) {
	entry.Fingerprint = ""
	return digest("entry", entry)
}

func documentFingerprint(document Document) (string, error) {
	document.Fingerprint = ""
	return digest("catalog", document)
}

func digest(domain string, value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte("openrealtime.graph-catalog/" + domain + "/v1\x00"))
	hash.Write(payload)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func cloneEntry(source Entry) Entry {
	result := source
	if frozen, err := ir.Freeze(source.Graph); err == nil {
		result.Graph = frozen
	}
	result.Tags = slices.Clone(source.Tags)
	result.Lineage = slices.Clone(source.Lineage)
	result.Boundaries = slices.Clone(source.Boundaries)
	result.Nodes = make([]Node, len(source.Nodes))
	for index, node := range source.Nodes {
		result.Nodes[index] = node
		result.Nodes[index].Implementation = cloneImplementation(node.Implementation)
	}
	result.Dependencies = make([]Dependency, len(source.Dependencies))
	for index, dependency := range source.Dependencies {
		result.Dependencies[index] = dependency
		if dependency.Artifact != nil {
			artifact := *dependency.Artifact
			result.Dependencies[index].Artifact = &artifact
		}
	}
	result.Effects = slices.Clone(source.Effects)
	result.Profiles = slices.Clone(source.Profiles)
	return result
}

func cloneImplementation(source graphconfig.ImplementationResolution) graphconfig.ImplementationResolution {
	result := source
	result.Placements = slices.Clone(source.Placements)
	result.Transports = slices.Clone(source.Transports)
	result.ResourceKeys = slices.Clone(source.ResourceKeys)
	result.SecretSlots = slices.Clone(source.SecretSlots)
	result.Capabilities = make([]inspect.CapabilityIdentity, len(source.Capabilities))
	for index, capability := range source.Capabilities {
		result.Capabilities[index] = capability
		if capability.Adapter != nil {
			adapter := *capability.Adapter
			result.Capabilities[index].Adapter = &adapter
		}
	}
	return result
}

func canonicalImplementation(source graphconfig.ImplementationResolution) (graphconfig.ImplementationResolution, error) {
	result := cloneImplementation(source)
	if result.Reference == "" || result.Reference != strings.TrimSpace(result.Reference) ||
		strings.ContainsAny(result.Reference, "\x00\r\n") {
		return graphconfig.ImplementationResolution{}, errors.New("reference is not canonical")
	}
	if err := element.ValidateIdentity(result.Contract); err != nil {
		return graphconfig.ImplementationResolution{}, fmt.Errorf("contract: %w", err)
	}
	if err := result.Artifact.Validate(); err != nil {
		return graphconfig.ImplementationResolution{}, fmt.Errorf("artifact: %w", err)
	}
	switch result.Evidence {
	case inspect.EvidenceDeclared, inspect.EvidenceRegistered:
	default:
		return graphconfig.ImplementationResolution{}, fmt.Errorf("invalid preflight evidence %q", result.Evidence)
	}
	var err error
	for _, item := range []struct {
		name   string
		values *[]string
	}{
		{"placement", &result.Placements}, {"transport", &result.Transports},
		{"resource key", &result.ResourceKeys}, {"secret slot", &result.SecretSlots},
	} {
		*item.values, err = canonicalStrings(item.name, *item.values)
		if err != nil {
			return graphconfig.ImplementationResolution{}, err
		}
	}
	result.Capabilities, err = inspect.CanonicalCapabilities(result.Capabilities)
	if err != nil {
		return graphconfig.ImplementationResolution{}, err
	}
	return result, nil
}

func canonicalStrings(kind string, source []string) ([]string, error) {
	result := slices.Clone(source)
	for _, value := range result {
		if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("%s %q is not canonical", kind, value)
		}
	}
	sort.Strings(result)
	return slices.Compact(result), nil
}

func validStage(stage Stage) bool {
	switch stage {
	case Experimental, Candidate, Stable, Retired:
		return true
	default:
		return false
	}
}

func containsAll(have, wanted []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, value := range have {
		set[value] = struct{}{}
	}
	for _, value := range wanted {
		if _, found := set[value]; !found {
			return false
		}
	}
	return true
}

func entryHasBoundaryTypes(entry Entry, inputs, outputs []string) bool {
	haveInputs := make([]string, 0)
	haveOutputs := make([]string, 0)
	for _, boundary := range entry.Boundaries {
		if boundary.Direction == ir.InputBoundary {
			haveInputs = append(haveInputs, boundary.Type)
		} else {
			haveOutputs = append(haveOutputs, boundary.Type)
		}
	}
	return containsAll(haveInputs, inputs) && containsAll(haveOutputs, outputs)
}

func entryHasEffects(entry Entry, wanted []string) bool {
	have := make([]string, 0, len(entry.Effects))
	for _, effect := range entry.Effects {
		have = append(have, effect.Name)
	}
	return containsAll(have, wanted)
}

func preflightJSON(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	depth := 0
	tokens := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("preflight graph catalog JSON: %w", err)
		}
		tokens++
		if tokens > maximumJSONTokens {
			return fmt.Errorf("graph catalog JSON exceeds %d tokens", maximumJSONTokens)
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > maximumJSONDepth {
					return fmt.Errorf("graph catalog JSON exceeds nesting depth %d", maximumJSONDepth)
				}
			case '}', ']':
				depth--
			}
		}
	}
}

func validateDerivedMetadata(entry Entry) error {
	wantBoundaries := make([]Boundary, 0, len(entry.Graph.Boundaries))
	for _, boundary := range entry.Graph.Boundaries {
		wantBoundaries = append(wantBoundaries, Boundary{
			Name: boundary.Name, Direction: boundary.Direction, Type: boundary.Type.String(),
		})
	}
	sort.Slice(wantBoundaries, func(left, right int) bool {
		if wantBoundaries[left].Direction != wantBoundaries[right].Direction {
			return wantBoundaries[left].Direction < wantBoundaries[right].Direction
		}
		return wantBoundaries[left].Name < wantBoundaries[right].Name
	})
	if !slices.Equal(entry.Boundaries, wantBoundaries) {
		return errors.New("boundary metadata differs from Graph IR")
	}

	type dependencyContract struct {
		Name     string
		Optional bool
	}
	dependencyContracts := make(map[string]dependencyContract)
	effects := make(map[string]element.Effect)
	for _, node := range entry.Graph.Nodes {
		for _, dependency := range node.Dependencies {
			current, found := dependencyContracts[dependency.Name]
			if !found || current.Optional && !dependency.Optional {
				dependencyContracts[dependency.Name] = dependencyContract{
					Name: dependency.Name, Optional: dependency.Optional,
				}
			}
		}
		for _, effect := range node.Effects {
			effects[effect.Name] = effect
		}
	}
	if len(entry.Dependencies) != len(dependencyContracts) {
		return errors.New("dependency metadata does not cover Graph IR")
	}
	for _, actual := range entry.Dependencies {
		expected, found := dependencyContracts[actual.Name]
		if !found || expected.Optional != actual.Optional {
			return errors.New("dependency metadata differs from Graph IR")
		}
	}
	wantEffects := make([]element.Effect, 0, len(effects))
	for _, effect := range effects {
		wantEffects = append(wantEffects, effect)
	}
	sort.Slice(wantEffects, func(left, right int) bool { return wantEffects[left].Name < wantEffects[right].Name })
	if !slices.Equal(entry.Effects, wantEffects) {
		return errors.New("effect metadata differs from Graph IR")
	}
	wantChannels := ChannelSummary{}
	for _, edge := range entry.Graph.Edges {
		wantChannels.Count++
		if edge.Delivery == ir.Lossy {
			wantChannels.Lossy++
		}
		wantChannels.MaxDepth = max(wantChannels.MaxDepth, edge.Depth)
	}
	if entry.Channels != wantChannels {
		return errors.New("channel metadata differs from Graph IR")
	}
	return nil
}

var tagPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
