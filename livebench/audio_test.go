package livebench

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWAVRoundTripAndResample(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	input := Audio{SampleRateHz: 8_000, PCM16: make([]byte, 800*2)}
	for sample := range 800 {
		binary.LittleEndian.PutUint16(input.PCM16[sample*2:], uint16(int16(sample-400)))
	}
	filename := filepath.Join(dir, "audio.wav")
	if _, err := WriteWAV(filename, input); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadWAV(filename)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SampleRateHz != input.SampleRateHz || len(decoded.PCM16) != len(input.PCM16) {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
	resampled, err := Resample(decoded, 16_000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(resampled.PCM16), len(input.PCM16)*2; got != want {
		t.Fatalf("resampled bytes=%d, want %d", got, want)
	}
}

func TestAlignChunksQueuesPlayback(t *testing.T) {
	t.Parallel()
	chunks := []OutputChunk{
		{Arrival: 100 * time.Millisecond, PCM16: make([]byte, 200)},
		{Arrival: 105 * time.Millisecond, PCM16: make([]byte, 200)},
	}
	audio := AlignChunks(chunks, 1_000)
	if got, want := len(audio.PCM16), 600; got != want {
		t.Fatalf("aligned bytes=%d, want %d", got, want)
	}
}

func TestAlignChunksFlushesQueuedPlayback(t *testing.T) {
	t.Parallel()
	chunks := []OutputChunk{
		{Arrival: 100 * time.Millisecond, PCM16: make([]byte, 400)},
		{Arrival: 150 * time.Millisecond, Flush: true},
		{Arrival: 200 * time.Millisecond, PCM16: make([]byte, 100)},
	}
	audio := AlignChunks(chunks, 1_000)
	if got, want := len(audio.PCM16), 500; got != want {
		t.Fatalf("aligned bytes=%d, want %d", got, want)
	}
	for offset, value := range audio.PCM16[300:400] {
		if value != 0 {
			t.Fatalf("flushed silence byte %d is %d", offset, value)
		}
	}
}

func TestFitDurationPadsAndCrops(t *testing.T) {
	t.Parallel()
	audio := Audio{SampleRateHz: 1_000, PCM16: make([]byte, 200)}
	padded := FitDuration(audio, 200*time.Millisecond)
	if got, want := len(padded.PCM16), 400; got != want {
		t.Fatalf("padded bytes=%d, want %d", got, want)
	}
	cropped := FitDuration(padded, 50*time.Millisecond)
	if got, want := len(cropped.PCM16), 100; got != want {
		t.Fatalf("cropped bytes=%d, want %d", got, want)
	}
}

func TestDiscoverFDB15(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "v1_5", "user_backchannel", "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	audio := Audio{SampleRateHz: 16_000, PCM16: make([]byte, 320)}
	for _, name := range []string{"input.wav", "clean_input.wav"} {
		if _, err := WriteWAV(filepath.Join(dir, name), audio); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"timestamps":[1.25,2.5]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := DiscoverFDB15(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].ID != "7" || *samples[0].OverlapStartS != 1.25 {
		t.Fatalf("unexpected samples: %+v", samples)
	}
}
