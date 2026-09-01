// Package testserver assembles a complete OpenRealtime server for tests that
// need a real one.
//
// It is the real gateway, the real cascade binding, and the real WebRTC
// adapter, with scripted providers standing in for the models. That
// combination is what several kinds of test need and none of them should
// reimplement: a browser cannot be pointed at a mock, and a client that only
// ever met a mock proves nothing about the server.
//
// Scripted rather than stubbed to nothing: the fast provider speaks, the slow
// provider calls a tool, and the synthesiser produces real audio, because a
// client's job is to handle all three and a test that skipped any of them
// would pass on a client that could not.
package testserver

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	effectauthority "github.com/bojieli/OpenRealtime/authority"
	legacybinding "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	compat "github.com/bojieli/OpenRealtime/elements/compatbinding"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphschema "github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/management"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/bojieli/OpenRealtime/transport/webrtc"
)

// Config describes the conversation the scripted providers will hold.
type Config struct {
	// Transcript is what the recogniser reports for any speech it is given.
	Transcript string
	// ToolName and ToolArguments are the call the slow provider issues. An
	// empty name means the slow provider only speaks.
	ToolName      string
	ToolArguments string
	// ToolCalls is a sequence of calls, one per slow turn, for a client whose
	// point is that it runs several different kinds of tool. A test that needs
	// one call uses ToolName; a test that needs to see a file read, a browser
	// clicked, and an artifact rendered needs them to arrive separately,
	// because a client that batched them would be a client the protocol does
	// not describe.
	ToolCalls []ScriptedCall
	// Narration is what the video observer reports for any frame.
	Narration string
	// FastSpendsBudgetThinking makes the fast provider return no assistant
	// text, having written its deliberation into the content field and hit
	// the output limit. It is what a thinking model on a short budget does,
	// and it is the case a turn has to report rather than fall silent on.
	FastSpendsBudgetThinking bool
	// AllowedOrigins are the web origins the WebRTC adapter will answer.
	AllowedOrigins []string
	// GraphInspection mounts the real compatibility graph adapter so a client
	// can negotiate and consume the canonical management API. Tests that only
	// need protocol behavior leave it off and retain the smallest stack.
	GraphInspection bool
	// TraceRecording opts the mounted compatibility graph into the bounded,
	// payload-free recorder. It is separate from live inspection so tests can
	// measure and assert the zero-recorder profile explicitly.
	TraceRecording bool
	// ClientEffectIssuer enables negotiated server-issued host effect receipts.
	// It is deliberately an interface seam so integration fixtures exercise
	// the same issuer selected by a real deployment.
	ClientEffectIssuer effectauthority.EffectReceiptIssuerProvider
	// ManagementAuthorizer explicitly enables the static-catalog and authoring
	// management profile on the same server as realtime. Session inspection
	// keeps its independently negotiated gateway capability. Tests issue their
	// own operator grants through this seam; no token is built into the server.
	ManagementAuthorizer management.Authorizer
	// SourceReading optionally adds the separately authorized, rooted authoring
	// lookup boundary. It never follows from ManagementAuthorizer alone.
	SourceReading management.SourceReading
	// SourcePublication optionally adds the separately authorized authoring
	// mutation boundary. It never follows from ManagementAuthorizer alone.
	SourcePublication management.SourcePublication
}

// ScriptedCall is one tool call the slow provider issues, on its own turn.
type ScriptedCall struct {
	Name      string
	Arguments string
}

// Stack is a running server and its adapter.
type Stack struct {
	// ProtocolURL is the WebSocket endpoint, ws://host:port/v1/realtime.
	ProtocolURL string
	// AdapterURL is where a WebRTC client POSTs an SDP offer.
	AdapterURL string
	// Static management identities are non-secret evidence used by clients to
	// request and rebind the exact graph, values schema, and descriptor.
	Graph           ir.Graph
	ValuesSchema    graphschema.Bundle
	ElementIdentity element.Identity
	AuthoringSource string
}

