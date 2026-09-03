package graphs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type conversationalReference struct {
	name   string
	graph  ir.Graph
	values graphvalues.Document
	bound  graphvalues.Bound
}

type conversationalTurnGolden struct {
	FormatVersion uint64                         `json:"format_version"`
	Cases         []conversationalTurnGoldenCase `json:"cases"`
}

type conversationalTurnGoldenCase struct {
	Profile           string   `json:"profile"`
	GraphFingerprint  string   `json:"graph_fingerprint"`
	PlayedSpeech      []string `json:"played_speech"`
	CommittedResults  int      `json:"committed_results"`
	RejectedResults   int      `json:"rejected_results"`
	RejectionCode     string   `json:"rejection_code"`
	FinalStoreVersion uint64   `json:"final_store_version"`
	ObservationItems  int      `json:"observation_items"`
	InstructionItems  int      `json:"instruction_items"`
	AssistantItems    int      `json:"assistant_items"`
}

func TestConversationalReferenceFamilyIsLockedTypedAndComposable(t *testing.T) {
	names := []string{
		"conversational-fast-only",
		"conversational-slow-only",
		"conversational-both",
	}
	references := make(map[string]conversationalReference, len(names))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			reference := loadConversationalReference(t, name)
			assertConversationalControlBoundaries(t, reference)
			assertConversationalExactModelContextDelivery(t, reference.graph)
			assertNoModelToModelEdges(t, reference.graph)
			exerciseConversationalProviderBoundary(t, reference)
			references[name] = reference
		})
	}
	if len(references) != len(names) {
		t.Fatalf("loaded %d conversational references, want %d", len(references), len(names))
	}

	assertSharedConversationalBackbone(t, references)
	assertConversationalRoutingVariants(t, references)
}

func assertConversationalExactModelContextDelivery(t *testing.T, graph ir.Graph) {
	t.Helper()
	want := map[string]bool{
		"fast_model": false,
		"slow_model": false,
	}
	for _, edge := range graph.Edges {
		if edge.From.Node != "snapshot_copy" || edge.From.Port != "out" ||
			edge.To.Port != "context" {
			continue
		}
		if _, expected := want[edge.To.Node]; !expected {
			continue
		}
		want[edge.To.Node] = true
		if edge.Delivery != ir.Lossless {
			t.Errorf("snapshot_copy.out -> %s.context delivery = %s, want %s for committed-prefix reconstruction",
				edge.To.Node, edge.Delivery, ir.Lossless)
		}
	}
	for node, found := range want {
		if !found {
			t.Errorf("missing snapshot_copy.out -> %s.context committed-prefix edge", node)
		}
	}
}

func TestConversationalReferenceFamilyExecutesCompleteTurns(t *testing.T) {
	golden := loadConversationalTurnGolden(t)
	for _, name := range []string{
		"conversational-fast-only", "conversational-slow-only", "conversational-both",
	} {
		t.Run(name, func(t *testing.T) {
			want, found := golden[name]
			if !found {
				t.Fatalf("complete-turn regression artifact omits %s", name)
			}
			reference := loadConversationalReference(t, name)
			if reference.bound.Graph.Fingerprint != want.GraphFingerprint {
				t.Fatalf("complete-turn graph identity = %s, retained artifact wants %s",
					reference.bound.Graph.Fingerprint, want.GraphFingerprint)
			}
			fastTerminalGate := make(chan struct{})
			slowTerminalGate := make(chan struct{})
			var releaseFast, releaseSlow sync.Once
			control := &conversationalContinuationControl{
				terminalGates: map[trajectory.Phase]<-chan struct{}{
					trajectory.PhaseFast: fastTerminalGate,
					trajectory.PhaseSlow: slowTerminalGate,
				},
			}
			fixture := mountConversationalTurn(t, reference, control)
			defer func() {
				releaseSlow.Do(func() { close(slowTerminalGate) })
				releaseFast.Do(func() { close(fastTerminalGate) })
				fixture.stop(t)
			}()

			const sessionID = "session-reference"
			const streamID = "stream-reference"
			for index := uint64(1); index <= 6; index++ {
				fixture.send(t, "audio", element.Envelope{
					Type: fixture.boundaryType("audio"), ItemID: "audio-" + strconv.FormatUint(index, 10),
					SessionID: sessionID, SourceID: streamID, CancellationScope: streamID,
					Payload: acousticelements.InputFrame{
						StreamID: streamID, Frame: conversationalAudioFrame(index, 2_000),
					},
				})
			}
			fixture.await(t, "acoustic_activity", func(payload any) bool {
				activity, ok := payload.(acousticelements.SpeechActivity)
				return ok && activity.Kind == acousticelements.SpeechStarted && activity.StreamID == streamID
			})
			// These reference profiles deliberately use automatic endpointing.
			// Drive the configured 500 ms silence window instead of bypassing the
			// endpoint owner through the manual-commit boundary.
			for index := uint64(7); index <= 31; index++ {
				fixture.send(t, "audio", element.Envelope{
					Type: fixture.boundaryType("audio"), ItemID: "audio-" + strconv.FormatUint(index, 10),
					SessionID: sessionID, SourceID: streamID, CancellationScope: streamID,
					Payload: acousticelements.InputFrame{
						StreamID: streamID, Frame: conversationalAudioFrame(index, 0),
					},
				})
			}
			fixture.await(t, "acoustic_activity", func(payload any) bool {
				activity, ok := payload.(acousticelements.SpeechActivity)
				return ok && activity.Kind == acousticelements.SpeechStopped && activity.StreamID == streamID
			})

			fastBegin := fixture.await(t, "fast_prepared_text", preparedBoundary(cognitionelements.TextBegin))
			slowBegin := fixture.await(t, "slow_prepared_text", preparedBoundary(cognitionelements.TextBegin))
			if fastBegin.RunID == "" || slowBegin.RunID == "" || fastBegin.RunID == slowBegin.RunID {
				t.Fatalf("independent cognition run IDs = fast %q slow %q", fastBegin.RunID, slowBegin.RunID)
			}
			if name == "conversational-both" {
				fixture.sendSelection(t, fastBegin.RunID, interactionelements.SelectionPreempt, "select-fast")
				fixture.sendSelection(t, slowBegin.RunID, interactionelements.SelectionQueue, "queue-slow")
			}
			// Both providers have now crossed the prepared-text safe point against
			// context v1. Complete deliberative cognition first, then release fast,
			// so this fixture tests the retained commit ordering rather than a Go
			// scheduler race between two otherwise independent providers.
			releaseSlow.Do(func() { close(slowTerminalGate) })
			slowCommit := fixture.awaitModelCommit(t, "slow_commit_outcome", slowBegin.RunID)
			if slowCommit.Kind != interactionelements.ModelCommitted || slowCommit.StoreVersion != want.FinalStoreVersion {
				t.Fatalf("deliberative model commit = %+v", slowCommit)
			}
			releaseFast.Do(func() { close(fastTerminalGate) })
			fastCommit := fixture.awaitModelCommit(t, "fast_commit_outcome", fastBegin.RunID)

			for range want.PlayedSpeech {
				fixture.await(t, "tts_outcome", func(payload any) bool {
					outcome, ok := payload.(speechelements.SynthesisOutcome)
					return ok && outcome.Kind == speechelements.OutcomeSucceeded && outcome.Chunks == 1
				})
				fixture.await(t, "playback_outcome", func(payload any) bool {
					outcome, ok := payload.(speechelements.PlaybackOutcome)
					return ok && outcome.Kind == speechelements.OutcomeSucceeded && outcome.CrossedBoundary
				})
			}
			commitOutcomes := []interactionelements.ModelCommitOutcome{fastCommit, slowCommit}
			var committed, rejected int
			for _, outcome := range commitOutcomes {
				switch outcome.Kind {
				case interactionelements.ModelCommitted:
					committed++
					if outcome.StoreVersion != want.FinalStoreVersion {
						t.Fatalf("model commit version = %d, want %d: %+v",
							outcome.StoreVersion, want.FinalStoreVersion, outcome)
					}
				case interactionelements.ModelRejected:
					rejected++
					if outcome.Code != want.RejectionCode || outcome.StoreVersion != want.FinalStoreVersion {
						t.Fatalf("stale model outcome = %+v", outcome)
					}
				}
			}
			if committed != want.CommittedResults || rejected != want.RejectedResults {
				t.Fatalf("safe-point results = %+v; want committed=%d rejected=%d",
					commitOutcomes, want.CommittedResults, want.RejectedResults)
			}
			snapshotEnvelope := fixture.await(t, "trajectory_snapshot", func(payload any) bool {
				snapshot, ok := payload.(trajectory.Snapshot)
				return ok && snapshot.Version == want.FinalStoreVersion
			})
			snapshot := snapshotEnvelope.Payload.(trajectory.Snapshot)
			var observations, instructions, assistants int
			for _, item := range snapshot.Items {
				switch item.Kind {
				case trajectory.KindObservation:
					observations++
				case trajectory.KindInstruction:
					instructions++
				case trajectory.KindAssistant:
					assistants++
				}
			}
			if observations != want.ObservationItems || instructions != want.InstructionItems ||
				assistants != want.AssistantItems {
				t.Fatalf("complete trajectory has observations=%d instructions=%d assistants=%d: %+v",
					observations, instructions, assistants, snapshot.Items)
			}
			if got := fixture.sink.texts(); !equalStrings(got, want.PlayedSpeech) {
				t.Fatalf("played speech = %v, want %v", got, want.PlayedSpeech)
			}
		})
	}
	if len(golden) != 3 {
		t.Fatalf("complete-turn regression artifact has %d cases, want exactly 3", len(golden))
	}
}

