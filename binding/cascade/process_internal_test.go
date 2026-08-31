package cascade

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestEarlierIdenticalPendingCallOnlySuppressesAnUnresolvedDuplicate(t *testing.T) {
	pending := trajectory.Item{
		ID: "tool-item-1", Kind: trajectory.KindToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "call-1", Name: "meeting.analyze_launch_review",
			Arguments: json.RawMessage(`{"section":"risks"}`),
		},
	}
	snapshot := trajectory.Snapshot{Items: []trajectory.Item{pending}}

	duplicate := trajectory.ToolCall{
		CallID: "call-2", Name: "meeting.analyze_launch_review",
		Arguments: json.RawMessage(`{"section":"risks"}`),
	}
	if got := earlierIdenticalPendingCall(snapshot, duplicate); got != "call-1" {
		t.Fatalf("duplicate resolved to %q, want call-1", got)
	}

	// Redispatching the authoritative call itself is not a second model
	// decision and must not be converted into an already-in-flight result.
	if got := earlierIdenticalPendingCall(snapshot, *pending.ToolCall); got != "" {
		t.Fatalf("the same call ID was treated as duplicate of %q", got)
	}

	resolved := snapshot
	resolved.Items = append(resolved.Items, trajectory.Item{
		ID: "tool-result-1", Kind: trajectory.KindToolResult,
		ToolResult: &trajectory.ToolResult{
			CallID: "call-1", Name: "meeting.analyze_launch_review",
			Output: json.RawMessage(`{"status":"complete"}`),
		},
	})
	if got := earlierIdenticalPendingCall(resolved, duplicate); got != "" {
		t.Fatalf("completed call still suppressed a later request as %q", got)
	}

	differentArguments := duplicate
	differentArguments.Arguments = json.RawMessage(`{"section":"summary"}`)
	if got := earlierIdenticalPendingCall(snapshot, differentArguments); got != "" {
		t.Fatalf("different arguments were treated as duplicate of %q", got)
	}
}

func TestParallelVisualObservationRequiresCachedUserAuthority(t *testing.T) {
	policies := interaction.Defaults()
	// The method under test must not invoke the model: observer frames can only
	// reuse a decision already derived from user words. A non-nil zero value is
	// therefore sufficient to select the interaction-policy code path.
	policies.Interaction = new(interaction.InteractionModel)
	runtime := &runtime{
		policies:         policies,
		visualPolicy:     make(map[string]interaction.VisualIntent),
		visualPolicyTask: make(map[string]string),
		visualEvaluated:  make(map[string]bool),
		visualArmed:      make(map[string]bool),
	}
	const intent = "utterance-1"
	const task = "Go to the Summary."
	if runtime.parallelVisualIntentAuthorized(context.Background(), intent, task) {
		t.Fatal("an unclassified observer frame created visual authority")
	}
	runtime.visualPolicy[intent] = interaction.VisualIntentNone
	runtime.visualPolicyTask[intent] = task
	if runtime.parallelVisualIntentAuthorized(context.Background(), intent, task) {
		t.Fatal("a denied task retained visual authority")
	}
	runtime.visualPolicy[intent] = interaction.VisualIntentDirect
	if !runtime.parallelVisualIntentAuthorized(context.Background(), intent, task) {
		t.Fatal("an exact cached direct-visible decision was not reusable by a frame")
	}
	if runtime.parallelVisualIntentAuthorized(context.Background(), intent, "Go to the Overview.") {
		t.Fatal("authority for stale user words leaked into a changed task")
	}
}

func TestCompositeVoiceExcludesOnlyItsOwnFreshStandingPolicy(t *testing.T) {
	board := &interaction.Pinboard{}
	board.SetForTurn(1, []interaction.StandingInstruction{{
		Text: "count each animal", Scope: interaction.ScopeConversation,
	}})
	board.SetForTurn(2, []interaction.StandingInstruction{{
		Text: "acknowledge a deployment alert if it appears", Scope: interaction.ScopeConversation,
	}})
	runtime := &runtime{
		pinboard: board, scheduler: clock.NewManual(uint64(3 * time.Second)), extractTurn: 2,
		pinnedFromText: "Present the launch overview. If a deployment alert appears, acknowledge it.",
	}
	current := board.Lines(runtime.scheduler.NowNS())
	filtered := runtime.standingExceptCurrentComposite(
		current,
		"Present the launch overview if a deployment alert appears, acknowledge it.",
	)
	if len(filtered) != 1 || !strings.Contains(filtered[0], "count each animal") {
		t.Fatalf("composite standing filter = %v", filtered)
	}
	unchanged := runtime.standingExceptCurrentComposite(current, "Go to the Summary slide.")
	if len(unchanged) != len(current) {
		t.Fatalf("unrelated visual task removed standing policy: %v", unchanged)
	}
}

func TestVisualTargetValidationAcceptsOrdinarySingularPluralInflection(t *testing.T) {
	request := cognition.Request{VisualTask: "Switch to the risk slide now."}
	outcome := cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAct, Target: "Risks"}
	if err := validateVisualTarget(request, outcome); err != nil {
		t.Fatalf("ordinary target inflection was rejected: %v", err)
	}
	request.VisualTask = "Switch to the Summary slide now."
	if err := validateVisualTarget(request, outcome); err == nil {
		t.Fatal("an unrelated target was accepted as inflection")
	}
}

