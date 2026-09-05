package scenario

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func acknowledgementScenario(t *testing.T) Scenario {
	t.Helper()
	for _, item := range Suite() {
		if item.Name == "an acknowledgement is not an interruption" {
			return item
		}
	}
	t.Fatal("acknowledgement scenario is missing")
	return Scenario{}
}

func heldTestCheck() Check {
	return Check{Kind: CheckHeldAcross, Line: 1, BeforeMS: 1000, AfterMS: 1000, MaxGapMS: 500}
}

func activityCapture(ranges ...[2]int) bench.SessionAudioCapture {
	capture := bench.SessionAudioCapture{SampleRateHz: 24_000}
	for _, interval := range ranges {
		samples := make([]int16, (interval[1]-interval[0])*24)
		for index := range samples {
			// An alternating signal has energy without a constant DC offset.
			if index%2 == 0 {
				samples[index] = 1200
			} else {
				samples[index] = -1200
			}
		}
		capture.Agent = append(capture.Agent, bench.TimedAudioChunk{AtMS: float64(interval[0]), PCM16: samples})
	}
	return capture
}

func TestAcknowledgementScoresRecordedPlayoutAcrossBothBackchannels(t *testing.T) {
	item, timeline := acknowledgementScenario(t), acknowledgementTimeline()
	// One prefetched packet arrives before either acknowledgement. Its
	// recorded playout spans both, so packet-arrival windows would be wrong.
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 4500, Kind: bench.MomentAgentAudio, PlayoutAtMS: 4500, AudioMS: 10000, ResponseID: "explanation"},
		{AtMS: 4500, Kind: bench.MomentAgentText, Text: "Find your order number and purchase email, use the return label, and receive the refund on the original payment method."},
		{AtMS: 4600, Kind: bench.MomentResponseDone, ResponseID: "explanation", ResponseStatus: "completed"},
	}}
	capture := activityCapture([2]int{4500, 14500})
	result := ScoreWithAudio(item, timeline, transcript, capture)
	if !result.Passed || result.ScorerVersion != ScorerVersion || len(result.Holds) != 2 {
		t.Fatalf("recorded continuation failed: %+v", result)
	}
	for _, measurement := range result.Holds {
		if measurement.BeforeActiveMS != 1000 || measurement.AfterActiveMS != 1000 || measurement.LongestGapMS != 0 {
			t.Fatalf("wrong playout evidence: %+v", measurement)
		}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var retained Result
	if err := json.Unmarshal(payload, &retained); err != nil || len(retained.Holds) != 2 || retained.Holds[1].Line != 2 {
		t.Fatalf("per-acknowledgement evidence was not retained: %s, %v", payload, err)
	}
	// Keep the first acknowledgement correct, but stop at the second one.
	capture = activityCapture([2]int{4500, 11500})
	result = ScoreWithAudio(item, timeline, transcript, capture)
	if result.Passed || len(result.Failures) != 1 || !strings.Contains(result.Failures[0], "line 2") {
		t.Fatalf("second acknowledgement was not independently checked: %+v", result)
	}
}

func TestHoldingDistinguishesSilenceOverlapStopAndLateRestart(t *testing.T) {
	for _, test := range []struct {
		name    string
		capture bench.SessionAudioCapture
		want    string
	}{
		{"never started", activityCapture([2]int{9200, 10400}), "not speaking before"},
		{"spoke only before", activityCapture([2]int{8000, 9000}), "did not speak through"},
		{"stopped at end", activityCapture([2]int{8000, 9600}), "insufficient continuation"},
		{"yield then restart", activityCapture([2]int{8000, 9000}, [2]int{9800, 10600}), "did not speak through"},
		{"long pause with some overlap", activityCapture([2]int{8000, 9240}, [2]int{9840, 10600}), "paused 600ms"},
		{"continuous", activityCapture([2]int{8000, 10600}), ""},
		{"natural pause", activityCapture([2]int{8000, 9200}, [2]int{9500, 10600}), ""},
		{"exact gap limit", activityCapture([2]int{8000, 9240}, [2]int{9740, 10600}), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			measurement, failure := heldAcross(heldTestCheck(), acknowledgementTimeline(), &test.capture)
			if test.want == "" && failure != "" || test.want != "" && !strings.Contains(failure, test.want) {
				t.Fatalf("failure=%q, want %q; measurements=%+v", failure, test.want, measurement)
			}
		})
	}
}

