package meeting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

type fixtureEvidencePlugin struct {
	begin  func(context.Context, EvidenceAttempt) (AttemptEvidence, error)
	finish func(context.Context, bench.Result) error
}

type fixtureAttemptEvidence struct {
	captureAudio func(bench.SessionAudioCapture) error
	captureVideo func(bench.SessionVideoCapture) error
	complete     func(context.Context, EvidenceCompletion) error
	abort        func() error
}

func (attempt fixtureAttemptEvidence) CaptureAudio(capture bench.SessionAudioCapture) error {
	if attempt.captureAudio == nil {
		return nil
	}
	return attempt.captureAudio(capture)
}

func (attempt fixtureAttemptEvidence) CaptureVideo(capture bench.SessionVideoCapture) error {
	if attempt.captureVideo == nil {
		return nil
	}
	return attempt.captureVideo(capture)
}

func (attempt fixtureAttemptEvidence) Complete(
	ctx context.Context, completion EvidenceCompletion,
) error {
	if attempt.complete == nil {
		return nil
	}
	return attempt.complete(ctx, completion)
}

func (attempt fixtureAttemptEvidence) Abort() error {
	if attempt.abort == nil {
		return nil
	}
	return attempt.abort()
}

func (plugin fixtureEvidencePlugin) BeginAttempt(
	ctx context.Context, attempt EvidenceAttempt,
) (AttemptEvidence, error) {
	return plugin.begin(ctx, attempt)
}

func (plugin fixtureEvidencePlugin) FinishSuite(ctx context.Context, result bench.Result) error {
	return plugin.finish(ctx, result)
}

func fixtureMeetingOrigin() EvidenceRunOrigin {
	return EvidenceRunOrigin{
		Kind: EvidenceOriginHermetic, Live: false, Transport: bench.TransportWebSocket,
		EndpointSHA256: meetingEndpointIdentity("ws://hermetic.invalid/v1/realtime"),
	}
}

func TestMeetingEvidenceAttemptPrecedesEnvironmentAndCarriesExactTreatment(t *testing.T) {
	want := errors.New("fixture evidence refusal")
	environmentErr := errors.New("fixture deterministic environment refusal")
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
	outcome, evidenceErr := runTask(context.Background(), meetingRunEnvironment{
		episode: func(context.Context, Task) (meetingRunEpisode, error) {
			return meetingRunEpisode{}, environmentErr
		},
	}, Options{
		Evidence: plugin, evidenceOrigin: fixtureMeetingOrigin(),
	}, Suite()[0], ReferenceCell(), bench.Provenance{})
	if called != 1 || outcome.Completed || outcome.Error != environmentErr.Error() ||
		!errors.Is(evidenceErr, want) || strings.Contains(outcome.Error, want.Error()) {
		t.Fatalf("runTask() outcome=%+v evidence=%v begin calls=%d", outcome, evidenceErr, called)
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

func TestMeetingRunSurfacesEnvironmentCloseFailure(t *testing.T) {
	dependencies := &runDependencies{
		newEnvironment: func(context.Context, EnvironmentConfig) (meetingRunEnvironment, error) {
			return meetingRunEnvironment{
				episode: func(context.Context, Task) (meetingRunEpisode, error) {
					return meetingRunEpisode{}, errors.New("fixture episode refusal")
				},
				close: func() error { return errors.New("private close detail") },
			}, nil
		},
		playSamples: func(context.Context, bench.SessionConfig, []int16) (bench.Transcript, error) {
			return bench.Transcript{}, errors.New("unexpected session invocation")
		},
		now: time.Now,
	}
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Limit: 1, dependencies: dependencies,
	})
	if err == nil || err.Error() != "close meeting environment" || len(result.Tasks) != 1 ||
		result.Tasks[0].Completed {
		t.Fatalf("Run() close result=%+v error=%v", result, err)
	}
}

func TestMeetingCanceledRunUsesBoundedEvidenceContextWithoutRewritingOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	deterministicErr := errors.New("deterministic episode stopped")
	evidenceErr := errors.New("diagnostic retention failed")
	completeCalled, finishCalled := false, false
	plugin := fixtureEvidencePlugin{
		begin: func(context.Context, EvidenceAttempt) (AttemptEvidence, error) {
			return fixtureAttemptEvidence{complete: func(cleanup context.Context, completion EvidenceCompletion) error {
				completeCalled = true
				if cleanup.Err() != nil {
					t.Fatalf("attempt cleanup inherited cancellation: %v", cleanup.Err())
				}
				deadline, bounded := cleanup.Deadline()
				if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > meetingAttemptEvidenceTimeout {
					t.Fatalf("attempt cleanup deadline = %v, bounded=%t", deadline, bounded)
				}
				if completion.Outcome.Error != deterministicErr.Error() {
					t.Fatalf("deterministic completion = %+v", completion.Outcome)
				}
				return evidenceErr
			}}, nil
		},
		finish: func(cleanup context.Context, result bench.Result) error {
			finishCalled = true
			if cleanup.Err() != nil {
				t.Fatalf("suite cleanup inherited cancellation: %v", cleanup.Err())
			}
			deadline, bounded := cleanup.Deadline()
			if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > meetingSuiteEvidenceTimeout {
				t.Fatalf("suite cleanup deadline = %v, bounded=%t", deadline, bounded)
			}
			return nil
		},
	}
	dependencies := &runDependencies{
		newEnvironment: func(context.Context, EnvironmentConfig) (meetingRunEnvironment, error) {
			return meetingRunEnvironment{
				episode: func(context.Context, Task) (meetingRunEpisode, error) {
					cancel()
					return meetingRunEpisode{}, deterministicErr
				},
				close: func() error { return nil },
			}, nil
		},
		playSamples: func(context.Context, bench.SessionConfig, []int16) (bench.Transcript, error) {
			return bench.Transcript{}, errors.New("unexpected session invocation")
		},
		now: time.Now,
	}
	result, err := Run(ctx, Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Limit: 1,
		Evidence: plugin, dependencies: dependencies,
	})
	var typed *EvidenceError
	if !completeCalled || !finishCalled || !errors.As(err, &typed) || !errors.Is(err, evidenceErr) ||
		len(result.Tasks) != 1 || result.Tasks[0].Error != deterministicErr.Error() ||
		strings.Contains(result.Tasks[0].Error, evidenceErr.Error()) {
		t.Fatalf("canceled evidence result=%+v error=%v complete=%t finish=%t", result, err, completeCalled, finishCalled)
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

func TestMeetingEvidenceAttemptCloneOwnsCellAndRequirementGraphs(t *testing.T) {
	requirement, _ := fixtureMeetingGraphRequirement(t)
	cell := ReferenceCell()
	cell.Execution = requirement
	source := EvidenceAttempt{
		Suite: SuiteName, Case: Suite()[0].ID, Trial: 1, Task: Suite()[0],
		Cell: cell, Origin: fixtureMeetingOrigin(), ExecutionRequirement: requirement,
	}
	cloned := cloneEvidenceAttempt(source)
	cloned.Cell.Levels[bench.FactorTransport] = bench.TransportWebRTC
	cloned.Cell.Execution.Graph.Nodes[0].Node = "changed-cell"
	cloned.ExecutionRequirement.Graph.Nodes[0].Node = "changed-requirement"
	if source.Cell.Levels[bench.FactorTransport] == bench.TransportWebRTC ||
		source.Cell.Execution.Graph.Nodes[0].Node != "foreground" ||
		source.ExecutionRequirement.Graph.Nodes[0].Node != "foreground" {
		t.Fatal("evidence attempt clone aliases mutable cell or execution requirement state")
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
