// Package binding adapts one existing binding.Binding into a coarse graph
// element. It lets the current gateway and benchmark oracle run through the
// graph kernel while finer elements replace the compatibility node.
package binding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	compat "github.com/bojieli/OpenRealtime/elements/compatbinding"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const nodeID = "runtime"

// Binding preserves the existing external session interface while mounting
// every session through immutable Graph IR.
type Binding struct {
	underlying     legacy.Binding
	graph          ir.Graph
	implementation string
	descriptor     element.Descriptor
	portTypes      map[string]element.Type
}

func New(underlying legacy.Binding) (*Binding, error) {
	if underlying == nil {
		return nil, errors.New("graph binding requires an underlying binding")
	}
	descriptor := compat.Descriptor()
	catalog := resolve.NewCatalog()
	if err := catalog.Register(descriptor); err != nil {
		return nil, err
	}
	topology := compatibilityTopology(graphID(underlying.Name()))
	compiled, err := graphcompiler.Compile(topology, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		return nil, err
	}
	implementation := "compat.binding/" + underlying.Name()
	identityPayload, err := compatibilityIdentity(underlying)
	if err != nil {
		return nil, err
	}
	compiled.Graph.Nodes[0].Implementation = implementation
	frozen, err := ir.Freeze(compiled.Graph)
	if err != nil {
		return nil, err
	}
	bound, err := graphvalues.Bind(frozen, graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: frozen.ID,
		Nodes: map[string]json.RawMessage{nodeID: identityPayload},
	})
	if err != nil {
		return nil, err
	}
	portTypes := make(map[string]element.Type, len(descriptor.Ports))
	for _, port := range descriptor.Ports {
		portTypes[port.Name] = port.Type.Clone()
	}
	return &Binding{
		underlying: underlying, graph: bound.Graph, implementation: implementation,
		descriptor: descriptor, portTypes: portTypes,
	}, nil
}

func (binding *Binding) Name() string                      { return binding.underlying.Name() }
func (binding *Binding) Ownership() legacy.Ownership       { return binding.underlying.Ownership() }
func (binding *Binding) Capabilities() legacy.Capabilities { return binding.underlying.Capabilities() }
func (binding *Binding) Graph() ir.Graph                   { return binding.graph }

func (binding *Binding) Start(ctx context.Context, options legacy.Options) (legacy.Runtime, error) {
	if ctx == nil {
		return nil, errors.New("start graph binding: nil context")
	}
	if options.Sink == nil {
		return nil, errors.New("start graph binding: sink is required")
	}
	// Mount and Run are deliberately separated. Own mutable session values
	// before the compatibility element starts asynchronously in its supervised
	// Run method.
	options.Settings = legacy.CloneSettings(options.Settings)
	if options.Policies != nil {
		policies := *options.Policies
		options.Policies = &policies
	}
	holder := compat.NewHolder()
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(compat.SessionServiceName, &compat.SessionService{
		Binding: binding.underlying, Options: options, Holder: holder,
	}); err != nil {
		return nil, err
	}
	registry := graphruntime.NewRegistry()
	if err := registry.Register(binding.implementation, compat.Factory{}); err != nil {
		return nil, err
	}
	configValue, err := compatibilityIdentity(binding.underlying)
	if err != nil {
		return nil, err
	}
	mounted, err := graphruntime.Mount(ctx, graphruntime.Config{
		Graph: binding.graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{nodeID: configValue},
	})
	if err != nil {
		return nil, err
	}
	sessionCtx, cancel := context.WithCancelCause(ctx)
	runtime := &Runtime{
		binding: binding, mounted: mounted, holder: holder, sink: options.Sink,
		ctx: sessionCtx, cancel: cancel, done: make(chan struct{}),
		portTypes: binding.portTypes, sessionID: options.SessionID,
	}
	for _, name := range inputBoundaryNames() {
		port, portErr := mounted.Ingress(name)
		if portErr != nil {
			_ = mounted.Close(context.Background())
			cancel(portErr)
			return nil, portErr
		}
		runtime.inputs.Store(name, port)
	}
	events, err := mounted.Egress("events")
	if err != nil {
		_ = mounted.Close(context.Background())
		cancel(err)
		return nil, err
	}
	runtime.events = events
	runtime.wait.Add(2)
	// Drain output before the graph starts. A legacy Binding.Start is allowed
	// to synchronously call its sink; the compatibility element acknowledges
	// that callback only after this goroutine has delivered it downstream.
	go runtime.runSink()
	go runtime.runGraph()
	if err := holder.Wait(ctx); err != nil {
		cancel(err)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		mountErr := mounted.Close(cleanupCtx)
		select {
		case <-runtime.done:
		case <-cleanupCtx.Done():
			mountErr = errors.Join(mountErr, context.Cause(cleanupCtx))
		}
		return nil, errors.Join(fmt.Errorf("start graph-backed binding: %w", err), mountErr)
	}
	return runtime, nil
}

