package fdbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/livebench"
)

type fakeAdapter struct {
	descriptor livebench.Descriptor
}

func (adapter fakeAdapter) Descriptor() livebench.Descriptor { return adapter.descriptor }

func (adapter fakeAdapter) Run(_ context.Context, input livebench.Audio) (livebench.SessionResult, error) {
	first := 120.0
	return livebench.SessionResult{
		Descriptor: adapter.descriptor, InputDurationMS: input.Duration().Seconds() * 1000,
		ElapsedMS: 1_250, FirstAudioMS: &first,
		Chunks: []livebench.OutputChunk{{Arrival: 120 * time.Millisecond, PCM16: make([]byte, 4_800)}},
	}, nil
}

func TestDiscoverAndRunSample(t *testing.T) {
	root := t.TempDir()
	cell := filepath.Join(root, "chattts-single-round-combine-easy")
	if err := os.MkdirAll(cell, 0o755); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(cell, "conversation_2.wav")
	if _, err := livebench.WriteWAV(inputPath, livebench.Audio{PCM16: make([]byte, 48_000), SampleRateHz: 24_000}); err != nil {
		t.Fatal(err)
	}
	timestampPath := filepath.Join(cell, "conversation_2.timestamps")
	if err := os.WriteFile(timestampPath, []byte(`[{"start":160,"end":12000}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Conversation != 2 || samples[0].InputDurationMS != 1_000 {
		t.Fatalf("unexpected samples: %+v", samples)
	}
	descriptor := livebench.Descriptor{Provider: "openrealtime", Model: "local", OutputSampleRate: 24_000, InputSampleRate: 24_000}
	outputRoot := filepath.Join(root, "output")
	result, err := RunSample(context.Background(), fakeAdapter{descriptor: descriptor}, samples[0], outputRoot, "openrealtime")
	if err != nil {
		t.Fatal(err)
	}
	output, err := livebench.ReadWAV(result.OutputWAV)
	if err != nil {
		t.Fatal(err)
	}
	if output.Duration() != 1_250*time.Millisecond || result.OutputVAD.Status != "pending" {
		t.Fatalf("unexpected result/output: duration=%s result=%+v", output.Duration(), result)
	}
	if _, found, err := LoadCompleted(samples[0], outputRoot, "openrealtime", "local"); err != nil || !found {
		t.Fatalf("LoadCompleted() = found %v, err %v", found, err)
	}
}

func TestDiscoverRejectsOutOfRangeTimestamp(t *testing.T) {
	root := t.TempDir()
	cell := filepath.Join(root, "condition")
	if err := os.MkdirAll(cell, 0o755); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(cell, "conversation_1.wav")
	if _, err := livebench.WriteWAV(inputPath, livebench.Audio{PCM16: make([]byte, 3_200), SampleRateHz: 16_000}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]Segment{{Start: 0, End: 2_000}})
	if err := os.WriteFile(filepath.Join(cell, "conversation_1.timestamps"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err == nil {
		t.Fatal("Discover accepted an out-of-range timestamp")
	}
}
