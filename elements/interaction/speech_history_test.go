package interaction

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestStaleResultRetainsOnlySpeechWithExactOriginalContext(t *testing.T) {
	for _, test := range []struct {
		name                                         string
		enabled, voice, proof, proposal, interrupted bool
		want                                         ModelCommitKind
	}{
		{"speech", true, true, true, false, false, ModelSpeechRetained},
		{"speech and stale proposal", true, true, true, true, false, ModelSpeechRetained},
		{"interrupted speech and stale proposal", true, true, true, true, true, ModelSpeechRetained},
		{"disabled", false, true, true, true, false, ModelRejected},
		{"silent model", true, false, true, true, false, ModelRejected},
		{"unbound legacy context", true, true, false, true, false, ModelRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, _ := json.Marshal(ModelResultCommitConfig{RetainRejectedSpeech: test.enabled})
			mounted, done, cancel := mountInteractionGraph(t, commitGraph, map[string]json.RawMessage{"commit": config}, nil)
			defer stopInteractionGraph(t, done, cancel)
			snapshots, outcomes := egress(t, mounted, "snapshot"), egress(t, mounted, "outcome")
			original := receive(t, snapshots).Payload.(trajectory.Snapshot)
			prefix, err := trajectory.IdentifyPrefix(original, original.Version)
			if err != nil {
				t.Fatal(err)
			}
			current := modelResult("newer-context", 0, "", false, false)
			send(t, ingress(t, mounted, "result"), resultEnvelopeForTest("newer", current))
			advanced := receive(t, snapshots).Payload.(trajectory.Snapshot)
			_ = receiveModelCommitKind(t, outcomes, ModelCommitted)
			stale := modelResult("count", 0, "", test.interrupted, test.proposal)
			if test.voice {
				stale.Descriptor.SpeechAuthority = continuation.SpeechAuthorityVoice
			}
			if test.proof {
				stale.ContextPrefix = prefix
			}
			stale.Outputs = append([]cognitionelements.PreparedOutput{{Kind: cognitionelements.PreparedReasoning, Text: "unpublished reasoning"}, {Kind: cognitionelements.PreparedAssistant, Text: "One."}}, stale.Outputs...)
			if !test.proposal {
				stale.Outputs = stale.Outputs[:2]
			}
			stale.AssistantText = "One."
			stale.ReasoningText = "unpublished reasoning"
			stale.ReasoningRetained = true
			stale.Completion.ProviderStateType = "opaque/native"
			stale.Completion.ProviderState = json.RawMessage(`{"text":"unpublished reasoning","tool":"stale proposal"}`)
			send(t, ingress(t, mounted, "result"), resultEnvelopeForTest("stale-count", stale))
			outcome := receiveModelCommitKind(t, outcomes, test.want)
			if outcome.Kind != test.want {
				t.Fatal(outcome)
			}
			if test.want == ModelRejected {
				assertNoEnvelope(t, snapshots)
				return
			}
			retained := receive(t, snapshots).Payload.(trajectory.Snapshot)
			if retained.Version != advanced.Version+2 || outcome.StoreVersion != retained.Version || len(outcome.ItemIDs) != 2 {
				t.Fatalf("unexpected history: %+v %+v", outcome, retained)
			}
			for _, item := range retained.Items[len(advanced.Items):] {
				if item.Kind != trajectory.KindInstruction && item.Kind != trajectory.KindAssistant || item.ToolCall != nil || item.ProviderStateType != "" || len(item.ProviderState) != 0 || strings.Contains(item.Content, "reasoning") {
					t.Fatalf("stale non-speech acquired history: %+v", item)
				}
				if item.Kind == trajectory.KindAssistant && (item.Visibility != trajectory.VisibilityPrepared || item.Content != "One." || item.Interrupted != test.interrupted) {
					t.Fatalf("history claims playback or loses speech: %+v", item)
				}
			}
			send(t, ingress(t, mounted, "result"), resultEnvelopeForTest("duplicate-count", stale))
			_ = receiveModelCommitKind(t, outcomes, ModelIgnored)
			assertNoEnvelope(t, snapshots)
		})
	}
}

func TestSpeechHistoryCommitRefusesChangedReceiptAndPreservesExactTime(t *testing.T) {
	for _, mode := range []string{"valid", "changed speech", "changed origin", "changed commit time", "added proposal"} {
		t.Run(mode, func(t *testing.T) {
			mounted, done, cancel := mountInteractionGraph(t, commitAdapterGraph, map[string]json.RawMessage{"commit": json.RawMessage(`{"retain_rejected_speech":true}`)}, nil)
			defer cancel()
			store := trajectory.NewStore()
			if err := store.Append(trajectory.Item{ID: "original", Kind: trajectory.KindObservation, MonotonicNS: 1, Content: "Count.", Producer: trajectory.Producer{Phase: trajectory.PhaseUser}}); err != nil {
				t.Fatal(err)
			}
			prefix, err := trajectory.IdentifyPrefix(store.Snapshot(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Append(trajectory.Item{ID: "later", Kind: trajectory.KindObservation, MonotonicNS: 1 << 60, Content: "A final fragment.", Producer: trajectory.Producer{Phase: trajectory.PhaseUser}}); err != nil {
				t.Fatal(err)
			}
			result := modelResult("count", 1, "original", false, false)
			result.Descriptor.SpeechAuthority = continuation.SpeechAuthorityVoice
			result.ContextPrefix = prefix
			send(t, ingress(t, mounted, "result"), resultEnvelopeForTest("count-result", result))
			original := receive(t, egress(t, mounted, "append"))
			send(t, ingress(t, mounted, "rejected"), element.Envelope{Type: rejectionType, ItemID: original.ItemID + ":rejected", RunID: original.RunID, SessionID: original.SessionID, CausalParents: []string{original.ItemID}, Payload: stateelements.Rejection{ExpectedVersion: 1, CurrentVersion: 2, Code: "version_conflict"}})
			history := receive(t, egress(t, mounted, "append"))
			request := history.Payload.(stateelements.Append)
			if request.Compare || request.Prefix == nil || *request.Prefix != prefix {
				t.Fatalf("history rebased its source: %+v", request)
			}
			if err = store.AppendBatchOnPrefix(*request.Prefix, request.Items); err != nil {
				t.Fatal(err)
			}
			snapshot := store.Snapshot()
			ids := make([]string, len(request.Items))
			for i, item := range request.Items {
				ids[i] = item.ID
			}
			switch mode {
			case "changed speech":
				snapshot.Items[len(snapshot.Items)-1].Content = "Two."
			case "changed origin":
				snapshot.Items[0].Content = "Forged request."
			case "changed commit time":
				snapshot.Items[len(snapshot.Items)-1].MonotonicNS++
			case "added proposal":
				snapshot.Items[len(snapshot.Items)-1].Kind = trajectory.KindToolProposal
			}
			send(t, ingress(t, mounted, "committed"), element.Envelope{Type: commitType, ItemID: history.ItemID + ":committed", RunID: history.RunID, SessionID: history.SessionID, CausalParents: []string{history.ItemID}, Payload: stateelements.Commit{Version: snapshot.Version, AppendedIDs: ids, Snapshot: snapshot}})
			if mode == "valid" {
				_ = receiveModelCommitKind(t, egress(t, mounted, "outcome"), ModelSpeechRetained)
				stopInteractionGraph(t, done, cancel)
				return
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("forged history receipt accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("forged history receipt did not stop commit")
			}
		})
	}
}
