package interaction

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestControlSerializationQuarantineSanitizesCompletedResultAndNativeReplayState(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	result := modelResult("sanitized-result", 0, "", false, false)
	const secret = "account-secret-123"
	assistantControl := `<tool_call>{"name":"lookup","arguments":{"token":"` + secret +
		`","note":"literal </tool_call> inside a string"}}</tool_call>`
	reasoningControl := "Tool call: {\"name\":\"lookup\",\"arguments\":{\"query\":\"hidden\"}}"
	result.Outputs = []cognitionelements.PreparedOutput{
		{Kind: cognitionelements.PreparedReasoning, Text: "Reason first. " + reasoningControl + " Continue."},
		{Kind: cognitionelements.PreparedAssistant, Text: "Answer first. " + assistantControl + " Done."},
	}
	result.ReasoningRetained = true
	result.ReasoningText = result.Outputs[0].Text
	result.AssistantText = result.Outputs[1].Text
	result.Completion.ProviderStateType = "test/native.v1"
	result.Completion.ProviderState = json.RawMessage(`{"assistant":"unsanitized secret state"}`)
	completeQuarantineTextRun(t, ingress(t, mounted, "text"), result.RunID)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("raw-result", result))

	safeEnvelope := receive(t, egress(t, mounted, "safe_result"))
	safe, ok := safeEnvelope.Payload.(cognitionelements.Result)
	if !ok {
		t.Fatalf("safe result payload = %T", safeEnvelope.Payload)
	}
	if safe.AssistantText != "Answer first.  Done." ||
		safe.ReasoningText != "Reason first.  Continue." {
		t.Fatalf("safe result text = assistant %q reasoning %q", safe.AssistantText, safe.ReasoningText)
	}
	if strings.Contains(safe.AssistantText+safe.ReasoningText, "tool_call") ||
		len(safe.Completion.ProviderState) != 0 || safe.Completion.ProviderStateType != "" {
		t.Fatalf("unsafe result state survived quarantine: %+v", safe)
	}
	if safeEnvelope.ItemID == "raw-result" ||
		!containsString(safeEnvelope.CausalParents, "raw-result") {
		t.Fatalf("safe result lost transformation causality: %+v", safeEnvelope)
	}

	for index := 0; index < 2; index++ {
		auditEnvelope := receive(t, egress(t, mounted, "quarantined"))
		audit, ok := auditEnvelope.Payload.(ControlSerializationQuarantine)
		if !ok || audit.RunID != result.RunID || audit.Bytes == 0 || len(audit.SHA256) != 64 ||
			audit.SourceItemID != "raw-result" || audit.BlockIndex != index+1 {
			t.Fatalf("quarantine audit %d = %#v in %+v", index, auditEnvelope.Payload, auditEnvelope)
		}
		encoded, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "hidden") {
			t.Fatalf("quarantine audit copied sensitive arguments: %s", encoded)
		}
	}
}

