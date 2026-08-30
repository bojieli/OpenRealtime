package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/graph/deployment"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
)

const PlanFormatVersion uint64 = 1

// Identity is the public, deterministic source-to-plan chain. Deployment
// secret references and secret-catalog locators are deliberately excluded;
// PublicDeploymentDigest is derived from the redacted per-node deployment
// digests frozen into Graph IR.
type Identity struct {
	FormatVersion          uint64 `json:"format_version"`
	GraphID                string `json:"graph_id"`
	GraphRevision          uint64 `json:"graph_revision"`
	SourceDigest           string `json:"source_digest"`
	LockDigest             string `json:"lock_digest"`
	ValuesDigest           string `json:"values_digest"`
	ChannelsDigest         string `json:"channels_digest"`
	ValuesSchemaDigest     string `json:"values_schema_digest"`
	PublicDeploymentDigest string `json:"public_deployment_digest"`
	ResolutionDigest       string `json:"resolution_digest"`
	GraphFingerprint       string `json:"graph_fingerprint"`
	PlanFingerprint        string `json:"plan_fingerprint"`
}

func (identity Identity) Validate() error {
	if identity.FormatVersion != PlanFormatVersion {
		return fmt.Errorf("plan identity format %d is unsupported; want %d",
			identity.FormatVersion, PlanFormatVersion)
	}
	if err := canonicalText("plan graph ID", identity.GraphID); err != nil {
		return err
	}
	if identity.GraphRevision == 0 {
		return errors.New("plan graph revision must be positive")
	}
	for label, digest := range map[string]string{
		"source": identity.SourceDigest, "lock": identity.LockDigest,
		"values": identity.ValuesDigest, "channels": identity.ChannelsDigest,
		"values schema":     identity.ValuesSchemaDigest,
		"public deployment": identity.PublicDeploymentDigest,
		"resolution":        identity.ResolutionDigest, "graph": identity.GraphFingerprint,
		"plan": identity.PlanFingerprint,
	} {
		if err := validateDigest(label, digest); err != nil {
			return err
		}
	}
	want, err := planFingerprint(identity)
	if err != nil {
		return err
	}
	if identity.PlanFingerprint != want {
		return fmt.Errorf("plan fingerprint is %q, want %q", identity.PlanFingerprint, want)
	}
	return nil
}

type NodeResolution struct {
	NodeID         string                   `json:"node_id"`
	Implementation ImplementationResolution `json:"implementation"`
}

type Resolution struct {
	Nodes        []NodeResolution       `json:"nodes"`
	Dependencies []DependencyResolution `json:"dependencies,omitempty"`
}

// Fingerprint returns the deterministic identity of a complete preflight
// resolution snapshot. It validates and canonicalizes the snapshot before
// hashing so catalogs can prove that the implementation metadata they publish
// is exactly the metadata selected by Plan creation.
func (resolution Resolution) Fingerprint() (string, error) {
	canonical, err := canonicalResolution(resolution)
	if err != nil {
		return "", err
	}
	return semanticDigest("resolution", canonical)
}

// Plan is immutable after Create. Every accessor returns an independent
// snapshot. Values and deployment bindings remain available to a future
// runtime adapter, while secret catalogs and resolved secret bytes are never
// retained.
type Plan struct {
	source                          Artifact
	graph                           ir.Graph
	lock                            resolve.Lock
	values                          map[string]json.RawMessage
	channels                        ChannelDocument
	deployment                      map[string]deployment.Node
	privateDeploymentFingerprint    string
	privateSecretCatalogFingerprint string
	schema                          schema.Bundle
	resolution                      Resolution
	identity                        Identity
}

func (plan *Plan) Identity() Identity {
	if plan == nil {
		return Identity{}
	}
	return plan.identity
}

func (plan *Plan) Graph() ir.Graph {
	if plan == nil {
		return ir.Graph{}
	}
	cloned, err := ir.Freeze(plan.graph)
	if err != nil {
		return ir.Graph{}
	}
	return cloned
}

func (plan *Plan) Lock() resolve.Lock {
	if plan == nil {
		return resolve.Lock{}
	}
	canonical, err := plan.lock.Canonical()
	if err != nil {
		return resolve.Lock{}
	}
	return canonical
}

func (plan *Plan) Source() Artifact {
	if plan == nil {
		return Artifact{}
	}
	result := plan.source
	result.Data = slices.Clone(plan.source.Data)
	return result
}

func (plan *Plan) Values() map[string]json.RawMessage {
	if plan == nil {
		return nil
	}
	return cloneValues(plan.values)
}

func (plan *Plan) Channels() ChannelDocument {
	if plan == nil {
		return ChannelDocument{}
	}
	return cloneChannels(plan.channels)
}

func (plan *Plan) Deployment() map[string]deployment.Node {
	if plan == nil {
		return nil
	}
	return cloneDeployment(plan.deployment)
}

