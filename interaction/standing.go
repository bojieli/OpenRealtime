package interaction

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// Counting says this policy asks for a running count, which is the one
	// distinction that has turned out to change what the voice must be told
	// about carrying a policy out: a count needs the arithmetic spelled out,
	// and attaching that to every running commentary taught a waiter scenario
	// to count - asked to order the dish that fits, the agent said "4 4 4".
	//
	// Set by the pass that reads the policy, from the person's own words,
	// rather than by a list of nouns chosen after watching one benchmark.
	Counting bool
	// Restricting says the policy closes off everything it did not ask for:
	// "and say nothing else", "only the number", "don't say anything apart
	// from that". It is a fact about the whole channel rather than about the
	// thing being watched for, which is why it changes the acts on offer and
	// not just how one of them is carried out.
	Restricting bool
	// Turn identifies the stretch of speech that set this policy. Policies are
	// replaced a turn at a time rather than one at a time, because the pass
	// reads a turn many times as it grows and each reading answers for the
	// whole of it.
	Turn uint64
	// After is the delay a policy names, when it names one: "ask if I go quiet
	// for fifteen seconds" is not due until fifteen seconds of quiet have
	// passed. Zero means the policy is about something happening rather than
	// about time.
	//
	// It is read once, here, rather than compared on every decision by the
	// model. Measured against the model that takes those decisions, at four
	// seconds of silence and at forty it chose to speak five times out of five
	// either way; a paragraph telling it to compare the numbers fixed one
	// phrasing of the transcript and left a near-identical one still wrong.
	// A capability that turns on how somebody phrased a sentence is not one.
	//
	// The division that does work: a model reads the language, and the runtime
	// compares the numbers. Reading "fifteen seconds" out of a sentence is
	// what the extraction pass is already for, and it does it once, off the
	// critical path, with the whole utterance in front of it.
	After time.Duration
}

// Due reports whether a policy that waits on a stretch of quiet has had it.
//
// A policy that names no delay is always due; what it waits for is something
// happening, and that is the model's to recognise.
func (instruction StandingInstruction) Due(quiet time.Duration) bool {
	return instruction.After <= 0 || quiet >= instruction.After
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
	recent    []string
	utterance string
	answer    string
}

func extractionExamples() []extractionExample {
	watching := []StandingInstruction{{Text: "shout if you see the train coming", Scope: ScopeConversation}}
	return []extractionExample{
		{nil, nil, "Book me a table for four at eight.", "none"},
		{nil, nil, "Keep your answers to a sentence or two.", "none"},
		{nil, nil, "Shout if you see the train coming.", "pin conversation shout if you see the train coming"},
		{nil, nil, "Hang on, I haven't got to the point yet.", "pin turn do not reply until they have made their point"},
		// The pair that turn scope keeps getting wrong. Both of these bound
		// themselves to something the speaker is about to do - "as I read it
		// out", "until I finish" - and only one of them is about this moment.
		// Measured, the first was pinned to the turn five times out of five,
		// which is over in seconds, and the policy governed nothing for the
		// rest of the story.
		{nil, nil, "I'll read out the numbers - add them up as I go and say the running total.",
			"pin conversation say the running total each time they read out a number"},
		{nil, nil, "Let me finish reading this out before you say anything.",
			"pin turn do not reply until they have finished reading it out"},
		{watching, nil, "Forget about the train, I can see it now.", "revoke shout if you see the train coming"},
		{watching, nil, "Also let me know if it starts raining.", "pin conversation say something if it starts raining"},
		{nil, nil, "Give me a nudge if I start talking too fast.", "pin conversation tell them if they start talking too fast"},
		{nil, nil, "Never talk over me when I'm reading something out.", "pin conversation do not speak while they are reading something out"},
		{watching, []string{"user: Shout if you see the train coming.", "agent: Will do."},
			"And nothing else, please.", "none"},
		// Ordinary speech while a policy stands. The bias towards pinning read
		// this as the policy being set, pinned a second one worded
		// differently, and the pinboard - comparing text - kept both. Measured,
		// one instruction ended up as three policies and the agent counted
		// every occurrence once per policy.
		{[]StandingInstruction{{Text: "say the running total each time they read out a number",
			Scope: ScopeConversation}},
			[]string{"user: I'll read out the numbers, give me the running total.", "agent: Will do."},
			"Four hundred and twelve.", "none"},
	}
}

