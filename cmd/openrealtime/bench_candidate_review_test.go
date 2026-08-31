package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/campaign"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
)

func candidateReviewFixtureConfig(prefix string) candidateReviewCLIConfig {
	return candidateReviewCLIConfig{
		Suite: "fixture-suite", Prefix: prefix, Provider: gemini.RegistrationName,
		APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 16,
	}
}

func candidateReviewFixtureLookup(name string) (string, bool) {
	if name != "TEST_GEMINI_KEY" {
		return "", false
	}
	return "gemini-review-key-fixture-long-enough", true
}

func TestOpenCandidateReviewCLIAutomaticallyRetainsEvidenceWithoutPrefix(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "automatic-candidate")
	preflighted := false
	resources, err := openCandidateReviewCLIWithOperations(
		t.Context(), candidateReviewCLIConfig{
			Suite:    "fixture-suite",
			Provider: gemini.RegistrationName, APIKeyEnvironment: "GEMINI_API_KEY",
			Concurrency: 16,
		}, "", "ws://127.0.0.1:8765/v1/realtime", func(name string) (string, bool) {
			return "gemini-review-key-fixture-long-enough", name == "GEMINI_API_KEY"
		}, candidateReviewOperations{
			automaticPath: func(suite string) (string, error) {
				if suite != "fixture-suite" {
					t.Fatalf("automatic suite = %q", suite)
				}
				return prefix, nil
			},
			preflight: func(context.Context, string) error { preflighted = true; return nil },
			run: func(context.Context, candidateReviewPaths, string, []string, int) (campaign.AggregateBundle, error) {
				return campaign.AggregateBundle{}, nil
			},
			verify: func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error) {
				return campaign.AggregateBundle{}, nil
			},
		},
	)
	if err != nil || resources == nil || resources.paths.Prefix != prefix || !preflighted {
		t.Fatalf("automatic candidate review resources=%+v error=%v preflight=%v", resources, err, preflighted)
	}
	if err := resources.bundle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCandidateReviewCLIPreflightsBeforeCreatingCurrentRunBundle(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "fdb-candidate-001")
	preflightCalls := 0
	operations := candidateReviewOperations{
		preflight: func(_ context.Context, key string) error {
			preflightCalls++
			if key != "gemini-review-key-fixture-long-enough" {
				t.Fatal("candidate review preflight received the wrong credential")
			}
			return nil
		},
		run: func(
			context.Context, candidateReviewPaths, string, []string, int,
		) (campaign.AggregateBundle, error) {
			return campaign.AggregateBundle{}, errors.New("not used")
		},
		verify: func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error) {
			return campaign.AggregateBundle{}, errors.New("not used")
		},
	}
	resources, err := openCandidateReviewCLIWithOperations(
		t.Context(), candidateReviewFixtureConfig(prefix), "deployment-token-fixture",
		"ws://127.0.0.1:8765/v1/realtime?ephemeral=not-retained",
		candidateReviewFixtureLookup, operations,
	)
	if err != nil {
		t.Fatal(err)
	}
	if preflightCalls != 1 || resources.bundle == nil || !resources.origin.Live ||
		resources.origin.Kind != candidate.OriginProduction || resources.origin.EndpointSHA256 == "" {
		t.Fatalf("candidate review resources=%+v preflight calls=%d", resources, preflightCalls)
	}
	if !strings.HasSuffix(resources.paths.SourceDirectory, ".source") ||
		!strings.HasSuffix(resources.paths.AggregateDirectory, ".aggregate") {
		t.Fatalf("candidate review paths=%+v", resources.paths)
	}
	if _, err := os.Lstat(resources.paths.SourceDirectory); err != nil {
		t.Fatal("current-run source bundle was not created")
	}
	if _, err := os.Lstat(resources.paths.SourceReceipt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source receipt became visible before the suite seal: %v", err)
	}
	if err := resources.bundle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCandidateReviewCLIRejectsInvalidConfigurationWithoutMutation(t *testing.T) {
	parent := t.TempDir()
	tests := []struct {
		name   string
		config candidateReviewCLIConfig
		lookup func(string) (string, bool)
	}{
		{
			name: "provider",
			config: candidateReviewCLIConfig{
				Suite: "fixture-suite", Prefix: filepath.Join(parent, "provider"), Provider: "google.latest",
				APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 16,
			},
			lookup: candidateReviewFixtureLookup,
		},
		{
			name: "concurrency",
			config: candidateReviewCLIConfig{
				Suite: "fixture-suite", Prefix: filepath.Join(parent, "concurrency"), Provider: gemini.RegistrationName,
				APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 17,
			},
			lookup: candidateReviewFixtureLookup,
		},
		{
			name: "credential",
			config: candidateReviewCLIConfig{
				Suite: "fixture-suite", Prefix: filepath.Join(parent, "credential"), Provider: gemini.RegistrationName,
				APIKeyEnvironment: "MISSING_REVIEW_KEY", Concurrency: 16,
			},
			lookup: func(string) (string, bool) { return "", false },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			operations := candidateReviewOperations{
				preflight: func(context.Context, string) error { called = true; return nil },
				run: func(
					context.Context, candidateReviewPaths, string, []string, int,
				) (campaign.AggregateBundle, error) {
					return campaign.AggregateBundle{}, nil
				},
				verify: func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error) {
					return campaign.AggregateBundle{}, nil
				},
			}
			resources, err := openCandidateReviewCLIWithOperations(
				t.Context(), test.config, "", "ws://127.0.0.1:8765/v1/realtime",
				test.lookup, operations,
			)
			if err == nil || resources != nil || called {
				t.Fatalf("invalid config resources=%+v error=%v preflight=%v", resources, err, called)
			}
			paths, resolveErr := resolveCandidateReviewPaths(test.config.Prefix)
			if resolveErr == nil {
				for _, path := range paths.all() {
					if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("rejected config created %s: %v", path, statErr)
					}
				}
			}
		})
	}
}

