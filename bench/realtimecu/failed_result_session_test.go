package realtimecu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	realtimecubinding "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// This controlled session keeps the production graph, gateway, benchmark
// client, dispatcher, and scorer. Only the model, observer, policy, and browser
// surface are deterministic fixtures. No benchmark score is published.
func TestFailedEffectRecoversAndSettlesAcrossRealtimeSession(t *testing.T) {
	model := &failedResultSessionModel{}
	observer := &failedResultSessionObserver{}
	policy := &failedResultSessionPolicy{observer: observer}
	endpoint, mounted := failedResultSessionEndpoint(t, model, observer, policy)
	var frame bytes.Buffer
	if err := png.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 1280, 720))); err != nil {
		t.Fatal(err)
	}
	var pageMu sync.Mutex
	var started time.Time
	page := PageResult{}
	effects := 0
	const failure = "target moved before the click was confirmed"
	surface := &failedEffectSurface{click: func() error {
		pageMu.Lock()
		defer pageMu.Unlock()
		effects++
		if effects == 1 {
			return errors.New(failure)
		}
		page = PageResult{Complete: true, Success: true, Actions: 1,
			Reason: "recovery reached the target", CompletedAtMS: milliseconds(time.Since(started))}
		return nil
	}}
	environment := realtimeCURunEnvironment{episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
		return realtimeCURunEpisode{
			ready: func(context.Context) error {
				pageMu.Lock()
				started = time.Now()
				pageMu.Unlock()
				return nil
			},
			started:       func() time.Time { pageMu.Lock(); defer pageMu.Unlock(); return started },
			surface:       surface,
			captureScreen: func(context.Context) ([]byte, error) { return frame.Bytes(), nil },
			captureCamera: func(context.Context) ([]byte, error) { return frame.Bytes(), nil },
			result: func(context.Context) (PageResult, error) {
				pageMu.Lock()
				defer pageMu.Unlock()
				return page, nil
			},
		}, nil
	}}
	// The test authors a short text-driven episode. The public suite and its
	// audio/timing definitions remain unchanged; this is a runtime regression.
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	item.Task.ID = "failed-result-session"
	item.Task.CueAt, item.Task.Deadline, item.Task.MaxActions = 100*time.Millisecond, 3*time.Second, 3
	var transcript bench.Transcript
	outcome, err := runCase(t.Context(), environment, Options{
		Endpoint: endpoint, Model: "failed-result-session", Cell: ReferenceCell(),
		Observers: []string{failedResultObserverName}, FrameRate: 10, Timeout: 8 * time.Second,
		dependencies: &runDependencies{
			now: time.Now,
			playSamples: func(ctx context.Context, config bench.SessionConfig, _ []int16) (bench.Transcript, error) {
				// Leave room for the asynchronous consequence/policy lane and
				// later screen frames after the final protocol response closes.
				config.TrailingSilence, config.PostPlaybackQuiet = 100*time.Millisecond, 2*time.Second
				config.CaptureRuntimeEvidence = true
				config.Scheduled = []bench.ScheduledEvent{{AtMS: 100, Name: "durable-intent", Event: map[string]any{
					"type": "conversation.item.create", "item": map[string]any{
						"type": "message", "role": "user", "content": []map[string]any{{
							"type": "input_text", "text": "click the visible control and recover if the first click fails",
						}},
					},
				}}}
				var playErr error
				transcript, playErr = bench.PlaySamples(ctx, config, make([]int16, 24_000))
				return transcript, playErr
			},
		},
		evidenceOrigin: EvidenceRunOrigin{Kind: EvidenceOriginHermetic},
	}, item)
	if err != nil || !outcome.Completed || !outcome.Passed ||
		outcome.Metrics["task_success_rate"] != 1 || outcome.Metrics["invalid_action_count"] != 1 ||
		outcome.Metrics["action_count"] != 2 || outcome.Metrics["settlement_evidence_missing_count"] != 0 ||
		outcome.Metrics["session_timeout_count"] != 0 || transcript.Failure != "" ||
		transcript.OutstandingTools != 0 || transcript.OutstandingResponses != 0 {
		t.Fatalf("failed-result session did not recover and settle: outcome=%+v err=%v model=%d policy=%d", outcome, err, model.calls.Load(), policy.calls.Load())
	}
	if model.calls.Load() != 2 || policy.calls.Load() != 1 || observer.framesAfterDecision.Load() == 0 {
		t.Fatalf("failed result reached success policy or later cadence was not quiescent: model=%d policy=%d frames_after_decision=%d",
			model.calls.Load(), policy.calls.Load(), observer.framesAfterDecision.Load())
	}
	if !slices.Equal(transcript.NegotiatedObservers, []string{failedResultObserverName}) ||
		transcript.Runtime == nil || transcript.Runtime.Graph.ID != "realtime_computer_use" {
		t.Fatalf("session did not negotiate the production graph and selected observer: %+v", transcript)
	}
	var actions []ActionRecord
	if err := json.Unmarshal([]byte(outcome.Notes["actions"]), &actions); err != nil || len(actions) != 2 {
		t.Fatalf("evaluator action trace=%+v err=%v", actions, err)
	}
	var runtime binding.Runtime
	select {
	case runtime = <-mounted:
	default:
		t.Fatal("session did not mount its graph")
	}
	snapshot := runtime.Trajectory()
	// The production activation must apply the terminal and acknowledge it
	// across the connected graph, beyond the policy fixture choosing success.
	native := runtime.(*graphbinding.NativeRuntime)
	live := native.Live()
	acknowledged := false
	for _, edge := range native.Graph().Edges {
		if edge.From.Node == "activation" && edge.From.Port == "settlement_ack" &&
			edge.To.Node == "settlement" && edge.To.Port == "ack" {
			state := live.Edges[edge.ID]
			acknowledged = state.Enqueued == 1 && state.Dequeued == 1 && state.Occupancy == 0 && state.Dropped == 0
		}
	}
	if !acknowledged {
		t.Fatal("production activation did not acknowledge terminal settlement")
	}
	observer.mu.Lock()
	consequences := slices.Clone(observer.consequences)
	observer.mu.Unlock()
	if len(consequences) != 2 {
		t.Fatalf("canonical result consequences=%+v", consequences)
	}
	for index, action := range actions {
		result := failedResultSessionCanonicalItem(t, snapshot, consequences[index].CanonicalResultItemID)
		if len(result.CausalParentIDs) == 0 {
			t.Fatalf("canonical result lost its call parent: %+v", result)
		}
		call := failedResultSessionCanonicalItem(t, snapshot, result.CausalParentIDs[0])
		if result.Kind != trajectory.KindToolResult || result.ToolResult == nil ||
			result.ToolResult.CallID != action.CallID || result.ToolResult.Name != action.Name ||
			result.ToolResult.Error != action.Error || result.InvocationID == "" ||
			call.Kind != trajectory.KindToolCall || call.ToolCall == nil ||
			call.ToolCall.CallID != action.CallID || string(call.ToolCall.Arguments) != string(action.Arguments) ||
			call.InvocationID != result.InvocationID || consequences[index].CallID != action.CallID {
			t.Fatalf("wire/evaluator/canonical action identity diverged: action=%+v call=%+v result=%+v consequence=%+v", action, call, result, consequences[index])
		}
		if (index == 0 && (result.ToolResult.Error != failure || len(result.ToolResult.Output) != 0)) ||
			(index == 1 && (result.ToolResult.Error != "" || len(result.ToolResult.Output) == 0)) {
			t.Fatalf("tool failure semantics changed across the session: %+v", result)
		}
		linked := slices.ContainsFunc(snapshot.Items, func(item trajectory.Item) bool {
			return item.Kind == trajectory.KindObservation && slices.Contains(item.CausalParentIDs, result.ID)
		})
		if !linked {
			t.Fatalf("result %s has no canonical visual consequence", result.ID)
		}
		if !slices.ContainsFunc(transcript.Moments, func(moment bench.Moment) bool {
			return moment.Kind == bench.MomentToolResult && moment.CallID == action.CallID &&
				((index == 0 && moment.Text == "Error: "+failure) || (index == 1 && moment.Text == string(result.ToolResult.Output)))
		}) {
			t.Fatalf("client transcript omitted the exact result for %s", action.CallID)
		}
	}
}