// DeploymentFingerprint is private deployment identity. It changes when a
// secret reference changes and therefore must not be placed in public catalog
// metadata or default telemetry.
func (plan *Plan) DeploymentFingerprint() string {
	if plan == nil {
		return ""
	}
	return plan.privateDeploymentFingerprint
}

// SecretCatalogFingerprint is private identity for the selected non-secret
// provider/locator bindings. Like DeploymentFingerprint, it must not appear in
// public catalogs or default telemetry.
func (plan *Plan) SecretCatalogFingerprint() string {
	if plan == nil {
		return ""
	}
	return plan.privateSecretCatalogFingerprint
}

func (plan *Plan) ValuesSchema() schema.Bundle {
	if plan == nil {
		return schema.Bundle{}
	}
	return cloneSchema(plan.schema)
}

func (plan *Plan) Resolution() Resolution {
	if plan == nil {
		return Resolution{}
	}
	return cloneResolution(plan.resolution)
}

// Validate rechecks the complete public identity chain and retained launch
// inputs without consulting mutable external catalogs.
func (plan *Plan) Validate() error {
	if plan == nil {
		return errors.New("validate plan: nil plan")
	}
	if err := plan.identity.Validate(); err != nil {
		return fmt.Errorf("validate plan identity: %w", err)
	}
	if err := plan.graph.Validate(); err != nil {
		return fmt.Errorf("validate plan graph: %w", err)
	}
	if plan.graph.ID != plan.identity.GraphID || plan.graph.Revision != plan.identity.GraphRevision ||
		plan.graph.Fingerprint != plan.identity.GraphFingerprint {
		return errors.New("validate plan: graph and identity disagree")
	}
	if artifactDigest("source", plan.source.Encoding, plan.source.Data) != plan.identity.SourceDigest {
		return errors.New("validate plan: source digest changed")
	}
	lockPayload, err := plan.lock.Marshal()
	if err != nil {
		return fmt.Errorf("validate plan lock: %w", err)
	}
	if artifactDigest("lock", JSON, lockPayload) != plan.identity.LockDigest {
		return errors.New("validate plan: lock digest changed")
	}
	channelsDigest, err := channelDigest(plan.channels)
	if err != nil || channelsDigest != plan.identity.ChannelsDigest {
		return errors.New("validate plan: channels digest changed")
	}
	if err := validateChannelsGraph(plan.graph, plan.channels); err != nil {
		return fmt.Errorf("validate plan: %w", err)
	}
	if schemaContentDigest(plan.schema.Schema) != plan.schema.Digest ||
		plan.schema.Digest != plan.identity.ValuesSchemaDigest {
		return errors.New("validate plan: values schema digest changed")
	}
	if !plan.schema.Complete || len(plan.schema.Unresolved) != 0 {
		return errors.New("validate plan: values schema is incomplete")
	}
	if plan.privateDeploymentFingerprint == "" {
		return errors.New("validate plan: private deployment identity is missing")
	}
	if err := validateDigest("private deployment", plan.privateDeploymentFingerprint); err != nil {
		return fmt.Errorf("validate plan: %w", err)
	}
	if plan.privateSecretCatalogFingerprint != "" {
		if err := validateDigest("private secret catalog", plan.privateSecretCatalogFingerprint); err != nil {
			return fmt.Errorf("validate plan: %w", err)
		}
	}
	valuesDigest, err := effectiveValuesDigest(plan.graph.ID, plan.values)
	if err != nil || valuesDigest != plan.identity.ValuesDigest {
		return errors.New("validate plan: values digest changed")
	}
	if err := validateValuesGraph(plan.graph, plan.values); err != nil {
		return fmt.Errorf("validate plan: %w", err)
	}
	rebound, err := deployment.Bind(plan.graph, deployment.Document{
		APIVersion: deployment.APIVersion,
		Graph:      plan.graph.ID,
		Nodes:      cloneDeployment(plan.deployment),
	})
	if err != nil {
		return fmt.Errorf("validate plan: rebind deployment: %w", err)
	}
	if rebound.Fingerprint != plan.privateDeploymentFingerprint ||
		rebound.Graph.Fingerprint != plan.graph.Fingerprint {
		return errors.New("validate plan: deployment snapshot and graph disagree")
	}
	publicDeployment, err := publicDeploymentDigest(plan.graph)
	if err != nil || publicDeployment != plan.identity.PublicDeploymentDigest {
		return errors.New("validate plan: public deployment digest changed")
	}
	if err := validateResolutionGraph(plan.graph, plan.deployment, plan.resolution); err != nil {
		return fmt.Errorf("validate plan: %w", err)
	}
	resolutionDigest, err := plan.resolution.Fingerprint()
	if err != nil || resolutionDigest != plan.identity.ResolutionDigest {
		return errors.New("validate plan: resolution digest changed")
	}
	return nil
}

