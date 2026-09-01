package sourcebundle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

type sourceFixture struct {
	directory  string
	receipt    string
	bundle     *Bundle
	cell       bench.Cell
	provenance bench.Provenance
	origin     candidate.RunOrigin
}

func TestPopulationMetadataBoundCoversFullFDBenchWithoutWideningAttempts(t *testing.T) {
	const (
		fdBenchPopulation        = 6_147
		attestedOutcomeRowBudget = 96 << 10
	)
	if maximumPopulationMetadataBytes < fdBenchPopulation*attestedOutcomeRowBudget {
		t.Fatalf("population metadata bound = %d, below full FD-Bench row budget %d",
			maximumPopulationMetadataBytes, fdBenchPopulation*attestedOutcomeRowBudget)
	}
	if maximumSourceBytesForPath(resultName) != maximumPopulationMetadataBytes ||
		maximumSourceBytesForPath(manifestName) != maximumPopulationMetadataBytes {
		t.Fatal("sealed population files do not use the full-population bound")
	}
	if maximumSourceBytesForPath("attempts/case/completion.json") != maximumSourceFileBytes ||
		maximumSourceBytesForPath("attempts/case/audio.stereo.wav") != maximumSourceFileBytes {
		t.Fatal("the full-population allowance leaked into per-attempt evidence")
	}
}

