package gateway_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
)

type textTimestampBinding struct {
	binding.Binding
	inputs chan<- binding.TextInput
}

func (bind textTimestampBinding) Start(
	ctx context.Context, options binding.Options,
) (binding.Runtime, error) {
	runtime, err := bind.Binding.Start(ctx, options)
	if err != nil {
		return nil, err
	}
	return &textTimestampRuntime{Runtime: runtime, inputs: bind.inputs}, nil
}

type textTimestampRuntime struct {
	binding.Runtime
	inputs chan<- binding.TextInput
}

func (runtime *textTimestampRuntime) Text(ctx context.Context, input binding.TextInput) error {
	runtime.inputs <- input
	return runtime.Runtime.Text(ctx, input)
}

func TestGatewayStampsTypedTextInAbsoluteDispatchClock(t *testing.T) {
	legacyBind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "unused"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{}, Voice: "test-voice", FastMaxTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphbinding.New(legacyBind)
	if err != nil {
		t.Fatal(err)
	}
	inputs := make(chan binding.TextInput, 1)
	gatewayServer, err := gateway.New(gateway.Config{
		Binding: textTimestampBinding{Binding: mounted, inputs: inputs},
		Model:   "openrealtime-test", ValidateWire: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(testGatewayHandler(gatewayServer))
	t.Cleanup(func() {
		httpServer.Close()
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := gatewayServer.Close(closeCtx); err != nil {
			t.Errorf("close gateway: %v", err)
		}
	})
	client := dial(t, httpServer)
	client.await("session.created", 5*time.Second)
	before := uint64(time.Now().UnixNano())
	client.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"id": "typed-source-time", "type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "click continue"}},
		},
	})
	client.await("conversation.item.created", 5*time.Second)
	select {
	case input := <-inputs:
		after := uint64(time.Now().UnixNano())
		if input.OccurredNS < before || input.OccurredNS > after || input.OccurredNS < 1_000_000_000_000_000_000 {
			t.Fatalf("typed input dispatch time = %d, want Unix nanoseconds in [%d, %d]", input.OccurredNS, before, after)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("typed input did not reach binding")
	}
}

// A computer-use agent wants the function calls and the reasoning, not a voice.
// Refusing text output made this server unusable for exactly the clients the
// extension exists for - and synthesising an answer nobody listens to spends a
// GPU on nothing.
func TestATextSessionAnswersInTextAndSynthesisesNothing(t *testing.T) {
	server := startServerWithSpeech(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."}}),
		staticASR{text: "what is my balance"},
		// A synthesiser that fails if it is called at all: a text turn must
		// never reach it.
		refusingSpeech{},
	)
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configureText()
	client.await("session.updated", 5*time.Second)

	client.speak()
	client.await("input_audio_buffer.speech_stopped", 5*time.Second)
	created := client.await("response.created", 10*time.Second)
	response, _ := created["response"].(map[string]any)
	modalities, _ := response["output_modalities"].([]any)
	if len(modalities) != 1 || modalities[0] != "text" {
		t.Fatalf("a response must declare the modality it produced, got %v", modalities)
	}

	part := client.await("response.content_part.added", 5*time.Second)
	if opened, _ := part["part"].(map[string]any); opened["type"] != "text" {
		t.Fatalf("a text turn opens a text content part, got %v", opened)
	}
	delta := client.await("response.output_text.delta", 5*time.Second)
	if delta["delta"] != "Forty dollars." {
		t.Fatalf("unexpected text delta %v", delta["delta"])
	}
	done := client.await("response.output_text.done", 5*time.Second)
	if done["text"] != "Forty dollars." {
		t.Fatalf("unexpected final text %v", done["text"])
	}
	client.await("response.done", 5*time.Second)

	// Nothing about audio may appear on a turn that produced none.
	if seen := client.seen(); contains(seen, "response.output_audio.delta") ||
		contains(seen, "response.output_audio.done") {
		t.Fatalf("a text turn announced audio it never sent: %s", seen)
	}
}

// And the modality is a choice of one, not a request for less of the same.
func TestOutputModalitiesMustNameExactlyOneKnownModality(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)

	for _, modalities := range [][]string{{}, {"audio", "text"}, {"video"}} {
		client.send(map[string]any{
			"type": "session.update",
			"session": map[string]any{
				"type": "realtime", "output_modalities": modalities,
			},
		})
		client.await("error", 5*time.Second)
	}
}
