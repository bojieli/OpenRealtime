package action

import (
	"encoding/json"
	"testing"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/toolargs"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const repetitionAdmissionGraph = `graph repetition_admission_test {
    action.RepetitionAdmission :: repetition;
    input action = repetition.action;
    input result = repetition.result;
    output admitted = repetition.admitted;
    output canonical_result = repetition.canonical_result;
    output terminal = repetition.terminal;
    output outcome = repetition.outcome;
    output resolved = repetition.resolved;
}`

func TestRepetitionAdmissionDefaultAllowPreservesCompatibility(t *testing.T) {
	mounted, done, cancel := mountGraph(t, repetitionAdmissionGraph,
		map[string]json.RawMessage{"repetition": json.RawMessage(`{}`)}, graphruntime.NewServiceSet())
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	first := repetitionDeclaration("call-one", "run-one", `{"source":"screen","x":7,"y":9}`)
	repetitionSendAction(t, mounted, first)
	result := repetitionCanonicalResult(first, "result-one", "")
	send(t, mustIngressAction(t, mounted, "result"), repetitionResultEnvelope(result))
	if forwarded := receive(t, mustEgressAction(t, mounted, "canonical_result")); forwarded.Payload.(CanonicalResult).TrajectoryItemID != result.TrajectoryItemID {
		t.Fatalf("forwarded canonical result = %+v", forwarded.Payload)
	}
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Code != "success_untracked" {
		t.Fatalf("allow-mode result outcome = %+v", outcome)
	}

	second := repetitionDeclaration("call-two", "run-two", ` { "y" : 9, "x" : 7, "source" : "screen" } `)
	repetitionSendAction(t, mounted, second)
	assertNoEnvelope(t, mustEgressAction(t, mounted, "terminal"))
}

func TestRepetitionAdmissionSuppressesFreshCallAfterCanonicalSuccess(t *testing.T) {
	mounted, done, cancel := mountGraph(t, repetitionAdmissionGraph,
		map[string]json.RawMessage{"repetition": json.RawMessage(
			`{"mode":"at_most_once_after_success_per_user_intent","max_tracked_effects":8}`,
		)}, graphruntime.NewServiceSet())
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	first := repetitionDeclaration("call-one", "run-one", `{"source":"screen","x":7,"y":9}`)
	repetitionSendAction(t, mounted, first)
	result := repetitionCanonicalResult(first, "result-one", "")
	send(t, mustIngressAction(t, mounted, "result"), repetitionResultEnvelope(result))
	// Receiving the forwarded safe point before sending the replay exercises the
	// serialization guarantee: success memory must already exist even though the
	// result audit outcome has not yet been consumed.
	_ = receive(t, mustEgressAction(t, mounted, "canonical_result"))

	second := repetitionDeclaration("call-two", "run-two", ` { "y" : 9, "source":"screen", "x" : 7 } `)
	send(t, mustIngressAction(t, mounted, "action"), repetitionActionEnvelope(second))
	terminal := receive(t, mustEgressAction(t, mounted, "terminal"))
	decision, ok := terminal.Payload.(PreEffectTerminal)
	if !ok || decision.Kind != PreEffectRepetitionSuppressed || decision.CallID != "call-two" ||
		terminal.SessionID != second.Admitted.SessionID || terminal.RunID != second.Admitted.ModelRunID {
		t.Fatalf("pre-effect terminal = %+v", terminal)
	}
	resultOutcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if resultOutcome.Code != "success_recorded" {
		t.Fatalf("result outcome = %+v", resultOutcome)
	}
	replayOutcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if replayOutcome.Kind != OutcomeIgnored || replayOutcome.Code != "successful_effect_replay" {
		t.Fatalf("replay outcome = %+v", replayOutcome)
	}
	assertNoEnvelope(t, mustEgressAction(t, mounted, "admitted"))
	assertNoEnvelope(t, mustEgressAction(t, mounted, "canonical_result"))
}

func TestRepetitionAdmissionErrorsRemainRetryableAndRepeatableToolsRemainUntracked(t *testing.T) {
	for _, test := range []struct {
		name       string
		repeatable bool
		resultErr  string
	}{
		{name: "error result", resultErr: "temporary failure"},
		{name: "explicit repeatable", repeatable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := `{"mode":"at_most_once_after_success_per_user_intent","max_tracked_effects":8}`
			if test.repeatable {
				config = `{"mode":"at_most_once_after_success_per_user_intent","repeatable_tools":["computer.click"],"max_tracked_effects":8}`
			}
			mounted, done, cancel := mountGraph(t, repetitionAdmissionGraph,
				map[string]json.RawMessage{"repetition": json.RawMessage(config)}, graphruntime.NewServiceSet())
			defer stopMounted(t, mounted, done, cancel)
			_ = receive(t, mustEgressAction(t, mounted, "resolved"))

			first := repetitionDeclaration("call-one", "run-one", `{"source":"screen","x":7,"y":9}`)
			repetitionSendAction(t, mounted, first)
			result := repetitionCanonicalResult(first, "result-one", test.resultErr)
			send(t, mustIngressAction(t, mounted, "result"), repetitionResultEnvelope(result))
			_ = receive(t, mustEgressAction(t, mounted, "canonical_result"))
			_ = receive(t, mustEgressAction(t, mounted, "outcome"))

			second := repetitionDeclaration("call-two", "run-two", `{"source":"screen","x":7,"y":9}`)
			repetitionSendAction(t, mounted, second)
			assertNoEnvelope(t, mustEgressAction(t, mounted, "terminal"))
		})
	}
}

func TestRepetitionEffectIdentityUsesEffectiveCanonicalArgumentsAndClosedScope(t *testing.T) {
	base := repetitionDeclaration("call-one", "run-one", `{"source":"screen","x":7,"y":9}`)
	reordered := repetitionDeclaration("fresh-call", "fresh-run", ` { "y" : 9, "x" : 7, "source" : "screen" } `)
	baseID, err := repetitionEffectIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	reorderedID, err := repetitionEffectIdentity(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if baseID != reorderedID {
		t.Fatalf("canonical equivalents differ: %s != %s", baseID, reorderedID)
	}

	normalized := repetitionDeclaration("raw-call", "raw-run", `{"source":"screen","x":[7,9]}`)
	effective := trajectory.ToolCall{
		CallID: "raw-call", Name: "computer.click", Arguments: json.RawMessage(`{"source":"screen","x":7,"y":9}`),
	}
	normalized.EffectiveCall = &effective
	normalized.Normalization = &ArgumentNormalization{
		Rewrites:                 []ArgumentRewrite{{Argument: "x", Normalizer: toolargs.CoordinatePairXYV1}},
		OriginalArgumentsDigest:  argumentBytesDigest(normalized.Admitted.Proposal.Call.Arguments),
		EffectiveArgumentsDigest: argumentBytesDigest(effective.Arguments),
		RegistryReference:        normalized.RegistryReference, RegistryDigest: normalized.RegistryDigest,
		DeclarationDigest: normalized.DeclarationDigest,
	}
	normalizedID, err := repetitionEffectIdentity(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedID != baseID {
		t.Fatalf("normalized effective action bypassed identity: %s != %s", normalizedID, baseID)
	}

	mutations := []struct {
		name string
		edit func(*DeclaredAction)
	}{
		{"session", func(value *DeclaredAction) { value.Admitted.SessionID = "other-session" }},
		{"authority observation", func(value *DeclaredAction) { value.Admitted.AuthorityItemID = "other-intent" }},
		{"target", func(value *DeclaredAction) { value.Target = "other-target" }},
		{"tool", func(value *DeclaredAction) { value.Admitted.Proposal.Call.Name = "computer.double_click" }},
		{"declaration", func(value *DeclaredAction) { value.DeclarationDigest = "sha256:other-declaration" }},
		{"arguments", func(value *DeclaredAction) {
			value.Admitted.Proposal.Call.Arguments = json.RawMessage(`{"source":"screen","x":8,"y":9}`)
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := cloneDeclared(base)
			mutation.edit(&changed)
			changedID, err := repetitionEffectIdentity(changed)
			if err != nil {
				t.Fatal(err)
			}
			if changedID == baseID {
				t.Fatalf("%s did not change effect identity", mutation.name)
			}
		})
	}
}

func TestRepetitionSuccessfulMemoryIsDeterministicallyBounded(t *testing.T) {
	memory := newSuccessfulEffectMemory(2)
	if !memory.record("first") || !memory.record("second") || memory.record("third") {
		t.Fatalf("bounded successful-effect records = %+v", memory)
	}
	if len(memory.items) != 2 || !memory.saturated || !memory.suppresses("first") ||
		!memory.suppresses("second") || !memory.suppresses("third") ||
		!memory.suppresses("never-observed") {
		t.Fatalf("bounded successful-effect memory did not fail closed: %+v", memory)
	}
}

func TestRepetitionAdmissionCapacityFailsClosedWithoutEviction(t *testing.T) {
	mounted, done, cancel := mountGraph(t, repetitionAdmissionGraph,
		map[string]json.RawMessage{"repetition": json.RawMessage(
			`{"mode":"at_most_once_after_success_per_user_intent","max_tracked_effects":1}`,
		)}, graphruntime.NewServiceSet())
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	first := repetitionDeclaration("call-one", "run-one", `{"source":"screen","x":1,"y":1}`)
	repetitionSendAction(t, mounted, first)
	send(t, mustIngressAction(t, mounted, "result"),
		repetitionResultEnvelope(repetitionCanonicalResult(first, "result-one", "")))
	_ = receive(t, mustEgressAction(t, mounted, "canonical_result"))
	_ = receive(t, mustEgressAction(t, mounted, "outcome"))

	second := repetitionDeclaration("call-two", "run-two", `{"source":"screen","x":2,"y":2}`)
	repetitionSendAction(t, mounted, second)
	send(t, mustIngressAction(t, mounted, "result"),
		repetitionResultEnvelope(repetitionCanonicalResult(second, "result-two", "")))
	_ = receive(t, mustEgressAction(t, mounted, "canonical_result"))
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Code != "success_capacity_closed" {
		t.Fatalf("capacity-closing result outcome = %+v", outcome)
	}

	third := repetitionDeclaration("call-three", "run-three", `{"source":"screen","x":3,"y":3}`)
	send(t, mustIngressAction(t, mounted, "action"), repetitionActionEnvelope(third))
	terminal := receive(t, mustEgressAction(t, mounted, "terminal")).Payload.(PreEffectTerminal)
	if terminal.CallID != "call-three" || terminal.Kind != PreEffectRepetitionSuppressed {
		t.Fatalf("fail-closed capacity terminal = %+v", terminal)
	}
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Kind != OutcomeIgnored || outcome.Code != "effect_memory_capacity_closed" {
		t.Fatalf("fail-closed capacity outcome = %+v", outcome)
	}
	assertNoEnvelope(t, mustEgressAction(t, mounted, "admitted"))
}

func repetitionDeclaration(callID, runID, arguments string) DeclaredAction {
	admitted := testAdmittedProposal()
	admitted.Proposal.Call.CallID = callID
	admitted.Proposal.Call.Arguments = json.RawMessage(arguments)
	admitted.ModelRunID = runID
	return DeclaredAction{
		Admitted: admitted, Confirmation: legacyaction.ConfirmNever, Target: "browser",
		RegistryReference: "deployment.tools", RegistryDigest: "sha256:registry",
		DeclarationDigest: "sha256:declaration", DispatcherIdentity: "computer:browser",
	}
}

func repetitionCanonicalResult(declared DeclaredAction, itemID, resultError string) CanonicalResult {
	call := callOfDeclared(declared)
	commitmentID := actionCommitmentID(declared.Admitted)
	result := trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Error: resultError}
	if resultError == "" {
		result.Output = json.RawMessage(`{"ok":true}`)
	}
	return CanonicalResult{
		Execution: ExecutionResult{
			Executable: ExecutableAction{
				Canonical: CanonicalAction{
					Authorized: AuthorizedAction{
						Confirmed:       ConfirmedAction{Declared: cloneDeclared(declared)},
						TargetReference: "deployment.browser", TargetDigest: "sha256:target",
					},
					ProposalItemID:   "canonical-proposal-" + call.CallID,
					TrajectoryItemID: "canonical-call-" + call.CallID, StoreVersion: 1,
				},
				LedgerReference: "deployment.ledger", LedgerIdentity: "ledger",
				CommitmentID: commitmentID, Capability: "capability",
			},
			ResultCapability: "result-capability", CompletionOrigin: CompletionReturned,
			CallID: call.CallID, Name: call.Name, CommitmentID: commitmentID,
			Result: result, CrossedNS: 1, FinishedNS: 2,
		},
		TrajectoryItemID: itemID, StoreVersion: 2,
	}
}

func repetitionActionEnvelope(declared DeclaredAction) element.Envelope {
	return element.Envelope{
		Type: DeclaredType(), ItemID: "declared-" + callOfDeclared(declared).CallID,
		SessionID: declared.Admitted.SessionID, RunID: declared.Admitted.ModelRunID, Payload: declared,
	}
}

func repetitionResultEnvelope(result CanonicalResult) element.Envelope {
	admitted := result.Execution.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
	return element.Envelope{
		Type: CanonicalResultType(), ItemID: result.TrajectoryItemID + "-envelope",
		SessionID: admitted.SessionID, RunID: admitted.ModelRunID, Payload: result,
	}
}

func repetitionSendAction(t *testing.T, mounted *graphruntime.Mounted, declared DeclaredAction) {
	t.Helper()
	send(t, mustIngressAction(t, mounted, "action"), repetitionActionEnvelope(declared))
	if admitted := receive(t, mustEgressAction(t, mounted, "admitted")).Payload.(DeclaredAction); callOfDeclared(admitted).CallID != callOfDeclared(declared).CallID {
		t.Fatalf("admitted action = %+v", admitted)
	}
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Kind != OutcomeSucceeded {
		t.Fatalf("admission outcome = %+v", outcome)
	}
}