func TestControlSerializationQuarantinePreservesEnvelopeCardinalitySequenceAndTerminalText(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "safe-envelope-framing"
	input := []element.Envelope{
		rawPreparedEnvelope("raw-begin", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextBegin, Index: 0,
		}),
		rawPreparedEnvelope("raw-marker", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 1, Text: "<tool_call>",
		}),
		rawPreparedEnvelope("raw-control", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 2,
			Text: `{"name":"lookup","arguments":{"secret":"hidden"}}</tool_call>`,
		}),
		rawPreparedEnvelope("raw-ordinary", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 3, Text: "ordinary <tool",
		}),
		rawPreparedEnvelope("raw-end", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextEnd, Index: 4,
		}),
	}
	sequences := []uint64{7, 19, 23, 41, 97}
	for index := range input {
		input[index].Sequence = sequences[index]
		send(t, ingress(t, mounted, "text"), input[index])
	}

	want := []cognitionelements.PreparedTextDelta{
		{Boundary: cognitionelements.TextBegin, Index: 0},
		{Boundary: cognitionelements.TextChunk, Index: 1},
		{Boundary: cognitionelements.TextChunk, Index: 2},
		{Boundary: cognitionelements.TextChunk, Index: 3, Text: "ordinary "},
		{Boundary: cognitionelements.TextEnd, Index: 4, Text: "<tool"},
	}
	for index := range want {
		envelope := receive(t, egress(t, mounted, "safe_text"))
		delta, ok := envelope.Payload.(cognitionelements.PreparedTextDelta)
		if !ok || delta != want[index] || envelope.Sequence != sequences[index] ||
			!containsString(envelope.CausalParents, input[index].ItemID) {
			t.Fatalf("safe envelope %d = %+v payload %#v, want sequence %d payload %#v",
				index, envelope, envelope.Payload, sequences[index], want[index])
		}
	}
	audit := receive(t, egress(t, mounted, "quarantined")).Payload.(ControlSerializationQuarantine)
	if audit.Syntax != ControlSyntaxTagged || audit.Disposition != ControlSerializationComplete {
		t.Fatalf("quarantine audit = %+v", audit)
	}
	assertNoEnvelope(t, egress(t, mounted, "safe_text"))
}

func TestControlSerializationQuarantineDefersResultUntilSameRunTextEnd(t *testing.T) {
	for _, resultPosition := range []string{"before_begin", "during_stream"} {
		t.Run(resultPosition, func(t *testing.T) {
			mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
			defer stopInteractionGraph(t, done, cancel)

			runID := "deferred-result-" + resultPosition
			result := modelResult(runID, 0, "", false, false)
			result.Outputs[0].Text = "ordinary terminal text"
			result.AssistantText = result.Outputs[0].Text
			resultEnvelope := rawResultEnvelopeForTest("raw-result-"+resultPosition, result)
			text := ingress(t, mounted, "text")
			if resultPosition == "before_begin" {
				send(t, ingress(t, mounted, "result"), resultEnvelope)
				assertNoEnvelope(t, egress(t, mounted, "safe_result"))
			}
			send(t, text, rawPreparedEnvelope("begin-"+resultPosition, runID,
				cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}))
			send(t, text, rawPreparedEnvelope("chunk-"+resultPosition, runID,
				cognitionelements.PreparedTextDelta{
					Boundary: cognitionelements.TextChunk, Index: 1, Text: result.AssistantText,
				}))
			if resultPosition == "during_stream" {
				send(t, ingress(t, mounted, "result"), resultEnvelope)
			}
			assertNoEnvelope(t, egress(t, mounted, "safe_result"))
			send(t, text, rawPreparedEnvelope("end-"+resultPosition, runID,
				cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextEnd, Index: 2}))

			for index := uint64(0); index < 3; index++ {
				delta := receive(t, egress(t, mounted, "safe_text")).Payload.(cognitionelements.PreparedTextDelta)
				if delta.Index != index {
					t.Fatalf("safe text %d = %+v", index, delta)
				}
			}
			safe := receive(t, egress(t, mounted, "safe_result")).Payload.(cognitionelements.Result)
			if safe.RunID != runID || safe.AssistantText != result.AssistantText {
				t.Fatalf("deferred result = %+v", safe)
			}
		})
	}
}