func TestHoldingRetainsResponseOutcomesWithoutInferringCancellation(t *testing.T) {
	item := Scenario{Checks: []Check{heldTestCheck()}}
	for _, status := range []string{"completed", "cancelled", "incomplete", "failed", "", "not-a-status"} {
		transcript := bench.Transcript{Moments: []bench.Moment{
			// Prefetch delivery occurs well before the recorded overlap window.
			{Kind: bench.MomentAgentAudio, AtMS: 1000, PlayoutAtMS: 8000, AudioMS: 1600, ResponseID: "first"},
			{Kind: bench.MomentResponseDone, AtMS: 2000, ResponseID: "first", ResponseStatus: status, ResponseStatusReason: "turn_detected"},
			// A later, unrelated response must not supply the first one's status.
			{Kind: bench.MomentAgentAudio, AtMS: 11000, PlayoutAtMS: 11000, AudioMS: 1000, ResponseID: "unrelated"},
			{Kind: bench.MomentResponseDone, AtMS: 12000, ResponseID: "unrelated", ResponseStatus: "cancelled"},
		}}
		result := ScoreWithAudio(item, acknowledgementTimeline(), transcript, activityCapture([2]int{8000, 9600}))
		if result.Passed || len(result.Holds[0].Responses) != 1 ||
			!strings.Contains(result.Failures[0], "insufficient continuation") {
			t.Fatalf("status %q changed waveform score: %+v", status, result)
		}
		response := result.Holds[0].Responses[0]
		want := status
		if status == "" || status == "not-a-status" {
			want = "unrecognized"
		}
		if result.Holds[0].ResponseEvidence != "recorded" || response.Status != want || response.ResponseID != "first" ||
			response.AudioFromMS != 8000 || response.AudioToMS != 9600 {
			t.Fatalf("wrong playout/termination join: %+v", result.Holds[0])
		}
		if want != "unrecognized" && (response.TerminalAtMS != 2000 || response.Reason != "turn_detected") {
			t.Fatalf("terminal evidence changed: %+v", response)
		}
		if want == "unrecognized" && (response.TerminalAtMS != 0 || response.Reason != "") {
			t.Fatalf("unknown status acquired evidence: %+v", response)
		}
	}
}

