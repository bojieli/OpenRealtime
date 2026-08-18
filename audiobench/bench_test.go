package audiobench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/livebench"
)

type mockASR struct {
	frames int
}

func (*mockASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "mock-asr", Version: "1", Capabilities: v1.Capabilities{}}
}

func (provider *mockASR) PushFrame(_ context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	provider.frames++
	if provider.frames == 1 {
		return []v1.PerceptionRevision{{RevisionID: 1, SourceSample: frame.SampleOffset + uint64(len(frame.PCM16LE)/2), UnstableText: "hello", Delta: "hello"}}, nil
	}
	return nil, nil
}

func (*mockASR) Finalize(_ context.Context, source uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 2, SourceSample: source, StableText: "hello world", Delta: " world", Final: true}, nil
}

type mockTTS struct{}

func (*mockTTS) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "mock-tts", Version: "1", Capabilities: v1.Capabilities{}}
}

func (provider *mockTTS) Stream(_ context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error) error {
	if err := consume(v1.SpeechChunk{ChunkID: "1", CandidateID: plan.CandidateID, SampleRateHz: 24_000, PCM16LE: make([]byte, 480)}); err != nil {
		return err
	}
	return consume(v1.SpeechChunk{ChunkID: "2", CandidateID: plan.CandidateID, SampleOffset: 240, SampleRateHz: 24_000, PCM16LE: make([]byte, 480), Final: true})
}

func (provider *mockTTS) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error { chunks = append(chunks, chunk); return nil })
	return chunks, err
}

func TestRunASRAndTTS(t *testing.T) {
	audio := livebench.Audio{SampleRateHz: 16_000, PCM16: make([]byte, 6_400)}
	var observed []uint64
	asr, err := RunASR(context.Background(), &mockASR{}, ASRConfig{
		CaseID: "asr", ReferenceText: "hello world", Audio: audio,
		FrameDuration: 100 * time.Millisecond, Paced: false,
		OnRevision: func(_ context.Context, revision v1.PerceptionRevision) error {
			observed = append(observed, revision.RevisionID)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(asr.Frames) != 2 || len(asr.Revisions) != 2 || asr.FinalTranscript != "hello world" || asr.WordErrorRate == nil || *asr.WordErrorRate != 0 {
		t.Fatalf("unexpected ASR result: %+v", asr)
	}
	if asr.SchedulerTicks != 2 || asr.ProviderInvocations != 2 || asr.Frames[0].ProviderInvocations != 1 {
		t.Fatalf("unexpected ASR invocation accounting: %+v", asr)
	}
	if len(observed) != 2 || observed[0] != 1 || observed[1] != 2 {
		t.Fatalf("revision observer order = %v", observed)
	}
	tts, err := RunTTS(context.Background(), &mockTTS{}, TTSConfig{CaseID: "tts", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tts.Chunks) != 2 || tts.OutputBytes != 960 || tts.OutputDurationMS != 20 {
		t.Fatalf("unexpected TTS result: %+v", tts)
	}
}

func TestWordErrorRate(t *testing.T) {
	if got := WordErrorRate("Hello, brave world!", "hello world"); got != 1.0/3.0 {
		t.Fatalf("WER = %v", got)
	}
	if got := WordErrorRate("", "extra"); got != 1 {
		t.Fatalf("empty-reference WER = %v", got)
	}
}

func TestWriteReport(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "nested", "report.json")
	if err := WriteReport(filename, Report{ASR: &ASRResult{CaseID: "case"}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != SchemaVersion || report.CreatedAt.IsZero() || report.ASR.CaseID != "case" {
		t.Fatalf("unexpected report: %+v", report)
	}
}

var _ v1.PerceptionProvider = (*mockASR)(nil)
var _ v1.StreamingSpeechProvider = (*mockTTS)(nil)