func TestVisualTargetValidationRequiresTheNextUnfulfilledAction(t *testing.T) {
	request := cognition.Request{
		VisualTask:             "Open the launch review, share your screen, and tell everyone the latest conversion rate.",
		CompletedVisualActions: 1,
	}
	stale := cognition.VisualReflexOutcome{
		Kind: cognition.VisualReflexAct, Target: "Open launch review",
	}
	if err := validateVisualTarget(request, stale); err == nil {
		t.Fatal("the completed first target was accepted for the second action chunk")
	}
	next := cognition.VisualReflexOutcome{
		Kind: cognition.VisualReflexAct, Target: "Share screen",
	}
	if err := validateVisualTarget(request, next); err != nil {
		t.Fatalf("the next requested target was rejected: %v", err)
	}
	provisional := cognition.Request{VisualTask: "Open the large.", VisualUnstable: "large"}
	wrongAutocompletion := cognition.VisualReflexOutcome{
		Kind: cognition.VisualReflexAct, Target: "Open launch review",
	}
	if err := validateVisualTarget(provisional, wrongAutocompletion); err == nil {
		t.Fatal("a shared UI verb accepted an autocompleted provisional target")
	}
}

func TestObservationPrioritiesKeepUsersOrderedAndObserversParallel(t *testing.T) {
	user := perception.Observation{Authority: trajectory.AuthorityUser}
	if got := observationPriority(user); got != eventloop.PriorityRoutine {
		t.Fatalf("user observation priority = %q, want routine", got)
	}
	observer := perception.Observation{Authority: trajectory.AuthorityObserver}
	if got := observationPriority(observer); got != eventloop.PriorityParallel {
		t.Fatalf("observer observation priority = %q, want parallel", got)
	}
}

func TestVisualRevisionDedupeSeparatesAcousticAndGroundedCounters(t *testing.T) {
	runtime := &runtime{visualHandledRev: make(map[string]visualHandledRevisions)}
	const intentID = "monitor-deployment-alert"

	// ASR and screen narration own independent revision counters. A completed
	// acoustic prefix at revision 8 must not suppress the later alert frame just
	// because that observer is only at its own revision 2.
	runtime.markVisualRevisionHandled(intentID, 8, false)
	if !runtime.visualRevisionHandled(intentID, 8, false) {
		t.Fatal("the completed acoustic revision was not retained")
	}
	if runtime.visualRevisionHandled(intentID, 2, true) {
		t.Fatal("an acoustic revision suppressed an unrelated grounded frame")
	}

	runtime.markVisualRevisionHandled(intentID, 2, true)
	if !runtime.visualRevisionHandled(intentID, 2, true) {
		t.Fatal("the completed grounded revision was not retained")
	}
	if runtime.visualRevisionHandled(intentID, 9, false) {
		t.Fatal("a grounded revision advanced the independent acoustic counter")
	}
}

func TestVisualIntentFallbackCompilesExplicitFutureMonitor(t *testing.T) {
	runtime := &runtime{
		config:   Config{RequireExplicitVisualAuthority: true},
		policies: interaction.Defaults(),
	}
	task := "Present the launch overview. If a deployment alert appears, acknowledge it immediately without stopping your presentation."
	if got := runtime.visualInteractionIntent(t.Context(), "meeting-turn", task, true); got != interaction.VisualIntentMonitor {
		t.Fatalf("fallback visual intent = %q, want %q", got, interaction.VisualIntentMonitor)
	}
	if got := runtime.visualInteractionIntent(t.Context(), "meeting-turn", "Go to Overview.", true); got != interaction.VisualIntentDirect {
		t.Fatalf("fallback direct visual intent = %q, want %q", got, interaction.VisualIntentDirect)
	}
	if got := runtime.visualInteractionIntent(
		t.Context(), "meeting-turn", "Analyze the launch review in the background.", true,
	); got != interaction.VisualIntentNone {
		t.Fatalf("semantic-only fallback visual intent = %q, want %q", got, interaction.VisualIntentNone)
	}
}

func TestStrictVisualAuthorityEnablesLiveCompilerWithoutALearnedPolicy(t *testing.T) {
	compatibility := &runtime{policies: interaction.Defaults()}
	if compatibility.liveVisualIntentPolicyEnabled() {
		t.Fatal("the compatibility fallback unexpectedly enabled partial-speech visual control")
	}
	strict := &runtime{
		config:   Config{RequireExplicitVisualAuthority: true},
		policies: interaction.Defaults(),
	}
	if !strict.liveVisualIntentPolicyEnabled() {
		t.Fatal("strict compiler authority did not enable partial-speech visual control")
	}
}

