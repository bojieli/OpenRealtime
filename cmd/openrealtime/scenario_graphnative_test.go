package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

func TestScenarioGraphChecklistRetainsVerifiesAndIndexesAllElevenAttempts(t *testing.T) {
	t.Chdir("../..")
	selection, requirement, adapterFingerprint := scenarioGraphCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "review")
	const secret = "scenario-command-secret-must-not-be-retained"
	var executorBuilds, executorCalls atomic.Int32
	newExecutor := func(
		config graphnative.LiveExecutorConfig,
	) (graphnative.AttemptExecutor, error) {
		executorBuilds.Add(1)
		if config.Retain == nil {
			return nil, errors.New("retainer was not composed")
		}
		return func(
			ctx context.Context, key graphnative.AttemptKey, item scenario.Scenario,
		) (graphnative.AttemptObservation, error) {
			executorCalls.Add(1)
			result := scenarioGraphSuccessfulResult(t, requirement, selection, adapterFingerprint, key, item)
			capture := graphnative.AttemptCapture{
				Key: key, Result: result, RunSucceeded: true,
				Audio: bench.SessionAudioCapture{
					SampleRateHz: 24_000, RoomPCM16: []int16{1, 2, 3, 4},
					Agent: []bench.TimedAudioChunk{{AtMS: 0.125, PCM16: []int16{5, 6}}},
				},
				Submitted: scenarioGraphSubmittedFixture(t, item),
			}
			reference, err := config.Retain(ctx, capture)
			if err != nil {
				return graphnative.AttemptObservation{Result: result}, err
			}
			return graphnative.AttemptObservation{Result: result, Media: &reference}, nil
		}, nil
	}
	bundle, err := newScenarioGraphReviewBundle(directory, 1, requirement, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := executeScenarioGraphChecklist(
		context.Background(), selection, requirement, adapterFingerprint, 1, time.Second,
		bundle, scenario.SpeechVoice{Endpoint: "http://speech.invalid"},
		bench.SessionConfig{Endpoint: "ws://realtime.invalid", Token: secret, Timeout: time.Second},
		newExecutor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if executorBuilds.Load() != 1 || executorCalls.Load() != 11 ||
		outcome.Checklist.Executed != 11 || outcome.Checklist.ReportableAttempts != 11 ||
		outcome.Checklist.PassedAttempts != 11 || !outcome.Checklist.Complete ||
		outcome.Checklist.Reportable {
		t.Fatalf("graph checklist outcome = %+v builds=%d calls=%d",
			outcome.Checklist, executorBuilds.Load(), executorCalls.Load())
	}
	if err := outcome.Checklist.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("review bundle published before caller finalization: %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}

	payload, err := os.ReadFile(filepath.Join(directory, "checklist.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained graphnative.Checklist
	if err := json.Unmarshal(payload, &retained); err != nil {
		t.Fatal(err)
	}
	if err := retained.Validate(); err != nil {
		t.Fatal(err)
	}
	if retained.Fingerprint != outcome.Checklist.Fingerprint ||
		retained.Attempts[9].Media == nil || len(retained.Attempts[9].Media.Submitted) != 2 {
		t.Fatalf("retained checklist fingerprint=%q want=%q visual=%+v",
			retained.Fingerprint, outcome.Checklist.Fingerprint, retained.Attempts[9].Media)
	}
	var review scenario.ReviewManifest
	payload, err = os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &review); err != nil {
		t.Fatal(err)
	}
	if !review.Complete || !review.Reportable || len(review.Attempts) != 11 {
		t.Fatalf("human review manifest = %+v", review)
	}
	if markdown, err := os.ReadFile(filepath.Join(directory, "REVIEW.md")); err != nil ||
		!bytes.Contains(markdown, []byte("## 10. telling them what it saw")) {
		t.Fatalf("human review = %q, %v", markdown, err)
	}
	if markdown, err := os.ReadFile(filepath.Join(directory, "CHECKLIST.md")); err != nil ||
		!bytes.Contains(markdown, []byte("## 10. telling them what it saw")) ||
		!bytes.Contains(markdown, []byte("Audio:")) ||
		!bytes.Contains(markdown, []byte("publication requires at least 15")) {
		t.Fatalf("graph-native checklist review = %q, %v", markdown, err)
	}

	counts := map[string]int{}
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("review file %s retained the session credential", path)
		}
		switch {
		case strings.HasSuffix(path, ".stereo.wav"):
			counts["audio"]++
		case strings.HasSuffix(path, ".media.json"):
			counts["media"]++
		case strings.HasSuffix(path, ".result.json"):
			counts["result"]++
		case strings.HasSuffix(path, ".checklist.json") && filepath.Base(path) != "checklist.json":
			counts["attempt"]++
		case strings.Contains(filepath.Base(path), "submitted"):
			counts["submitted"]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts["audio"] != 11 || counts["media"] != 11 || counts["result"] != 11 ||
		counts["attempt"] != 11 ||
		counts["submitted"] != 2 {
		t.Fatalf("review evidence counts = %+v", counts)
	}

	_, err = newScenarioGraphReviewBundle(directory, 1, requirement, nil)
	if err == nil || !strings.Contains(err.Error(), "exclusively") || executorBuilds.Load() != 1 {
		t.Fatalf("create-only rerun error = %v, executor builds=%d", err, executorBuilds.Load())
	}
}

func TestScenarioGraphReviewRejectsSensitiveScorerResultBeforeAttemptWrites(t *testing.T) {
	t.Chdir("../..")
	_, requirement, _ := scenarioGraphCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "review")
	const secret = "scenario-sensitive-result-value-123456789"
	bundle, err := newScenarioGraphReviewBundle(directory, 1, requirement, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	item := scenario.Suite()[0]
	key := graphnative.AttemptKey{
		CaseOrdinal: 1, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1",
	}
	_, err = bundle.Retain(context.Background(), graphnative.AttemptCapture{
		Key: key, RunSucceeded: true,
		Result: scenario.Result{
			Scenario: item.Name, Passed: false, Failures: []string{"provider said " + secret},
		},
		Audio: bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: []int16{1, 2}},
	})
	if err == nil || !strings.Contains(err.Error(), "sensitive value") {
		t.Fatalf("sensitive result retention error = %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		payload, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(payload, []byte(secret)) ||
			strings.HasSuffix(entry.Name(), ".result.json") ||
			strings.HasSuffix(entry.Name(), ".stereo.wav") {
			t.Fatalf("sensitive attempt wrote %q", entry.Name())
		}
	}
	if err := bundle.Close(); err == nil || !strings.Contains(err.Error(), "retained 0 of 11") {
		t.Fatalf("incomplete sensitive bundle close error = %v", err)
	}
}

func TestPrepareScenarioGraphSelectionRejectsDriftBeforePluginsRun(t *testing.T) {
	selection, requirement, adapterFingerprint := scenarioGraphCommandFixture(t)
	directory := t.TempDir()
	payload, err := launchprofile.MarshalYAML(selection.Profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "scenario.launch.yaml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	cell := archbench.Cell{Architecture: archbench.Architecture{
		RuntimeBinding: selection.Profile.Adapter.ProfileName, Profile: adapterFingerprint,
	}}
	prepared, err := prepareScenarioGraphSelection(path, cell, requirement, 15)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Profile.Fingerprint != selection.Profile.Fingerprint ||
		prepared.Contract.Fingerprint != selection.Contract.Fingerprint {
		t.Fatalf("prepared graph selection = %+v", prepared)
	}

	tests := []struct {
		name        string
		path        string
		cell        archbench.Cell
		requirement bench.ExecutionRequirement
		want        string
	}{
		{name: "missing profile", cell: cell, requirement: requirement, want: "-launch-profile"},
		{name: "binding drift", path: path, cell: archbench.Cell{Architecture: archbench.Architecture{
			RuntimeBinding: "other-binding", Profile: adapterFingerprint,
		}}, requirement: requirement, want: "differs"},
		{name: "adapter fingerprint", path: path, cell: archbench.Cell{Architecture: archbench.Architecture{
			RuntimeBinding: selection.Profile.Adapter.ProfileName, Profile: "latest",
		}}, requirement: requirement, want: "adapter profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := prepareScenarioGraphSelection(
				test.path, test.cell, test.requirement, 15,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("selection error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestScenarioLaunchProfileCannotBeIgnoredByCompatibilityExecution(t *testing.T) {
	var output bytes.Buffer
	err := runScenario([]string{
		"-launch-profile", filepath.Join(t.TempDir(), "must-not-be-read.yaml"),
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "graph-native architecture cell") {
		t.Fatalf("compatibility launch-profile error = %v", err)
	}
}

func TestScenarioGraphMediaVerifierRejectsRetainedByteTampering(t *testing.T) {
	t.Chdir("../..")
	selection, requirement, adapterFingerprint := scenarioGraphCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "review")
	bundle, err := newScenarioGraphReviewBundle(directory, 1, requirement, nil)
	if err != nil {
		t.Fatal(err)
	}
	item := scenario.Suite()[0]
	key := graphnative.AttemptKey{
		CaseOrdinal: 1, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1",
	}
	result := scenarioGraphSuccessfulResult(
		t, requirement, selection, adapterFingerprint, key, item,
	)
	reference, err := bundle.Retain(context.Background(), graphnative.AttemptCapture{
		Key: key, Result: result, RunSucceeded: true,
		Audio: bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: []int16{1, 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := bundle.Verify(
		context.Background(), key, selection.Contract.Cases[0], reference,
	)
	if err != nil || !verified.Audio {
		t.Fatalf("initial media verification = %+v, %v", verified, err)
	}
	payload, err := os.ReadFile(filepath.Join(directory, reference.Handle))
	if err != nil {
		t.Fatal(err)
	}
	var manifest scenarioGraphMediaManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, manifest.Audio.Path), []byte("tampered"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Verify(
		context.Background(), key, selection.Contract.Cases[0], reference,
	); err == nil || !strings.Contains(err.Error(), "exact regular file") {
		t.Fatalf("tampered media verification error = %v", err)
	}
	if err := bundle.Close(); err == nil || !strings.Contains(err.Error(), "retained 1 of 11") {
		t.Fatalf("partial review close error = %v", err)
	}
	if err := bundle.Close(); err == nil || !strings.Contains(err.Error(), "retained 1 of 11") {
		t.Fatalf("idempotent partial review close error = %v", err)
	}
}

func scenarioGraphCommandFixture(
	t testing.TB,
) (scenarioGraphSelection, bench.ExecutionRequirement, string) {
	t.Helper()
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	graphFingerprint := scenarioGraphTestDigest("graph")
	plan := graphconfig.Identity{
		FormatVersion: graphconfig.PlanFormatVersion,
		GraphID:       "test.scenario-conversation", GraphRevision: 1,
		SourceDigest: scenarioGraphTestDigest("source"), LockDigest: scenarioGraphTestDigest("lock"),
		ValuesDigest: scenarioGraphTestDigest("values"), ChannelsDigest: scenarioGraphTestDigest("channels"),
		ValuesSchemaDigest:     scenarioGraphTestDigest("schema"),
		PublicDeploymentDigest: scenarioGraphTestDigest("deployment"),
		ResolutionDigest:       scenarioGraphTestDigest("resolution"),
		GraphFingerprint:       graphFingerprint,
	}
	plan.PlanFingerprint = scenarioGraphTestPlanFingerprint(t, plan)
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	requirement := bench.ExecutionRequirement{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Graph: &bench.GraphEvidence{
			Graph: bench.GraphIdentity{
				FormatVersion: ir.FormatVersion, ID: plan.GraphID,
				Revision: plan.GraphRevision, Fingerprint: graphFingerprint,
			},
			Configuration: bench.ArtifactIdentity{
				ID: "config://test/scenario", Revision: "1", Digest: scenarioGraphTestDigest("configuration"),
			},
			Nodes: []bench.GraphNodeEvidence{{
				Node: "scenario", Element: element.Identity{
					Name: "test.Scenario", Revision: 1, Digest: scenarioGraphTestDigest("element"),
				},
				Implementation: "go://test/scenario@1",
				Config: bench.ArtifactIdentity{
					ID: "config://test/scenario/node", Revision: "1", Digest: scenarioGraphTestDigest("node-config"),
				},
				Runtime: bench.ArtifactIdentity{
					ID: "runtime://test/scenario/node", Revision: "1", Digest: scenarioGraphTestDigest("node-runtime"),
				},
			}},
		},
	}
	requirementPayload, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	requirement, err = bench.ParseExecutionRequirement(requirementPayload)
	if err != nil {
		t.Fatal(err)
	}
	delegate := launchprofile.Registration{
		Reference: "application.test.delegate", Artifact: scenarioGraphTestArtifact("application/delegate"),
		ProviderArtifact: scenarioGraphTestArtifact("provider/delegate"),
		Factory: func(context.Context, json.RawMessage) (graphlaunch.Config, error) {
			panic("resource-free fixture delegate must not run")
		},
	}
	configuration, err := graphnative.FreezeApplicationConfiguration(
		contract, delegate, json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	applicationArtifact := scenarioGraphTestArtifact("application/scenario")
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion, Name: "test.scenario", Revision: 1,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: "application.test.scenario", Artifact: applicationArtifact,
			},
			Configuration: configuration,
		},
		Plan: plan,
		Adapter: launchprofile.AdapterSelection{
			Reference: "adapter.test.scenario", RuntimeArtifact: scenarioGraphTestArtifact("adapter/scenario"),
			ProfileName: "scenario-conversation", ProfileRevision: 1,
		},
		Server: launchprofile.Server{
			ProfileName: "test-server", ProfileRevision: 1,
			ProviderArtifact: delegate.ProviderArtifact,
			GatewayArtifact:  scenarioGraphTestArtifact("gateway/scenario"),
			Model:            "test-model", TranscriptionModel: "test-transcription",
			ValidateWire: true, InspectionTokenTTLMS: 60_000, MaxAudioFrameBytes: 1 << 20,
			VideoLimits: openrealtime.Limits{
				Format: "png", FPSCap: 1, MaxDimension: 1024, MaxFrameBytes: 1 << 20,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return scenarioGraphSelection{Contract: contract, Profile: profile},
		requirement, scenarioGraphTestDigest("adapter-profile")
}

func scenarioGraphSuccessfulResult(
	t testing.TB,
	requirement bench.ExecutionRequirement,
	selection scenarioGraphSelection,
	adapterFingerprint string,
	key graphnative.AttemptKey,
	item scenario.Scenario,
) scenario.Result {
	t.Helper()
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: key.TaskID, Graph: requirement.Graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	status := binding.Status{
		Graph: binding.ArchitectureIdentity{
			ID: selection.Profile.Plan.GraphID, Revision: int(selection.Profile.Plan.GraphRevision),
			Fingerprint: selection.Profile.Plan.GraphFingerprint,
		},
		Binding: selection.Profile.Adapter.ProfileName, Profile: adapterFingerprint,
	}
	result := scenario.Result{
		Scenario: item.Name, Passed: true,
		Transcript: bench.Transcript{Runtime: &status, Execution: &evidence},
	}
	for index := range item.Sees {
		result.Transcript.Moments = append(result.Transcript.Moments,
			bench.Moment{Kind: bench.MomentScheduled, Name: "scenario.sight." + strconv.Itoa(index+1)})
	}
	return result
}

func scenarioGraphSubmittedFixture(
	t testing.TB, item scenario.Scenario,
) []graphnative.SubmittedInputCapture {
	t.Helper()
	result := make([]graphnative.SubmittedInputCapture, len(item.Sees))
	for index, sight := range item.Sees {
		payload, err := os.ReadFile(sight.Path)
		if err != nil {
			t.Fatal(err)
		}
		result[index] = graphnative.SubmittedInputCapture{
			Receipt: graphnative.SubmittedInputReceipt{
				SightID: "scenario.sight." + strconv.Itoa(index+1), CueMS: sight.AtMS,
				SHA256: scenarioGraphDigest(payload), SizeBytes: int64(len(payload)),
				MediaType: http.DetectContentType(payload),
			},
			Data: payload,
		}
	}
	return result
}

func scenarioGraphTestArtifact(name string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "artifact://test/" + name, Revision: "1", Digest: scenarioGraphTestDigest(name),
	}
}

func scenarioGraphTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func scenarioGraphTestPlanFingerprint(t testing.TB, identity graphconfig.Identity) string {
	t.Helper()
	identity.PlanFingerprint = ""
	payload, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	hash.Write([]byte("openrealtime.config/plan/v1\x00"))
	hash.Write(payload)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}
