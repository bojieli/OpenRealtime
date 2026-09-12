package scenario

import "github.com/bojieli/OpenRealtime/bench"

// Listener answers what the agent was audibly saying in a window of the
// episode clock. It is what a harness that produced the agent's audio itself
// supplies in place of a transcription of it.
type Listener = heard

// ScorePlayed scores a scenario a harness played without the wire.
//
// Play drives a live endpoint and transcribes the recorded loudspeaker; a
// harness that runs the graph in-process has the same transcript, the same
// captured playout, the menu it answered the agent's presses with, and its
// own knowledge of which words the agent's audio carried. This scores those
// with the same checks Play applies, so an in-process run and a live one
// are judged by one rule.
func ScorePlayed(
	item Scenario, timeline Timeline, transcript bench.Transcript, menu *Menu,
	capture bench.SessionAudioCapture, listen Listener,
) Result {
	if err := validateScenarioChecks(item); err != nil {
		return Result{ScorerVersion: ScorerVersion, Scenario: item.Name, Transcript: transcript,
			Failures: []string{"invalid scenario checks: " + err.Error()}}
	}
	if menu == nil && item.Menu != nil {
		menu = item.Menu()
	}
	result := score(item, timeline, transcript, menu, listen, &capture)
	result.Latencies = latencies(item, timeline, transcript)
	return result
}
