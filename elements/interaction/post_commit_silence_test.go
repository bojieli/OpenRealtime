package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const postCommitSilenceGraph = `graph post_commit_silence_test {
    interaction.PostCommitSilence :: silence;
    input audio_commit = silence.committed;
    input message_commit = silence.committed;
    output create = silence.create;
    output state = silence.state;
    output outcome = silence.outcome;
}
`

func TestPostCommitSilenceWaitsExactIntervalAndResetsFromLatestDurableCommit(t *testing.T) {
	manual := clock.NewManual(uint64(time.Second))
	mounted, done, cancel := mountPostCommitSilence(t, manual)
	defer stopInteractionGraph(t, done, cancel)

	audio := ingress(t, mounted, "audio_commit")
	message := ingress(t, mounted, "message_commit")
	created := egress(t, mounted, "create")
	states := egress(t, mounted, "state")
	outcomes := egress(t, mounted, "outcome")

	first := postCommitSilenceEnvelope(1, "audio")
	send(t, audio, first)
	armed := receive(t, outcomes).Payload.(PostCommitSilenceOutcome)
	state := receive(t, states).Payload.(PostCommitSilenceState)
	wantFirstDeadline := uint64(time.Second + 15*time.Second)
	if armed.Kind != PostCommitSilenceArmed || armed.Generation != 1 ||
		armed.DeadlineNS != wantFirstDeadline || !state.Armed ||
		state.ContextVersion != 1 || state.ResetCount != 0 {
		t.Fatalf("first silence arm outcome=%+v state=%+v", armed, state)
	}

	manual.AdvanceNS(uint64(14*time.Second + 999*time.Millisecond))
	assertNoEnvelope(t, created)

	second := postCommitSilenceEnvelope(2, "message")
	send(t, message, second)
	armed = receive(t, outcomes).Payload.(PostCommitSilenceOutcome)
	state = receive(t, states).Payload.(PostCommitSilenceState)
	wantSecondDeadline := uint64(30*time.Second + 999*time.Millisecond)
	if armed.Kind != PostCommitSilenceArmed || armed.Generation != 2 ||
		armed.DeadlineNS != wantSecondDeadline || state.ResetCount != 1 ||
		state.ContextVersion != 2 {
		t.Fatalf("reset silence arm outcome=%+v state=%+v", armed, state)
	}

	// Reaching the first commit's old deadline cannot wake the graph after the
	// later durable observation reset it.
	manual.AdvanceNS(uint64(time.Millisecond))
	assertNoEnvelope(t, created)

	// A reordered older lane is visible but cannot move the latest-prefix
	// deadline backwards.
	send(t, audio, first)
	ignored := receive(t, outcomes).Payload.(PostCommitSilenceOutcome)
	state = receive(t, states).Payload.(PostCommitSilenceState)
	if ignored.Kind != PostCommitSilenceIgnored || ignored.Code != "stale_commit" ||
		state.Generation != 2 || state.ContextVersion != 2 || state.ResetCount != 1 {
		t.Fatalf("stale commit outcome=%+v state=%+v", ignored, state)
	}

	manual.AdvanceNS(uint64(14*time.Second + 998*time.Millisecond))
	assertNoEnvelope(t, created)
	manual.AdvanceNS(uint64(time.Millisecond))

	createEnvelope := receive(t, created)
	create, ok := createEnvelope.Payload.(policyelements.ResponseCreate)
	if !ok || create.ResponseID != createEnvelope.ItemID ||
		create.ExpectedContextVersion == nil || *create.ExpectedContextVersion != 2 ||
		create.ExpectedContextItemID != "state-message-2" ||
		!createEnvelope.Type.Equal(policyelements.ResponseCreateType()) ||
		createEnvelope.SessionID != second.SessionID ||
		!slicesContain(createEnvelope.CausalParents, second.ItemID) ||
		!slicesContain(createEnvelope.CausalParents, "state-message-2") {
		t.Fatalf("post-commit response create = %+v payload=%+v", createEnvelope, create)
	}
	fired := receive(t, outcomes).Payload.(PostCommitSilenceOutcome)
	state = receive(t, states).Payload.(PostCommitSilenceState)
	if fired.Kind != PostCommitSilenceFired || fired.Generation != 2 ||
		fired.ResponseID != create.ResponseID || state.Armed || state.FiredCount != 1 ||
		state.ContextVersion != 2 {
		t.Fatalf("fired silence outcome=%+v state=%+v", fired, state)
	}
	manual.AdvanceNS(uint64(time.Minute))
	assertNoEnvelope(t, created)
}