// Start brings up a server and registers its shutdown with the test.
func Start(t testing.TB, config Config) Stack {
	t.Helper()
	if strings.TrimSpace(config.Transcript) == "" {
		config.Transcript = "please read the notes file"
	}
	if strings.TrimSpace(config.Narration) == "" {
		config.Narration = "The screen shows an editor with a file open and a terminal below it."
	}

	fastTurns := [][]continuation.Event{
		{{Kind: continuation.EventAssistantDelta, Text: "Let me look."}},
		{{Kind: continuation.EventAssistantDelta, Text: "Here is what I found."}},
	}
	if config.FastSpendsBudgetThinking {
		fastTurns = [][]continuation.Event{{}}
	}
	fast := &scripted{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}, turns: fastTurns, handsOn: true, spentBudgetThinking: config.FastSpendsBudgetThinking}

	slowTurns := [][]continuation.Event{
		{{Kind: continuation.EventAssistantDelta, Text: "The notes say the deadline moved to Friday."}},
	}
	scriptedCalls := config.ToolCalls
	if strings.TrimSpace(config.ToolName) != "" {
		scriptedCalls = append([]ScriptedCall{
			{Name: config.ToolName, Arguments: config.ToolArguments},
		}, scriptedCalls...)
	}
	var callTurns [][]continuation.Event
	for _, call := range scriptedCalls {
		arguments := call.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		callTurns = append(callTurns, []continuation.Event{
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				Name: call.Name, Arguments: []byte(arguments),
			}},
		})
	}
	slowTurns = append(callTurns, slowTurns...)
	slow := &scripted{descriptor: continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}, turns: slowTurns}

	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return recogniser{text: config.Transcript}, nil
		},
		Fast: fast, Slow: slow, Speech: synthesiser{},
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: perception.StaticNarrator{Text: config.Narration},
			Cadence:  2 * time.Second,
		})},
	})
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	var served legacybinding.Binding = bind
	var graphServed *graphbinding.Binding
	if config.GraphInspection {
		if config.TraceRecording {
			artifact := inspect.ArtifactIdentity{
				ID:       "go://openrealtime/internal-testserver/compat-binding",
				Revision: "scripted-v1", Digest: "sha256:" + strings.Repeat("d", 64),
			}
			graphServed, err = graphbinding.NewWithConfig(bind, graphbinding.Config{
				ImplementationArtifact: &artifact,
				TraceRecording: &graphbinding.TraceRecordingConfig{
					MaxRetainedBytes: 2 << 20, CaptureInterval: time.Millisecond,
				},
			})
		} else {
			graphServed, err = graphbinding.New(bind)
		}
		if err != nil {
			t.Fatalf("mount test binding through Graph IR: %v", err)
		}
		served = graphServed
	}
	if config.TraceRecording && !config.GraphInspection {
		t.Fatal("testserver trace recording requires graph inspection")
	}
	if config.ManagementAuthorizer != nil && graphServed == nil {
		t.Fatal("testserver static/authoring management requires graph inspection")
	}
	if (config.SourceReading != nil || config.SourcePublication != nil) && config.ManagementAuthorizer == nil {
		t.Fatal("testserver rooted source access requires an explicit management authorizer")
	}
	server, err := gateway.New(gateway.Config{
		Binding: served, ValidateWire: true, ClientEffectIssuer: config.ClientEffectIssuer,
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	router := http.NewServeMux()
	router.Handle("GET /v1/realtime", server.RealtimeHandler())
	router.Handle("GET /healthz", server.HealthHandler())
	router.Handle("GET /metrics", server.MetricsHandler())
	router.Handle(management.APIPrefix+"/", server.ManagementHandler())
	var protocolHandler http.Handler = router
	var staticGraph ir.Graph
	var valuesSchema graphschema.Bundle
	var elementIdentity element.Identity
	authoringSource := ""
	if config.ManagementAuthorizer != nil {
		staticGraph = graphServed.Graph()
		elementCatalog := resolve.NewCatalog()
		descriptor := compat.Descriptor()
		if err := elementCatalog.Register(descriptor); err != nil {
			t.Fatalf("register test management element: %v", err)
		}
		plugins := plugin.NewCatalog()
		staticCatalog, err := management.NewCatalog(elementCatalog, plugins)
		if err != nil {
			t.Fatalf("create test management catalog: %v", err)
		}
		valuesSchema, err = graphschema.Generate(context.Background(), staticGraph, elementCatalog,
			graphschema.Options{})
		if err != nil {
			t.Fatalf("generate test values schema: %v", err)
		}
		if err := staticCatalog.RegisterGraph(staticGraph, valuesSchema); err != nil {
			t.Fatalf("register test management graph: %v", err)
		}
		authoring, err := management.NewAuthoringEngine(management.AuthoringOptions{Catalog: elementCatalog})
		if err != nil {
			t.Fatalf("create test authoring engine: %v", err)
		}
		operatorAPI, err := managementserver.MountOperatorAPI(context.Background(), protocolHandler,
			managementserver.OperatorAPIConfig{
				Authorizer: config.ManagementAuthorizer, StaticCatalog: staticCatalog, Authoring: authoring,
				SourceReading:     config.SourceReading,
				SourcePublication: config.SourcePublication,
			})
		if err != nil {
			t.Fatalf("mount test static/authoring management API: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := operatorAPI.Close(ctx); err != nil {
				t.Errorf("close test static/authoring management API: %v", err)
			}
		})
		protocolHandler = operatorAPI.Handler()
		elementIdentity = staticGraph.Nodes[0].Element
		authoringSource = compatibilityAuthoringSource("browser_authoring")
	}
	protocol := httptest.NewServer(protocolHandler)
	t.Cleanup(func() {
		protocol.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close test gateway: %v", err)
		}
	})
	protocolURL := "ws" + strings.TrimPrefix(protocol.URL, "http") + "/v1/realtime"

	adapter, err := webrtc.New(webrtc.Config{
		Endpoint: protocolURL, AllowedOrigins: config.AllowedOrigins,
	})
	if err != nil {
		t.Fatalf("webrtc adapter: %v", err)
	}
	adapterServer := httptest.NewServer(adapter.Handler())
	t.Cleanup(adapterServer.Close)

	return Stack{
		ProtocolURL: protocolURL, AdapterURL: adapterServer.URL + "/v1/realtime/calls",
		Graph: staticGraph, ValuesSchema: valuesSchema, ElementIdentity: elementIdentity,
		AuthoringSource: authoringSource,
	}
}