func compatibilityIdentity(binding legacy.Binding) (json.RawMessage, error) {
	payload, err := json.Marshal(struct {
		Name         string              `json:"name"`
		Ownership    legacy.Ownership    `json:"ownership"`
		Capabilities legacy.Capabilities `json:"capabilities"`
	}{
		Name: binding.Name(), Ownership: binding.Ownership(),
		Capabilities: binding.Capabilities(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode compatibility binding identity: %w", err)
	}
	return payload, nil
}

func compatibilityTopology(name string) syntax.File {
	descriptor := compat.Descriptor()
	statements := []syntax.Statement{{Node: &syntax.Node{Element: descriptor.Name, Name: nodeID}}}
	for _, port := range descriptor.Ports {
		direction := syntax.BoundaryInput
		if port.Direction == element.Output {
			direction = syntax.BoundaryOutput
		}
		statements = append(statements, syntax.Statement{Boundary: &syntax.Boundary{
			Direction: direction, Name: port.Name,
			Endpoint: syntax.Endpoint{Node: nodeID, Port: port.Name},
		}})
	}
	return syntax.File{Graph: syntax.Graph{Name: name, Statements: statements}}
}

func graphID(bindingName string) string {
	var output strings.Builder
	output.WriteString("compat_")
	for _, character := range bindingName {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			output.WriteRune(character)
		} else {
			output.WriteByte('_')
		}
	}
	return output.String()
}

func inputBoundaryNames() []string {
	return []string{
		"update", "audio", "video", "text", "tool_result",
		"commit_audio", "create_response", "cancel", "truncate",
	}
}

var _ legacy.Binding = (*Binding)(nil)

// Runtime is the current management-plane session interface backed by graph
// observation/action channels.
type Runtime struct {
	binding *Binding
	mounted *graphruntime.Mounted
	holder  *compat.Holder
	sink    legacy.Sink
	events  element.InputPort
	inputs  sync.Map // map[string]element.OutputPort; immutable after Start

	ctx    context.Context
	cancel context.CancelCauseFunc
	next   atomic.Uint64
	wait   sync.WaitGroup
	done   chan struct{}

	portTypes map[string]element.Type
	sessionID string
	closeOnce sync.Once
	closeErr  error
	runMu     sync.Mutex
	runErr    error
}

func (runtime *Runtime) Update(ctx context.Context, settings legacy.Settings) error {
	call := &compat.Call{Kind: compat.CallUpdate, Context: ctx, Settings: legacy.CloneSettings(settings)}
	return runtime.invoke(ctx, "update", call)
}

func (runtime *Runtime) Audio(ctx context.Context, frame perception.Frame) error {
	call := &compat.Call{Kind: compat.CallAudio, Context: ctx, Audio: cloneFrame(frame)}
	return runtime.invoke(ctx, "audio", call)
}

func (runtime *Runtime) Video(ctx context.Context, frame perception.Frame) error {
	call := &compat.Call{Kind: compat.CallVideo, Context: ctx, Video: cloneFrame(frame)}
	return runtime.invoke(ctx, "video", call)
}

func (runtime *Runtime) Text(ctx context.Context, input legacy.TextInput) error {
	input.Images = slices.Clone(input.Images)
	for index := range input.Images {
		input.Images[index].Bytes = slices.Clone(input.Images[index].Bytes)
	}
	call := &compat.Call{Kind: compat.CallText, Context: ctx, Text: input}
	return runtime.invoke(ctx, "text", call)
}

func (runtime *Runtime) ToolResult(ctx context.Context, result trajectory.ToolResult) error {
	result.Output = slices.Clone(result.Output)
	call := &compat.Call{Kind: compat.CallToolResult, Context: ctx, ToolResult: result}
	return runtime.invoke(ctx, "tool_result", call)
}

func (runtime *Runtime) CommitAudio(ctx context.Context) error {
	return runtime.invoke(ctx, "commit_audio", &compat.Call{Kind: compat.CallCommitAudio, Context: ctx})
}

func (runtime *Runtime) CreateResponse(ctx context.Context) error {
	return runtime.invoke(ctx, "create_response", &compat.Call{Kind: compat.CallCreateResponse, Context: ctx})
}

func (runtime *Runtime) Cancel(ctx context.Context, reason string) error {
	return runtime.invoke(ctx, "cancel", &compat.Call{Kind: compat.CallCancel, Context: ctx, Reason: reason})
}

func (runtime *Runtime) Truncate(ctx context.Context, truncation legacy.Truncation) error {
	return runtime.invoke(ctx, "truncate", &compat.Call{Kind: compat.CallTruncate, Context: ctx, Truncate: truncation})
}

func (runtime *Runtime) Trajectory() trajectory.Snapshot {
	snapshot, err := runtime.holder.Trajectory()
	if err != nil {
		return trajectory.Snapshot{}
	}
	return snapshot
}

func (runtime *Runtime) Status() legacy.Status {
	status, err := runtime.holder.Status()
	if err != nil {
		return legacy.Status{Binding: runtime.binding.Name()}
	}
	status.Graph = legacy.ArchitectureIdentity{
		ID: runtime.binding.graph.ID, Revision: int(runtime.binding.graph.Revision),
		Fingerprint: runtime.binding.graph.Fingerprint,
	}
	return status
}

func (runtime *Runtime) Graph() ir.Graph    { return runtime.binding.graph }
func (runtime *Runtime) Live() inspect.Live { return runtime.mounted.Live() }

func (runtime *Runtime) Close(ctx context.Context, cause error) error {
	if ctx == nil {
		return errors.New("close graph-backed binding: nil context")
	}
	runtime.closeOnce.Do(func() {
		runtime.holder.RequestClose(cause)
		graphCause := error(graphruntime.ErrGraphClosed)
		if cause != nil {
			graphCause = fmt.Errorf("%w: %v", graphruntime.ErrGraphClosed, cause)
		}
		runtime.cancel(graphCause)
		mountErr := runtime.mounted.Close(ctx)
		select {
		case <-runtime.done:
		case <-ctx.Done():
			mountErr = errors.Join(mountErr, context.Cause(ctx))
		}
		runtime.closeErr = errors.Join(mountErr, runtime.graphError(false))
	})
	return runtime.closeErr
}

func (runtime *Runtime) invoke(ctx context.Context, portName string, call *compat.Call) error {
	if ctx == nil {
		return errors.New("graph-backed binding call has nil context")
	}
	if cause := context.Cause(runtime.ctx); cause != nil {
		return cause
	}
	portValue, found := runtime.inputs.Load(portName)
	if !found {
		return fmt.Errorf("graph-backed binding has no input boundary %q", portName)
	}
	port := portValue.(element.OutputPort)
	call.Done = make(chan error, 1)
	identityPrefix := runtime.binding.graph.ID
	if runtime.sessionID != "" {
		identityPrefix = runtime.sessionID
	}
	identity := fmt.Sprintf("%s-call-%d", identityPrefix, runtime.next.Add(1))
	result, err := port.Broadcast(ctx, element.Envelope{
		Type: runtime.portTypes[portName], ItemID: identity,
		SessionID: runtime.sessionID, TraceID: identity, CancellationScope: identity,
		Payload: call,
	})
	if err != nil {
		return err
	}
	if result.Delivered != 1 {
		return fmt.Errorf("graph-backed binding call %s delivered to %d lanes", identity, result.Delivered)
	}
	select {
	case err := <-call.Done:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-runtime.ctx.Done():
		return context.Cause(runtime.ctx)
	}
}

func (runtime *Runtime) runGraph() {
	defer runtime.wait.Done()
	err := runtime.mounted.Run(runtime.ctx)
	runtime.runMu.Lock()
	runtime.runErr = err
	runtime.runMu.Unlock()
	runtime.cancel(err)
}

func (runtime *Runtime) runSink() {
	defer runtime.wait.Done()
	var firstError error
	for {
		envelope, err := runtime.events.Receive(context.Background())
		if err != nil {
			if !errors.Is(err, graphruntime.ErrChannelClosed) && firstError == nil {
				firstError = err
			}
			break
		}
		event, ok := envelope.Payload.(compat.SinkEvent)
		if !ok {
			if firstError == nil {
				firstError = fmt.Errorf("graph sink boundary received payload %T", envelope.Payload)
				runtime.cancel(firstError)
			}
			continue
		}
		if event.Done == nil {
			if firstError == nil {
				firstError = fmt.Errorf("graph sink event %q has no acknowledgement channel", event.Kind)
				runtime.cancel(firstError)
			}
			continue
		}
		if firstError != nil {
			acknowledge(event.Done, firstError)
			continue // drain so lifecycle producers cannot deadlock on shutdown
		}
		dispatchErr := runtime.dispatchSink(event)
		acknowledge(event.Done, dispatchErr)
		if dispatchErr != nil {
			firstError = dispatchErr
			runtime.cancel(dispatchErr)
		}
	}
	if firstError != nil {
		runtime.runMu.Lock()
		runtime.runErr = errors.Join(runtime.runErr, firstError)
		runtime.runMu.Unlock()
	}
	close(runtime.done)
}

func (runtime *Runtime) dispatchSink(event compat.SinkEvent) error {
	ctx := event.Context
	if ctx == nil {
		ctx = runtime.ctx
	}
	switch event.Kind {
	case compat.EventTurnBegin:
		return runtime.sink.TurnBegin(ctx)
	case compat.EventTurnEnd:
		return runtime.sink.TurnEnd(ctx, event.TurnOutcome)
	case compat.EventActivity:
		return runtime.sink.Activity(ctx, event.Activity)
	case compat.EventTranscript:
		return runtime.sink.Transcript(ctx, event.Transcript)
	case compat.EventObservation:
		return runtime.sink.Observation(ctx, event.Observation)
	case compat.EventSpeechReserved:
		if sink, ok := runtime.sink.(legacy.SpeechReservationSink); ok {
			return sink.SpeechReserved(ctx, event.Utterance)
		}
		return nil
	case compat.EventSpeechReserveCanceled:
		if sink, ok := runtime.sink.(legacy.SpeechReservationSink); ok {
			sink.SpeechReservationCancelled(ctx, event.Utterance)
		}
		return nil
	case compat.EventSpeechBegin:
		return runtime.sink.SpeechBegin(ctx, event.Utterance)
	case compat.EventSpeechText:
		return runtime.sink.SpeechText(ctx, event.Utterance, event.Text)
	case compat.EventSpeechAudio:
		return runtime.sink.SpeechAudio(ctx, event.Utterance, event.Frame)
	case compat.EventSpeechEnd:
		return runtime.sink.SpeechEnd(ctx, event.Utterance, event.SpeechEnd)
	case compat.EventToolCalls:
		return runtime.sink.ToolCalls(ctx, event.ToolCalls)
	case compat.EventFailed:
		runtime.sink.Failed(ctx, event.Failure)
		return nil
	case compat.EventDebug:
		if sink, ok := runtime.sink.(legacy.DebugSink); ok {
			return sink.Debug(ctx, event.Debug)
		}
		return nil
	default:
		return fmt.Errorf("unknown graph sink event kind %q", event.Kind)
	}
}

func acknowledge(done chan error, err error) {
	select {
	case done <- err:
	default:
	}
}

func (runtime *Runtime) graphError(includeCancellation bool) error {
	runtime.runMu.Lock()
	defer runtime.runMu.Unlock()
	if !includeCancellation && (errors.Is(runtime.runErr, context.Canceled) || errors.Is(runtime.runErr, graphruntime.ErrGraphClosed)) {
		return nil
	}
	return runtime.runErr
}

func cloneFrame(frame perception.Frame) perception.Frame {
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	frame.Image = slices.Clone(frame.Image)
	return frame
}

var (
	_ legacy.Runtime = (*Runtime)(nil)
)