func TestControlSerializationQuarantineClosesToolOnlyRunForSpeechLifecycle(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineCommitSpeechGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "tool-only-speechless-run"
	result := modelResult(runID, 0, "", false, true)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("tool-only-result", result))
	// A successful model terminal may overtake the independently drained safe
	// text lane. It is not sufficient evidence that a stream is speechless;
	// the quarantine's explicit empty begin/end framing closes that lifecycle.
	send(t, ingress(t, mounted, "terminal"), element.Envelope{
		Type: ModelOutcomeType(), ItemID: "tool-only-model-terminal", RunID: runID,
		CancellationScope: runID,
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
		},
	})

	outcomes := egress(t, mounted, "segment_outcome")
	var completed bool
	for range 2 {
		outcome := receive(t, outcomes).Payload.(SegmentationOutcome)
		switch outcome.Kind {
		case OutcomeCompleted:
			if outcome.RunID != runID || outcome.Segments != 0 || outcome.BufferedBytes != 0 {
				t.Fatalf("tool-only segmentation outcome = %+v", outcome)
			}
			completed = true
		case OutcomeIgnored:
			if outcome.RunID != runID || outcome.Code != "stream_end_authoritative" {
				t.Fatalf("tool-only racing terminal outcome = %+v", outcome)
			}
		default:
			t.Fatalf("tool-only unexpected segmentation outcome = %+v", outcome)
		}
	}
	if !completed {
		t.Fatal("tool-only safe framing did not complete segmentation")
	}
	assertNoEnvelope(t, egress(t, mounted, "segments"))
}

func TestControlSerializationQuarantineEmitsCanonicalEmptySafeStreamBeforeToolOnlyResult(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "tool-only-safe-framing"
	result := modelResult(runID, 0, "", false, true)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("tool-only-result", result))

	safeText := egress(t, mounted, "safe_text")
	var safeTextTerminalID string
	for index, boundary := range []cognitionelements.TextBoundary{
		cognitionelements.TextBegin, cognitionelements.TextEnd,
	} {
		envelope := receive(t, safeText)
		delta, ok := envelope.Payload.(cognitionelements.PreparedTextDelta)
		if !ok || envelope.RunID != runID || envelope.Sequence != uint64(index+1) ||
			delta.Boundary != boundary || delta.Index != uint64(index) || delta.Text != "" ||
			delta.Interrupted || !containsString(envelope.CausalParents, "tool-only-result") {
			t.Fatalf("empty safe framing %d = %+v payload %#v", index, envelope, envelope.Payload)
		}
		if boundary == cognitionelements.TextEnd {
			safeTextTerminalID = envelope.ItemID
		}
	}
	safeResult := receive(t, egress(t, mounted, "safe_result"))
	safe, ok := safeResult.Payload.(cognitionelements.Result)
	if !ok || safe.RunID != runID || safe.AssistantText != "" || len(safe.ToolProposals) != 1 ||
		safeTextTerminalID == "" ||
		!containsString(safeResult.CausalParents, safeTextTerminalID) {
		t.Fatalf("tool-only safe result = %+v payload %#v", safeResult, safeResult.Payload)
	}
	assertNoEnvelope(t, safeText)
}

func TestControlSerializationQuarantineBoundsResultsWaitingForText(t *testing.T) {
	runner := &controlSerializationQuarantineRunner{
		config: ControlSerializationQuarantineConfig{
			MaxCandidateBytes: 4096, MaxBlocks: 16, MaxActiveStreams: 1,
		},
		runs: make(map[string]*quarantinedRun),
	}
	first := modelResult("waiting-one", 0, "", false, false)
	if err := runner.acceptResult(context.Background(),
		rawResultEnvelopeForTest("waiting-one-result", first)); err != nil {
		t.Fatal(err)
	}
	second := modelResult("waiting-two", 0, "", false, false)
	err := runner.acceptResult(context.Background(),
		rawResultEnvelopeForTest("waiting-two-result", second))
	if err == nil || !strings.Contains(err.Error(), "exceeds 1 tracked runs") {
		t.Fatalf("second waiting result error = %v", err)
	}
}

