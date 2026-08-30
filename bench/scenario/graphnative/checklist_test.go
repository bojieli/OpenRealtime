package graphnative_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	graphnative "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
)

func TestChecklistRunsCompleteElevenCaseMatrixWithIndependentEvidenceAndMedia(t *testing.T) {
	fixture := newChecklistFixture(t, graphnative.MinimumReportableRepetitions)
	var executed, verified, retained atomic.Int64
	retainedKeys := make([]graphnative.AttemptKey, 0, 11*graphnative.MinimumReportableRepetitions)
	fixture.config.Executor = func(
		_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		executed.Add(1)
		return fixture.observation(t, key, item, true, nil), nil
	}
	fixture.config.VerifyMedia = func(
		_ context.Context, key graphnative.AttemptKey, requirement graphnative.CaseRequirement,
		reference graphnative.MediaReference,
	) (graphnative.VerifiedMedia, error) {
		verified.Add(1)
		if key.CaseName != requirement.Name || reference.Handle != "attempt/"+key.TaskID {
			return graphnative.VerifiedMedia{}, errors.New("media verifier attribution drifted")
		}
		return graphnative.VerifiedMedia{
			Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
			Audio: true, Submitted: slices.Clone(reference.Submitted),
		}, nil
	}
	fixture.config.Sink = graphnative.ChecklistSink{
		Attempt: func(_ context.Context, record graphnative.AttemptRecord) error {
			retained.Add(1)
			retainedKeys = append(retainedKeys, record.Key)
			// The sink receives a clone. Mutating it cannot change the final index.
			record.Failures = append(record.Failures, "sink mutation")
			if record.Media != nil {
				record.Media.Submitted = append(record.Media.Submitted,
					graphnative.SubmittedInputReceipt{SightID: "sink.mutation"})
			}
			return nil
		},
		Finalize: func(_ context.Context, checklist graphnative.Checklist) error {
			if checklist.Executed != 11*graphnative.MinimumReportableRepetitions {
				return errors.New("sink saw an incomplete final checklist")
			}
			checklist.Cases[0].Name = "sink mutation"
			return nil
		},
	}

	checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	wantAttempts := 11 * graphnative.MinimumReportableRepetitions
	if executed.Load() != int64(wantAttempts) || verified.Load() != int64(wantAttempts) ||
		retained.Load() != int64(wantAttempts) || len(retainedKeys) != wantAttempts {
		t.Fatalf("checklist callbacks execute=%d verify=%d retain=%d keys=%d, want %d",
			executed.Load(), verified.Load(), retained.Load(), len(retainedKeys), wantAttempts)
	}
	if !checklist.FullSuite || !checklist.Complete || !checklist.Reportable || !checklist.Passed ||
		checklist.Expected != wantAttempts || checklist.Executed != wantAttempts ||
		checklist.ReportableAttempts != wantAttempts || checklist.PassedAttempts != wantAttempts ||
		checklist.FailedAttempts != 0 || checklist.InfrastructureFailures != 0 ||
		len(checklist.Cases) != 11 || len(checklist.Attempts) != wantAttempts {
		t.Fatalf("complete scenario checklist = %+v", checklist)
	}
	for index, item := range checklist.Cases {
		if item.Ordinal != index+1 || item.Expected != graphnative.MinimumReportableRepetitions ||
			item.Executed != item.Expected || item.Reportable != item.Expected ||
			item.Passed != item.Expected || item.Failed != 0 || item.InfrastructureFailed != 0 {
			t.Fatalf("case checklist %d = %+v", index, item)
		}
	}
	visualAttempts := 0
	for index, attempt := range checklist.Attempts {
		wantResult := fixture.observation(
			t, attempt.Key, scenario.Suite()[attempt.Key.CaseOrdinal-1], true, nil,
		).Result
		wantResultSHA256, err := graphnative.FingerprintResult(wantResult)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.Key != retainedKeys[index] || attempt.Behavior != graphnative.BehaviorPassed ||
			!attempt.Reportable || !attempt.Execution.Validated || attempt.Media == nil ||
			attempt.Media.Handle != "attempt/"+attempt.Key.TaskID || len(attempt.Failures) != 0 ||
			attempt.Execution.ResultSHA256 != wantResultSHA256 {
			t.Fatalf("attempt %d = %+v", index, attempt)
		}
		if attempt.Key.CaseName == "telling them what it saw" {
			visualAttempts++
			if len(attempt.Media.Submitted) != 2 {
				t.Fatalf("visual attempt submitted media = %+v", attempt.Media.Submitted)
			}
		} else if len(attempt.Media.Submitted) != 0 {
			t.Fatalf("nonvisual attempt submitted media = %+v", attempt.Media.Submitted)
		}
	}
	if visualAttempts != graphnative.MinimumReportableRepetitions {
		t.Fatalf("visual attempts = %d", visualAttempts)
	}
	if err := checklist.Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := graphnative.MarshalChecklist(checklist)
	if err != nil {
		t.Fatal(err)
	}
	second, err := graphnative.MarshalChecklist(checklist)
	if err != nil || !slices.Equal(first, second) || first[len(first)-1] != '\n' {
		t.Fatalf("checklist marshal is not deterministic: %v", err)
	}
}