func buildExtraction() string {
	var text strings.Builder
	text.WriteString(
		"Someone in a voice conversation is speaking. Decide which policies about *when* the agent should " +
			"speak or act their words below establish, and reply with one line for each and nothing else.\n\n" +
			"Answer for all of what they said below, not just the last part of it. You are shown everything " +
			"they have said since the agent last spoke, and you are shown it again each time they add to it, " +
			"so list every policy that this stretch of speech sets - all of them, every time, because your " +
			"answer replaces whatever you said about this same stretch before. One left out is one they no " +
			"longer have.\n\n" +
			"Policies set earlier are already on the list above and are not yours to repeat. Only what the " +
			"words below set. If they set nothing, that is none, however much is already in force.\n\n" +
			"none - they set no such policy. This is the usual answer, and it is a whole answer on its own.\n" +
			"pin conversation <policy> - they set one that stands from now until somebody lifts it.\n" +
			"pin turn <policy> - they set one that expires when they finish what they are currently saying.\n" +
			"revoke <policy> - they lifted one already in force. Quote the one they lifted, from the list above.\n\n" +
			"When the policy waits on a stretch of quiet and says how long, put that first as \"after <n>s\": " +
			"\"if I go quiet for fifteen seconds, ask whether I'm still there\" is " +
			"\"pin conversation after 15s ask whether they are still there\". Only for a length of silence, " +
			"and only when they named one - a policy waiting on something happening does not take it.\n\n" +
			"A policy names a trigger: a condition to watch for, and speech or action to produce when it happens. " +
			"\"Stop me if I get a date wrong\", \"tell me the moment it lands\", \"count them out loud as I " +
			"mention them\" each name one. Work names no trigger - \"finish the report by Friday\" says what to " +
			"do and never when to say it. Neither does style: \"give me shorter answers\" and \"always include " +
			"the order number\" change the shape of replies the agent was going to make anyway, and start none " +
			"of them. If nothing new would ever cause the agent to speak, it is not a policy.\n\n" +
			"For scope, ask whether they stated a rule or made a request about this moment. A rule describes " +
			"how things should go from now on and survives the sentence that stated it - never talk over me, " +
			"always tell me when it lands, count them as I go - and is a conversation policy however briefly " +
			"it was put. A request about this moment dies when the moment does: hang on, wait, not yet, let me " +
			"finish this thought. Watching for something that has not happened yet is always a rule, because " +
			"the thing being watched for has not happened yet.\n\n" +
			"Phrasing decides this, not subject. \"Don't interrupt me\" and \"hang on, I'm not finished\" are " +
			"both about interruption and are not the same scope: the first says how the conversation should " +
			"go, and the second asks for a few more seconds.\n\n" +
			"A sentence can arrive in pieces, because a recogniser splits where a speaker breathes. Read the " +
			"utterance against what was just said: a fragment that continues the previous sentence qualifies " +
			"it and does not replace it. Revoke only when they are plainly taking back something on the list " +
			"above, not when they are still finishing the thought that put it there.\n\n" +
			"The punctuation is the recogniser's, not theirs. It writes a full stop wherever they paused for " +
			"breath, so one request often reads as two sentences with the second starting mid-thought - " +
			"\"count the animals out loud as I mention them. and say nothing else.\" is one instruction, and " +
			"the part after the full stop is the half that says what not to do. Read a trailing clause as " +
			"part of what precedes it and keep what it adds: a policy written back without the restriction " +
			"they attached is not the policy they set.\n\n" +
			"An immediate command is not a policy. The test is whether obeying it takes one action or requires " +
			"watching for something: \"stop\" is finished the moment it is obeyed and is none, while \"let me " +
			"finish\" means staying quiet until a condition holds and is a policy for this turn. Neither is a " +
			"revocation: revoke is only for lifting something on the list above.\n\n" +
			"A policy already on the list is in force, and somebody talking on under it is not setting it " +
			"again. Once it is there, the answer for everything they say while it stands is none, unless they " +
			"are lifting it or setting something new. This matters more than it looks: two policies asking for " +
			"the same thing are two triggers, and the agent does the thing twice for every occurrence - so a " +
			"running count pinned twice counts every animal twice.\n\n" +
			"Prefer pinning to missing, for a policy that is not already there. One set and not recorded fails " +
			"silently and looks like the agent ignoring someone; one recorded that turns out not to apply is " +
			"simply never triggered.\n\n" +
			"Write the policy back as a short instruction to the agent, in the speaker's own words where you " +
			"can.\n\nWorked examples. These are other conversations, not this one.\n")
	for _, example := range extractionExamples() {
		text.WriteString("\n---\n" + RenderForExtraction(example.existing, example.recent, example.utterance) +
			"\n-> " + example.answer + "\n")
	}
	text.WriteString("\n---\nNow decide the case below. One line per policy, and nothing else.")
	return text.String()
}