func TestHoldingCannotBorrowOrChooseConflictingTerminalEvidence(t *testing.T) {
	base := bench.Transcript{Moments: []bench.Moment{
		{Kind: bench.MomentAgentAudio, AtMS: 1000, PlayoutAtMS: 8000, AudioMS: 2600, ResponseID: "first"},
	}}
	for _, test := range []struct {
		name string
		ends []bench.Moment
		want string
	}{
		{"no end", nil, "unobserved"},
		{"wrong response", []bench.Moment{{Kind: bench.MomentResponseDone, ResponseID: "other", ResponseStatus: "completed"}}, "unobserved"},
		{"duplicate completion", []bench.Moment{
			{Kind: bench.MomentResponseDone, ResponseID: "first", ResponseStatus: "completed"},
			{Kind: bench.MomentResponseDone, ResponseID: "first", ResponseStatus: "completed"},
		}, "ambiguous"},
		{"conflicting completion", []bench.Moment{
			{Kind: bench.MomentResponseDone, ResponseID: "first", ResponseStatus: "completed"},
			{Kind: bench.MomentResponseDone, ResponseID: "first", ResponseStatus: "cancelled"},
		}, "ambiguous"},
		{"invalid time", []bench.Moment{{Kind: bench.MomentResponseDone, AtMS: math.NaN(), ResponseID: "first", ResponseStatus: "completed"}}, "unrecognized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := bench.Transcript{Moments: append(append([]bench.Moment{}, base.Moments...), test.ends...)}
			result := ScoreWithAudio(Scenario{Checks: []Check{heldTestCheck()}}, acknowledgementTimeline(), transcript,
				activityCapture([2]int{8000, 10600}))
			if result.Passed || result.Holds[0].Responses[0].Status != test.want || !strings.Contains(result.Failures[0], "NOT VERIFIED") {
				t.Fatalf("missing or conflicting terminal evidence earned hold credit: %+v", result)
			}
		})
	}
	legacy := ScoreWithAudio(Scenario{Checks: []Check{heldTestCheck()}}, acknowledgementTimeline(), bench.Transcript{}, activityCapture([2]int{8000, 10600}))
	if legacy.Passed || legacy.Holds[0].ResponseEvidence != "unavailable" || len(legacy.Holds[0].Responses) != 0 {
		t.Fatalf("old transcript received terminal evidence: %+v", legacy)
	}
}

func TestHoldingCannotBorrowTextRoomAudioOrSilentPackets(t *testing.T) {
	for _, amplitude := range []int16{0, 1, 127} {
		capture := activityCapture([2]int{8000, 10600})
		capture.RoomPCM16 = append([]int16(nil), capture.Agent[0].PCM16...)
		for index := range capture.Agent[0].PCM16 {
			capture.Agent[0].PCM16[index] = amplitude
		}
		transcript := bench.Transcript{Moments: []bench.Moment{{Kind: bench.MomentAgentText, AtMS: 9200, Text: "I am continuing the explanation."},
			{Kind: bench.MomentAgentAudio, AtMS: 9200, AudioMS: 2600}}}
		result := ScoreWithAudio(Scenario{Checks: []Check{heldTestCheck()}}, acknowledgementTimeline(), transcript, capture)
		if result.Passed || result.Holds[0].BeforeActiveMS != 0 || result.Holds[0].DuringActiveMS != 0 || result.Holds[0].AfterActiveMS != 0 {
			t.Fatalf("non-speech amplitude %d earned continuation credit: %+v", amplitude, result)
		}
	}
}

func TestHoldingRefusesMissingOrInvalidCapture(t *testing.T) {
	if _, failure := heldAcross(heldTestCheck(), acknowledgementTimeline(), nil); !strings.Contains(failure, "NOT VERIFIED") {
		t.Fatalf("missing capture passed: %q", failure)
	}
	for _, mutate := range []func(*bench.SessionAudioCapture){
		func(c *bench.SessionAudioCapture) { c.SampleRateHz = 0 },
		func(c *bench.SessionAudioCapture) { c.Agent[0].AtMS = -1 },
		func(c *bench.SessionAudioCapture) { c.Agent[0].AtMS = math.NaN() },
		func(c *bench.SessionAudioCapture) { c.Agent[0].AtMS = math.Inf(1) },
		func(c *bench.SessionAudioCapture) { c.Agent = append(c.Agent, c.Agent[0]) },
	} {
		capture := activityCapture([2]int{8000, 10600})
		mutate(&capture)
		if _, failure := heldAcross(heldTestCheck(), acknowledgementTimeline(), &capture); !strings.Contains(failure, "NOT VERIFIED") {
			t.Fatalf("invalid capture passed: %q", failure)
		}
	}
}