func newSourceFixture(t testing.TB, sensitive ...string) sourceFixture {
	t.Helper()
	root := t.TempDir()
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket,
		"ws://127.0.0.1:8181/v1/realtime?ephemeral=not-retained",
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture := sourceFixture{
		directory: filepath.Join(root, "candidate-source"),
		receipt:   filepath.Join(root, "candidate-source.receipt.json"),
		cell:      bench.Reference(),
		provenance: bench.Provenance{
			Revision: "candidate-revision", ExecutableSHA256: strings.Repeat("a", 64),
			StartedAt: "2026-08-30T00:00:00Z",
		},
		origin: origin,
	}
	fixture.bundle, err = New(Options{
		Directory: fixture.directory, ReceiptPath: fixture.receipt,
		SensitiveValues: sensitive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture sourceFixture) attempt(
	t testing.TB, caseID string, trial int, external bool,
) candidate.Attempt {
	t.Helper()
	constructor := candidate.NewAttempt
	if external {
		constructor = candidate.NewExternalAttempt
	}
	attempt, err := constructor(
		"source-suite", caseID, trial, fixture.cell, fixture.provenance,
		fixture.origin, map[string]any{"criterion": "exact", "case": caseID},
	)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func fixtureCapture() bench.SessionAudioCapture {
	return bench.SessionAudioCapture{
		SampleRateHz: 24_000,
		RoomPCM16:    []int16{100, -100, 200, -200, 300, -300},
		Agent: []bench.TimedAudioChunk{
			{AtMS: 0, PCM16: []int16{400, -400, 500}},
		},
	}
}

func fixtureOutcome(caseID string, passed bool) bench.TaskOutcome {
	return bench.TaskOutcome{
		ID: caseID, Completed: true, Passed: passed,
		Metrics: map[string]float64{"latency_ms": 42},
		Notes:   map[string]string{"scorer": "deterministic"},
	}
}

func fixtureTranscript() bench.Transcript {
	return bench.Transcript{
		PlaybackMS: 125,
		Moments: []bench.Moment{
			{AtMS: 10, Kind: bench.MomentTranscript, Text: "hello"},
			{AtMS: 40, Kind: bench.MomentAgentText, Text: "hi"},
			{AtMS: 80, Kind: bench.MomentResponseDone},
		},
	}
}

func fixtureResult(fixture sourceFixture, outcomes ...bench.TaskOutcome) bench.Result {
	result := bench.Result{
		Suite: "source-suite", Cell: fixture.cell, Provenance: fixture.provenance,
		Expected: len(outcomes), Tasks: outcomes,
	}
	result.Finish()
	return result
}

func beginAttempt(
	t testing.TB, fixture sourceFixture, specification candidate.Attempt,
) candidate.AttemptEvidence {
	t.Helper()
	attempt, err := fixture.bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func completeAttempt(
	t testing.TB, attempt candidate.AttemptEvidence, specification candidate.Attempt,
	outcome bench.TaskOutcome,
) {
	t.Helper()
	if err := attempt.Complete(context.Background(), candidate.Completion{
		Attempt: specification, Outcome: outcome, Transcript: fixtureTranscript(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleSealsSharedSessionAudioAndVerifiesEveryBinding(t *testing.T) {
	fixture := newSourceFixture(t)
	specification := fixture.attempt(t, "ordinary-question", 1, false)
	attempt := beginAttempt(t, fixture, specification)
	if err := attempt.CaptureAudio(fixtureCapture()); err != nil {
		t.Fatal(err)
	}
	outcome := fixtureOutcome(specification.Case, true)
	completeAttempt(t, attempt, specification, outcome)
	if err := fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcome)); err != nil {
		t.Fatal(err)
	}

	manifest, receipt, err := Verify(t.Context(), fixture.directory, fixture.receipt)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AttemptCount != 1 || manifest.AttemptCount != 1 ||
		manifest.Advisory != "pending" || !manifest.Attempts[0].EvidenceComplete ||
		manifest.Attempts[0].Media == nil || manifest.Attempts[0].Media.Role != "time_aligned_room_and_agent" {
		t.Fatalf("sealed bundle = manifest %+v receipt %+v", manifest, receipt)
	}
	review, err := os.ReadFile(filepath.Join(fixture.directory, reviewName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ordinary-question", "| pass | complete |", "[audio](attempts/", "advisory",
	} {
		if !bytes.Contains(review, []byte(want)) {
			t.Fatalf("REVIEW.md does not contain %q:\n%s", want, review)
		}
	}
}

func TestBundleOwnsExternalHarnessMediaAndTraceWithComplexCaseIdentity(t *testing.T) {
	fixture := newSourceFixture(t)
	caseID := "retail[customer:account]|policy:exchange-after-delivery"
	specification := fixture.attempt(t, caseID, 7, true)
	attempt := beginAttempt(t, fixture, specification)
	wav, _, err := reviewmedia.EncodeStereoWAV(fixtureCapture())
	if err != nil {
		t.Fatal(err)
	}
	wantWAV := bytes.Clone(wav)
	media := attempt.(candidate.CapturedMediaEvidence)
	if err := media.CaptureMedia(candidate.CapturedMedia{
		Name: "conversation.wav", Kind: "audio", Role: "external_harness_mix",
		MediaType: "audio/wav", Bytes: wav,
	}); err != nil {
		t.Fatal(err)
	}
	for index := range wav {
		wav[index] = 0
	}
	trace := []byte(`{"events":[{"kind":"user","text":"return this"}]}`)
	wantTrace := bytes.Clone(trace)
	artifacts := attempt.(candidate.CapturedArtifactEvidence)
	if err := artifacts.CaptureArtifact(candidate.CapturedArtifact{
		Name: "simulation.json", Kind: "trace", Role: "exact_upstream_simulation",
		ContentType: "application/json", Bytes: trace,
	}); err != nil {
		t.Fatal(err)
	}
	for index := range trace {
		trace[index] = 'x'
	}
	outcome := fixtureOutcome(caseID, false)
	completeAttempt(t, attempt, specification, outcome)
	if err := fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcome)); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := Verify(t.Context(), fixture.directory, fixture.receipt)
	if err != nil {
		t.Fatal(err)
	}
	entry := manifest.Attempts[0]
	retainedWAV, err := os.ReadFile(filepath.Join(fixture.directory, entry.Directory, entry.Media.Path))
	if err != nil {
		t.Fatal(err)
	}
	retainedTrace, err := os.ReadFile(filepath.Join(fixture.directory, entry.Directory, entry.Artifacts[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retainedWAV, wantWAV) || !bytes.Equal(retainedTrace, wantTrace) ||
		entry.Case != caseID || entry.MediaSource != candidate.MediaExternalHarness {
		t.Fatal("external source bytes or identity changed after transfer")
	}
}

func TestBundleSealsAndVerifiesIncompleteNewAttemptEvidence(t *testing.T) {
	fixture := newSourceFixture(t)
	specification := fixture.attempt(t, "missing-audio", 1, false)
	attempt := beginAttempt(t, fixture, specification)
	outcome := bench.TaskOutcome{ID: specification.Case, Error: "session capture failed"}
	err := attempt.Complete(t.Context(), candidate.Completion{
		Attempt: specification, Outcome: outcome, Transcript: fixtureTranscript(),
	})
	if err == nil || !strings.Contains(err.Error(), "missing audio") {
		t.Fatalf("Complete() error = %v, want missing audio", err)
	}
	err = fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcome))
	if err == nil || !strings.Contains(err.Error(), "incomplete attempt evidence") {
		t.Fatalf("FinishSuite() error = %v, want incomplete evidence", err)
	}
	manifest, _, verifyErr := Verify(t.Context(), fixture.directory, fixture.receipt)
	if verifyErr != nil {
		t.Fatalf("Verify() refused retained failure evidence: %v", verifyErr)
	}
	entry := manifest.Attempts[0]
	if entry.EvidenceComplete || entry.Terminal != "completed" || entry.Media != nil ||
		entry.Deterministic.Error != outcome.Error {
		t.Fatalf("incomplete entry = %+v", entry)
	}
}

func TestBundleRejectsCrossSourceAndMutatedCompletionContracts(t *testing.T) {
	fixture := newSourceFixture(t)
	shared := fixture.attempt(t, "shared", 1, false)
	sharedAttempt := beginAttempt(t, fixture, shared)
	wav, _, err := reviewmedia.EncodeStereoWAV(fixtureCapture())
	if err != nil {
		t.Fatal(err)
	}
	if err := sharedAttempt.(candidate.CapturedMediaEvidence).CaptureMedia(candidate.CapturedMedia{
		Name: "wrong.wav", Kind: "audio", Role: "wrong_source", MediaType: "audio/wav", Bytes: wav,
	}); err == nil || !strings.Contains(err.Error(), "external-harness") {
		t.Fatalf("shared CaptureMedia() error = %v", err)
	}
	if err := sharedAttempt.CaptureAudio(fixtureCapture()); err != nil {
		t.Fatal(err)
	}
	mutated := shared
	mutated.Context = []byte(`{"criterion":"different"}`)
	err = sharedAttempt.Complete(t.Context(), candidate.Completion{
		Attempt: mutated, Outcome: fixtureOutcome(shared.Case, true), Transcript: fixtureTranscript(),
	})
	if err == nil || !strings.Contains(err.Error(), "preregistered") {
		t.Fatalf("mutated Complete() error = %v", err)
	}
	if abortErr := sharedAttempt.Abort(); abortErr != nil && !strings.Contains(abortErr.Error(), "external-harness") {
		t.Fatalf("Abort() error = %v", abortErr)
	}
	if closeErr := fixture.bundle.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestBundleRefusesActiveFinishAndRetainsAbortedAttempt(t *testing.T) {
	fixture := newSourceFixture(t)
	specification := fixture.attempt(t, "aborted", 1, false)
	attempt := beginAttempt(t, fixture, specification)
	result := fixtureResult(fixture)
	result.Expected = 1
	result.Finish()
	if err := fixture.bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "active attempts") {
		t.Fatalf("active FinishSuite() error = %v", err)
	}
	if err := attempt.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.bundle.FinishSuite(t.Context(), result); err == nil {
		t.Fatal("aborted population unexpectedly reported complete")
	}
	manifest, _, err := Verify(t.Context(), fixture.directory, fixture.receipt)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Attempts[0].Terminal != "aborted" || manifest.Attempts[0].EvidenceComplete {
		t.Fatalf("aborted entry = %+v", manifest.Attempts[0])
	}
}

func TestBundleCreateOnceSensitiveAndFilesystemTamperGates(t *testing.T) {
	t.Run("create once", func(t *testing.T) {
		fixture := newSourceFixture(t)
		if _, err := New(Options{Directory: fixture.directory, ReceiptPath: fixture.receipt}); err == nil {
			t.Fatal("existing candidate source directory was reused")
		}
		if err := fixture.bundle.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("sensitive attempt", func(t *testing.T) {
		fixture := newSourceFixture(t, "credential-canary")
		specification, err := candidate.NewAttempt(
			"source-suite", "sensitive", 1, fixture.cell, fixture.provenance, fixture.origin,
			map[string]any{"value": "credential-canary"},
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.bundle.BeginAttempt(t.Context(), specification); err == nil ||
			!strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("BeginAttempt() error = %v", err)
		}
		if err := fixture.bundle.Close(); err != nil {
			t.Fatal(err)
		}
	})
	for _, test := range []struct {
		name   string
		tamper func(t *testing.T, fixture sourceFixture, manifest Manifest)
	}{
		{
			name: "content",
			tamper: func(t *testing.T, fixture sourceFixture, manifest Manifest) {
				t.Helper()
				path := filepath.Join(fixture.directory, manifest.Attempts[0].Directory, manifest.Attempts[0].Media.Path)
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("not a wave"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			tamper: func(t *testing.T, fixture sourceFixture, manifest Manifest) {
				t.Helper()
				source := filepath.Join(fixture.directory, manifest.Attempts[0].Directory, manifest.Attempts[0].Media.Path)
				if err := os.Link(source, filepath.Join(filepath.Dir(fixture.directory), "outside-hardlink.wav")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			tamper: func(t *testing.T, fixture sourceFixture, _ Manifest) {
				t.Helper()
				path := filepath.Join(fixture.directory, reviewName)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("result.json", path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceFixture(t)
			specification := fixture.attempt(t, "tamper", 1, false)
			attempt := beginAttempt(t, fixture, specification)
			if err := attempt.CaptureAudio(fixtureCapture()); err != nil {
				t.Fatal(err)
			}
			outcome := fixtureOutcome(specification.Case, true)
			completeAttempt(t, attempt, specification, outcome)
			if err := fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcome)); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := Verify(t.Context(), fixture.directory, fixture.receipt)
			if err != nil {
				t.Fatal(err)
			}
			test.tamper(t, fixture, manifest)
			if _, _, err := Verify(t.Context(), fixture.directory, fixture.receipt); err == nil {
				t.Fatal("tampered candidate source bundle verified")
			}
		})
	}
}

func TestBundleCompletionFailureDoesNotPermitASecondTerminalWrite(t *testing.T) {
	fixture := newSourceFixture(t)
	specification := fixture.attempt(t, "one-terminal", 1, false)
	attempt := beginAttempt(t, fixture, specification)
	completion := candidate.Completion{
		Attempt: specification, Outcome: fixtureOutcome(specification.Case, true),
		Transcript: fixtureTranscript(),
	}
	if err := attempt.Complete(t.Context(), completion); err == nil {
		t.Fatal("completion without audio unexpectedly succeeded")
	}
	if err := attempt.Complete(t.Context(), completion); err == nil ||
		!strings.Contains(err.Error(), "terminal") {
		t.Fatalf("second Complete() error = %v", err)
	}
	if err := attempt.Abort(); err != nil {
		t.Fatalf("idempotent Abort() = %v", err)
	}
}

func TestReviewSourceStreamsOwnedProviderRequestsAndReverifiesOnClose(t *testing.T) {
	fixture := newSourceFixture(t)
	var outcomes []bench.TaskOutcome
	for trial, caseID := range []string{"case-a", "case-b", "case-c"} {
		specification := fixture.attempt(t, caseID, trial+1, false)
		attempt := beginAttempt(t, fixture, specification)
		if err := attempt.CaptureAudio(fixtureCapture()); err != nil {
			t.Fatal(err)
		}
		outcome := fixtureOutcome(caseID, trial%2 == 0)
		completeAttempt(t, attempt, specification, outcome)
		outcomes = append(outcomes, outcome)
	}
	if err := fixture.bundle.FinishSuite(t.Context(), fixtureResult(fixture, outcomes...)); err != nil {
		t.Fatal(err)
	}
	source, err := OpenReviewSource(t.Context(), fixture.directory, fixture.receipt)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Manifest()
	if err != nil || manifest.AttemptCount != 3 {
		t.Fatalf("Manifest() = %+v, %v", manifest, err)
	}
	manifest.Attempts[0].Case = "caller mutation"
	var cases []string
	for {
		request, found, err := source.Next(t.Context(), []string{"provider-secret-canary"})
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		prepared, err := review.PrepareContext(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(prepared.Media) != 1 || prepared.Media[0].SizeBytes == 0 ||
			request.Media[0].SizeBytes != 0 || request.Media[0].Validation != "" {
			t.Fatalf("provider request/prepared media = %+v / %+v", request.Media, prepared.Media)
		}
		cases = append(cases, request.Case)
		request.Context[0] = 'x'
		request.SensitiveValues[0] = "mutated"
	}
	if strings.Join(cases, ",") != "case-a,case-b,case-c" {
		t.Fatalf("streamed cases = %v", cases)
	}
	if _, found, err := source.Next(t.Context(), nil); err != nil || found {
		t.Fatalf("exhausted Next() = found %t, err %v", found, err)
	}
	if err := source.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Next(t.Context(), nil); err == nil {
		t.Fatal("closed source returned another request")
	}
}

func TestReviewSourceSkipsUnreviewableEvidenceAndCloseDetectsMutation(t *testing.T) {
	fixture := newSourceFixture(t)
	completeSpec := fixture.attempt(t, "complete", 1, false)
	complete := beginAttempt(t, fixture, completeSpec)
	if err := complete.CaptureAudio(fixtureCapture()); err != nil {
		t.Fatal(err)
	}
	completeOutcome := fixtureOutcome(completeSpec.Case, true)
	completeAttempt(t, complete, completeSpec, completeOutcome)
	incompleteSpec := fixture.attempt(t, "incomplete", 1, false)
	incomplete := beginAttempt(t, fixture, incompleteSpec)
	incompleteOutcome := bench.TaskOutcome{ID: incompleteSpec.Case, Error: "no audio"}
	if err := incomplete.Complete(t.Context(), candidate.Completion{
		Attempt: incompleteSpec, Outcome: incompleteOutcome, Transcript: fixtureTranscript(),
	}); err == nil {
		t.Fatal("incomplete attempt unexpectedly completed evidence")
	}
	if err := fixture.bundle.FinishSuite(
		t.Context(), fixtureResult(fixture, completeOutcome, incompleteOutcome),
	); err == nil {
		t.Fatal("incomplete source unexpectedly sealed without an error")
	}
	source, err := OpenReviewSource(t.Context(), fixture.directory, fixture.receipt)
	if err != nil {
		t.Fatal(err)
	}
	request, found, err := source.Next(t.Context(), nil)
	if err != nil || !found || request.Case != "complete" {
		t.Fatalf("first request = %+v, found=%t, err=%v", request, found, err)
	}
	if _, found, err := source.Next(t.Context(), nil); err != nil || found {
		t.Fatalf("unreviewable attempt was emitted: found=%t err=%v", found, err)
	}
	path := filepath.Join(fixture.directory, reviewName)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(t.Context()); err == nil {
		t.Fatal("source mutation during review was not detected")
	}
}

func BenchmarkSealAndVerifyCandidateAudioSource(b *testing.B) {
	root := b.TempDir()
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8181/v1/realtime",
	)
	if err != nil {
		b.Fatal(err)
	}
	cell := bench.Reference()
	provenance := bench.Provenance{Revision: "benchmark", StartedAt: "2026-08-30T00:00:00Z"}
	capture := fixtureCapture()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		directory := filepath.Join(root, "source-"+strings.Repeat("x", index%7)+string(rune('a'+index%26)))
		// Include the iteration in a parent-safe filename without measuring random generation.
		directory += "-" + benchmarkIndex(index)
		receipt := directory + ".receipt.json"
		bundle, err := New(Options{Directory: directory, ReceiptPath: receipt})
		if err != nil {
			b.Fatal(err)
		}
		specification, err := candidate.NewAttempt(
			"source-suite", "case", 1, cell, provenance, origin,
			map[string]any{"criterion": "exact"},
		)
		if err != nil {
			b.Fatal(err)
		}
		attempt, err := bundle.BeginAttempt(context.Background(), specification)
		if err != nil {
			b.Fatal(err)
		}
		if err := attempt.CaptureAudio(capture); err != nil {
			b.Fatal(err)
		}
		outcome := fixtureOutcome("case", true)
		if err := attempt.Complete(context.Background(), candidate.Completion{
			Attempt: specification, Outcome: outcome, Transcript: fixtureTranscript(),
		}); err != nil {
			b.Fatal(err)
		}
		result := bench.Result{
			Suite: "source-suite", Cell: cell, Provenance: provenance,
			Expected: 1, Tasks: []bench.TaskOutcome{outcome},
		}
		result.Finish()
		if err := bundle.FinishSuite(context.Background(), result); err != nil {
			b.Fatal(err)
		}
		if _, _, err := Verify(context.Background(), directory, receipt); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkIndex(value int) string {
	if value == 0 {
		return "0"
	}
	const digits = "0123456789"
	var reversed [32]byte
	position := len(reversed)
	for value > 0 {
		position--
		reversed[position] = digits[value%10]
		value /= 10
	}
	return string(reversed[position:])
}
