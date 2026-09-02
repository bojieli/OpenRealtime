package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	graphvalidate "github.com/bojieli/OpenRealtime/graph/validate"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

type Config struct {
	Graph    ir.Graph
	Registry *Registry
	Values   map[string]json.RawMessage
	Services *ServiceSet
	// Configuration is the exact separate values artifact identity. It is
	// optional for historical/local mounts, but remote attestation must refuse
	// a snapshot without it rather than reconstructing it from node values.
	Configuration *inspect.ArtifactIdentity
	// Deployment is the public, redacted deployment identity. Private
	// deployment and secret-catalog identities are installed only by the
	// sealed plan-preparation path through privatePlanIdentity below.
	Deployment             *inspect.ArtifactIdentity
	privatePlanIdentity    *PrivatePlanIdentity
	registeredCapabilities map[string][]inspect.CapabilityIdentity
	secretStore            *graphsecret.Store
	secretBindings         map[string]map[string]string
	Tracer                 Tracer
	Now                    func() uint64
	ShutdownTimeout        time.Duration
	Inspection             InspectionConfig
	TraceRecording         *TraceRecordingConfig
}

func validateFactoryConfig(node ir.Node, factory element.Factory, value json.RawMessage) error {
	if len(value) == 0 {
		value = json.RawMessage("{}")
	}
	canonical, _, err := graphvalues.Digest(value)
	if err != nil {
		return err
	}
	validator, validates := factory.(element.ConfigValidator)
	switch {
	case node.ConfigSchema == "" && !bytes.Equal(canonical, []byte("{}")):
		return fmt.Errorf("has values but element %s declares no config schema", node.Element.Name)
	case node.ConfigSchema != "" && !validates:
		return fmt.Errorf("declares config schema %q but implementation %q has no config validator",
			node.ConfigSchema, node.Implementation)
	case validates:
		return validator.ValidateConfig(value)
	default:
		return nil
	}
}

// InspectionConfig bounds payload-free per-correlation route retention.
type InspectionConfig struct {
	MaxFlows                 int
	MaxEdgesPerFlow          int
	MaxCorrelationBytes      int
	MaxCausalParentsPerStage int
}

type mountedNode struct {
	id             string
	identity       element.Identity
	implementation string
	runnable       element.Runnable
	scope          *lifecycleScope
	stateSchema    string
	stateTransfer  *element.StateTransferCapabilities
	snapshot       element.StateSnapshotter
	quiesce        element.StateQuiescer
}

type bindingBuilder struct {
	inputs  map[string]map[string]*queue
	outputs map[string]map[string]*queue
}

// Mounted is one fully validated and resource-mounted graph. Run starts the
// reaction loops exactly once; Close is idempotent and unwinds lifecycle
// effects in reverse mount order.
type Mounted struct {
	graph   ir.Graph
	nodes   []mountedNode
	queues  map[string]*queue
	ingress map[string]*outputPort
	egress  map[string]*inputPort
	// boundaries owns the session-long public handles. ingress and egress are
	// the concrete first generation retained for runtime inspection/ownership.
	boundaries *boundaryRouter
	changed    *condition
	timeout    time.Duration
	// lifecycleFailures carries at most one supervised-worker failure per node.
	// It is provisioned before any factory mounts so work started during Mount
	// cannot fail outside the graph supervisor.
	lifecycleFailures chan nodeResult

	mu          sync.Mutex
	started     bool
	closed      bool
	cancel      context.CancelCauseFunc
	done        chan struct{}
	runErr      error
	shutdownErr error
	shutdown    sync.Once

	liveMu              sync.Mutex
	nodeLive            map[string]inspect.NodeLive
	sequence            atomic.Uint64
	flows               *flowTracker
	configuration       *inspect.ArtifactIdentity
	deployment          *inspect.ArtifactIdentity
	deploymentEvidence  *inspect.DeploymentEvidence
	privatePlanIdentity PrivatePlanIdentity
	recorder            *traceRecorder
	clock               func() uint64
}