func TestControlSerializationQuarantineCarriesParserStateAcrossPreparedOutputs(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	result := modelResult("split-control-result", 0, "", false, false)
	result.Outputs = []cognitionelements.PreparedOutput{
		{Kind: cognitionelements.PreparedAssistant, Text: "Speak first. <tool_"},
		{Kind: cognitionelements.PreparedReasoning, Text: "retained reasoning separator"},
		{Kind: cognitionelements.PreparedAssistant, Text: `call>{"name":"lookup","arguments":{"token":"split-secret"}}</tool_call> Done.`},
	}
	result.ReasoningRetained = true
	result.AssistantText = result.Outputs[0].Text + result.Outputs[2].Text
	result.ReasoningText = result.Outputs[1].Text
	result.Completion.ProviderStateType = "test/native.v1"
	result.Completion.ProviderState = json.RawMessage(`{"assistant":"split raw serialization"}`)
	completeQuarantineTextRun(t, ingress(t, mounted, "text"), result.RunID)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("split-raw-result", result))

	safe := receive(t, egress(t, mounted, "safe_result")).Payload.(cognitionelements.Result)
	if safe.AssistantText != "Speak first.  Done." ||
		safe.ReasoningText != "retained reasoning separator" || len(safe.Outputs) != 3 {
		t.Fatalf("split safe result = %+v", safe)
	}
	if safe.Outputs[0].Text != "Speak first. " ||
		safe.Outputs[1].Kind != cognitionelements.PreparedReasoning ||
		safe.Outputs[2].Text != " Done." {
		t.Fatalf("sanitization reordered text around the separator: %+v", safe.Outputs)
	}
	if len(safe.Completion.ProviderState) != 0 || safe.Completion.ProviderStateType != "" ||
		strings.Contains(safe.AssistantText, "split-secret") {
		t.Fatalf("split control or native replay state survived: %+v", safe)
	}
	audit := receive(t, egress(t, mounted, "quarantined")).Payload.(ControlSerializationQuarantine)
	if audit.Source != ControlSerializationAssistant || audit.OutputIndex != 0 ||
		audit.Syntax != ControlSyntaxTagged || audit.Disposition != ControlSerializationComplete {
		t.Fatalf("split quarantine audit = %+v", audit)
	}
	assertNoEnvelope(t, egress(t, mounted, "quarantined"))
}

func TestControlSerializationQuarantineCarriesParserStateAcrossToolOutput(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	result := modelResult("tool-separated-control", 0, "", false, true)
	toolOutput := result.Outputs[0]
	result.Outputs = []cognitionelements.PreparedOutput{
		{Kind: cognitionelements.PreparedAssistant, Text: "Before <tool_"},
		toolOutput,
		{Kind: cognitionelements.PreparedAssistant, Text: `call>{"name":"lookup","arguments":{"query":"hidden"}}</tool_call> after.`},
	}
	result.AssistantText = result.Outputs[0].Text + result.Outputs[2].Text
	result.Completion.ProviderStateType = "test/native.v1"
	result.Completion.ProviderState = json.RawMessage(`{"assistant":"raw split control"}`)
	completeQuarantineTextRun(t, ingress(t, mounted, "text"), result.RunID)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("tool-separated-raw", result))

	safe := receive(t, egress(t, mounted, "safe_result")).Payload.(cognitionelements.Result)
	if safe.AssistantText != "Before  after." || safe.ReasoningText != "" || len(safe.Outputs) != 3 ||
		safe.Outputs[0].Text != "Before " || safe.Outputs[1].Kind != cognitionelements.PreparedTool ||
		safe.Outputs[1].Proposal == nil || safe.Outputs[2].Text != " after." ||
		len(safe.ToolProposals) != 1 {
		t.Fatalf("tool-separated safe result = %+v", safe)
	}
	if len(safe.Completion.ProviderState) != 0 || safe.Completion.ProviderStateType != "" {
		t.Fatalf("tool-separated native replay state survived: %+v", safe.Completion)
	}
	audit := receive(t, egress(t, mounted, "quarantined")).Payload.(ControlSerializationQuarantine)
	if audit.Source != ControlSerializationAssistant || audit.OutputIndex != 0 ||
		audit.Syntax != ControlSyntaxTagged || audit.Disposition != ControlSerializationComplete {
		t.Fatalf("tool-separated quarantine audit = %+v", audit)
	}
	assertNoEnvelope(t, egress(t, mounted, "quarantined"))
}

