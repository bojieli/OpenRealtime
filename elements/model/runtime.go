package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// Factory may wrap any descriptor that declares the external-model config
// schema and services. The zero value supplies StandardDescriptor.
type Factory struct{ descriptor *element.Descriptor }

func NewFactory(descriptor element.Descriptor) (Factory, error) {
	canonical, err := descriptor.Canonical()
	if err != nil {
		return Factory{}, err
	}
	if canonical.ConfigSchema != ConfigSchema {
		return Factory{}, fmt.Errorf("external model descriptor %s config schema is %q, want %q",
			canonical.Name, canonical.ConfigSchema, ConfigSchema)
	}
	if len(canonical.Generics) != 0 {
		return Factory{}, fmt.Errorf("external model descriptor %s must package concrete port types",
			canonical.Name)
	}
	dependencies := make(map[string]bool, len(canonical.Dependencies))
	for _, dependency := range canonical.Dependencies {
		if !dependency.Optional {
			dependencies[dependency.Name] = true
		}
	}
	for _, required := range []string{DeploymentRegistryService, PayloadCodecService} {
		if !dependencies[required] {
			return Factory{}, fmt.Errorf("external model descriptor %s requires dependency %q",
				canonical.Name, required)
		}
	}
	return Factory{descriptor: &canonical}, nil
}

func (factory Factory) Descriptor() element.Descriptor {
	if factory.descriptor == nil {
		return StandardDescriptor()
	}
	return factory.descriptor.Clone()
}

func (Factory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeConfig(source)
	return err
}

func (factory Factory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("external model %s config: %w", mount.InstanceID, err)
	}
	registryService, _, found := mount.Services.Lookup(DeploymentRegistryService)
	if !found {
		return nil, fmt.Errorf("external model %s has no deployment registry service", mount.InstanceID)
	}
	registry, ok := registryService.(*DeploymentRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("external model deployment registry service has type %T", registryService)
	}
	entry, err := registry.resolve(config.Deployment)
	if err != nil {
		return nil, err
	}
	codecService, _, found := mount.Services.Lookup(PayloadCodecService)
	if !found {
		return nil, fmt.Errorf("external model %s has no payload codec service", mount.InstanceID)
	}
	codec, ok := codecService.(PayloadCodec)
	if !ok || reflectedNil(codec) {
		return nil, fmt.Errorf("external model payload codec service has type %T", codecService)
	}

	descriptor := factory.Descriptor()
	ports, selections, err := resolvePorts(mount.Ports, descriptor)
	if err != nil {
		return nil, err
	}
	if err := validateSelectionCompleteness(descriptor, selections); err != nil {
		return nil, fmt.Errorf("external model %s selected ports: %w", mount.InstanceID, err)
	}
	holder := &sessionHolder{}
	if err := mount.Lifecycle.Defer("close-external-model-session", func(context.Context) error {
		return holder.close()
	}); err != nil {
		return nil, err
	}
	return &runner{
		instance: mount.InstanceID, descriptor: descriptor, config: config,
		dial: entry.dial, codec: codec, ports: ports, selections: selections,
		resolution: mount.Resolution, holder: holder,
	}, nil
}

type boundPorts struct {
	inputs  map[string]element.InputPort
	outputs map[string]element.OutputPort
}

func resolvePorts(
	ports element.Ports, descriptor element.Descriptor,
) (boundPorts, []sidecar.PortSelection, error) {
	result := boundPorts{
		inputs: make(map[string]element.InputPort), outputs: make(map[string]element.OutputPort),
	}
	selections := make([]sidecar.PortSelection, 0, len(descriptor.Ports))
	for _, port := range descriptor.Ports {
		selection := sidecar.PortSelection{Name: port.Name, Direction: port.Direction, Type: port.Type.Clone()}
		if port.Direction == element.Input {
			input, err := ports.Input(port.Name)
			if err != nil {
				return boundPorts{}, nil, err
			}
			if len(input.Lanes()) != 0 {
				result.inputs[port.Name] = input
				selections = append(selections, selection)
			}
			continue
		}
		output, err := ports.Output(port.Name)
		if err != nil {
			return boundPorts{}, nil, err
		}
		if len(output.Lanes()) != 0 {
			result.outputs[port.Name] = output
			selections = append(selections, selection)
		}
	}
	return result, selections, nil
}