func TestConversationalCommittedActivationAdmitsBothLanesWithoutProviderOrdering(t *testing.T) {
	for _, name := range []string{
		"conversational-fast-only", "conversational-slow-only", "conversational-both",
	} {
		t.Run(name, func(t *testing.T) {
			control := &conversationalContinuationControl{}
			fixture := mountConversationalTurn(t, loadConversationalReference(t, name), control)
			defer fixture.stop(t)
			driveConversationalGraphAudio(t, fixture)

			fastBegin := fixture.await(t, "fast_prepared_text", preparedBoundary(cognitionelements.TextBegin))
			slowBegin := fixture.await(t, "slow_prepared_text", preparedBoundary(cognitionelements.TextBegin))
			for _, phase := range []trajectory.Phase{trajectory.PhaseFast, trajectory.PhaseSlow} {
				request := control.request(t, phase, 0)
				if request.Trajectory.Version != 1 || len(request.Trajectory.Items) != 1 {
					t.Fatalf("%s provider sampled trajectory %+v, want exact committed prefix v1", phase, request.Trajectory)
				}
			}

			outcomes := []interactionelements.ModelCommitOutcome{
				fixture.awaitModelCommit(t, "fast_commit_outcome", fastBegin.RunID),
				fixture.awaitModelCommit(t, "slow_commit_outcome", slowBegin.RunID),
			}
			var committed, rejected int
			for _, outcome := range outcomes {
				switch outcome.Kind {
				case interactionelements.ModelCommitted:
					committed++
				case interactionelements.ModelRejected:
					if outcome.Code != "version_conflict" {
						t.Fatalf("stale terminal = %+v", outcome)
					}
					rejected++
				default:
					t.Fatalf("non-terminal model commit outcome = %+v", outcome)
				}
				if outcome.StoreVersion != 3 {
					t.Fatalf("model commit store version = %+v, want 3", outcome)
				}
			}
			if committed != 1 || rejected != 1 {
				t.Fatalf("concurrent safe point outcomes = %+v", outcomes)
			}
		})
	}
}

