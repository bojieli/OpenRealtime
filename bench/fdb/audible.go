package fdb

import (
	"fmt"
	"math"

	"github.com/bojieli/OpenRealtime/bench"
)

// Where the overlapping speech actually starts.
//
// Each recording carries a pair of timestamps saying when its event happens,
// and the suite has always treated the first of them as the moment the person
// began talking over the agent. It is not. It is where the event clip was
// placed in the mix, and the clip begins with however much silence the speaker
// left before opening their mouth.
//
// Measured across the whole of FDB v1.5 on 2026-09-05, that silence is
// negligible in three categories - a median of 20 ms for backchannels, 50 ms
// for talking to somebody else, 120 ms for background speech - and large in
// the fourth. Sixty-six of the two hundred user_interruption recordings begin
// with more than 400 ms of silence and one begins with 1,340 ms. That is the
// one category scored against a one-second deadline, so a third of it was
// charging the agent for time in which there was nothing to react to.
//
// On thirty of those recordings run twice each, the difference is the whole
// result: measured from the annotation, 32 of 50 attempts yielded inside the
// window and the slowest took 1,709 ms; measured from the first sound, all 50
// did and the slowest took 878 ms.

// audibleFloor is the RMS, in the units of a 16-bit sample, at which this
// suite calls a block of audio sound rather than silence.
//
// It is fixed here rather than read from the deployment on purpose: the answer
// to "when does this recording start making noise" is a property of the
// recording, and a number that moved with a server's gate configuration could
// not be compared between two runs. The conclusion does not rest on the exact
// value. Sweeping it across twenty-four decibels, from 160 to 2,560, moved the
// median lead only from 240 ms to 360 ms and changed the verdict on two of
// fifty attempts.
const audibleFloor = 640.0

// audibleBlockMS is the window the floor is applied over. Twenty milliseconds
// is one packet at the rate this engine works in, so a block is the smallest
// quantity of sound the pipeline could deliver anyway.
const audibleBlockMS = 20.0

// audibleSearchMS bounds the search. An event whose speech does not begin
// within four seconds of where it was placed is not a late start; it is a
// recording this suite cannot ask its question of, and the caller is told so.
const audibleSearchMS = 4000.0

// audibleAfterMS reports how long after fromMS the recording first carries
// sound, and whether it does so at all within the search bound.
func audibleAfterMS(samples []int16, rate int, fromMS float64) (float64, bool) {
	if rate <= 0 || len(samples) == 0 || fromMS < 0 {
		return 0, false
	}
	blockSamples := int(audibleBlockMS * float64(rate) / 1000.0)
	if blockSamples <= 0 {
		return 0, false
	}
	begin := int(fromMS * float64(rate) / 1000.0)
	if begin >= len(samples) {
		return 0, false
	}
	limit := begin + int(audibleSearchMS*float64(rate)/1000.0)
	if limit > len(samples) {
		limit = len(samples)
	}
	for offset := begin; offset+blockSamples <= limit; offset += blockSamples {
		var total float64
		for _, sample := range samples[offset : offset+blockSamples] {
			value := float64(sample)
			total += value * value
		}
		if math.Sqrt(total/float64(blockSamples)) >= audibleFloor {
			return float64(offset-begin) * 1000.0 / float64(rate), true
		}
	}
	return 0, false
}

// eventAudibleAfterMS loads a recording and reports where its event's speech
// begins, relative to the annotation.
func eventAudibleAfterMS(audioPath string, eventStartMS float64) (float64, error) {
	samples, err := bench.LoadPCM24k(audioPath)
	if err != nil {
		return 0, fmt.Errorf("read %s to locate the event's first sound: %w", audioPath, err)
	}
	lead, found := audibleAfterMS(samples, 24_000, eventStartMS)
	if !found {
		return 0, fmt.Errorf(
			"%s carries no sound in the %.0f s after its event begins at %.0f ms",
			audioPath, audibleSearchMS/1000.0, eventStartMS,
		)
	}
	return lead, nil
}
