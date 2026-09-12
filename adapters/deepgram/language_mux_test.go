package deepgram

import (
	"context"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

type scriptedLanguageStream struct {
	pushes     [][]v1.PerceptionRevision
	final      v1.PerceptionRevision
	endpoint   bool
	confidence float64
	closed     bool
}

func (stream *scriptedLanguageStream) Descriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: "deepgram-listen/nova-test", Version: "test",
		Capabilities: v1.Capabilities{v1.CapabilityStreamingInput: true},
	}
}

func (stream *scriptedLanguageStream) PushFrame(
	context.Context, v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	if len(stream.pushes) == 0 {
		return nil, nil
	}
	result := stream.pushes[0]
	stream.pushes = stream.pushes[1:]
	return result, nil
}

func (stream *scriptedLanguageStream) Finalize(
	context.Context, uint64,
) (v1.PerceptionRevision, error) {
	return stream.final, nil
}

func (stream *scriptedLanguageStream) SpeechEndpointed() bool { return stream.endpoint }
func (stream *scriptedLanguageStream) Confidence() float64    { return stream.confidence }
func (stream *scriptedLanguageStream) EndUtterance() error    { return nil }

func (stream *scriptedLanguageStream) Close() error {
	stream.closed = true
	return nil
}

func TestLanguageMuxWithholdsPhoneticEnglishUntilChineseCanWin(t *testing.T) {
	primary := &scriptedLanguageStream{pushes: [][]v1.PerceptionRevision{
		{{RevisionID: 1, SourceSample: 100, UnstableText: "nee how"}},
		{{RevisionID: 2, SourceSample: 200, UnstableText: "nee how hen gao"}},
	}, confidence: 0.62}
	chinese := &scriptedLanguageStream{pushes: [][]v1.PerceptionRevision{
		nil,
		{{RevisionID: 1, SourceSample: 200, UnstableText: "你好，很高兴"}},
	}, confidence: 0.99}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	frame := v1.AudioFrame{Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: []byte{0, 0}}
	if revisions, err := mux.PushFrame(context.Background(), frame); err != nil || len(revisions) != 0 {
		t.Fatalf("first phonetic hypothesis escaped: revisions=%+v err=%v", revisions, err)
	}
	frame.Index, frame.SampleOffset = 1, 1
	revisions, err := mux.PushFrame(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 || revisionText(revisions[0]) != "你好，很高兴" {
		t.Fatalf("Chinese stream did not replace held phonetics: %+v", revisions)
	}
}

func TestLanguageMuxReleasesAHighConfidenceEnglishHypothesis(t *testing.T) {
	primary := &scriptedLanguageStream{pushes: [][]v1.PerceptionRevision{
		{{RevisionID: 1, SourceSample: 100, UnstableText: "hello there"}}, nil,
	}, confidence: 0.99}
	chinese := &scriptedLanguageStream{pushes: [][]v1.PerceptionRevision{nil, nil}}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	frame := v1.AudioFrame{Index: 0, SampleOffset: 0, SampleRateHz: 16_000, PCM16LE: []byte{0, 0}}
	revisions, err := mux.PushFrame(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 1 || revisionText(revisions[0]) != "hello there" {
		t.Fatalf("high-confidence primary hypothesis was not released: %+v", revisions)
	}
	frame.Index, frame.SampleOffset = 1, 1
	revisions, err = mux.PushFrame(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 0 {
		t.Fatalf("an unchanged primary hypothesis was repeated: %+v", revisions)
	}
}

func TestLanguageMuxChoosesChineseAtFinalizationAndClosesBothLanes(t *testing.T) {
	primary := &scriptedLanguageStream{final: v1.PerceptionRevision{StableText: "hang out then", Final: true}, confidence: 0.42}
	chinese := &scriptedLanguageStream{final: v1.PerceptionRevision{StableText: "很高兴见到你", Final: true}, confidence: 0.99}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	final, err := mux.Finalize(context.Background(), 900)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.SourceSample != 900 || final.StableText != "很高兴见到你" {
		t.Fatalf("final revision = %+v", final)
	}
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	if !primary.closed || !chinese.closed {
		t.Fatal("closing the mux did not close both Deepgram lanes")
	}
}

func TestLanguageMuxRequiresBothUnknownLanesToEndpoint(t *testing.T) {
	primary := &scriptedLanguageStream{endpoint: true}
	chinese := &scriptedLanguageStream{endpoint: false}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	if mux.SpeechEndpointed() {
		t.Fatal("one unselected lane ended the utterance")
	}
	chinese.endpoint = true
	if !mux.SpeechEndpointed() {
		t.Fatal("two endpointed lanes did not end the utterance")
	}
}

func TestLanguageMuxChineseHelloCannotConsumeEnglishQuestion(t *testing.T) {
	primary := &scriptedLanguageStream{pushes: [][]v1.PerceptionRevision{nil, {{UnstableText: "Hello, my home internet is slow. Can you help me?"}}}, confidence: 0.99, final: v1.PerceptionRevision{StableText: "Hello, my home internet is slow. Can you help me?"}}
	chinese := &scriptedLanguageStream{pushes: [][]v1.PerceptionRevision{{{UnstableText: "哈喽"}}, nil}, confidence: 0.99, endpoint: true, final: v1.PerceptionRevision{StableText: "哈喽"}}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	if r, err := mux.PushFrame(context.Background(), v1.AudioFrame{}); err != nil || len(r) != 0 || mux.SpeechEndpointed() {
		t.Fatalf("short greeting selected/ended Chinese lane: %+v %v", r, err)
	}
	r, err := mux.PushFrame(context.Background(), v1.AudioFrame{})
	if err != nil || len(r) != 1 || revisionText(r[0]) != primary.final.StableText {
		t.Fatalf("English question lost: %+v %v", r, err)
	}
	final, err := mux.Finalize(context.Background(), 1000)
	if err != nil || final.StableText != primary.final.StableText {
		t.Fatalf("English final lost: %+v %v", final, err)
	}
}

func TestLanguageMuxShortChineseStillAvailableAtFinal(t *testing.T) {
	primary := &scriptedLanguageStream{confidence: 0.4, final: v1.PerceptionRevision{StableText: "nee how"}}
	chinese := &scriptedLanguageStream{confidence: 0.99, final: v1.PerceptionRevision{StableText: "你好"}}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	final, err := mux.Finalize(context.Background(), 1000)
	if err != nil || final.StableText != "你好" {
		t.Fatalf("short Chinese final lost: %+v %v", final, err)
	}
}

func TestLanguageMuxPrefersStrongerEnglishOverCompetingHanReading(t *testing.T) {
	primary := &scriptedLanguageStream{confidence: 0.99, pushes: [][]v1.PerceptionRevision{{{UnstableText: "Hello, can you hear me?"}}}}
	chinese := &scriptedLanguageStream{confidence: 0.86, pushes: [][]v1.PerceptionRevision{{{UnstableText: "哈喽可以听见"}}}}
	mux := newLanguageMux(primary, chinese, primary.Descriptor())
	revisions, err := mux.PushFrame(context.Background(), v1.AudioFrame{})
	if err != nil || len(revisions) != 1 || revisionText(revisions[0]) != "Hello, can you hear me?" {
		t.Fatalf("weaker competing language won: %+v %v", revisions, err)
	}
}
