package candidate_test

import (
	"context"
	"errors"
	"strings"
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

type resumableLifecyclePlugin struct {
	lifecyclePlugin
	retained             bench.Provenance
	bindCalls            int
	recoverCalls         int
	completionTranscript bench.Transcript
	evidenceTranscript   bench.Transcript
	evidenceErr          error
	mutateOutcome        func(*bench.TaskOutcome)
}

type recoveredTranscript struct {
	transcript bench.Transcript
	err        error
}

func (evidence recoveredTranscript) ReopenTranscript(context.Context) (bench.Transcript, error) {
	return evidence.transcript, evidence.err
}
func (recoveredTranscript) CommitValidated(context.Context) error { return nil }

func (plugin *resumableLifecyclePlugin) BindRun(
	_ context.Context, _ string, _ bench.Cell, _ bench.Provenance, _ candidate.RunOrigin,
) (bench.Provenance, error) {
	plugin.bindCalls++
	return plugin.retained, nil
}

func (plugin *resumableLifecyclePlugin) RecoverAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.Recovery, bool, error) {
	plugin.recoverCalls++
	completion := candidate.Completion{
		Attempt:    attempt,
		Outcome:    bench.TaskOutcome{ID: attempt.Case, Completed: true, Passed: true},
		Transcript: plugin.completionTranscript,
	}
	if plugin.mutateOutcome != nil {
		plugin.mutateOutcome(&completion.Outcome)
	}
	return candidate.Recovery{
		Completion: completion,
		Evidence:   recoveredTranscript{transcript: plugin.evidenceTranscript, err: plugin.evidenceErr},
	}, true, nil
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

func TestLifecycleBindsOriginalRunAndCommitsRecoveredAttempt(t *testing.T) {
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	current := bench.Provenance{
		Revision: "candidate", ExecutableSHA256: "executable", StartedAt: "2026-09-01T01:00:00Z",
	}
	retained := current
	retained.StartedAt = "2026-09-01T00:00:00Z"
	plugin := &resumableLifecyclePlugin{retained: retained}
	lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: plugin, Suite: "suite", Cell: bench.Reference(),
		Provenance: current, Origin: origin,
		RecoveryValidator: func(
			context.Context, candidate.Attempt, bench.Transcript,
		) (bench.TaskOutcome, error) {
			return bench.TaskOutcome{ID: "case", Completed: true, Passed: true}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.Provenance(); got != retained {
		t.Fatalf("bound provenance = %+v, want %+v", got, retained)
	}
	attempt, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"})
	if err != nil {
		t.Fatal(err)
	}
	completion, found, err := attempt.Recovered()
	if err != nil || !found || !completion.Outcome.Passed {
		t.Fatalf("recovered completion = %+v, found=%v err=%v", completion, found, err)
	}
	completion.Outcome.ID = "caller-mutation"
	again, found, err := attempt.Recovered()
	if err != nil || !found || again.Outcome.ID != "case" {
		t.Fatalf("recovered completion mutation escaped: %+v, found=%v err=%v", again, found, err)
	}
	result := bench.Result{
		Suite: "suite", Cell: bench.Reference(), Provenance: lifecycle.Provenance(),
		Expected: 1, Tasks: []bench.TaskOutcome{again.Outcome},
	}
	result.Finish()
	if err := lifecycle.Finish(result); err != nil {
		t.Fatal(err)
	}
	if plugin.bindCalls != 1 || plugin.recoverCalls != 1 || len(plugin.begin) != 0 ||
		len(plugin.completions) != 0 || len(plugin.finish) != 1 {
		t.Fatalf("resume calls: bind=%d recover=%d begin=%d complete=%d finish=%d",
			plugin.bindCalls, plugin.recoverCalls, len(plugin.begin), len(plugin.completions), len(plugin.finish))
	}
}

func TestLifecycleRejectsRecoveredBuildDrift(t *testing.T) {
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	current := bench.Provenance{
		Revision: "candidate", ExecutableSHA256: "current", StartedAt: "2026-09-01T01:00:00Z",
	}
	retained := current
	retained.ExecutableSHA256 = "different"
	plugin := &resumableLifecyclePlugin{retained: retained}
	if _, err := candidate.NewLifecycle(candidate.LifecycleConfig{
		Context: t.Context(), Plugin: plugin, Suite: "suite", Cell: bench.Reference(),
		Provenance: current, Origin: origin,
		RecoveryValidator: func(
			context.Context, candidate.Attempt, bench.Transcript,
		) (bench.TaskOutcome, error) {
			return bench.TaskOutcome{}, nil
		},
	}); err == nil {
		t.Fatal("candidate lifecycle accepted recovered evidence from another build")
	}
}

func TestLifecycleRecoveryFailsClosedWithoutExactDeterministicRescore(t *testing.T) {
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	provenance := bench.Provenance{Revision: "candidate", ExecutableSHA256: "executable"}
	newLifecycle := func(t *testing.T, plugin *resumableLifecyclePlugin) *candidate.Lifecycle {
		t.Helper()
		lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
			Context: t.Context(), Plugin: plugin, Suite: "suite", Cell: bench.Reference(),
			Provenance: provenance, Origin: origin,
			RecoveryValidator: func(
				context.Context, candidate.Attempt, bench.Transcript,
			) (bench.TaskOutcome, error) {
				return bench.TaskOutcome{ID: "case", Completed: true, Passed: true}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return lifecycle
	}

	t.Run("missing suite validator", func(t *testing.T) {
		plugin := &resumableLifecyclePlugin{retained: provenance}
		if _, err := candidate.NewLifecycle(candidate.LifecycleConfig{
			Context: t.Context(), Plugin: plugin, Suite: "suite", Cell: bench.Reference(),
			Provenance: provenance, Origin: origin,
		}); err == nil || !strings.Contains(err.Error(), "suite-owned deterministic validator") {
			t.Fatalf("missing validator error = %v", err)
		}
	})

	t.Run("validator returns impossible outcome", func(t *testing.T) {
		plugin := &resumableLifecyclePlugin{retained: provenance}
		lifecycle, err := candidate.NewLifecycle(candidate.LifecycleConfig{
			Context: t.Context(), Plugin: plugin, Suite: "suite", Cell: bench.Reference(),
			Provenance: provenance, Origin: origin,
			RecoveryValidator: func(
				context.Context, candidate.Attempt, bench.Transcript,
			) (bench.TaskOutcome, error) {
				return bench.TaskOutcome{
					ID: "case", Completed: true, Error: "completed rows cannot fail",
				}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"}); err == nil || !strings.Contains(err.Error(), "validator produced an invalid outcome") {
			t.Fatalf("invalid validator outcome error = %v", err)
		}
	})

	for _, testCase := range []struct {
		name   string
		plugin *resumableLifecyclePlugin
		want   string
	}{
		{
			name: "forged passed bit",
			plugin: &resumableLifecyclePlugin{
				retained:      provenance,
				mutateOutcome: func(outcome *bench.TaskOutcome) { outcome.Passed = false },
			},
			want: "differs from deterministic rescore",
		},
		{
			name: "cached transcript differs from reopened evidence",
			plugin: &resumableLifecyclePlugin{
				retained: provenance, evidenceTranscript: bench.Transcript{PlaybackMS: 1},
			},
			want: "differs from reopened raw evidence",
		},
		{
			name: "reopen failure",
			plugin: &resumableLifecyclePlugin{
				retained: provenance, evidenceErr: errors.New("durable source changed"),
			},
			want: "durable source changed",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lifecycle := newLifecycle(t, testCase.plugin)
			if _, err := lifecycle.Begin("case", 1, map[string]any{"criterion": "exact"}); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("recovery error = %v, want %q", err, testCase.want)
			}
			if len(testCase.plugin.begin) != 0 {
				t.Fatal("failed recovery started a replacement external attempt")
			}
		})
	}
}
