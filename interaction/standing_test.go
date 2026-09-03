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
	grounding    string
	groundingErr error
	counting     string
	restricting  string
	scope        string
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
	case interaction.StandingPolicyGroundingInstruction:
		return generator.grounding, generator.groundingErr
	case interaction.CountingInstruction:
		return generator.counting, nil
	case interaction.RestrictingInstruction:
		return generator.restricting, nil
	case interaction.ScopeInstruction:
		return generator.scope, nil
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
