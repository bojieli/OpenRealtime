package sourcebundle

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
)

func legacyCompletionFixture(t *testing.T, observers string) (candidate.Completion, []byte, []byte) {
	t.Helper()
	fixture := newSourceFixture(t)
	completion := candidate.Completion{
		Attempt: fixture.attempt(t, "historical-overlap", 1, false),
		Outcome: fixtureOutcome("historical-overlap", true), Transcript: fixtureTranscript(),
	}
	completion.Outcome.Notes["applicable"] = "false"
	completion.Transcript.Runtime = &binding.Status{
		Architecture: binding.ArchitectureIdentity{ID: "historical"}, Graph: binding.ArchitectureIdentity{ID: "graph"},
		Binding: "cascade", Ownership: binding.Ownership{Perception: binding.OwnerEngine},
		Stack: binding.StackCapabilities{AudioInput: true}, Policies: interaction.Report{Trigger: "historical"},
		Interaction: binding.InteractionStatus{Evidence: "historical"}, Tools: binding.ToolStatus{Fast: "historical"},
		Fast: "fixture-fast",
	}
	if observers == "[]" {
		completion.Transcript.Runtime.Observers = []string{}
	}
	// Construct the former representation independently of the compatibility
	// encoder: before 997a4e0, an empty observer field was always present here.
	encode := func(value any, indented bool) []byte {
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		needle := []byte(`"fast":"fixture-fast"`)
		if bytes.Count(payload, needle) != 1 {
			t.Fatal("fixture runtime anchor is ambiguous")
		}
		payload = bytes.Replace(payload, needle, []byte(`"observers":`+observers+`,"fast":"fixture-fast"`), 1)
		if indented {
			var output bytes.Buffer
			if err := json.Indent(&output, payload, "", "  "); err != nil {
				t.Fatal(err)
			}
			payload = append(output.Bytes(), '\n')
		}
		return payload
	}
	context := reviewContext{
		Attempt: completion.Attempt, DeterministicOutcome: completion.Outcome, Transcript: completion.Transcript,
		DeterministicAuthority: "benchmark scorer is authoritative", AdvisoryReviewAuthority: "offline multimodal review is advisory",
	}
	return completion, encode(completion, true), encode(context, false)
}

func TestHistoricalRuntimeCompletionReopensExactContextAndKeepsOriginalScore(t *testing.T) {
	for _, observers := range []string{"null", "[]"} {
		t.Run(observers, func(t *testing.T) {
			want, completionPayload, contextPayload := legacyCompletionFixture(t, observers)
			directory := t.TempDir()
			files := make(map[string]SourceFile)
			for name, payload := range map[string][]byte{"completion.json": completionPayload, "context.json": contextPayload} {
				if err := os.WriteFile(filepath.Join(directory, name), payload, 0600); err != nil {
					t.Fatal(err)
				}
				files[name] = SourceFile{Path: name, SHA256: digest(payload), SizeBytes: int64(len(payload))}
			}
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			contextSHA, err := review.CanonicalContextSHA256(t.Context(), contextPayload)
			if err != nil {
				t.Fatal(err)
			}
			entry := AttemptEntry{Directory: ".", CompletionPath: "completion.json", CompletionSHA256: digest(completionPayload),
				ContextPath: "context.json", ContextSHA256: contextSHA}
			got, retainedContext, err := verifyAttemptCompletion(t.Context(), root, entry, files)
			if err != nil || !reflect.DeepEqual(got, want) || !bytes.Equal(retainedContext, contextPayload) {
				t.Fatalf("historical completion/context changed or failed reopening: %v", err)
			}
			// Representation compatibility never permits editing sealed bytes,
			// even when the edited values still decode into a valid outcome.
			changed := bytes.Replace(completionPayload, []byte(`"passed": true`), []byte(`"passed":false`), 1)
			if len(changed) != len(completionPayload) {
				t.Fatal("mutation changed file size")
			}
			if err := os.WriteFile(filepath.Join(directory, "completion.json"), changed, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := verifyAttemptCompletion(t.Context(), root, entry, files); err == nil {
				t.Fatal("changed historical evidence passed its original digest")
			}
		})
	}
}

func TestHistoricalRuntimeCompatibilityDoesNotRelaxCanonicalDecoding(t *testing.T) {
	_, payload, context := legacyCompletionFixture(t, "null")
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{"whitespace", append(append([]byte{}, payload...), ' ')},
		{"unknown field", bytes.Replace(payload, []byte(`"observers": null`), []byte(`"unknown": null, "observers": null`), 1)},
		{"duplicate field", bytes.Replace(payload, []byte(`"observers": null`), []byte(`"observers": null, "observers": null`), 1)},
		{"invalid type", bytes.Replace(payload, []byte(`"observers": null`), []byte(`"observers": false`), 1)},
		{"hybrid encoding", bytes.Replace(payload, []byte(`"architecture": {`), []byte(`"architecture": null, "unused": {`), 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got candidate.Completion
			if err := decodeCanonical(test.payload, &got); err == nil {
				t.Fatal("noncanonical legacy completion accepted")
			}
		})
	}
	var retained reviewContext
	if err := decodeCompact(append(context, '\n'), &retained); err == nil {
		t.Fatal("noncanonical legacy context accepted")
	}
	var unrelated map[string]any
	if err := decodeCanonical(payload, &unrelated); err == nil {
		t.Fatal("legacy fallback applied outside typed archive records")
	}
}
