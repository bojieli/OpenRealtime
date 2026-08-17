package conformance

import (
	"context"
	"path/filepath"
	"testing"

	reference "github.com/bojieli/OpenRealtime/adapters/reference"
	referencev1 "github.com/bojieli/OpenRealtime/adapters/reference/v1"
	stable "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/fixture"
)

func TestStableReferenceProvidersConform(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	manifest, err := reference.LoadManifest(filepath.Join(root, "tests", "fixtures", "m1-reference-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	workload, err := reference.LoadDifficultWorkload(filepath.Join(root, "tests", "fixtures", "m4-difficult-workload.json"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := fixture.Load(filepath.Join(root, "tests", "fixtures", "m0-tone.wav"), 20, "m7-test")
	if err != nil {
		t.Fatal(err)
	}
	frames := make([]stable.AudioFrame, len(input.Frames))
	for index, frame := range input.Frames {
		frames[index] = stable.AudioFrame{
			Index: frame.Index, SampleOffset: frame.SampleOffset,
			SampleRateHz: frame.SampleRateHz, PCM16LE: frame.PCM16LE,
		}
	}
	task := workload.Tasks[0]
	report, err := RunProviders(context.Background(), stable.ProviderSet{
		Perception:   referencev1.NewPerception(manifest),
		Cognition:    referencev1.NewCognition(manifest.ResponseText),
		Speech:       referencev1.NewSpeech(100),
		Fast:         referencev1.NewFast(task, reference.FastModeAcknowledge),
		Deliberation: referencev1.NewDeliberation(task, reference.DeliberationComplete),
	}, ProviderProbe{
		Frames: frames, EndSample: input.SampleCount, SpeechText: manifest.ResponseText,
		Goal: stable.GoalSnapshot{GoalID: task.ID, RevisionID: 1, Question: task.Question, DeadlineNS: 500_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Frames != 50 || report.Revisions != 4 || report.SynthesizedChunks != 1 || report.StreamedChunks != 5 || report.DeliberationUpdates != 2 || report.CancellationChecks != 6 || report.BehaviorChecks != 8 {
		t.Fatalf("unexpected provider conformance: %+v", report)
	}
}
