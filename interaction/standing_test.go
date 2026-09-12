package interaction_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

func TestParsePinReadsEachForm(t *testing.T) {
	for _, testCase := range []struct {
		answer string
		kind   string
		text   string
		scope  interaction.Scope
	}{
		{"none", "none", "", ""},
		{"pin conversation count them out loud", "pin", "count them out loud", interaction.ScopeConversation},
		{"pin turn do not reply until they finish", "pin", "do not reply until they finish", interaction.ScopeTurn},
		{"revoke tell them when the kettle boils", "revoke", "tell them when the kettle boils", ""},
	} {
		kind, pinned, ok := interaction.ParsePin(testCase.answer)
		if !ok || kind != testCase.kind {
			t.Fatalf("%q parsed as (%q, %v)", testCase.answer, kind, ok)
		}
		if pinned.Text != testCase.text || pinned.Scope != testCase.scope {
			t.Fatalf("%q gave %+v", testCase.answer, pinned)
		}
	}
}

// A pass that invents a policy nobody set is worse than one that misses.
func TestParsePinRefusesToGuess(t *testing.T) {
	for _, answer := range []string{"", "maybe pin this?", "I think they set a policy", "pin"} {
		if _, _, ok := interaction.ParsePin(answer); ok {
			t.Fatalf("%q was accepted as an answer", answer)
		}
	}
}

func TestExtractorTreatsAnExactBareEchoOfAnExistingPolicyAsNoMutation(t *testing.T) {
	existing := []interaction.StandingInstruction{{
		Text:  "translate everything he says into English as he goes and do not wait for him to finish",
		Scope: interaction.ScopeConversation,
	}}
	generator := &standingScriptGenerator{
		extraction: "translate everything he says into English as he goes and do not wait for him to finish",
	}
	extractor, err := interaction.NewExtractor(generator)
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractor.Extract(context.Background(), existing, nil, "很高兴见到你")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pins) != 0 || len(got.Revokes) != 0 {
		t.Fatalf("bare existing-policy echo mutated the pinboard: %+v", got)
	}
	generator.assertCalls(t, "很高兴见到你", 1, 0, 0, 0, 0)
}

func TestExtractorStillRefusesUnknownBarePolicyText(t *testing.T) {
	generator := &standingScriptGenerator{extraction: "tell them when the build finishes"}
	extractor, err := interaction.NewExtractor(generator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractor.Extract(context.Background(), nil, nil, "Tell me when the build finishes."); err == nil {
		t.Fatal("unknown bare policy text was accepted")
	}
}

// A revocation is only recognisable against the thing it lifts.
func TestRenderForExtractionShowsWhatIsInForce(t *testing.T) {
	block := interaction.RenderForExtraction([]interaction.StandingInstruction{
		{Text: "tell them when the kettle has boiled", Scope: interaction.ScopeConversation},
	}, nil, "never mind about the kettle")
	if !strings.Contains(block, "tell them when the kettle has boiled") {
		t.Fatalf("the policy being lifted was not shown:\n%s", block)
	}
	if !strings.Contains(block, "never mind about the kettle") {
		t.Fatalf("the utterance was not shown:\n%s", block)
	}
	empty := interaction.RenderForExtraction(nil, nil, "hello")
	if !strings.Contains(empty, "No policies") {
		t.Fatalf("an empty policy list must say so rather than omit the section:\n%s", empty)
	}
}

// A policy that names a delay waits on something that has not happened yet,
// and is standing by construction. It must not be asked about, because the
// delay is lifted out of the text before anything else reads it: "if I go
// quiet for fifteen seconds, ask whether I'm still there" arrives as "ask
// whether they are still there", which reads as a request about this moment.
func TestADelayedPolicyStandsWithoutBeingAsked(t *testing.T) {
	kind, instruction, ok := interaction.ParsePin(
		"pin turn after 15s ask whether they are still there")
	if !ok || kind != "pin" {
		t.Fatalf("the pin did not parse: %q %v", kind, ok)
	}
	if instruction.After == 0 {
		t.Fatal("the delay was not read out of the policy")
	}
}

// ParsePin turns the natural-language delay into typed runtime state before
// grounding. The verifier still needs to see that trigger: otherwise it is
// asked whether the remaining one-shot action is itself a standing policy.
func TestExtractorGroundingRetainsDelayedTrigger(t *testing.T) {
	generator := &standingScriptGenerator{
		extraction:  "pin conversation after 15s ask whether they are still there",
		grounding:   "yes",
		counting:    "no",
		restricting: "no",
	}
	extractor, err := interaction.NewExtractor(generator)
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractor.Extract(context.Background(), nil, nil,
		"If I have not said anything for about fifteen seconds, ask whether I am still there.")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pins) != 1 || got.Pins[0].After.String() != "15s" ||
		got.Pins[0].Text != "ask whether they are still there" ||
		got.Pins[0].Scope != interaction.ScopeConversation {
		t.Fatalf("delayed extraction = %+v", got)
	}
	for _, call := range generator.calls {
		if call.prompt == interaction.StandingPolicyGroundingInstruction &&
			!strings.Contains(call.evidence,
				"Proposed standing policy:\nafter 15s ask whether they are still there") {
			t.Fatalf("grounding lost typed delay:\n%s", call.evidence)
		}
	}
}