func TestHoldingRequiresExplicitValidWindows(t *testing.T) {
	for _, mutate := range []func(*Check){
		func(c *Check) { c.Line = -1 },
		func(c *Check) { c.Sight = 1 },
		func(c *Check) { c.BeforeMS = 0 },
		func(c *Check) { c.AfterMS = -1 },
		func(c *Check) { c.MaxGapMS = 0 },
		func(c *Check) { c.BeforeMS = 10_001 },
		func(c *Check) { c.FromMS = 100 },
	} {
		check := heldTestCheck()
		mutate(&check)
		if err := validateScenarioChecks(Scenario{Script: []Line{{}, {}}, Checks: []Check{check}}); err == nil {
			t.Fatalf("bad check reached synthesis: %+v", check)
		}
	}
	for _, timeline := range []Timeline{
		{Spans: []Span{{}, {StartMS: 500, EndMS: 1000}}, TotalMS: 2000},
		{Spans: []Span{{}, {StartMS: 9000, EndMS: 9600}}, TotalMS: 10000},
		{Spans: []Span{{}, {StartMS: 9000, EndMS: 140000}}, TotalMS: 150000},
	} {
		if _, failure := heldAcross(heldTestCheck(), timeline, nil); !strings.Contains(failure, "invalid scenario check") {
			t.Fatalf("invalid timeline passed: %+v, %q", timeline, failure)
		}
	}
}

func acknowledgementTimeline() Timeline {
	return Timeline{Spans: []Span{{StartMS: 0, EndMS: 4000}, {StartMS: 9000, EndMS: 9600}, {StartMS: 11500, EndMS: 12300}}, TotalMS: 18300}
}

func TestAcknowledgementCannotPassBySpeakingOnlyBeforeTheBackchannels(t *testing.T) {
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 4500, Kind: bench.MomentAgentAudio, AudioMS: 2000},
		{AtMS: 4500, Kind: bench.MomentAgentText, Text: "The refund process begins with the order details."},
		{AtMS: 6500, Kind: bench.MomentResponseDone},
	}}
	if result := Score(acknowledgementScenario(t), acknowledgementTimeline(), transcript); result.Passed {
		t.Fatalf("the agent stopped before both acknowledgements but passed: %+v", result)
	}
}

func TestAcknowledgementRejectsUngroundedNonAnswerDespiteContinuousAudio(t *testing.T) {
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 4500, Kind: bench.MomentAgentAudio, AudioMS: 10000},
		{AtMS: 4500, Kind: bench.MomentAgentText, Text: "I do not have any information about the refund process in my current context."},
	}}
	if result := ScoreWithAudio(acknowledgementScenario(t), acknowledgementTimeline(), transcript,
		activityCapture([2]int{4500, 14500})); result.Passed {
		t.Fatalf("sustained non-answer passed refund explanation: %+v", result)
	}
}

func TestAcknowledgementRequiresPurchaseConfirmationDespiteOtherEmailAndContinuousSpeech(t *testing.T) {
	// The retained live omission still mentions support emailing a return
	// label. Neither that different email nor passing acoustic holds supplies
	// the missing purchase confirmation.
	const omitted = "First, you must find the order number and purchase Check that the request is within thirty days of delivery. " +
		"Support will email a prepaid return label. The refund goes to the original payment method."
	for _, detail := range []string{"", "purchase email", "confirmation email", "email confirmation", "order confirmation"} {
		t.Run(detail, func(t *testing.T) {
			text := omitted
			if detail != "" {
				text = strings.Replace(text, "purchase Check", detail+". Check", 1)
			}
			transcript := bench.Transcript{Moments: []bench.Moment{
				{AtMS: 4500, Kind: bench.MomentAgentAudio, PlayoutAtMS: 4500, AudioMS: 10000, ResponseID: "explanation"},
				{AtMS: 4500, Kind: bench.MomentAgentText, Text: text},
				{AtMS: 14500, Kind: bench.MomentResponseDone, ResponseID: "explanation", ResponseStatus: "completed"},
			}}
			result := ScoreWithAudio(acknowledgementScenario(t), acknowledgementTimeline(), transcript,
				activityCapture([2]int{4500, 14500}))
			if result.Passed != (detail != "") || len(result.Holds) != 2 {
				t.Fatalf("purchase confirmation %q: %+v", detail, result)
			}
			if detail == "" && (len(result.Failures) != 1 || !strings.Contains(result.Failures[0], "purchase confirmation")) {
				t.Fatalf("omission was not independently diagnosed: %+v", result)
			}
		})
	}
}

