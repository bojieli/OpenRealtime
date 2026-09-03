package releasevalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	candidatesource "github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
)

func init() {
	campaignSourceVerifiers["fixture.source-receipt"] = func(
		_ context.Context, _ string, payload []byte, _ CampaignArtifactReceipt,
	) (string, error) {
		var receipt struct {
			Complete      bool   `json:"complete"`
			Format        string `json:"format"`
			ReceiptSHA256 string `json:"receipt_sha256"`
			ResultSHA256  string `json:"result_sha256"`
		}
		if err := json.Unmarshal(payload, &receipt); err != nil || !receipt.Complete ||
			receipt.Format != "fixture.source-receipt" ||
			!sha256Pattern.MatchString(receipt.ResultSHA256) {
			return "", errors.New("fixture source receipt is invalid")
		}
		return receipt.ResultSHA256, nil
	}
}

func TestCampaignClosurePublishesCreateOnlyAndReopensEveryArtifact(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.writeResult(t)
	closure := fixture.campaignClosure(t, "final", "final", fixture.result, nil)
	published, err := PublishCampaignClosure(CampaignClosurePublication{
		Closure: closure, Path: fixture.closurePath,
	})
	if err != nil {
		t.Fatalf("publish campaign closure: %v", err)
	}
	if !sha256Pattern.MatchString(published.ClosureSHA256) {
		t.Fatalf("published closure digest = %q", published.ClosureSHA256)
	}
	original, err := os.ReadFile(fixture.closurePath)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyCampaignClosure(fixture.closurePath)
	if err != nil || verified.Closure.ClosureSHA256 != published.ClosureSHA256 ||
		len(verified.Result.Tasks) != 2 || len(verified.SourceReceipts) != 1 {
		t.Fatalf("verified closure = %+v, %v", verified, err)
	}
	if _, err := PublishCampaignClosure(CampaignClosurePublication{
		Closure: closure, Path: fixture.closurePath,
	}); err == nil || !strings.Contains(err.Error(), "exist") {
		t.Fatalf("closure overwrite was not refused: %v", err)
	}
	after, err := os.ReadFile(fixture.closurePath)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("create-only closure changed: %v", err)
	}
}

func TestBehavioralAcceptanceRejectsBareOrUnsealedResult(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.writeResult(t)
	report := EvaluateBehavioralAcceptance(
		fixture.targets, digestFor('1'), fixture.candidate, digestFor('2'),
		[]BehavioralClosureInput{{ID: "fixture", Path: fixture.resultPath}},
	)
	if report.Accepted || !strings.Contains(strings.Join(report.Failures, "\n"),
		"verify final-candidate campaign closure") {
		t.Fatalf("hand-authored result passed without a closure: %+v", report)
	}

	report = EvaluateBehavioralAcceptance(
		fixture.targets, digestFor('1'), fixture.candidate, digestFor('2'), nil,
	)
	if report.Accepted || !strings.Contains(strings.Join(report.Failures, "\n"),
		"campaign closure was not supplied") {
		t.Fatalf("declarative final_full label passed without closure: %+v", report)
	}
}

