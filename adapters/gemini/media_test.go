package gemini_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// An observation's keyframes must reach a model that can see. Narration is
// what survives after images are pruned, but while they exist, reasoning about
// a screen and clicking on one are different tasks and only the second needs
// pixels.
func TestObservationKeyframesReachTheModel(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &captured)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer server.Close()

	provider, err := gemini.New(gemini.Config{
		APIKey: "test", Model: "test-model", Endpoint: server.URL,
		Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh,
		ToolAuthority: continuation.ToolAuthorityExecute,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: "obs-1", Kind: trajectory.KindObservation, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: "video"},
		Content:  "A confirmation dialog is open.",
		Observation: &trajectory.ObservationMeta{
			Observer: "video", Source: "screen", Authority: trajectory.AuthorityObserver,
			Media: []trajectory.MediaRef{
				{Handle: "frame-1", MIMEType: "image/jpeg", Width: 1280, Height: 720},
				{Handle: "expired", MIMEType: "image/jpeg"},
			},
		},
	}}}
	resolved := 0
	_, err = provider.Continue(context.Background(), continuation.Request{
		InvocationID: "inv-1", Descriptor: provider.Descriptor(),
		Trajectory: snapshot,
		Invocation: continuation.Invocation{Instruction: "look"},
		Media: func(handle string) (continuation.Media, error) {
			resolved++
			if handle != "frame-1" {
				// Retention is bounded on purpose: a handle whose window has
				// passed is an ordinary outcome, not a failure.
				return continuation.Media{}, io.EOF
			}
			return continuation.Media{MIMEType: "image/jpeg", Bytes: []byte("jpegbytes")}, nil
		},
	}, func(continuation.Event) error { return nil })
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if resolved != 2 {
		t.Fatalf("every handle must be attempted, got %d", resolved)
	}

	encoded, _ := json.Marshal(captured)
	body := string(encoded)
	if !strings.Contains(body, "inlineData") {
		t.Fatalf("the keyframe never reached the model:\n%s", body)
	}
	if !strings.Contains(body, "anBlZ2J5dGVz") { // base64 of "jpegbytes"
		t.Fatalf("the image bytes are not what was attached:\n%s", body)
	}
	if strings.Count(body, "inlineData") != 1 {
		t.Fatal("an expired handle must be skipped rather than failing the continuation")
	}
	if !strings.Contains(body, "A confirmation dialog is open.") {
		t.Fatal("the narration must accompany the image, not be replaced by it")
	}
}

// A provider that is handed no resolver must still work: a runtime that
// retains nothing is a normal configuration, not a broken one.
func TestNoResolverMeansNarrationOnly(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &captured)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	}))
	defer server.Close()

	provider, _ := gemini.New(gemini.Config{
		APIKey: "test", Model: "test-model", Endpoint: server.URL,
		Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh,
	})
	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: "obs-1", Kind: trajectory.KindObservation, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver},
		Content:  "A dialog is open.",
		Observation: &trajectory.ObservationMeta{
			Observer: "video", Authority: trajectory.AuthorityObserver,
			Media: []trajectory.MediaRef{{Handle: "frame-1", MIMEType: "image/jpeg"}},
		},
	}}}
	if _, err := provider.Continue(context.Background(), continuation.Request{
		InvocationID: "inv-1", Descriptor: provider.Descriptor(),
		Trajectory: snapshot, Invocation: continuation.Invocation{Instruction: "look"},
	}, func(continuation.Event) error { return nil }); err != nil {
		t.Fatalf("continue: %v", err)
	}
	encoded, _ := json.Marshal(captured)
	if strings.Contains(string(encoded), "inlineData") {
		t.Fatal("with no resolver there are no images to attach")
	}
	if !strings.Contains(string(encoded), "A dialog is open.") {
		t.Fatal("the narration must still be there")
	}
}
