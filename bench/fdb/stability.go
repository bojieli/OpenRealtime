package fdb

import (
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

// Stability answers the question a single run cannot: is this recording's
// verdict a property of the system or of the afternoon.
//
// Two runs of the same forty recordings against the same executable disagreed
// on five of them and reported the same number of passes, so the total looked
// settled while a seventh of what it summed had moved. A recording that passes
// every time and one that passes half the time are different facts, and an
// aggregate cannot tell them apart. Only a repeated run can, which is why this
// reads the attempts rather than the summary.
type Stability struct {
	// Recordings is how many distinct recordings were attempted.
	Recordings int
	// Trials is the smallest number of attempts any recording received. One
	// means the run cannot say anything here, and Mixed will be zero because
	// nothing had the chance to disagree with itself.
	Trials int
	// AlwaysPassed, AlwaysFailed and AlwaysNotApplicable reached the same
	// verdict on every attempt.
	AlwaysPassed        int
	AlwaysFailed        int
	AlwaysNotApplicable int
	// Mixed reached more than one verdict. These are the recordings whose
	// single-run verdict means nothing, and their identities are kept because
	// the useful next step is to look at them rather than to count them.
	Mixed []string
}

// verdict is what one attempt concluded, kept deliberately coarse: the three
// states a reader of this suite acts on.
func verdict(task bench.TaskOutcome) string {
	switch {
	case !task.Completed:
		return "incomplete"
	case task.Applicability == bench.NotApplicable,
		task.Applicability == "" && task.Notes["applicable"] == "false":
		return "not-applicable"
	case task.Passed:
		return "passed"
	default:
		return "failed"
	}
}

// RecordingID strips the trial suffix a repeated run appends, so attempts of
// one recording group together. A single run has no suffix and is unchanged.
func RecordingID(taskID string) string {
	if index := strings.LastIndexByte(taskID, '#'); index > 0 {
		return taskID[:index]
	}
	return taskID
}

// Measure groups a result's attempts by recording and reports how consistent
// each one was.
func Measure(result bench.Result) Stability {
	verdicts := map[string]map[string]int{}
	order := []string{}
	for _, task := range result.Tasks {
		id := RecordingID(task.ID)
		if _, seen := verdicts[id]; !seen {
			verdicts[id] = map[string]int{}
			order = append(order, id)
		}
		verdicts[id][verdict(task)]++
	}
	stability := Stability{Recordings: len(order)}
	for index, id := range order {
		seen := verdicts[id]
		attempts := 0
		for _, count := range seen {
			attempts += count
		}
		if index == 0 || attempts < stability.Trials {
			stability.Trials = attempts
		}
		if len(seen) > 1 {
			stability.Mixed = append(stability.Mixed, id)
			continue
		}
		for only := range seen {
			switch only {
			case "passed":
				stability.AlwaysPassed++
			case "failed":
				stability.AlwaysFailed++
			case "not-applicable":
				stability.AlwaysNotApplicable++
			}
		}
	}
	sort.Strings(stability.Mixed)
	return stability
}