// Mount validates the exact factory contracts, constructs every bounded
// channel, resolves required services, and mounts elements without starting
// their long-lived run loops.
func Mount(ctx context.Context, config Config) (*Mounted, error) {
	return mount(ctx, config, nil)
}

// mount is also the private restoration boundary used by graph reconciliation.
// Public callers cannot inject unauthenticated state through Config.
func mount(
	ctx context.Context, config Config, restoredSource map[string]json.RawMessage,
) (*Mounted, error) {
	if ctx == nil {
		return nil, errors.New("mount graph: nil context")
	}
	if err := config.Graph.Validate(); err != nil {
		return nil, fmt.Errorf("mount graph: %w", err)
	}
	if coreErrors := graphvalidate.Errors(graphvalidate.Check(config.Graph, graphvalidate.Core)); len(coreErrors) != 0 {
		return nil, fmt.Errorf("mount graph: %s: %s", coreErrors[0].Code, coreErrors[0].Message)
	}
	if config.Registry == nil {
		return nil, errors.New("mount graph: element registry is required")
	}
	if config.Services == nil {
		config.Services = NewServiceSet()
	}
	if config.Configuration != nil {
		if err := config.Configuration.Validate(); err != nil {
			return nil, fmt.Errorf("mount graph configuration identity: %w", err)
		}
	}
	if config.Deployment != nil {
		if err := config.Deployment.Validate(); err != nil {
			return nil, fmt.Errorf("mount graph deployment identity: %w", err)
		}
	}
	if config.privatePlanIdentity != nil && config.Deployment == nil {
		return nil, errors.New("mount graph private deployment identity requires a public deployment identity")
	}
	if config.secretStore != nil && (config.privatePlanIdentity == nil ||
		config.privatePlanIdentity.secretCatalogFingerprint == "") {
		return nil, errors.New("mount graph secret store requires an exact private secret-catalog identity")
	}
	if config.secretStore == nil && len(config.secretBindings) != 0 {
		return nil, errors.New("mount graph secret bindings require a sealed secret store")
	}
	if config.Now == nil {
		origin := time.Now()
		config.Now = func() uint64 { return uint64(time.Since(origin)) }
	}
	services := newMountServices(config.Services, config.Now)
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 5 * time.Second
	}
	if config.Inspection.MaxFlows < 0 || config.Inspection.MaxEdgesPerFlow < 0 ||
		config.Inspection.MaxCausalParentsPerStage < 0 {
		return nil, errors.New("mount graph inspection bounds cannot be negative")
	}
	if config.Inspection.MaxFlows == 0 {
		config.Inspection.MaxFlows = 1024
	}
	if config.Inspection.MaxEdgesPerFlow == 0 {
		config.Inspection.MaxEdgesPerFlow = 256
	}
	if config.Inspection.MaxCorrelationBytes == 0 {
		config.Inspection.MaxCorrelationBytes = 1024
	}
	if config.Inspection.MaxCausalParentsPerStage == 0 {
		config.Inspection.MaxCausalParentsPerStage = inspect.MaximumCausalParentsPerStage
	}
	if config.Inspection.MaxFlows < 1 || config.Inspection.MaxFlows > 1_000_000 ||
		config.Inspection.MaxEdgesPerFlow < 1 || config.Inspection.MaxEdgesPerFlow > 65_536 ||
		config.Inspection.MaxCorrelationBytes < 1 || config.Inspection.MaxCorrelationBytes > 65_536 ||
		config.Inspection.MaxCausalParentsPerStage < 1 ||
		config.Inspection.MaxCausalParentsPerStage > inspect.MaximumCausalParentsPerStage {
		return nil, errors.New("mount graph inspection bounds exceed safety limits")
	}
	for _, edge := range config.Graph.Edges {
		if strings.HasPrefix(edge.ID, ir.BoundaryQueuePrefix) {
			return nil, fmt.Errorf("mount graph edge %q uses reserved boundary queue namespace", edge.ID)
		}
	}
	graphNodes := make(map[string]struct{}, len(config.Graph.Nodes))
	for _, node := range config.Graph.Nodes {
		graphNodes[node.ID] = struct{}{}
	}
	for nodeID, bindings := range config.secretBindings {
		if _, found := graphNodes[nodeID]; !found {
			return nil, fmt.Errorf("mount graph secret bindings name unknown node %q", nodeID)
		}
		if len(bindings) == 0 {
			return nil, fmt.Errorf("mount graph secret bindings for node %q are empty", nodeID)
		}
		for slot, reference := range bindings {
			if err := canonicalRuntimeText("secret slot", slot); err != nil {
				return nil, fmt.Errorf("mount graph node %s: %w", nodeID, err)
			}
			if err := graphsecret.ValidateReference(reference); err != nil {
				return nil, fmt.Errorf("mount graph node %s secret slot %s: invalid private reference", nodeID, slot)
			}
		}
	}
	if err := validateValues(config.Graph, config.Values); err != nil {
		return nil, err
	}
	restoredState, err := validateRestoredState(config.Graph, restoredSource)
	if err != nil {
		return nil, err
	}

	type factoryResolution struct {
		factory    element.Factory
		descriptor element.Descriptor
		artifact   inspect.ArtifactIdentity
		evidence   inspect.ResolutionEvidence
	}
	factories := make(map[string]factoryResolution, len(config.Graph.Nodes))
	for _, node := range config.Graph.Nodes {
		factory, descriptor, artifact, evidence, err := config.Registry.resolve(node.Implementation, node.Element)
		if err != nil {
			return nil, fmt.Errorf("mount graph node %s: %w", node.ID, err)
		}
		if err := verifyNodeContract(node, descriptor); err != nil {
			return nil, fmt.Errorf("mount graph node %s contract verification: %w", node.ID, err)
		}
		nodeSecretBindings := config.secretBindings[node.ID]
		if len(nodeSecretBindings) != 0 && !descriptorRequiresDependency(descriptor, SecretServiceName) {
			return nil, fmt.Errorf("mount graph node %s binds secrets without requiring service %q",
				node.ID, SecretServiceName)
		}
		for _, dependency := range descriptor.Dependencies {
			_, _, found := services.Lookup(dependency.Name)
			if dependency.Name == SecretServiceName {
				found = config.secretStore != nil && len(nodeSecretBindings) != 0
			}
			if !found && !dependency.Optional {
				return nil, fmt.Errorf("mount graph node %s requires unavailable service %q", node.ID, dependency.Name)
			}
		}
		factories[node.ID] = factoryResolution{
			factory: factory, descriptor: descriptor, artifact: artifact, evidence: evidence,
		}
	}
	// Validate every node's values before mounting the first element. Mount may
	// acquire resources and register effects; a later config typo must never
	// require rolling those back just to report an authoring error.
	for _, node := range config.Graph.Nodes {
		value := cloneRaw(config.Values[node.ID])
		factory := factories[node.ID].factory
		if err := validateFactoryConfig(node, factory, value); err != nil {
			return nil, fmt.Errorf("mount graph node %s config: %w", node.ID, err)
		}
	}

	var configuration *inspect.ArtifactIdentity
	if config.Configuration != nil {
		copy := *config.Configuration
		configuration = &copy
	}
	var deploymentIdentity *inspect.ArtifactIdentity
	if config.Deployment != nil {
		copy := *config.Deployment
		deploymentIdentity = &copy
	}
	var privatePlanIdentity PrivatePlanIdentity
	if config.privatePlanIdentity != nil {
		privatePlanIdentity = *config.privatePlanIdentity
	}
	var deploymentEvidence *inspect.DeploymentEvidence
	if deploymentIdentity != nil {
		candidate := inspect.DeploymentEvidence{Public: *deploymentIdentity}
		if config.privatePlanIdentity != nil {
			candidate.PrivateDeploymentFingerprint = privatePlanIdentity.deploymentFingerprint
			candidate.SecretCatalogFingerprint = privatePlanIdentity.secretCatalogFingerprint
		}
		canonical, evidenceErr := inspect.CanonicalDeploymentEvidence(candidate)
		if evidenceErr != nil {
			return nil, fmt.Errorf("mount graph deployment evidence: %w", evidenceErr)
		}
		if config.privatePlanIdentity != nil {
			if evidenceErr := canonical.ValidateExact(); evidenceErr != nil {
				return nil, fmt.Errorf("mount graph deployment evidence: %w", evidenceErr)
			}
		}
		deploymentEvidence = &canonical
	}
	recorder, err := newTraceRecorder(
		config.Graph, config.Configuration, config.Inspection, config.TraceRecording,
	)
	if err != nil {
		return nil, fmt.Errorf("mount graph: %w", err)
	}
	mounted := &Mounted{
		graph: config.Graph, queues: make(map[string]*queue),
		ingress: make(map[string]*outputPort), egress: make(map[string]*inputPort),
		changed: newCondition(), timeout: config.ShutdownTimeout,
		lifecycleFailures: make(chan nodeResult, len(config.Graph.Nodes)),
		done:              make(chan struct{}), nodeLive: make(map[string]inspect.NodeLive),
		flows: newFlowTracker(
			config.Inspection.MaxFlows, config.Inspection.MaxEdgesPerFlow,
			config.Inspection.MaxCorrelationBytes,
			config.Inspection.MaxCausalParentsPerStage,
		),
		configuration: configuration, deployment: deploymentIdentity,
		deploymentEvidence:  deploymentEvidence,
		privatePlanIdentity: privatePlanIdentity,
		recorder:            recorder, clock: config.Now,
	}
	trace := func(queue *queue, kind TraceKind, envelope element.Envelope, occupancy int) {
		atNS := config.Now()
		mounted.flows.record(queue.id, kind, envelope, atNS)
		mounted.recorder.signal()
		if config.Tracer == nil {
			return
		}
		config.Tracer.Record(TraceEvent{
			Kind: kind, AtNS: atNS, Graph: config.Graph.ID,
			Fingerprint: config.Graph.Fingerprint, Channel: queue.id,
			ItemID: envelope.ItemID, TraceID: envelope.TraceID, RunID: envelope.RunID,
			Occupancy: occupancy, Depth: queue.depth,
		})
	}
	builders := make(map[string]*bindingBuilder, len(config.Graph.Nodes))
	portIndex := make(map[string]map[string]ir.Port, len(config.Graph.Nodes))
	for _, node := range config.Graph.Nodes {
		builders[node.ID] = &bindingBuilder{
			inputs: make(map[string]map[string]*queue), outputs: make(map[string]map[string]*queue),
		}
		portIndex[node.ID] = make(map[string]ir.Port, len(node.Ports))
		for _, port := range node.Ports {
			portIndex[node.ID][port.Name] = port
		}
	}

	cleanupChannels := func() {
		for _, queue := range mounted.queues {
			queue.close()
		}
	}
	abortMount := func(current *lifecycleScope) error {
		nodes := mounted.nodes
		if current != nil {
			nodes = append(slices.Clone(nodes), mountedNode{scope: current})
		}
		cleanupErr := cleanupMountedScopes(nodes, config.ShutdownTimeout)
		cleanupChannels()
		mounted.recorder.discard()
		return cleanupErr
	}
	for _, edge := range config.Graph.Edges {
		channel, err := newQueue(edge.ID, edge.Type, edge.Delivery, edge.Depth, mounted.changed, config.Now, trace)
		if err != nil {
			return nil, errors.Join(err, abortMount(nil))
		}
		mounted.queues[edge.ID] = channel
		bindQueue(builders[edge.From.Node].outputs, edge.From.Port, edge.From.Lane, channel)
		bindQueue(builders[edge.To.Node].inputs, edge.To.Port, edge.To.Lane, channel)
	}
	for _, boundary := range config.Graph.Boundaries {
		port := portIndex[boundary.Endpoint.Node][boundary.Endpoint.Port]
		depth := boundaryDepth(port, boundary.Type)
		identity := ir.BoundaryQueuePrefix + boundary.Name
		channel, err := newQueue(identity, boundary.Type, ir.Lossless, depth, mounted.changed, config.Now, trace)
		if err != nil {
			return nil, errors.Join(err, abortMount(nil))
		}
		mounted.queues[identity] = channel
		if boundary.Direction == ir.InputBoundary {
			bindQueue(builders[boundary.Endpoint.Node].inputs, boundary.Endpoint.Port, boundary.Endpoint.Lane, channel)
			mounted.ingress[boundary.Name] = &outputPort{
				name: boundary.Name, typ: boundary.Type, queues: []*queue{channel}, changed: mounted.changed,
			}
		} else {
			bindQueue(builders[boundary.Endpoint.Node].outputs, boundary.Endpoint.Port, boundary.Endpoint.Lane, channel)
			mounted.egress[boundary.Name] = &inputPort{
				name: boundary.Name, typ: boundary.Type, queues: []*queue{channel}, changed: mounted.changed,
			}
		}
	}

	for _, node := range config.Graph.Nodes {
		ports, err := buildPortSet(node, builders[node.ID], mounted.changed, newNodeTelemetry(mounted, node))
		if err != nil {
			primary := fmt.Errorf("mount graph node %s ports: %w", node.ID, err)
			return nil, errors.Join(primary, abortMount(nil))
		}
		nodeID := node.ID
		scope := newLifecycleScope(ctx, nodeID, func(failure error) {
			// lifecycleScope invokes this callback once. The channel has one slot
			// per node and exists before the first mount, so this send cannot make
			// element cleanup depend on the Run loop already being active.
			mounted.lifecycleFailures <- nodeResult{id: nodeID, err: failure}
		})
		nodeServices := element.Services(services)
		if bindings := config.secretBindings[node.ID]; len(bindings) != 0 {
			nodeServices = nodeMountServices{
				base: services,
				secrets: &nodeSecretAccess{
					node: node.ID, bindings: cloneRuntimeStringMap(bindings),
					store: config.secretStore, lifecycle: scope, mounted: mounted,
				},
			}
		}
		nodeServices = bindDeclaredServices(nodeServices, node.Dependencies)
		value := cloneRaw(config.Values[node.ID])
		if len(value) == 0 {
			value = json.RawMessage("{}")
		}
		resolution := factories[node.ID]
		liveResolution := &inspect.NodeResolution{
			Element: node.Element, Implementation: node.Implementation,
			Runtime: resolution.artifact, RuntimeEvidence: resolution.evidence,
		}
		if capabilities, found := config.registeredCapabilities[node.ID]; found {
			liveResolution.Capabilities = cloneRuntimeCapabilities(capabilities)
			liveResolution.CapabilitiesEvidence = inspect.EvidenceRegistered
		}
		mounted.nodeLive[node.ID] = inspect.NodeLive{
			State: "mounted", Resolution: liveResolution,
		}
		var state *stateLifecycle
		if node.StateTransfer != nil {
			state = newStateLifecycle(
				node.ID, node.StateSchema, node.StateTransfer, restoredState[node.ID],
			)
		}
		var stateBoundary element.StateLifecycle
		if state != nil {
			stateBoundary = state
		}
		runnable, err := mountElementFactory(resolution.factory, scope.ctx, element.MountContext{
			InstanceID: node.ID, Identity: node.Element, Config: value,
			Ports: ports, Services: nodeServices, Lifecycle: scope, State: stateBoundary,
			Resolution: nodeResolutionReporter{mounted: mounted, node: node.ID},
		})
		if err != nil {
			primary := fmt.Errorf("mount graph node %s: %w", node.ID, err)
			return nil, errors.Join(primary, abortMount(scope))
		}
		if runnable == nil {
			primary := fmt.Errorf("mount graph node %s returned a nil runnable", node.ID)
			return nil, errors.Join(primary, abortMount(scope))
		}
		snapshot, quiesce, stateErr := state.seal()
		if stateErr != nil {
			primary := fmt.Errorf("mount graph node %s state: %w", node.ID, stateErr)
			return nil, errors.Join(primary, abortMount(scope))
		}
		mounted.nodes = append(mounted.nodes, mountedNode{
			id: node.ID, identity: node.Element, implementation: node.Implementation,
			runnable: runnable, scope: scope, stateSchema: node.StateSchema,
			stateTransfer: node.StateTransfer.Clone(),
			snapshot:      snapshot, quiesce: quiesce,
		})
	}
	boundaries, err := newBoundaryRouter(mounted.ingress, mounted.egress)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("mount graph boundaries: %w", err), abortMount(nil))
	}
	mounted.boundaries = boundaries
	mounted.recorder.start(mounted)
	return mounted, nil
}