func TestEarlierIdenticalCompletedVisualCallRequiresFreshIntentOrAVisualCycle(t *testing.T) {
	instruction := trajectory.Item{
		ID: "first-instruction", Kind: trajectory.KindInstruction, InvocationID: "visual-1",
		Content: "visual policy\n\nThey are still speaking. What they have said so far in this sentence, which is not yet in the conversation above, is: \"go to the summary slide\"\n\nAct silently.",
	}
	call := trajectory.Item{
		ID: "first-call", Kind: trajectory.KindToolCall, SourceRevision: 1,
		InvocationID: "visual-1",
		Producer:     trajectory.Producer{Phase: trajectory.PhaseFast},
		ToolCall:     &trajectory.ToolCall{CallID: "click-1", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":517,"y":829}`)},
	}
	result := trajectory.Item{
		ID: "first-result", Kind: trajectory.KindToolResult,
		ToolResult: &trajectory.ToolResult{CallID: "click-1", Name: "computer.click_normalized", Output: json.RawMessage(`{"ok":true}`)},
	}
	postActionFrame := trajectory.Item{
		ID: "post-action", Kind: trajectory.KindObservation, SourceRevision: 2,
		Producer:    trajectory.Producer{Phase: trajectory.PhaseObserver},
		Observation: &trajectory.ObservationMeta{Observer: "video", Authority: trajectory.AuthorityObserver, Media: []trajectory.MediaRef{{Handle: "frame-2", MIMEType: "image/jpeg", Source: "screen"}}},
	}
	candidate := trajectory.ToolCall{CallID: "click-2", Name: call.ToolCall.Name, Arguments: json.RawMessage(`{"x":517,"y":829}`)}
	base := trajectory.Snapshot{Items: []trajectory.Item{instruction, call, result, postActionFrame}}
	intent := map[string]string{"click-1": "utterance-1"}
	if got := earlierIdenticalCompletedVisualCall(base, candidate, "utterance-1", intent); got != "click-1" {
		t.Fatalf("post-action frame re-executed completed click; duplicate = %q", got)
	}

	// A final ASR revision replacing the partial which caused the click is the
	// same request, even though it lands after the tool result.
	finalRevision := trajectory.Item{
		ID: "final-user", Kind: trajectory.KindObservation, SourceRevision: 3,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "go to the summary slide and begin presenting",
		Observation: &trajectory.ObservationMeta{Observer: "audio", Authority: trajectory.AuthorityUser},
		Event:       &trajectory.EventMetadata{EventID: "final", Type: "observation", Source: "audio", Channel: "audio", SupersedesRevision: 1},
	}
	sameUtterance := base
	sameUtterance.Items = append(append([]trajectory.Item(nil), base.Items...), finalRevision)
	if got := earlierIdenticalCompletedVisualCall(sameUtterance, candidate, "utterance-1", intent); got != "click-1" {
		t.Fatalf("final ASR revision incorrectly re-armed the same click: %q", got)
	}
	finalWithoutCanonicalPartial := finalRevision
	finalWithoutCanonicalPartial.Event = &trajectory.EventMetadata{EventID: "final-only", Type: "observation", Source: "audio", Channel: "audio"}
	finalWithoutCanonicalPartial.Content = "go to the summary slide and begin presenting"
	finalOnly := base
	finalOnly.Items = append(append([]trajectory.Item(nil), base.Items...), finalWithoutCanonicalPartial)
	if got := earlierIdenticalCompletedVisualCall(finalOnly, candidate, "utterance-1", intent); got != "click-1" {
		t.Fatalf("final extension of a live partial incorrectly re-armed the same click: %q", got)
	}

	newRequest := finalRevision
	newRequest.ID = "new-user"
	newRequest.SourceRevision = 4
	newRequest.Content = "go to the summary slide again"
	newRequest.Event = &trajectory.EventMetadata{EventID: "new", Type: "observation", Source: "audio", Channel: "audio"}
	withNewRequest := base
	withNewRequest.Items = append(append([]trajectory.Item(nil), base.Items...), newRequest)
	if got := earlierIdenticalCompletedVisualCall(withNewRequest, candidate, "utterance-2", intent); got != "" {
		t.Fatalf("new user request did not re-arm the same coordinate: %q", got)
	}

	reappeared := postActionFrame
	reappeared.ID = "condition-reappeared"
	reappeared.SourceRevision = 5
	reappeared.Observation = &trajectory.ObservationMeta{Observer: "video", Authority: trajectory.AuthorityObserver, Media: []trajectory.MediaRef{{Handle: "frame-3", MIMEType: "image/jpeg", Source: "screen"}}}
	withVisualCycle := base
	withVisualCycle.Items = append(append([]trajectory.Item(nil), base.Items...), reappeared)
	if got := earlierIdenticalCompletedVisualCall(withVisualCycle, candidate, "utterance-1", intent); got != "click-1" {
		t.Fatalf("visual feedback incorrectly created fresh user authority: %q", got)
	}
}

func TestSameToolArgumentsIgnoresJSONMemberOrder(t *testing.T) {
	if !sameToolArguments("meeting.lookup", json.RawMessage(`{"source":"screen","x":517,"y":827}`), json.RawMessage(`{"x":517,"y":827,"source":"screen"}`)) {
		t.Fatal("equivalent tool arguments with a different member order were not equal")
	}
}

func TestSameToolArgumentsTreatsNearbyGroundingAsTheSameControl(t *testing.T) {
	if !sameToolArguments("computer.click", json.RawMessage(`{"source":"screen","x":137,"y":633}`), json.RawMessage(`{"x":142,"y":637,"source":"screen"}`)) {
		t.Fatal("small coordinate jitter was treated as a new control")
	}
	if sameToolArguments("computer.click", json.RawMessage(`{"source":"screen","x":137,"y":633}`), json.RawMessage(`{"x":117,"y":849,"source":"screen"}`)) {
		t.Fatal("distinct controls were collapsed")
	}
	if !sameToolArguments("computer.click_normalized",
		json.RawMessage(`{"source":"screen","x":106,"y":846}`),
		json.RawMessage(`{"source":"screen","x":145,"y":844}`)) {
		t.Fatal("normalized jitter repeated a completed control")
	}
	if sameToolArguments("computer.click_normalized",
		json.RawMessage(`{"source":"screen","x":106,"y":846}`),
		json.RawMessage(`{"source":"screen","x":152,"y":630}`)) {
		t.Fatal("distinct normalized controls were collapsed")
	}
}

func TestSameClickArgumentsNormalizesDefaultOptionalMembers(t *testing.T) {
	plain := json.RawMessage(`{"source":"screen","x":166,"y":625}`)
	decorated := json.RawMessage(`{"source":"screen","x":146,"y":627,"button":"left","type":"click"}`)
	if !sameToolArguments("computer.click", plain, decorated) {
		t.Fatal("default click decoration and coordinate jitter were treated as a new effect")
	}
	if sameToolArguments("computer.click", plain,
		json.RawMessage(`{"source":"screen","x":146,"y":627,"button":"right","type":"click"}`)) {
		t.Fatal("a right click was collapsed into the default left click")
	}
	if sameToolArguments("computer.move", plain, decorated) {
		t.Fatal("click-only normalization weakened identity for another action")
	}
}

func TestEarlierIdenticalCompletedVisualCallDoesNotHideAFailedAction(t *testing.T) {
	snapshot := trajectory.Snapshot{Items: []trajectory.Item{
		{ID: "call", Kind: trajectory.KindToolCall, ToolCall: &trajectory.ToolCall{CallID: "click-1", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":10,"y":20}`)}},
		{ID: "result", Kind: trajectory.KindToolResult, ToolResult: &trajectory.ToolResult{CallID: "click-1", Name: "computer.click_normalized", Output: json.RawMessage(`{"error":"target moved"}`)}},
	}}
	candidate := trajectory.ToolCall{CallID: "click-2", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":10,"y":20}`)}
	if got := earlierIdenticalCompletedVisualCall(snapshot, candidate, "utterance-1", map[string]string{"click-1": "utterance-1"}); got != "" {
		t.Fatalf("failed action was treated as completed by %q", got)
	}
}

func TestCompletedVisualActionsForIntentCountsOnlyRealSuccessfulEffects(t *testing.T) {
	runtime := &runtime{visualIntentByCall: map[string]string{
		"open": "utterance-1", "duplicate": "utterance-1", "failed": "utterance-1", "other": "utterance-2",
	}}
	call := func(id string) trajectory.Item {
		return trajectory.Item{
			ID: "call-" + id, Kind: trajectory.KindToolCall, Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{CallID: id, Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":10,"y":20}`)},
		}
	}
	result := func(id, output, failure string) trajectory.Item {
		return trajectory.Item{ID: "result-" + id, Kind: trajectory.KindToolResult,
			ToolResult: &trajectory.ToolResult{CallID: id, Name: "computer.click_normalized", Output: json.RawMessage(output), Error: failure}}
	}
	snapshot := trajectory.Snapshot{Items: []trajectory.Item{
		call("open"), result("open", `"clicked"`, ""),
		call("duplicate"), result("duplicate", `{"status":"already_completed"}`, ""),
		call("failed"), result("failed", `{"error":"target moved"}`, ""),
		call("other"), result("other", `"clicked"`, ""),
	}}
	if got := runtime.completedVisualActionsForIntent(snapshot, "utterance-1"); got != 1 {
		t.Fatalf("completed action chunks = %d, want 1", got)
	}
	if got := runtime.completedVisualActionsForIntent(snapshot, "utterance-2"); got != 1 {
		t.Fatalf("other intent action chunks = %d, want 1", got)
	}
}

func TestPrepareVisualRequestBindsNextChunkAndCurrentIntentHistory(t *testing.T) {
	runtime := &runtime{visualIntentByCall: map[string]string{
		"open": "utterance-1", "summary": "utterance-2",
	}}
	snapshot := trajectory.Snapshot{Items: []trajectory.Item{
		{
			ID: "open-call", Kind: trajectory.KindToolCall,
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{
				CallID: "open", Name: "computer.click_normalized",
				Arguments: json.RawMessage(`{"source":"screen","x":120,"y":840}`),
			},
		},
		{
			ID: "open-result", Kind: trajectory.KindToolResult,
			ToolResult: &trajectory.ToolResult{
				CallID: "open", Name: "computer.click_normalized", Output: json.RawMessage(`"clicked"`),
			},
		},
	}}
	request := cognition.Request{
		VisualIntentID: "utterance-1",
		VisualTask:     "Open the launch review, share your screen, and report the metric.",
	}
	runtime.prepareVisualRequest(snapshot, &request)
	if request.CompletedVisualActions != 1 || request.NextVisualAction != "share your screen" {
		t.Fatalf("prepared progress = %d next %q", request.CompletedVisualActions, request.NextVisualAction)
	}
	if !slices.Equal(request.CurrentVisualCallIDs, []string{"open"}) {
		t.Fatalf("prepared call IDs = %v, want only current intent", request.CurrentVisualCallIDs)
	}
}

func TestPureSuccessfulVisualResultBatchIsControllerMemory(t *testing.T) {
	runtime := &runtime{visualIntentByCall: map[string]string{"click-1": "utterance-1"}}
	batch := eventloop.Batch{
		Items: []trajectory.Item{{
			Kind: trajectory.KindToolResult,
			ToolResult: &trajectory.ToolResult{
				CallID: "click-1", Name: "computer.click_normalized", Output: json.RawMessage(`"clicked"`),
			},
		}},
		Events: []eventloop.Event{{Kind: trajectory.KindToolResult}},
	}
	if !runtime.batchOnlyVisualToolResults(batch) || !batchToolResultsSucceeded(batch) {
		t.Fatal("successful visual-only result was not classified as controller memory")
	}
	mixed := batch
	mixed.Events = append(mixed.Events, eventloop.Event{Kind: eventloop.KindSignal})
	if runtime.batchOnlyVisualToolResults(mixed) {
		t.Fatal("a merged signal was hidden as a pure visual-result batch")
	}
	failed := batch
	failed.Items = []trajectory.Item{{
		Kind: trajectory.KindToolResult,
		ToolResult: &trajectory.ToolResult{
			CallID: "click-1", Name: "computer.click_normalized", Error: "target moved",
		},
	}}
	if batchToolResultsSucceeded(failed) {
		t.Fatal("failed visual action was classified as successful controller memory")
	}
}

func TestCompositeEndpointSuppressionRequiresCoveredClauseAndActiveSpeech(t *testing.T) {
	runtime := &runtime{
		visualIntentByCall: map[string]string{"visual-1": "utterance-1"},
	}
	runtime.markCompositeResumeSpoken(
		"utterance-1",
		"Present the overview. If an alert appears, acknowledge it without stopping your press.",
		[]string{"assistant-1"},
	)
	if !runtime.compositeResumeAlreadySpokeFor(
		"utterance-1",
		"Present the overview. If an alert appears, acknowledge it without stopping your presentation.",
	) {
		t.Fatal("a provisional conditional-tail correction lost immediate-clause speech coverage")
	}
	if runtime.compositeResumeAlreadySpokeFor(
		"utterance-1",
		"Present the overview and summarize risks. If an alert appears, acknowledge it without stopping your presentation.",
	) {
		t.Fatal("a new immediate semantic obligation was hidden by earlier composite speech")
	}
	played := &trajectory.AssistantState{
		AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityPlayed, PlayedAudioMS: 1000,
	}
	queued := &trajectory.AssistantState{
		AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityQueued,
	}
	batch := eventloop.Batch{
		Items: []trajectory.Item{
			{
				Kind: trajectory.KindAssistantState, AssistantState: queued,
			},
			{
				Kind: trajectory.KindAssistantState, AssistantState: played,
			},
			{
				Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
				Content:     "Present the overview. If an alert appears, acknowledge it without stopping your presentation.",
				Observation: &trajectory.ObservationMeta{Authority: trajectory.AuthorityUser},
			},
		},
		Events: []eventloop.Event{
			{Kind: trajectory.KindAssistantState, AssistantState: queued},
			{Kind: trajectory.KindAssistantState, AssistantState: played},
			{Kind: trajectory.KindObservation},
			{Kind: eventloop.KindSignal, Type: interaction.SignalEscalated},
		},
	}
	if !runtime.batchOnlyCompositeEndpointArtifacts(batch, []string{"assistant-1"}) {
		t.Fatal("played composite speech plus its endpoint was not recognized")
	}
	userOnly := eventloop.Batch{
		Items:  append([]trajectory.Item(nil), batch.Items[2]),
		Events: append([]eventloop.Event(nil), batch.Events[2]),
	}
	if !runtime.batchOnlyCompositeEndpointArtifacts(userOnly, []string{"assistant-1"}) {
		t.Fatal("an endpoint arriving while covered speech was already queued was not recognized")
	}
	cancelled := batch
	cancelled.Items = append([]trajectory.Item(nil), batch.Items...)
	cancelled.Events = append([]eventloop.Event(nil), batch.Events...)
	cancelledState := &trajectory.AssistantState{
		AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityCancelled,
	}
	cancelled.Items[1].AssistantState = cancelledState
	cancelled.Events[1].AssistantState = cancelledState
	if runtime.batchOnlyCompositeEndpointArtifacts(cancelled, []string{"assistant-1"}) {
		t.Fatal("cancelled composite speech suppressed the canonical endpoint")
	}
	unmatchedQueued := batch
	unmatchedQueued.Items = append([]trajectory.Item(nil), batch.Items...)
	unmatchedQueued.Events = append([]eventloop.Event(nil), batch.Events...)
	unmatchedQueued.Items[0].AssistantState = &trajectory.AssistantState{
		AssistantItemID: "assistant-other", Visibility: trajectory.VisibilityQueued,
	}
	unmatchedQueued.Events[0].AssistantState = unmatchedQueued.Items[0].AssistantState
	if runtime.batchOnlyCompositeEndpointArtifacts(unmatchedQueued, []string{"assistant-1"}) {
		t.Fatal("unmatched queued speech suppressed the canonical endpoint")
	}
	unrelated := batch
	unrelated.Items = append(append([]trajectory.Item(nil), batch.Items...), trajectory.Item{
		Kind: trajectory.KindToolResult,
		ToolResult: &trajectory.ToolResult{
			CallID: "semantic-1", Name: "meeting.lookup", Output: json.RawMessage(`{"ok":true}`),
		},
	})
	if runtime.batchOnlyCompositeEndpointArtifacts(unrelated, []string{"assistant-1"}) {
		t.Fatal("unrelated semantic work was hidden with a composite endpoint")
	}
	unrelatedSignal := batch
	unrelatedSignal.Events = append([]eventloop.Event(nil), batch.Events...)
	unrelatedSignal.Events[len(unrelatedSignal.Events)-1].Type = interaction.SignalBackgroundResult
	if runtime.batchOnlyCompositeEndpointArtifacts(unrelatedSignal, []string{"assistant-1"}) {
		t.Fatal("an unrelated signal was hidden with a composite endpoint")
	}
}

func TestCompositeSpeechCoverageFollowsCanonicalAssistantVisibility(t *testing.T) {
	runtime := &runtime{}
	runtime.markCompositeResumeSpoken(
		"utterance-1",
		"Present the overview. If an alert appears, acknowledge it without stopping your presentation.",
		[]string{"assistant-1"},
	)
	task := "Present the overview. If an alert appears, acknowledge it without stopping your presentation."
	assistant := trajectory.Item{
		ID: "assistant-1", Kind: trajectory.KindAssistant, Visibility: trajectory.VisibilityPrepared,
		Content: "Here is the overview.", Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
	}
	queued := trajectory.Item{
		ID: "queued", Kind: trajectory.KindAssistantState,
		AssistantState: &trajectory.AssistantState{
			AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityQueued,
		},
	}
	queuedSnapshot := trajectory.Snapshot{Items: []trajectory.Item{assistant, queued}}
	if ids, active := runtime.activeCompositeResumeSpeechFor(
		queuedSnapshot, "utterance-1", task,
	); !active || !slices.Equal(ids, []string{"assistant-1"}) {
		t.Fatalf("queued composite speech was not active coverage: %q, %t", ids, active)
	}
	cancelled := trajectory.Item{
		ID: "cancelled", Kind: trajectory.KindAssistantState,
		AssistantState: &trajectory.AssistantState{
			AssistantItemID: "assistant-1", Visibility: trajectory.VisibilityCancelled,
		},
	}
	cancelledSnapshot := queuedSnapshot
	cancelledSnapshot.Items = append(append([]trajectory.Item(nil), queuedSnapshot.Items...), cancelled)
	if _, active := runtime.activeCompositeResumeSpeechFor(
		cancelledSnapshot, "utterance-1", task,
	); active {
		t.Fatal("cancelled composite speech still covered the canonical endpoint")
	}
}

func TestCompositeSpeechCoverageUsesLedgerBeforeVisibilityCommits(t *testing.T) {
	ledger := action.NewLedger()
	runtime := &runtime{ledger: ledger}
	runtime.markCompositeResumeSpoken(
		"utterance-1",
		"Present the overview. If an alert appears, acknowledge it without stopping your presentation.",
		[]string{"assistant-1"},
	)
	task := "Present the overview. If an alert appears, acknowledge it without stopping your presentation."
	preparedOnly := trajectory.Snapshot{Items: []trajectory.Item{{
		ID: "assistant-1", Kind: trajectory.KindAssistant, Visibility: trajectory.VisibilityPrepared,
		Content: "Here is the overview.", Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
	}}}
	if err := ledger.Prepare(action.Commitment{
		ID: "speech-1", Kind: action.KindSpeech, AssistantItemIDs: []string{"assistant-1"},
	}); err != nil {
		t.Fatalf("prepare speech coverage: %v", err)
	}
	if err := ledger.Queue("speech-1"); err != nil {
		t.Fatalf("queue speech coverage: %v", err)
	}
	if ids, active := runtime.activeCompositeResumeSpeechFor(
		preparedOnly, "utterance-1", task,
	); !active || !slices.Equal(ids, []string{"assistant-1"}) {
		t.Fatalf("ledger queue did not cover pre-visibility endpoint: %q, %t", ids, active)
	}
	if _, err := ledger.Cancel("speech-1", "user resumed"); err != nil {
		t.Fatalf("cancel queued speech: %v", err)
	}
	if _, active := runtime.activeCompositeResumeSpeechFor(
		preparedOnly, "utterance-1", task,
	); active {
		t.Fatal("cancelled ledger commitment still covered the endpoint")
	}
}

func TestExplicitVisualActionBudgetWaitsForAnIncompleteSecondCommand(t *testing.T) {
	for _, task := range []string{
		"Open the launch review share.",
		"Open the launch review, share your.",
	} {
		request := cognition.Request{VisualTask: task, CompletedVisualActions: 1}
		if !explicitVisualActionsComplete(request) {
			t.Fatalf("completed first action did not gate incomplete task %q", task)
		}
	}
	complete := cognition.Request{
		VisualTask: "Open the launch review, share your screen.", CompletedVisualActions: 1,
	}
	if explicitVisualActionsComplete(complete) {
		t.Fatal("a completed second screen command did not reopen the actor budget")
	}
	complete.CompletedVisualActions = 2
	if !explicitVisualActionsComplete(complete) {
		t.Fatal("two successful chunks did not exhaust two explicit actions")
	}
}

func TestVisualReplanStateRequiresAnInitialFrameAndStopsAfterATerminalChunk(t *testing.T) {
	runtime := &runtime{
		visualArmed: make(map[string]bool), visualEvaluated: make(map[string]bool),
	}
	if !runtime.visualIntentEligible("request-1") {
		t.Fatal("a user task whose first frame has not arrived was not eligible")
	}
	runtime.applyVisualOutcome("request-1", cognition.VisualReflexOutcome{
		Kind: cognition.VisualReflexAct, Continue: false,
	})
	if runtime.visualIntentEligible("request-1") {
		t.Fatal("terminal action left autonomous post-action replanning armed")
	}
	runtime.applyVisualOutcome("request-2", cognition.VisualReflexOutcome{
		Kind: cognition.VisualReflexAct, Continue: true,
	})
	if !runtime.visualIntentEligible("request-2") {
		t.Fatal("an ordered action with a remaining visible control did not re-arm")
	}
	runtime.applyVisualOutcome("request-2", cognition.VisualReflexOutcome{Kind: cognition.VisualReflexWait})
	if !runtime.visualIntentEligible("request-2") {
		t.Fatal("a conditional visual WAIT did not retain monitoring")
	}
}

func TestVisualTaskJoinsRecognitionTailButStartsANewDelayedCorrection(t *testing.T) {
	growthRuntime := &runtime{
		visualArmed: make(map[string]bool), visualEvaluated: make(map[string]bool),
	}
	user := func(id, correlation, text string, at uint64) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindObservation, Content: text, MonotonicNS: at,
			Producer:    trajectory.Producer{Phase: trajectory.PhaseUser},
			Observation: &trajectory.ObservationMeta{Authority: trajectory.AuthorityUser},
			Event:       &trajectory.EventMetadata{CorrelationID: correlation},
		}
	}
	firstAt := uint64(time.Second)
	first := user("first", "utterance-1", "Go to the Summary slide.", firstAt)
	intent, task := growthRuntime.visualTask(trajectory.Snapshot{Items: []trajectory.Item{first}}, true)
	if intent != "utterance-1" || task != first.Content {
		t.Fatalf("first visual task = %q %q", intent, task)
	}
	growthRuntime.applyVisualOutcome(intent, cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAbstain})
	if growthRuntime.visualIntentEligible(intent) {
		t.Fatal("terminal first fragment remained eligible before new user words")
	}
	extended := first
	extended.Content = "Go to the Summary slide and then share the screen."
	intent, task = growthRuntime.visualTask(trajectory.Snapshot{Items: []trajectory.Item{extended}}, true)
	if task != extended.Content || !growthRuntime.visualIntentEligible(intent) {
		t.Fatalf("same ASR utterance growth did not reopen one decision: %q eligible=%t",
			task, growthRuntime.visualIntentEligible(intent))
	}
	growthRuntime.applyVisualOutcome(intent, cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAbstain})

	// Use a fresh controller for the independent adjacent-fragment case. The
	// same-ID growth above deliberately changed the task to include sharing;
	// carrying that state into this case would test a synthetic mixed utterance.
	runtime := &runtime{
		visualArmed: make(map[string]bool), visualEvaluated: make(map[string]bool),
	}
	intent, task = runtime.visualTask(trajectory.Snapshot{Items: []trajectory.Item{first}}, true)
	runtime.applyVisualOutcome(intent, cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAbstain})

	// The next acoustic stretch began 400 ms after the prior committed piece:
	// it is a recognition-created tail of the same controller task.
	runtime.speechStartNS = firstAt + uint64(400*time.Millisecond)
	tail := user("tail", "utterance-2", "and begin presenting.", runtime.speechStartNS+1)
	joined := trajectory.Snapshot{Items: []trajectory.Item{first, tail}}
	intent, task = runtime.visualTask(joined, true)
	if intent != "utterance-1" || task != "Go to the Summary slide. and begin presenting." {
		t.Fatalf("recognition tail was not joined: %q %q", intent, task)
	}
	if !runtime.visualIntentEligible(intent) {
		t.Fatal("new recognition tail did not reopen one visual decision")
	}

	// A later correction begins outside the continuation horizon and therefore
	// gets fresh authority even though it refers to the same screen.
	runtime.speechStartNS = tail.MonotonicNS + uint64(3*time.Second)
	correction := user("correction", "utterance-3", "Go back to Overview.", runtime.speechStartNS+1)
	corrected := trajectory.Snapshot{Items: []trajectory.Item{first, tail, correction}}
	intent, task = runtime.visualTask(corrected, true)
	if intent != "utterance-3" || task != correction.Content {
		t.Fatalf("delayed correction inherited stale intent: %q %q", intent, task)
	}
}