func failedResultSessionCanonicalItem(t *testing.T, snapshot trajectory.Snapshot, id string) trajectory.Item {
	t.Helper()
	for _, item := range snapshot.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("canonical item %q missing", id)
	return trajectory.Item{}
}

func failedResultSessionEndpoint(t *testing.T, model *failedResultSessionModel, observer *failedResultSessionObserver, policy *failedResultSessionPolicy) (string, <-chan binding.Runtime) {
	t.Helper()
	artifact := func(name string) inspect.ArtifactIdentity {
		return inspect.ArtifactIdentity{ID: "artifact://test/failed-result-session/" + name, Revision: "v1", Digest: "sha256:" + strings.Repeat("8", 64)}
	}
	config, err := graphs.RealtimeComputerUseLaunchConfig(realtimecubinding.PluginConfig{
		RuntimeArtifact: artifact("runtime"),
		Target:          computeruse.Target{Name: "benchmark-browser", Sources: []string{"screen"}, Width: 1280, Height: 720},
		Model: realtimecubinding.ModelPlugin{
			Reference: "go://test/failed-result-session/model", Artifact: artifact("model"), Descriptor: model.Descriptor(),
			Factory: func(context.Context, binding.Options) (continuation.Provider, error) { return model, nil },
		},
		SettlementPolicy: realtimecubinding.PolicyPlugin{
			Reference: realtimecubinding.SettlementPolicyReference, Artifact: artifact("policy"), Descriptor: policy.Descriptor(),
			Factory: func(context.Context, binding.Options) (policyelements.SemanticDecider, error) { return policy, nil },
		},
		Observer: realtimecubinding.ObserverPlugin{
			Reference: "go://test/failed-result-session/observer", Name: failedResultObserverName, Artifact: artifact("observer"),
			Sources: []string{"microphone", "screen", "camera"},
			ResourceFactory: func(_ context.Context, _ binding.Options, resources realtimecubinding.ObserverResources) (realtimecubinding.Observer, error) {
				observer.retainer = resources.Retainer
				return observer, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	launched, err := graphlaunch.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	mounted := make(chan binding.Runtime, 1)
	bundle, err := server.NewBundle(server.BundleConfig{
		ProfileName: "openrealtime.server.failed-result-session", ProfileRevision: 1,
		Provider:         &failedResultSessionBinding{NativeBinding: launched.Binding, mounted: mounted},
		ProviderArtifact: artifact("provider"), GatewayArtifact: artifact("gateway"),
		Gateway: gateway.Config{Model: "failed-result-session", ValidateWire: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	realm, err := bundle.Mount(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close session realm: %v", err)
		}
	})
	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	return "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/realtime", mounted
}

type failedResultSessionBinding struct {
	*graphbinding.NativeBinding
	mounted chan<- binding.Runtime
}

func (provider *failedResultSessionBinding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	runtime, err := provider.NativeBinding.Start(ctx, options)
	if err == nil {
		provider.mounted <- runtime
	}
	return runtime, err
}

type failedResultSessionModel struct{ calls atomic.Int32 }

func (*failedResultSessionModel) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{Provider: "test", Model: "failed-result-session", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent}
}

func (model *failedResultSessionModel) Continue(_ context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	index := model.calls.Add(1)
	if index > 2 {
		return continuation.Completion{}, errors.New("settled intent invoked another model turn")
	}
	call := trajectory.ToolCall{CallID: request.InvocationID + ":effect", Name: computeruse.ClickNormalized,
		Arguments: json.RawMessage(fmt.Sprintf(`{"source":"screen","x":%d,"y":500}`, 400+index*50))}
	return continuation.Completion{StopReason: "tool_call"}, emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call})
}

const failedResultObserverName = "failed-result-session-observer"

type failedResultSessionObserver struct {
	retainer            perception.Retainer
	frames              atomic.Uint64
	decisionMade        atomic.Bool
	framesAfterDecision atomic.Uint64
	mu                  sync.Mutex
	consequences        []realtimecubinding.VisualConsequence
}

func (*failedResultSessionObserver) Audio(context.Context, perception.Frame) ([]perception.Observation, error) {
	return nil, nil
}

func (observer *failedResultSessionObserver) Video(_ context.Context, frame perception.Frame) ([]perception.Observation, error) {
	revision := observer.frames.Add(1)
	if observer.decisionMade.Load() {
		observer.framesAfterDecision.Add(1)
	}
	media, err := observer.retainer.Retain(trajectory.MediaRef{Source: frame.Source, MIMEType: frame.MIMEType,
		Width: frame.Width, Height: frame.Height, CapturedNS: frame.CapturedNS}, frame.Image)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("current screen observation %d", revision)
	return []perception.Observation{{Text: text, StableText: text, Observer: failedResultObserverName,
		Source: frame.Source, Authority: trajectory.AuthorityObserver, Revision: revision, Final: true,
		Media: []trajectory.MediaRef{media}}}, nil
}

func (observer *failedResultSessionObserver) Consequence(_ context.Context, consequence realtimecubinding.VisualConsequence) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.consequences = append(observer.consequences, consequence)
	return nil
}

func (*failedResultSessionObserver) Close() error { return nil }

type failedResultSessionPolicy struct {
	calls    atomic.Int32
	observer *failedResultSessionObserver
}

func (*failedResultSessionPolicy) Name() string { return "failed-result-session-policy" }
func (*failedResultSessionPolicy) Descriptor() policyelements.SemanticDeciderDescriptor {
	return policyelements.SemanticDeciderDescriptor{Provider: "test", Model: "settlement", Protocol: "enum-v1",
		Revision: "v1", ConfigurationDigest: "sha256:" + strings.Repeat("9", 64), Vision: true, DecisionTimeoutMS: 1000}
}
func (policy *failedResultSessionPolicy) Decide(_ context.Context, request interaction.Decision) (interaction.Outcome, error) {
	policy.calls.Add(1)
	choice := string(policyelements.IntentDispositionSucceeded)
	index := slices.Index(request.Options, choice)
	if index < 0 {
		return interaction.Outcome{}, errors.New("settlement omitted succeeded option")
	}
	policy.observer.decisionMade.Store(true)
	return interaction.Outcome{Index: index, Option: choice}, nil
}