func planFingerprint(identity Identity) (string, error) {
	identity.PlanFingerprint = ""
	return semanticDigest("plan", identity)
}

func publicDeploymentDigest(graph ir.Graph) (string, error) {
	type item struct {
		Node           string `json:"node"`
		Implementation string `json:"implementation"`
		Reference      string `json:"reference"`
		Digest         string `json:"digest"`
	}
	items := make([]item, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		items = append(items, item{
			Node: node.ID, Implementation: node.Implementation,
			Reference: node.DeploymentReference, Digest: node.DeploymentDigest,
		})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Node < items[right].Node })
	return semanticDigest("public-deployment", struct {
		Graph string `json:"graph"`
		Nodes []item `json:"nodes"`
	}{Graph: graph.ID, Nodes: items})
}

func effectiveValuesDigest(graphID string, values map[string]json.RawMessage) (string, error) {
	digests := make(map[string]string, len(values))
	for node, raw := range values {
		_, digest, err := canonicalValue(raw)
		if err != nil {
			return "", fmt.Errorf("digest plan value %s: %w", node, err)
		}
		digests[node] = digest
	}
	payload, err := json.Marshal(struct {
		Graph string            `json:"graph"`
		Nodes map[string]string `json:"nodes"`
	}{Graph: graphID, Nodes: digests})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func digestResolution(resolution Resolution) (string, error) {
	return resolution.Fingerprint()
}

func canonicalResolution(source Resolution) (Resolution, error) {
	canonical := cloneResolution(source)
	seenNodes := make(map[string]struct{}, len(canonical.Nodes))
	for index := range canonical.Nodes {
		node := &canonical.Nodes[index]
		if err := canonicalText("resolution node ID", node.NodeID); err != nil {
			return Resolution{}, err
		}
		if _, duplicate := seenNodes[node.NodeID]; duplicate {
			return Resolution{}, fmt.Errorf("resolution repeats node %q", node.NodeID)
		}
		seenNodes[node.NodeID] = struct{}{}
		implementation, err := canonicalImplementation(node.Implementation)
		if err != nil {
			return Resolution{}, fmt.Errorf("resolution node %s: %w", node.NodeID, err)
		}
		node.Implementation = implementation
	}
	seenDependencies := make(map[string]struct{}, len(canonical.Dependencies))
	for index := range canonical.Dependencies {
		dependency, err := canonicalDependency(canonical.Dependencies[index])
		if err != nil {
			return Resolution{}, err
		}
		if _, duplicate := seenDependencies[dependency.Name]; duplicate {
			return Resolution{}, fmt.Errorf("resolution repeats dependency %q", dependency.Name)
		}
		seenDependencies[dependency.Name] = struct{}{}
		canonical.Dependencies[index] = dependency
	}
	sort.Slice(canonical.Nodes, func(left, right int) bool {
		return canonical.Nodes[left].NodeID < canonical.Nodes[right].NodeID
	})
	sort.Slice(canonical.Dependencies, func(left, right int) bool {
		return canonical.Dependencies[left].Name < canonical.Dependencies[right].Name
	})
	return canonical, nil
}

func validateValuesGraph(graph ir.Graph, values map[string]json.RawMessage) error {
	if len(values) != len(graph.Nodes) {
		return errors.New("values snapshot does not cover Graph IR")
	}
	for _, node := range graph.Nodes {
		raw, found := values[node.ID]
		if !found {
			return fmt.Errorf("values snapshot is missing node %s", node.ID)
		}
		_, digest, err := canonicalValue(raw)
		if err != nil {
			return fmt.Errorf("values node %s: %w", node.ID, err)
		}
		if node.ConfigReference != "values://"+graph.ID+"/"+node.ID || node.ConfigDigest != digest {
			return fmt.Errorf("values node %s identity differs from Graph IR", node.ID)
		}
	}
	return nil
}

func validateChannelsGraph(graph ir.Graph, channels ChannelDocument) error {
	if channels.Graph != graph.ID {
		return errors.New("channel overlay targets a different graph")
	}
	edges := make(map[string]ir.Edge, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.ID] = edge
	}
	for id, channel := range channels.Edges {
		edge, found := edges[id]
		if !found {
			return fmt.Errorf("channel overlay refers to absent edge %s", id)
		}
		if edge.Depth != channel.Depth {
			return fmt.Errorf("channel overlay depth for edge %s differs from Graph IR", id)
		}
	}
	return nil
}

