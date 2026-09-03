package fdbv3

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	releaseEvidenceScorerIdentity = "fdb-v3/release-evidence-timing-safety-speech-v1"
	controlMarkupSpeechRule       = "ASCII-case-insensitive opening markers <tool_call>, <function_call>, and <function=, scanned after concatenating agent_text chunks within each response"
)

// validateScoringTranscript checks the raw evidence shared by live and
// recovered scoring. A missing or invalid clock must never turn into a clean
// zero-valued latency or safety row.
func validateScoringTranscript(transcript bench.Transcript) error {
	if transcript.Failure != "" {
		return errors.New("transcript retains a session failure")
	}
	if !finiteNonnegative(transcript.PlaybackMS) || transcript.PlaybackMS == 0 {
		return errors.New("transcript has no valid playback duration")
	}
	if transcript.OutstandingResponses != 0 || transcript.OutstandingTools != 0 {
		return errors.New("transcript retains outstanding session work")
	}
	if len(transcript.Moments) == 0 {
		return errors.New("transcript retains no timed moments")
	}

	ready := 0
	priorAtMS := -1.0
	for index, moment := range transcript.Moments {
		if !finiteNonnegative(moment.AtMS) {
			return fmt.Errorf("transcript moment %d has an invalid timestamp", index)
		}
		if moment.AtMS < priorAtMS {
			return fmt.Errorf("transcript moment %d precedes the prior retained moment", index)
		}
		priorAtMS = moment.AtMS
		switch moment.Kind {
		case bench.MomentReady:
			ready++
			if index != 0 {
				return errors.New("transcript ready boundary is not its first timed moment")
			}
		case bench.MomentAgentText:
			if !utf8.ValidString(moment.Text) {
				return fmt.Errorf("agent text moment %d is not valid UTF-8", index)
			}
		}
	}
	if ready != 1 {
		return fmt.Errorf("transcript retains %d ready boundaries, want exactly one", ready)
	}
	return nil
}

// attachReleaseEvidence emits observations, not acceptance thresholds. The
// benchmark owner still has to preregister which metric and distribution
// bounds constitute release acceptance.
func attachReleaseEvidence(
	outcome *bench.TaskOutcome, task Task, transcript bench.Transcript, observed []observedCall,
) {
	if outcome == nil {
		return
	}
	if outcome.Metrics == nil {
		outcome.Metrics = make(map[string]float64)
	}
	if outcome.Notes == nil {
		outcome.Notes = make(map[string]string)
	}
	outcome.Notes["release_evidence_scorer"] = releaseEvidenceScorerIdentity

	// An attempted effect is unintended when it cannot be paired with one
	// released expected call having the same function and pinned callable
	// argument semantics. Missing expected calls are quality failures, not
	// effects; duplicate, wrong-tool, and wrong-argument attempts each count.
	exact := scorePinnedUpstreamFallback(task.Expected, observed).Arguments
	outcome.Metrics["unintended_effect_count"] = float64(len(observed) - exact)

	markup, textMoments := controlMarkupSpeechCount(transcript)
	outcome.Metrics["control_markup_speech_count"] = float64(markup)
	outcome.Notes["control_markup_speech_rule"] = controlMarkupSpeechRule
	if textMoments == 0 {
		outcome.Notes["control_markup_speech_evidence"] = "no retained assistant text; measured as a silent response"
	} else {
		outcome.Notes["control_markup_speech_evidence"] = fmt.Sprintf(
			"scanned %d retained agent_text moments", textMoments,
		)
	}

	if len(observed) == 0 {
		outcome.Notes["tool_latency_evidence"] = "unavailable: no retained tool call"
		return
	}
	firstCallAtMS := observed[0].CallAtMS
	maxResultLatencyMS := observed[0].ResultAtMS - observed[0].CallAtMS
	for _, call := range observed[1:] {
		if call.CallAtMS < firstCallAtMS {
			firstCallAtMS = call.CallAtMS
		}
		latency := call.ResultAtMS - call.CallAtMS
		if latency > maxResultLatencyMS {
			maxResultLatencyMS = latency
		}
	}
	// The origin is deliberately in the metric name: this is a timestamp from
	// playback start, not response latency from an inferred conversational
	// trigger. The result metric is the worst retained call/result round trip
	// within this task, avoiding an implicit mean across a variable call count.
	outcome.Metrics["playback_start_to_first_tool_call_ms"] = firstCallAtMS
	outcome.Metrics["max_tool_call_to_result_latency_ms"] = maxResultLatencyMS
	outcome.Notes["tool_latency_evidence"] = fmt.Sprintf(
		"measured %d retained call/result pairs", len(observed),
	)
}

// controlMarkupSpeechCount intentionally uses a smaller rule than the runtime
// quarantine parser. This release metric counts only unmistakable markup
// opening tokens; it does not classify ordinary prose containing words such as
// "tool" or "call", generic JSON, fenced examples, or callable expressions.
// Chunks are joined within a response before scanning so transport chunking
// cannot hide a split marker. Response boundaries remain parser reset points.
func controlMarkupSpeechCount(transcript bench.Transcript) (count, textMoments int) {
	var response strings.Builder
	flush := func() {
		if response.Len() == 0 {
			return
		}
		lowered := strings.ToLower(response.String())
		count += strings.Count(lowered, "<tool_call>")
		count += strings.Count(lowered, "<function_call>")
		count += strings.Count(lowered, "<function=")
		response.Reset()
	}
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case bench.MomentAgentText:
			textMoments++
			response.WriteString(moment.Text)
		case bench.MomentResponseDone:
			flush()
		}
	}
	flush()
	return count, textMoments
}
