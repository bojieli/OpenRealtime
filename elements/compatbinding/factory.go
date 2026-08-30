package compatbinding

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/perception"
)

type Factory struct{}

func (Factory) Descriptor() element.Descriptor { return Descriptor() }

func (Factory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeConfig(source)
	return err
}

func (Factory) Mount(ctx context.Context, mount element.MountContext) (element.Runnable, error) {
	serviceValue, _, found := mount.Services.Lookup(SessionServiceName)
	if !found {
		return nil, fmt.Errorf("compatibility binding session service is unavailable")
	}
	service, ok := serviceValue.(*SessionService)
	if !ok || service == nil || service.Binding == nil || service.Holder == nil {
		return nil, fmt.Errorf("compatibility binding session service has type %T or incomplete fields", serviceValue)
	}
	config, err := decodeConfig(mount.Config)
	if err != nil {
		return nil, err
	}
	want := Config{
		Name: service.Binding.Name(), Ownership: service.Binding.Ownership(),
		Capabilities: service.Binding.Capabilities(),
	}
	if !reflect.DeepEqual(config, want) {
		return nil, fmt.Errorf("compatibility binding identity config %+v does not match live binding %+v", config, want)
	}
	// The coarse compatibility element selects no descriptor-level provider
	// capabilities. Reporting the complete empty set is live evidence; leaving
	// it unknown would make payload-free trace attestation fail closed even
	// though the registered runtime artifact is exact.
	if err := mount.Resolution.Capabilities(nil); err != nil {
		return nil, fmt.Errorf("report compatibility binding capabilities: %w", err)
	}
	output, err := mount.Ports.Output("events")
	if err != nil {
		return nil, err
	}
	prefix := mount.InstanceID
	if service.Options.SessionID != "" {
		prefix = service.Options.SessionID + "-" + prefix
	}
	emitter := &sinkEmitter{
		output: output, prefix: prefix, sessionID: service.Options.SessionID,
	}
	if err := mount.Lifecycle.Defer("close-binding-runtime", func(disposeCtx context.Context) error {
		return service.Holder.dispose(disposeCtx)
	}); err != nil {
		return nil, err
	}
	inputs := make(map[CallKind]element.InputPort)
	for kind, name := range map[CallKind]string{
		CallUpdate: "update", CallAudio: "audio", CallVideo: "video", CallText: "text",
		CallToolResult: "tool_result", CallCommitAudio: "commit_audio",
		CallCreateResponse: "create_response", CallCancel: "cancel", CallTruncate: "truncate",
	} {
		input, portErr := mount.Ports.Input(name)
		if portErr != nil {
			return nil, portErr
		}
		inputs[kind] = input
	}
	return &runner{service: service, emitter: emitter, inputs: inputs}, nil
}

