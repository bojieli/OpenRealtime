package v1

import (
	"math"
	"os"
	"strings"
	"testing"
)

func TestStableTimingAndOverflowValidation(t *testing.T) {
	t.Parallel()
	frame := AudioFrame{SampleRateHz: 24_000, PCM16LE: make([]byte, 960)}
	end, err := frame.EndSample()
	if err != nil || end != 480 {
		t.Fatalf("unexpected frame end: %d, %v", end, err)
	}
	chunk := SpeechChunk{ChunkID: "c", CandidateID: "candidate", SampleRateHz: 24_000, PCM16LE: make([]byte, 960)}
	duration, err := chunk.DurationNS()
	if err != nil || duration != 20_000_000 {
		t.Fatalf("unexpected duration: %d, %v", duration, err)
	}
	frame.SampleOffset = math.MaxUint64
	if _, err := frame.EndSample(); err == nil {
		t.Fatal("frame overflow was accepted")
	}
}

// Version names the component contract frozen at the api/v1 import path, and
// it is deliberately not the release version. This test used to require the
// two to be equal, which read as a consistency check and was really a
// coupling: it meant the contract constant had to move every time the project
// cut a release, and it meant a release could not be numbered independently of
// a contract that is supposed to be frozen. The release version is checked
// against the VERSION file in cmd/openrealtime instead, where it belongs.
//
// What stays asserted here is the freeze itself. Changing this constant is a
// change to the promise in docs/api-v1.md, and a breaking change needs api/v2
// rather than a new number here.
func TestVersionAndDescriptorAreStable(t *testing.T) {
	t.Parallel()
	if Version != "1.0.0" {
		t.Fatalf("unexpected API version %q", Version)
	}
	release, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(release)) == "" {
		t.Fatal("VERSION is empty, so the release the contract ships in cannot be named")
	}
	descriptor := Descriptor{Name: "example", Version: "1", Capabilities: Capabilities{CapabilityDeterministic: true}}
	if err := descriptor.Validate(); err != nil || !descriptor.Capabilities.Has(CapabilityDeterministic) {
		t.Fatalf("invalid descriptor: %v", err)
	}
}

// A recogniser asked about audio with no words in it still answers, and the
// answers differ: SenseVoice says ".", others return whatever punctuation
// their language model puts around nothing. Every adapter has to report that
// the same way and every consumer has to read it the same way, or one of them
// turns room tone into something the user said.
func TestARevisionWithNoWordsReportsNoSpeech(t *testing.T) {
	for _, text := range []string{"", " ", ".", " . ", "。", "…", "-", "!?", "、。"} {
		revision := PerceptionRevision{StableText: text}
		if revision.CarriesSpeech() {
			t.Errorf("%q has no words in it", text)
		}
	}
	for _, text := range []string{"hello", "嗯", "42", "ABC123", "a.", "¿qué?"} {
		revision := PerceptionRevision{StableText: text}
		if !revision.CarriesSpeech() {
			t.Errorf("%q is something the user said", text)
		}
	}
	// The two halves are read together, because a stable prefix with an
	// unstable tail is one transcript.
	split := PerceptionRevision{StableText: ".", UnstableText: "hello"}
	if !split.CarriesSpeech() {
		t.Error("the unstable half is part of what was heard")
	}
}
