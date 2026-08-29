package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphvalidate "github.com/bojieli/OpenRealtime/graph/validate"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

type Config struct {
	Graph           ir.Graph
	Registry        *Registry
	Values          map[string]json.RawMessage
	Services        *ServiceSet
	Tracer          Tracer
	Now             func() uint64
	ShutdownTimeout time.Duration
}

type mountedNode struct {
	id       string
	runnable element.Runnable
	scope    *lifecycleScope
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
	changed *condition
	timeout time.Duration

	mu          sync.Mutex
	started     bool
	closed      bool
	cancel      context.CancelCauseFunc
	done        chan struct{}
	runErr      error
	shutdownErr error
	shutdown    sync.Once

	liveMu   sync.Mutex
	nodeLive map[string]inspect.NodeLive
	sequence atomic.Uint64
}

// Mount validates the exact factory contracts, constructs every bounded
// channel, resolves required services, and mounts elements without starting
// their long-lived run loops.
func Mount(ctx context.Context, config Config) (*Mounted, error) {
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
	if config.Now == nil {
		origin := time.Now()
		config.Now = func() uint64 { return uint64(time.Since(origin)) }
	}
	services := newMountServices(config.Services, config.Now)
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 5 * time.Second
	}
	if err := validateValues(config.Graph, config.Values); err != nil {
		return nil, err
	}

	type factoryResolution struct {
		factory    element.Factory
		descriptor element.Descriptor
	}
	factories := make(map[string]factoryResolution, len(config.Graph.Nodes))
	for _, node := range config.Graph.Nodes {
		factory, descriptor, err := config.Registry.resolve(node.Implementation, node.Element)
		if err != nil {
			return nil, fmt.Errorf("mount graph node %s: %w", node.ID, err)
		}
		if err := verifyNodeContract(node, descriptor); err != nil {
			return nil, fmt.Errorf("mount graph node %s contract verification: %w", node.ID, err)
		}
		for _, dependency := range descriptor.Dependencies {
			if _, _, found := services.Lookup(dependency.Name); !found && !dependency.Optional {
				return nil, fmt.Errorf("mount graph node %s requires unavailable service %q", node.ID, dependency.Name)
			}
		}
		factories[node.ID] = factoryResolution{factory: factory, descriptor: descriptor}
	}
	// Validate every node's values before mounting the first element. Mount may
	// acquire resources and register effects; a later config typo must never
	// require rolling those back just to report an authoring error.
	for _, node := range config.Graph.Nodes {
		value := cloneRaw(config.Values[node.ID])
		if len(value) == 0 {
			value = json.RawMessage("{}")
		}
		canonical, _, err := graphvalues.Digest(value)
		if err != nil {
			return nil, fmt.Errorf("mount graph node %s config: %w", node.ID, err)
		}
		factory := factories[node.ID].factory
		validator, validates := factory.(element.ConfigValidator)
		switch {
		case node.ConfigSchema == "" && !bytes.Equal(canonical, []byte("{}")):
			return nil, fmt.Errorf("mount graph node %s has values but element %s declares no config schema",
				node.ID, node.Element.Name)
		case node.ConfigSchema != "" && !validates:
			return nil, fmt.Errorf("mount graph node %s declares config schema %q but implementation %q has no config validator",
				node.ID, node.ConfigSchema, node.Implementation)
		case validates:
			if err := validator.ValidateConfig(value); err != nil {
				return nil, fmt.Errorf("mount graph node %s config: %w", node.ID, err)
			}
		}
	}

	mounted := &Mounted{
		graph: config.Graph, queues: make(map[string]*queue),
		ingress: make(map[string]*outputPort), egress: make(map[string]*inputPort),
		changed: newCondition(), timeout: config.ShutdownTimeout,
		done: make(chan struct{}), nodeLive: make(map[string]inspect.NodeLive),
	}
	trace := func(queue *queue, kind TraceKind, envelope element.Envelope, occupancy int) {
		if config.Tracer == nil {
			return
		}
		config.Tracer.Record(TraceEvent{
			Kind: kind, AtNS: config.Now(), Graph: config.Graph.ID,
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
	for _, edge := range config.Graph.Edges {
		channel, err := newQueue(edge.ID, edge.Type, edge.Delivery, edge.Depth, mounted.changed, config.Now, trace)
		if err != nil {
			cleanupChannels()
			return nil, err
		}
		mounted.queues[edge.ID] = channel
		bindQueue(builders[edge.From.Node].outputs, edge.From.Port, edge.From.Lane, channel)
		bindQueue(builders[edge.To.Node].inputs, edge.To.Port, edge.To.Lane, channel)
	}
	for _, boundary := range config.Graph.Boundaries {
		port := portIndex[boundary.Endpoint.Node][boundary.Endpoint.Port]
		depth := boundaryDepth(port, boundary.Type)
		identity := "boundary:" + boundary.Name
		channel, err := newQueue(identity, boundary.Type, ir.Lossless, depth, mounted.changed, config.Now, trace)
		if err != nil {
			cleanupChannels()
			return nil, err
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
		ports, err := buildPortSet(node, builders[node.ID], mounted.changed)
		if err != nil {
			cleanupChannels()
			return nil, fmt.Errorf("mount graph node %s ports: %w", node.ID, err)
		}
		scope := &lifecycleScope{instance: node.ID}
		value := cloneRaw(config.Values[node.ID])
		if len(value) == 0 {
			value = json.RawMessage("{}")
		}
		resolution := factories[node.ID]
		runnable, err := resolution.factory.Mount(ctx, element.MountContext{
			InstanceID: node.ID, Identity: node.Element, Config: value,
			Ports: ports, Services: services, Lifecycle: scope,
		})
		if err != nil {
			_ = scope.close(context.Background())
			cleanupMountedScopes(mounted.nodes, config.ShutdownTimeout)
			cleanupChannels()
			return nil, fmt.Errorf("mount graph node %s: %w", node.ID, err)
		}
		if runnable == nil {
			_ = scope.close(context.Background())
			cleanupMountedScopes(mounted.nodes, config.ShutdownTimeout)
			cleanupChannels()
			return nil, fmt.Errorf("mount graph node %s returned a nil runnable", node.ID)
		}
		mounted.nodes = append(mounted.nodes, mountedNode{id: node.ID, runnable: runnable, scope: scope})
		mounted.nodeLive[node.ID] = inspect.NodeLive{State: "mounted"}
	}
	return mounted, nil
}

func (mounted *Mounted) Graph() ir.Graph { return mounted.graph }

func (mounted *Mounted) Ingress(name string) (element.OutputPort, error) {
	port, found := mounted.ingress[name]
	if !found {
		return nil, fmt.Errorf("graph %s has no input boundary %q", mounted.graph.ID, name)
	}
	return port, nil
}

func (mounted *Mounted) Egress(name string) (element.InputPort, error) {
	port, found := mounted.egress[name]
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
		nodes[id] = state
	}
	mounted.liveMu.Unlock()
	edges := make(map[string]inspect.EdgeLive, len(mounted.queues))
	for id, queue := range mounted.queues {
		edges[id] = queue.snapshot()
	}
	return inspect.Live{
		Fingerprint: mounted.graph.Fingerprint,
		Sequence:    mounted.sequence.Add(1), ObservedAt: time.Now().UTC(),
		Nodes: nodes, Edges: edges,
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

func buildPortSet(node ir.Node, builder *bindingBuilder, changed *condition) (*portSet, error) {
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
			result.inputs[port.Name] = &inputPort{name: port.Name, typ: port.Type, queues: queues, changed: changed}
		} else {
			result.outputs[port.Name] = &outputPort{name: port.Name, typ: port.Type, queues: queues, changed: changed}
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

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func cleanupMountedScopes(nodes []mountedNode, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for index := len(nodes) - 1; index >= 0; index-- {
		_ = nodes[index].scope.close(ctx)
	}
}
