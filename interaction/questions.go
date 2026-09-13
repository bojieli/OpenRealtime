package interaction

import "strings"

// The policy is asked about one step in narrow questions, and the runtime
// composes the Choice from the answers.
//
// It is asked this way because of what one page of rules and a single
// constrained answer measured: the fast policy model chose speak on "My
// brother", "Then a", "Count the" - two-word partials with nothing to answer
// - at 0.9 to 1.0 confidence whatever the rules said, 13 of 37 recorded
// decisions right under the best of eleven layouts. Asked the same evidence as
// yes/no questions about one line each, the same model got 33 to 34 of 37.
// A small instruct model answers a concrete question about a concrete line;
// it does not fold a page of rules into a token. Every step is still put to
// the model - nothing is decided ahead of it - and each answer is recorded,
// so a wrong choice traces to the one question that went wrong.
type StepQuestion struct {
	// Name identifies the question in decisions and traces.
	Name string
	// Text is what the model is asked, after the evidence.
	Text string
}

const (
	QuestionStop       = "stop"
	QuestionOccurrence = "occurrence"
	QuestionRequest    = "request"
	QuestionQuiet      = "quiet"
	QuestionElsewhere  = "elsewhere"
	QuestionUrgent     = "urgent"
	AnswerYes          = "yes"
	AnswerNo           = "no"
)

// StopQuestion is asked while the agent is speaking: does the person cut in?
var StopQuestion = StepQuestion{Name: QuestionStop, Text: "Do the new words show the person interrupting or " +
	"correcting the agent (\"hold on\", \"wait\", \"actually\", \"no, ...\")?"}

// OccurrenceQuestion is asked on every step: has a standing instruction just
// come due in the new words?
var OccurrenceQuestion = StepQuestion{Name: QuestionOccurrence, Text: "First check the \"Standing " +
	"instructions\" list above. If it says none are in force, the answer is no, whatever the person is " +
	"saying: nothing can be an occurrence of a rule that is not in force, the agent's purpose is not a rule, " +
	"and the words that set a rule up (\"translate everything he says\", \"count the animals\") are not " +
	"an occurrence of it. Otherwise look at \"new words since the last step\", and at anything earlier in " +
	"\"heard from user so far\" that no recent step answered (a name a listening step passed over, or one a " +
	"step spoke for while the agent then said nothing, is still unanswered). Do they " +
	"contain a NEW, specific occurrence of what a standing instruction listed above watches for - one named " +
	"or clearly identified there (a newly mentioned animal for a running count; speech in the other language " +
	"for a translation rule; a menu option, a dish, or a date that matches or contradicts what the rule names; " +
	"a phrase like \"the open models\" or \"those animals\" names no specific one) - that is not already " +
	"covered by what was answered in this utterance or by a recent step that spoke? For a translation rule " +
	"every new word in the other language not yet translated is an occurrence, the last words of a sentence " +
	"included: new words \"见面。\" after \"我们明天下午三点在办公室\" was already answered are an occurrence, " +
	"even while the agent is still saying the earlier part. Words that ask the agent to start, resume, pause, or stop the rule itself (\"carry on\", " +
	"\"hold on\", \"go ahead\") are not an occurrence; they are a request, answered when the person " +
	"finishes. Once a recent step shows the agent did what the rule asks for this utterance (pressed the key " +
	"for the option the user wanted), the rest of the same utterance - other options the menu goes on to " +
	"list - is not a new occurrence."}

// SightQuestion replaces OccurrenceQuestion when the event is a picture: the
// words are only a caption, and what happened is in the frame.
var SightQuestion = StepQuestion{Name: QuestionOccurrence, Text: "The agent was just shown a picture (attached, " +
	"or described under \"just seen\"). Does it show that what a standing instruction listed above watches " +
	"for has now happened - a build finished, an error appeared - rather than still under way, and has the " +
	"agent not already reported it? If no standing instruction is listed, answer no."}

// RequestQuestion is asked on a final when nothing standing came due: is
// there something to answer? The short-question clause is there because a
// person in the room set up a rule and asked "Are you ready?" twice, and
// the policy refused it both times (P(yes) 0.41); replayed against every
// recorded request decision, this clause answers it at 0.88 and changes
// nothing else - a wording that also acknowledged "Call them and find out
// where my order has got to" put that acknowledgement under the recording
// that follows it.
var RequestQuestion = StepQuestion{Name: QuestionRequest, Text: "This is the final of what the person said. " +
	"Is it a question or a request that no recent step already answered? If \"already answered in this " +
	"utterance\" shows words, or a recent step spoke for these same words, the agent is already answering " +
	"it: answer no unless the final adds a different request. A story, small talk, a rule being set up, or " +
	"a recording reading out options is not a request. A question put to the agent is a request wherever it " +
	"comes: \"By the way, what is seven times eight?\" in the middle of a story or a running count is one. " +
	"So is a short question that expects the agent to answer - \"Are you ready?\", \"Did you get that?\", " +
	"\"Can you hear me?\" - even right after a rule was set up."}

