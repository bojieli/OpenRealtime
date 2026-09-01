package binding

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// MountDependencyFactory supplies only per-session service objects whose
// names, artifacts, and mount scope were already frozen into the prepared
// graph plan. It cannot replace process services, implementations, secret
// providers, or dependency identities.
type MountDependencyFactory func(
	context.Context, legacy.Options,
) ([]graphruntime.PreparedMountDependency, error)

// SessionAdapter is the deliberately small seam between the stable Realtime
// session API and a graph's exported typed boundaries. Implementations contain
// protocol-to-boundary translation only; conversation policy belongs in graph
// elements. Run drains graph outputs until its context is cancelled.
//
// NativeRuntime keeps the gateway's legacy.Runtime surface stable. The adapter
// deliberately excludes Status: graph identity comes from the immutable plan,
// while live node, capability, deployment, and adapter evidence comes from the
// mounted graph's authenticated inspection snapshot. An adapter must not
// manufacture a competing topology/role projection or mount a second graph.
type SessionAdapter interface {
	Run(context.Context) error
	Update(context.Context, legacy.Settings) error
	Audio(context.Context, perception.Frame) error
	Video(context.Context, perception.Frame) error
	Text(context.Context, legacy.TextInput) error
	ToolResult(context.Context, trajectory.ToolResult) error
	CommitAudio(context.Context) error
	CreateResponse(context.Context) error
	Cancel(context.Context, string) error
	Truncate(context.Context, legacy.Truncation) error
	Trajectory() trajectory.Snapshot
	Close(context.Context, error) error
}

// AdapterFactory binds one mounted graph to one client session. It must acquire
// graph ports synchronously and return only after the adapter is ready for Run;
// this lets NativeBinding start the output drainer before graph nodes can
// publish readiness or response events.
type AdapterFactory func(
	context.Context, *graphruntime.Mounted, legacy.Options, SessionAdapterProfile,
) (SessionAdapter, error)

// NativeConfig describes a graph-native binding. Plan and AdapterProfile are
// immutable and shared by sessions; Catalog is selected and preflighted once;
// MountDependencies may supply only the exact session-scoped service values
// that preflight deliberately left unacquired.
type NativeConfig struct {
	Plan *graphconfig.Plan
	// Catalog and SecretCatalog are the complete resource-free process
	// inventory validated when the binding is constructed, before a listener
	// can accept traffic.
	Catalog       graphassembly.Catalog
	SecretCatalog *graphsecret.Document

	MountDependencies MountDependencyFactory
	AdapterProfile    SessionAdapterProfile
	Adapter           AdapterRegistration

	Inspection      graphruntime.InspectionConfig
	ShutdownTimeout time.Duration
	TraceRecording  *TraceRecordingConfig
}

// NativeBinding creates one independently mounted graph per Realtime session.
// It is the inverse of Binding: Binding places a legacy runtime behind a coarse
// compatibility node, while NativeBinding presents a fine-grained graph to the
// unchanged protocol gateway.
type NativeBinding struct {
	name              string
	plan              *graphconfig.Plan
	prepared          *graphruntime.PreparedPlan
	mountDependencies MountDependencyFactory
	adapterProfile    SessionAdapterProfile
	adapter           AdapterRegistration
	adapterResolution inspect.SessionAdapterResolution
	inspection        graphruntime.InspectionConfig
	shutdownTimeout   time.Duration
	traceRecording    *TraceRecordingConfig
}