func mountElementFactory(
	factory element.Factory, ctx context.Context, mount element.MountContext,
) (runnable element.Runnable, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("factory panicked: %v", recovered)
			runnable = nil
		}
	}()
	return factory.Mount(ctx, mount)
}

func (mounted *Mounted) Graph() ir.Graph { return mounted.graph }

func (mounted *Mounted) now() uint64 {
	if mounted == nil || mounted.clock == nil {
		return 0
	}
	return mounted.clock()
}

func (mounted *Mounted) Ingress(name string) (element.OutputPort, error) {
	port, found := mounted.boundaries.input(name)
	if !found {
		return nil, fmt.Errorf("graph %s has no input boundary %q", mounted.graph.ID, name)
	}
	return port, nil
}

func (mounted *Mounted) Egress(name string) (element.InputPort, error) {
	port, found := mounted.boundaries.output(name)
	if !found {
		return nil, fmt.Errorf("graph %s has no output boundary %q", mounted.graph.ID, name)
	}
	return port, nil
}

// Live returns one consistent best-effort inspection snapshot without
// blocking graph execution on a telemetry consumer.
func (mounted *Mounted) Live() inspect.Live {
	mounted.liveMu.Lock()
	nodes := make(map[string]inspect.NodeLive, len(mounted.nodeLive))
	for id, state := range mounted.nodeLive {
		nodes[id] = state.Clone()
	}
	var deploymentEvidence *inspect.DeploymentEvidence
	if mounted.deploymentEvidence != nil {
		copy := mounted.deploymentEvidence.Clone()
		deploymentEvidence = &copy
	}
	mounted.liveMu.Unlock()
	edges := make(map[string]inspect.EdgeLive, len(mounted.queues))
	for id, queue := range mounted.queues {
		edges[id] = queue.snapshot()
	}
	flows, dropped := mounted.flows.snapshot()
	state, failure := mounted.lifecycleSnapshot()
	var configuration *inspect.ArtifactIdentity
	if mounted.configuration != nil {
		copy := *mounted.configuration
		configuration = &copy
	}
	return inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       mounted.graph.ID, GraphRevision: mounted.graph.Revision,
		Fingerprint: mounted.graph.Fingerprint, Configuration: configuration,
		Deployment: deploymentEvidence,
		Sequence:   mounted.sequence.Add(1), ObservedAt: time.Now().UTC(),
		State: state, Error: failure,
		Nodes: nodes, Edges: edges, Flows: flows, TraceDropped: dropped,
	}
}

