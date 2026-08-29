// Package compatbinding mounts the existing binding.Runtime behind one coarse
// graph element. It is a migration oracle, not a reference architecture: its
// ports preserve current behavior while the binding internals are decomposed
// into ordinary perception, interaction, cognition, and action elements.
package compatbinding

import (
	"context"
	"fmt"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const SessionServiceName = "compat.binding.session"

var (
	settingsType       = element.Event(element.Named("session.Settings"))
	audioType          = element.Stream(element.Named("audio.InputFrame"))
	videoType          = element.Stream(element.Named("video.InputFrame"))
	textType           = element.Event(element.Named("text.UserInput"))
	toolResultType     = element.Event(element.Named("tool.Result"))
	commitAudioType    = element.Trigger(element.Named("audio.Commit"))
	createResponseType = element.Trigger(element.Named("response.Create"))
	cancelType         = element.Interrupt(element.Named("response.Scope"))
	truncateType       = element.Event(element.Named("audio.Truncation"))
	sinkEventType      = element.Stream(element.Named("compat.BindingSinkEvent"))
)

func Descriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "compat.BindingRuntime",
		Revision:      1,
		Ports: []element.Port{
			{Name: "update", Direction: element.Input, Type: settingsType, Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "audio", Direction: element.Input, Type: audioType, Cardinality: element.One, Required: true, DefaultDepth: 64},
			{Name: "video", Direction: element.Input, Type: videoType, Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 4},
			{Name: "text", Direction: element.Input, Type: textType, Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "tool_result", Direction: element.Input, Type: toolResultType, Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "commit_audio", Direction: element.Input, Type: commitAudioType, Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "create_response", Direction: element.Input, Type: createResponseType, Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "cancel", Direction: element.Input, Type: cancelType, Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "truncate", Direction: element.Input, Type: truncateType, Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "events", Direction: element.Output, Type: sinkEventType, Cardinality: element.One, Required: true, DefaultDepth: 256},
		},
		Reaction: element.Reaction{
			Triggers: []string{
				"update", "audio", "video", "text", "tool_result",
				"commit_audio", "create_response", "truncate",
			},
			Interrupts: []string{"cancel"}, Outcomes: []string{"events"},
		},
		Dependencies: []element.Dependency{{Name: SessionServiceName}},
		ConfigSchema: "schema://openrealtime/compat/binding-identity/v1",
	}
}

type Config struct {
	Name         string               `json:"name"`
	Ownership    binding.Ownership    `json:"ownership"`
	Capabilities binding.Capabilities `json:"capabilities"`
}

type CallKind string

const (
	CallUpdate         CallKind = "update"
	CallAudio          CallKind = "audio"
	CallVideo          CallKind = "video"
	CallText           CallKind = "text"
	CallToolResult     CallKind = "tool_result"
	CallCommitAudio    CallKind = "commit_audio"
	CallCreateResponse CallKind = "create_response"
	CallCancel         CallKind = "cancel"
	CallTruncate       CallKind = "truncate"
)

// Call stays in-process and is intentionally absent from descriptor bundles
// for remote placement. The compatibility element is local-only; decomposed
// elements use serializable protocol payloads.
type Call struct {
	Kind       CallKind
	Context    context.Context
	Settings   binding.Settings
	Audio      perception.Frame
	Video      perception.Frame
	Text       binding.TextInput
	ToolResult trajectory.ToolResult
	Reason     string
	Truncate   binding.Truncation
	Done       chan error
}

type SinkEventKind string

const (
	EventTurnBegin             SinkEventKind = "turn_begin"
	EventTurnEnd               SinkEventKind = "turn_end"
	EventActivity              SinkEventKind = "activity"
	EventTranscript            SinkEventKind = "transcript"
	EventObservation           SinkEventKind = "observation"
	EventSpeechReserved        SinkEventKind = "speech_reserved"
	EventSpeechReserveCanceled SinkEventKind = "speech_reservation_cancelled"
	EventSpeechBegin           SinkEventKind = "speech_begin"
	EventSpeechText            SinkEventKind = "speech_text"
	EventSpeechAudio           SinkEventKind = "speech_audio"
	EventSpeechEnd             SinkEventKind = "speech_end"
	EventToolCalls             SinkEventKind = "tool_calls"
	EventFailed                SinkEventKind = "failed"
	EventDebug                 SinkEventKind = "debug"
)