func TestChecklistSeparatesBehaviorFailureFromInfrastructureAndContinues(t *testing.T) {
	fixture := newChecklistFixture(t, 1)
	fixture.config.Executor = func(
		_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		switch key.CaseOrdinal {
		case 1:
			return fixture.observation(t, key, item, false,
				[]string{"the behavior check failed"}), nil
		case 2:
			observation := fixture.observation(t, key, item, false, []string{"not scored"})
			return observation, errors.New("provider unavailable secret-value")
		case 3:
			observation := fixture.observation(t, key, item, true, nil)
			evidence := checklistEvidence(t, fixture.requirement, "another-task")
			observation.Result.Transcript.Execution = &evidence
			return observation, nil
		case 4:
			observation := fixture.observation(t, key, item, true, nil)
			observation.Media = nil
			return observation, nil
		default:
			return fixture.observation(t, key, item, true, nil), nil
		}
	}
	fixture.config.SanitizeError = func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[REDACTED]")
	}

	checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if !checklist.Complete || checklist.Reportable || checklist.Passed ||
		checklist.Executed != 11 || checklist.ReportableAttempts != 8 ||
		checklist.PassedAttempts != 9 || checklist.FailedAttempts != 1 ||
		checklist.InfrastructureFailures != 3 {
		t.Fatalf("mixed checklist summary = %+v", checklist)
	}
	behavior := checklist.Attempts[0]
	if behavior.Behavior != graphnative.BehaviorFailed || !behavior.Reportable ||
		len(behavior.Failures) != 1 || len(behavior.Infrastructure) != 0 {
		t.Fatalf("behavior failure became infrastructure = %+v", behavior)
	}
	runFailure := checklist.Attempts[1]
	if runFailure.Behavior != graphnative.BehaviorUnscored || runFailure.Reportable ||
		!attemptHasFailure(runFailure, "execution.run") ||
		!strings.Contains(runFailure.Infrastructure[0].Detail, "[REDACTED]") ||
		strings.Contains(runFailure.Infrastructure[0].Detail, "secret-value") {
		t.Fatalf("run failure record = %+v", runFailure)
	}
	if attempt := checklist.Attempts[2]; attempt.Behavior != graphnative.BehaviorPassed ||
		attempt.Reportable || !attemptHasFailure(attempt, "evidence.scope") {
		t.Fatalf("cross-task evidence record = %+v", attempt)
	}
	if attempt := checklist.Attempts[3]; attempt.Behavior != graphnative.BehaviorPassed ||
		attempt.Reportable || !attemptHasFailure(attempt, "media.missing") {
		t.Fatalf("missing-media record = %+v", attempt)
	}
	if err := checklist.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestChecklistRejectsConfigurationDriftBeforeAnyPluginRuns(t *testing.T) {
	baseline := newChecklistFixture(t, 1)
	var calls atomic.Int64
	baseline.config.Executor = func(
		context.Context, graphnative.AttemptKey, scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		calls.Add(1)
		return graphnative.AttemptObservation{}, nil
	}
	baseline.config.VerifyMedia = func(
		context.Context, graphnative.AttemptKey, graphnative.CaseRequirement,
		graphnative.MediaReference,
	) (graphnative.VerifiedMedia, error) {
		calls.Add(1)
		return graphnative.VerifiedMedia{}, nil
	}

	tests := []struct {
		name   string
		mutate func(*graphnative.ChecklistConfig)
		want   string
	}{
		{name: "missing executor", mutate: func(config *graphnative.ChecklistConfig) {
			config.Executor = nil
		}, want: "executor"},
		{name: "zero repetitions", mutate: func(config *graphnative.ChecklistConfig) {
			config.Repetitions = 0
		}, want: "repetitions"},
		{name: "too many repetitions", mutate: func(config *graphnative.ChecklistConfig) {
			config.Repetitions = 1_001
		}, want: "repetitions"},
		{name: "missing media verifier", mutate: func(config *graphnative.ChecklistConfig) {
			config.VerifyMedia = nil
		}, want: "verifier"},
		{name: "half sink", mutate: func(config *graphnative.ChecklistConfig) {
			config.Sink.Attempt = func(context.Context, graphnative.AttemptRecord) error { return nil }
		}, want: "both"},
		{name: "contract fingerprint", mutate: func(config *graphnative.ChecklistConfig) {
			config.Contract.Fingerprint = checklistDigest("drifted-contract")
		}, want: "contract"},
		{name: "profile fingerprint", mutate: func(config *graphnative.ChecklistConfig) {
			config.Profile.Fingerprint = checklistDigest("drifted-profile")
		}, want: "launch profile"},
		{name: "adapter fingerprint", mutate: func(config *graphnative.ChecklistConfig) {
			config.AdapterProfileFingerprint = "latest"
		}, want: "adapter profile"},
		{name: "removed execution kind", mutate: func(config *graphnative.ChecklistConfig) {
			config.ExecutionRequirement = bench.ExecutionRequirement{
				FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionKind("legacy"),
			}
		}, want: "unknown execution requirement kind"},
		{name: "requirement graph drift", mutate: func(config *graphnative.ChecklistConfig) {
			copy := *config.ExecutionRequirement.Graph
			copy.Graph.Fingerprint = checklistDigest("other-graph")
			config.ExecutionRequirement.Graph = &copy
		}, want: "frozen plan graph"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := baseline.config
			test.mutate(&config)
			_, err := graphnative.RunChecklist(context.Background(), config)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("checklist configuration error = %v, want %q", err, test.want)
			}
			if calls.Load() != 0 {
				t.Fatalf("rejected checklist invoked a plugin %d times", calls.Load())
			}
		})
	}
	if _, err := graphnative.RunChecklist(nil, baseline.config); err == nil ||
		!strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil checklist context error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("nil-context checklist invoked a plugin %d times", calls.Load())
	}
}