func TestExtractorRejectsPoliciesInventedFromImmediateSpeech(t *testing.T) {
	tests := []struct {
		name       string
		utterance  string
		extraction string
	}{
		{
			name:       "current rain observation",
			utterance:  "Oh, it's starting to rain outside.",
			extraction: "pin conversation say something if it starts raining",
		},
		{
			name:       "floor-taking question",
			utterance:  "Hold on, what time is the meeting scheduled today?",
			extraction: "pin turn do not reply until they have asked what time the meeting is scheduled today",
		},
		{
			name:       "topic change",
			utterance:  "Hold that thought. Can we discuss cooking tips instead?",
			extraction: "pin turn do not reply until they have finished their thought and ask about cooking tips",
		},
		{
			name:       "one-shot command",
			utterance:  "Book me a table for four at eight.",
			extraction: "pin conversation book a table when they ask",
		},
		{
			name:       "reply length style",
			utterance:  "Keep your answers to a sentence or two.",
			extraction: "pin conversation keep every answer short",
		},
		{
			name:       "always reply style",
			utterance:  "Always include the order number in replies.",
			extraction: "pin conversation include the order number in every reply",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			generator := &standingScriptGenerator{
				extraction: testCase.extraction,
				grounding:  "no",
			}
			extractor, err := interaction.NewExtractor(generator)
			if err != nil {
				t.Fatal(err)
			}
			got, err := extractor.Extract(context.Background(), nil, nil, testCase.utterance)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Pins) != 0 || len(got.Revokes) != 0 {
				t.Fatalf("invented extraction escaped grounding: %+v", got)
			}
			generator.assertCalls(t, testCase.utterance, 1, 1, 0, 0, 0)
		})
	}
}

func TestExtractorKeepsExplicitStandingPolicies(t *testing.T) {
	tests := []struct {
		name        string
		utterance   string
		extraction  string
		counting    string
		restricting string
		scope       string
		wantScope   interaction.Scope
		wantCount   bool
	}{
		{
			name: "future event", utterance: "Tell me when the build finishes.",
			extraction: "pin conversation tell them when the build finishes",
			counting:   "no", restricting: "no", scope: "standing", wantScope: interaction.ScopeConversation,
		},
		{
			name: "temporary silence", utterance: "Hang on, I am not finished.",
			extraction: "pin turn do not reply until they are finished",
			counting:   "no", restricting: "no", scope: "passing", wantScope: interaction.ScopeTurn,
		},
		{
			name: "repeated count", utterance: "Count the animals as I mention them.",
			extraction: "pin conversation count the animals as they mention them",
			counting:   "yes", restricting: "no", scope: "standing", wantScope: interaction.ScopeConversation,
			wantCount: true,
		},
		{
			name: "threshold interruption", utterance: "Stop me if I quote a price under fifty.",
			extraction: "pin conversation interrupt when they quote a price under fifty",
			counting:   "no", restricting: "no", scope: "standing", wantScope: interaction.ScopeConversation,
		},
		{
			name: "ongoing reading silence", utterance: "Never talk over me while I am reading.",
			extraction: "pin conversation do not speak while they are reading",
			counting:   "no", restricting: "no", scope: "standing", wantScope: interaction.ScopeConversation,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			generator := &standingScriptGenerator{
				extraction: testCase.extraction,
				grounding:  "yes", counting: testCase.counting,
				restricting: testCase.restricting, scope: testCase.scope,
			}
			extractor, err := interaction.NewExtractor(generator)
			if err != nil {
				t.Fatal(err)
			}
			got, err := extractor.Extract(context.Background(), nil, nil, testCase.utterance)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Pins) != 1 || got.Pins[0].Scope != testCase.wantScope ||
				got.Pins[0].Counting != testCase.wantCount {
				t.Fatalf("grounded extraction = %+v", got)
			}
			generator.assertCalls(t, testCase.utterance, 1, 1, 1, 1, 1)
		})
	}
}

