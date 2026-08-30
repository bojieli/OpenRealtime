package candidate_test

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

type mediaPlugin struct {
	captured  []candidate.CapturedMedia
	artifacts []candidate.CapturedArtifact
}

func (plugin *mediaPlugin) BeginAttempt(
	_ context.Context, _ candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	return &mediaAttempt{plugin: plugin}, nil
}
func (plugin *mediaPlugin) FinishSuite(context.Context, bench.Result) error { return nil }

type mediaAttempt struct{ plugin *mediaPlugin }

func (*mediaAttempt) CaptureAudio(bench.SessionAudioCapture) error { return nil }
func (*mediaAttempt) CaptureVideo(bench.SessionVideoCapture) error { return nil }
func (attempt *mediaAttempt) CaptureMedia(media candidate.CapturedMedia) error {
	attempt.plugin.captured = append(attempt.plugin.captured, media)
	return nil
}
func (attempt *mediaAttempt) CaptureArtifact(artifact candidate.CapturedArtifact) error {
	attempt.plugin.artifacts = append(attempt.plugin.artifacts, artifact)
	return nil
}
func (*mediaAttempt) Complete(context.Context, candidate.Completion) error { return nil }
func (*mediaAttempt) Abort() error                                         { return nil }

func TestCapturedMediaIsValidatedAndOwnedByThePlugin(t *testing.T) {
	plugin := &mediaPlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.BeginExternal("case", 1, map[string]any{"criterion": "listen"})
	if err != nil {
		t.Fatal(err)
	}
	source := candidate.CapturedMedia{
		Name: "conversation.wav", Kind: "audio", Role: "user_left_agent_right",
		MediaType: "audio/wav", Bytes: []byte("RIFF-owned-test-bytes"),
	}
	if err := attempt.CaptureMedia(source); err != nil {
		t.Fatal(err)
	}
	source.Bytes[0] = 'X'
	if len(plugin.captured) != 1 || plugin.captured[0].Bytes[0] != 'R' {
		t.Fatal("captured-media plug-in aliases caller-owned bytes")
	}
	artifact := candidate.CapturedArtifact{
		Name: "simulation.json", Kind: "trace", Role: "upstream_simulation",
		ContentType: "application/json", Bytes: []byte(`{"id":"candidate"}`),
	}
	if err := attempt.CaptureArtifact(artifact); err != nil {
		t.Fatal(err)
	}
	artifact.Bytes[0] = 'X'
	if len(plugin.artifacts) != 1 || plugin.artifacts[0].Bytes[0] != '{' {
		t.Fatal("captured-artifact plug-in aliases caller-owned bytes")
	}
	if err := attempt.Complete(bench.TaskOutcome{ID: "case"}, bench.Transcript{}); err != nil {
		t.Fatal(err)
	}
}

func TestCapturedMediaRefusesTraversalAndUnsupportedTypes(t *testing.T) {
	plugin := &mediaPlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.BeginExternal("case", 1, map[string]any{"criterion": "listen"})
	if err != nil {
		t.Fatal(err)
	}
	for _, media := range []candidate.CapturedMedia{
		{Name: "../outside.wav", Kind: "audio", Role: "both", MediaType: "audio/wav", Bytes: []byte("x")},
		{Name: string([]byte{'b', 'a', 'd', 0xff}) + ".wav", Kind: "audio", Role: "both", MediaType: "audio/wav", Bytes: []byte("x")},
		{Name: "payload.bin", Kind: "binary", Role: "both", MediaType: "application/octet-stream", Bytes: []byte("x")},
	} {
		if err := attempt.CaptureMedia(media); err == nil {
			t.Fatalf("invalid captured media was accepted: %+v", media)
		}
	}
	if err := attempt.Abort(); err == nil {
		t.Fatal("invalid captured media did not remain a terminal evidence failure")
	}
}

func TestCapturedArtifactRefusesTraversalAndUnsupportedTypes(t *testing.T) {
	plugin := &mediaPlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.BeginExternal("case", 1, map[string]any{"criterion": "listen"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []candidate.CapturedArtifact{
		{Name: "../outside.json", Kind: "trace", Role: "source", ContentType: "application/json", Bytes: []byte("{}")},
		{Name: "payload.bin", Kind: "binary", Role: "source", ContentType: "application/octet-stream", Bytes: []byte("x")},
	} {
		if err := attempt.CaptureArtifact(artifact); err == nil {
			t.Fatalf("invalid captured artifact was accepted: %+v", artifact)
		}
	}
	if err := attempt.Abort(); err == nil {
		t.Fatal("invalid captured artifact did not remain a terminal evidence failure")
	}
}

func TestCapturedMediaIsExclusiveToExternalHarnessAttempts(t *testing.T) {
	plugin := &mediaPlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "listen"})
	if err != nil {
		t.Fatal(err)
	}
	err = attempt.CaptureMedia(candidate.CapturedMedia{
		Name: "conversation.wav", Kind: "audio", Role: "both",
		MediaType: "audio/wav", Bytes: []byte("RIFF-audio"),
	})
	if err == nil {
		t.Fatal("a shared-session attempt accepted external-harness media")
	}
	if err := attempt.CaptureArtifact(candidate.CapturedArtifact{
		Name: "simulation.json", Kind: "trace", Role: "source",
		ContentType: "application/json", Bytes: []byte("{}"),
	}); err == nil {
		t.Fatal("a shared-session attempt accepted an external-harness artifact")
	}
	if abortErr := attempt.Abort(); abortErr == nil {
		t.Fatal("the invalid media source was not retained as an evidence failure")
	}
}

func TestCapturedMediaRequiresPluginCapability(t *testing.T) {
	plugin := &lifecyclePlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.BeginExternal("case", 1, map[string]any{"criterion": "listen"})
	if err != nil {
		t.Fatal(err)
	}
	err = attempt.CaptureMedia(candidate.CapturedMedia{
		Name: "conversation.wav", Kind: "audio", Role: "both",
		MediaType: "audio/wav", Bytes: []byte("RIFF-audio"),
	})
	if err == nil {
		t.Fatal("an external-harness attempt accepted a plug-in without media-import support")
	}
	if err := attempt.CaptureArtifact(candidate.CapturedArtifact{
		Name: "simulation.json", Kind: "trace", Role: "source",
		ContentType: "application/json", Bytes: []byte("{}"),
	}); err == nil {
		t.Fatal("an external-harness attempt accepted a plug-in without artifact-import support")
	}
	if abortErr := attempt.Abort(); abortErr == nil {
		t.Fatal("the missing plug-in capability was not retained as an evidence failure")
	}
}