// RenderForExtraction is what the extraction pass reads.
//
// It carries the policies already in force, without which a revocation is
// invisible: "never mind about the kettle" is only recognisable as lifting
// something if the kettle instruction is on the page. Both models tested
// answered "none" to every revocation until they were shown what there was to
// revoke.
//
// It carries the recent conversation for the opposite reason. A recogniser
// splits sentences where a speaker breathes, so an instruction arrives in
// pieces, and a piece read alone means something else entirely: "count the
// animals out loud, as I mention them" followed by "and say nothing else"
// produced a pin and then a revocation of that same pin two seconds later.
// Which is a fair reading of the fragment and a wrong one of the sentence.
func RenderForExtraction(existing []StandingInstruction, recent []string, utterance string) string {
	var block strings.Builder
	if len(recent) > 0 {
		block.WriteString("Recent conversation:\n")
		for _, line := range recent {
			block.WriteString(line + "\n")
		}
		block.WriteString("\n")
	}
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
// ParseExtraction reads the whole answer: one line per policy.
func ParseExtraction(text string) (Extraction, error) {
	var extraction Extraction
	var read bool
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, instruction, ok := ParsePin(line)
		if !ok {
			return Extraction{}, fmt.Errorf("extraction returned %q, which is not an answer", truncateAnswer(line))
		}
		read = true
		switch kind {
		case "pin":
			extraction.Pins = append(extraction.Pins, instruction)
		case "revoke":
			extraction.Revokes = append(extraction.Revokes, instruction.Text)
		}
	}
	if !read {
		return Extraction{}, fmt.Errorf("extraction returned %q, which is not an answer", truncateAnswer(text))
	}
	return extraction, nil
}

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
		return "pin", withDelay(StandingInstruction{Text: strings.TrimSpace(line[len("pin turn "):]), Scope: ScopeTurn}), true
	case strings.HasPrefix(lowered, "pin conversation "):
		return "pin", withDelay(StandingInstruction{Text: strings.TrimSpace(line[len("pin conversation "):]), Scope: ScopeConversation}), true
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
	//
	// The utterance is what the speaker said, whole. A recogniser cuts where
	// somebody breathes, and joining the pieces back up before they get here
	// is the caller's job - a tail read on its own means something else.
	Extract(ctx context.Context, existing []StandingInstruction, recent []string, utterance string) (Extraction, error)
}

// Extraction is what a turn did to the set of standing policies.
//
// Every policy the turn sets, not the first one a reading happened to notice.
// One-at-a-time was the source of a class of failures rather than a detail of
// the format: the pass reads a turn over and over as the words arrive, so the
// runtime saw a sequence of single answers and had to guess whether each
// replaced the last or joined it. Both guesses are wrong somewhere - joining
// accumulates one request into three policies that each fire, replacing
// destroys a real second policy - and there is no third guess. Answering for
// the whole turn removes the question.
type Extraction struct {
	Pins    []StandingInstruction
	Revokes []string
}

// NewExtractor builds the pass over a generator.
func NewExtractor(generator Generator) (Extractor, error) {
	if generator == nil {
		return nil, errors.New("an extractor requires a generator")
	}
	return &modelExtractor{generator: generator}, nil
}

type modelExtractor struct {
	generator Generator
	// read caches what the three questions about a policy answered, keyed by
	// its words.
	//
	// A turn is re-read every time the speaker adds to it, and each reading
	// lists the same policies again, so classifying on every pin meant three
	// model calls per policy per reading - dozens of them, on the same local
	// model the voice and the interaction model are waiting on. Measured, that
	// took hearing from half a second to four, and a count that was correct
	// arrived two seconds after the window it was correct for.
	//
	// What the questions ask about is the policy's own words, which do not
	// change. Asked once, they stay answered.
	read sync.Map
}

// policyReading is what the three questions found.
type policyReading struct {
	counting    bool
	restricting bool
	scope       Scope
}

func (extractor *modelExtractor) Name() string { return "extract:" + extractor.generator.Name() }