func validateSelectionCompleteness(
	descriptor element.Descriptor, selections []sidecar.PortSelection,
) error {
	if len(selections) == 0 {
		return errors.New("at least one graph connection is required")
	}
	selected := make(map[string]element.Direction, len(selections))
	for _, selection := range selections {
		selected[selection.Name] = selection.Direction
	}
	reactive := false
	for _, name := range append(slices.Clone(descriptor.Reaction.Triggers), descriptor.Reaction.Interrupts...) {
		if selected[name] == element.Input {
			reactive = true
			break
		}
	}
	if !reactive {
		return nil
	}
	for _, name := range descriptor.Reaction.Outcomes {
		if selected[name] == element.Output {
			return nil
		}
	}
	return errors.New("a selected trigger or interrupt requires an explicit selected outcome port")
}

type runner struct {
	instance   string
	descriptor element.Descriptor
	config     Config
	dial       Dialer
	codec      PayloadCodec
	ports      boundPorts
	selections []sidecar.PortSelection
	resolution element.ResolutionReporter
	holder     *sessionHolder
}

func (runner *runner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	descriptor := runner.descriptor.Clone()
	hello := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &descriptor, ElementConfig: slices.Clone(runner.config.Settings),
		SelectedPorts:        slices.Clone(runner.selections),
		RequiredCapabilities: slices.Clone(runner.config.RequiredCapabilities),
	}
	session, err := runner.dial(ctx, hello)
	if err != nil {
		return fmt.Errorf("dial external model deployment %q: %w", runner.config.Deployment, err)
	}
	if session == nil || reflectedNil(session) {
		return fmt.Errorf("external model deployment %q returned a nil session", runner.config.Deployment)
	}
	if err := runner.holder.set(session); err != nil {
		return errors.Join(err, session.Close())
	}
	ready := session.Ready()
	if err := sidecar.ValidateElementReady(hello, ready); err != nil {
		return fmt.Errorf("external model deployment %q readiness: %w", runner.config.Deployment, err)
	}
	baselineEvidence, err := readyEvidenceOf(ready)
	if err != nil {
		return err
	}
	ready.ResolvedCapabilities = slices.Clone(baselineEvidence.Capabilities)
	if err := runner.reportResolution(ready); err != nil {
		return err
	}

	failures := make(chan error, len(runner.ports.inputs))
	var receivers sync.WaitGroup
	sendMu := &sync.Mutex{}
	for name, input := range runner.ports.inputs {
		receivers.Add(1)
		go runner.forwardInput(ctx, name, input, session, sendMu, failures, &receivers)
	}
	defer func() {
		stop(nil)
		receivers.Wait()
	}()

	frames := session.Frames()
	if frames == nil {
		return errors.New("external model session returned a nil frame channel")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case frame, open := <-frames:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				if err := session.Err(); err != nil {
					return fmt.Errorf("external model session stopped: %w", err)
				}
				return errors.New("external model session closed before graph shutdown")
			}
			if err := frame.Validate(); err != nil {
				return fmt.Errorf("external model sent an invalid frame: %w", err)
			}
			switch frame.Type {
			case sidecar.TypeElementFrame:
				if err := runner.forwardOutput(ctx, frame); err != nil {
					return err
				}
			case sidecar.TypeReady:
				if err := sidecar.ValidateElementReady(hello, frame); err != nil {
					return fmt.Errorf("external model readiness changed after startup: %w", err)
				}
				next, err := readyEvidenceOf(frame)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(next, baselineEvidence) {
					return errors.New("external model runtime or capability identity changed after readiness")
				}
				frame.ResolvedCapabilities = slices.Clone(next.Capabilities)
				if err := runner.reportResolution(frame); err != nil {
					return err
				}
			case sidecar.TypeError:
				return fmt.Errorf("external model transport error: %s", frame.Message())
			default:
				return fmt.Errorf("external model sent legacy frame %q on a graph-native session", frame.Type)
			}
		}
	}
}

func (runner *runner) forwardInput(
	ctx context.Context, name string, input element.InputPort, session Session,
	sendMu *sync.Mutex, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				sendFailure(ctx, failures, fmt.Errorf("external model input %s: %w", name, err))
			}
			return
		}
		data, binary, err := runner.codec.Encode(input.Type(), envelope.Payload)
		if err != nil {
			sendFailure(ctx, failures, fmt.Errorf("external model input %s encode: %w", name, err))
			return
		}
		wire := sidecar.FromEnvelope(envelope, data)
		sendMu.Lock()
		err = session.Send(sidecar.Message{
			Type: sidecar.TypeElementFrame, Port: name, Envelope: &wire, Payload: binary,
		})
		sendMu.Unlock()
		if err != nil {
			sendFailure(ctx, failures, fmt.Errorf("external model input %s send: %w", name, err))
			return
		}
	}
}