func TestPostCommitSilenceRejectsInvalidEvidenceWithoutDisarmingAValidTimer(t *testing.T) {
	manual := clock.NewManual(0)
	mounted, done, cancel := mountPostCommitSilence(t, manual)
	defer stopInteractionGraph(t, done, cancel)

	audio := ingress(t, mounted, "audio_commit")
	created := egress(t, mounted, "create")
	states := egress(t, mounted, "state")
	outcomes := egress(t, mounted, "outcome")
	send(t, audio, postCommitSilenceEnvelope(1, "audio"))
	_ = receive(t, outcomes)
	_ = receive(t, states)

	invalid := postCommitSilenceEnvelope(2, "audio")
	invalid.Payload = "forged"
	send(t, audio, invalid)
	refused := receive(t, outcomes).Payload.(PostCommitSilenceOutcome)
	state := receive(t, states).Payload.(PostCommitSilenceState)
	if refused.Kind != PostCommitSilenceRefused || refused.Code != "invalid_payload" ||
		!state.Armed || state.Generation != 1 || state.ContextVersion != 1 {
		t.Fatalf("invalid commit outcome=%+v state=%+v", refused, state)
	}

	rejected := postCommitSilenceEnvelope(2, "audio")
	commit := rejected.Payload.(stateelements.ObservationCommitOutcome)
	commit.Kind = stateelements.ObservationRejected
	rejected.Payload = commit
	send(t, audio, rejected)
	ignored := receive(t, outcomes).Payload.(PostCommitSilenceOutcome)
	state = receive(t, states).Payload.(PostCommitSilenceState)
	if ignored.Kind != PostCommitSilenceIgnored || ignored.Code != "observation_not_committed" ||
		!state.Armed || state.Generation != 1 {
		t.Fatalf("rejected commit outcome=%+v state=%+v", ignored, state)
	}

	manual.AdvanceNS(uint64(15 * time.Second))
	create := receive(t, created).Payload.(policyelements.ResponseCreate)
	if create.ExpectedContextVersion == nil || *create.ExpectedContextVersion != 1 {
		t.Fatalf("invalid evidence changed armed prefix: %+v", create)
	}
	_ = receive(t, outcomes)
	_ = receive(t, states)
}

func TestPostCommitSilenceDescriptorAndConfigAreExplicitAndBounded(t *testing.T) {
	descriptor := PostCommitSilenceDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	committed, _ := descriptor.Port("committed")
	create, _ := descriptor.Port("create")
	if committed.Cardinality != element.Variadic || committed.MinConnections != 1 ||
		!committed.Type.Equal(stateelements.ObservationCommitOutcomeType()) ||
		!create.Type.Equal(policyelements.ResponseCreateType()) ||
		!descriptor.Reaction.BreaksCycles {
		t.Fatalf("post-commit silence descriptor = %+v", descriptor)
	}
	for _, source := range []string{
		`{}`, `{"delay_ms":0}`, `{"delay_ms":600001}`,
		`{"delay_ms":15000,"delay_ms":15001}`, `{"delay_ms":15000,"policy":"speak"}`,
	} {
		if _, err := decodePostCommitSilenceConfig(json.RawMessage(source)); err == nil {
			t.Fatalf("invalid post-commit silence config accepted: %s", source)
		}
	}
	config, err := decodePostCommitSilenceConfig(json.RawMessage(`{"delay_ms":15000}`))
	if err != nil || config.DelayMS != 15_000 {
		t.Fatalf("post-commit silence config=%+v err=%v", config, err)
	}
}

func TestPostCommitSilenceStopsPromptlyWithArmedTimer(t *testing.T) {
	manual := clock.NewManual(0)
	mounted, done, cancel := mountPostCommitSilence(t, manual)
	send(t, ingress(t, mounted, "audio_commit"), postCommitSilenceEnvelope(1, "audio"))
	_ = receive(t, egress(t, mounted, "outcome"))
	_ = receive(t, egress(t, mounted, "state"))
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("armed silence timer stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("armed silence timer did not stop promptly")
	}
	manual.AdvanceNS(uint64(time.Hour))
}

func mountPostCommitSilence(
	t *testing.T, manual *clock.Manual,
) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	graph := compileSource(t, "post-commit-silence-test.ortg", []byte(postCommitSilenceGraph))
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(PostCommitSilenceSchedulerService, manual); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: testRegistry(t), Services: services,
		Values: map[string]json.RawMessage{"silence": json.RawMessage(`{"delay_ms":15000}`)},
		Now:    manual.NowNS,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func postCommitSilenceEnvelope(version uint64, source string) element.Envelope {
	identity := strconv.FormatUint(version, 10)
	stateID := "state-" + source + "-" + identity
	commit := stateelements.ObservationCommitOutcome{
		Kind:             stateelements.ObservationCommitted,
		TriggerItemID:    "trigger-" + source + "-" + identity,
		TrajectoryItemID: "trajectory-" + source + "-" + identity,
		StreamID:         source, ObservationRevision: version, SourceRevision: version,
		StoreVersion: version,
		Context: stateelements.CommittedContext{
			Prefix: trajectory.PrefixIdentity{
				Version: version, Digest: "sha256:" + strings.Repeat(strconv.FormatUint(version%10, 10), 64),
			},
			StateItemID: stateID,
		},
	}
	return element.Envelope{
		Type:   stateelements.ObservationCommitOutcomeType(),
		ItemID: "commit-" + source + "-" + identity, SessionID: "session-silence",
		CausalParents: []string{stateID}, Payload: commit,
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