func TestLiveVisualTaskCannotRegressBehindANewerASRRevision(t *testing.T) {
	runtime := &runtime{
		visualArmed: make(map[string]bool), visualEvaluated: make(map[string]bool),
	}
	intent, task := runtime.liveVisualTask(
		trajectory.Snapshot{}, "utterance-1", "Open the launch review and share your screen.", 0,
	)
	if intent != "utterance-1" || task != "Open the launch review and share your screen." {
		t.Fatalf("newest live task = %q %q", intent, task)
	}
	intent, task = runtime.liveVisualTask(
		trajectory.Snapshot{}, "utterance-1", "Open the launch review.", 0,
	)
	if intent != "utterance-1" || task != "Open the launch review and share your screen." {
		t.Fatalf("older queued revision regressed live task = %q %q", intent, task)
	}
}

func TestCanonicalVisualTaskAcceptsWithinWordASRCompletion(t *testing.T) {
	runtime := &runtime{
		visualTaskID: "utterance-1", visualTaskRawID: "utterance-1",
		visualTaskText: "Wait, go back to the over.",
		visualArmed:    map[string]bool{}, visualEvaluated: map[string]bool{},
		visualProvisionalTerminal: map[string]bool{},
	}
	final := trajectory.Item{
		ID: "final", Kind: trajectory.KindObservation,
		Content: "Wait, go back to the overview.", SourceRevision: 2,
		Producer:    trajectory.Producer{Phase: trajectory.PhaseUser},
		Observation: &trajectory.ObservationMeta{Authority: trajectory.AuthorityUser},
		Event:       &trajectory.EventMetadata{CorrelationID: "utterance-1"},
	}
	intent, task := runtime.visualTask(trajectory.Snapshot{Items: []trajectory.Item{final}}, true)
	if intent != "utterance-1" || task != final.Content {
		t.Fatalf("within-word final did not complete visual task: %q %q", intent, task)
	}
}