func TestOpenCandidateReviewCLIMissingCredentialDoesNotReserveAutomaticPath(t *testing.T) {
	automaticCalled, preflightCalled := false, false
	resources, err := openCandidateReviewCLIWithOperations(
		t.Context(), candidateReviewCLIConfig{
			Suite: "fdb-v1-5", Provider: gemini.RegistrationName,
			APIKeyEnvironment: "MISSING_REVIEW_KEY", Concurrency: 16,
		}, "", "ws://127.0.0.1:8765/v1/realtime", func(string) (string, bool) {
			return "", false
		}, candidateReviewOperations{
			automaticPath: func(string) (string, error) {
				automaticCalled = true
				return filepath.Join(t.TempDir(), "must-not-be-used"), nil
			},
			preflight: func(context.Context, string) error { preflightCalled = true; return nil },
			run: func(context.Context, candidateReviewPaths, string, []string, int) (campaign.AggregateBundle, error) {
				return campaign.AggregateBundle{}, nil
			},
			verify: func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error) {
				return campaign.AggregateBundle{}, nil
			},
		},
	)
	if err == nil || resources != nil || automaticCalled || preflightCalled ||
		!strings.Contains(err.Error(), "unset") {
		t.Fatalf("missing credential resources=%+v error=%v automatic=%v preflight=%v",
			resources, err, automaticCalled, preflightCalled)
	}
}

func TestOpenCandidateReviewCLIRejectsRawAndEncodedCredentialPathsWithoutDisclosure(t *testing.T) {
	key := "gemini-review-key-fixture-long-enough"
	encoded := base64.RawURLEncoding.EncodeToString([]byte(key))
	for _, fragment := range []string{key, encoded} {
		t.Run(fragment[:8], func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "run-"+fragment)
			preflighted := false
			operations := candidateReviewOperations{
				preflight: func(context.Context, string) error { preflighted = true; return nil },
				run: func(
					context.Context, candidateReviewPaths, string, []string, int,
				) (campaign.AggregateBundle, error) {
					return campaign.AggregateBundle{}, nil
				},
				verify: func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error) {
					return campaign.AggregateBundle{}, nil
				},
			}
			resources, err := openCandidateReviewCLIWithOperations(
				t.Context(), candidateReviewFixtureConfig(prefix), "",
				"ws://127.0.0.1:8765/v1/realtime", func(name string) (string, bool) {
					return key, name == "TEST_GEMINI_KEY"
				}, operations,
			)
			if err == nil || resources != nil || preflighted ||
				!strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), fragment) {
				t.Fatalf("credential path resources=%+v error=%v preflight=%v", resources, err, preflighted)
			}
		})
	}
}

