package scenario

import (
	"os"
	"path/filepath"
	"testing"
)

// A benchmark holds its input constant. Re-synthesising every line on every
// run re-rolls two dice at once: the timing, because a synthesiser does not
// produce the same durations twice and every check window is anchored to where
// a line ends, and the recognition, because a slightly different waveform is
// heard differently - "the build" came back as "the bill" in two runs of five
// and cost both.
func TestSpeechIsTheSameSpeechOnEveryRun(t *testing.T) {
	CacheDir = t.TempDir()
	samples := []int16{0, 1, -1, 32767, -32768, 5}
	key := cacheKey("fish/s2-pro", "alloy", "tell me the moment the build finishes")

	if _, ok := readCached(key); ok {
		t.Fatal("an empty cache must not answer")
	}
	writeCached(key, samples)
	got, ok := readCached(key)
	if !ok {
		t.Fatal("what was written must come back")
	}
	if len(got) != len(samples) {
		t.Fatalf("length changed: %d against %d", len(got), len(samples))
	}
	for index := range samples {
		if got[index] != samples[index] {
			t.Fatalf("sample %d changed: %d against %d", index, got[index], samples[index])
		}
	}

	// The voice is part of the key, or a scenario with a waiter in it would
	// get the user's voice back for both and lose the difficulty it poses.
	if other := cacheKey("fish/s2-pro", "echo", "tell me the moment the build finishes"); other == key {
		t.Fatal("two voices must not share a cache entry")
	}
	if other := cacheKey("other-model", "alloy", "tell me the moment the build finishes"); other == key {
		t.Fatal("two models must not share a cache entry")
	}

	// A run interrupted mid-write must not leave a truncated line that every
	// later run then treats as the audio.
	if entries, err := os.ReadDir(CacheDir); err == nil {
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".part" {
				t.Fatalf("a partial write was left behind: %s", entry.Name())
			}
		}
	}
}