func TestCanonicalVisualTaskAcceptsOnlyEndpointFinalWordCorrections(t *testing.T) {
	newRuntime := func() *runtime {
		return &runtime{
			visualTaskID: "utterance-1", visualTaskRawID: "utterance-1",
			visualTaskText: "Present the overview without stopping your press.",
			visualArmed:    map[string]bool{}, visualEvaluated: map[string]bool{},
			visualProvisionalTerminal: map[string]bool{},
		}
	}
	observation := func(eventType string) trajectory.Item {
		return trajectory.Item{
			ID: "settled", Kind: trajectory.KindObservation,
			Content: "Present the overview without stopping your presentation.", SourceRevision: 2,
			Producer:    trajectory.Producer{Phase: trajectory.PhaseUser},
			Observation: &trajectory.ObservationMeta{Authority: trajectory.AuthorityUser},
			Event: &trajectory.EventMetadata{
				CorrelationID: "utterance-1", Type: eventType,
			},
		}
	}
	partialRuntime := newRuntime()
	_, partialTask := partialRuntime.visualTask(
		trajectory.Snapshot{Items: []trajectory.Item{observation("audio.partial")}}, true,
	)
	if partialTask != "Present the overview without stopping your press." {
		t.Fatalf("provisional rewrite replaced controller state: %q", partialTask)
	}
	endpointRuntime := newRuntime()
	_, endpointTask := endpointRuntime.visualTask(
		trajectory.Snapshot{Items: []trajectory.Item{observation("audio.endpoint")}}, true,
	)
	if endpointTask != "Present the overview without stopping your presentation." {
		t.Fatalf("canonical final-word correction was not accepted: %q", endpointTask)
	}
}

