package action

import (
	"encoding/json"
	"strings"
	"testing"

	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

const toolAdmissionGraph = `graph tool_admission_test {
    action.ToolAdmission :: admission;
    input action = admission.action;
    output admitted = admission.admitted;
    output terminal = admission.terminal;
    output outcome = admission.outcome;
    output resolved = admission.resolved;
}`

func TestToolAdmissionDefaultAllowsDeclaredAction(t *testing.T) {
	mounted, done, cancel := mountGraph(t, toolAdmissionGraph,
		map[string]json.RawMessage{"admission": json.RawMessage(`{}`)}, graphruntime.NewServiceSet())
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	declared := repetitionDeclaration("call-one", "run-one", `{"source":"screen","x":7,"y":9}`)
	send(t, mustIngressAction(t, mounted, "action"), repetitionActionEnvelope(declared))
	forwarded := receive(t, mustEgressAction(t, mounted, "admitted"))
	if got, ok := declaredPayload(forwarded.Payload); !ok || callOfDeclared(got).CallID != "call-one" {
		t.Fatalf("admitted payload = %+v", forwarded.Payload)
	}
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeSucceeded || outcome.Code != "allowed" {
		t.Fatalf("admission outcome = %+v", outcome)
	}
	assertNoEnvelope(t, mustEgressAction(t, mounted, "terminal"))
}

func TestToolAdmissionSuppressesDeniedToolBeforeEffect(t *testing.T) {
	mounted, done, cancel := mountGraph(t, toolAdmissionGraph,
		map[string]json.RawMessage{"admission": json.RawMessage(
			`{"denied_tools":["computer.screenshot","computer.wait"]}`,
		)}, graphruntime.NewServiceSet())
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	declared := repetitionDeclaration("wait-call", "wait-run", `{"duration_ms":1000}`)
	declared.Admitted.Proposal.Call.Name = "computer.wait"
	send(t, mustIngressAction(t, mounted, "action"), repetitionActionEnvelope(declared))
	terminalEnvelope := receive(t, mustEgressAction(t, mounted, "terminal"))
	terminal, ok := terminalEnvelope.Payload.(PreEffectTerminal)
	if !ok || terminal.Kind != PreEffectToolPolicySuppressed || terminal.CallID != "wait-call" ||
		terminalEnvelope.SessionID != declared.Admitted.SessionID ||
		terminalEnvelope.RunID != declared.Admitted.ModelRunID {
		t.Fatalf("tool-policy terminal = %+v", terminalEnvelope)
	}
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeIgnored || outcome.Code != "tool_denied" || outcome.CallID != "wait-call" {
		t.Fatalf("denied outcome = %+v", outcome)
	}
	assertNoEnvelope(t, mustEgressAction(t, mounted, "admitted"))
}

func TestToolAdmissionAllowListClosesUnknownNames(t *testing.T) {
	mounted, done, cancel := mountGraph(t, toolAdmissionGraph,
		map[string]json.RawMessage{"admission": json.RawMessage(
			`{"allowed_tools":["computer.click"]}`,
		)}, graphruntime.NewServiceSet())
	defer stopMounted(t, mounted, done, cancel)
	_ = receive(t, mustEgressAction(t, mounted, "resolved"))

	declared := repetitionDeclaration("type-call", "type-run", `{"source":"screen","text":"hello"}`)
	declared.Admitted.Proposal.Call.Name = "computer.type"
	send(t, mustIngressAction(t, mounted, "action"), repetitionActionEnvelope(declared))
	terminal := receive(t, mustEgressAction(t, mounted, "terminal")).Payload.(PreEffectTerminal)
	if terminal.Kind != PreEffectToolPolicySuppressed || terminal.CallID != "type-call" {
		t.Fatalf("allow-list terminal = %+v", terminal)
	}
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeIgnored || outcome.Code != "tool_not_allowed" {
		t.Fatalf("allow-list outcome = %+v", outcome)
	}
}

func TestToolAdmissionConfigRejectsAmbiguousOrNonCanonicalNames(t *testing.T) {
	for _, source := range []string{
		`{"allowed_tools":["computer.click"],"denied_tools":["computer.click"]}`,
		`{"allowed_tools":["computer.click","computer.click"]}`,
		`{"denied_tools":[" computer.wait"]}`,
	} {
		if _, err := decodeToolAdmissionConfig(json.RawMessage(source)); err == nil ||
			(!strings.Contains(err.Error(), "cannot be both") &&
				!strings.Contains(err.Error(), "duplicated") &&
				!strings.Contains(err.Error(), "canonical")) {
			t.Fatalf("decodeToolAdmissionConfig(%s) error = %v", source, err)
		}
	}
}
