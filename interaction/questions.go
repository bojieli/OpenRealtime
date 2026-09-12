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
	AnswerYes          = "yes"
	AnswerNo           = "no"
)

// StopQuestion is asked while the agent is speaking: does the person cut in?
var StopQuestion = StepQuestion{Name: QuestionStop, Text: "Do the new words show the person interrupting or " +
	"correcting the agent (\"hold on\", \"wait\", \"actually\", \"no, ...\")?"}

// OccurrenceQuestion is asked on every step: has a standing instruction just
// come due in the new words?
var OccurrenceQuestion = StepQuestion{Name: QuestionOccurrence, Text: "Look at \"new words since the last " +
	"step\". Do they contain a NEW occurrence of what a standing instruction watches for (for example a newly " +
	"mentioned animal for a running count) that is not already covered by what was answered in this utterance " +
	"or by a recent step that spoke?"}

// RequestQuestion is asked when the person has stopped and nothing came due:
// is there something to answer?
var RequestQuestion = StepQuestion{Name: QuestionRequest, Text: "This is the final of what the person said. " +
	"Is it a question or a request to the agent that no recent step already answered (not a story, not small " +
	"talk, not a rule being set up)?"}

// YesNo is the option set of every step question.
func YesNo() []string { return []string{AnswerYes, AnswerNo} }

// ComposeChoice is the Choice the answers add up to. Only questions that
// were asked appear in answers; an unasked question counts as no.
func ComposeChoice(speaking bool, answers map[string]bool) Choice {
	return Choice{
		Speaking: speaking,
		Stop:     speaking && answers[QuestionStop],
		Speak:    answers[QuestionOccurrence] || answers[QuestionRequest],
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
	case QuestionOccurrence, QuestionRequest:
		if choice.Speak {
			return AnswerYes
		}
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