func TestLiveVisualTaskStartsANewDelayedCorrectionAfterACompletedAction(t *testing.T) {
	firstAt := uint64(time.Second)
	first := trajectory.Item{
		ID: "summary-request", Kind: trajectory.KindObservation,
		Content: "Go to the Summary slide and begin presenting.", MonotonicNS: firstAt,
		Producer:    trajectory.Producer{Phase: trajectory.PhaseUser},
		Observation: &trajectory.ObservationMeta{Authority: trajectory.AuthorityUser},
		Event:       &trajectory.EventMetadata{CorrelationID: "utterance-1"},
	}
	call := trajectory.Item{
		ID: "summary-call", Kind: trajectory.KindToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "click-summary", Name: "computer.click_normalized",
			Arguments: json.RawMessage(`{"source":"screen","x":180,"y":620}`),
		},
	}
	result := trajectory.Item{
		ID: "summary-result", Kind: trajectory.KindToolResult,
		ToolResult: &trajectory.ToolResult{
			CallID: "click-summary", Name: "computer.click_normalized",
			Output: json.RawMessage(`{"status":"clicked"}`),
		},
	}
	runtime := &runtime{
		visualTaskID: "utterance-1", visualTaskText: first.Content,
		visualIntentByCall: map[string]string{"click-summary": "utterance-1"},
		visualArmed:        map[string]bool{}, visualEvaluated: map[string]bool{"utterance-1": true},
	}
	runtime.speechStartNS = firstAt + uint64(3*time.Second)
	snapshot := trajectory.Snapshot{Items: []trajectory.Item{first, call, result}}

	intent, task := runtime.liveVisualTask(
		snapshot, "utterance-2", "Wait, go back to the Overview.", 0,
	)
	if intent != "utterance-2" || task != "Wait, go back to the Overview." {
		t.Fatalf("new correction inherited the completed Summary task: %q %q", intent, task)
	}
	if !runtime.visualIntentEligible(intent) {
		t.Fatal("new correction did not receive fresh visual authority")
	}

	intent, task = runtime.liveVisualTask(
		snapshot, "utterance-2", "Wait, go back to the Overview slide now.", 0,
	)
	if intent != "utterance-2" || task != "Wait, go back to the Overview slide now." {
		t.Fatalf("same correction ASR growth did not stay within its new task: %q %q", intent, task)
	}
}

