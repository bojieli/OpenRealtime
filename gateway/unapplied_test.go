package gateway_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

// A field this deployment parses and does not act on is refused by name.
//
// The alternative it replaces is not a refusal but a silence: the field was
// dropped, session.updated reported the value actually in force, and the only
// way to notice was to read the field back and see it had changed. Most
// clients never read it back - and one of these is not cosmetic. A client that
// asks for no tools and receives tool calls is watching behaviour it turned
// off.
func TestFieldsThisDeploymentDoesNotApplyAreRefusedByName(t *testing.T) {
	server := startServer(t, fast([]continuation.Event{}), slow([]continuation.Event{}), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type":        "realtime",
		"tool_choice": "none",
		"reasoning":   map[string]any{"effort": "high"},
		"audio": map[string]any{
			"input": map[string]any{
				"format":        map[string]any{"type": "audio/pcm", "rate": 24000},
				"transcription": map[string]any{"model": "whisper-invented"},
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"speed":  1.5,
			},
		},
	}})

	// The rest of the update still applies. That is the whole point of
	// refusing by name rather than rejecting the event.
	updated := client.await("session.updated", 5*time.Second)
	session, _ := updated["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	format, _ := input["format"].(map[string]any)
	if rate, _ := format["rate"].(float64); rate != 24000 {
		t.Fatalf("the applicable half of the update must still apply, got %#v", format)
	}

	wanted := map[string]bool{
		"session.tool_choice":                     false,
		"session.audio.output.speed":              false,
		"session.audio.input.transcription.model": false,
		"session.reasoning":                       false,
	}
	for range wanted {
		event, ok := client.awaitOptional("error", 5*time.Second)
		if !ok {
			break
		}
		failure, _ := event["error"].(map[string]any)
		param, _ := failure["param"].(string)
		if _, expected := wanted[param]; !expected {
			t.Fatalf("unexpected refusal of %q: %v", param, failure["message"])
		}
		wanted[param] = true
		if message, _ := failure["message"].(string); !strings.Contains(message, "not applied") {
			t.Fatalf("a refusal must say the field was not applied, got %q", message)
		}
	}
	for param, seen := range wanted {
		if !seen {
			t.Fatalf("%s was dropped without a word", param)
		}
	}
}

// The converse: asking for what is already in force is not a refusal. A client
// that sends the server's own defaults back has asked for nothing it will not
// get, and answering it with errors would make the honest clients the noisy
// ones.
func TestAskingForWhatIsInForceIsNotRefused(t *testing.T) {
	server := startServer(t, fast([]continuation.Event{}), slow([]continuation.Event{}), "hello")
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	session, _ := created["session"].(map[string]any)
	audio, _ := session["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	transcription, _ := input["transcription"].(map[string]any)
	inForce, _ := transcription["model"].(string)

	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime", "tool_choice": "auto",
		"audio": map[string]any{
			"input": map[string]any{
				"format":        map[string]any{"type": "audio/pcm", "rate": 24000},
				"transcription": map[string]any{"model": inForce},
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "speed": 1,
			},
		},
	}})
	client.await("session.updated", 5*time.Second)
	if event, ok := client.awaitOptional("error", 2*time.Second); ok {
		failure, _ := event["error"].(map[string]any)
		t.Fatalf("the settings already in force must not be refused: %v", failure)
	}
}
