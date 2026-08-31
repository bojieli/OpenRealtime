package candidate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

type lifecyclePlugin struct {
	begin       []candidate.Attempt
	completions []candidate.Completion
	finish      []bench.Result
	captureErr  error
	completeErr error
	aborts      int
}

func (plugin *lifecyclePlugin) BeginAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.begin = append(plugin.begin, attempt)
	return &lifecycleAttempt{plugin: plugin}, nil
}

func (plugin *lifecyclePlugin) FinishSuite(_ context.Context, result bench.Result) error {
	plugin.finish = append(plugin.finish, result)
	if len(result.Tasks) > 0 {
		result.Tasks[0].ID = "mutated"
	}
	return nil
}

type lifecycleAttempt struct{ plugin *lifecyclePlugin }

func TestRequirePluginRejectsNilAndTypedNil(t *testing.T) {
	var typed *lifecyclePlugin
	for _, plugin := range []candidate.Plugin{nil, typed} {
		if err := candidate.RequirePlugin(plugin); err == nil {
			t.Fatal("nil candidate evidence plug-in was accepted")
		}
	}
	if err := candidate.RequirePlugin(&lifecyclePlugin{}); err != nil {
		t.Fatal(err)
	}
}

func (attempt *lifecycleAttempt) CaptureAudio(bench.SessionAudioCapture) error {
	return attempt.plugin.captureErr
}
func (attempt *lifecycleAttempt) CaptureVideo(bench.SessionVideoCapture) error { return nil }
func (attempt *lifecycleAttempt) Complete(_ context.Context, completion candidate.Completion) error {
	attempt.plugin.completions = append(attempt.plugin.completions, completion)
	return attempt.plugin.completeErr
}
func (attempt *lifecycleAttempt) Abort() error {
	attempt.plugin.aborts++
	return nil
}

func fixtureLifecycle(t *testing.T, plugin candidate.Plugin) *candidate.Lifecycle {
	t.Helper()
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: plugin, Suite: "suite", Cell: bench.Reference(),
		Provenance: bench.Provenance{Revision: "candidate"}, Origin: origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle
}

func TestLifecycleCommitsOneCandidateAndFreezesFinalResult(t *testing.T) {
	plugin := &lifecyclePlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"})
	if err != nil {
		t.Fatal(err)
	}
	specification, err := attempt.Specification()
	if err != nil || specification.Case != "case" {
		t.Fatalf("specification = %+v, err = %v", specification, err)
	}
	outcome := bench.TaskOutcome{ID: "case", Completed: true, Passed: true}
	if err := attempt.Complete(outcome, bench.Transcript{}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: "suite", Cell: bench.Reference(), Provenance: bench.Provenance{Revision: "candidate"},
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := lifecycle.Finish(result); err != nil {
		t.Fatal(err)
	}
	if len(plugin.begin) != 1 || len(plugin.completions) != 1 || len(plugin.finish) != 1 {
		t.Fatalf("plug-in calls: begin=%d complete=%d finish=%d",
			len(plugin.begin), len(plugin.completions), len(plugin.finish))
	}
	if result.Tasks[0].ID != "case" {
		t.Fatal("plug-in mutation escaped the frozen final result")
	}
	if _, err := lifecycle.Begin("later", 1, map[string]any{"criterion": "late"}); err == nil {
		t.Fatal("post-finish attempt was accepted")
	}
}

func TestLifecycleRefusesDuplicateAndUncommittedCapture(t *testing.T) {
	captureFailure := errors.New("audio sink failed")
	plugin := &lifecyclePlugin{captureErr: captureFailure}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"}); err == nil {
		t.Fatal("duplicate attempt identity was accepted")
	}
	if err := attempt.CaptureAudio(bench.SessionAudioCapture{}); !errors.Is(err, captureFailure) {
		t.Fatalf("capture error = %v", err)
	}
	outcome := bench.TaskOutcome{ID: "case", Completed: false, Error: "capture failed"}
	if err := attempt.Complete(outcome, bench.Transcript{}); !errors.Is(err, captureFailure) {
		t.Fatalf("completion error = %v", err)
	}
	result := bench.Result{
		Suite: "suite", Cell: bench.Reference(), Provenance: bench.Provenance{Revision: "candidate"},
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := lifecycle.Finish(result); err == nil {
		t.Fatal("uncommitted attempt was accepted at suite finish")
	}
	if plugin.aborts != 0 || len(plugin.finish) != 1 {
		t.Fatalf("abort=%d finish=%d", plugin.aborts, len(plugin.finish))
	}
}

func TestLifecycleRefusesFinishWhileAttemptIsActive(t *testing.T) {
	plugin := &lifecyclePlugin{}
	lifecycle := fixtureLifecycle(t, plugin)
	attempt, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"})
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: "suite", Cell: bench.Reference(), Provenance: bench.Provenance{Revision: "candidate"},
	}
	result.Finish()
	if err := lifecycle.Finish(result); err == nil {
		t.Fatal("suite finish accepted an active attempt")
	}
	if err := attempt.Abort(); err != nil {
		t.Fatal(err)
	}
}
