package livebench

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestScoreTiming(t *testing.T) {
	t.Parallel()
	output := Audio{SampleRateHz: 1_000, PCM16: make([]byte, 3_000*2)}
	for sample := 500; sample < 1_500; sample++ {
		binary.LittleEndian.PutUint16(output.PCM16[sample*2:], uint16(int16(10_000)))
	}
	for sample := 2_100; sample < 2_500; sample++ {
		binary.LittleEndian.PutUint16(output.PCM16[sample*2:], uint16(int16(10_000)))
	}
	input := Audio{SampleRateHz: 1_000, PCM16: make([]byte, 3_000*2)}
	for sample := 800; sample < 1_200; sample++ {
		binary.LittleEndian.PutUint16(input.PCM16[sample*2:], uint16(int16(10_000)))
	}
	start, end := 1.0, 1.8
	metrics := ScoreTiming(input, output, &start, &end)
	if !metrics.SpeechDuringOverlap || metrics.MeanStopLatencyMS == nil || *metrics.MeanStopLatencyMS != 400 {
		t.Fatalf("unexpected stop metrics: %+v", metrics)
	}
	if metrics.MeanResponseLatencyMS == nil || math.Abs(*metrics.MeanResponseLatencyMS-900) > 0.001 {
		t.Fatalf("unexpected response metrics: %+v", metrics)
	}
}
