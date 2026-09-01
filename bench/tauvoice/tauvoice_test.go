package tauvoice_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/tauvoice"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestConditionsCoverTheAblationsTau2Accepts(t *testing.T) {
	for _, condition := range tauvoice.Conditions {
		parsed, err := tauvoice.ParseCondition(string(condition))
		if err != nil {
			t.Fatalf("%s must parse: %v", condition, err)
		}
		if parsed != condition {
			t.Fatalf("%s parsed as %s", condition, parsed)
		}
	}
	if parsed, err := tauvoice.ParseCondition(""); err != nil || parsed != tauvoice.Control {
		t.Fatalf("the empty condition must default to control, got %s (%v)", parsed, err)
	}
	// A misspelt condition would otherwise run the default and be reported as
	// whatever the operator meant, which is the quietest way to publish a
	// number about the wrong thing.
	if _, err := tauvoice.ParseCondition("realistic"); err == nil {
		t.Fatal("an unknown condition must be rejected rather than defaulted")
	}
}

func TestAttestedTauVoiceCellRequiresIndependentEvidenceBeforeRunning(t *testing.T) {
	requirement, evidence := fixtureTauGraphExecution(t)
	cell := bench.Reference()
	cell.Execution = requirement
	config := tauvoice.Config{Cell: cell}
	if err := config.Verify(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "independently captured execution evidence") {
		t.Fatalf("tau-Voice reached its expensive environment checks without evidence: %v", err)
	}

	config.ExecutionEvidence = &evidence
	if err := config.Verify(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "prepared tau2-bench checkout") {
		t.Fatalf("matching evidence was not accepted before ordinary environment validation: %v", err)
	}
}

func fixtureTauGraphExecution(t testing.TB) (bench.ExecutionRequirement, bench.ExecutionEvidence) {
	t.Helper()
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	graph := bench.GraphEvidence{
		Graph: bench.GraphIdentity{
			FormatVersion: ir.FormatVersion, ID: "tau_voice", Revision: 1, Fingerprint: digest,
		},
		Configuration: bench.ArtifactIdentity{ID: "config://tau-voice", Digest: digest},
		Nodes: []bench.GraphNodeEvidence{{
			Node: "agent",
			Element: element.Identity{
				Name: "tauvoice.FixtureAgent", Revision: 1, Digest: digest,
			},
			Implementation: "fixture.tauvoice.agent.v1",
			Config:         bench.ArtifactIdentity{ID: "config://tau-voice/agent", Digest: digest},
			Runtime:        bench.ArtifactIdentity{ID: "runtime://tau-voice/agent", Revision: "1"},
		}},
	}
	requirement := bench.ExecutionRequirement{
		FormatVersion: bench.AttestationFormatVersion,
		Kind:          bench.ExecutionGraphNative,
		Graph:         &graph,
	}
	if err := requirement.Validate(); err != nil {
		t.Fatal(err)
	}
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion,
		Kind:          bench.ExecutionGraphNative,
		Graph:         &graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	return requirement, evidence
}

// Every check in Verify is a way to produce numbers that look fine and mean
// nothing, so each one has to actually fire.
func TestVerifyRefusesAnUnpreparedEnvironment(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		config tauvoice.Config
		wants  string
	}{
		{
			name:   "no checkout",
			config: tauvoice.Config{Endpoint: "ws://127.0.0.1:8765/v1/realtime"},
			wants:  "prepare-tau-voice.sh",
		},
		{
			name: "negative seed",
			config: tauvoice.Config{
				Endpoint: "ws://127.0.0.1:8765/v1/realtime", Seed: -1,
			},
			wants: "seed cannot be negative",
		},
		{
			name: "invalid concurrency",
			config: tauvoice.Config{
				Endpoint: "ws://127.0.0.1:8765/v1/realtime", MaxConcurrency: 65,
			},
			wants: "max concurrency must be 1..64",
		},
		{
			name: "invalid workers",
			config: tauvoice.Config{
				Endpoint: "ws://127.0.0.1:8765/v1/realtime", Workers: -1,
			},
			wants: "workers must be 0..64",
		},
		{
			name:   "no endpoint",
			config: tauvoice.Config{Tau2Dir: t.TempDir()},
			wants:  "endpoint is required",
		},
		{
			name: "not a git checkout",
			config: tauvoice.Config{
				Tau2Dir: t.TempDir(), Endpoint: "ws://127.0.0.1:8765/v1/realtime",
			},
			wants: "not a Git checkout",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Verify(ctx)
			if err == nil {
				t.Fatal("expected an unprepared environment to be refused")
			}
			if !strings.Contains(err.Error(), testCase.wants) {
				t.Fatalf("the error must say what to do, got %q", err)
			}
		})
	}
}

