package meeting

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

type fixtureEvidencePlugin struct {
	begin  func(context.Context, EvidenceAttempt) (AttemptEvidence, error)
	finish func(context.Context, bench.Result) error
}

func (plugin fixtureEvidencePlugin) BeginAttempt(
	ctx context.Context, attempt EvidenceAttempt,
) (AttemptEvidence, error) {
	return plugin.begin(ctx, attempt)
}

func (plugin fixtureEvidencePlugin) FinishSuite(ctx context.Context, result bench.Result) error {
	return plugin.finish(ctx, result)
}

func TestMeetingEvidenceAttemptPrecedesEnvironmentAndCarriesExactTreatment(t *testing.T) {
	want := errors.New("fixture evidence refusal")
	called := 0
	plugin := fixtureEvidencePlugin{
		begin: func(_ context.Context, attempt EvidenceAttempt) (AttemptEvidence, error) {
			called++
			if attempt.Suite != SuiteName || attempt.Case != Suite()[0].ID || attempt.Trial != 1 ||
				attempt.Task.ID != Suite()[0].ID || attempt.ExecutionRequirement.Required() {
				t.Fatalf("evidence attempt = %+v", attempt)
			}
			return nil, want
		},
		finish: func(context.Context, bench.Result) error { return nil },
	}
	outcome := runTask(context.Background(), nil, Options{Evidence: plugin}, Suite()[0])
	if called != 1 || outcome.Completed || !strings.Contains(outcome.Error, want.Error()) {
		t.Fatalf("runTask() outcome=%+v begin calls=%d", outcome, called)
	}
}

func TestMeetingRunFinishesEvidenceBundleOnEnvironmentFailure(t *testing.T) {
	finished := 0
	plugin := fixtureEvidencePlugin{
		begin: func(context.Context, EvidenceAttempt) (AttemptEvidence, error) {
			t.Fatal("an attempt began after the environment failed")
			return nil, nil
		},
		finish: func(_ context.Context, result bench.Result) error {
			finished++
			if result.Suite != SuiteName || result.Expected != ExpectedTasks() ||
				len(result.Tasks) != 0 || result.Summary.Complete {
				t.Fatalf("finished result = %+v", result)
			}
			return nil
		},
	}
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://127.0.0.1:1/v1/realtime",
		Browser:  "/definitely/not/an/openrealtime-browser",
		Evidence: plugin,
	})
	if err == nil || finished != 1 || result.Summary.Complete {
		t.Fatalf("Run() result=%+v error=%v finish calls=%d", result, err, finished)
	}
}

func TestMeetingEvidenceCompletionClonesMutableBenchmarkState(t *testing.T) {
	execution := fixtureMeetingExecutionEvidence(t, "clone-case")
	outcome := bench.TaskOutcome{
		ID: "clone-case", Completed: true, Passed: true,
		Metrics:   map[string]float64{"latency_ms": 1},
		Notes:     map[string]string{"note": "original"},
		Execution: &execution,
	}
	transcript := bench.Transcript{
		Moments:   []bench.Moment{{Kind: bench.MomentAgentText, Text: "original"}},
		Execution: &execution,
	}
	clonedOutcome := cloneTaskOutcome(outcome)
	clonedTranscript := cloneTranscript(transcript)
	clonedOutcome.Metrics["latency_ms"] = 9
	clonedOutcome.Notes["note"] = "changed"
	clonedOutcome.Execution.Scope = "changed"
	clonedTranscript.Moments[0].Text = "changed"
	clonedTranscript.Execution.Scope = "changed"
	if outcome.Metrics["latency_ms"] != 1 || outcome.Notes["note"] != "original" ||
		outcome.Execution.Scope != "clone-case" || transcript.Moments[0].Text != "original" ||
		transcript.Execution.Scope != "clone-case" {
		t.Fatal("evidence completion aliases mutable benchmark state")
	}
}

func fixtureMeetingExecutionEvidence(t testing.TB, scope string) bench.ExecutionEvidence {
	t.Helper()
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion,
		Kind:          bench.ExecutionLegacy,
		Scope:         scope,
		Legacy: &bench.LegacyEvidence{
			Binding:       "meeting-fixture",
			RuntimeDigest: digest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}
