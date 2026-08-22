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
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/perception"
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
	// Narration is what the video observer reports for any frame.
	Narration string
	// FastSpendsBudgetThinking makes the fast provider return no assistant
	// text, having written its deliberation into the content field and hit
	// the output limit. It is what a thinking model on a short budget does,
	// and it is the case a turn has to report rather than fall silent on.
	FastSpendsBudgetThinking bool
	// AllowedOrigins are the web origins the WebRTC adapter will answer.
	AllowedOrigins []string
}

// Stack is a running server and its adapter.
type Stack struct {
	// ProtocolURL is the WebSocket endpoint, ws://host:port/v1/realtime.
	ProtocolURL string
	// AdapterURL is where a WebRTC client POSTs an SDP offer.
	AdapterURL string
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
	}, turns: fastTurns, spentBudgetThinking: config.FastSpendsBudgetThinking}

	slowTurns := [][]continuation.Event{
		{{Kind: continuation.EventAssistantDelta, Text: "The notes say the deadline moved to Friday."}},
	}
	if strings.TrimSpace(config.ToolName) != "" {
		arguments := config.ToolArguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		slowTurns = append([][]continuation.Event{
			{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				Name: config.ToolName, Arguments: []byte(arguments),
			}}},
		}, slowTurns...)
	}
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
	server, err := gateway.New(gateway.Config{Binding: bind, ValidateWire: true})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	protocol := httptest.NewServer(server.Handler())
	t.Cleanup(protocol.Close)
	protocolURL := "ws" + strings.TrimPrefix(protocol.URL, "http") + "/v1/realtime"

	adapter, err := webrtc.New(webrtc.Config{
		Endpoint: protocolURL, AllowedOrigins: config.AllowedOrigins,
	})
	if err != nil {
		t.Fatalf("webrtc adapter: %v", err)
	}
	adapterServer := httptest.NewServer(adapter.Handler())
	t.Cleanup(adapterServer.Close)

	return Stack{ProtocolURL: protocolURL, AdapterURL: adapterServer.URL + "/v1/realtime"}
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
	// spentBudgetThinking reports the completion a provider makes when it
	// wrote its deliberation into the content field and the output limit is
	// what stopped it.
	spentBudgetThinking bool
}

func (provider *scripted) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scripted) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	var events []continuation.Event
	if len(provider.turns) > 0 {
		events = provider.turns[index%len(provider.turns)]
	}
	provider.mu.Unlock()
	for _, event := range events {
		if event.ToolCall != nil {
			// A fresh identifier per invocation. A cycling script that reused
			// one would be a duplicate call, which the trajectory refuses -
			// correctly, and it would look like a client defect.
			call := *event.ToolCall
			call.CallID = "call_scripted_" + strconv.Itoa(index)
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