func TestControlSerializationQuarantineDropsNativeStateWhenReasoningWasNotRetained(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	result := modelResult("hidden-reasoning-control", 0, "", false, false)
	result.Outputs[0].Text = "Okay."
	result.AssistantText = "Okay."
	result.ReasoningRetained = false
	result.Completion.ProviderStateType = "openai-compatible/state.v1"
	result.Completion.ProviderState = json.RawMessage(
		`{"messages":[{"role":"assistant","reasoning_content":"<tool_call>{\"name\":\"lookup\",\"arguments\":{\"token\":\"hidden-secret\"}}</tool_call>","content":"Okay."}]}`,
	)
	completeQuarantineTextRun(t, ingress(t, mounted, "text"), result.RunID)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("hidden-reasoning-raw", result))

	safe := receive(t, egress(t, mounted, "safe_result")).Payload.(cognitionelements.Result)
	if safe.AssistantText != "Okay." || safe.ReasoningText != "" || len(safe.Outputs) != 1 ||
		safe.Outputs[0].Kind != cognitionelements.PreparedAssistant ||
		safe.Outputs[0].Text != "Okay." {
		t.Fatalf("portable assistant projection changed: %+v", safe)
	}
	if safe.Completion.ProviderStateType != "" || len(safe.Completion.ProviderState) != 0 {
		t.Fatalf("uninspected hidden reasoning survived in native state: %+v", safe.Completion)
	}
	assertNoEnvelope(t, egress(t, mounted, "quarantined"))
}

func TestControlSerializationQuarantineDropsInterruptedNativeStateWithUndeliveredText(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	result := modelResult("interrupted-native-control", 0, "", true, false)
	result.Outputs[0].Text = "Visible prefix."
	result.AssistantText = "Visible prefix."
	result.ReasoningRetained = true
	result.Completion.ProviderStateType = "gemini/state.v1"
	result.Completion.ProviderState = json.RawMessage(
		`{"parts":[{"text":"Visible prefix.<tool_call>{\"name\":\"lookup\",\"arguments\":{\"token\":\"undelivered-secret\"}}</tool_call>"}]}`,
	)
	completeQuarantineTextRun(t, ingress(t, mounted, "text"), result.RunID)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("interrupted-native-raw", result))

	safe := receive(t, egress(t, mounted, "safe_result")).Payload.(cognitionelements.Result)
	if safe.AssistantText != "Visible prefix." || !safe.Interrupted ||
		safe.Completion.ProviderStateType != "" || len(safe.Completion.ProviderState) != 0 {
		t.Fatalf("interrupted native state was not removed: %+v", safe)
	}
	assertNoEnvelope(t, egress(t, mounted, "quarantined"))
}

func TestCompletedResultKeepsSplitMarkerDiscussionAtOriginalOutputPositions(t *testing.T) {
	source := []cognitionelements.PreparedOutput{
		{Kind: cognitionelements.PreparedAssistant, Text: "Discuss <tool_"},
		{Kind: cognitionelements.PreparedReasoning, Text: "reasoning stays between fragments"},
		{Kind: cognitionelements.PreparedAssistant, Text: "box> as ordinary prose."},
	}
	outputs, records, changed, err := sanitizeCompletedOutputs(source, 4096, 16)
	if err != nil {
		t.Fatal(err)
	}
	if changed || len(records) != 0 || len(outputs) != len(source) {
		t.Fatalf("ordinary split marker was quarantined: changed=%t records=%+v outputs=%+v",
			changed, records, outputs)
	}
	for index := range source {
		if outputs[index].Kind != source[index].Kind || outputs[index].Text != source[index].Text {
			t.Fatalf("output %d moved across separator: got %+v want %+v", index, outputs[index], source[index])
		}
	}
}