func decodeConfig(source json.RawMessage) (Config, error) {
	var config Config
	if err := elementconfig.Decode(source, &config); err != nil {
		return Config{}, err
	}
	if config.Name == "" {
		return Config{}, fmt.Errorf("compatibility binding identity requires a name")
	}
	if err := config.Ownership.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

type runner struct {
	service *SessionService
	emitter *sinkEmitter
	runtime binding.Runtime
	inputs  map[CallKind]element.InputPort
}

func (runner *runner) Run(ctx context.Context) error {
	options := runner.service.Options
	options.Sink = selectSinkEmitter(runner.emitter, options.Sink)
	live, err := runner.service.Binding.Start(ctx, options)
	if err != nil {
		_ = runner.service.Holder.fail(err)
		return err
	}
	runner.runtime = live
	managed := &managedRuntime{runtime: live}
	if err := runner.service.Holder.set(managed); err != nil {
		_ = managed.Close(context.Background(), err)
		return err
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wait sync.WaitGroup
	errorsOut := make(chan error, len(runner.inputs))
	for expected, input := range runner.inputs {
		wait.Add(1)
		go func(expected CallKind, input element.InputPort) {
			defer wait.Done()
			for {
				envelope, err := input.Receive(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					errorsOut <- err
					cancel(err)
					return
				}
				call, ok := envelope.Payload.(*Call)
				if !ok || call == nil || call.Kind != expected || call.Done == nil {
					err := fmt.Errorf("compatibility binding port %s received invalid call payload %T", expected, envelope.Payload)
					errorsOut <- err
					cancel(err)
					return
				}
				wait.Add(1)
				go func(call *Call) {
					defer wait.Done()
					callCtx := call.Context
					if callCtx == nil {
						callCtx = context.Background()
					}
					combined, stop := context.WithCancelCause(callCtx)
					cancelFromGraph := context.AfterFunc(ctx, func() { stop(context.Cause(ctx)) })
					err := runner.dispatch(combined, call)
					cancelFromGraph()
					stop(nil)
					select {
					case call.Done <- err:
					default:
					}
				}(call)
			}
		}(expected, input)
	}
	<-ctx.Done()
	runner.service.Holder.RequestClose(context.Cause(ctx))
	wait.Wait()
	select {
	case err := <-errorsOut:
		return err
	default:
		return nil
	}
}

func (runner *runner) dispatch(ctx context.Context, call *Call) error {
	switch call.Kind {
	case CallUpdate:
		return runner.runtime.Update(ctx, call.Settings)
	case CallAudio:
		return runner.runtime.Audio(ctx, call.Audio)
	case CallVideo:
		return runner.runtime.Video(ctx, call.Video)
	case CallText:
		return runner.runtime.Text(ctx, call.Text)
	case CallToolResult:
		return runner.runtime.ToolResult(ctx, call.ToolResult)
	case CallCommitAudio:
		return runner.runtime.CommitAudio(ctx)
	case CallCreateResponse:
		return runner.runtime.CreateResponse(ctx)
	case CallCancel:
		return runner.runtime.Cancel(ctx, call.Reason)
	case CallTruncate:
		return runner.runtime.Truncate(ctx, call.Truncate)
	default:
		return fmt.Errorf("unknown compatibility binding call %q", call.Kind)
	}
}

type sinkEmitter struct {
	output    element.OutputPort
	prefix    string
	sessionID string
	next      atomic.Uint64
}

func (sink *sinkEmitter) emit(ctx context.Context, event SinkEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	identity := fmt.Sprintf("%s-sink-%d", sink.prefix, sink.next.Add(1))
	event = cloneSinkEvent(event)
	event.Context = ctx
	event.Done = make(chan error, 1)
	result, err := sink.output.Broadcast(ctx, element.Envelope{
		Type: sinkEventType, ItemID: identity, SessionID: sink.sessionID,
		TraceID: identity, CancellationScope: identity, Payload: event,
	})
	if err != nil {
		return err
	}
	if result.Delivered != 1 {
		return fmt.Errorf("compatibility sink event %s delivered to %d lanes", identity, result.Delivered)
	}
	select {
	case err := <-event.Done:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func cloneSinkEvent(event SinkEvent) SinkEvent {
	event.Observation.Media = slices.Clone(event.Observation.Media)
	event.Utterance.AssistantItemIDs = slices.Clone(event.Utterance.AssistantItemIDs)
	event.Frame.PCM16LE = slices.Clone(event.Frame.PCM16LE)
	event.ToolCalls.Calls = slices.Clone(event.ToolCalls.Calls)
	for index := range event.ToolCalls.Calls {
		event.ToolCalls.Calls[index].Arguments = slices.Clone(event.ToolCalls.Calls[index].Arguments)
	}
	if event.ToolCalls.Usage != nil {
		copy := *event.ToolCalls.Usage
		event.ToolCalls.Usage = &copy
	}
	event.Debug.Attributes = cloneDebugMap(event.Debug.Attributes)
	event.Debug.Payload = cloneDebugMap(event.Debug.Payload)
	return event
}

type cloneVisit struct {
	typeOf  reflect.Type
	pointer uintptr
	length  int
	cap     int
}

// cloneDebugMap owns recursively mutable debug evidence without narrowing its
// concrete Go representation. Debug values are normally JSON-shaped, but the
// reflective fallback also preserves named slices, maps, pointers, and
// structs used by in-process developer sinks. Cycles retain their topology.
func cloneDebugMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	cloned := cloneDebugValue(reflect.ValueOf(input), make(map[cloneVisit]reflect.Value))
	return cloned.Interface().(map[string]any)
}

func cloneDebugValue(value reflect.Value, seen map[cloneVisit]reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		item := cloneDebugValue(value.Elem(), seen)
		output := reflect.New(value.Type()).Elem()
		output.Set(item)
		return output
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), pointer: value.Pointer()}
		if prior, found := seen[visit]; found {
			return prior
		}
		output := reflect.New(value.Type().Elem())
		seen[visit] = output
		output.Elem().Set(cloneDebugValue(value.Elem(), seen))
		return output
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), pointer: value.Pointer()}
		if prior, found := seen[visit]; found {
			return prior
		}
		output := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[visit] = output
		iterator := value.MapRange()
		for iterator.Next() {
			output.SetMapIndex(iterator.Key(), cloneDebugValue(iterator.Value(), seen))
		}
		return output
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{
			typeOf: value.Type(), pointer: value.Pointer(), length: value.Len(), cap: value.Cap(),
		}
		if prior, found := seen[visit]; found {
			return prior
		}
		output := reflect.MakeSlice(value.Type(), value.Len(), value.Cap())
		seen[visit] = output
		for index := 0; index < value.Len(); index++ {
			output.Index(index).Set(cloneDebugValue(value.Index(index), seen))
		}
		return output
	case reflect.Array:
		output := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			output.Index(index).Set(cloneDebugValue(value.Index(index), seen))
		}
		return output
	case reflect.Struct:
		// Copy unexported implementation state as a value, then recursively own
		// every exported field that can carry mutable developer evidence.
		output := reflect.New(value.Type()).Elem()
		output.Set(value)
		for index := 0; index < value.NumField(); index++ {
			if value.Field(index).CanInterface() && output.Field(index).CanSet() {
				output.Field(index).Set(cloneDebugValue(value.Field(index), seen))
			}
		}
		return output
	default:
		return value
	}
}