type SinkEvent struct {
	Kind SinkEventKind
	// Context and Done are local-only synchronization coeffects. The coarse
	// compatibility element is deliberately not remotely placeable: existing
	// Sink methods are synchronous, so an adapter must return the downstream
	// result to the legacy producer instead of merely acknowledging enqueue.
	Context     context.Context
	Done        chan error
	TurnOutcome binding.TurnOutcome
	Activity    binding.ActivityEvent
	Transcript  binding.TranscriptEvent
	Observation perception.Observation
	Utterance   action.Utterance
	Text        string
	Frame       action.Frame
	SpeechEnd   action.Outcome
	ToolCalls   binding.ToolCallEvent
	Failure     binding.ErrorEvent
	Debug       binding.DebugEvent
}

// SessionService provides session-scoped coeffects without putting a Binding,
// Sink, dispatcher function, or secret into Graph IR.
type SessionService struct {
	Binding binding.Binding
	Options binding.Options
	Holder  *Holder
}

type managedRuntime struct {
	runtime binding.Runtime
	once    sync.Once
	err     error
}

func (runtime *managedRuntime) Close(ctx context.Context, cause error) error {
	runtime.once.Do(func() { runtime.err = runtime.runtime.Close(ctx, cause) })
	return runtime.err
}

// Holder is the management-plane handle published during mount.
type Holder struct {
	mu             sync.RWMutex
	runtime        *managedRuntime
	startErr       error
	ready          chan struct{}
	readyOnce      sync.Once
	closeRequested bool
	closeCause     error
}

func NewHolder() *Holder { return &Holder{ready: make(chan struct{})} }

func (holder *Holder) readyChannel() (<-chan struct{}, error) {
	if holder == nil {
		return nil, fmt.Errorf("compatibility binding runtime holder is nil")
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.ready == nil {
		holder.ready = make(chan struct{})
	}
	return holder.ready, nil
}

func (holder *Holder) complete(runtime *managedRuntime, startErr error) error {
	if holder == nil {
		return fmt.Errorf("compatibility binding session requires a runtime holder")
	}
	if (runtime == nil) == (startErr == nil) {
		return fmt.Errorf("compatibility binding completion requires exactly one runtime or error")
	}
	holder.mu.Lock()
	if holder.ready == nil {
		holder.ready = make(chan struct{})
	}
	if holder.runtime != nil || holder.startErr != nil {
		holder.mu.Unlock()
		return fmt.Errorf("compatibility binding runtime holder is already completed")
	}
	holder.runtime = runtime
	holder.startErr = startErr
	holder.mu.Unlock()
	holder.readyOnce.Do(func() { close(holder.ready) })
	return nil
}

func (holder *Holder) set(runtime *managedRuntime) error {
	return holder.complete(runtime, nil)
}

func (holder *Holder) fail(err error) error {
	if err == nil {
		err = fmt.Errorf("compatibility binding failed to start")
	}
	return holder.complete(nil, err)
}

func (holder *Holder) Wait(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("wait for compatibility binding runtime: nil context")
	}
	ready, err := holder.readyChannel()
	if err != nil {
		return err
	}
	select {
	case <-ready:
		holder.mu.RLock()
		defer holder.mu.RUnlock()
		return holder.startErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (holder *Holder) get() (*managedRuntime, error) {
	if holder == nil {
		return nil, fmt.Errorf("compatibility binding runtime holder is nil")
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	if holder.runtime == nil {
		if holder.startErr != nil {
			return nil, holder.startErr
		}
		return nil, fmt.Errorf("compatibility binding runtime is not mounted")
	}
	return holder.runtime, nil
}

// Status and Trajectory are management-plane reads and therefore do not
// compete with realtime observation channels.
func (holder *Holder) Status() (binding.Status, error) {
	runtime, err := holder.get()
	if err != nil {
		return binding.Status{}, err
	}
	return runtime.runtime.Status(), nil
}

func (holder *Holder) Trajectory() (trajectory.Snapshot, error) {
	runtime, err := holder.get()
	if err != nil {
		return trajectory.Snapshot{}, err
	}
	return runtime.runtime.Trajectory(), nil
}

// RequestClose records the session-level cause for the lifecycle disposer.
// The request is separate from graph cancellation so final sink events can be
// drained and acknowledged before output queues close.
func (holder *Holder) RequestClose(cause error) {
	if holder == nil {
		return
	}
	holder.mu.Lock()
	if !holder.closeRequested {
		holder.closeRequested = true
		holder.closeCause = cause
	}
	holder.mu.Unlock()
}

func (holder *Holder) dispose(ctx context.Context) error {
	if holder == nil {
		return nil
	}
	holder.mu.RLock()
	runtime := holder.runtime
	cause := holder.closeCause
	holder.mu.RUnlock()
	if runtime == nil {
		return nil
	}
	return runtime.Close(ctx, cause)
}
