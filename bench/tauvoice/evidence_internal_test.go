package tauvoice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

type tauCandidatePlugin struct {
	attempts    []candidate.Attempt
	media       []candidate.CapturedMedia
	artifacts   []candidate.CapturedArtifact
	completions []candidate.Completion
	finishes    []bench.Result
	recovered   *bench.TaskOutcome
	recoveries  int
}

type tauRecoveredEvidence struct{ transcript bench.Transcript }

func (evidence tauRecoveredEvidence) ReopenTranscript(context.Context) (bench.Transcript, error) {
	return evidence.transcript, nil
}
func (tauRecoveredEvidence) CommitValidated(context.Context) error { return nil }

func (plugin *tauCandidatePlugin) BindRun(
	_ context.Context, _ string, _ bench.Cell, provenance bench.Provenance, _ candidate.RunOrigin,
) (bench.Provenance, error) {
	return provenance, nil
}

func (plugin *tauCandidatePlugin) BeginAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.attempts = append(plugin.attempts, attempt)
	return &tauCandidateAttempt{plugin: plugin}, nil
}

func (plugin *tauCandidatePlugin) FinishSuite(_ context.Context, result bench.Result) error {
	plugin.finishes = append(plugin.finishes, result)
	return nil
}

func (plugin *tauCandidatePlugin) RecoverAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.Recovery, bool, error) {
	if plugin.recovered == nil {
		return candidate.Recovery{}, false, nil
	}
	plugin.recoveries++
	transcript := bench.Transcript{PlaybackMS: plugin.recovered.Metrics["simulation_duration_ms"]}
	return candidate.Recovery{
		Completion: candidate.Completion{
			Attempt: attempt, Outcome: *plugin.recovered, Transcript: transcript,
		},
		Evidence: tauRecoveredEvidence{transcript: transcript},
	}, true, nil
}

type tauCandidateAttempt struct{ plugin *tauCandidatePlugin }

func (*tauCandidateAttempt) CaptureAudio(bench.SessionAudioCapture) error { return nil }
func (*tauCandidateAttempt) CaptureVideo(bench.SessionVideoCapture) error { return nil }
func (attempt *tauCandidateAttempt) CaptureMedia(media candidate.CapturedMedia) error {
	attempt.plugin.media = append(attempt.plugin.media, media)
	return nil
}
func (attempt *tauCandidateAttempt) CaptureArtifact(artifact candidate.CapturedArtifact) error {
	attempt.plugin.artifacts = append(attempt.plugin.artifacts, artifact)
	return nil
}
func (attempt *tauCandidateAttempt) Complete(
	_ context.Context, completion candidate.Completion,
) error {
	attempt.plugin.completions = append(attempt.plugin.completions, completion)
	return nil
}
func (*tauCandidateAttempt) Abort() error { return nil }

