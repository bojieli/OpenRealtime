package fdbench

import (
	"fmt"
	"math"

	"github.com/bojieli/OpenRealtime/bench"
)

// Where a turn's speech actually stops.
//
// This suite counts agent audio inside an annotated turn as speaking over the
// person, and measures response latency from the annotated end. Both treat the
// annotation as the moment the speaker stopped. It is not: the released turn
// boundaries enclose whatever silence the synthesiser left at the end of the
// clip, and how much that is depends on which system generated the speech.
// Sampling six conversations in each of the twenty-one conditions on
// 2026-09-05, the audio inside a turn goes quiet before its annotated end by a
// p90 of 20 to 200 ms across the seven F5-TTS conditions, 420 to 640 ms across
// the seven CosyVoice 2 ones, and 1,180 to 1,420 ms across the three ChatTTS
// ones, with a worst case of 1,620 ms.
//
// An agent that endpoints on real silence and answers quickly therefore
// produces audio inside the annotated turn and is charged for it - an order of
// magnitude more often on some conditions than on others, underneath the
// comparisons across conditions that this suite exists to invite.
//
// The scoring is deliberately unchanged. What is added is the number that
// would justify changing it: how many of the turns counted as spoken over had
// the agent starting after the speaker had already stopped. A run now carries
// its own evidence instead of needing one written for it.

// audibleFloor is the RMS, in the units of a 16-bit sample, at which a block of
// audio counts as sound rather than silence. It is fixed rather than read from
// a deployment because when a recording stops making noise is a property of
// the recording.
const audibleFloor = 640.0

// audibleBlockMS is the window the floor is applied over: one packet at the
// rate this engine works in.
const audibleBlockMS = 20.0

// audibleEndMS reports where the last sound inside a span is.
//
// It returns the span's own end when nothing in it falls below the floor,
// which is what happens in the 0 dB background conditions where the noise
// never stops. That is the safe direction: the measurement degrades to the
// annotation exactly where it cannot do better than the annotation.
func audibleEndMS(samples []int16, rate int, startMS, endMS float64) float64 {
	if rate <= 0 || endMS <= startMS {
		return endMS
	}
	block := int(audibleBlockMS * float64(rate) / 1000.0)
	if block <= 0 {
		return endMS
	}
	first := int(startMS * float64(rate) / 1000.0)
	last := int(endMS * float64(rate) / 1000.0)
	if first < 0 {
		first = 0
	}
	if last > len(samples) {
		last = len(samples)
	}
	end := startMS
	found := false
	for offset := first; offset+block <= last; offset += block {
		var total float64
		for _, sample := range samples[offset : offset+block] {
			value := float64(sample)
			total += value * value
		}
		if math.Sqrt(total/float64(block)) >= audibleFloor {
			end = float64(offset+block) * 1000.0 / float64(rate)
			found = true
		}
	}
	if !found {
		return endMS
	}
	return end
}

// measureAudibleEnds fills in each turn's real end from the recording.
func measureAudibleEnds(audioPath string, turns []Turn) error {
	samples, err := bench.LoadPCM24k(audioPath)
	if err != nil {
		return fmt.Errorf("read %s to locate where its turns stop: %w", audioPath, err)
	}
	for index := range turns {
		turns[index].AudibleEndMS = audibleEndMS(
			samples, 24_000, turns[index].StartMS, turns[index].EndMS,
		)
	}
	return nil
}
