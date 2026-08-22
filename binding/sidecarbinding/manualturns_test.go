package sidecarbinding_test

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/duplex"
	"github.com/bojieli/OpenRealtime/binding/omni"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// A client that took the floor does not get turns created for it.
//
// The engine holds the floor on omni and ends turns on silence, which is the
// binding's whole point - until a client declares it will end its own. Then
// the endpoint is still observed and still reported, and it is simply not
// acted on. A client that declared its own turns and got a server-created one
// alongside them hears the agent answer twice.
func TestAClientThatTookTheFloorGetsNoTurnsItDidNotAskFor(t *testing.T) {
	binary, received := buildFakeSidecar(t)
	bind, err := omni.New(omni.Config{
		Sidecar: sidecar.Config{
			Command: []string{binary}, Environment: []string{"FAKE_SIDECAR_LOG=" + received},
		},
		Slow: &scriptedSlow{},
	})
	if err != nil {
		t.Fatalf("new omni: %v", err)
	}
	if !bind.Capabilities().ManualTurns {
		t.Fatal("the engine holds this floor, so it is the engine's to hand over")
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test", Settings: binding.Settings{ManualTurns: true},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runtime.Close(context.Background(), nil)

	// Speech then silence: exactly the shape that ends a turn when the engine
	// is deciding.
	loud := make([]byte, 4800)
	for index := 0; index < len(loud); index += 2 {
		loud[index+1] = 0x40
	}
	quiet := make([]byte, 4800)
	ctx := context.Background()
	for index := 0; index < 12; index++ {
		payload := loud
		if index >= 3 {
			payload = quiet
		}
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000, PCM16LE: payload,
		}); err != nil {
			t.Fatalf("audio: %v", err)
		}
	}

	// The endpoint still happened - it is observed and reported, which is what
	// makes this a floor decision rather than deafness.
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for _, event := range sink.activity {
			if event.Stopped {
				return true
			}
		}
		return false
	}, "the engine must still hear the endpoint, it simply must not act on it")

	for _, message := range sidecarReceived(t, received) {
		if message["type"] == "respond" {
			t.Fatal("a client that declared its own turns must not be answered before it asks")
		}
	}

	// And when the client does ask, it is answered.
	if err := runtime.CreateResponse(ctx); err != nil {
		t.Fatalf("create response: %v", err)
	}
	waitFor(t, func() bool {
		for _, message := range sidecarReceived(t, received) {
			if message["type"] == "respond" {
				return true
			}
		}
		return false
	}, "the client asked for a response and must get one")
}

// A full-duplex model owns its floor, so there is nothing for the engine to
// hand over. Saying so is the binding refusing to promise on the model's
// behalf; the gateway turns that into an error naming the field.
func TestAModelThatOwnsItsFloorCannotGiveItAway(t *testing.T) {
	binary, received := buildFakeSidecar(t)
	bind, err := duplex.New(duplex.Config{
		Sidecar: sidecar.Config{
			Command: []string{binary}, Environment: []string{"FAKE_SIDECAR_LOG=" + received},
		},
		Slow: &scriptedSlow{},
	})
	if err != nil {
		t.Fatalf("new duplex: %v", err)
	}
	if bind.Capabilities().ManualTurns {
		t.Fatal("a binding cannot hand over a floor its model owns")
	}
	if _ = received; !bind.Capabilities().Voice.Selectable {
		t.Fatal("a model with more than one voice takes the session's choice")
	}
}

// The client's endpointing parameters reach the gate that endpoints.
//
// The engine holds this floor on omni, which is exactly what makes the
// client's turn-detection settings honourable here: there is a gate, it is
// ours, and it decides when the turn ended. It was being built from the
// deployment's configuration while the gateway reported the client's own
// values back, so a client that asked to wait longer before its turn was
// called over was told it would and was cut off on the deployment's schedule.
func TestTheClientsEndpointingReachesTheGate(t *testing.T) {
	binary, received := buildFakeSidecar(t)
	bind, err := omni.New(omni.Config{
		Sidecar: sidecar.Config{
			Command: []string{binary}, Environment: []string{"FAKE_SIDECAR_LOG=" + received},
		},
		Slow: &scriptedSlow{},
		// The deployment ends a turn after a short silence.
		Gate: perception.GateConfig{Threshold: 0.5, PrefixPaddingMS: 300, SilenceDurationMS: 200},
	})
	if err != nil {
		t.Fatalf("new omni: %v", err)
	}
	sink := &collectingSink{}
	// The client asks to be given far longer.
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test",
		Settings: binding.Settings{Gate: perception.GateConfig{
			Threshold: 0.5, PrefixPaddingMS: 300, SilenceDurationMS: 60_000,
		}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runtime.Close(context.Background(), nil)

	loud := make([]byte, 4800)
	for index := 0; index < len(loud); index += 2 {
		loud[index+1] = 0x40
	}
	quiet := make([]byte, 4800)
	ctx := context.Background()
	for index := 0; index < 24; index++ {
		payload := loud
		if index >= 3 {
			payload = quiet
		}
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000, PCM16LE: payload,
		}); err != nil {
			t.Fatalf("audio: %v", err)
		}
	}
	// Two full seconds of silence: ten times the deployment's window, a
	// thirtieth of the client's.
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for _, event := range sink.activity {
			if event.Started {
				return true
			}
		}
		return false
	}, "the gate must hear the speech before it can be asked not to end it")

	sink.mu.Lock()
	for _, event := range sink.activity {
		if event.Stopped {
			sink.mu.Unlock()
			t.Fatal("the turn ended on the deployment's window, not the one the client asked for")
		}
	}
	sink.mu.Unlock()
}
