package gateway_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/gateway"
)

// modelHoldsTheFloor is a binding whose model decides when a turn ended, which
// is what duplex declares and what the engine cannot override.
type modelHoldsTheFloor struct{ binding.Binding }

func (bind modelHoldsTheFloor) Capabilities() binding.Capabilities {
	capabilities := bind.Binding.Capabilities()
	capabilities.ManualTurns = false
	return capabilities
}

// Turning turn detection off is a client taking the floor, and a floor can
// only be handed over by whoever holds it.
//
// Accepting the declaration against a binding whose model owns the floor would
// leave the client waiting to be asked for a response while the model answered
// on its own schedule - two sides each believing the other was going to wait.
func TestTakingAFloorTheBindingDoesNotHoldIsRefused(t *testing.T) {
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hi"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{}, Voice: "test-voice",
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	server, err := gateway.New(gateway.Config{
		Binding: modelHoldsTheFloor{bind}, Model: "openrealtime-test", ValidateWire: true,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	http := httptest.NewServer(server.Handler())
	t.Cleanup(http.Close)

	client := dial(t, http)
	client.await("session.created", 5*time.Second)
	client.configureManualTurns()

	// The rest of the update still applies, and the session reports the
	// detection actually in force rather than the one that was refused.
	updated := client.await("session.updated", 5*time.Second)
	session, _ := updated["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	if detection, _ := input["turn_detection"].(map[string]any); detection == nil {
		t.Fatal("a refused handover must leave the deployment's own detector in force")
	}

	event, ok := client.awaitOptional("error", 5*time.Second)
	if !ok {
		t.Fatal("a floor that cannot be handed over must be refused, not silently kept")
	}
	failure, _ := event["error"].(map[string]any)
	if param, _ := failure["param"].(string); param != "session.audio.input.turn_detection" {
		t.Fatalf("the refusal must name the field, got %v", failure)
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "owns the floor") {
		t.Fatalf("the refusal must say why, got %q", message)
	}
}

// The binding that does hold its floor still hands it over.
func TestABindingThatHoldsItsFloorStillHandsItOver(t *testing.T) {
	server := startServer(t, fast(), slow(), "hi")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configureManualTurns()
	updated := client.await("session.updated", 5*time.Second)
	session, _ := updated["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	if detection, present := input["turn_detection"]; !present || detection != nil {
		t.Fatalf("the client took the floor, so nothing should be detecting turns: %#v", detection)
	}
	if event, ok := client.awaitOptional("error", 2*time.Second); ok {
		failure, _ := event["error"].(map[string]any)
		t.Fatalf("a handover this binding can make must not be refused: %v", failure)
	}
}