func TestValidateChecklistConfigIsResourceFree(t *testing.T) {
	fixture := newChecklistFixture(t, 15)
	var executorCalls, verifierCalls, attemptSinkCalls, finalSinkCalls int
	fixture.config.Executor = func(
		context.Context, graphnative.AttemptKey, scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		executorCalls++
		return graphnative.AttemptObservation{}, errors.New("must not execute")
	}
	fixture.config.VerifyMedia = func(
		context.Context, graphnative.AttemptKey, graphnative.CaseRequirement,
		graphnative.MediaReference,
	) (graphnative.VerifiedMedia, error) {
		verifierCalls++
		return graphnative.VerifiedMedia{}, errors.New("must not verify")
	}
	fixture.config.Sink = graphnative.ChecklistSink{
		Attempt: func(context.Context, graphnative.AttemptRecord) error {
			attemptSinkCalls++
			return errors.New("must not retain")
		},
		Finalize: func(context.Context, graphnative.Checklist) error {
			finalSinkCalls++
			return errors.New("must not finalize")
		},
	}
	if err := graphnative.ValidateChecklistConfig(fixture.config); err != nil {
		t.Fatal(err)
	}
	if executorCalls != 0 || verifierCalls != 0 || attemptSinkCalls != 0 || finalSinkCalls != 0 {
		t.Fatalf("validation opened plug-ins: executor=%d verifier=%d attempt=%d final=%d",
			executorCalls, verifierCalls, attemptSinkCalls, finalSinkCalls)
	}

	graph := *fixture.config.ExecutionRequirement.Graph
	graph.Graph.Fingerprint = checklistDigest("drifted-graph")
	fixture.config.ExecutionRequirement.Graph = &graph
	if err := graphnative.ValidateChecklistConfig(fixture.config); err == nil ||
		!strings.Contains(err.Error(), "frozen plan graph") {
		t.Fatalf("drifted selection validation error = %v", err)
	}
	if executorCalls != 0 || verifierCalls != 0 || attemptSinkCalls != 0 || finalSinkCalls != 0 {
		t.Fatal("drifted validation opened a plug-in")
	}
}