// A run against a moved benchmark is not a result for the pinned benchmark,
// and the difference is invisible in the numbers.
func TestVerifyRefusesADriftedRevision(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := tauvoice.Config{Tau2Dir: directory, Endpoint: "ws://127.0.0.1:8765/v1/realtime"}
	err := config.Verify(context.Background())
	if err == nil {
		t.Fatal("expected a checkout that is not at the pinned revision to be refused")
	}
	// The empty .git is not a real repository, so the revision read fails
	// first; either way the run must not proceed.
	if !strings.Contains(err.Error(), "revision") && !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("the refusal must be about the revision, got %q", err)
	}
}

// A cell that ran one domain, or ten tasks, is not the declared suite. The
// report has to say so without anybody remembering to.
func TestARestrictedRunIsNeverReportable(t *testing.T) {
	result := bench.Result{
		Suite: "tau-voice", Cell: bench.Reference(), Expected: tauvoice.TaskCount,
		Provenance: bench.Capture().Complete(),
	}
	for index := 0; index < 10; index++ {
		result.Tasks = append(result.Tasks, bench.TaskOutcome{
			ID: "airline/task/trial-0", Completed: true, Passed: true,
		})
	}
	result.Finish()
	if result.Summary.Complete {
		t.Fatal("ten of 278 tasks must not be a complete cell")
	}
	if err := result.Reportable(); err == nil {
		t.Fatal("an incomplete cell must not be reportable")
	}
}

