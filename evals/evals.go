// Package evals is the step-by-step layer under the benchmark suites.
//
// A suite in bench/ plays a whole recording and scores the outcome. That is
// the end-to-end layer, and it answers "did the task work". It is slow, it is
// noisy at small n, and when it fails it does not say which decision went
// wrong - a task scored "no tool call" may have failed at perception, at the
// hand-off, at the reasoner, or at none of them.
//
// A case here freezes everything up to one decision and asks only for the next
// observable action. It runs in milliseconds against one provider rather than
// minutes against a whole stack, so a change can be judged before it is
// shipped, and a failure names the boundary it happened at.
//
// Two rules keep these honest, and both come from the same place: an
// evaluation that scores one canonical answer measures conformity rather than
// correctness.
//
//   - A case declares an *acceptable set*, not an answer. Asking the user,
//     handing the turn on, and refusing a dangerous action can all be right at
//     the same boundary, and a case that admits one of them is testing a
//     preference.
//   - A case may declare *forbidden* actions, and those are vetoes. Claiming a
//     result nobody supplied is not a low score, it is a failure whatever else
//     the turn did well: the user acts on the claim.
package evals

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
)

// Decision is the boundary a case is frozen at. Each names one question the
// runtime asks a model, so a failing case says which question was answered
// wrongly rather than that a conversation went badly.
type Decision string

const (
	// DecisionHandOff is the voice deciding whether the turn needs the
	// reasoning half. Every tool call in the cascade depends on it.
	DecisionHandOff Decision = "hand-off"
	// DecisionProjection is the turn-projection model deciding whether a
	// person who has gone quiet has finished.
	DecisionProjection Decision = "projection"
	// DecisionIdentifier is the reasoner reassembling an identifier a
	// recogniser wrote down the way it was spoken.
	DecisionIdentifier Decision = "identifier"
	// DecisionInteraction is the model that decides what the agent does in an
	// instant rather than what it says. It is the only boundary here whose
	// correct answer usually is to do nothing, which is why its cases are
	// scored as two numbers rather than one.
	DecisionInteraction Decision = "interaction"
	// DecisionStandingInstruction is the pass that notices when someone has
	// set an interaction policy out loud - wait, let me finish; tell me the
	// moment it lands - and pins it where a truncated window cannot repeal it.
	DecisionStandingInstruction Decision = "standing-instruction"
	// DecisionResult is the voice reporting what a tool came back with. It is
	// where a caller is most likely to be misled and least likely to notice:
	// the work really was done, so the sentence sounds authoritative whatever
	// it says.
	DecisionResult Decision = "result"
)

// Action is one observable thing a model did at a boundary.
type Action string

const (
	ActionHandOn    Action = "hand-on"    // left the turn to the reasoning half
	ActionFinish    Action = "finish"     // declared the turn complete
	ActionAsk       Action = "ask"        // asked the user for something
	ActionDraft     Action = "draft"      // proposed a tool call
	ActionContinue  Action = "continuing" // judged the speaker unfinished
	ActionFinished  Action = "finished"   // judged the speaker done
	ActionFabricate Action = "fabricate"  // stated a result nobody supplied
	ActionWrongID   Action = "wrong-id"   // altered a user-supplied identifier
	ActionSilent    Action = "silent"     // produced nothing
	ActionOverclaim Action = "overclaim"  // said more than the tool result said
)

// Case is one frozen decision.
type Case struct {
	Name     string
	Decision Decision
	// Context is what the model is given: the conversation so far, or the
	// partial transcript and the silence, depending on the boundary.
	Context string
	// Accept is the set of actions that are right here. Any one of them
	// passes; a case with a single-element Accept is asserting there is
	// genuinely one right answer, which is rare.
	Accept []Action
	// Forbid vetoes. A forbidden action fails the case whatever else happened.
	Forbid []Action
	// Note explains what the case is really testing, and appears in reports.
	Note string
}

// Observation is what a runner saw the model do.
type Observation struct {
	Actions []Action
	Text    string
	Elapsed time.Duration
	Err     error
}

// Runner drives one boundary against one provider.
type Runner interface {
	Name() string
	Decision() Decision
	Observe(context.Context, Case) Observation
}

// Outcome is one case scored.
type Outcome struct {
	Case
	Observation
	Passed  bool
	Vetoed  bool
	Because string
}

// Score applies the acceptable set and the vetoes.
func Score(item Case, observed Observation) Outcome {
	outcome := Outcome{Case: item, Observation: observed}
	if observed.Err != nil {
		outcome.Because = "provider error: " + observed.Err.Error()
		return outcome
	}
	for _, forbidden := range item.Forbid {
		if hasAction(observed.Actions, forbidden) {
			outcome.Vetoed = true
			outcome.Because = "forbidden: " + string(forbidden)
			return outcome
		}
	}
	for _, allowed := range item.Accept {
		if hasAction(observed.Actions, allowed) {
			outcome.Passed = true
			outcome.Because = "accepted: " + string(allowed)
			return outcome
		}
	}
	outcome.Because = fmt.Sprintf("did %v, needed one of %v", observed.Actions, item.Accept)
	return outcome
}