func TestCandidateReviewFinishPublishesOnlyAfterSealedSource(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "candidate")
	runCalls := 0
	operations := candidateReviewOperations{
		preflight: func(context.Context, string) error { return nil },
		run: func(
			_ context.Context, paths candidateReviewPaths, key string,
			sensitive []string, concurrency int,
		) (campaign.AggregateBundle, error) {
			runCalls++
			if key != "gemini-review-key-fixture-long-enough" || concurrency != 16 ||
				!strings.HasSuffix(paths.SourceReceipt, ".source.receipt.json") ||
				len(sensitive) != 2 {
				t.Fatalf("campaign inputs paths=%+v key=%q sensitive=%d concurrency=%d",
					paths, key, len(sensitive), concurrency)
			}
			return campaign.AggregateBundle{
				Manifest: campaign.AggregateManifest{
					Expected: 1, EvaluationCount: 1, Provider: gemini.Descriptor(),
				},
				Receipt: campaign.AggregateReceipt{ReceiptSHA256: "sha256:fixture"},
			}, nil
		},
		verify: func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error) {
			return campaign.AggregateBundle{}, nil
		},
	}
	resources, err := openCandidateReviewCLIWithOperations(
		t.Context(), candidateReviewFixtureConfig(prefix), "deployment-token-fixture",
		"ws://127.0.0.1:8765/v1/realtime", candidateReviewFixtureLookup, operations,
	)
	if err != nil {
		t.Fatal(err)
	}
	cell := bench.Reference()
	provenance := bench.Provenance{
		Revision: "fixture-revision", ExecutableSHA256: strings.Repeat("a", 64),
		StartedAt: "2026-08-31T00:00:00Z",
	}
	specification, err := candidate.NewAttempt(
		"fixture-suite", "case-1", 1, cell, provenance, resources.origin,
		map[string]string{"criterion": "exact"},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := resources.bundle.BeginAttempt(t.Context(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: []int16{100, -100, 200, -200},
	}); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: "case-1", Completed: true, Passed: true}
	if err := attempt.Complete(t.Context(), candidate.Completion{
		Attempt: specification, Outcome: outcome,
		Transcript: bench.Transcript{PlaybackMS: 10, Moments: []bench.Moment{
			{AtMS: 1, Kind: bench.MomentTranscript, Text: "hello"},
			{AtMS: 2, Kind: bench.MomentAgentText, Text: "hi"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: "fixture-suite", Cell: cell, Provenance: provenance,
		Expected: 1, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := resources.bundle.FinishSuite(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := resources.finish(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	if runCalls != 1 || !strings.Contains(output.String(), "1/1 recordings") ||
		!strings.Contains(output.String(), "gemini-3.7-flash") ||
		!strings.Contains(output.String(), "advisory") {
		t.Fatalf("campaign calls=%d output=%q", runCalls, output.String())
	}
}

func TestStandardAudioBenchmarkCommandsPreflightCandidateReview(t *testing.T) {
	missing := "OPENREALTIME_TEST_MISSING_GEMINI_KEY"
	t.Setenv(missing, "")
	tests := []struct {
		name string
		run  func([]string, *bytes.Buffer) error
		args []string
	}{
		{name: "fdb", run: func(args []string, output *bytes.Buffer) error { return runFDB(args, output) }},
		{name: "fdbench", run: func(args []string, output *bytes.Buffer) error { return runFDBench(args, output) }, args: []string{"-conditions", "clean"}},
		{name: "fdbv3", run: func(args []string, output *bytes.Buffer) error { return runFDBv3(args, output) }},
		{name: "tau-voice", run: func(args []string, output *bytes.Buffer) error { return runTauVoice(args, output) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "candidate")
			arguments := append([]string{}, test.args...)
			arguments = append(arguments,
				"-review-prefix", prefix, "-review-key-env", missing,
			)
			var output bytes.Buffer
			err := test.run(arguments, &output)
			if err == nil || !strings.Contains(err.Error(), "key environment") {
				t.Fatalf("candidate review preflight error=%v output=%q", err, output.String())
			}
			paths, resolveErr := resolveCandidateReviewPaths(prefix)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			for _, path := range paths.all() {
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed preflight created %s: %v", path, statErr)
				}
			}
		})
	}
}

func TestExactGeminiCandidateReviewerPreflightIsResourceOnly(t *testing.T) {
	if err := preflightExactCandidateReviewer(
		t.Context(), "gemini-review-key-fixture-long-enough",
	); err != nil {
		t.Fatal(err)
	}
	secret := "bad key"
	if err := preflightExactCandidateReviewer(t.Context(), secret); err == nil ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid credential preflight error=%v", err)
	}
}
