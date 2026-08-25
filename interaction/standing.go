package interaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// Scope is how long a standing instruction lasts.
//
// The distinction is not decoration. "Don't cut me off" governs a
// conversation; "wait, I have more to say" governs the next few seconds. Pin
// the second with the lifetime of the first and the agent goes permanently
// mute - a failure that would surface weeks later as an unexplained regression
// and would be very hard to trace back to the sentence that caused it.
type Scope string

const (
	// ScopeTurn expires when the current turn ends.
	ScopeTurn Scope = "turn"
	// ScopeConversation lasts until someone revokes it.
	ScopeConversation Scope = "conversation"
)

// StandingInstruction is one pinned policy.
type StandingInstruction struct {
	Text  string
	Scope Scope
	// SetNS is when it was pinned. Age is part of the instruction: "don't cut
	// me off" said thirty seconds ago governs differently from the same words
	// an hour and three topics back, and a decision that cannot see which one
	// it is has been handed the words without the context.
	SetNS uint64
}

// ExtractionInstruction governs the pass that notices when somebody has set an
// interaction policy out loud.
//
// It is a separate pass and not another paragraph in the reasoning prompt,
// because instruction capacity is real and measured: adding one paragraph to
// that prompt moved unrelated decisions from 8/15 to 3/15. A model already
// reasoning and calling tools should not also be watching for this.
//
// It runs off the critical path. An interaction policy governs what happens
// next, so it has to land before the next utterance rather than before this
// answer, and that is a budget of hundreds of milliseconds rather than tens.
//
// The pass records; it never adjudicates. When "don't cut me off" and "tell me
// the moment the build finishes" collide - and they will, mid-sentence - that
// conflict belongs to the model deciding the instant it bites, which has both
// instructions and their ages in front of it. Resolving it here would be
// settling a case that has not happened yet.
// ExtractionInstruction is built rather than written out, so that its worked
// examples pass through the same renderer as a live decision. An example in a
// different shape from the input costs a model capacity on translating between
// two formats of the same thing before it can answer, which measured as ten
// points on the sibling boundary.
var ExtractionInstruction = buildExtraction()

type extractionExample struct {
	existing  []StandingInstruction
	utterance string
	answer    string
}

func extractionExamples() []extractionExample {
	watching := []StandingInstruction{{Text: "shout if you see the train coming", Scope: ScopeConversation}}
	return []extractionExample{
		{nil, "Book me a table for four at eight.", "none"},
		{nil, "Keep your answers to a sentence or two.", "none"},
		{nil, "Shout if you see the train coming.", "pin conversation shout if you see the train coming"},
		{nil, "Hang on, I haven't got to the point yet.", "pin turn do not reply until they have made their point"},
		{watching, "Forget about the train, I can see it now.", "revoke shout if you see the train coming"},
		{watching, "Also let me know if it starts raining.", "pin conversation say something if it starts raining"},
		{nil, "Give me a nudge if I start talking too fast.", "pin conversation tell them if they start talking too fast"},
	}
}

func buildExtraction() string {
	var text strings.Builder
	text.WriteString(
		"Someone in a voice conversation has just finished speaking. Decide whether they set a policy about " +
			"*when* the agent should speak or act, and reply with one line and nothing else.\n\n" +
			"none - they set no such policy. This is the usual answer.\n" +
			"pin conversation <policy> - they set one that stands from now until somebody lifts it.\n" +
			"pin turn <policy> - they set one that expires when they finish what they are currently saying.\n" +
			"revoke <policy> - they lifted one already in force. Quote the one they lifted, from the list above.\n\n" +
			"A policy names a trigger: a condition to watch for, and speech or action to produce when it happens. " +
			"\"Stop me if I get a date wrong\", \"tell me the moment it lands\", \"count them out loud as I " +
			"mention them\" each name one. Work names no trigger - \"finish the report by Friday\" says what to " +
			"do and never when to say it. Neither does style: \"give me shorter answers\" and \"always include " +
			"the order number\" change the shape of replies the agent was going to make anyway, and start none " +
			"of them. If nothing new would ever cause the agent to speak, it is not a policy.\n\n" +
			"For scope, ask one question: once this speaker stops talking, does the policy still apply? If it " +
			"does, it is a conversation policy. Only if it dies the moment they finish - hang on, wait, let me " +
			"finish, not yet - is it a turn policy. Watching for something that has not happened yet outlives " +
			"the sentence that asked for it, so it is a conversation policy however briefly it was asked for.\n\n" +
			"An immediate command is not a policy. The test is whether obeying it takes one action or requires " +
			"watching for something: \"stop\" is finished the moment it is obeyed and is none, while \"let me " +
			"finish\" means staying quiet until a condition holds and is a policy for this turn. Neither is a " +
			"revocation: revoke is only for lifting something on the list above.\n\n" +
			"Prefer pinning to missing. A policy set and not recorded fails silently and looks like the agent " +
			"ignoring someone; one recorded that turns out not to apply is simply never triggered.\n\n" +
			"Write the policy back as a short instruction to the agent, in the speaker's own words where you " +
			"can.\n\nWorked examples. These are other conversations, not this one.\n")
	for _, example := range extractionExamples() {
		text.WriteString("\n---\n" + RenderForExtraction(example.existing, example.utterance) +
			"\n-> " + example.answer + "\n")
	}
	text.WriteString("\n---\nNow decide the case below. Reply with one line and nothing else.")
	return text.String()
}