// NewNative validates and freezes the process-level graph session contract.
// It performs no provider discovery, secret resolution, or resource mounting;
// those operations remain behind Start and graph/assembly's exact preflight.
func NewNative(config NativeConfig) (*NativeBinding, error) {
	if config.Plan == nil {
		return nil, errors.New("native graph binding requires an immutable plan")
	}
	if err := config.Plan.Validate(); err != nil {
		return nil, fmt.Errorf("native graph binding plan: %w", err)
	}
	if err := config.AdapterProfile.ValidateGraph(config.Plan.Graph()); err != nil {
		return nil, fmt.Errorf("native graph binding adapter profile: %w", err)
	}
	if err := config.Adapter.validate(); err != nil {
		return nil, fmt.Errorf("native graph binding %s: %w", config.AdapterProfile.Name, err)
	}
	adapterResolution := config.AdapterProfile.resolution(config.Adapter)
	if err := adapterResolution.Validate(); err != nil {
		return nil, fmt.Errorf("native graph binding %s adapter resolution: %w", config.AdapterProfile.Name, err)
	}
	if config.Inspection.MaxFlows < 0 || config.Inspection.MaxEdgesPerFlow < 0 ||
		config.Inspection.MaxCorrelationBytes < 0 {
		return nil, fmt.Errorf("native graph binding %q inspection bounds cannot be negative", config.AdapterProfile.Name)
	}
	if config.ShutdownTimeout < 0 {
		return nil, fmt.Errorf("native graph binding %q shutdown timeout cannot be negative", config.AdapterProfile.Name)
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = 5 * time.Second
	}
	selected, err := config.Catalog.Select(config.Plan, config.SecretCatalog)
	if err != nil {
		return nil, fmt.Errorf("native graph binding %q startup assembly: %w", config.AdapterProfile.Name, err)
	}
	prepared, err := graphassembly.Preflight(context.Background(), config.Plan, selected)
	if err != nil {
		return nil, fmt.Errorf("native graph binding %q startup preflight: %w", config.AdapterProfile.Name, err)
	}

	profile := config.AdapterProfile.Clone()
	var recording *TraceRecordingConfig
	if config.TraceRecording != nil {
		copy := *config.TraceRecording
		recording = &copy
	}
	return &NativeBinding{
		name: profile.Name,
		plan: config.Plan, prepared: prepared,
		mountDependencies: config.MountDependencies,
		adapterProfile:    profile, adapter: config.Adapter,
		adapterResolution: adapterResolution, inspection: config.Inspection,
		shutdownTimeout: config.ShutdownTimeout, traceRecording: recording,
	}, nil
}

func (binding *NativeBinding) Name() string { return binding.name }

// SessionAdapterProfile is the graph-native protocol contract consumed by the
// gateway. Returning a defensive clone keeps the binding's mounted boundary
// selection immutable without recreating legacy Ownership/Capabilities methods.
func (binding *NativeBinding) SessionAdapterProfile() SessionAdapterProfile {
	return binding.adapterProfile.Clone()
}

func (binding *NativeBinding) Graph() ir.Graph { return binding.plan.Graph() }

func (binding *NativeBinding) Start(
	ctx context.Context, options legacy.Options,
) (legacy.Runtime, error) {
	if ctx == nil {
		return nil, errors.New("start native graph binding: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if options.Sink == nil {
		return nil, errors.New("start native graph binding: sink is required")
	}
	options = cloneNativeOptions(options)

	recording, err := binding.recordingConfig()
	if err != nil {
		return nil, err
	}
	if recording != nil {
		defer eraseNativeBytes(recording.SessionCorrelationKey)
	}
	mountOptions := graphruntime.PreparedMountOptions{
		Inspection: binding.inspection, ShutdownTimeout: binding.shutdownTimeout,
		TraceRecording: recording,
	}
	if binding.mountDependencies != nil {
		mountOptions.Dependencies, err = binding.mountDependencies(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("start native graph binding %s mount dependencies: %w", binding.name, err)
		}
	}
	mounted, err := binding.prepared.Mount(ctx, mountOptions)
	if err != nil {
		return nil, fmt.Errorf("start native graph binding %s: %w", binding.name, err)
	}
	adapter, err := binding.adapter.Factory(ctx, mounted, options, binding.adapterProfile.Clone())
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), binding.shutdownTimeout)
		defer cancel()
		return nil, errors.Join(
			fmt.Errorf("start native graph binding %s adapter: %w", binding.name, err),
			mounted.Close(cleanupCtx),
		)
	}
	if adapter == nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), binding.shutdownTimeout)
		defer cancel()
		return nil, errors.Join(
			fmt.Errorf("start native graph binding %s adapter: factory returned nil", binding.name),
			mounted.Close(cleanupCtx),
		)
	}

	sessionCtx, cancel := context.WithCancelCause(ctx)
	runtime := &NativeRuntime{
		binding: binding, mounted: mounted, adapter: adapter, sink: options.Sink,
		ctx: sessionCtx, cancel: cancel, done: make(chan struct{}),
		results: make(chan nativeComponentResult, 2),
	}
	// The adapter drainer is scheduled first so finite startup queues are not
	// the only thing protecting a graph that reports readiness immediately.
	go runtime.runComponent("adapter", adapter.Run)
	go runtime.runComponent("graph", mounted.Run)
	go runtime.supervise()
	return runtime, nil
}