func (mounted *Mounted) lifecycleSnapshot() (string, string) {
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	switch {
	case mounted.closed && mounted.runErr != nil:
		return "closed", mounted.runErr.Error()
	case mounted.closed:
		return "closed", ""
	case mounted.started:
		return "running", ""
	default:
		return "mounted", ""
	}
}

func bindQueue(groups map[string]map[string]*queue, port, lane string, channel *queue) {
	lanes := groups[port]
	if lanes == nil {
		lanes = make(map[string]*queue)
		groups[port] = lanes
	}
	lanes[lane] = channel
}

func buildPortSet(
	node ir.Node, builder *bindingBuilder, changed *condition, telemetry *nodeTelemetry,
) (*portSet, error) {
	result := &portSet{inputs: make(map[string]*inputPort), outputs: make(map[string]*outputPort)}
	for _, port := range node.Ports {
		groups := builder.inputs
		if port.Direction == element.Output {
			groups = builder.outputs
		}
		bound := groups[port.Name]
		var queues []*queue
		if port.Cardinality == element.One {
			if queue := bound[""]; queue != nil {
				queues = append(queues, queue)
			}
		} else {
			for _, lane := range port.Lanes {
				queue := bound[lane]
				if queue == nil {
					return nil, fmt.Errorf("variadic port %s.%s lane %q has no runtime queue", node.ID, port.Name, lane)
				}
				queues = append(queues, queue)
			}
		}
		if port.Direction == element.Input {
			result.inputs[port.Name] = &inputPort{
				name: port.Name, typ: port.Type, queues: queues, changed: changed,
				observe: telemetry.inputObserver(port.Name),
			}
		} else {
			result.outputs[port.Name] = &outputPort{
				name: port.Name, typ: port.Type, queues: queues, changed: changed,
				observe: telemetry.outputObserver(port.Name),
			}
		}
	}
	return result, nil
}