// UrgentQuestion is asked on a partial when no standing instruction is in
// force: nothing can come due, so the one thing a half-sentence can be is
// something that cannot wait for the person to finish.
var UrgentQuestion = StepQuestion{Name: QuestionUrgent, Text: "No standing instruction is in force and the " +
	"person is still mid-sentence. Do the words so far demand that the agent act before they finish - a " +
	"warning, an emergency, \"stop!\" - rather than something the agent can answer or carry out once " +
	"they have finished? A request, a question, a rule being set up, or a story is not urgent. Answer yes " +
	"only if waiting for the end of the sentence would be wrong."}

// ElsewhereQuestion is asked once a final reads as a request: was it put to
// the agent at all? Worded around what the agent is - a voice program with
// no body - rather than around examples of exchanges: replayed against
// every recorded addressee decision, the example-led wording answered "no"
// to "Did you get the milk on the way in?" asked on its own (P(yes) 0.27)
// while this one answers yes at 0.97 and keeps every real request at no.
var ElsewhereQuestion = StepQuestion{Name: QuestionElsewhere, Text: "These words read as a question or a " +
	"request. Who are they for? The agent is a voice program: it has no body, was not out, did not come in, " +
	"and carries nothing. A question about what somebody did, brought, or saw (\"Did you get the milk on the " +
	"way in?\") is for a person in the room, as are words that answer another person, carry on their own " +
	"exchange, or that a recording reads out. A question the agent can answer or a thing it can do - a fact, " +
	"arithmetic, its own task, something it should count, translate, watch for, call, or go on with - is for " +
	"the agent. Answer yes if the words are for somebody else, not the agent."}

// ReplyQuestion replaces ElsewhereQuestion when the previous line was a
// question the agent chose not to answer: asked whether the words are "for
// somebody else" the model kept answering the request; asked whether they
// reply to that question, it can read the answer off the words.
var ReplyQuestion = StepQuestion{Name: QuestionElsewhere, Text: "These words read as a question or a " +
	"request. The evidence says the previous line was a question the agent chose not to answer because it " +
	"was not for the agent. Do these new words reply to that question, or carry on the same exchange between " +
	"the people in the room (\"No, I forgot again. Can you put it on the list?\" replies to \"Did you get " +
	"the milk?\")? If so they are for the person who asked, not for the agent. Answer yes if they reply to " +
	"it or carry it on."}

// QuietQuestion is asked when a stretch of silence is the event: nobody said
// or showed anything, and a standing instruction that waits on quiet may have
// come due.
var QuietQuestion = StepQuestion{Name: QuestionQuiet, Text: "Nothing has been said or seen for the silence " +
	"shown under \"Now\". Does a standing instruction listed above ask the agent to do something once this " +
	"much quiet has passed, that the agent has not already done since the person last spoke?"}

// YesNo is the option set of every step question.
func YesNo() []string { return []string{AnswerYes, AnswerNo} }

// ComposeChoice is the Choice the answers add up to. Only questions that
// were asked appear in answers; an unasked question counts as no.
func ComposeChoice(speaking bool, answers map[string]bool) Choice {
	return Choice{
		Speaking: speaking,
		Stop:     speaking && answers[QuestionStop],
		Speak: answers[QuestionOccurrence] || answers[QuestionQuiet] || answers[QuestionUrgent] ||
			(answers[QuestionRequest] && !answers[QuestionElsewhere]),
	}
}

// AnswerFor is the answer a decider gives one question when it means to make
// one choice: the inverse of ComposeChoice, for fixtures and replays that
// script a Choice rather than answers.
func AnswerFor(question string, choice Choice) string {
	switch question {
	case QuestionStop:
		if choice.Stop {
			return AnswerYes
		}
	case QuestionOccurrence, QuestionRequest, QuestionQuiet, QuestionUrgent:
		if choice.Speak {
			return AnswerYes
		}
	case QuestionElsewhere:
		// A choice that speaks was addressed to the agent; one that listens
		// already answered the request question with no.
		return AnswerNo
	}
	return AnswerNo
}

// AskedQuestion records one question and its answer.
type AskedQuestion struct {
	Question   string  `json:"question"`
	Answer     string  `json:"answer"`
	Confidence float64 `json:"confidence,omitempty"`
	Measured   bool    `json:"measured,omitempty"`
	DurationMS float64 `json:"duration_ms,omitempty"`
}

// DescribeAnswers renders answers for a trace line: stop=no occurrence=yes.
func DescribeAnswers(asked []AskedQuestion) string {
	parts := make([]string, 0, len(asked))
	for _, question := range asked {
		parts = append(parts, question.Question+"="+question.Answer)
	}
	return strings.Join(parts, " ")
}