// A simulation that never reached evaluation is not a failed task. Scoring it
// zero would fold infrastructure failures into the benchmark score, which is
// how a broken endpoint becomes a published capability claim.
func TestSimulationsWithoutARewardAreIncompleteNotFailed(t *testing.T) {
	directory := t.TempDir()
	saveTo := filepath.Join(directory, "data", "simulations", "openrealtime-airline-control")
	if err := os.MkdirAll(saveTo, 0o755); err != nil {
		t.Fatal(err)
	}
	one, zero := 1.0, 0.0
	payload, err := json.Marshal(map[string]any{
		"simulation_index": []map[string]any{
			{"id": "a", "task_id": "airline_1", "trial": 0, "reward": one, "duration": 12.5},
			{"id": "b", "task_id": "airline_2", "trial": 0, "reward": zero},
			{"id": "c", "task_id": "airline_3", "trial": 0, "reward": nil,
				"termination_reason": "agent_connection_error"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saveTo, "results.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	outcomes, err := tauvoice.ReadOutcomesForTest(saveTo, "airline")
	if err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	if len(outcomes) != 3 {
		t.Fatalf("expected three rows, got %d", len(outcomes))
	}
	byID := map[string]bench.TaskOutcome{}
	for _, outcome := range outcomes {
		byID[outcome.ID] = outcome
	}
	passed := byID["airline/airline_1/trial-0"]
	if !passed.Completed || !passed.Passed {
		t.Fatalf("a reward of 1 is a pass: %+v", passed)
	}
	if passed.Metrics["simulation_duration_ms"] != 12500 {
		t.Fatalf("duration must be reported in milliseconds, got %v", passed.Metrics)
	}
	failed := byID["airline/airline_2/trial-0"]
	if !failed.Completed || failed.Passed {
		t.Fatalf("a reward of 0 is a completed failure: %+v", failed)
	}
	crashed := byID["airline/airline_3/trial-0"]
	if crashed.Completed {
		t.Fatalf("a simulation with no reward did not complete: %+v", crashed)
	}
	if !strings.Contains(crashed.Error, "agent_connection_error") {
		t.Fatalf("the error must carry why, got %q", crashed.Error)
	}
}

func TestMissingResultsAreAnErrorRatherThanAnEmptyCell(t *testing.T) {
	if _, err := tauvoice.ReadOutcomesForTest(t.TempDir(), "retail"); err == nil {
		t.Fatal("a run that wrote nothing must fail rather than report zero tasks")
	}
}

// tau2 runs optional post-processing after it has written results - a
// conversation review, a hallucination check - and any of those can fail on a
// credential that has nothing to do with the benchmark. Losing hours of
// completed simulations to a footnote at the end is the failure this pins.
func TestResultsSurviveANonZeroExitAfterTheyAreWritten(t *testing.T) {
	checkout := t.TempDir()
	saveTo := filepath.Join(checkout, "data", "simulations", "cell-airline-control")
	if err := os.MkdirAll(saveTo, 0o755); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(saveTo, "results.json")
	stub := filepath.Join(checkout, "fake-tau2")
	script := "#!/bin/sh\ncat > " + results + " <<'JSON'\n" +
		`{"simulation_index":[{"id":"a","task_id":"airline_1","trial":0,"reward":1.0}]}` +
		"\nJSON\n" +
		"echo 'AuthenticationError: API key is invalid' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var logged []string
	outcomes, err := tauvoice.RunDomainForTest(context.Background(), tauvoice.Config{
		Tau2Dir: checkout, Endpoint: "ws://127.0.0.1:8765/v1/realtime", Python: stub,
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		},
	}, "airline", "cell-airline-control")
	if err != nil {
		t.Fatalf("results that were written must be kept: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Passed {
		t.Fatalf("expected the written pass to survive, got %+v", outcomes)
	}
	// Kept, but not kept quietly: something did fail and the operator has to
	// be able to see it.
	if !strings.Contains(strings.Join(logged, "\n"), "after writing results") {
		t.Fatalf("the non-zero exit must be reported, logged: %v", logged)
	}
}

// A run that produced nothing is a failure, and the exit status is the reason.
func TestANonZeroExitWithNoResultsIsAFailure(t *testing.T) {
	checkout := t.TempDir()
	stub := filepath.Join(checkout, "fake-tau2")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := tauvoice.RunDomainForTest(context.Background(), tauvoice.Config{
		Tau2Dir: checkout, Endpoint: "ws://127.0.0.1:8765/v1/realtime", Python: stub,
	}, "airline", "cell-airline-control")
	if err == nil {
		t.Fatal("a run that wrote nothing and exited non-zero must fail")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the failure must carry what the process said, got %q", err)
	}
}

func TestTheRunnerNamesTheMeasuredLocalVoiceAndTranscriber(t *testing.T) {
	t.Setenv("PYTHONHASHSEED", "random")
	checkout := t.TempDir()
	saveTo := filepath.Join(checkout, "data", "simulations", "cell-airline-control")
	if err := os.MkdirAll(saveTo, 0o755); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(saveTo, "results.json")
	stub := filepath.Join(checkout, "fake-tau2")
	script := "#!/bin/sh\n" +
		"test \"$OPENAI_REALTIME_VOICE\" = fish-fixed || exit 41\n" +
		"test \"$OPENAI_REALTIME_TRANSCRIPTION_MODEL\" = local-asr || exit 42\n" +
		"test \"$TAU2_VOICE_USER_DECISION_MODEL\" = openai/qwen-caller || exit 43\n" +
		"case \"$TAU2_VOICE_USER_DECISION_ARGS\" in *\"http://127.0.0.1:8000/v1\"*) ;; *) exit 44;; esac\n" +
		"test \"$PYTHONHASHSEED\" = 0 || exit 45\n" +
		"case \" $* \" in *\" --auto-resume \"*) ;; *) exit 46;; esac\n" +
		"case \" $* \" in *\" --seed 417 \"*) ;; *) exit 47;; esac\n" +
		"case \" $* \" in *\" --max-concurrency 2 \"*) ;; *) exit 48;; esac\n" +
		"case \" $* \" in *\" --workers 0 \"*) ;; *) exit 49;; esac\n" +
		"printf '%s\\n' '{\"simulation_index\":[{\"id\":\"a\",\"task_id\":\"airline_1\",\"trial\":0,\"reward\":1.0}]}' > " + results + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	outcomes, err := tauvoice.RunDomainForTest(context.Background(), tauvoice.Config{
		Tau2Dir: checkout, Endpoint: "ws://127.0.0.1:8765/v1/realtime", Python: stub,
		AgentVoice: "fish-fixed", AgentTranscriptionModel: "local-asr",
		UserModel: "qwen-caller", UserModelURL: "http://127.0.0.1:8000/v1",
		Seed: 417, MaxConcurrency: 2, Workers: 0,
	}, "airline", "cell-airline-control")
	if err != nil {
		t.Fatalf("run with explicit local identities: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Passed {
		t.Fatalf("expected the stubbed result, got %+v", outcomes)
	}
}

// tau2 checkpoints voice runs so a multi-hour cell can recover after an
// interruption. Its default resume path prompts on stdin, but the runner does
// not attach one; without --auto-resume, an existing checkpoint fails with
// EOF before any missing tasks are scheduled.
func TestTheRunnerResumesWithoutAnInteractivePrompt(t *testing.T) {
	checkout := t.TempDir()
	saveTo := filepath.Join(checkout, "data", "simulations", "cell-airline-control")
	if err := os.MkdirAll(saveTo, 0o755); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(saveTo, "results.json")
	stub := filepath.Join(checkout, "fake-tau2")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --auto-resume \"*) ;; *) echo 'missing unattended resume' >&2; exit 43;; esac\n" +
		"printf '%s\\n' '{\"simulation_index\":[{\"id\":\"a\",\"task_id\":\"airline_1\",\"trial\":0,\"reward\":1.0}]}' > " + results + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	outcomes, err := tauvoice.RunDomainForTest(context.Background(), tauvoice.Config{
		Tau2Dir: checkout, Endpoint: "ws://127.0.0.1:8765/v1/realtime", Python: stub,
	}, "airline", "cell-airline-control")
	if err != nil {
		t.Fatalf("resume an existing checkpoint: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Passed {
		t.Fatalf("expected the resumed result, got %+v", outcomes)
	}
}

// A local endpoint with authentication disabled has no key, and demanding the
// operator invent one protects nothing. A remote one with no credential is a
// run that will fail 278 times, and that is worth refusing in advance.
func TestACredentialIsRequiredOnlyWhereItCouldExist(t *testing.T) {
	t.Setenv("TAU_VOICE_TEST_TOKEN", "")
	checkout := t.TempDir()
	stub := filepath.Join(checkout, "fake-tau2")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := tauvoice.Config{
		Tau2Dir: checkout, Endpoint: "wss://realtime.example.com/v1/realtime",
		Python: stub, TokenEnv: "TAU_VOICE_TEST_TOKEN",
	}
	if _, err := tauvoice.RunDomainForTest(context.Background(), remote, "airline", "cell"); err == nil {
		t.Fatal("a remote endpoint with no credential must be refused")
	}

	t.Setenv("TAU_VOICE_TEST_TOKEN", "a-real-token")
	if _, err := tauvoice.RunDomainForTest(context.Background(), remote, "airline", "cell"); err != nil &&
		strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("a configured credential must satisfy the check, got %q", err)
	}
}