func boundaryDepth(port ir.Port, valueType element.Type) int {
	if port.DefaultDepth > 0 {
		return port.DefaultDepth
	}
	switch valueType.Name {
	case "State":
		return 1
	case "Stream", "Segmented", "Revisions":
		return 32
	default:
		return 16
	}
}

func validateValues(graph ir.Graph, values map[string]json.RawMessage) error {
	known := make(map[string]ir.Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		known[node.ID] = node
	}
	keys := make([]string, 0, len(values))
	for node := range values {
		keys = append(keys, node)
	}
	sort.Strings(keys)
	for _, node := range keys {
		graphNode, found := known[node]
		if !found {
			return fmt.Errorf("mount graph values refer to unknown node %q", node)
		}
		value := values[node]
		_, digest, err := graphvalues.Digest(value)
		if err != nil {
			return fmt.Errorf("mount graph node %s config must be one strict JSON object: %w", node, err)
		}
		if graphNode.ConfigDigest != "" {
			if digest != graphNode.ConfigDigest {
				return fmt.Errorf("mount graph node %s config digest is %s, Graph IR requires %s",
					node, digest, graphNode.ConfigDigest)
			}
		}
	}
	for _, node := range graph.Nodes {
		if node.ConfigDigest == "" {
			continue
		}
		if _, found := values[node.ID]; !found {
			return fmt.Errorf("mount graph node %s requires config %s with digest %s",
				node.ID, node.ConfigReference, node.ConfigDigest)
		}
	}
	return nil
}

