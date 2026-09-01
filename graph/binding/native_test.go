package binding_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	videograph "github.com/bojieli/OpenRealtime/elements/video"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestNativeBindingRunsAnExactPlanThroughTypedSessionBoundaries(t *testing.T) {
	plan, catalog := nativeVideoPlan(t)
	profile := nativeVideoProfile(t, plan)
	observed := make(chan videograph.InlineFrame, 1)
	bind, err := graphbinding.NewNative(graphbinding.NativeConfig{
		Plan: plan, Catalog: catalog, AdapterProfile: profile,
		Adapter: graphbinding.AdapterRegistration{
			Reference: "go://openrealtime/test/native-video-adapter",
			Artifact:  nativeAdapterArtifact("native-video-adapter", "a"),
			Factory: func(
				_ context.Context, mounted *graphruntime.Mounted, options legacy.Options,
				selected graphbinding.SessionAdapterProfile,
			) (graphbinding.SessionAdapter, error) {
				if selected.Fingerprint != profile.Fingerprint {
					return nil, errors.New("adapter received a different frozen profile")
				}
				return newNativeVideoAdapter(mounted, options, observed)
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bind.Graph().Fingerprint != plan.Graph().Fingerprint {
		t.Fatal("native binding did not retain the immutable plan graph")
	}

	live, err := bind.Start(context.Background(), legacy.Options{
		Sink: &recordingSink{}, SessionID: "native-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := perception.Frame{
		Kind: perception.FrameImage, Source: "screen", CapturedNS: 1_000_000,
		Index: 7, Image: []byte{1, 2, 3, 4}, MIMEType: "image/jpeg", Width: 2, Height: 2,
	}
	if err := live.Video(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	frame.Image[0] = 99
	select {
	case relayed := <-observed:
		if relayed.Frame.Index != 7 || relayed.Frame.Image[0] != 1 ||
			relayed.StreamID != "native-session:screen" {
			t.Fatalf("relayed graph frame = %+v", relayed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("typed graph frame did not cross the mounted graph")
	}

	status := live.Status()
	wantStatus := legacy.Status{
		Binding: profile.Name, Profile: profile.Fingerprint,
		Graph: legacy.ArchitectureIdentity{
			ID: plan.Graph().ID, Revision: int(plan.Graph().Revision),
			Fingerprint: plan.Graph().Fingerprint,
		},
	}
	if !reflect.DeepEqual(status, wantStatus) {
		t.Fatalf("native status = %+v", status)
	}
	encodedStatus, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var statusFields map[string]json.RawMessage
	if err := json.Unmarshal(encodedStatus, &statusFields); err != nil {
		t.Fatal(err)
	}
	if len(statusFields) != 3 || statusFields["binding"] == nil ||
		statusFields["profile"] == nil || statusFields["graph"] == nil {
		t.Fatalf("graph-native JSON status has legacy projections: %s", encodedStatus)
	}
	native := live.(*graphbinding.NativeRuntime)
	inspection := native.Live()
	if inspection.Fingerprint != plan.Graph().Fingerprint ||
		inspection.Deployment == nil || inspection.Deployment.Public.Digest == "" {
		t.Fatalf("native live inspection = %+v", inspection)
	}
	if inspection.Adapter == nil || inspection.Adapter.ProfileFingerprint != profile.Fingerprint ||
		inspection.Adapter.Runtime != nativeAdapterArtifact("native-video-adapter", "a") ||
		inspection.Adapter.RuntimeEvidence != inspect.EvidenceRegistered ||
		inspection.Adapter.BoundaryMapDigest != profile.BoundaryMapDigest {
		t.Fatalf("native adapter resolution = %+v", inspection.Adapter)
	}
	if node := inspection.Nodes["ingress"]; node.Resolution == nil ||
		node.Resolution.RuntimeEvidence != "live" {
		t.Fatalf("native node resolution = %+v", node)
	}
	if err := live.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestNativeBindingFailsBeforeListenerStartupWhenPluginInventoryIsIncomplete(t *testing.T) {
	plan, catalog := nativeVideoPlan(t)
	profile := nativeVideoProfile(t, plan)
	catalog.Implementations = nil
	_, err := graphbinding.NewNative(graphbinding.NativeConfig{
		Plan: plan, Catalog: catalog, AdapterProfile: profile,
		Adapter: graphbinding.AdapterRegistration{
			Reference: "go://openrealtime/test/missing-adapter",
			Artifact:  nativeAdapterArtifact("missing-adapter", "b"),
			Factory: func(
				context.Context, *graphruntime.Mounted, legacy.Options,
				graphbinding.SessionAdapterProfile,
			) (graphbinding.SessionAdapter, error) {
				return nil, errors.New("must not be reached")
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "startup assembly") ||
		!strings.Contains(err.Error(), "missing") {
		t.Fatalf("incomplete startup inventory error = %v", err)
	}
}

func TestNativeBindingRejectsAdapterEvidenceThatLiveInspectionCannotRepresent(t *testing.T) {
	plan, catalog := nativeVideoPlan(t)
	profile := nativeVideoProfile(t, plan)
	tests := []struct {
		name     string
		artifact inspect.ArtifactIdentity
	}{
		{
			name: "uppercase digest",
			artifact: inspect.ArtifactIdentity{
				ID: "go://openrealtime/test/adapter", Revision: "implementation:v1",
				Digest: "sha256:" + strings.Repeat("A", 64),
			},
		},
		{
			name: "control in ID",
			artifact: inspect.ArtifactIdentity{
				ID: "go://openrealtime/test/adapter\x00hidden", Revision: "implementation:v1",
				Digest: "sha256:" + strings.Repeat("a", 64),
			},
		},
		{
			name: "control in revision",
			artifact: inspect.ArtifactIdentity{
				ID: "go://openrealtime/test/adapter", Revision: "implementation\x00v1",
				Digest: "sha256:" + strings.Repeat("a", 64),
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := graphbinding.NewNative(graphbinding.NativeConfig{
				Plan: plan, Catalog: catalog, AdapterProfile: profile,
				Adapter: graphbinding.AdapterRegistration{
					Reference: "go://openrealtime/test/adapter",
					Artifact:  testCase.artifact,
					Factory: func(
						context.Context, *graphruntime.Mounted, legacy.Options,
						graphbinding.SessionAdapterProfile,
					) (graphbinding.SessionAdapter, error) {
						return nil, errors.New("must not reach adapter factory")
					},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "adapter resolution") {
				t.Fatalf("NewNative() error = %v", err)
			}
		})
	}
}

func TestNativeBindingCancellationDoesNotWaitBehindFailureSink(t *testing.T) {
	plan, catalog := nativeVideoPlan(t)
	profile := nativeVideoProfile(t, plan)
	sink := &contextBlockingFailureSink{
		recordingSink: &recordingSink{}, entered: make(chan struct{}),
	}
	bind, err := graphbinding.NewNative(graphbinding.NativeConfig{
		Plan: plan, Catalog: catalog, AdapterProfile: profile,
		ShutdownTimeout: 50 * time.Millisecond,
		Adapter: graphbinding.AdapterRegistration{
			Reference: "go://openrealtime/test/failing-adapter",
			Artifact:  nativeAdapterArtifact("failing-adapter", "e"),
			Factory: func(
				_ context.Context, mounted *graphruntime.Mounted, options legacy.Options,
				_ graphbinding.SessionAdapterProfile,
			) (graphbinding.SessionAdapter, error) {
				adapter, err := newNativeVideoAdapter(mounted, options, make(chan videograph.InlineFrame, 1))
				if err != nil {
					return nil, err
				}
				return &failingRunAdapter{SessionAdapter: adapter}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := bind.Start(context.Background(), legacy.Options{Sink: sink, SessionID: "failure-session"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("failure sink was not called")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	err = runtime.Close(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "forced adapter failure") {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("native cancellation waited %s behind the failure sink", elapsed)
	}
}

func TestNativeBindingRejectsExcessSessionDependencyBeforeMount(t *testing.T) {
	plan, catalog := nativeVideoPlan(t)
	profile := nativeVideoProfile(t, plan)
	bind, err := graphbinding.NewNative(graphbinding.NativeConfig{
		Plan: plan, Catalog: catalog, AdapterProfile: profile,
		MountDependencies: func(
			context.Context, legacy.Options,
		) ([]graphruntime.PreparedMountDependency, error) {
			return []graphruntime.PreparedMountDependency{{
				Name: "service.excess", Artifact: nativeAdapterArtifact("excess-service", "c"),
				Service: &struct{}{},
			}}, nil
		},
		Adapter: graphbinding.AdapterRegistration{
			Reference: "go://openrealtime/test/excess-adapter",
			Artifact:  nativeAdapterArtifact("excess-adapter", "d"),
			Factory: func(
				context.Context, *graphruntime.Mounted, legacy.Options,
				graphbinding.SessionAdapterProfile,
			) (graphbinding.SessionAdapter, error) {
				return nil, errors.New("must not mount")
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bind.Start(context.Background(), legacy.Options{Sink: &recordingSink{}})
	if err == nil || !strings.Contains(err.Error(), "excess [service.excess]") {
		t.Fatalf("session dependency overlay error = %v", err)
	}
}

// nativeVideoAdapter is intentionally policy-free. It translates the stable
// gateway video method to one typed graph boundary and translates the graph's
// typed output to the stable observation sink. The frame ingress element—not
// the adapter—owns relay behavior and live implementation evidence.
type nativeVideoAdapter struct {
	input    element.OutputPort
	output   element.InputPort
	inputTyp element.Type
	sink     legacy.Sink
	session  string
	next     atomic.Uint64
	observed chan<- videograph.InlineFrame
}

type failingRunAdapter struct {
	graphbinding.SessionAdapter
}

func (*failingRunAdapter) Run(context.Context) error {
	return errors.New("forced adapter failure")
}

type contextBlockingFailureSink struct {
	*recordingSink
	entered chan struct{}
}

func (sink *contextBlockingFailureSink) Failed(ctx context.Context, event legacy.ErrorEvent) {
	close(sink.entered)
	<-ctx.Done()
	sink.recordingSink.Failed(ctx, event)
}

func newNativeVideoAdapter(
	mounted *graphruntime.Mounted, options legacy.Options, observed chan<- videograph.InlineFrame,
) (*nativeVideoAdapter, error) {
	input, err := mounted.Ingress("frame")
	if err != nil {
		return nil, err
	}
	output, err := mounted.Egress("frames")
	if err != nil {
		return nil, err
	}
	typeName, err := nativeBoundaryType(mounted.Graph(), "frame", ir.InputBoundary)
	if err != nil {
		return nil, err
	}
	return &nativeVideoAdapter{
		input: input, output: output, inputTyp: typeName, sink: options.Sink,
		session: options.SessionID, observed: observed,
	}, nil
}

func (adapter *nativeVideoAdapter) Run(ctx context.Context) error {
	for {
		envelope, err := adapter.output.Receive(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, graphruntime.ErrChannelClosed) {
				return nil
			}
			return err
		}
		frame, ok := envelope.Payload.(videograph.InlineFrame)
		if !ok {
			return fmt.Errorf("native video output has payload %T", envelope.Payload)
		}
		select {
		case adapter.observed <- frame:
		case <-ctx.Done():
			return nil
		}
		if err := adapter.sink.Observation(ctx, perception.Observation{
			Text: "graph received frame", Observer: "native-video", Source: frame.Frame.Source,
			Authority: trajectory.AuthorityObserver, Final: true, OccurredNS: frame.Frame.CapturedNS,
		}); err != nil {
			return err
		}
	}
}

func (*nativeVideoAdapter) Update(context.Context, legacy.Settings) error { return nil }
func (*nativeVideoAdapter) Audio(context.Context, perception.Frame) error {
	return legacy.ErrUnsupported
}
func (adapter *nativeVideoAdapter) Video(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	identity := adapter.next.Add(1)
	item := fmt.Sprintf("%s-video-%d", adapter.session, identity)
	result, err := adapter.input.Broadcast(ctx, element.Envelope{
		Type: adapter.inputTyp, ItemID: item, SessionID: adapter.session,
		TraceID: item, CancellationScope: item,
		Payload: videograph.InlineFrame{
			StreamID: adapter.session + ":" + frame.Source, SourceRevision: 1, Frame: frame,
		},
	})
	if err != nil {
		return err
	}
	if result.Delivered != 1 {
		return fmt.Errorf("native video frame delivered to %d graph lanes", result.Delivered)
	}
	return nil
}
func (*nativeVideoAdapter) Text(context.Context, legacy.TextInput) error {
	return legacy.ErrUnsupported
}
func (*nativeVideoAdapter) ToolResult(context.Context, trajectory.ToolResult) error {
	return legacy.ErrUnsupported
}
func (*nativeVideoAdapter) CommitAudio(context.Context) error    { return legacy.ErrUnsupported }
func (*nativeVideoAdapter) CreateResponse(context.Context) error { return legacy.ErrUnsupported }
func (*nativeVideoAdapter) Cancel(context.Context, string) error { return nil }
func (*nativeVideoAdapter) Truncate(context.Context, legacy.Truncation) error {
	return legacy.ErrUnsupported
}
func (*nativeVideoAdapter) Trajectory() trajectory.Snapshot    { return trajectory.Snapshot{} }
func (*nativeVideoAdapter) Close(context.Context, error) error { return nil }

func nativeBoundaryType(graph ir.Graph, name string, direction ir.BoundaryDirection) (element.Type, error) {
	for _, boundary := range graph.Boundaries {
		if boundary.Name == name && boundary.Direction == direction {
			return boundary.Type.Clone(), nil
		}
	}
	return element.Type{}, fmt.Errorf("graph %s has no %s boundary %q", graph.ID, direction, name)
}

func nativeVideoPlan(t testing.TB) (*graphconfig.Plan, graphassembly.Catalog) {
	t.Helper()
	descriptors, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.AssemblyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := catalog.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		t.Fatal(err)
	}
	topology := graphconfig.Artifact{Path: "agent.ortg", Data: []byte(`graph native_video {
    video.FrameIngress :: ingress;
    input frame = ingress.frame_in;
    input reference = ingress.reference_in;
    output frames = ingress.frames;
    output references = ingress.references;
}
`)}
	options := graphconfig.Options{
		Catalog: descriptors, Discovery: discovery, SchemaResolver: schemas,
		Loader: graphcompiler.FileLoader{},
	}
	updated, err := graphconfig.UpdateLock(context.Background(), topology, graphconfig.Artifact{}, options)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := updated.Lock().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := graphconfig.Create(context.Background(), graphconfig.Artifacts{
		Topology: topology,
		Values: graphconfig.Artifact{Path: "agent.values.yaml", Data: []byte(`apiVersion: openrealtime.ai/config/v1alpha1
graph: native_video
nodes: {}
`)},
		Lock: graphconfig.Artifact{Path: "openrealtime.lock", Data: lock},
		Deployment: graphconfig.Artifact{Path: "agent.deployment.yaml", Data: []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: native_video
nodes: {}
`)},
	}, options)
	if err != nil {
		t.Fatal(err)
	}
	return plan, catalog
}

func nativeVideoProfile(t testing.TB, plan *graphconfig.Plan) graphbinding.SessionAdapterProfile {
	t.Helper()
	graph := plan.Graph()
	videoType, err := nativeBoundaryType(graph, "frame", ir.InputBoundary)
	if err != nil {
		t.Fatal(err)
	}
	observationType, err := nativeBoundaryType(graph, "frames", ir.OutputBoundary)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          "openrealtime.graph.native-video", Revision: 1,
		GraphFingerprint: graph.Fingerprint, Ownership: nativeOwnership(),
		Capabilities: legacy.Capabilities{
			Video: true, Observations: true, FastSlow: true,
		},
		Boundaries: []graphbinding.AdapterBoundary{
			{Operation: graphbinding.AdapterInputVideo, Boundary: "frame", Direction: ir.InputBoundary, Type: videoType},
			{Operation: graphbinding.AdapterOutputObservation, Boundary: "frames", Direction: ir.OutputBoundary, Type: observationType},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func nativeAdapterArtifact(name, hexadecimal string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "go://openrealtime/test/" + name, Revision: "implementation:v1",
		Digest: "sha256:" + strings.Repeat(hexadecimal, 64),
	}
}

func nativeOwnership() legacy.Ownership {
	return legacy.Ownership{
		Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
		Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
	}
}