func cloneNativeOptions(options legacy.Options) legacy.Options {
	options.Settings = legacy.CloneSettings(options.Settings)
	if options.Policies != nil {
		policies := *options.Policies
		options.Policies = &policies
	}
	return options
}

func (binding *NativeBinding) recordingConfig() (*graphruntime.TraceRecordingConfig, error) {
	if binding.traceRecording == nil {
		return nil, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("start native graph binding trace key: %w", err)
	}
	return &graphruntime.TraceRecordingConfig{
		Limits: binding.traceRecording.Limits, SessionCorrelationKey: key,
		MaxRetainedBytes: binding.traceRecording.MaxRetainedBytes,
		CaptureInterval:  binding.traceRecording.CaptureInterval,
	}, nil
}

func eraseNativeBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

type nativeComponentResult struct {
	name string
	err  error
}

// NativeRuntime supervises one adapter and one mounted graph as a single
// session. The management plane observes the mounted graph directly; protocol
// calls remain delegated to the boundary adapter.
type NativeRuntime struct {
	binding *NativeBinding
	mounted *graphruntime.Mounted
	adapter SessionAdapter
	sink    legacy.Sink

	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}
	results chan nativeComponentResult

	mu     sync.Mutex
	runErr error
	closed bool
}

func (runtime *NativeRuntime) runComponent(
	name string, run func(context.Context) error,
) {
	runtime.results <- nativeComponentResult{name: name, err: run(runtime.ctx)}
}

func (runtime *NativeRuntime) supervise() {
	first := <-runtime.results
	cause := context.Cause(runtime.ctx)
	unexpected := cause == nil
	var primaryErr error
	var failureDone chan struct{}
	var failureContext context.Context
	if unexpected {
		if first.err != nil {
			cause = fmt.Errorf("native graph session %s stopped: %w", first.name, first.err)
		} else {
			cause = fmt.Errorf("native graph session %s stopped without cancellation", first.name)
		}
		primaryErr = cause
	}
	if cause == nil {
		cause = graphruntime.ErrGraphClosed
	}
	// Cancellation must never wait behind a client-facing error callback. A
	// sink is an extension boundary and may be slow or buggy; the graph and its
	// adapter still have to observe the failure immediately.
	runtime.cancel(cause)
	if unexpected {
		notificationContext, cancelNotification := context.WithTimeout(
			context.Background(), runtime.binding.shutdownTimeout,
		)
		defer cancelNotification()
		failureContext = notificationContext
		failureDone = make(chan struct{})
		event := legacy.ErrorEvent{Code: "graph_session_failed", Message: cause.Error()}
		go func() {
			defer close(failureDone)
			runtime.sink.Failed(failureContext, event)
		}()
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), runtime.binding.shutdownTimeout)
	closeErrors := make(chan error, 2)
	go func() { closeErrors <- runtime.adapter.Close(cleanupCtx, cause) }()
	go func() { closeErrors <- runtime.mounted.Close(cleanupCtx) }()
	cleanupErr := errors.Join(<-closeErrors, <-closeErrors)
	cleanupCancel()

	var second nativeComponentResult
	var stopErr error
	select {
	case second = <-runtime.results:
		stopErr = nativeUnexpectedComponentError(second, unexpected)
	case <-time.After(runtime.binding.shutdownTimeout):
		stopErr = fmt.Errorf("native graph session shutdown timed out after %s waiting for sibling component",
			runtime.binding.shutdownTimeout)
	}
	var failureErr error
	if failureDone != nil {
		select {
		case <-failureDone:
		case <-failureContext.Done():
			failureErr = fmt.Errorf("native graph session failure notification timed out after %s",
				runtime.binding.shutdownTimeout)
		}
	}
	runtime.mu.Lock()
	runtime.runErr = errors.Join(primaryErr, stopErr, cleanupErr, failureErr)
	runtime.closed = true
	runtime.mu.Unlock()
	close(runtime.done)
}

func nativeUnexpectedComponentError(result nativeComponentResult, unexpected bool) error {
	if !unexpected || result.err == nil || errors.Is(result.err, context.Canceled) ||
		errors.Is(result.err, graphruntime.ErrGraphClosed) {
		return nil
	}
	return fmt.Errorf("native graph session %s: %w", result.name, result.err)
}