func validateResolutionGraph(
	graph ir.Graph, bindings map[string]deployment.Node, resolution Resolution,
) error {
	canonical, err := canonicalResolution(resolution)
	if err != nil {
		return err
	}
	if len(canonical.Nodes) != len(graph.Nodes) {
		return errors.New("implementation resolution does not cover Graph IR")
	}
	byNode := make(map[string]ImplementationResolution, len(canonical.Nodes))
	for _, node := range canonical.Nodes {
		byNode[node.NodeID] = node.Implementation
	}
	requiredDependencies := make(map[string]struct{})
	optionalDependencies := make(map[string]struct{})
	for _, node := range graph.Nodes {
		implementation, found := byNode[node.ID]
		binding, bound := bindings[node.ID]
		if !found || !bound {
			return fmt.Errorf("implementation resolution is missing node %s", node.ID)
		}
		if implementation.Contract != node.Element || implementation.Reference != node.Implementation ||
			implementation.Reference != binding.Implementation {
			return fmt.Errorf("implementation resolution for node %s differs from Graph IR or deployment", node.ID)
		}
		if binding.Placement != "" && !containsCanonical(implementation.Placements, binding.Placement) {
			return fmt.Errorf("implementation resolution for node %s lost placement evidence", node.ID)
		}
		if binding.Transport != "" && !containsCanonical(implementation.Transports, binding.Transport) {
			return fmt.Errorf("implementation resolution for node %s lost transport evidence", node.ID)
		}
		for resource := range binding.Resources {
			if !containsCanonical(implementation.ResourceKeys, resource) {
				return fmt.Errorf("implementation resolution for node %s lost resource evidence", node.ID)
			}
		}
		for slot := range binding.Secrets {
			if !containsCanonical(implementation.SecretSlots, slot) {
				return fmt.Errorf("implementation resolution for node %s lost secret-slot evidence", node.ID)
			}
		}
		for _, dependency := range node.Dependencies {
			if dependency.Optional {
				optionalDependencies[dependency.Name] = struct{}{}
			} else {
				requiredDependencies[dependency.Name] = struct{}{}
			}
		}
	}
	resolvedDependencies := make(map[string]struct{}, len(canonical.Dependencies))
	for _, dependency := range canonical.Dependencies {
		resolvedDependencies[dependency.Name] = struct{}{}
		if _, required := requiredDependencies[dependency.Name]; required {
			continue
		}
		if _, optional := optionalDependencies[dependency.Name]; !optional {
			return fmt.Errorf("dependency resolution contains unexpected dependency %s", dependency.Name)
		}
	}
	for name := range requiredDependencies {
		if _, found := resolvedDependencies[name]; !found {
			return fmt.Errorf("dependency resolution is missing required dependency %s", name)
		}
	}
	return nil
}

func validateDigest(label, digest string) error {
	const prefix = "sha256:"
	if len(digest) != len(prefix)+sha256.Size*2 || digest[:len(prefix)] != prefix {
		return fmt.Errorf("%s identity has invalid SHA-256 digest %q", label, digest)
	}
	if _, err := hex.DecodeString(digest[len(prefix):]); err != nil {
		return fmt.Errorf("%s identity has invalid SHA-256 digest: %w", label, err)
	}
	return nil
}

func schemaContentDigest(source []byte) string {
	digest := sha256.Sum256(source)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneValues(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = slices.Clone(value)
	}
	return result
}

func cloneChannels(source ChannelDocument) ChannelDocument {
	result := source
	result.Edges = make(map[string]Channel, len(source.Edges))
	for key, value := range source.Edges {
		result.Edges[key] = value
	}
	return result
}

func cloneDeployment(source map[string]deployment.Node) map[string]deployment.Node {
	result := make(map[string]deployment.Node, len(source))
	for id, node := range source {
		copy := node
		copy.Resources = make(map[string]string, len(node.Resources))
		for key, value := range node.Resources {
			copy.Resources[key] = value
		}
		copy.Secrets = make(map[string]string, len(node.Secrets))
		for key, value := range node.Secrets {
			copy.Secrets[key] = value
		}
		result[id] = copy
	}
	return result
}

func cloneSchema(source schema.Bundle) schema.Bundle {
	result := source
	result.Schema = slices.Clone(source.Schema)
	result.Nodes = slices.Clone(source.Nodes)
	result.Contracts = make([]schema.ContractResolution, len(source.Contracts))
	for index, contract := range source.Contracts {
		result.Contracts[index] = contract
		result.Contracts[index].NodeIDs = slices.Clone(contract.NodeIDs)
	}
	result.Unresolved = slices.Clone(source.Unresolved)
	return result
}

func cloneResolution(source Resolution) Resolution {
	result := Resolution{
		Nodes:        make([]NodeResolution, len(source.Nodes)),
		Dependencies: slices.Clone(source.Dependencies),
	}
	for index, node := range source.Nodes {
		result.Nodes[index] = NodeResolution{
			NodeID: node.NodeID, Implementation: cloneImplementation(node.Implementation),
		}
	}
	return result
}
