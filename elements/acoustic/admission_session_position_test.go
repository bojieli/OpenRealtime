package acoustic

import "testing"

// The wire defines audio_start_ms as milliseconds "from the start of all audio
// written to the buffer during the session", and a client uses it to find the
// speech inside audio it still holds - to trim a recording, or to cut playback
// at the point the person began talking. This element keeps one gate per
// stream and each gate counts from its own first sample, so the position has
// to be carried forward or every utterance after the first is reported at the
// position of the one before it. Probed against a live endpoint with two
// bursts written at known offsets, the second came back three seconds early,
// which is exactly how long the first utterance lasted.
func TestSpeechPositionsCountFromTheStartOfTheSessionNotOfTheStream(t *testing.T) {
	harness := mountAcoustic(t, EndpointPolicyConfig{Mode: EndpointAutomatic})

	harness.send("audio", "first-loud-1", input("first", audioFrame("microphone", 1, 2_000)))
	harness.send("audio", "first-loud-2", input("first", audioFrame("microphone", 2, 2_000)))
	started := nextActivity(t, harness, SpeechStarted)
	_ = receive(t, harness.output("admitted"))
	if started.AudioStartMS != 0 {
		t.Fatalf("first utterance start = %d ms, want the beginning of the session", started.AudioStartMS)
	}
	harness.send("audio", "first-silence-1", input("first", audioFrame("microphone", 3, 0)))
	harness.send("audio", "first-silence-2", input("first", audioFrame("microphone", 4, 0)))
	candidate := payload[EndpointCandidate](t, receive(t, harness.output("candidate")))
	_ = payload[GateCommand](t, receive(t, harness.output("command")))
	stopped := nextActivity(t, harness, SpeechStopped)
	_ = receive(t, harness.output("flush"))

	// Four frames of twenty milliseconds each have now been written.
	const firstStreamMS = 4 * audioFrameMS
	if candidate.AudioEndMS != firstStreamMS || stopped.AudioEndMS != firstStreamMS {
		t.Fatalf("first utterance end: candidate %d ms, activity %d ms, want %d",
			candidate.AudioEndMS, stopped.AudioEndMS, firstStreamMS)
	}

	// A second stream. Its gate starts over; the session does not.
	harness.send("audio", "second-loud-1", input("second", audioFrame("microphone", 5, 2_000)))
	harness.send("audio", "second-loud-2", input("second", audioFrame("microphone", 6, 2_000)))
	secondStart := nextActivity(t, harness, SpeechStarted)
	_ = receive(t, harness.output("admitted"))
	if secondStart.AudioStartMS != firstStreamMS {
		t.Fatalf("second utterance start = %d ms, want %d; a client trimming its own audio "+
			"at this offset would cut %d ms too early",
			secondStart.AudioStartMS, firstStreamMS, firstStreamMS-secondStart.AudioStartMS)
	}

	harness.send("audio", "second-silence-1", input("second", audioFrame("microphone", 7, 0)))
	harness.send("audio", "second-silence-2", input("second", audioFrame("microphone", 8, 0)))
	secondCandidate := payload[EndpointCandidate](t, receive(t, harness.output("candidate")))
	if want := 2 * firstStreamMS; secondCandidate.AudioEndMS != want {
		t.Fatalf("second utterance end = %d ms, want %d", secondCandidate.AudioEndMS, want)
	}
}

func nextActivity(t *testing.T, harness *acousticHarness, kind SpeechActivityKind) SpeechActivity {
	t.Helper()
	for {
		activity := payload[SpeechActivity](t, receive(t, harness.output("activity")))
		if activity.Kind == kind {
			return activity
		}
	}
}