// RenderForExtraction is what the extraction pass reads.
//
// It carries the policies already in force, without which a revocation is
// invisible: "never mind about the kettle" is only recognisable as lifting
// something if the kettle instruction is on the page. Both models tested
// answered "none" to every revocation until they were shown what there was to
// revoke.
func RenderForExtraction(existing []StandingInstruction, utterance string) string {
	var block strings.Builder
	if len(existing) > 0 {
		block.WriteString("Policies already in force:\n")
		for _, pinned := range existing {
			block.WriteString("- " + pinned.Text + " (" + string(pinned.Scope) + ")\n")
		}
		block.WriteString("\n")
	} else {
		block.WriteString("No policies are currently in force.\n\n")
	}
	block.WriteString("They just said: \"" + utterance + "\"")
	return block.String()
}

// ParsePin reads what the extraction pass returned. An unrecognised answer is
// reported as unrecognised rather than guessed at: a pass that silently
// invents a policy nobody set is worse than one that misses.
func ParsePin(text string) (kind string, instruction StandingInstruction, ok bool) {
	line := strings.TrimSpace(text)
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = strings.TrimSpace(line[:index])
	}
	lowered := strings.ToLower(line)
	switch {
	case lowered == "none" || strings.HasPrefix(lowered, "none "):
		return "none", StandingInstruction{}, true
	case strings.HasPrefix(lowered, "pin turn "):
		return "pin", StandingInstruction{Text: strings.TrimSpace(line[len("pin turn "):]), Scope: ScopeTurn}, true
	case strings.HasPrefix(lowered, "pin conversation "):
		return "pin", StandingInstruction{Text: strings.TrimSpace(line[len("pin conversation "):]), Scope: ScopeConversation}, true
	case strings.HasPrefix(lowered, "revoke "):
		return "revoke", StandingInstruction{Text: strings.TrimSpace(line[len("revoke "):])}, true
	}
	return "", StandingInstruction{}, false
}

// Generator produces a short free-form answer. Extraction needs one because
// the policy it finds cannot be drawn from an enumeration.
type Generator interface {
	Name() string
	Generate(ctx context.Context, prompt, evidence string, maxTokens int) (string, error)
}

// Extractor notices when somebody set an interaction policy out loud.
type Extractor interface {
	Name() string
	// Extract reads one finished utterance against the policies already in
	// force, and returns what to do about it.
	Extract(ctx context.Context, existing []StandingInstruction, utterance string) (Extraction, error)
}

// Extraction is what an utterance did to the set of standing policies.
type Extraction struct {
	// Kind is "none", "pin", or "revoke".
	Kind        string
	Instruction StandingInstruction
}

// NewExtractor builds the pass over a generator.
func NewExtractor(generator Generator) (Extractor, error) {
	if generator == nil {
		return nil, errors.New("an extractor requires a generator")
	}
	return modelExtractor{generator: generator}, nil
}

type modelExtractor struct{ generator Generator }

func (extractor modelExtractor) Name() string { return "extract:" + extractor.generator.Name() }

// Extract runs the pass and refuses to guess.
//
// An unparseable answer produces nothing rather than a policy nobody set.
// The asymmetry elsewhere runs the other way - a missed policy fails silently
// and a spurious one is merely never triggered - but that argument is about
// what the model should lean towards, not about what this should invent when
// the model has said something it does not understand.
func (extractor modelExtractor) Extract(
	ctx context.Context, existing []StandingInstruction, utterance string,
) (Extraction, error) {
	if !v1.CarriesSpeech(utterance) {
		return Extraction{Kind: "none"}, nil
	}
	answer, err := extractor.generator.Generate(
		ctx, ExtractionInstruction, RenderForExtraction(existing, utterance), 96)
	if err != nil {
		return Extraction{}, err
	}
	kind, instruction, ok := ParsePin(answer)
	if !ok {
		return Extraction{}, fmt.Errorf("extraction returned %q, which is not an answer", truncateAnswer(answer))
	}
	return Extraction{Kind: kind, Instruction: instruction}, nil
}

func truncateAnswer(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 80 {
		return text[:80] + "…"
	}
	return text
}
