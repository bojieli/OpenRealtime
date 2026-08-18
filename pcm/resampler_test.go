package pcm

import (
	"encoding/binary"
	"slices"
	"testing"
)

func TestResamplerIsIndependentOfFramePartition(t *testing.T) {
	input := encodeSamples([]int16{-32768, -20000, -1000, 0, 1000, 20000, 32767, 1234, -4321})
	want := runResampler(t, 24_000, 16_000, [][]byte{input})
	got := runResampler(t, 24_000, 16_000, [][]byte{input[:2], input[2:8], input[8:14], input[14:]})
	if !slices.Equal(got, want) {
		t.Fatalf("partitioned output differs\n got %v\nwant %v", decodeSamples(got), decodeSamples(want))
	}
}

func TestResamplerSameRatePreservesInput(t *testing.T) {
	input := encodeSamples([]int16{-32768, -1, 0, 1, 32767})
	got := runResampler(t, 16_000, 16_000, [][]byte{input[:4], input[4:]})
	if !slices.Equal(got, input) {
		t.Fatalf("same-rate output %v, want %v", decodeSamples(got), decodeSamples(input))
	}
}

func TestResamplerUpsamplingUsesTerminalHold(t *testing.T) {
	input := encodeSamples([]int16{0, 1000})
	got := runResampler(t, 2, 4, [][]byte{input})
	want := []int16{0, 500, 1000, 1000}
	if !slices.Equal(decodeSamples(got), want) {
		t.Fatalf("output %v, want %v", decodeSamples(got), want)
	}
}

func TestResamplerRejectsInvalidLifecycle(t *testing.T) {
	if _, err := NewResampler(0, 16_000); err == nil {
		t.Fatal("expected invalid-rate error")
	}
	resampler, err := NewResampler(16_000, 24_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resampler.Push([]byte{1}); err == nil {
		t.Fatal("expected odd-byte error")
	}
	if _, err := resampler.Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := resampler.Push([]byte{0, 0}); err == nil {
		t.Fatal("expected finalized error")
	}
	if _, err := resampler.Finalize(); err == nil {
		t.Fatal("expected repeated-finalize error")
	}
}

func runResampler(t *testing.T, inputRate, outputRate uint32, fragments [][]byte) []byte {
	t.Helper()
	resampler, err := NewResampler(inputRate, outputRate)
	if err != nil {
		t.Fatal(err)
	}
	var output []byte
	for _, fragment := range fragments {
		part, err := resampler.Push(fragment)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, part...)
	}
	part, err := resampler.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	return append(output, part...)
}

func encodeSamples(samples []int16) []byte {
	encoded := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(encoded[index*2:], uint16(sample))
	}
	return encoded
}

func decodeSamples(encoded []byte) []int16 {
	samples := make([]int16, len(encoded)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(encoded[index*2:]))
	}
	return samples
}