func TestCampaignClosureRejectsArtifactAndTaskPopulationTampering(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, *behavioralFixture)
		want   string
	}{
		{
			name: "result bytes",
			tamper: func(t *testing.T, fixture *behavioralFixture) {
				payload, err := os.ReadFile(fixture.resultPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture.resultPath, append(payload, ' '), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "result digest differs",
		},
		{
			name: "source receipt bytes",
			tamper: func(t *testing.T, fixture *behavioralFixture) {
				if err := os.WriteFile(filepath.Join(fixture.directory, "final.source.raw.json"),
					[]byte("{\"complete\":false}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "source receipt 0 artifact digest mismatch",
		},
		{
			name: "task population digest",
			tamper: func(t *testing.T, fixture *behavioralFixture) {
				rewriteClosure(t, fixture.closurePath, func(closure *CampaignClosure) {
					closure.TaskPopulationSHA256 = digestFor('9')
				})
			},
			want: "task population digest differs",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBehavioralFixture(t)
			fixture.publishClosure(t)
			test.tamper(t, &fixture)
			if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampering error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBehavioralAcceptanceBindsFrozenCampaignInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*behavioralFixture)
		want   string
	}{
		{"candidate", func(fixture *behavioralFixture) {
			fixture.candidate.CandidateID = "another-candidate"
		}, "different frozen candidate"},
		{"executable", func(fixture *behavioralFixture) {
			fixture.candidate.ExecutableSHA256 = digestFor('9')
		}, "build or machine differs"},
		{"run specification", func(fixture *behavioralFixture) {
			fixture.candidate.Suites[0].RunSpecSHA256 = digestFor('9')
		}, "run specification differs"},
		{"inventory", func(fixture *behavioralFixture) {
			fixture.candidate.Suites[0].TaskInventorySHA256 = digestFor('9')
		}, "task inventory differs"},
		{"scorer", func(fixture *behavioralFixture) {
			fixture.candidate.Suites[0].ScorerSHA256 = digestFor('9')
		}, "scorer differs"},
		{"source receipt", func(fixture *behavioralFixture) {
			fixture.candidate.Suites[0].SourceReceipts[0].ArtifactFormat = "another.receipt"
		}, "source-receipt identity differs"},
		{"population", func(fixture *behavioralFixture) {
			fixture.targets.Suites[0].ExpectedPopulation = 3
			fixture.targets.Suites[0].Aggregate.MinimumPassed = intPointer(1)
			fixture.targets.Suites[0].Cases.Targets = []CaseTarget{
				{Case: "case-a", ExpectedAttempts: 1, MinimumPassed: 1},
				{Case: "case-b", ExpectedAttempts: 1, MinimumPassed: 0},
				{Case: "case-c", ExpectedAttempts: 1, MinimumPassed: 0},
			}
		}, "population differs from the preregistered target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBehavioralFixture(t)
			fixture.publishClosure(t)
			test.mutate(&fixture)
			report := EvaluateBehavioralAcceptance(
				fixture.targets, digestFor('1'), fixture.candidate, digestFor('2'),
				[]BehavioralClosureInput{{ID: "fixture", Path: fixture.closurePath}},
			)
			if report.Accepted || !strings.Contains(strings.Join(report.Failures, "\n"), test.want) {
				t.Fatalf("mutated frozen input passed; want %q: %+v", test.want, report)
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestBehavioralAcceptanceRequiresExactClosedRepairLineage(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.candidate.Suites[0].Lineage = []RunLineage{
		{CampaignID: "failed-full", Kind: RunFailedFull, Population: 2, ArtifactSHA256: digestFor('4')},
		{CampaignID: "focused-fix", Kind: RunFocusedDiagnostic, Population: 1, ArtifactSHA256: digestFor('5')},
		{CampaignID: "final-rerun", Kind: RunFinalFull, Population: 2},
	}
	fixture.publishClosure(t)
	fixture.candidate.Suites[0].Lineage[0].ArtifactSHA256 = digestFor('9')
	report := EvaluateBehavioralAcceptance(
		fixture.targets, digestFor('1'), fixture.candidate, digestFor('2'),
		[]BehavioralClosureInput{{ID: "fixture", Path: fixture.closurePath}},
	)
	if report.Accepted || !strings.Contains(strings.Join(report.Failures, "\n"),
		"does not match declared repair lineage") {
		t.Fatalf("declarative repair lineage passed without exact closures: %+v", report)
	}
}

func TestCampaignClosureRejectsBrokenPredecessorChain(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.candidate.Suites[0].Lineage = []RunLineage{
		{CampaignID: "failed-full", Kind: RunFailedFull, Population: 2, ArtifactSHA256: digestFor('4')},
		{CampaignID: "focused-fix", Kind: RunFocusedDiagnostic, Population: 1, ArtifactSHA256: digestFor('5')},
		{CampaignID: "final-rerun", Kind: RunFinalFull, Population: 2},
	}
	fixture.publishClosure(t)
	predecessor := filepath.Join(fixture.directory, "prior-00.closure.json")
	payload, err := os.ReadFile(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(predecessor, append(payload, ' '), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
		!strings.Contains(err.Error(), "predecessor 0 artifact digest mismatch") {
		t.Fatalf("broken predecessor chain was accepted: %v", err)
	}
}

func TestCampaignClosureRejectsPredecessorFromAnotherSuite(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.candidate.Suites[0].Lineage = []RunLineage{
		{CampaignID: "failed-full", Kind: RunFailedFull, Population: 2, ArtifactSHA256: digestFor('4')},
		{CampaignID: "focused-fix", Kind: RunFocusedDiagnostic, Population: 1, ArtifactSHA256: digestFor('5')},
		{CampaignID: "final-rerun", Kind: RunFinalFull, Population: 2},
	}
	fixture.publishClosure(t)
	predecessorPath := filepath.Join(fixture.directory, "prior-00.closure.json")
	rewriteClosure(t, predecessorPath, func(closure *CampaignClosure) {
		closure.SuiteID = "another-suite"
	})
	payload, err := os.ReadFile(predecessorPath)
	if err != nil {
		t.Fatal(err)
	}
	rewriteClosure(t, fixture.closurePath, func(closure *CampaignClosure) {
		closure.Predecessors[0].ArtifactSHA256 = digestBytes(payload)
	})
	if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
		!strings.Contains(err.Error(), "belongs to a different suite") {
		t.Fatalf("cross-suite predecessor was accepted: %v", err)
	}
}

func TestCampaignClosureRejectsRepeatedCurrentCampaignIdentity(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.candidate.Suites[0].Lineage = []RunLineage{
		{CampaignID: "failed-full", Kind: RunFailedFull, Population: 2, ArtifactSHA256: digestFor('4')},
		{CampaignID: "focused-fix", Kind: RunFocusedDiagnostic, Population: 1, ArtifactSHA256: digestFor('5')},
		{CampaignID: "final-rerun", Kind: RunFinalFull, Population: 2},
	}
	fixture.publishClosure(t)
	predecessorPath := filepath.Join(fixture.directory, "prior-00.closure.json")
	rewriteClosure(t, predecessorPath, func(closure *CampaignClosure) {
		closure.CampaignID = "final-rerun"
	})
	payload, err := os.ReadFile(predecessorPath)
	if err != nil {
		t.Fatal(err)
	}
	rewriteClosure(t, fixture.closurePath, func(closure *CampaignClosure) {
		closure.Predecessors[0].ArtifactSHA256 = digestBytes(payload)
	})
	if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
		!strings.Contains(err.Error(), "repeats a campaign identity") {
		t.Fatalf("repeated campaign identity was accepted: %v", err)
	}
}

func TestCampaignClosureRejectsFalsePortableSourceReceiptIdentity(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.publishClosure(t)
	wrapperPath := filepath.Join(fixture.directory, "final.source.receipt.json")
	payload, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatal(err)
	}
	var wrapper CampaignArtifactReceipt
	if err := decodeCampaignCanonical(payload, &wrapper, maximumCampaignManifestBytes); err != nil {
		t.Fatal(err)
	}
	wrapper.PortableReceiptSHA256 = digestFor('8')
	wrapper.ReceiptSHA256 = ""
	sealed, err := SealCampaignArtifactReceipt(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = MarshalCampaignArtifact(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapperPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteClosure(t, fixture.closurePath, func(closure *CampaignClosure) {
		closure.SourceReceipts[0].ArtifactSHA256 = digestBytes(payload)
	})
	if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
		!strings.Contains(err.Error(), "portable digest mismatch") {
		t.Fatalf("false portable source-receipt identity was accepted: %v", err)
	}
}

func TestCampaignClosureRequiresARepositoryOwnedSourceVerifier(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.publishClosure(t)
	rewriteFixtureSourceWrapper(t, &fixture, func(wrapper *CampaignArtifactReceipt) {
		wrapper.ArtifactFormat = "unregistered.source-receipt"
	})
	if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
		!strings.Contains(err.Error(), "no repository-owned verifier") {
		t.Fatalf("unregistered source verifier was accepted: %v", err)
	}
}

func TestCampaignClosureCrossBindsSourceVerifiedResult(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.publishClosure(t)
	rawPath := filepath.Join(fixture.directory, "final.source.raw.json")
	payload, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	raw["result_sha256"] = digestFor('8')
	payload, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(rawPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteFixtureSourceWrapper(t, &fixture, func(wrapper *CampaignArtifactReceipt) {
		wrapper.ArtifactSHA256 = digestBytes(payload)
	})
	if _, err := VerifyCampaignClosure(fixture.closurePath); err == nil ||
		!strings.Contains(err.Error(), "authenticates a different deterministic result") {
		t.Fatalf("source/result mismatch was accepted: %v", err)
	}
}

func TestCampaignClosureReopensRepositoryCandidateSourceReceipt(t *testing.T) {
	fixture := newBehavioralFixture(t)
	sourceDirectory := filepath.Join(fixture.directory, "repository-source")
	receiptName := "repository.source.raw.json"
	receiptPath := filepath.Join(fixture.directory, receiptName)
	bundle, err := candidatesource.New(candidatesource.Options{
		Directory: sourceDirectory, ReceiptPath: receiptPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8181/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, outcome := range fixture.result.Tasks {
		specification, err := candidate.NewAttempt(
			fixture.result.Suite, outcome.ID, index+1, fixture.result.Cell,
			fixture.result.Provenance, origin, map[string]any{"criterion": "exact"},
		)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := bundle.BeginAttempt(t.Context(), specification)
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{100, -100, 200, -200},
			Agent: []bench.TimedAudioChunk{{AtMS: 0, PCM16: []int16{300, -300, 400, -400}}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := attempt.Complete(t.Context(), candidate.Completion{
			Attempt: specification, Outcome: outcome,
			Transcript: bench.Transcript{PlaybackMS: 50, Moments: []bench.Moment{
				{AtMS: 10, Kind: bench.MomentTranscript, Text: "fixture"},
				{AtMS: 30, Kind: bench.MomentResponseDone},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bundle.FinishSuite(t.Context(), fixture.result); err != nil {
		t.Fatal(err)
	}
	rawReceipt, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var sourceReceipt candidatesource.Receipt
	if err := json.Unmarshal(rawReceipt, &sourceReceipt); err != nil {
		t.Fatal(err)
	}
	wrapper, err := SealCampaignArtifactReceipt(CampaignArtifactReceipt{
		Format: CampaignArtifactReceiptFormat, FormatVersion: CampaignArtifactReceiptVersion,
		Kind: "deterministic-source", ArtifactFormat: candidatesource.ReceiptFormat,
		ArtifactPath: receiptName, ArtifactSHA256: digestBytes(rawReceipt),
		PortableReceiptSHA256: sourceReceipt.ReceiptSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapperArtifact := fixture.writeCampaignArtifact(
		t, "repository.source.wrapper.json", wrapper,
		CampaignArtifactReceiptFormat, CampaignArtifactReceiptVersion,
	)

	fixture.writeResult(t)
	closure := fixture.campaignClosure(t, "final", "final", fixture.result, nil)
	closure.SourceReceipts = []CampaignArtifact{wrapperArtifact}
	fixture.candidate.Suites[0].SourceReceipts = []CampaignSourceRequirement{{
		Kind: "deterministic-source", ArtifactFormat: candidatesource.ReceiptFormat,
	}}
	if _, err := PublishCampaignClosure(CampaignClosurePublication{
		Closure: closure, Path: fixture.closurePath,
	}); err != nil {
		t.Fatalf("publish repository-backed campaign closure: %v", err)
	}
}

func TestCampaignClosureParsingIsStrictCanonicalAndBounded(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.publishClosure(t)
	payload, err := os.ReadFile(fixture.closurePath)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := filepath.Join(fixture.directory, "noncanonical.json")
	if err := os.WriteFile(noncanonical, append([]byte(" "), payload...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCampaignClosure(noncanonical); err == nil ||
		!strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical closure was accepted: %v", err)
	}
	unknown := filepath.Join(fixture.directory, "unknown.json")
	unknownPayload := bytes.Replace(payload, []byte(`"format_version":1`),
		[]byte(`"unknown":true,"format_version":1`), 1)
	if err := os.WriteFile(unknown, unknownPayload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCampaignClosure(unknown); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown closure field was accepted: %v", err)
	}
	oversized := filepath.Join(fixture.directory, "oversized.json")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte{'x'}, maximumCampaignClosureBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCampaignClosure(oversized); err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("oversized closure was accepted: %v", err)
	}
}

func TestCampaignRunSpecRequiresWorkingDirectoryAndExecutableButPreservesEmptyArguments(t *testing.T) {
	spec := CampaignRunSpec{
		Format: CampaignRunSpecFormat, FormatVersion: CampaignRunSpecVersion,
		SuiteID: "fixture", CampaignID: "final",
		WorkingDirectory: "repository-root",
		Arguments:        []string{"openrealtime", "bench", "fixture", "-optional", ""},
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid run specification rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CampaignRunSpec)
	}{
		{"missing working directory", func(value *CampaignRunSpec) { value.WorkingDirectory = "" }},
		{"missing argument vector", func(value *CampaignRunSpec) { value.Arguments = nil }},
		{"missing executable", func(value *CampaignRunSpec) { value.Arguments[0] = "" }},
		{"noncanonical working directory", func(value *CampaignRunSpec) { value.WorkingDirectory = " repository-root" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := spec
			candidate.Arguments = slices.Clone(spec.Arguments)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid run specification was accepted")
			}
		})
	}
}

func rewriteClosure(t *testing.T, path string, mutate func(*CampaignClosure)) {
	t.Helper()
	closure, err := LoadCampaignClosure(path)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&closure)
	closure.ClosureSHA256 = campaignClosureDigest(closure)
	payload, err := marshalCampaignCanonical(closure)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func rewriteFixtureSourceWrapper(
	t *testing.T, fixture *behavioralFixture, mutate func(*CampaignArtifactReceipt),
) {
	t.Helper()
	path := filepath.Join(fixture.directory, "final.source.receipt.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wrapper CampaignArtifactReceipt
	if err := decodeCampaignCanonical(payload, &wrapper, maximumCampaignManifestBytes); err != nil {
		t.Fatal(err)
	}
	mutate(&wrapper)
	wrapper.ReceiptSHA256 = ""
	wrapper, err = SealCampaignArtifactReceipt(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = MarshalCampaignArtifact(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteClosure(t, fixture.closurePath, func(closure *CampaignClosure) {
		closure.SourceReceipts[0].ArtifactSHA256 = digestBytes(payload)
	})
}