func TestConversationalCommittedPrefixCanBeReconstructedAfterStateOvertakesTrigger(t *testing.T) {
	reference := loadConversationalReference(t, "conversational-slow-only")
	defective := conversationalWithLossyContextEdge(t, reference, "fast_model")
	fastNodeGate := make(chan struct{})
	slowTerminalGate := make(chan struct{})
	var releaseFastNode, releaseSlow sync.Once
	control := &conversationalContinuationControl{
		nodeRunGates: map[string]<-chan struct{}{"fast_model": fastNodeGate},
		terminalGates: map[trajectory.Phase]<-chan struct{}{
			trajectory.PhaseSlow: slowTerminalGate,
		},
	}
	fixture := mountConversationalTurn(t, defective, control)
	defer func() {
		releaseFastNode.Do(func() { close(fastNodeGate) })
		releaseSlow.Do(func() { close(slowTerminalGate) })
		fixture.stop(t)
	}()

	driveConversationalGraphAudio(t, fixture)
	slowBegin := fixture.await(t, "slow_prepared_text", preparedBoundary(cognitionelements.TextBegin))
	fixture.await(t, "fast_activation_outcome", func(payload any) bool {
		outcome, ok := payload.(policyelements.GenerationOutcome)
		return ok && outcome.Kind == policyelements.GenerationEmitted && outcome.ContextVersion == 1
	})
	// The deliberately lossy edge is occupied by startup v0 while the fast
	// model is gated, so v1 is physically absent. Drain v0, let the slow result
	// advance State to v3, and require the compact v1 prefix identity carried by
	// the trigger to reconstruct the exact earlier prefix from that later State.
	releaseFastNode.Do(func() { close(fastNodeGate) })
	awaitConversationalContextQueueEmpty(t, fixture, "fast_model")
	releaseSlow.Do(func() { close(slowTerminalGate) })
	slowCommit := fixture.awaitModelCommit(t, "slow_commit_outcome", slowBegin.RunID)
	if slowCommit.Kind != interactionelements.ModelCommitted || slowCommit.StoreVersion != 3 {
		t.Fatalf("deliberative branch did not advance canonical context: %+v", slowCommit)
	}
	fastBegin := fixture.await(t, "fast_prepared_text", preparedBoundary(cognitionelements.TextBegin))
	fastRequest := control.request(t, trajectory.PhaseFast, 0)
	if fastRequest.Trajectory.Version != 1 || len(fastRequest.Trajectory.Items) != 1 {
		t.Fatalf("overtaken fast request sampled trajectory %+v", fastRequest.Trajectory)
	}
	fastCommit := fixture.awaitModelCommit(t, "fast_commit_outcome", fastBegin.RunID)
	if fastCommit.Kind != interactionelements.ModelRejected ||
		fastCommit.Code != "version_conflict" || fastCommit.StoreVersion != 3 {
		t.Fatalf("overtaken fast terminal = %+v", fastCommit)
	}
}

func awaitConversationalContextQueueEmpty(
	t *testing.T, fixture *conversationalTurnFixture, targetNode string,
) {
	t.Helper()
	edgeID := ""
	for _, edge := range fixture.reference.bound.Graph.Edges {
		if edge.From.Node == "snapshot_copy" && edge.From.Port == "out" &&
			edge.To.Node == targetNode && edge.To.Port == "context" {
			edgeID = edge.ID
			break
		}
	}
	if edgeID == "" {
		t.Fatalf("missing %s context queue", targetNode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		edge, found := fixture.mounted.Live().Edges[edgeID]
		if found && edge.Occupancy == 0 && edge.Dequeued > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s context queue did not drain startup state: %+v", targetNode, edge)
		}
		time.Sleep(time.Millisecond)
	}
}

func conversationalWithLossyContextEdge(
	t *testing.T, reference conversationalReference, targetNode string,
) conversationalReference {
	t.Helper()
	graph := reference.bound.Graph
	graph.Edges = append([]ir.Edge(nil), graph.Edges...)
	found := false
	for index := range graph.Edges {
		edge := &graph.Edges[index]
		if edge.From.Node == "snapshot_copy" && edge.From.Port == "out" &&
			edge.To.Node == targetNode && edge.To.Port == "context" {
			edge.Delivery = ir.Lossy
			found = true
		}
	}
	if !found {
		t.Fatalf("cannot construct lossy diagnostic: missing %s context edge", targetNode)
	}
	frozen, err := ir.Freeze(graph)
	if err != nil {
		t.Fatal(err)
	}
	reference.bound.Graph = frozen
	return reference
}

func loadConversationalTurnGolden(t *testing.T) map[string]conversationalTurnGoldenCase {
	t.Helper()
	payload := conversationalRead(t, filepath.Join("testdata", "conversational_complete_turns.json"))
	if len(payload) > 64<<10 {
		t.Fatalf("complete-turn regression artifact is %d bytes, limit is %d", len(payload), 64<<10)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var document conversationalTurnGolden
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode complete-turn regression artifact: %v", err)
	}
	if document.FormatVersion != 1 || len(document.Cases) != 3 {
		t.Fatalf("complete-turn regression artifact header = version %d cases %d",
			document.FormatVersion, len(document.Cases))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("complete-turn regression artifact has trailing data: %v", err)
	}
	result := make(map[string]conversationalTurnGoldenCase, len(document.Cases))
	for _, candidate := range document.Cases {
		if candidate.Profile == "" || candidate.GraphFingerprint == "" ||
			len(candidate.PlayedSpeech) == 0 || candidate.RejectionCode == "" {
			t.Fatalf("incomplete complete-turn regression case: %+v", candidate)
		}
		if _, duplicate := result[candidate.Profile]; duplicate {
			t.Fatalf("complete-turn regression artifact repeats %s", candidate.Profile)
		}
		result[candidate.Profile] = candidate
	}
	return result
}

func preparedBoundary(want cognitionelements.TextBoundary) func(any) bool {
	return func(payload any) bool {
		delta, ok := payload.(cognitionelements.PreparedTextDelta)
		return ok && delta.Boundary == want
	}
}