func TestExtractorGroundingFailsClosed(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		grounding string
		err       error
	}{
		{name: "hedged answer", grounding: "yes, probably"},
		{name: "empty answer"},
		{name: "provider error", err: errors.New("grounding unavailable")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			generator := &standingScriptGenerator{
				extraction: "pin conversation tell them when the build finishes",
				grounding:  testCase.grounding, groundingErr: testCase.err,
			}
			extractor, err := interaction.NewExtractor(generator)
			if err != nil {
				t.Fatal(err)
			}
			got, err := extractor.Extract(
				context.Background(), nil, nil, "Tell me when the build finishes.",
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Pins) != 0 {
				t.Fatalf("uncertain grounding admitted %+v", got.Pins)
			}
			generator.assertCalls(t, "Tell me when the build finishes.", 1, 1, 0, 0, 0)
		})
	}
}

type standingScriptGenerator struct {
	extraction   string
	contract     string
	grounding    string
	groundingErr error
	counting     string
	restricting  string
	scope        string
	lift         string
	liftConfirm  string
	calls        []standingGeneratorCall
}

type standingGeneratorCall struct {
	prompt   string
	evidence string
}

func (generator *standingScriptGenerator) Name() string { return "standing-script" }

func (generator *standingScriptGenerator) Generate(
	_ context.Context, prompt, evidence string, _ int,
) (string, error) {
	generator.calls = append(generator.calls, standingGeneratorCall{prompt: prompt, evidence: evidence})
	switch prompt {
	case interaction.ExtractionInstruction:
		return generator.extraction, nil
	case interaction.ContractExtractionInstruction:
		return generator.contract, nil
	case interaction.StandingPolicyGroundingInstruction:
		return generator.grounding, generator.groundingErr
	case interaction.CountingInstruction:
		return generator.counting, nil
	case interaction.RestrictingInstruction:
		return generator.restricting, nil
	case interaction.ScopeInstruction:
		return generator.scope, nil
	case interaction.LiftInstruction:
		if generator.lift == "" {
			return "no", nil
		}
		return generator.lift, nil
	case interaction.LiftConfirmInstruction:
		if generator.liftConfirm == "" {
			return "yes", nil
		}
		return generator.liftConfirm, nil
	default:
		return "", errors.New("unexpected standing-policy prompt")
	}
}

func (generator *standingScriptGenerator) assertCalls(
	t *testing.T, utterance string, extraction, grounding, counting, restricting, scope int,
) {
	t.Helper()
	wants := map[string]int{
		interaction.ExtractionInstruction:              extraction,
		interaction.StandingPolicyGroundingInstruction: grounding,
		interaction.CountingInstruction:                counting,
		interaction.RestrictingInstruction:             restricting,
		interaction.ScopeInstruction:                   scope,
	}
	got := make(map[string]int, len(wants))
	for _, call := range generator.calls {
		got[call.prompt]++
		if call.prompt == interaction.StandingPolicyGroundingInstruction &&
			(!strings.Contains(call.evidence, utterance) ||
				!strings.Contains(call.evidence, "Proposed standing policy:")) {
			t.Fatalf("grounding lost exact utterance or proposal:\n%s", call.evidence)
		}
	}
	for prompt, want := range wants {
		if got[prompt] != want {
			t.Fatalf("prompt %q calls = %d, want %d (all calls: %+v)",
				prompt[:min(40, len(prompt))], got[prompt], want, generator.calls)
		}
	}
}

