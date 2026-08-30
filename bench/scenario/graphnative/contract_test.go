package graphnative_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/scenario"
	graphnative "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
)

func TestFullSuiteContractCoversTheReviewedElevenCasesAndOnlyTheirWireSeams(t *testing.T) {
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate(); err != nil {
		t.Fatal(err)
	}
	suite := scenario.Suite()
	if len(contract.Cases) != 11 || len(contract.Cases) != len(suite) {
		t.Fatalf("scenario graph contract has %d cases; suite has %d", len(contract.Cases), len(suite))
	}
	for index, item := range suite {
		if contract.Cases[index].Name != item.Name {
			t.Fatalf("contract case %d = %q, want %q", index, contract.Cases[index].Name, item.Name)
		}
	}

	menu := caseNamed(t, contract, "a recorded menu")
	assertSeam(t, menu, graphnative.SeamFunctionTool)
	for _, operation := range []graphbinding.AdapterOperation{
		graphbinding.AdapterInputToolResult,
		graphbinding.AdapterInputCreateResponse,
		graphbinding.AdapterOutputToolCalls,
	} {
		assertOperation(t, menu, operation)
	}

	visual := caseNamed(t, contract, "telling them what it saw")
	assertSeam(t, visual, graphnative.SeamStillImageMessage)
	assertOperation(t, visual, graphbinding.AdapterInputText)
	assertOperation(t, visual, graphbinding.AdapterInputCreateResponse)
	if !visual.Capabilities.VisualInput || !visual.Capabilities.TextInjection {
		t.Fatalf("still-image scenario capabilities = %+v", visual.Capabilities)
	}

	ordinary := caseNamed(t, contract, "an ordinary question")
	if slices.Contains(ordinary.Seams, graphnative.SeamFunctionTool) ||
		slices.Contains(ordinary.Seams, graphnative.SeamStillImageMessage) ||
		ordinary.Capabilities.VisualInput || ordinary.Capabilities.TextInjection ||
		!ordinary.Capabilities.ConcurrentIO {
		t.Fatalf("ordinary conversational path gained unrelated extensions: %+v", ordinary)
	}
	for _, operation := range []graphbinding.AdapterOperation{
		graphbinding.AdapterInputUpdate,
		graphbinding.AdapterInputAudio,
		graphbinding.AdapterOutputTurnBegin,
		graphbinding.AdapterOutputTurnEnd,
		graphbinding.AdapterOutputActivity,
		graphbinding.AdapterOutputTranscript,
		graphbinding.AdapterOutputSpeechBegin,
		graphbinding.AdapterOutputSpeechText,
		graphbinding.AdapterOutputSpeechAudio,
		graphbinding.AdapterOutputSpeechEnd,
		graphbinding.AdapterOutputFailed,
	} {
		assertOperation(t, ordinary, operation)
	}

	for _, item := range contract.Cases {
		if !item.Capabilities.ConcurrentIO {
			t.Errorf("scenario %q omitted concurrent-I/O from the continuous-audio path", item.Name)
		}
	}
	if !contract.Capabilities.ConcurrentIO || !contract.Capabilities.VisualInput ||
		!contract.Capabilities.TextInjection {
		t.Fatalf("aggregate scenario capabilities = %+v", contract.Capabilities)
	}
}

func TestScenarioContractSelectionIsExactCanonicalAndSuiteOrdered(t *testing.T) {
	contract, err := graphnative.BuildContract("an ordinary question", "a recorded menu")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a recorded menu", "an ordinary question"}
	for index, item := range contract.Cases {
		if item.Name != want[index] {
			t.Fatalf("selected case %d = %q, want %q", index, item.Name, want[index])
		}
	}
	for _, selection := range [][]string{
		{" an ordinary question"},
		{"an ordinary question", "an ordinary question"},
		{"ordinary-question"},
		{""},
	} {
		if _, err := graphnative.BuildContract(selection...); err == nil {
			t.Fatalf("invalid scenario selection %q was accepted", selection)
		}
	}
}

func TestScenarioContractOwnsItsMutableContainersAndDetectsDrift(t *testing.T) {
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	clone := contract.Clone()
	clone.Cases[0].Name = "redirected"
	clone.Cases[1].Seams[0] = graphnative.Seam("redirected")
	clone.Cases[2].Operations[0] = graphbinding.AdapterInputVideo
	clone.Operations[0] = graphbinding.AdapterInputVideo
	if contract.Cases[0].Name == "redirected" ||
		contract.Cases[1].Seams[0] == graphnative.Seam("redirected") ||
		contract.Cases[2].Operations[0] == graphbinding.AdapterInputVideo ||
		contract.Operations[0] == graphbinding.AdapterInputVideo {
		t.Fatal("contract clone aliases the source")
	}
	if err := clone.Validate(); err == nil {
		t.Fatal("mutated scenario graph contract was accepted")
	}
}

func TestFullSuiteContractMatchesCheckedCanonicalArtifact(t *testing.T) {
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := contract.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"fingerprint": "`+contract.Fingerprint+`"`) {
		t.Fatal("marshaled scenario contract omitted its validated fingerprint")
	}
	want, err := os.ReadFile("testdata/full-suite-contract.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if contract.Fingerprint != strings.TrimSpace(string(want)) {
		t.Fatalf("checked scenario graph contract drifted: got %s, want %s",
			contract.Fingerprint, strings.TrimSpace(string(want)))
	}
}

func caseNamed(t testing.TB, contract graphnative.Contract, name string) graphnative.CaseRequirement {
	t.Helper()
	for _, item := range contract.Cases {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("scenario graph contract has no case %q", name)
	return graphnative.CaseRequirement{}
}

func assertSeam(t testing.TB, requirement graphnative.CaseRequirement, seam graphnative.Seam) {
	t.Helper()
	if !slices.Contains(requirement.Seams, seam) {
		t.Errorf("scenario %q seams %v omit %s", requirement.Name, requirement.Seams, seam)
	}
}

func assertOperation(
	t testing.TB,
	requirement graphnative.CaseRequirement,
	operation graphbinding.AdapterOperation,
) {
	t.Helper()
	if !slices.Contains(requirement.Operations, operation) {
		t.Errorf("scenario %q operations omit %s: %s", requirement.Name, operation,
			strings.Join(adapterOperationStrings(requirement.Operations), ", "))
	}
}

func adapterOperationStrings(source []graphbinding.AdapterOperation) []string {
	result := make([]string, len(source))
	for index, operation := range source {
		result[index] = string(operation)
	}
	return result
}