func validateRestoredState(
	graph ir.Graph, source map[string]json.RawMessage,
) (map[string]json.RawMessage, error) {
	if len(source) == 0 {
		return nil, nil
	}
	nodes := make(map[string]ir.Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	keys := make([]string, 0, len(source))
	for node := range source {
		keys = append(keys, node)
	}
	sort.Strings(keys)
	result := make(map[string]json.RawMessage, len(source))
	for _, nodeID := range keys {
		node, found := nodes[nodeID]
		if !found {
			return nil, fmt.Errorf("mount graph restored state refers to unknown node %q", nodeID)
		}
		if node.StateSchema == "" {
			return nil, fmt.Errorf("mount graph restored state refers to stateless node %q", nodeID)
		}
		if node.StateTransfer == nil || !node.StateTransfer.Restore {
			return nil, fmt.Errorf(
				"mount graph restored state for node %q requires declared restore capability",
				nodeID,
			)
		}
		canonical, _, err := canonicalStateSnapshot(source[nodeID])
		if err != nil {
			return nil, fmt.Errorf(
				"mount graph node %s restored state for schema %s: %w",
				nodeID, node.StateSchema, err,
			)
		}
		result[nodeID] = canonical
	}
	return result, nil
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func cleanupMountedScopes(nodes []mountedNode, timeout time.Duration) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for index := len(nodes) - 1; index >= 0; index-- {
		if closeErr := nodes[index].scope.close(ctx); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}
