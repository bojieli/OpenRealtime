package state

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestObservationCommitOutcomeOmitsAbsentCommittedContextOnWire(t *testing.T) {
	rejected, err := json.Marshal(ObservationCommitOutcome{
		Kind: ObservationRejected, Code: "invalid_observation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rejected), `"context"`) {
		t.Fatalf("rejected outcome serialized an absent committed context: %s", rejected)
	}

	_, commit, _ := validObservationCommitReply(t)
	committed, err := json.Marshal(ObservationCommitOutcome{
		Kind: ObservationCommitted, StoreVersion: commit.Version, Context: commit.Context,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(committed), `"context"`) ||
		!strings.Contains(string(committed), commit.Context.Prefix.Digest) {
		t.Fatalf("committed outcome omitted its compact context identity: %s", committed)
	}
}

func TestObservationCommitReplyRequiresExactCommittedContext(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*element.Envelope, *Commit, *pendingObservationCommit)
		want   string
	}{
		{name: "valid"},
		{
			name: "session", want: "does not match pending observation session",
			mutate: func(envelope *element.Envelope, _ *Commit, _ *pendingObservationCommit) {
				envelope.SessionID = "other-session"
			},
		},
		{
			name: "appended observation", want: "do not exactly attest",
			mutate: func(_ *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				commit.AppendedIDs = append(commit.AppendedIDs, "unattested")
			},
		},
		{
			name: "version", want: "do not match",
			mutate: func(_ *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				commit.Version++
			},
		},
		{
			name: "snapshot shape", want: "does not equal item count",
			mutate: func(_ *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				commit.Version = 2
				commit.Snapshot.Version = 2
				commit.Context.Prefix.Version = 2
			},
		},
		{
			name: "digest", want: "digest mismatch",
			mutate: func(_ *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				commit.Context.Prefix.Digest = "sha256:" + strings.Repeat("0", 64)
			},
		},
		{
			name: "state causal parent", want: "does not causally name State item",
			mutate: func(envelope *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				envelope.CausalParents = slices.DeleteFunc(envelope.CausalParents, func(parent string) bool {
					return parent == commit.Context.StateItemID
				})
			},
		},
		{
			name: "state identity", want: "invalid State item ID",
			mutate: func(envelope *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				old := commit.Context.StateItemID
				commit.Context.StateItemID = " state-item "
				for index := range envelope.CausalParents {
					if envelope.CausalParents[index] == old {
						envelope.CausalParents[index] = commit.Context.StateItemID
					}
				}
			},
		},
		{
			name: "state identity internal whitespace", want: "invalid State item ID",
			mutate: func(envelope *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				old := commit.Context.StateItemID
				commit.Context.StateItemID = "state item"
				for index := range envelope.CausalParents {
					if envelope.CausalParents[index] == old {
						envelope.CausalParents[index] = commit.Context.StateItemID
					}
				}
			},
		},
		{
			name: "state identity overlong", want: "invalid State item ID",
			mutate: func(envelope *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				old := commit.Context.StateItemID
				commit.Context.StateItemID = strings.Repeat("s", 257)
				for index := range envelope.CausalParents {
					if envelope.CausalParents[index] == old {
						envelope.CausalParents[index] = commit.Context.StateItemID
					}
				}
			},
		},
		{
			name: "observation tail", want: "snapshot tail does not match",
			mutate: func(_ *element.Envelope, commit *Commit, _ *pendingObservationCommit) {
				commit.Snapshot.Items[0].Content = "forged observation"
				commit.Context.Prefix = identifyStateTestPrefix(commit.Snapshot)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, commit, pending := validObservationCommitReply(t)
			if testCase.mutate != nil {
				testCase.mutate(&envelope, &commit, &pending)
			}
			err := validateObservationCommitReply(envelope, commit, pending)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("valid commit context refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("validation error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestObservationCommitReplyOwnershipIsExactAndInstanceScoped(t *testing.T) {
	runner := &observationCommitRunner{
		instance: "message_observation_commit",
		pending: map[string]pendingObservationCommit{
			"message_observation_commit-append-7": {},
		},
	}
	tests := []struct {
		name      string
		envelope  element.Envelope
		found     bool
		addressed bool
	}{
		{
			name: "pending exact causal parent", found: true, addressed: true,
			envelope: element.Envelope{
				ItemID:        "trajectory:committed",
				CausalParents: []string{"message_observation_commit-append-7"},
			},
		},
		{
			name: "pending exact reply base", found: true, addressed: true,
			envelope: element.Envelope{ItemID: "message_observation_commit-append-7:committed"},
		},
		{
			name: "foreign fanout receipt", found: false, addressed: false,
			envelope: element.Envelope{
				ItemID:        "audio_observation_commit-append-8:committed",
				CausalParents: []string{"audio_observation_commit-append-8"},
			},
		},
		{
			name: "missing own receipt", found: false, addressed: true,
			envelope: element.Envelope{
				ItemID:        "message_observation_commit-append-9:committed",
				CausalParents: []string{"message_observation_commit-append-9"},
			},
		},
		{
			name: "prefix confusion", found: false, addressed: false,
			envelope: element.Envelope{
				ItemID: "other-message_observation_commit-append-9:committed",
			},
		},
		{
			name: "case confusion", found: false, addressed: false,
			envelope: element.Envelope{
				ItemID: "Message_observation_commit-append-9:committed",
			},
		},
		{
			name: "suffix collision", found: false, addressed: false,
			envelope: element.Envelope{
				ItemID: "message_observation_commit-append-9evil:committed",
			},
		},
		{
			name: "noncanonical leading zero", found: false, addressed: false,
			envelope: element.Envelope{
				ItemID: "message_observation_commit-append-09:committed",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, found := runner.pendingForReply(test.envelope)
			if found != test.found || runner.replyTargetsInstance(test.envelope) != test.addressed {
				t.Fatalf("reply ownership found=%t addressed=%t, want %t/%t",
					found, runner.replyTargetsInstance(test.envelope), test.found, test.addressed)
			}
		})
	}
}

func validObservationCommitReply(
	t *testing.T,
) (element.Envelope, Commit, pendingObservationCommit) {
	t.Helper()
	item := trajectory.Item{
		ID: "observation_commit-observation-1", Kind: trajectory.KindObservation,
		MonotonicNS: 42, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
		Event: &trajectory.EventMetadata{
			EventID: "asr-1", Type: "audio", Source: "audio", Channel: "microphone",
		},
	}
	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{item}}
	context := CommittedContext{
		Prefix: identifyStateTestPrefix(snapshot), StateItemID: "trajectory-snapshot-1",
	}
	pending := pendingObservationCommit{
		trigger:           element.Envelope{ItemID: "asr-1", SessionID: "session-state"},
		canonicalRevision: 1, trajectoryItemID: item.ID, trajectoryItem: item,
	}
	commit := Commit{
		Version: 1, AppendedIDs: []string{item.ID}, Snapshot: snapshot, Context: context,
	}
	envelope := element.Envelope{
		ItemID: "observation_commit-append-1:committed", SessionID: "session-state",
		CausalParents: []string{"observation_commit-append-1", context.StateItemID},
	}
	return envelope, commit, pending
}

func identifyStateTestPrefix(snapshot trajectory.Snapshot) trajectory.PrefixIdentity {
	identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		panic(err)
	}
	return identity
}