// A fresh response can fill every acoustic window after the original response
// is cancelled. Continuation credit must still reject that interrupted turn.
func TestHoldingRejectsAbortDespiteContinuousReplacementAudio(t *testing.T) {
	for _, status := range []string{"completed", "cancelled", "failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			transcript := bench.Transcript{Moments: []bench.Moment{
				{Kind: bench.MomentAgentAudio, AtMS: 1000, PlayoutAtMS: 8000, AudioMS: 1400, ResponseID: "original"},
				{Kind: bench.MomentResponseDone, AtMS: 9250, ResponseID: "original", ResponseStatus: status, ResponseStatusReason: "turn_detected"},
				{Kind: bench.MomentAgentAudio, AtMS: 9400, PlayoutAtMS: 9400, AudioMS: 1200, ResponseID: "replacement"},
				{Kind: bench.MomentResponseDone, AtMS: 9500, ResponseID: "replacement", ResponseStatus: "completed"},
			}}
			result := ScoreWithAudio(Scenario{Checks: []Check{heldTestCheck()}}, acknowledgementTimeline(), transcript,
				activityCapture([2]int{8000, 10600}))
			if result.Passed != (status == "completed") || len(result.Holds) != 1 || len(result.Holds[0].Responses) != 2 {
				t.Fatalf("%s followed by replacement audio received wrong hold outcome: %+v", status, result)
			}
			if status != "completed" && !strings.Contains(strings.Join(result.Failures, " "), status) {
				t.Fatalf("failure did not identify the observed terminal status: %+v", result)
			}
		})
	}
}