func (runner *runner) forwardOutput(ctx context.Context, frame sidecar.Message) error {
	output, selected := runner.ports.outputs[frame.Port]
	if !selected {
		return fmt.Errorf("external model emitted unselected output port %q", frame.Port)
	}
	if frame.Envelope == nil {
		return fmt.Errorf("external model output %s has no envelope", frame.Port)
	}
	if !frame.Envelope.Type.Equal(output.Type()) {
		return fmt.Errorf("external model output %s has type %s, graph selected %s",
			frame.Port, frame.Envelope.Type.String(), output.Type().String())
	}
	payload, err := runner.codec.Decode(output.Type(), frame.Envelope.JSON, frame.Payload)
	if err != nil {
		return fmt.Errorf("external model output %s decode: %w", frame.Port, err)
	}
	envelope := frame.Envelope.Envelope(payload)
	if err := envelope.ValidateFor(output.Type()); err != nil {
		return fmt.Errorf("external model output %s: %w", frame.Port, err)
	}
	result, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return fmt.Errorf("external model output %s: %w", frame.Port, err)
	}
	if result.Delivered+result.Dropped == 0 && len(output.Lanes()) != 0 {
		return fmt.Errorf("external model output %s produced no delivery decision", frame.Port)
	}
	return nil
}

func (runner *runner) reportResolution(ready sidecar.Message) error {
	if runner.resolution == nil || reflectedNil(runner.resolution) {
		return errors.New("external model has no live resolution reporter")
	}
	if err := runner.resolution.Runtime(
		ready.RuntimeArtifact.ID, ready.RuntimeArtifact.Revision, ready.RuntimeArtifact.Digest,
	); err != nil {
		return err
	}
	capabilities := make([]element.CapabilityResolution, 0, len(ready.ResolvedCapabilities))
	for _, capability := range ready.ResolvedCapabilities {
		converted := element.CapabilityResolution{
			Name: capability.Name, Contract: capability.Contract,
			ProviderID: capability.Provider.ID, ProviderRevision: capability.Provider.Revision,
			ProviderDigest: capability.Provider.Digest,
		}
		if capability.Adapter != nil {
			converted.AdapterID = capability.Adapter.ID
			converted.AdapterRevision = capability.Adapter.Revision
			converted.AdapterDigest = capability.Adapter.Digest
		}
		capabilities = append(capabilities, converted)
	}
	return runner.resolution.Capabilities(capabilities)
}

type readyEvidence struct {
	Element      element.Identity
	Runtime      sidecar.ArtifactIdentity
	Capabilities []sidecar.CapabilityIdentity
}

func readyEvidenceOf(ready sidecar.Message) (readyEvidence, error) {
	if ready.ElementDescriptor == nil {
		return readyEvidence{}, errors.New("external model ready frame has no descriptor")
	}
	identity, err := ready.ElementDescriptor.Identity()
	if err != nil {
		return readyEvidence{}, err
	}
	capabilities, err := sidecar.CanonicalCapabilities(ready.ResolvedCapabilities)
	if err != nil {
		return readyEvidence{}, err
	}
	return readyEvidence{
		Element: identity, Runtime: ready.RuntimeArtifact, Capabilities: capabilities,
	}, nil
}

func sendFailure(ctx context.Context, failures chan<- error, err error) {
	select {
	case failures <- err:
	case <-ctx.Done():
	}
}

type sessionHolder struct {
	mu      sync.Mutex
	session Session
	closed  bool
}

func (holder *sessionHolder) set(session Session) error {
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.closed {
		return errors.New("external model session lifecycle is already closed")
	}
	if holder.session != nil {
		return errors.New("external model session is already set")
	}
	holder.session = session
	return nil
}

func (holder *sessionHolder) close() error {
	holder.mu.Lock()
	if holder.closed {
		holder.mu.Unlock()
		return nil
	}
	holder.closed = true
	session := holder.session
	holder.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

func reflectedNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var (
	_ element.Factory         = Factory{}
	_ element.ConfigValidator = Factory{}
)