func TestChecklistCancellationAndSinkFailureReturnFingerprintValidPartialRecords(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		fixture := newChecklistFixture(t, 1)
		ctx, cancel := context.WithCancelCause(context.Background())
		want := errors.New("stop after two attributable cases")
		calls, finalized := 0, 0
		fixture.config.Executor = func(
			_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
		) (graphnative.AttemptObservation, error) {
			calls++
			if calls == 2 {
				cancel(want)
			}
			return fixture.observation(t, key, item, true, nil), nil
		}
		fixture.config.Sink = graphnative.ChecklistSink{
			Attempt: func(context.Context, graphnative.AttemptRecord) error { return nil },
			Finalize: func(_ context.Context, checklist graphnative.Checklist) error {
				finalized++
				return checklist.Validate()
			},
		}
		checklist, err := graphnative.RunChecklist(ctx, fixture.config)
		if !errors.Is(err, want) || calls != 2 || finalized != 1 || checklist.Complete ||
			checklist.Executed != 2 || checklist.Fingerprint == "" {
			t.Fatalf("canceled checklist = %+v, err=%v calls=%d finalized=%d",
				checklist, err, calls, finalized)
		}
		if err := checklist.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("attempt sink", func(t *testing.T) {
		fixture := newChecklistFixture(t, 1)
		fixture.config.Sink = graphnative.ChecklistSink{
			Attempt: func(context.Context, graphnative.AttemptRecord) error {
				return errors.New("retention unavailable")
			},
			Finalize: func(context.Context, graphnative.Checklist) error {
				t.Fatal("finalize ran after attempt retention failed")
				return nil
			},
		}
		checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
		if err == nil || !strings.Contains(err.Error(), "retention unavailable") ||
			checklist.Executed != 1 || checklist.Fingerprint == "" {
			t.Fatalf("sink failure checklist = %+v, err=%v", checklist, err)
		}
		if err := checklist.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestChecklistRejectsReusedMediaAndTampering(t *testing.T) {
	fixture := newChecklistFixture(t, 1)
	fixture.config.Executor = func(
		_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		observation := fixture.observation(t, key, item, true, nil)
		if key.CaseOrdinal <= 2 {
			observation.Media.Handle = "attempt/reused"
		}
		if key.CaseOrdinal == 4 {
			observation.Media.ManifestSHA256 = checklistDigest("media/shared")
		}
		if key.CaseOrdinal == 5 {
			observation.Media.ManifestSHA256 = checklistDigest("media/shared")
		}
		return observation, nil
	}
	checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	// The first remains independently valid; only the reuse is refused.
	if !checklist.Attempts[0].Reportable || checklist.Attempts[1].Reportable ||
		checklist.Attempts[1].Media != nil ||
		!attemptHasFailure(checklist.Attempts[1], "media.reused") {
		t.Fatalf("reused media attempts = %+v / %+v", checklist.Attempts[0], checklist.Attempts[1])
	}
	if !checklist.Attempts[3].Reportable || checklist.Attempts[4].Reportable ||
		checklist.Attempts[4].Media != nil ||
		!attemptHasFailure(checklist.Attempts[4], "media.manifest_reused") {
		t.Fatalf("reused manifest attempts = %+v / %+v", checklist.Attempts[3], checklist.Attempts[4])
	}

	clone := checklist.Clone()
	clone.Attempts[0].Execution.GraphFingerprint = checklistDigest("forged-graph")
	if err := clone.Validate(); err == nil || !strings.Contains(err.Error(), "attribution") {
		t.Fatalf("tampered graph attribution error = %v", err)
	}
	clone = checklist.Clone()
	clone.Attempts[0].Media.Submitted = append(clone.Attempts[0].Media.Submitted,
		graphnative.SubmittedInputReceipt{SightID: "forged"})
	if reflect.DeepEqual(clone.Attempts[0].Media.Submitted, checklist.Attempts[0].Media.Submitted) {
		t.Fatal("checklist clone retained media aliases")
	}
	if err := clone.Validate(); err == nil {
		t.Fatal("tampered media receipt passed checklist validation")
	}
}

func TestChecklistDoesNotExposeAuthoredOrAttestedStateToPlugins(t *testing.T) {
	fixture := newChecklistFixture(t, 1)
	fixture.config.Executor = func(
		_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		canonical := scenario.Suite()[key.CaseOrdinal-1]
		item.Name = "mutated"
		if len(item.Script) > 0 {
			item.Script[0].Text = "mutated"
		}
		if len(item.Tools) > 0 {
			item.Tools[0].Name = "mutated"
			if len(item.Tools[0].Parameters) > 0 {
				item.Tools[0].Parameters[0] = "mutated"
			}
		}
		if len(item.Sees) > 0 {
			item.Sees[0].AtMS++
		}
		if len(item.Checks) > 0 {
			item.Checks[0].Note = "mutated"
			if len(item.Checks[0].Any) > 0 {
				item.Checks[0].Any[0] = "mutated"
			}
		}
		return fixture.observation(t, key, canonical, true, nil), nil
	}
	fixture.config.VerifyMedia = func(
		_ context.Context, key graphnative.AttemptKey, requirement graphnative.CaseRequirement,
		reference graphnative.MediaReference,
	) (graphnative.VerifiedMedia, error) {
		want := fixture.config.Contract.Cases[key.CaseOrdinal-1]
		if !reflect.DeepEqual(requirement, want) {
			return graphnative.VerifiedMedia{}, errors.New("verifier received mutated case authority")
		}
		verified := graphnative.VerifiedMedia{
			Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
			Audio: true, Submitted: slices.Clone(reference.Submitted),
		}
		requirement.Seams[0] = "mutated"
		requirement.Operations[0] = "mutated"
		if len(reference.Submitted) > 0 {
			reference.Submitted[0].CueMS++
		}
		return verified, nil
	}

	checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if checklist.Executed != 11 || checklist.ReportableAttempts != 11 ||
		checklist.InfrastructureFailures != 0 {
		t.Fatalf("plugin mutation changed checklist authority: %+v", checklist)
	}
	if !reflect.DeepEqual(fixture.config.Contract.Cases, newChecklistFixture(t, 1).config.Contract.Cases) {
		t.Fatal("plugin mutation escaped into the caller's contract")
	}
}

func TestChecklistMediaVerifierFailsClosedWithoutChangingBehaviorOrEvidence(t *testing.T) {
	tests := []struct {
		name   string
		verify func(graphnative.MediaReference) (graphnative.VerifiedMedia, error)
		code   string
	}{
		{
			name: "verifier error", code: "media.verification",
			verify: func(graphnative.MediaReference) (graphnative.VerifiedMedia, error) {
				return graphnative.VerifiedMedia{}, errors.New("bundle signature is invalid")
			},
		},
		{
			name: "wrong handle", code: "media.receipt",
			verify: func(reference graphnative.MediaReference) (graphnative.VerifiedMedia, error) {
				return graphnative.VerifiedMedia{
					Handle: "attempt/other", ManifestSHA256: reference.ManifestSHA256,
					Audio: true, Submitted: slices.Clone(reference.Submitted),
				}, nil
			},
		},
		{
			name: "wrong manifest", code: "media.receipt",
			verify: func(reference graphnative.MediaReference) (graphnative.VerifiedMedia, error) {
				return graphnative.VerifiedMedia{
					Handle: reference.Handle, ManifestSHA256: checklistDigest("other-manifest"),
					Audio: true, Submitted: slices.Clone(reference.Submitted),
				}, nil
			},
		},
		{
			name: "missing audio", code: "media.receipt",
			verify: func(reference graphnative.MediaReference) (graphnative.VerifiedMedia, error) {
				return graphnative.VerifiedMedia{
					Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
					Submitted: slices.Clone(reference.Submitted),
				}, nil
			},
		},
		{
			name: "unordered video sources", code: "media.receipt",
			verify: func(reference graphnative.MediaReference) (graphnative.VerifiedMedia, error) {
				return graphnative.VerifiedMedia{
					Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
					Audio: true, VideoSources: []string{"window", "camera"},
					Submitted: slices.Clone(reference.Submitted),
				}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newChecklistFixture(t, 1)
			calls := 0
			fixture.config.VerifyMedia = func(
				_ context.Context, _ graphnative.AttemptKey, _ graphnative.CaseRequirement,
				reference graphnative.MediaReference,
			) (graphnative.VerifiedMedia, error) {
				calls++
				if calls == 1 {
					return test.verify(reference)
				}
				return graphnative.VerifiedMedia{
					Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
					Audio: true, Submitted: slices.Clone(reference.Submitted),
				}, nil
			}
			checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
			if err != nil {
				t.Fatal(err)
			}
			first := checklist.Attempts[0]
			if first.Behavior != graphnative.BehaviorPassed || !first.Execution.Validated ||
				first.Reportable || first.Media != nil || !attemptHasFailure(first, test.code) ||
				checklist.ReportableAttempts != 10 || calls != 11 {
				t.Fatalf("failed-closed media attempt = %+v; checklist=%+v calls=%d",
					first, checklist, calls)
			}
		})
	}
}

func TestChecklistUnattestedDiagnosticModeCannotBecomeReportable(t *testing.T) {
	fixture := newChecklistFixture(t, 1)
	fixture.config.RequireMedia = false
	fixture.config.VerifyMedia = nil
	checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if checklist.MediaPolicy != graphnative.MediaPolicyUnattested || checklist.Reportable ||
		checklist.Passed || checklist.ReportableAttempts != 0 || checklist.PassedAttempts != 11 {
		t.Fatalf("unattested diagnostic checklist = %+v", checklist)
	}
	for _, attempt := range checklist.Attempts {
		if attempt.Media != nil || attempt.Reportable || attempt.Behavior != graphnative.BehaviorPassed ||
			!attempt.Execution.Validated {
			t.Fatalf("unattested attempt = %+v", attempt)
		}
	}
}

func TestChecklistRetainsUnencodableScorerOutputAsTypedInfrastructureFailure(t *testing.T) {
	fixture := newChecklistFixture(t, 1)
	fixture.config.Executor = func(
		_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		observation := fixture.observation(t, key, item, true, nil)
		if key.CaseOrdinal == 1 {
			observation.Result.Transcript.PlaybackMS = math.NaN()
		}
		return observation, nil
	}
	checklist, err := graphnative.RunChecklist(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	first := checklist.Attempts[0]
	if first.Behavior != graphnative.BehaviorUnscored || first.Reportable ||
		first.Execution.ResultSHA256 != "" || !first.Execution.Validated ||
		!attemptHasFailure(first, "result.encoding") || checklist.ReportableAttempts != 10 {
		t.Fatalf("unencodable scorer result = %+v; checklist=%+v", first, checklist)
	}
	if err := checklist.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := graphnative.FingerprintResult(fixture.observation(
		t, first.Key, scenario.Suite()[0], true, nil,
	).Result); err != nil {
		t.Fatalf("ordinary scorer result is not fingerprintable: %v", err)
	}
}

type checklistFixture struct {
	config      graphnative.ChecklistConfig
	requirement bench.ExecutionRequirement
	profileName string
	adapterFP   string
}

func newChecklistFixture(t testing.TB, repetitions int) checklistFixture {
	t.Helper()
	profiled := newProfiledScenarioFixture(t, nil)
	launch, err := profiled.application.Factory(context.Background(), profiled.configuration)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := graphlaunch.New(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	var selected graphlaunch.AdapterPlugin
	for _, plugin := range launch.Catalog.Adapters {
		if plugin.Reference == launch.Adapter.Reference {
			selected = plugin
			break
		}
	}
	if selected.Bind == nil {
		t.Fatal("scenario checklist fixture has no selected adapter binder")
	}
	bound, err := selected.Bind(context.Background(), preview.Plan)
	if err != nil {
		t.Fatal(err)
	}
	requirement := checklistRequirement(t, profiled.profile)
	fixture := checklistFixture{
		requirement: requirement, profileName: profiled.profile.Adapter.ProfileName,
		adapterFP: bound.Profile.Fingerprint,
	}
	fixture.config = graphnative.ChecklistConfig{
		Contract: profiled.contract, Profile: profiled.profile,
		AdapterProfileFingerprint: bound.Profile.Fingerprint,
		ExecutionRequirement:      requirement, Repetitions: repetitions, RequireMedia: true,
		Executor: func(
			_ context.Context, key graphnative.AttemptKey, item scenario.Scenario,
		) (graphnative.AttemptObservation, error) {
			return fixture.observation(t, key, item, true, nil), nil
		},
		VerifyMedia: func(
			_ context.Context, _ graphnative.AttemptKey, _ graphnative.CaseRequirement,
			reference graphnative.MediaReference,
		) (graphnative.VerifiedMedia, error) {
			return graphnative.VerifiedMedia{
				Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
				Audio: true, Submitted: slices.Clone(reference.Submitted),
			}, nil
		},
	}
	return fixture
}

func (fixture checklistFixture) observation(
	t testing.TB,
	key graphnative.AttemptKey,
	item scenario.Scenario,
	passed bool,
	failures []string,
) graphnative.AttemptObservation {
	t.Helper()
	evidence := checklistEvidence(t, fixture.requirement, key.TaskID)
	status := binding.Status{
		Graph: binding.ArchitectureIdentity{
			ID:          fixture.requirement.Graph.Graph.ID,
			Revision:    int(fixture.requirement.Graph.Graph.Revision),
			Fingerprint: fixture.requirement.Graph.Graph.Fingerprint,
		},
		Binding: fixture.profileName, Profile: fixture.adapterFP,
	}
	submitted := make([]graphnative.SubmittedInputReceipt, len(item.Sees))
	for index, sight := range item.Sees {
		sightID := strconv.Itoa(index + 1)
		submitted[index] = graphnative.SubmittedInputReceipt{
			SightID: "scenario.sight." + sightID, CueMS: sight.AtMS,
			SHA256:    checklistDigest(key.TaskID + "/sight/" + sightID),
			SizeBytes: int64(100 + index), MediaType: "image/png",
		}
	}
	return graphnative.AttemptObservation{
		Result: scenario.Result{
			Scenario: key.CaseName, Passed: passed, Failures: slices.Clone(failures),
			Transcript: bench.Transcript{Runtime: &status, Execution: &evidence},
		},
		Media: &graphnative.MediaReference{
			Handle: "attempt/" + key.TaskID, ManifestSHA256: checklistDigest("media/" + key.TaskID),
			Submitted: submitted,
		},
	}
}

func checklistRequirement(
	t testing.TB, profile launchprofile.Document,
) bench.ExecutionRequirement {
	t.Helper()
	graph := bench.GraphEvidence{
		Graph: bench.GraphIdentity{
			FormatVersion: ir.FormatVersion,
			ID:            profile.Plan.GraphID,
			Revision:      profile.Plan.GraphRevision,
			Fingerprint:   profile.Plan.GraphFingerprint,
		},
		Configuration: bench.ArtifactIdentity{
			ID: "config://test/scenario-checklist", Revision: "1",
			Digest: checklistDigest("scenario-checklist-configuration"),
		},
		Nodes: []bench.GraphNodeEvidence{{
			Node: "scenario",
			Element: element.Identity{
				Name: "test.ScenarioChecklist", Revision: 1,
				Digest: checklistDigest("scenario-checklist-element"),
			},
			Implementation: "go://test/scenario-checklist@1",
			Config: bench.ArtifactIdentity{
				ID: "config://test/scenario-checklist/node", Revision: "1",
				Digest: checklistDigest("scenario-checklist-node-configuration"),
			},
			Runtime: bench.ArtifactIdentity{
				ID: "runtime://test/scenario-checklist/node", Revision: "1",
				Digest: checklistDigest("scenario-checklist-node-runtime"),
			},
		}},
	}
	requirement := bench.ExecutionRequirement{
		FormatVersion: bench.AttestationFormatVersion,
		Kind:          bench.ExecutionGraphNative,
		Graph:         &graph,
	}
	payload, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	requirement, err = bench.ParseExecutionRequirement(payload)
	if err != nil {
		t.Fatal(err)
	}
	return requirement
}

func checklistEvidence(
	t testing.TB, requirement bench.ExecutionRequirement, scope string,
) bench.ExecutionEvidence {
	t.Helper()
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: scope, Graph: requirement.Graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func attemptHasFailure(attempt graphnative.AttemptRecord, code string) bool {
	for _, failure := range attempt.Infrastructure {
		if failure.Code == code {
			return true
		}
	}
	return false
}

func checklistDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