func TestHoldingUsesBoundedTerminalTimeAndExactAudioAttribution(t *testing.T) {
	base := []bench.Moment{
		{Kind: bench.MomentAgentAudio, AtMS: 1000, PlayoutAtMS: 8000, AudioMS: 2600, ResponseID: "speech"},
		{Kind: bench.MomentResponseDone, AtMS: 2000, ResponseID: "speech", ResponseStatus: "completed"},
	}
	for _, test := range []struct {
		name    string
		mutate  func([]bench.Moment) []bench.Moment
		want    string
		missing float64
	}{
		{name: "prefetched completed", mutate: func(m []bench.Moment) []bench.Moment { return m }},
		{name: "prefetched cancelled", mutate: func(m []bench.Moment) []bench.Moment { m[1].ResponseStatus = "cancelled"; return m }, want: "cancelled"},
		{name: "cancelled at window end", mutate: func(m []bench.Moment) []bench.Moment { m[1].ResponseStatus = "cancelled"; m[1].AtMS = 10600; return m }, want: "cancelled"},
		{name: "cancelled after window", mutate: func(m []bench.Moment) []bench.Moment { m[1].ResponseStatus = "cancelled"; m[1].AtMS = 10601; return m }},
		{name: "failed after window", mutate: func(m []bench.Moment) []bench.Moment { m[1].ResponseStatus = "failed"; m[1].AtMS = 10601; return m }},
		{name: "unrelated earlier cancel", mutate: func(m []bench.Moment) []bench.Moment {
			return append(m,
				bench.Moment{Kind: bench.MomentAgentAudio, PlayoutAtMS: 7000, AudioMS: 1000, ResponseID: "earlier"},
				bench.Moment{Kind: bench.MomentResponseDone, AtMS: 7900, ResponseID: "earlier", ResponseStatus: "cancelled"})
		}},
		{name: "unidentified later audio", mutate: func(m []bench.Moment) []bench.Moment {
			return append(m,
				bench.Moment{Kind: bench.MomentAgentAudio, PlayoutAtMS: 10600, AudioMS: 1000})
		}},
		{name: "unidentified overlap audio", mutate: func(m []bench.Moment) []bench.Moment {
			return append(m,
				bench.Moment{Kind: bench.MomentAgentAudio, PlayoutAtMS: 9000, AudioMS: 100})
		}, want: "evidence is partial"},
		{name: "missing identity", mutate: func(m []bench.Moment) []bench.Moment { m[0].ResponseID = ""; return m }, want: "evidence is unavailable"},
		{name: "missing playout", mutate: func(m []bench.Moment) []bench.Moment { m[0].PlayoutAtMS = 0; return m }, want: "evidence is unavailable"},
		{name: "missing audio tail", mutate: func(m []bench.Moment) []bench.Moment { m[0].AudioMS = 1600; return m }, want: "not attributed", missing: 1000},
		{name: "one missing sample", mutate: func(m []bench.Moment) []bench.Moment { m[0].AudioMS -= 1.0 / 24; return m }, want: "not attributed", missing: 1.0 / 24},
		{name: "gap between same response packets", mutate: func(m []bench.Moment) []bench.Moment {
			m[0].AudioMS = 1000
			return append(m,
				bench.Moment{Kind: bench.MomentAgentAudio, PlayoutAtMS: 9600, AudioMS: 1000, ResponseID: "speech"})
		}, want: "not attributed", missing: 600},
		{name: "invalid packet time", mutate: func(m []bench.Moment) []bench.Moment { m[0].PlayoutAtMS = math.Inf(1); return m }, want: "evidence is unavailable"},
		{name: "unknown terminal", mutate: func(m []bench.Moment) []bench.Moment { m[1].ResponseStatus = "unknown"; return m }, want: "unrecognized terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := bench.Transcript{Moments: test.mutate(append([]bench.Moment(nil), base...))}
			result := ScoreWithAudio(Scenario{Checks: []Check{heldTestCheck()}}, acknowledgementTimeline(), transcript,
				activityCapture([2]int{8000, 10600}))
			if result.Passed != (test.want == "") || test.want != "" && !strings.Contains(strings.Join(result.Failures, " "), test.want) ||
				result.Holds[0].UnattributedActiveMS != test.missing {
				t.Fatalf("incorrect bounded/attributed response outcome, want %q and %.3fms missing: %+v", test.want, test.missing, result)
			}
		})
	}
}

func TestHoldingDoesNotRequireAttributionForSilentWAVPadding(t *testing.T) {
	capture := activityCapture([2]int{8000, 9200}, [2]int{9400, 10600})
	wav, _, err := encodeReviewStereoWAV(capture)
	if err != nil {
		t.Fatal(err)
	}
	_, samples := decodeStereoReviewWAV(t, wav)
	transcript := bench.Transcript{Moments: []bench.Moment{
		{Kind: bench.MomentAgentAudio, AtMS: 1000, PlayoutAtMS: 8000, AudioMS: 1200, ResponseID: "speech"},
		{Kind: bench.MomentAgentAudio, AtMS: 2000, PlayoutAtMS: 9400, AudioMS: 1200, ResponseID: "speech"},
		{Kind: bench.MomentResponseDone, AtMS: 2001, ResponseID: "speech", ResponseStatus: "completed"},
	}}
	result := ScoreWithAudio(Scenario{Checks: []Check{heldTestCheck()}}, acknowledgementTimeline(), transcript,
		bench.SessionAudioCapture{SampleRateHz: 24000, Agent: []bench.TimedAudioChunk{{PCM16: samples}}})
	if !result.Passed || result.Holds[0].LongestGapMS != 200 || result.Holds[0].UnattributedActiveMS != 0 {
		t.Fatalf("WAV padding required response attribution: %+v", result)
	}
}
