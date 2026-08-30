package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/deployment"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// Artifacts are the separate inputs to one immutable plan. Channels is
// optional and defaults to descriptor queue depths; every other artifact is
// required for a normal locked deployment.
type Artifacts struct {
	Topology   Artifact
	Values     Artifact
	Lock       Artifact
	Channels   Artifact
	Deployment Artifact
}

type Options struct {
	Catalog        *resolve.Catalog
	Loader         graph.SourceLoader
	SchemaResolver schema.Resolver
	Discovery      Discovery
	SecretCatalog  *graphsecret.Document

	// OptionalDependencies explicitly selects descriptor-declared optional
	// services for this immutable plan. Selection and exact service artifacts
	// are frozen into ResolutionDigest; an unselected optional service cannot
	// appear later as ambient mount state.
	OptionalDependencies []string

	Revision uint64
	Limits   Limits

	// AllowDeclaredImplementations is compatibility-only. Production plans
	// should require registered artifact evidence. It is needed when using
	// LegacyDescriptorDiscovery during the documented migration window.
	AllowDeclaredImplementations bool
}

// Create parses, resolves, validates, binds, and fingerprints every artifact
// without acquiring runtime resources. The returned Plan is suitable for a
// later runtime adapter only after its identity is rechecked at mount/startup.
func Create(ctx context.Context, artifacts Artifacts, options Options) (*Plan, error) {
	if ctx == nil {
		return nil, errors.New("create graph plan: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if options.Catalog == nil {
		return nil, errors.New("create graph plan: descriptor catalog is required")
	}
	if options.Discovery == nil {
		return nil, errors.New("create graph plan: implementation discovery is required")
	}
	limits, normalized, err := normalizeArtifacts(artifacts, options.Limits)
	if err != nil {
		return nil, err
	}
	artifacts = normalized

	file, err := parseTopology(artifacts.Topology, limits)
	if err != nil {
		return nil, err
	}
	channels, channelsDigest, err := parseChannels(artifacts.Channels, file.Graph.Name, limits)
	if err != nil {
		return nil, err
	}
	lock, lockPayload, err := parseLock(artifacts.Lock, limits)
	if err != nil {
		return nil, err
	}
	compiled, err := graph.Compile(file, graph.Options{
		Catalog: options.Catalog, Lock: lock, ResolutionMode: resolve.Locked,
		Revision: options.Revision, ChannelDepth: channels.depths(), Loader: options.Loader,
	})
	if err != nil {
		return nil, fmt.Errorf("create graph plan: compile locked topology: %w", err)
	}
	if !compiled.Lock.Equal(lock) {
		return nil, errors.New("create graph plan: resolution lock is not the exact minimal lock consumed by the topology")
	}
	if err := validateGraphLimits(compiled.Graph, compiled.Lock, limits); err != nil {
		return nil, err
	}

	valuesDocument, err := parseValues(artifacts.Values, limits)
	if err != nil {
		return nil, err
	}
	boundValues, err := graphvalues.Bind(compiled.Graph, valuesDocument)
	if err != nil {
		return nil, fmt.Errorf("create graph plan: bind values: %w", err)
	}
	valuesSchema, err := schema.Generate(ctx, boundValues.Graph, options.Catalog, schema.Options{
		Resolver: options.SchemaResolver, RequireResolved: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create graph plan: resolve typed values schema: %w", err)
	}
	if err := validateTypedValues(valuesSchema, boundValues.Graph.ID, boundValues.Values, limits); err != nil {
		return nil, fmt.Errorf("create graph plan: %w", err)
	}

	deploymentDocument, err := parseDeployment(artifacts.Deployment, limits)
	if err != nil {
		return nil, err
	}
	boundDeployment, err := deployment.Bind(boundValues.Graph, deploymentDocument)
	if err != nil {
		return nil, fmt.Errorf("create graph plan: bind deployment: %w", err)
	}
	if err := validateSecrets(boundDeployment.Nodes, options.SecretCatalog); err != nil {
		return nil, fmt.Errorf("create graph plan: %w", err)
	}
	secretCatalogFingerprint, err := SecretCatalogFingerprint(options.SecretCatalog)
	if err != nil {
		return nil, fmt.Errorf("create graph plan: secret catalog identity: %w", err)
	}
	resolution, err := resolvePlan(ctx, boundDeployment.Graph, boundDeployment.Nodes, options)
	if err != nil {
		return nil, fmt.Errorf("create graph plan: %w", err)
	}

	publicDeployment, err := publicDeploymentDigest(boundDeployment.Graph)
	if err != nil {
		return nil, err
	}
	resolutionDigest, err := digestResolution(resolution)
	if err != nil {
		return nil, err
	}
	identity := Identity{
		FormatVersion: PlanFormatVersion,
		GraphID:       boundDeployment.Graph.ID, GraphRevision: boundDeployment.Graph.Revision,
		SourceDigest: artifactDigest("source", artifacts.Topology.Encoding, artifacts.Topology.Data),
		LockDigest:   artifactDigest("lock", JSON, lockPayload),
		ValuesDigest: boundValues.Fingerprint, ChannelsDigest: channelsDigest,
		ValuesSchemaDigest:     valuesSchema.Digest,
		PublicDeploymentDigest: publicDeployment, ResolutionDigest: resolutionDigest,
		GraphFingerprint: boundDeployment.Graph.Fingerprint,
	}
	identity.PlanFingerprint, err = planFingerprint(identity)
	if err != nil {
		return nil, err
	}
	plan := &Plan{
		source: artifacts.Topology, graph: boundDeployment.Graph, lock: compiled.Lock,
		values: cloneValues(boundValues.Values), channels: cloneChannels(channels),
		deployment:                      cloneDeployment(boundDeployment.Nodes),
		privateDeploymentFingerprint:    boundDeployment.Fingerprint,
		privateSecretCatalogFingerprint: secretCatalogFingerprint,
		schema:                          cloneSchema(valuesSchema), resolution: cloneResolution(resolution), identity: identity,
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("create graph plan: internal validation: %w", err)
	}
	return plan, nil
}

// UpdatedLock is the result of the only API that deliberately selects latest
// descriptor revisions. It is not a launch plan; callers must review/store its
// lock and call Create in locked mode.
type UpdatedLock struct {
	lock           resolve.Lock
	graph          ir.Graph
	sourceDigest   string
	channelsDigest string
}

func (updated UpdatedLock) Lock() resolve.Lock {
	canonical, err := updated.lock.Canonical()
	if err != nil {
		return resolve.Lock{}
	}
	return canonical
}

func (updated UpdatedLock) Graph() ir.Graph {
	frozen, err := ir.Freeze(updated.graph)
	if err != nil {
		return ir.Graph{}
	}
	return frozen
}

func (updated UpdatedLock) SourceDigest() string   { return updated.sourceDigest }
func (updated UpdatedLock) ChannelsDigest() string { return updated.channelsDigest }

// UpdateLock explicitly resolves the latest registered descriptor revisions.
// It performs no values, deployment, implementation, secret, or launch work.
func UpdateLock(
	ctx context.Context, topology Artifact, channelsArtifact Artifact, options Options,
) (UpdatedLock, error) {
	if ctx == nil {
		return UpdatedLock{}, errors.New("update graph lock: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return UpdatedLock{}, err
	}
	if options.Catalog == nil {
		return UpdatedLock{}, errors.New("update graph lock: descriptor catalog is required")
	}
	limits, err := options.Limits.normalized()
	if err != nil {
		return UpdatedLock{}, err
	}
	topology, err = normalizeArtifact("topology", topology, limits.MaxTopologyBytes, true, limits)
	if err != nil {
		return UpdatedLock{}, err
	}
	channelsArtifact, err = normalizeArtifact("channels", channelsArtifact, limits.MaxChannelsBytes, false, limits)
	if err != nil {
		return UpdatedLock{}, err
	}
	if len(topology.Data)+len(channelsArtifact.Data) > limits.MaxTotalBytes {
		return UpdatedLock{}, fmt.Errorf("graph lock update artifacts exceed %d total bytes", limits.MaxTotalBytes)
	}
	file, err := parseTopology(topology, limits)
	if err != nil {
		return UpdatedLock{}, err
	}
	channels, channelsDigest, err := parseChannels(channelsArtifact, file.Graph.Name, limits)
	if err != nil {
		return UpdatedLock{}, err
	}
	compiled, err := graph.Compile(file, graph.Options{
		Catalog: options.Catalog, ResolutionMode: resolve.Update,
		Revision: options.Revision, ChannelDepth: channels.depths(), Loader: options.Loader,
	})
	if err != nil {
		return UpdatedLock{}, fmt.Errorf("update graph lock: %w", err)
	}
	if err := validateGraphLimits(compiled.Graph, compiled.Lock, limits); err != nil {
		return UpdatedLock{}, err
	}
	return UpdatedLock{
		lock: compiled.Lock, graph: compiled.Graph,
		sourceDigest:   artifactDigest("source", topology.Encoding, topology.Data),
		channelsDigest: channelsDigest,
	}, nil
}

func normalizeArtifacts(artifacts Artifacts, source Limits) (Limits, Artifacts, error) {
	limits, err := source.normalized()
	if err != nil {
		return Limits{}, Artifacts{}, err
	}
	items := []struct {
		kind     string
		artifact *Artifact
		maximum  int
		required bool
	}{
		{"topology", &artifacts.Topology, limits.MaxTopologyBytes, true},
		{"values", &artifacts.Values, limits.MaxValuesBytes, true},
		{"lock", &artifacts.Lock, limits.MaxLockBytes, true},
		{"channels", &artifacts.Channels, limits.MaxChannelsBytes, false},
		{"deployment", &artifacts.Deployment, limits.MaxDeploymentBytes, true},
	}
	total := 0
	for _, item := range items {
		normalized, normalizeErr := normalizeArtifact(item.kind, *item.artifact, item.maximum, item.required, limits)
		if normalizeErr != nil {
			return Limits{}, Artifacts{}, normalizeErr
		}
		*item.artifact = normalized
		if len(normalized.Data) > limits.MaxTotalBytes-total {
			return Limits{}, Artifacts{}, fmt.Errorf("graph configuration artifacts exceed %d total bytes", limits.MaxTotalBytes)
		}
		total += len(normalized.Data)
	}
	return limits, artifacts, nil
}

func parseTopology(artifact Artifact, limits Limits) (syntax.File, error) {
	switch artifact.Encoding {
	case ORTG:
		return syntax.Parse(artifact.Path, artifact.Data)
	case JSON:
		if err := preflightJSON(artifact.Path, artifact.Data, limits); err != nil {
			return syntax.File{}, err
		}
		return manifest.ParseJSON(artifact.Path, artifact.Data)
	case YAML:
		if err := preflightYAML(artifact.Path, artifact.Data, limits); err != nil {
			return syntax.File{}, err
		}
		return manifest.ParseYAML(artifact.Path, artifact.Data)
	default:
		return syntax.File{}, fmt.Errorf("topology artifact %s has unsupported encoding %q", artifact.Path, artifact.Encoding)
	}
}

func parseLock(artifact Artifact, limits Limits) (resolve.Lock, []byte, error) {
	if artifact.Encoding != JSON {
		return resolve.Lock{}, nil, fmt.Errorf("resolution lock %s must use JSON", artifact.Path)
	}
	if err := preflightJSON(artifact.Path, artifact.Data, limits); err != nil {
		return resolve.Lock{}, nil, err
	}
	lock, err := resolve.ParseLock(artifact.Data)
	if err != nil {
		return resolve.Lock{}, nil, fmt.Errorf("parse resolution lock %s: %w", artifact.Path, err)
	}
	if len(lock.Entries) > limits.MaxLockEntries {
		return resolve.Lock{}, nil, fmt.Errorf("resolution lock contains %d entries; maximum is %d",
			len(lock.Entries), limits.MaxLockEntries)
	}
	payload, err := lock.Marshal()
	if err != nil {
		return resolve.Lock{}, nil, err
	}
	return lock, payload, nil
}

func parseValues(artifact Artifact, limits Limits) (graphvalues.Document, error) {
	switch artifact.Encoding {
	case JSON:
		if err := preflightJSON(artifact.Path, artifact.Data, limits); err != nil {
			return graphvalues.Document{}, err
		}
		return graphvalues.ParseJSON(artifact.Path, artifact.Data)
	case YAML:
		if err := preflightYAML(artifact.Path, artifact.Data, limits); err != nil {
			return graphvalues.Document{}, err
		}
		return graphvalues.ParseYAML(artifact.Path, artifact.Data)
	default:
		return graphvalues.Document{}, fmt.Errorf("values artifact %s must use JSON or YAML", artifact.Path)
	}
}

func parseDeployment(artifact Artifact, limits Limits) (deployment.Document, error) {
	switch artifact.Encoding {
	case JSON:
		if err := preflightJSON(artifact.Path, artifact.Data, limits); err != nil {
			return deployment.Document{}, err
		}
		return deployment.ParseJSON(artifact.Path, artifact.Data)
	case YAML:
		if err := preflightYAML(artifact.Path, artifact.Data, limits); err != nil {
			return deployment.Document{}, err
		}
		return deployment.ParseYAML(artifact.Path, artifact.Data)
	default:
		return deployment.Document{}, fmt.Errorf("deployment artifact %s must use JSON or YAML", artifact.Path)
	}
}

func validateGraphLimits(graph ir.Graph, lock resolve.Lock, limits Limits) error {
	if len(graph.Nodes) > limits.MaxGraphNodes {
		return fmt.Errorf("compiled graph contains %d nodes; maximum is %d", len(graph.Nodes), limits.MaxGraphNodes)
	}
	if len(graph.Edges) > limits.MaxGraphEdges {
		return fmt.Errorf("compiled graph contains %d edges; maximum is %d", len(graph.Edges), limits.MaxGraphEdges)
	}
	if len(lock.Entries) > limits.MaxLockEntries {
		return fmt.Errorf("compiled lock contains %d entries; maximum is %d", len(lock.Entries), limits.MaxLockEntries)
	}
	return nil
}

func validateTypedValues(
	bundle schema.Bundle, graphID string, effective map[string]json.RawMessage, limits Limits,
) error {
	if !bundle.Complete || len(bundle.Unresolved) != 0 {
		return fmt.Errorf("typed values schema is incomplete: unresolved %s", strings.Join(bundle.Unresolved, ", "))
	}
	document := graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: graphID, Nodes: cloneValues(effective),
	}
	payload, err := graphvalues.MarshalJSON(document)
	if err != nil {
		return fmt.Errorf("encode effective typed values: %w", err)
	}
	if len(payload) > limits.MaxValuesBytes {
		return fmt.Errorf("effective typed values have %d bytes; maximum is %d", len(payload), limits.MaxValuesBytes)
	}
	var schemaDocument any
	schemaDecoder := json.NewDecoder(bytes.NewReader(bundle.Schema))
	schemaDecoder.UseNumber()
	if err := schemaDecoder.Decode(&schemaDocument); err != nil {
		return fmt.Errorf("decode generated values schema: %w", err)
	}
	root, ok := schemaDocument.(map[string]any)
	if !ok {
		return errors.New("generated values schema root is not an object")
	}
	id, ok := root["$id"].(string)
	if !ok || id == "" {
		return errors.New("generated values schema has no canonical $id")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectSchemaLoader{})
	if err := compiler.AddResource(id, schemaDocument); err != nil {
		return fmt.Errorf("load generated values schema: %w", err)
	}
	compiled, err := compiler.Compile(id)
	if err != nil {
		return fmt.Errorf("compile generated values schema: %w", err)
	}
	valuesDecoder := json.NewDecoder(bytes.NewReader(payload))
	valuesDecoder.UseNumber()
	var value any
	if err := valuesDecoder.Decode(&value); err != nil {
		return fmt.Errorf("decode effective typed values: %w", err)
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("values do not satisfy element-owned schemas: %w", err)
	}
	return nil
}

type rejectSchemaLoader struct{}

func (rejectSchemaLoader) Load(resource string) (any, error) {
	return nil, fmt.Errorf("external schema resolution is disabled for %s", resource)
}

func validateSecrets(nodes map[string]deployment.Node, catalog *graphsecret.Document) error {
	requires := false
	for _, node := range nodes {
		if len(node.Secrets) != 0 {
			requires = true
			break
		}
	}
	if catalog == nil {
		if requires {
			return errors.New("deployment uses secret references but no graph-scoped secret catalog was provided")
		}
		return nil
	}
	return deployment.ValidateSecretCatalog(nodes, *catalog)
}

// SecretCatalogFingerprint returns the exact private artifact identity used by
// Plan creation. It covers canonical provider/locator bindings but never
// resolves or hashes credential bytes. Runtime preparation uses this function
// to prove that the Store it snapshots was built from the same reviewed
// catalog as the immutable Plan.
func SecretCatalogFingerprint(catalog *graphsecret.Document) (string, error) {
	if catalog == nil {
		return "", nil
	}
	payload, err := graphsecret.MarshalJSON(*catalog)
	if err != nil {
		return "", err
	}
	return artifactDigest("secret-catalog", JSON, payload), nil
}

func resolvePlan(
	ctx context.Context, graph ir.Graph, nodes map[string]deployment.Node, options Options,
) (Resolution, error) {
	graphNodes := append([]ir.Node(nil), graph.Nodes...)
	sort.Slice(graphNodes, func(left, right int) bool { return graphNodes[left].ID < graphNodes[right].ID })
	result := Resolution{Nodes: make([]NodeResolution, 0, len(graphNodes))}
	requiredDependencies := make(map[string]struct{})
	declaredOptionalDependencies := make(map[string]struct{})
	for _, node := range graphNodes {
		if err := context.Cause(ctx); err != nil {
			return Resolution{}, err
		}
		binding, found := nodes[node.ID]
		if !found {
			return Resolution{}, fmt.Errorf("deployment resolution is missing node %s", node.ID)
		}
		request := ImplementationRequest{
			NodeID: node.ID, Reference: binding.Implementation, Contract: node.Element,
			Placement: binding.Placement, Transport: binding.Transport,
			ResourceKeys: sortedKeys(binding.Resources), SecretSlotKeys: sortedKeys(binding.Secrets),
		}
		resolved, err := options.Discovery.ResolveImplementation(ctx, request)
		if err != nil {
			return Resolution{}, fmt.Errorf("resolve node %s implementation %s: %w", node.ID, binding.Implementation, err)
		}
		canonical, err := canonicalImplementation(resolved)
		if err != nil {
			return Resolution{}, fmt.Errorf("resolve node %s: %w", node.ID, err)
		}
		if canonical.Reference != request.Reference {
			return Resolution{}, fmt.Errorf("node %s discovery returned reference %q, want %q",
				node.ID, canonical.Reference, request.Reference)
		}
		if canonical.Contract != request.Contract {
			return Resolution{}, fmt.Errorf("node %s implementation %s provides %+v, graph requires %+v",
				node.ID, request.Reference, canonical.Contract, request.Contract)
		}
		if canonical.Evidence == "declared" && !options.AllowDeclaredImplementations {
			return Resolution{}, fmt.Errorf("node %s implementation %s has declaration-only evidence", node.ID, request.Reference)
		}
		if request.Placement != "" && !containsCanonical(canonical.Placements, request.Placement) {
			return Resolution{}, fmt.Errorf("node %s implementation %s does not attest placement %q",
				node.ID, request.Reference, request.Placement)
		}
		if request.Transport != "" && !containsCanonical(canonical.Transports, request.Transport) {
			return Resolution{}, fmt.Errorf("node %s implementation %s does not attest transport %q",
				node.ID, request.Reference, request.Transport)
		}
		for _, resource := range request.ResourceKeys {
			if !containsCanonical(canonical.ResourceKeys, resource) {
				return Resolution{}, fmt.Errorf("node %s implementation %s does not attest resource key %q",
					node.ID, request.Reference, resource)
			}
		}
		for _, slot := range request.SecretSlotKeys {
			if !containsCanonical(canonical.SecretSlots, slot) {
				return Resolution{}, fmt.Errorf("node %s implementation %s does not attest secret slot %q",
					node.ID, request.Reference, slot)
			}
		}
		result.Nodes = append(result.Nodes, NodeResolution{NodeID: node.ID, Implementation: canonical})
		for _, dependency := range node.Dependencies {
			if dependency.Optional {
				declaredOptionalDependencies[dependency.Name] = struct{}{}
			} else {
				requiredDependencies[dependency.Name] = struct{}{}
			}
		}
	}
	selectedOptionalDependencies, err := canonicalOptionalDependencySelection(
		options.OptionalDependencies,
	)
	if err != nil {
		return Resolution{}, err
	}
	for _, name := range selectedOptionalDependencies {
		if _, required := requiredDependencies[name]; required {
			return Resolution{}, fmt.Errorf("optional dependency selection %q is already required", name)
		}
		if _, declared := declaredOptionalDependencies[name]; !declared {
			return Resolution{}, fmt.Errorf("optional dependency selection %q is not declared by the graph", name)
		}
		requiredDependencies[name] = struct{}{}
	}
	names := make([]string, 0, len(requiredDependencies))
	for name := range requiredDependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := context.Cause(ctx); err != nil {
			return Resolution{}, err
		}
		resolved, err := options.Discovery.ResolveDependency(ctx, name)
		if err != nil {
			return Resolution{}, fmt.Errorf("resolve required dependency %s: %w", name, err)
		}
		canonical, err := canonicalDependency(resolved)
		if err != nil {
			return Resolution{}, err
		}
		if canonical.Name != name {
			return Resolution{}, fmt.Errorf("dependency discovery returned %q, want %q", canonical.Name, name)
		}
		result.Dependencies = append(result.Dependencies, canonical)
	}
	return result, nil
}

func canonicalOptionalDependencySelection(source []string) ([]string, error) {
	if len(source) > 65_536 {
		return nil, fmt.Errorf("optional dependency selection has %d entries; maximum is %d", len(source), 65_536)
	}
	result := make([]string, len(source))
	seen := make(map[string]struct{}, len(source))
	for index, name := range source {
		if err := canonicalText("optional dependency", name); err != nil {
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("optional dependency selection repeats %q", name)
		}
		seen[name] = struct{}{}
		result[index] = name
	}
	sort.Strings(result)
	return result, nil
}

func sortedKeys[V any](source map[string]V) []string {
	result := make([]string, 0, len(source))
	for key := range source {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func canonicalValue(raw json.RawMessage) (json.RawMessage, string, error) {
	return graphvalues.Digest(raw)
}