func compatibilityAuthoringSource(name string) string {
	descriptor := compat.Descriptor()
	statements := []syntax.Statement{{Node: &syntax.Node{Element: descriptor.Name, Name: "runtime"}}}
	for _, port := range descriptor.Ports {
		direction := syntax.BoundaryInput
		if port.Direction == element.Output {
			direction = syntax.BoundaryOutput
		}
		statements = append(statements, syntax.Statement{Boundary: &syntax.Boundary{
			Direction: direction, Name: port.Name,
			Endpoint: syntax.Endpoint{Node: "runtime", Port: port.Name},
		}})
	}
	return syntax.Format(syntax.File{Graph: syntax.Graph{Name: name, Statements: statements}})
}

// --- the scripted stand-ins -------------------------------------------------

type recogniser struct{ text string }

func (recogniser) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "scripted", Version: "1"}
}
func (recogniser) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (r recogniser) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 1, StableText: r.text, Final: true}, nil
}
func (recogniser) Close() error { return nil }

type scripted struct {
	descriptor continuation.Descriptor
	mu         sync.Mutex
	turns      [][]continuation.Event
	calls      int
	// handsOn makes this stand-in behave like a voice rather than a script: it
	// leaves the turn open while the reasoner has produced nothing, and
	// declares it finished once the reasoner has answered. Which turn that falls on depends on the
	// transport and on what else opened a turn first, so deciding it from the
	// conversation is the only way a fixed script cannot get wrong.
	handsOn bool
	// spentBudgetThinking reports the completion a provider makes when it
	// wrote its deliberation into the content field and the output limit is
	// what stopped it.
	spentBudgetThinking bool
}

// invocationsByPhase counts how many continuations this phase has already run
// in this conversation.
func invocationsByPhase(snapshot trajectory.Snapshot, phase trajectory.Phase) int {
	seen := make(map[string]struct{})
	for _, item := range snapshot.Items {
		if item.Producer.Phase == phase && item.InvocationID != "" {
			seen[item.InvocationID] = struct{}{}
		}
	}
	return len(seen)
}

// awaitingReasoner reports that nothing silent has been written yet, which is
// what "the reasoner has not answered" looks like in the log.
func awaitingReasoner(snapshot trajectory.Snapshot) bool {
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindAssistant && continuation.ProducedSilently(item) {
			return false
		}
	}
	return true
}

func (provider *scripted) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scripted) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	// Which turn this is comes from the conversation, not from a counter on
	// the provider. One server holds many sessions, and a counter shared
	// between them starts the second session in the middle of the first
	// session's script - so the scenario the test set up never happens.
	//
	// The script also runs out rather than cycling. A conversation is longer
	// than any script, and wrapping round replays the first turn, which for a
	// provider whose first turn calls a tool means calling it again.
	index := invocationsByPhase(request.Trajectory, provider.descriptor.Phase)
	var events []continuation.Event
	if len(provider.turns) > 0 {
		events = provider.turns[min(index, len(provider.turns)-1)]
	}
	// The voice declares a turn finished once the reasoner has answered; until
	// then it leaves the marker off, which is what asks the reasoner to work.
	if provider.handsOn && !awaitingReasoner(request.Trajectory) {
		events = append(slices.Clone(events), continuation.Event{
			Kind: continuation.EventAssistantDelta, Text: continuation.CompletionMarker,
		})
	}
	for _, event := range events {
		if event.ToolCall != nil {
			// A fresh identifier per invocation. A cycling script that reused
			// one would be a duplicate call, which the trajectory refuses -
			// correctly, and it would look like a client defect.
			provider.mu.Lock()
			provider.calls++
			serial := provider.calls
			provider.mu.Unlock()
			call := *event.ToolCall
			call.CallID = "call_scripted_" + strconv.Itoa(serial)
			event.ToolCall = &call
		}
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	if provider.spentBudgetThinking {
		return continuation.Completion{StopReason: "length", ReasoningInContent: true}, nil
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

type synthesiser struct{}

func (synthesiser) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "tone", Version: "1"}
}
func (synthesiser) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("tests use the streaming path")
}

// Stream emits half a second of tone. Real audio rather than silence, because
// a client that decodes and schedules it is doing the thing under test and a
// buffer of zeros would let a broken one pass.
func (synthesiser) Stream(_ context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	const samples = 12000
	pcm := make([]byte, samples*2)
	for index := range samples {
		value := int16(6000 * math.Sin(2*math.Pi*440*float64(index)/24000))
		pcm[index*2] = byte(value)
		pcm[index*2+1] = byte(value >> 8)
	}
	return emit(v1.SpeechChunk{
		ChunkID: "chunk", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: pcm, Final: true,
	})
}