func TestRetainCandidateOutcomeImportsPinnedTauArtifacts(t *testing.T) {
	const (
		runName      = "candidate-telecom-regular"
		simulationID = "70bbf463-1baf-48be-9ae5-a66dd9d4ac22"
		taskID       = "[mobile_data_issue]airplane_mode_on|user_abroad_roaming_enabled_off[PERSONA:None]"
	)
	config := Config{
		Tau2Dir: t.TempDir(), Condition: Regular, Cadence: 0.2,
		Model: "agent", UserModel: "caller", SynthesisProvider: "local",
		SynthesisModel: "speech", SynthesisVoice: "voice",
	}
	saveTo := config.simulationDir(runName)
	simulation := []byte("{\n  \"messages\": [],\n  \"id\": \"" + simulationID + "\"\n}\n")
	audio := []byte("RIFF-exact-stereo-candidate-audio")
	writeTauFixture(t, filepath.Join(saveTo, "simulations", simulationID+".json"), simulation)
	writeTauFixture(t, filepath.Join(
		saveTo, "artifacts", "task_"+taskID, "sim_"+simulationID, "audio", "both.wav",
	), audio)

	plugin := &tauCandidatePlugin{}
	cell := bench.Reference()
	provenance := bench.Provenance{Revision: "candidate"}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginProduction, bench.TransportWebSocket, "wss://example.test/v1/realtime?key=secret",
	)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: plugin, Suite: "tau-voice", Cell: cell,
		Provenance: provenance, Origin: origin,
		RecoveryValidator: refuseRecoveredOutcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{
		ID: "telecom/" + taskID + "/trial-0", Completed: true, Passed: true,
		Metrics: map[string]float64{"simulation_duration_ms": 1234},
		Notes: map[string]string{
			"simulation_id": simulationID, "task_id": taskID, "trial_index": "0",
		},
	}
	if err := config.retainCandidateOutcome(
		t.Context(), lifecycle, "telecom", runName, outcome,
	); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: "tau-voice", Cell: cell, Provenance: provenance, Expected: 1,
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := lifecycle.Finish(result); err != nil {
		t.Fatal(err)
	}

	if len(plugin.attempts) != 1 || plugin.attempts[0].MediaSource != candidate.MediaExternalHarness {
		t.Fatalf("candidate attempts = %+v", plugin.attempts)
	}
	if !strings.Contains(string(plugin.attempts[0].Context), `"task_id":"`+taskID+`"`) {
		t.Fatalf("candidate context omitted pinned tau evidence: %s", plugin.attempts[0].Context)
	}
	var retained tauCandidateContext
	if err := json.Unmarshal(plugin.attempts[0].Context, &retained); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(simulation)
	if retained.SimulationSHA256 != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatalf("simulation digest %q does not cover the exact upstream JSON", retained.SimulationSHA256)
	}
	if retained.SimulationBytes != len(simulation) || len(plugin.artifacts) != 1 ||
		plugin.artifacts[0].Name != "simulation.json" ||
		plugin.artifacts[0].Role != "upstream_simulation" ||
		string(plugin.artifacts[0].Bytes) != string(simulation) {
		t.Fatalf("captured simulation artifact = %+v", plugin.artifacts)
	}
	if len(plugin.media) != 1 || plugin.media[0].Name != "conversation.stereo.wav" ||
		plugin.media[0].Role != "time_aligned_user_left_agent_right" ||
		string(plugin.media[0].Bytes) != string(audio) {
		t.Fatalf("captured media = %+v", plugin.media)
	}
	if len(plugin.completions) != 1 || plugin.completions[0].Transcript.PlaybackMS != 1234 ||
		len(plugin.finishes) != 1 {
		t.Fatalf("completion=%+v finishes=%d", plugin.completions, len(plugin.finishes))
	}
}

func TestRetainCandidateOutcomeRecoveryFailsClosedWithoutReplayableTauScore(t *testing.T) {
	const (
		runName      = "candidate-telecom-regular"
		simulationID = "70bbf463-1baf-48be-9ae5-a66dd9d4ac22"
		taskID       = "telecom-task"
	)
	config := Config{
		Tau2Dir: t.TempDir(), Condition: Regular, Cadence: 0.2,
		Model: "agent", UserModel: "caller", SynthesisProvider: "local",
		SynthesisModel: "speech", SynthesisVoice: "voice",
	}
	simulation := []byte("{\n  \"messages\": [],\n  \"id\": \"" + simulationID + "\"\n}\n")
	writeTauFixture(t, filepath.Join(
		config.simulationDir(runName), "simulations", simulationID+".json",
	), simulation)
	// No audio is created. A recovered completion must return before the
	// external-harness media path is opened.
	outcome := bench.TaskOutcome{
		ID: "telecom/" + taskID + "/trial-0", Completed: true, Passed: true,
		Metrics: map[string]float64{"simulation_duration_ms": 1234},
		Notes: map[string]string{
			"simulation_id": simulationID, "task_id": taskID, "trial_index": "0",
		},
	}
	plugin := &tauCandidatePlugin{recovered: &outcome}
	cell := bench.Reference()
	provenance := bench.Provenance{Revision: "candidate"}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: plugin, Suite: "tau-voice", Cell: cell,
		Provenance: provenance, Origin: origin,
		RecoveryValidator: refuseRecoveredOutcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.retainCandidateOutcome(
		t.Context(), lifecycle, "telecom", runName, outcome,
	); err == nil || !strings.Contains(err.Error(), "cannot be deterministically rescored") {
		t.Fatalf("tau recovery error = %v", err)
	}
	if plugin.recoveries != 1 || len(plugin.attempts) != 0 || len(plugin.media) != 0 ||
		len(plugin.artifacts) != 0 || len(plugin.completions) != 0 || len(plugin.finishes) != 0 {
		t.Fatalf("tau recovery: recover=%d attempts=%d media=%d artifacts=%d completions=%d finishes=%d",
			plugin.recoveries, len(plugin.attempts), len(plugin.media), len(plugin.artifacts),
			len(plugin.completions), len(plugin.finishes))
	}
}

func TestReadTauArtifactRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.wav")
	writeTauFixture(t, outside, []byte("outside"))
	if _, err := readTauArtifact(root, 1024, "..", filepath.Base(outside)); err == nil {
		t.Fatal("tau artifact traversal was accepted")
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := readTauArtifact(root, 1024, "linked.wav"); err == nil {
		t.Fatal("tau artifact symlink was accepted")
	}
	if !safeTauArtifactComponent("task_[case:a|b]") {
		t.Fatal("a real tau telecom task component was rejected")
	}
	for _, unsafe := range []string{"", ".", "..", "../escape", `a\b`, " padded ", "control\x00"} {
		if safeTauArtifactComponent(unsafe) {
			t.Fatalf("unsafe tau artifact component %q was accepted", unsafe)
		}
	}
}

func TestVerifyRejectsCandidateOriginBeforeInspectingCheckout(t *testing.T) {
	config := Config{
		Tau2Dir: filepath.Join(t.TempDir(), "missing"), Endpoint: "ws://127.0.0.1:8080/v1/realtime",
		Cell: bench.Reference(), Evidence: &tauCandidatePlugin{},
	}
	err := config.Verify(t.Context())
	if err == nil || !strings.Contains(err.Error(), "candidate evidence origin") {
		t.Fatalf("Verify error = %v", err)
	}
}

func TestRetainCandidateOutcomeRejectsMissingArtifactIdentity(t *testing.T) {
	plugin := &tauCandidatePlugin{}
	cell := bench.Reference()
	provenance := bench.Provenance{Revision: "candidate"}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: plugin, Suite: "tau-voice", Cell: cell,
		Provenance: provenance, Origin: origin,
		RecoveryValidator: refuseRecoveredOutcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Tau2Dir: t.TempDir(), Condition: Control}
	outcome := bench.TaskOutcome{
		ID: "telecom/missing/trial-0", Completed: false,
		Notes: map[string]string{"trial_index": "0"},
	}
	err = config.retainCandidateOutcome(t.Context(), lifecycle, "telecom", "missing", outcome)
	if err == nil || !strings.Contains(err.Error(), "artifact identity") {
		t.Fatalf("retain error = %v", err)
	}
	if len(plugin.completions) != 1 || len(plugin.media) != 0 {
		t.Fatalf("completion=%d media=%d", len(plugin.completions), len(plugin.media))
	}
	result := bench.Result{
		Suite: "tau-voice", Cell: cell, Provenance: provenance, Expected: 1,
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if finishErr := lifecycle.Finish(result); finishErr == nil ||
		!strings.Contains(finishErr.Error(), "committed 0 of 1") {
		t.Fatalf("finish error = %v", finishErr)
	}
}

func writeTauFixture(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

var _ candidate.Plugin = (*tauCandidatePlugin)(nil)
var _ candidate.AttemptEvidence = (*tauCandidateAttempt)(nil)
var _ candidate.CapturedMediaEvidence = (*tauCandidateAttempt)(nil)
var _ candidate.CapturedArtifactEvidence = (*tauCandidateAttempt)(nil)