// Extract runs the pass and refuses to guess.
//
// An unparseable answer produces nothing rather than a policy nobody set.
// The asymmetry elsewhere runs the other way - a missed policy fails silently
// and a spurious one is merely never triggered - but that argument is about
// what the model should lean towards, not about what this should invent when
// the model has said something it does not understand.
func (extractor *modelExtractor) Extract(
	ctx context.Context, existing []StandingInstruction, recent []string, utterance string,
) (Extraction, error) {
	if !v1.CarriesSpeech(utterance) {
		return Extraction{}, nil
	}
	answer, err := extractor.generator.Generate(
		ctx, ExtractionInstruction, RenderForExtraction(existing, recent, utterance), 160)
	if err != nil {
		return Extraction{}, err
	}
	extraction, err := ParseExtraction(answer)
	if err != nil {
		return Extraction{}, err
	}
	for index := range extraction.Pins {
		reading := extractor.readingOf(ctx, extraction.Pins[index])
		extraction.Pins[index].Counting = reading.counting
		extraction.Pins[index].Restricting = reading.restricting
		extraction.Pins[index].Scope = reading.scope
	}
	return extraction, nil
}

// CountingInstruction asks what kind of thing a policy is asking for.
//
// One question, because only one distinction has turned out to change what the
// voice needs to be told: a running count needs arithmetic spelled out - count
// them all again, compare with the last number, say it only if it went up -
// and attaching that to every running commentary taught a waiter scenario to
// count. Asked to order the dish that fits, the agent said "4 4 4".
//
// A model reading the person's own words rather than a list of nouns I chose
// after watching one benchmark. The words people use for this are not
// enumerable: count them, keep a tally, say how many so far, give me the
// running total, number them as they come.
var CountingInstruction = "Somebody set a standing policy for a voice assistant. Does it ask for a " +
	"running count of things - a number that goes up as more of them are mentioned? Answer yes or no " +
	"and nothing else.\n\n" +
	"yes: count the animals out loud as I mention them\n" +
	"yes: keep a tally and tell me where we are each time I add one\n" +
	"yes: say how many we are up to whenever another one comes in\n" +
	"no: order the dish that fits when the waiter names one\n" +
	"no: translate what he says into English as he goes\n" +
	"no: tell me the moment the build finishes\n" +
	"no: stop me if I quote a price under fifty\n\n" +
	"The policy:"

// asksForACount reads the policy once, when it is pinned.
//
// Once per policy rather than once per turn, because policies are set rarely
// and turns happen many times a second. An unreadable answer is "no": the
// arithmetic is help for one kind of policy, and withholding it from a count
// costs a scenario while attaching it to everything else costs several.
func (extractor *modelExtractor) asksForACount(ctx context.Context, policy string) bool {
	if strings.TrimSpace(policy) == "" {
		return false
	}
	answer, err := extractor.generator.Generate(ctx, CountingInstruction, policy, 4)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "yes")
}

// RestrictingInstruction asks whether a policy closes off everything else.
//
// People attach this to a policy constantly - count them and say nothing else,
// just the translation, only tell me the price - and it is not a footnote to
// the thing they asked for. It withdraws the ordinary reply. Measured, an
// agent under "count the animals and say nothing else" answered the pause at
// the end of a sentence with no animal in it, twice, because somebody
// stopping mid-story is an overwhelming case for a reply and one line of
// standing instruction does not outweigh it.
//
// Read from the person's own words, like the count, because the phrasings are
// not enumerable and a list of them would be a list I wrote after watching one
// benchmark.
var RestrictingInstruction = "Somebody set a standing policy for a voice assistant.\n\n" +
	"Does the policy explicitly forbid saying anything besides the thing it asks for? There has to be " +
	"an actual exclusion in their words - and nothing else, only that, just the number, don't say " +
	"anything apart from it. A policy that simply names one thing to do is not an exclusion, however " +
	"narrow the thing is. A policy about when to speak or when to stay out of the way is not an " +
	"exclusion either: it restricts the timing, not what may be said.\n\n" +
	"Answer yes or no and nothing else.\n\n" +
	"yes: count the animals out loud as I mention them, and say nothing else\n" +
	"yes: just give me the translation, nothing around it\n" +
	"yes: only say the price, no commentary\n" +
	"no: count the animals out loud as I mention them\n" +
	"no: tell me the moment the build finishes\n" +
	"no: wait until I have finished before you say anything\n\n" +
	"The policy:"

// restrictsEverythingElse reads the policy once, when it is pinned.
//
// An unreadable answer is "no". Withholding the restriction leaves an agent
// too talkative, which is the failure the person can hear and correct; adding
// one nobody asked for leaves it mute for a reason they cannot see.
func (extractor *modelExtractor) restrictsEverythingElse(ctx context.Context, policy string) bool {
	if strings.TrimSpace(policy) == "" {
		return false
	}
	answer, err := extractor.generator.Generate(ctx, RestrictingInstruction, policy, 4)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "yes")
}