func loadConversationalReference(t *testing.T, name string) conversationalReference {
	t.Helper()
	directory := filepath.Join("components", name)
	topology := conversationalRead(t, filepath.Join(directory, "agent.ortg"))
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(conversationalRead(t, filepath.Join(directory, "openrealtime.lock")))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Lock.Equal(lock) {
		got, _ := updated.Lock.Marshal()
		want, _ := lock.Marshal()
		t.Fatalf("committed lock is stale\ngenerated:\n%scommitted:\n%s", got, want)
	}
	locked, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Graph.Fingerprint != updated.Graph.Fingerprint {
		t.Fatalf("locked fingerprint %s differs from generated %s",
			locked.Graph.Fingerprint, updated.Graph.Fingerprint)
	}
	if err := locked.Graph.Validate(); err != nil {
		t.Fatal(err)
	}

	values, err := graphvalues.ParseYAML("agent.values.yaml",
		conversationalRead(t, filepath.Join(directory, "agent.values.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(locked.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Graph.Fingerprint == locked.Graph.Fingerprint {
		t.Fatal("values binding did not change executable graph identity")
	}
	if err := bound.Graph.Validate(); err != nil {
		t.Fatal(err)
	}
	return conversationalReference{name: name, graph: locked.Graph, values: values, bound: bound}
}

func assertConversationalControlBoundaries(t *testing.T, reference conversationalReference) {
	t.Helper()
	want := map[string]string{
		"endpoint_tick":          "Trigger<timing.Tick>",
		"audio_cancel":           "Interrupt<audio.StreamID>",
		"fast_activation_cancel": "Interrupt<policy.GenerationAddress>",
		"slow_activation_cancel": "Interrupt<policy.GenerationAddress>",
		"fast_model_cancel":      "Interrupt<flow.RunID>",
		"slow_model_cancel":      "Interrupt<flow.RunID>",
		"fast_action_cancel":     "Interrupt<tool.CallID>",
		"slow_action_cancel":     "Interrupt<tool.CallID>",
		"fast_action_timeout":    "Interrupt<tool.CallID>",
		"slow_action_timeout":    "Interrupt<tool.CallID>",
		"segmentation_timeout":   "Event<interaction.Timeout>",
		"segmentation_cancel":    "Interrupt<flow.RunID>",
		"tts_cancel":             "Interrupt<speech.UtteranceID>",
		"playback_cancel":        "Interrupt<speech.UtteranceID>",
	}
	if reference.name == "conversational-both" {
		want["speech_selection"] = "Trigger<interaction.SpeechSelection>"
		want["arbitration_timeout"] = "Event<interaction.Timeout>"
		want["arbitration_cancel"] = "Interrupt<flow.RunID>"
	}
	boundaries := make(map[string]ir.Boundary, len(reference.graph.Boundaries))
	for _, boundary := range reference.graph.Boundaries {
		boundaries[boundary.Name] = boundary
	}
	for name, typeName := range want {
		boundary, found := boundaries[name]
		if !found {
			t.Errorf("missing explicit control boundary %s", name)
			continue
		}
		if boundary.Direction != ir.InputBoundary || boundary.Type.String() != typeName {
			t.Errorf("boundary %s = %s %s, want input %s",
				name, boundary.Direction, boundary.Type.String(), typeName)
		}
	}
	for name, wantType := range map[string]element.Type{
		"fast_prepared_text":                    interactionelements.SafePreparedTextType(),
		"slow_prepared_text":                    interactionelements.SafePreparedTextType(),
		"fast_result":                           interactionelements.SafeModelResultType(),
		"slow_result":                           interactionelements.SafeModelResultType(),
		"fast_control_serialization_quarantine": interactionelements.ControlSerializationQuarantineType(),
		"slow_control_serialization_quarantine": interactionelements.ControlSerializationQuarantineType(),
	} {
		boundary, found := boundaries[name]
		if !found {
			t.Errorf("missing quarantined model output boundary %s", name)
			continue
		}
		if boundary.Direction != ir.OutputBoundary || !boundary.Type.Equal(wantType) {
			t.Errorf("boundary %s = %s %s, want output %s",
				name, boundary.Direction, boundary.Type.String(), wantType.String())
		}
	}
	for _, connector := range []struct {
		id      string
		element string
	}{
		{id: "append_mux", element: "flow.Mux"},
		{id: "snapshot_copy", element: "flow.Tee"},
		{id: "observation_outcome_copy", element: "flow.Tee"},
		{id: "fast_tool_copy", element: "flow.Tee"},
		{id: "slow_tool_copy", element: "flow.Tee"},
		{id: "fast_action_join", element: "authority.ProvenanceJoin"},
		{id: "slow_action_join", element: "authority.ProvenanceJoin"},
		{id: "fast_control_quarantine", element: "interaction.ControlSerializationQuarantine"},
		{id: "slow_control_quarantine", element: "interaction.ControlSerializationQuarantine"},
		{id: "speech_cancel_copy", element: "flow.Tee"},
	} {
		if got := conversationalNodeElement(reference.graph, connector.id); got != connector.element {
			t.Errorf("connector %s = %q, want %q", connector.id, got, connector.element)
		}
	}
	assertConversationalActionProvenance(t, reference.graph)
}

func assertConversationalActionProvenance(t *testing.T, graph ir.Graph) {
	t.Helper()
	boundaries := make(map[string]ir.Boundary, len(graph.Boundaries))
	for _, boundary := range graph.Boundaries {
		boundaries[boundary.Name] = boundary
	}
	for _, role := range []string{"fast", "slow"} {
		join := role + "_action_join"
		for _, edge := range []struct {
			fromNode string
			fromPort string
			toNode   string
			toPort   string
		}{
			{role + "_activation", "authority", join, "candidate"},
			{role + "_model", "tools", role + "_tool_copy", "in"},
			{role + "_tool_copy", "out", join, "proposal"},
			{role + "_result_copy", "out", join, "result"},
		} {
			if !conversationalHasEdge(graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort) {
				t.Errorf("missing role-local action evidence edge %s.%s -> %s.%s",
					edge.fromNode, edge.fromPort, edge.toNode, edge.toPort)
			}
		}
		for _, boundaryName := range []string{
			role + "_tools", role + "_action_provenance_audit",
			role + "_action_join_outcome", role + "_action_join_resolution",
		} {
			if _, found := boundaries[boundaryName]; !found {
				t.Errorf("missing explicit role-local action boundary %s", boundaryName)
			}
		}
		other := "slow"
		if role == "slow" {
			other = "fast"
		}
		for _, edge := range graph.Edges {
			if edge.To.Node != join {
				continue
			}
			if strings.HasPrefix(edge.From.Node, other+"_") {
				t.Errorf("cross-role authority alias into %s: %s", join, edge.ID)
			}
		}
	}
	if got := boundaries["fast_action_provenance_audit"].Type.String(); got != actionelements.ProvenanceType().String() {
		t.Errorf("fast provenance audit type = %s, want %s", got, actionelements.ProvenanceType().String())
	}
}

func assertNoModelToModelEdges(t *testing.T, graph ir.Graph) {
	t.Helper()
	modelNodes := make(map[string]bool)
	for _, node := range graph.Nodes {
		if node.Element.Name == "cognition.TextModel" {
			modelNodes[node.ID] = true
		}
	}
	if len(modelNodes) != 2 || !modelNodes["fast_model"] || !modelNodes["slow_model"] {
		t.Fatalf("cognition model instances = %v, want independent fast_model and slow_model", modelNodes)
	}
	for _, edge := range graph.Edges {
		if modelNodes[edge.From.Node] && modelNodes[edge.To.Node] {
			t.Errorf("forbidden model-to-model edge %s", edge.ID)
		}
		if strings.HasPrefix(edge.From.Node, "fast_") && strings.HasPrefix(edge.To.Node, "slow_") ||
			strings.HasPrefix(edge.From.Node, "slow_") && strings.HasPrefix(edge.To.Node, "fast_") {
			t.Errorf("implicit cross-role handoff edge %s (%s -> %s)",
				edge.ID, edge.From.String(), edge.To.String())
		}
	}
	for _, edge := range []struct {
		fromNode string
		fromPort string
		toNode   string
		toPort   string
	}{
		{"fast_model", "text", "fast_control_quarantine", "text"},
		{"fast_model", "result", "fast_control_quarantine", "result"},
		{"slow_model", "text", "slow_control_quarantine", "text"},
		{"slow_model", "result", "slow_control_quarantine", "result"},
		{"fast_control_quarantine", "safe_text", "fast_text_copy", "in"},
		{"fast_control_quarantine", "safe_result", "fast_result_copy", "in"},
		{"slow_control_quarantine", "safe_text", "slow_text_copy", "in"},
		{"slow_control_quarantine", "safe_result", "slow_result_copy", "in"},
		{"fast_result_copy", "out", "fast_result_commit", "result"},
		{"slow_result_copy", "out", "slow_result_commit", "result"},
		{"append_mux", "out", "trajectory", "append"},
		{"snapshot_copy", "out", "fast_model", "context"},
		{"snapshot_copy", "out", "slow_model", "context"},
	} {
		if !conversationalHasEdge(graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort) {
			t.Errorf("missing explicit state/context edge %s.%s -> %s.%s",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort)
		}
	}
}

func assertSharedConversationalBackbone(
	t *testing.T, references map[string]conversationalReference,
) {
	t.Helper()
	fast := references["conversational-fast-only"]
	for _, name := range []string{"conversational-slow-only", "conversational-both"} {
		candidate := references[name]
		if got, want := conversationalCoreNodes(candidate.graph), conversationalCoreNodes(fast.graph); !equalStrings(got, want) {
			t.Errorf("%s core nodes drifted\ngot  %v\nwant %v", name, got, want)
		}
		if got, want := conversationalCoreEdges(candidate.graph), conversationalCoreEdges(fast.graph); !equalStrings(got, want) {
			t.Errorf("%s core edges drifted\ngot  %v\nwant %v", name, got, want)
		}
		for node, want := range fast.values.Nodes {
			if node == "arbiter" {
				continue
			}
			got, found := candidate.values.Nodes[node]
			if !found || !bytes.Equal(got, want) {
				t.Errorf("%s values for shared node %s differ: got %s want %s", name, node, got, want)
			}
		}
	}
}

func assertConversationalRoutingVariants(
	t *testing.T, references map[string]conversationalReference,
) {
	t.Helper()
	fast := references["conversational-fast-only"].graph
	if !conversationalHasEdge(fast, "fast_text_copy", "out", "segment", "text") ||
		!conversationalHasEdge(fast, "fast_outcome_copy", "out", "segment", "terminal") {
		t.Error("fast-only graph does not route fast text and terminal directly to segmentation")
	}
	if conversationalHasEdgeToPort(fast, "slow_text_copy", "segment", "text") ||
		conversationalNodeElement(fast, "arbiter") != "" {
		t.Error("fast-only graph grants an unintended slow or arbitration speech path")
	}

	slow := references["conversational-slow-only"].graph
	if !conversationalHasEdge(slow, "slow_text_copy", "out", "segment", "text") ||
		!conversationalHasEdge(slow, "slow_outcome_copy", "out", "segment", "terminal") {
		t.Error("slow-only graph does not route deliberative text and terminal directly to segmentation")
	}
	if conversationalHasEdgeToPort(slow, "fast_text_copy", "segment", "text") ||
		conversationalNodeElement(slow, "arbiter") != "" {
		t.Error("slow-only graph grants an unintended fast or arbitration speech path")
	}

	both := references["conversational-both"].graph
	if conversationalNodeElement(both, "arbiter") != "interaction.SpeechArbiter" {
		t.Fatal("both-speaking graph has no explicit stream-aware arbiter")
	}
	for _, source := range []string{"fast_text_copy", "slow_text_copy"} {
		if !conversationalHasEdge(both, source, "out", "arbiter", "text") {
			t.Errorf("both-speaking graph does not route %s through arbiter", source)
		}
		if conversationalHasEdgeToPort(both, source, "segment", "text") {
			t.Errorf("both-speaking graph bypasses arbiter from %s", source)
		}
	}
	if !conversationalHasEdge(both, "arbiter", "selected", "segment", "text") {
		t.Error("both-speaking graph does not route selected arbiter output to segmentation")
	}
	for _, graph := range []ir.Graph{fast, slow, both} {
		if !conversationalHasEdge(graph, "segment", "segments", "tts", "text") ||
			!conversationalHasEdge(graph, "tts", "audio", "playback", "audio") {
			t.Errorf("%s does not retain the typed segmentation -> TTS -> playback chain", graph.ID)
		}
	}
}

func conversationalCoreNodes(graph ir.Graph) []string {
	result := make([]string, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.ID == "arbiter" {
			continue
		}
		result = append(result, node.ID+"="+node.Element.Name)
	}
	sort.Strings(result)
	return result
}

func conversationalCoreEdges(graph ir.Graph) []string {
	result := make([]string, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		if edge.From.Node == "arbiter" || edge.To.Node == "arbiter" ||
			edge.To.Node == "segment" && (edge.To.Port == "text" || edge.To.Port == "terminal") {
			continue
		}
		result = append(result, strings.Join([]string{
			edge.From.Node, edge.From.Port, edge.To.Node, edge.To.Port,
			edge.Type.String(), string(edge.Delivery), strconv.Itoa(edge.Depth),
		}, "|"))
	}
	sort.Strings(result)
	return result
}

func conversationalHasEdge(
	graph ir.Graph, fromNode, fromPort, toNode, toPort string,
) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func conversationalHasEdgeToPort(graph ir.Graph, fromNode, toNode, toPort string) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func conversationalNodeElement(graph ir.Graph, id string) string {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node.Element.Name
		}
	}
	return ""
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func exerciseConversationalProviderBoundary(t *testing.T, reference conversationalReference) {
	t.Helper()
	services := graphruntime.NewServiceSet()
	asrProviders := perceptionelements.NewASRProviderRegistry()
	if err := asrProviders.Register("deployment.asr", conversationalASRDescriptor,
		func() (v1.PerceptionProvider, error) { return &conversationalASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	cognitionProviders := cognitionelements.NewProviderRegistry()
	for _, registration := range []struct {
		reference  string
		descriptor continuation.Descriptor
	}{
		{reference: "deployment.fast", descriptor: conversationalFastDescriptor},
		{reference: "deployment.slow", descriptor: conversationalSlowDescriptor},
	} {
		descriptor := registration.descriptor
		if err := cognitionProviders.Register(registration.reference, descriptor,
			func() (continuation.Provider, error) {
				return &conversationalContinuation{descriptor: descriptor}, nil
			}); err != nil {
			t.Fatal(err)
		}
	}
	ttsProviders := speechelements.NewTTSProviderRegistry()
	if err := ttsProviders.Register("deployment.tts", conversationalTTSDescriptor,
		func() (v1.SpeechProvider, error) { return &conversationalTTS{}, nil }); err != nil {
		t.Fatal(err)
	}
	playbackSinks := speechelements.NewPlaybackSinkRegistry()
	if err := playbackSinks.Register("deployment.playback", conversationalPlaybackDescriptor,
		func() (speechelements.PlaybackSink, error) { return &conversationalPlayback{}, nil }); err != nil {
		t.Fatal(err)
	}
	for name, service := range map[string]any{
		perceptionelements.ASRProviderRegistryService: asrProviders,
		cognitionelements.ProviderRegistryService:     cognitionProviders,
		speechelements.TTSProviderRegistryService:     ttsProviders,
		speechelements.PlaybackSinkRegistryService:    playbackSinks,
		speechelements.IrreversibilityLedgerService:   action.NewLedger(),
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: reference.bound.Graph, Registry: registry, Services: services,
		Values: reference.bound.Values, Now: func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(runContext) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("conversational graph stopped with %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("conversational graph did not stop")
		}
	}()

	if got := conversationalReceive(t, mounted, "asr_resolution").(perceptionelements.ProviderResolution).Reference; got != "deployment.asr" {
		t.Errorf("ASR resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "fast_model_resolution").(cognitionelements.ProviderResolution).Reference; got != "deployment.fast" {
		t.Errorf("fast model resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "slow_model_resolution").(cognitionelements.ProviderResolution).Reference; got != "deployment.slow" {
		t.Errorf("slow model resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "tts_resolution").(speechelements.ProviderResolution).Reference; got != "deployment.tts" {
		t.Errorf("TTS resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "playback_resolution").(speechelements.SinkResolution).Reference; got != "deployment.playback" {
		t.Errorf("playback resolution = %q", got)
	}
	for _, role := range []string{"fast", "slow"} {
		resolution := conversationalReceive(t, mounted, role+"_action_join_resolution").(actionelements.Resolution)
		if resolution.Stage != "provenance_join" || resolution.Identity != "authority.ProvenanceJoin" {
			t.Errorf("%s action provenance resolution = %+v", role, resolution)
		}
	}
	if snapshot := conversationalReceive(t, mounted, "trajectory_snapshot").(trajectory.Snapshot); snapshot.Version != 0 {
		t.Errorf("initial trajectory version = %d, want 0", snapshot.Version)
	}
}

type conversationalTurnEvent struct {
	name     string
	envelope element.Envelope
}

type conversationalTurnFixture struct {
	reference conversationalReference
	mounted   *graphruntime.Mounted
	context   context.Context
	cancel    context.CancelFunc
	done      chan error
	events    chan conversationalTurnEvent
	pending   map[string][]element.Envelope
	sink      *conversationalTurnPlayback
}

// conversationalContinuationControl is test-only orchestration for semantic
// parity fixtures. Requests are retained at the provider boundary (the exact
// immutable input), while terminal gates let a test choose one deterministic
// completion ordering without changing any emitted content.
type conversationalContinuationControl struct {
	mu            sync.Mutex
	requests      map[trajectory.Phase][]continuation.Request
	terminalGates map[trajectory.Phase]<-chan struct{}
	nodeRunGates  map[string]<-chan struct{}
}

type conversationalGatedFactory struct {
	base  element.Factory
	gates map[string]<-chan struct{}
}

func (factory conversationalGatedFactory) Descriptor() element.Descriptor {
	return factory.base.Descriptor()
}

func (factory conversationalGatedFactory) ValidateConfig(source json.RawMessage) error {
	validator, ok := factory.base.(element.ConfigValidator)
	if !ok {
		return nil
	}
	return validator.ValidateConfig(source)
}

func (factory conversationalGatedFactory) Mount(
	ctx context.Context, mount element.MountContext,
) (element.Runnable, error) {
	runnable, err := factory.base.Mount(ctx, mount)
	if err != nil {
		return nil, err
	}
	gate := factory.gates[mount.InstanceID]
	if gate == nil {
		return runnable, nil
	}
	return element.RunnableFunc(func(runCtx context.Context) error {
		select {
		case <-runCtx.Done():
			return nil
		case <-gate:
			return runnable.Run(runCtx)
		}
	}), nil
}

func (control *conversationalContinuationControl) record(request continuation.Request) {
	if control == nil {
		return
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.requests == nil {
		control.requests = make(map[trajectory.Phase][]continuation.Request)
	}
	control.requests[request.Descriptor.Phase] = append(
		control.requests[request.Descriptor.Phase], request,
	)
}

func (control *conversationalContinuationControl) request(
	t *testing.T, phase trajectory.Phase, index int,
) continuation.Request {
	t.Helper()
	control.mu.Lock()
	defer control.mu.Unlock()
	requests := control.requests[phase]
	if index < 0 || index >= len(requests) {
		t.Fatalf("%s continuation request %d is unavailable; retained %d", phase, index, len(requests))
	}
	return requests[index]
}

func mountConversationalTurn(
	t *testing.T, reference conversationalReference, controls ...*conversationalContinuationControl,
) *conversationalTurnFixture {
	t.Helper()
	if len(controls) > 1 {
		t.Fatalf("conversational turn accepts at most one continuation control, got %d", len(controls))
	}
	var control *conversationalContinuationControl
	if len(controls) == 1 {
		control = controls[0]
	}
	services := graphruntime.NewServiceSet()
	asrProviders := perceptionelements.NewASRProviderRegistry()
	if err := asrProviders.Register("deployment.asr", conversationalASRDescriptor,
		func() (v1.PerceptionProvider, error) { return &conversationalASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	cognitionProviders := cognitionelements.NewProviderRegistry()
	for _, registration := range []struct {
		reference  string
		descriptor continuation.Descriptor
	}{
		{reference: "deployment.fast", descriptor: conversationalFastDescriptor},
		{reference: "deployment.slow", descriptor: conversationalSlowDescriptor},
	} {
		descriptor := registration.descriptor
		if err := cognitionProviders.Register(registration.reference, descriptor,
			func() (continuation.Provider, error) {
				return &conversationalContinuation{descriptor: descriptor, control: control}, nil
			}); err != nil {
			t.Fatal(err)
		}
	}
	ttsProviders := speechelements.NewTTSProviderRegistry()
	if err := ttsProviders.Register("deployment.tts", conversationalTTSDescriptor,
		func() (v1.SpeechProvider, error) { return &conversationalTTS{}, nil }); err != nil {
		t.Fatal(err)
	}
	sink := &conversationalTurnPlayback{ended: make(chan struct{}, 8)}
	playbackSinks := speechelements.NewPlaybackSinkRegistry()
	if err := playbackSinks.Register("deployment.playback", conversationalPlaybackDescriptor,
		func() (speechelements.PlaybackSink, error) { return sink, nil }); err != nil {
		t.Fatal(err)
	}
	for name, service := range map[string]any{
		perceptionelements.ASRProviderRegistryService: asrProviders,
		cognitionelements.ProviderRegistryService:     cognitionProviders,
		speechelements.TTSProviderRegistryService:     ttsProviders,
		speechelements.PlaybackSinkRegistryService:    playbackSinks,
		speechelements.IrreversibilityLedgerService:   action.NewLedger(),
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := conversationalRuntimeRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: reference.bound.Graph, Registry: registry, Services: services,
		Values: reference.bound.Values, Now: func() uint64 { return now.Add(1_000_000) },
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	fixture := &conversationalTurnFixture{
		reference: reference, mounted: mounted, context: runContext, cancel: cancel,
		done: make(chan error, 1), events: make(chan conversationalTurnEvent, 8192),
		pending: make(map[string][]element.Envelope), sink: sink,
	}
	for _, boundary := range reference.bound.Graph.Boundaries {
		if boundary.Direction != ir.OutputBoundary {
			continue
		}
		port, portErr := mounted.Egress(boundary.Name)
		if portErr != nil {
			cancel()
			t.Fatal(portErr)
		}
		go func(name string, input element.InputPort) {
			for {
				envelope, receiveErr := input.Receive(runContext)
				if receiveErr != nil {
					return
				}
				select {
				case fixture.events <- conversationalTurnEvent{name: name, envelope: envelope}:
				case <-runContext.Done():
					return
				}
			}
		}(boundary.Name, port)
	}
	go func() { fixture.done <- mounted.Run(runContext) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		live := mounted.Live()
		ready := len(live.Nodes) == len(reference.bound.Graph.Nodes)
		for nodeID, node := range live.Nodes {
			_, gated := controlNodeRunGate(control, nodeID)
			ready = ready && (gated || node.Resolution != nil &&
				string(node.Resolution.RuntimeEvidence) == "live" &&
				string(node.Resolution.CapabilitiesEvidence) == "live")
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("conversational graph did not become ready: %+v", live.Nodes)
		}
		time.Sleep(time.Millisecond)
	}
	return fixture
}

func conversationalRuntimeRegistry(
	control *conversationalContinuationControl,
) (*graphruntime.Registry, error) {
	if control == nil || len(control.nodeRunGates) == 0 {
		return elements.RuntimeRegistry()
	}
	registrations, err := elements.FactoryRegistrations()
	if err != nil {
		return nil, err
	}
	registry := graphruntime.NewRegistry()
	for _, registration := range registrations {
		registration.Factory = conversationalGatedFactory{
			base: registration.Factory, gates: control.nodeRunGates,
		}
		if err := registry.RegisterFactory(registration); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func controlNodeRunGate(
	control *conversationalContinuationControl, node string,
) (<-chan struct{}, bool) {
	if control == nil {
		return nil, false
	}
	gate, found := control.nodeRunGates[node]
	return gate, found && gate != nil
}

func (fixture *conversationalTurnFixture) boundaryType(name string) element.Type {
	for _, boundary := range fixture.reference.bound.Graph.Boundaries {
		if boundary.Name == name && boundary.Direction == ir.InputBoundary {
			return boundary.Type.Clone()
		}
	}
	return element.Type{}
}

func (fixture *conversationalTurnFixture) send(
	t *testing.T, name string, envelope element.Envelope,
) {
	t.Helper()
	output, err := fixture.mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Fatalf("send %s: %v", name, err)
	}
}

func (fixture *conversationalTurnFixture) sendSelection(
	t *testing.T, runID string, mode interactionelements.SelectionMode, itemID string,
) {
	t.Helper()
	fixture.send(t, "speech_selection", element.Envelope{
		Type: fixture.boundaryType("speech_selection"), ItemID: itemID,
		SessionID: "session-reference", RunID: runID,
		Payload: interactionelements.SpeechSelection{RunID: runID, Mode: mode},
	})
}

func (fixture *conversationalTurnFixture) awaitModelCommit(
	t *testing.T, boundary, runID string,
) interactionelements.ModelCommitOutcome {
	t.Helper()
	envelope := fixture.await(t, boundary, func(payload any) bool {
		outcome, ok := payload.(interactionelements.ModelCommitOutcome)
		return ok && outcome.RunID == runID &&
			(outcome.Kind == interactionelements.ModelCommitted ||
				outcome.Kind == interactionelements.ModelRejected)
	})
	return envelope.Payload.(interactionelements.ModelCommitOutcome)
}

func (fixture *conversationalTurnFixture) await(
	t *testing.T, name string, match func(any) bool,
) element.Envelope {
	t.Helper()
	queued := fixture.pending[name]
	for index, envelope := range queued {
		if match(envelope.Payload) {
			fixture.pending[name] = append(queued[:index], queued[index+1:]...)
			return envelope
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		select {
		case event := <-fixture.events:
			if event.name == name && match(event.envelope.Payload) {
				return event.envelope
			}
			fixture.pending[event.name] = append(fixture.pending[event.name], event.envelope)
		case err := <-fixture.done:
			t.Fatalf("await %s: graph stopped: %v", name, err)
		case <-ctx.Done():
			t.Fatalf("await %s: %v; %s", name, ctx.Err(), fixture.timeoutDiagnostic())
		}
	}
}

func (fixture *conversationalTurnFixture) timeoutDiagnostic() string {
	pending := make(map[string][]string)
	for name, envelopes := range fixture.pending {
		if len(envelopes) == 0 {
			continue
		}
		values := make([]string, 0, len(envelopes))
		for _, envelope := range envelopes {
			values = append(values, conversationalDiagnosticPayload(envelope.Payload))
		}
		pending[name] = values
	}
	live := fixture.mounted.Live()
	nodes := make(map[string]inspect.NodeLive)
	for _, name := range []string{
		"observation_commit", "trajectory", "snapshot_copy",
		"fast_activation", "slow_activation", "fast_model", "slow_model",
		"fast_text_copy", "slow_text_copy",
	} {
		if node, found := live.Nodes[name]; found {
			nodes[name] = node
		}
	}
	edges := make(map[string]inspect.EdgeLive)
	for name, edge := range live.Edges {
		if edge.Occupancy != 0 || edge.Dropped != 0 || edge.Backpressure != 0 {
			edges[name] = edge
		}
	}
	return fmt.Sprintf("pending=%v; nodes=%+v; active_edges=%+v", pending, nodes, edges)
}

func conversationalDiagnosticPayload(payload any) string {
	switch value := payload.(type) {
	case policyelements.GenerationOutcome:
		return fmt.Sprintf("policy.Outcome{%s code=%s v=%d}", value.Kind, value.Code, value.ContextVersion)
	case policyelements.GenerationState:
		return fmt.Sprintf("policy.State{role=%s v=%d emitted=%d refused=%d canceled=%d ignored=%d}",
			value.Role, value.ContextVersion, value.Emitted, value.Refused, value.Canceled, value.Ignored)
	case cognitionelements.Outcome:
		return fmt.Sprintf("cognition.Outcome{%s code=%s v=%d}", value.Kind, value.Code, value.ContextVersion)
	case cognitionelements.PreparedTextDelta:
		return fmt.Sprintf("cognition.Text{%s index=%d interrupted=%t}",
			value.Boundary, value.Index, value.Interrupted)
	case cognitionelements.Result:
		return fmt.Sprintf("cognition.Result{v=%d interrupted=%t}", value.ContextVersion, value.Interrupted)
	case interactionelements.ModelCommitOutcome:
		return fmt.Sprintf("interaction.ModelCommit{%s code=%s v=%d}", value.Kind, value.Code, value.StoreVersion)
	case stateelements.ObservationCommitOutcome:
		return fmt.Sprintf("state.ObservationCommit{%s code=%s v=%d}", value.Kind, value.Code, value.StoreVersion)
	case trajectory.Snapshot:
		return fmt.Sprintf("trajectory.Snapshot{v=%d}", value.Version)
	default:
		return fmt.Sprintf("%T", payload)
	}
}

func (fixture *conversationalTurnFixture) stop(t *testing.T) {
	t.Helper()
	fixture.cancel()
	select {
	case err := <-fixture.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("conversational graph stopped with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("conversational graph did not stop")
	}
}

func conversationalAudioFrame(index uint64, amplitude int16) coreperception.Frame {
	const rate uint32 = 16_000
	const milliseconds = 20
	pcm := make([]byte, int(rate)*milliseconds/1_000*2)
	for offset := 0; offset < len(pcm); offset += 2 {
		binary.LittleEndian.PutUint16(pcm[offset:], uint16(amplitude))
	}
	return coreperception.Frame{
		Kind: coreperception.FrameAudio, Source: "microphone",
		CapturedNS: index * milliseconds * 1_000_000, Index: index,
		PCM16LE: pcm, SampleRateHz: rate,
	}
}

type conversationalTurnPlayback struct {
	mu     sync.Mutex
	begins []action.Utterance
	ends   []action.Outcome
	ended  chan struct{}
}

func (*conversationalTurnPlayback) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalPlaybackDescriptor)
}
func (sink *conversationalTurnPlayback) Begin(_ context.Context, utterance action.Utterance) error {
	sink.mu.Lock()
	sink.begins = append(sink.begins, utterance)
	sink.mu.Unlock()
	return nil
}
func (*conversationalTurnPlayback) Audio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (sink *conversationalTurnPlayback) End(
	_ context.Context, _ action.Utterance, outcome action.Outcome,
) error {
	sink.mu.Lock()
	sink.ends = append(sink.ends, outcome)
	sink.mu.Unlock()
	select {
	case sink.ended <- struct{}{}:
	default:
	}
	return nil
}
func (sink *conversationalTurnPlayback) texts() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	result := make([]string, len(sink.begins))
	for index := range sink.begins {
		result[index] = sink.begins[index].Text
	}
	return result
}

func conversationalReceive(t *testing.T, mounted *graphruntime.Mounted, name string) any {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Payload
}

func conversationalRead(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

var (
	conversationalASRDescriptor = v1.Descriptor{
		Name: "conversational-test-asr", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityRevisions:      true,
			v1.CapabilityCancellation:   true,
		},
	}
	conversationalFastDescriptor = continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
	conversationalSlowDescriptor = continuation.Descriptor{
		Provider: "test", Model: "deliberative", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
	conversationalTTSDescriptor = v1.Descriptor{
		Name: "conversational-test-tts", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output:  true,
			v1.CapabilityCancellation: true,
		},
	}
	conversationalPlaybackDescriptor = v1.Descriptor{
		Name: "conversational-test-playback", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityCancellation:   true,
		},
	}
)

type conversationalASR struct{}

func (*conversationalASR) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalASRDescriptor)
}
func (*conversationalASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (*conversationalASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{
		RevisionID: 1,
		StableText: "hello from reference",
		Final:      true,
	}, nil
}

type conversationalContinuation struct {
	descriptor continuation.Descriptor
	control    *conversationalContinuationControl
}

func (provider *conversationalContinuation) Descriptor() continuation.Descriptor {
	return provider.descriptor
}
func (provider *conversationalContinuation) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.control.record(request)
	text := "fast response."
	if provider.descriptor.Phase == trajectory.PhaseSlow {
		text = "slow response."
	}
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: text}); err != nil {
		return continuation.Completion{}, err
	}
	if provider.control != nil {
		if gate := provider.control.terminalGates[provider.descriptor.Phase]; gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return continuation.Completion{}, context.Cause(ctx)
			}
		}
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

type conversationalTTS struct{}

func (*conversationalTTS) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalTTSDescriptor)
}
func (*conversationalTTS) Synthesize(_ context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return []v1.SpeechChunk{{
		ChunkID: "chunk-" + plan.CandidateID, CandidateID: plan.CandidateID,
		SampleRateHz: 16_000, PCM16LE: []byte{1, 0}, Final: true,
	}}, nil
}

type conversationalPlayback struct{}

func (*conversationalPlayback) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalPlaybackDescriptor)
}
func (*conversationalPlayback) Begin(context.Context, action.Utterance) error { return nil }
func (*conversationalPlayback) Audio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (*conversationalPlayback) End(context.Context, action.Utterance, action.Outcome) error {
	return nil
}

func conversationalCloneDescriptor(source v1.Descriptor) v1.Descriptor {
	result := source
	result.Capabilities = make(v1.Capabilities, len(source.Capabilities))
	for capability, enabled := range source.Capabilities {
		result.Capabilities[capability] = enabled
	}
	return result
}
