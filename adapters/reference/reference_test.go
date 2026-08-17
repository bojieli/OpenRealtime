package reference

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

func TestLoadManifestRejectsUnknownAndTrailingData(t *testing.T) {
	t.Parallel()
	valid := `{"schema_version":"0.1.0","annotation_mode":"symbolic_non_transcription","fixture_sha256":"e7adb582e0ea62d376f28b38d12bedb8ca78149eb35441775bf810c9ff2521af","item_id":"item","final_transcript":"final","response_text":"response","cues":[{"end_sample":1,"stable_text":"a","unstable_text":"","delta":"a"}]}`
	for name, data := range map[string]string{
		"unknown":  valid[:len(valid)-1] + `,"unknown":true}`,
		"trailing": valid + `{}`,
	} {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(path); err == nil {
				t.Fatal("expected strict manifest validation to fail")
			}
		})
	}
}

func TestPerceptionEnforcesOrderedPCMAndEmitsRevisions(t *testing.T) {
	t.Parallel()
	provider := NewPerception(Manifest{
		Cues:            []Cue{{EndSample: 2, StableText: "ready", Delta: "ready"}},
		FinalTranscript: "ready",
	})
	frame := engine.AudioFrame{Index: 0, SampleRateHz: audio.OpenAIPCMSampleRate, PCM16LE: make([]byte, 4)}
	revisions, err := provider.PushFrame(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 || revisions[0].StableText != "ready" || revisions[0].RevisionID != 1 {
		t.Fatalf("unexpected revisions: %+v", revisions)
	}
	final, err := provider.Finalize(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.RevisionID != 2 || final.SourceSample != 2 {
		t.Fatalf("unexpected final revision: %+v", final)
	}
	if _, err := provider.Finalize(context.Background(), 2); err == nil {
		t.Fatal("expected duplicate finalization to fail")
	}
	if _, err := provider.PushFrame(context.Background(), frame); err == nil {
		t.Fatal("expected duplicate frame to fail")
	}
}

func TestReferenceCognitionAndSpeech(t *testing.T) {
	t.Parallel()
	candidate, err := NewCognition("reference answer").Respond(context.Background(), engine.PerceptionRevision{
		RevisionID: 3, StableText: "question",
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := NewSpeech(100).Synthesize(context.Background(), engine.SpeechPlan{
		CandidateID: candidate.CandidateID, Text: candidate.Text,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || !chunks[0].Final || chunks[0].DurationNS() != 100_000_000 {
		t.Fatalf("unexpected speech chunks: %+v", chunks)
	}
	var streamed []engine.SpeechChunk
	if err := NewSpeech(100).Stream(context.Background(), engine.SpeechPlan{
		CandidateID: candidate.CandidateID, Text: candidate.Text,
	}, func(chunk engine.SpeechChunk) error {
		streamed = append(streamed, chunk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 5 || streamed[0].SampleOffset != 0 || streamed[4].SampleOffset != 1_920 || !streamed[4].Final {
		t.Fatalf("unexpected streaming speech: %+v", streamed)
	}
}