func TestLiveVisualTaskSplitsALongHeldPauseEvenWhenTheAcousticIDDoesNotChange(t *testing.T) {
	runtime := &runtime{
		visualTaskID: "utterance-1", visualTaskRawID: "utterance-1",
		visualTaskText:       "Go to the Summary slide and begin presenting.",
		visualTaskObservedNS: uint64(time.Second),
		visualArmed:          map[string]bool{},
		visualEvaluated:      map[string]bool{"utterance-1": true},
	}
	observed := uint64(4 * time.Second)
	intent, task := runtime.liveVisualTask(
		trajectory.Snapshot{}, "utterance-1", "Wait, go back to the Overview.", observed,
	)
	if intent == "utterance-1" || !strings.HasPrefix(intent, "utterance-1/visual-") {
		t.Fatalf("held acoustic ID did not receive new controller lineage: %q", intent)
	}
	if task != "Wait, go back to the Overview." {
		t.Fatalf("held-pause correction inherited old task text: %q", task)
	}
	if !runtime.visualIntentEligible(intent) {
		t.Fatal("held-pause correction did not receive fresh effect authority")
	}

	intent2, task2 := runtime.liveVisualTask(
		trajectory.Snapshot{}, "utterance-1", "Wait, go back to the Overview slide.",
		observed+uint64(200*time.Millisecond),
	)
	if intent2 != intent || task2 != "Wait, go back to the Overview slide." {
		t.Fatalf("same correction did not keep its controller lineage: %q %q", intent2, task2)
	}
}