func hasAction(actions []Action, want Action) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// Report is a run of one runner over a set of cases.
type Report struct {
	Runner   string
	Decision Decision
	Outcomes []Outcome
}

// Run scores every case, in order, and never stops at the first failure: the
// point of a step-by-step layer is to see the whole shape of a regression.
func Run(ctx context.Context, runner Runner, cases []Case) Report {
	report := Report{Runner: runner.Name(), Decision: runner.Decision()}
	for _, item := range cases {
		if item.Decision != runner.Decision() {
			continue
		}
		report.Outcomes = append(report.Outcomes, Score(item, runner.Observe(ctx, item)))
	}
	return report
}

// Summary counts what happened, with vetoes separated from ordinary failures
// because they mean something different: a veto is a turn that would have
// misled somebody.
type Summary struct {
	Total, Passed, Vetoed, Errored int
	Slowest                        time.Duration
	// Acting and Restraint split the cases by what the right answer was.
	//
	// A single pass rate hides the only failure mode that matters at a
	// boundary whose commonest correct answer is to do nothing: a model that
	// always acts and a model that never acts can post identical totals, and
	// they are opposite bugs needing opposite fixes. Measured separately they
	// are impossible to confuse, and the first two runs of the interaction
	// suite were exactly that pair.
	ActingTotal, ActingPassed       int
	RestraintTotal, RestraintPassed int
}

// restraintExpected reports whether the right answer at a case was to leave
// things as they are. Continuing to speak counts: carrying on mid-sentence is
// not an action, it is the absence of one.
func restraintExpected(item Case) bool {
	for _, allowed := range item.Accept {
		if allowed == Action(interaction.ActStaySilent) || allowed == Action(interaction.ActKeepSpeaking) {
			return true
		}
	}
	return false
}

func (report Report) Summary() Summary {
	var summary Summary
	for _, outcome := range report.Outcomes {
		summary.Total++
		switch {
		case outcome.Err != nil:
			summary.Errored++
		case outcome.Vetoed:
			summary.Vetoed++
		case outcome.Passed:
			summary.Passed++
		}
		if outcome.Elapsed > summary.Slowest {
			summary.Slowest = outcome.Elapsed
		}
		if restraintExpected(outcome.Case) {
			summary.RestraintTotal++
			if outcome.Passed {
				summary.RestraintPassed++
			}
		} else {
			summary.ActingTotal++
			if outcome.Passed {
				summary.ActingPassed++
			}
		}
	}
	return summary
}

// Format renders a report for a terminal, failures first.
//
// Failures first because a passing case is not what anybody reads a report
// for, and a format that buries the three broken ones under thirty green
// lines is a format that gets skimmed.
func (report Report) Format() string {
	var builder strings.Builder
	summary := report.Summary()
	fmt.Fprintf(&builder, "%s  %s\n\n", report.Decision, report.Runner)

	sorted := append([]Outcome(nil), report.Outcomes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return rank(sorted[i]) < rank(sorted[j])
	})
	for _, outcome := range sorted {
		mark := "ok  "
		switch {
		case outcome.Err != nil:
			mark = "err "
		case outcome.Vetoed:
			mark = "VETO"
		case !outcome.Passed:
			mark = "FAIL"
		}
		fmt.Fprintf(&builder, "  %s %-26s %5dms  %s\n", mark, outcome.Name,
			outcome.Elapsed.Milliseconds(), outcome.Because)
		if !outcome.Passed && strings.TrimSpace(outcome.Text) != "" {
			fmt.Fprintf(&builder, "       %q\n", truncate(outcome.Text, 92))
		}
	}
	fmt.Fprintf(&builder, "\n  passed %d/%d", summary.Passed, summary.Total)
	if summary.Vetoed > 0 {
		fmt.Fprintf(&builder, "   vetoed %d", summary.Vetoed)
	}
	if summary.Errored > 0 {
		fmt.Fprintf(&builder, "   errored %d", summary.Errored)
	}
	fmt.Fprintf(&builder, "   slowest %dms\n", summary.Slowest.Milliseconds())
	if summary.ActingTotal > 0 && summary.RestraintTotal > 0 {
		acting := float64(summary.ActingPassed) / float64(summary.ActingTotal)
		restraint := float64(summary.RestraintPassed) / float64(summary.RestraintTotal)
		// Balanced accuracy, because the plain total moves when the case mix
		// moves and this does not. It is the number to compare configurations
		// on: naming the do-nothing act differently slides a model along the
		// trade between these two without changing how well it tells the cases
		// apart, and only a measure that holds the mix fixed makes that visible.
		fmt.Fprintf(&builder, "  acting %d/%d   restraint %d/%d   balanced %.2f\n",
			summary.ActingPassed, summary.ActingTotal,
			summary.RestraintPassed, summary.RestraintTotal,
			(acting+restraint)/2)
	}
	return builder.String()
}

func rank(outcome Outcome) int {
	switch {
	case outcome.Err != nil:
		return 0
	case outcome.Vetoed:
		return 1
	case !outcome.Passed:
		return 2
	default:
		return 3
	}
}

func truncate(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
