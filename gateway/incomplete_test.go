package gateway_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

// A turn that produces nothing because it ran out of room has to say so.
//
// This is the failure a safety net creates. The fast provider is a thinking
// model with a short budget: every token it produced went into a reasoning
// block that never closed, the runtime correctly declined to read the
// deliberation aloud, and what reached the client was nothing at all - a
// session that opened, said nothing, and closed. Silence is what a correct
// runtime with nothing to add also looks like, so without this the two are
// indistinguishable and the client waits for a turn that is never coming.
func TestATurnCutShortReportsThatItWasIncomplete(t *testing.T) {
	fastProvider := fast([]continuation.Event{}).stopping(continuation.Completion{
		StopReason: "length", ReasoningInContent: true,
	})
	server := startServer(t, fastProvider, slow([]continuation.Event{}), "what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)
	client.speak()

	// The response is opened for the sole purpose of closing it: announcing
	// and completing in the same breath is still the whole story.
	created := client.await("response.created", 10*time.Second)
	response, _ := created["response"].(map[string]any)
	responseID, _ := response["id"].(string)
	if responseID == "" {
		t.Fatal("the announcement must carry the response it announces")
	}

	done := client.await("response.done", 10*time.Second)
	finished, _ := done["response"].(map[string]any)
	if got, _ := finished["id"].(string); got != responseID {
		t.Fatalf("the turn that was announced is the turn that ended: %q vs %q", got, responseID)
	}
	if status, _ := finished["status"].(string); status != "incomplete" {
		t.Fatalf("a truncated turn is incomplete, not %q", status)
	}
	details, _ := finished["status_details"].(map[string]any)
	if kind, _ := details["type"].(string); kind != "incomplete" {
		t.Fatalf("status_details must agree with the status, got %#v", details)
	}
	if reason, _ := details["reason"].(string); reason != "max_output_tokens" {
		t.Fatalf("the reason must be the protocol's own vocabulary, got %q", reason)
	}
}

// The converse, and the reason this cannot simply announce every turn: a
// rollout that had nothing to add is the runtime working. Opening a response
// to report that nothing happened would make every deferred batch look like a
// turn, and a client counting responses would count wrong.
func TestATurnWithNothingToAddAnnouncesNothing(t *testing.T) {
	server := startServer(t, fast([]continuation.Event{}), slow([]continuation.Event{}), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)
	client.speak()
	// The turn has to actually happen for its silence to mean anything: a
	// test that timed out before the rollout ran would pass for the wrong
	// reason.
	client.await("input_audio_buffer.speech_stopped", 5*time.Second)

	if event, ok := client.awaitOptional("response.created", 2*time.Second); ok {
		t.Fatalf("a silent turn must not announce a response: %#v", event)
	}
}

// The warning is for the operator, who is the only one who can fix it. The
// client learns the turn was cut short; the log says which knob to turn.
func TestTheOperatorIsToldWhichKnobToTurn(t *testing.T) {
	fastProvider := fast([]continuation.Event{}).stopping(continuation.Completion{
		StopReason: "length", ReasoningInContent: true,
	})
	logs := &syncBuffer{}
	server := startServerWithLogger(t, fastProvider, slow([]continuation.Event{}), logs)
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(nil)
	client.await("session.updated", 5*time.Second)
	client.speak()
	client.await("response.done", 10*time.Second)

	recorded := logs.String()
	if !strings.Contains(recorded, "turn produced no speech") {
		t.Fatalf("the operator must be told the turn was silent, got %q", recorded)
	}
	for _, actionable := range []string{"reasoning block", "-fast-max-tokens"} {
		if !strings.Contains(recorded, actionable) {
			t.Fatalf("the warning must name what to change (%q), got %q", actionable, recorded)
		}
	}
}