// "Stop counting" comes back from the extraction as a rule about not
// counting, which grounds as nothing - so each policy in force is put to the
// model as one question, and only its exact yes lifts the policy.
func TestExtractorLiftsAPolicyTheExtractionRestatedAsANegativeRule(t *testing.T) {
	counting := []interaction.StandingInstruction{{
		Text: "count the cities out loud as they mention them", Scope: interaction.ScopeConversation, Counting: true,
	}}
	for _, testCase := range []struct {
		name string
		lift string
		want int
	}{
		{"the question lifts it", "yes", 1},
		{"an unsure answer leaves it", "not sure", 0},
		{"a lift the words do not confirm leaves it", "yes", 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			generator := &standingScriptGenerator{
				extraction: "pin conversation do not count the cities out loud as they mention them anymore",
				grounding:  "no", lift: testCase.lift,
			}
			if testCase.want == 0 && testCase.lift == "yes" {
				generator.liftConfirm = "no"
			}
			extractor, err := interaction.NewExtractor(generator)
			if err != nil {
				t.Fatal(err)
			}
			got, err := extractor.Extract(context.Background(), counting,
				[]string{"user: and took the train to Berlin the week after.", "agent: Two."},
				"Okay, you can stop counting now.")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Pins) != 0 || len(got.Revokes) != testCase.want {
				t.Fatalf("extraction = %+v, want no pins and %d revocation", got, testCase.want)
			}
			if testCase.want == 1 && got.Revokes[0] != counting[0].Text {
				t.Fatalf("revoked %q, want the policy in force", got.Revokes[0])
			}
			lifts := 0
			for _, call := range got.Calls {
				if call.Question == "lift" {
					lifts++
				}
			}
			if lifts != 1 {
				t.Fatalf("lift question asked %d times, want once per policy in force: %+v", lifts, got.Calls)
			}
		})
	}
}

// The deployment's instruction sets standing policies of its own - "press
// the key when the menu offers what the user wants" - and they come back as
// the operator's: in force for the conversation, never lifted, read for
// counting and restriction like a spoken policy, and never grounded against
// a person's words because there is no person in them.
func TestExtractorReadsTheOperatorsRulesFromTheContract(t *testing.T) {
	generator := &standingScriptGenerator{
		contract: "pin conversation when a recorded menu offers the option the user wants, press that key\n" +
			"pin turn correct them the moment they say a date that contradicts the third",
		counting: "no", restricting: "no", scope: "turn",
	}
	extractor, err := interaction.NewExtractor(generator)
	if err != nil {
		t.Fatal(err)
	}
	contract, ok := extractor.(interaction.ContractExtractor)
	if !ok {
		t.Fatal("the model extractor does not read contracts")
	}
	got, err := contract.ExtractContract(context.Background(),
		"You are calling a company's support line on behalf of the user. When a recorded menu offers an option "+
			"that matches what the user wants, press that key.")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pins) != 2 || len(got.Revokes) != 0 {
		t.Fatalf("contract extraction = %+v, want two pins and no revocation", got)
	}
	for _, pin := range got.Pins {
		if !pin.Operator || pin.Scope != interaction.ScopeConversation || pin.Counting || pin.Restricting {
			t.Fatalf("operator pin = %+v, want an unliftable conversation policy", pin)
		}
	}
	if got.Pins[0].Text != "when a recorded menu offers the option the user wants, press that key" {
		t.Fatalf("pin text = %q", got.Pins[0].Text)
	}
	for _, call := range generator.calls {
		if call.prompt == interaction.StandingPolicyGroundingInstruction ||
			call.prompt == interaction.AddressedElsewhereInstruction {
			t.Fatalf("the contract pass asked %q, which is a question about a person's words", call.prompt[:40])
		}
	}
	if len(got.Calls) == 0 || got.Calls[0].Question != "contract" {
		t.Fatalf("calls = %+v, want the contract question first", got.Calls)
	}
	empty, err := contract.ExtractContract(context.Background(), "   ")
	if err != nil || len(empty.Pins) != 0 || len(empty.Calls) != 0 {
		t.Fatalf("an empty contract was read: %+v, %v", empty, err)
	}
}