func TestLiveVisualTaskKeepsCumulativeTextAfterASlowRecognitionGap(t *testing.T) {
	runtime := &runtime{
		visualTaskID: "utterance-1", visualTaskRawID: "utterance-1",
		visualTaskText:       "Go to the Summary.",
		visualTaskObservedNS: uint64(time.Second),
		visualArmed:          map[string]bool{}, visualEvaluated: map[string]bool{},
	}
	intent, task := runtime.liveVisualTask(
		trajectory.Snapshot{}, "utterance-1",
		"Go to the Summary slide and begin presenting.", uint64(4*time.Second),
	)
	if intent != "utterance-1" || task != "Go to the Summary slide and begin presenting." {
		t.Fatalf("slow cumulative recognition created a false controller boundary: %q %q", intent, task)
	}
}

func TestLiveVisualTaskRejectsAStaleWithinWordRegression(t *testing.T) {
	runtime := &runtime{
		visualTaskID: "utterance-1", visualTaskRawID: "utterance-1",
		visualTaskText:       "Wait, go back to the Overview.",
		visualTaskObservedNS: uint64(time.Second),
		visualArmed:          map[string]bool{}, visualEvaluated: map[string]bool{},
	}
	intent, task := runtime.liveVisualTask(
		trajectory.Snapshot{}, "utterance-1", "Wait, go back to the over.",
		uint64(1200*time.Millisecond),
	)
	if intent != "utterance-1" || task != "Wait, go back to the Overview." {
		t.Fatalf("stale ASR tail regressed controller task: %q %q", intent, task)
	}
}

func TestVisualTargetMustBeNamedByTheCurrentTask(t *testing.T) {
	request := cognition.Request{
		VisualTask: "Wait, go back to the over.", VisualUnstable: "over.",
	}
	wrong := cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAct, Target: "Summary"}
	if err := validateVisualTarget(request, wrong); err == nil {
		t.Fatal("pixel actor autocompleted an unrelated visible target from provisional speech")
	}
	request.VisualTask = "Wait, go back to the Overview."
	request.VisualUnstable = "Overview."
	correct := cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAct, Target: "Overview"}
	if err := validateVisualTarget(request, correct); err != nil {
		t.Fatalf("explicit grounded destination was rejected: %v", err)
	}
	if err := validateVisualTarget(request, cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAct}); err == nil {
		t.Fatal("unlabelled provisional visual action was accepted")
	}
	request.VisualUnstable = ""
	if err := validateVisualTarget(request, cognition.VisualReflexOutcome{Kind: cognition.VisualReflexAct}); err != nil {
		t.Fatalf("committed compatibility action was rejected: %v", err)
	}
}