func (sink *sinkEmitter) TurnBegin(ctx context.Context) error {
	return sink.emit(ctx, SinkEvent{Kind: EventTurnBegin})
}
func (sink *sinkEmitter) TurnEnd(ctx context.Context, outcome binding.TurnOutcome) error {
	return sink.emit(ctx, SinkEvent{Kind: EventTurnEnd, TurnOutcome: outcome})
}
func (sink *sinkEmitter) Activity(ctx context.Context, event binding.ActivityEvent) error {
	return sink.emit(ctx, SinkEvent{Kind: EventActivity, Activity: event})
}
func (sink *sinkEmitter) Transcript(ctx context.Context, event binding.TranscriptEvent) error {
	return sink.emit(ctx, SinkEvent{Kind: EventTranscript, Transcript: event})
}
func (sink *sinkEmitter) Observation(ctx context.Context, observation perception.Observation) error {
	return sink.emit(ctx, SinkEvent{Kind: EventObservation, Observation: observation})
}

type reservationEmitter struct{ *sinkEmitter }

func (sink *reservationEmitter) SpeechReserved(ctx context.Context, utterance action.Utterance) error {
	return sink.emit(ctx, SinkEvent{Kind: EventSpeechReserved, Utterance: utterance})
}
func (sink *reservationEmitter) SpeechReservationCancelled(ctx context.Context, utterance action.Utterance) {
	_ = sink.emit(ctx, SinkEvent{Kind: EventSpeechReserveCanceled, Utterance: utterance})
}
func (sink *sinkEmitter) SpeechBegin(ctx context.Context, utterance action.Utterance) error {
	return sink.emit(ctx, SinkEvent{Kind: EventSpeechBegin, Utterance: utterance})
}
func (sink *sinkEmitter) SpeechText(ctx context.Context, utterance action.Utterance, text string) error {
	return sink.emit(ctx, SinkEvent{Kind: EventSpeechText, Utterance: utterance, Text: text})
}
func (sink *sinkEmitter) SpeechAudio(ctx context.Context, utterance action.Utterance, frame action.Frame) error {
	return sink.emit(ctx, SinkEvent{Kind: EventSpeechAudio, Utterance: utterance, Frame: frame})
}
func (sink *sinkEmitter) SpeechEnd(ctx context.Context, utterance action.Utterance, outcome action.Outcome) error {
	return sink.emit(ctx, SinkEvent{Kind: EventSpeechEnd, Utterance: utterance, SpeechEnd: outcome})
}
func (sink *sinkEmitter) ToolCalls(ctx context.Context, event binding.ToolCallEvent) error {
	return sink.emit(ctx, SinkEvent{Kind: EventToolCalls, ToolCalls: event})
}
func (sink *sinkEmitter) Failed(ctx context.Context, event binding.ErrorEvent) {
	_ = sink.emit(ctx, SinkEvent{Kind: EventFailed, Failure: event})
}

type debugEmitter struct{ *sinkEmitter }

func (sink *debugEmitter) Debug(ctx context.Context, event binding.DebugEvent) error {
	return sink.emit(ctx, SinkEvent{Kind: EventDebug, Debug: event})
}

type fullEmitter struct{ *sinkEmitter }

func (sink *fullEmitter) SpeechReserved(ctx context.Context, utterance action.Utterance) error {
	return sink.emit(ctx, SinkEvent{Kind: EventSpeechReserved, Utterance: utterance})
}
func (sink *fullEmitter) SpeechReservationCancelled(ctx context.Context, utterance action.Utterance) {
	_ = sink.emit(ctx, SinkEvent{Kind: EventSpeechReserveCanceled, Utterance: utterance})
}
func (sink *fullEmitter) Debug(ctx context.Context, event binding.DebugEvent) error {
	return sink.emit(ctx, SinkEvent{Kind: EventDebug, Debug: event})
}

func selectSinkEmitter(emitter *sinkEmitter, downstream binding.Sink) binding.Sink {
	_, reservations := downstream.(binding.SpeechReservationSink)
	_, debugging := downstream.(binding.DebugSink)
	switch {
	case reservations && debugging:
		return &fullEmitter{sinkEmitter: emitter}
	case reservations:
		return &reservationEmitter{sinkEmitter: emitter}
	case debugging:
		return &debugEmitter{sinkEmitter: emitter}
	default:
		return emitter
	}
}

var (
	_ binding.Sink                  = (*sinkEmitter)(nil)
	_ binding.SpeechReservationSink = (*reservationEmitter)(nil)
	_ binding.DebugSink             = (*debugEmitter)(nil)
	_ binding.SpeechReservationSink = (*fullEmitter)(nil)
	_ binding.DebugSink             = (*fullEmitter)(nil)
)