func TestControlSerializationQuarantinePrecedesCanonicalCommitAndSpeech(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(
		t, controlQuarantineCommitSpeechGraph,
		map[string]json.RawMessage{
			"segment": json.RawMessage(`{"minimum_runes":1}`),
		}, nil,
	)
	defer stopInteractionGraph(t, done, cancel)

	snapshots := egress(t, mounted, "snapshot")
	if initial := receive(t, snapshots).Payload.(trajectory.Snapshot); initial.Version != 0 {
		t.Fatalf("initial trajectory = %+v", initial)
	}

	const runID = "adversarial-control"
	serialized := `<tool_call>{"name":"track_order","arguments":{` +
		`"note":"escaped quote \\\" and literal </tool_call> stay in JSON",` +
		`"order_id":"SECRET88"}}</tool_call>`
	text := ingress(t, mounted, "text")
	send(t, text, rawPreparedEnvelope("raw-begin", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	for index := range serialized {
		send(t, text, rawPreparedEnvelope(
			"raw-byte-"+strconv.Itoa(index), runID,
			cognitionelements.PreparedTextDelta{
				Boundary: cognitionelements.TextChunk, Index: uint64(index + 1),
				Text: serialized[index : index+1],
			},
		))
	}
	send(t, text, rawPreparedEnvelope("raw-end", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: uint64(len(serialized) + 1),
	}))
	segmentOutcome := receiveSegmentationOutcome(
		t, egress(t, mounted, "segment_outcome"), OutcomeCompleted,
	)
	if segmentOutcome.RunID != runID || segmentOutcome.Segments != 0 ||
		segmentOutcome.BufferedBytes != 0 {
		t.Fatalf("quarantined segmentation outcome = %+v", segmentOutcome)
	}
	assertNoEnvelope(t, egress(t, mounted, "segments"))

	result := modelResult(runID, 0, "", false, false)
	result.Outputs[0].Text = serialized
	result.AssistantText = serialized
	result.Completion.ProviderStateType = "test/native.v1"
	result.Completion.ProviderState = json.RawMessage(`{"assistant":"raw tool serialization"}`)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("raw-complete-result", result))

	snapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
	if snapshot.Version != 1 || len(snapshot.Items) != 1 ||
		snapshot.Items[0].Kind != trajectory.KindInstruction {
		t.Fatalf("canonical trajectory retained quarantined assistant text: %+v", snapshot)
	}
	for _, item := range snapshot.Items {
		if strings.Contains(item.Content, "tool_call") || strings.Contains(item.Content, "SECRET88") ||
			len(item.ProviderState) != 0 {
			t.Fatalf("canonical item retained quarantined serialization: %+v", item)
		}
	}
	committed := receiveModelCommitKind(t, egress(t, mounted, "commit_outcome"), ModelCommitted)
	if committed.RunID != runID || committed.StoreVersion != 1 {
		t.Fatalf("sanitized result commit = %+v", committed)
	}

	for index := 0; index < 2; index++ {
		audit := receive(t, egress(t, mounted, "quarantined"))
		if _, executable := audit.Payload.(cognitionelements.ToolProposal); executable {
			t.Fatalf("quarantine audit acquired proposal authority: %+v", audit)
		}
		record, ok := audit.Payload.(ControlSerializationQuarantine)
		if !ok || record.RunID != runID || record.Syntax != ControlSyntaxTagged ||
			record.Disposition != ControlSerializationComplete {
			t.Fatalf("quarantine record %d = %#v", index, audit.Payload)
		}
	}
}