// ScopeInstruction asks how long a policy lasts.
//
// The extraction pass decides this badly and the reason is structural: it is
// answering several questions at once about a turn it is re-reading, and scope
// is the one where the words give least away. Measured, "count the animals out
// loud as I mention them, and say nothing else" came back turn-scoped, which
// expires the moment the speaker finishes a sentence - so the policy was gone
// before the first animal, nothing was in force, and the agent fell back on an
// ordinary reply.
//
// Asked on its own, about one policy, it is the same kind of question as the
// count and the restriction, and gets the same kind of answer.
var ScopeInstruction = "A policy was set for a voice assistant. Does it stand from now on, until somebody " +
	"lifts it, or does it expire as soon as the speaker finishes what they are currently saying?\n\n" +
	"Answer standing or passing, and nothing else.\n\n" +
	"Watching for something that has not happened yet is always standing, because the thing being " +
	"watched for has not happened yet. Asking for a few more seconds is passing.\n\n" +
	"standing: count the animals out loud each time they mention one\n" +
	"standing: tell them the moment the build finishes\n" +
	"standing: never speak while they are reading something out\n" +
	"standing: say the running total each time they read out a number\n" +
	"passing: do not reply until they have made their point\n" +
	"passing: do not reply until they have finished reading it out\n" +
	"passing: wait, they have not got to the point yet\n\n" +
	"The policy:"

// scopeOf reads how long one policy lasts, once, when it is pinned.
//
// An unreadable answer keeps what the extraction pass said. This leans towards
// standing on its own: a passing policy wrongly left standing keeps an agent
// quiet until somebody tells it to speak, which they can do, while a standing
// one wrongly expired fails silently at the moment it was set for.
func (extractor *modelExtractor) scopeOf(ctx context.Context, instruction StandingInstruction) Scope {
	if strings.TrimSpace(instruction.Text) == "" {
		return instruction.Scope
	}
	// A policy that names a delay is waiting on something that has not
	// happened yet, which this question's own rule makes standing. It must not
	// be asked, because the delay has been lifted out of the text by the time
	// this sees it: "if I go quiet for fifteen seconds, ask whether I'm still
	// there" arrives here as "ask whether they are still there", which reads
	// as a request about this moment and came back passing - so the policy
	// expired with the turn and the check that fifteen seconds later nobody
	// had said anything failed five times out of five.
	if instruction.After > 0 {
		return ScopeConversation
	}
	answer, err := extractor.generator.Generate(ctx, ScopeInstruction, instruction.Text, 4)
	if err != nil {
		return instruction.Scope
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "standing":
		return ScopeConversation
	case "passing":
		return ScopeTurn
	}
	return instruction.Scope
}

// readingOf answers the three questions about a policy, once.
func (extractor *modelExtractor) readingOf(ctx context.Context, instruction StandingInstruction) policyReading {
	key := strings.ToLower(strings.TrimSpace(instruction.Text))
	if cached, ok := extractor.read.Load(key); ok {
		return cached.(policyReading)
	}
	reading := policyReading{
		counting:    extractor.asksForACount(ctx, instruction.Text),
		restricting: extractor.restrictsEverythingElse(ctx, instruction.Text),
		scope:       extractor.scopeOf(ctx, instruction),
	}
	// Only a complete reading is kept. A model call that failed answers with
	// the safe default, and caching that would make one bad moment permanent.
	if ctx.Err() == nil {
		extractor.read.Store(key, reading)
	}
	return reading
}

func truncateAnswer(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 80 {
		return text[:80] + "…"
	}
	return text
}

// withDelay lifts a leading "after <n>s" off a pinned policy.
//
// The delay is written into the answer rather than left in the text, because
// what the runtime needs is a number and what the voice needs is the sentence.
// Both come out of the one reading.
func withDelay(instruction StandingInstruction) StandingInstruction {
	rest, ok := strings.CutPrefix(strings.ToLower(instruction.Text), "after ")
	if !ok {
		return instruction
	}
	end := strings.IndexByte(rest, ' ')
	if end <= 0 {
		return instruction
	}
	seconds, err := strconv.Atoi(strings.TrimSuffix(rest[:end], "s"))
	if err != nil || seconds <= 0 {
		return instruction
	}
	instruction.After = time.Duration(seconds) * time.Second
	instruction.Text = strings.TrimSpace(instruction.Text[len("after ")+end:])
	return instruction
}