func (runtime *NativeRuntime) available() error {
	select {
	case <-runtime.done:
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		if runtime.runErr != nil {
			return runtime.runErr
		}
		return graphruntime.ErrGraphClosed
	default:
		if err := context.Cause(runtime.ctx); err != nil {
			return err
		}
		return nil
	}
}

func (runtime *NativeRuntime) Update(ctx context.Context, settings legacy.Settings) error {
	if err := runtime.available(); err != nil {
		return err
	}
	return runtime.adapter.Update(ctx, legacy.CloneSettings(settings))
}

func (runtime *NativeRuntime) Audio(ctx context.Context, frame perception.Frame) error {
	if err := runtime.available(); err != nil {
		return err
	}
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	frame.Image = slices.Clone(frame.Image)
	return runtime.adapter.Audio(ctx, frame)
}

func (runtime *NativeRuntime) Video(ctx context.Context, frame perception.Frame) error {
	if err := runtime.available(); err != nil {
		return err
	}
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	frame.Image = slices.Clone(frame.Image)
	return runtime.adapter.Video(ctx, frame)
}

func (runtime *NativeRuntime) Text(ctx context.Context, input legacy.TextInput) error {
	if err := runtime.available(); err != nil {
		return err
	}
	input.Images = slices.Clone(input.Images)
	for index := range input.Images {
		input.Images[index].Bytes = slices.Clone(input.Images[index].Bytes)
	}
	return runtime.adapter.Text(ctx, input)
}

func (runtime *NativeRuntime) ToolResult(ctx context.Context, result trajectory.ToolResult) error {
	if err := runtime.available(); err != nil {
		return err
	}
	result.Output = slices.Clone(result.Output)
	return runtime.adapter.ToolResult(ctx, result)
}

func (runtime *NativeRuntime) CommitAudio(ctx context.Context) error {
	if err := runtime.available(); err != nil {
		return err
	}
	return runtime.adapter.CommitAudio(ctx)
}

func (runtime *NativeRuntime) CreateResponse(ctx context.Context) error {
	if err := runtime.available(); err != nil {
		return err
	}
	return runtime.adapter.CreateResponse(ctx)
}

func (runtime *NativeRuntime) Cancel(ctx context.Context, reason string) error {
	if err := runtime.available(); err != nil {
		return err
	}
	return runtime.adapter.Cancel(ctx, reason)
}

func (runtime *NativeRuntime) Truncate(ctx context.Context, truncation legacy.Truncation) error {
	if err := runtime.available(); err != nil {
		return err
	}
	return runtime.adapter.Truncate(ctx, truncation)
}

func (runtime *NativeRuntime) Trajectory() trajectory.Snapshot {
	return runtime.adapter.Trajectory()
}

func (runtime *NativeRuntime) Status() legacy.Status {
	graph := runtime.binding.plan.Graph()
	return legacy.Status{
		Binding: runtime.binding.name,
		Profile: runtime.binding.adapterProfile.Fingerprint,
		Graph: legacy.ArchitectureIdentity{
			ID: graph.ID, Revision: int(graph.Revision), Fingerprint: graph.Fingerprint,
		},
	}
}

func (runtime *NativeRuntime) Graph() ir.Graph { return runtime.binding.plan.Graph() }
func (runtime *NativeRuntime) Live() inspect.Live {
	live := runtime.mounted.Live()
	resolution := runtime.binding.adapterResolution
	live.Adapter = &resolution
	return live
}
func (runtime *NativeRuntime) Done() <-chan struct{} { return runtime.done }

func (runtime *NativeRuntime) RecordedTrace() (inspect.LiveTrace, error) {
	trace, err := runtime.mounted.RecordedTrace()
	if err != nil {
		return inspect.LiveTrace{}, err
	}
	resolution := runtime.binding.adapterResolution
	trace.Adapter = &resolution
	return inspect.FreezeLiveTrace(trace)
}

func (runtime *NativeRuntime) Close(ctx context.Context, cause error) error {
	if ctx == nil {
		return errors.New("close native graph session: nil context")
	}
	graphCause := error(graphruntime.ErrGraphClosed)
	if cause != nil {
		graphCause = fmt.Errorf("%w: %v", graphruntime.ErrGraphClosed, cause)
	}
	runtime.cancel(graphCause)
	select {
	case <-runtime.done:
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.runErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

var _ legacy.Runtime = (*NativeRuntime)(nil)