func TestControlSerializationQuarantineDoesNotReplayDivergentMatchingProviderState(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(
		t, controlQuarantineCommitSpeechGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":64}`)}, nil,
	)
	defer stopInteractionGraph(t, done, cancel)

	snapshots := egress(t, mounted, "snapshot")
	if initial := receive(t, snapshots).Payload.(trajectory.Snapshot); initial.Version != 0 {
		t.Fatalf("initial trajectory = %+v", initial)
	}
	const runID = "divergent-native-replay"
	const portable = "A harmless portable answer."
	for _, envelope := range rawPreparedStream(runID, portable) {
		send(t, ingress(t, mounted, "text"), envelope)
	}
	segment := receive(t, egress(t, mounted, "segments")).Payload.(speech.TextSegment)
	if segment.Text != portable {
		t.Fatalf("portable speech = %+v", segment)
	}
	_ = receiveSegmentationOutcome(t, egress(t, mounted, "segment_outcome"), OutcomeCompleted)

	result := modelResult(runID, 0, "", false, false)
	result.Outputs[0].Text = portable
	result.AssistantText = portable
	result.ReasoningRetained = true
	result.Descriptor.NativeStateType = "test/native.v1"
	result.Completion.ProviderStateType = result.Descriptor.NativeStateType
	result.Completion.ProviderState = json.RawMessage(
		`{"role":"assistant","content":"A harmless portable answer.","tool_calls":[{"name":"hidden","arguments":{"secret":"native-only"}}]}`,
	)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("divergent-native-result", result))

	snapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
	if snapshot.Version != 2 || len(snapshot.Items) != 2 ||
		snapshot.Items[1].Kind != trajectory.KindAssistant || snapshot.Items[1].Content != portable {
		t.Fatalf("committed portable trajectory = %+v", snapshot)
	}
	for _, item := range snapshot.Items {
		if item.ProviderStateType != "" || len(item.ProviderState) != 0 ||
			strings.Contains(string(item.ProviderState), "native-only") {
			t.Fatalf("divergent provider replay state survived: %+v", item)
		}
	}
	committed := receiveModelCommitKind(t, egress(t, mounted, "commit_outcome"), ModelCommitted)
	if committed.RunID != runID || committed.StoreVersion != 2 {
		t.Fatalf("divergent native result commit = %+v", committed)
	}
}

func TestControlSerializationQuarantineLeavesUnambiguousSyntaxDiscussionUntouched(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, controlQuarantineGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	const runID = "ordinary-discussion"
	const ordinary = "The <tool_call> tag wraps a JSON object. Tool call: is a label, not a request."
	for _, envelope := range rawPreparedStream(runID, ordinary) {
		send(t, ingress(t, mounted, "text"), envelope)
	}
	var rebuilt strings.Builder
	for index := 0; index < 3; index++ {
		envelope := receive(t, egress(t, mounted, "safe_text"))
		delta := envelope.Payload.(cognitionelements.PreparedTextDelta)
		if delta.Index != uint64(index) {
			t.Fatalf("safe text index %d = %+v", index, delta)
		}
		rebuilt.WriteString(delta.Text)
	}
	if rebuilt.String() != ordinary {
		t.Fatalf("ordinary discussion changed to %q", rebuilt.String())
	}
	assertNoEnvelope(t, egress(t, mounted, "quarantined"))

	result := modelResult(runID+"-result", 0, "", false, false)
	result.Outputs[0].Text = ordinary
	result.AssistantText = ordinary
	result.ReasoningRetained = true
	result.Completion = continuation.Completion{
		StopReason: "stop", ProviderStateType: "test/native.v1",
		ProviderState: json.RawMessage(`{"cursor":1}`),
	}
	completeQuarantineTextRun(t, ingress(t, mounted, "text"), result.RunID)
	send(t, ingress(t, mounted, "result"), rawResultEnvelopeForTest("ordinary-result", result))
	safe := receive(t, egress(t, mounted, "safe_result")).Payload.(cognitionelements.Result)
	if safe.AssistantText != ordinary || safe.Completion.ProviderStateType != "" ||
		len(safe.Completion.ProviderState) != 0 {
		t.Fatalf("ordinary result changed: %+v", safe)
	}
	assertNoEnvelope(t, egress(t, mounted, "quarantined"))
}

func completeQuarantineTextRun(t *testing.T, text element.OutputPort, runID string) {
	t.Helper()
	send(t, text, rawPreparedEnvelope(runID+"-empty-begin", runID,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}))
	send(t, text, rawPreparedEnvelope(runID+"-empty-end", runID,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextEnd, Index: 1}))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
