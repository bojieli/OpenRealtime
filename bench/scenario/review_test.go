package scenario

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestEncodeReviewStereoWAVAlignsChannelsAndSaturatesOverlaps(t *testing.T) {
	frameMS := 1000.0 / 24_000
	wav, spec, err := encodeReviewStereoWAV(bench.SessionAudioCapture{
		SampleRateHz: 24_000,
		RoomPCM16:    []int16{10, 20, 30, 40, 50},
		Agent: []bench.TimedAudioChunk{
			{AtMS: 2 * frameMS, PCM16: []int16{20_000, 20_000}},
			{AtMS: 2 * frameMS, PCM16: []int16{20_000, -30_000}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Frames != 5 || spec.Channels != 2 || spec.SampleRateHz != 24_000 ||
		spec.DurationMS != float64(5)*1000/24_000 {
		t.Fatalf("audio spec = %+v", spec)
	}
	left, right := decodeStereoReviewWAV(t, wav)
	if !slices.Equal(left, []int16{10, 20, 30, 40, 50}) {
		t.Fatalf("left room channel = %v", left)
	}
	if !slices.Equal(right, []int16{0, 0, 32767, -10000, 0}) {
		t.Fatalf("right agent channel = %v", right)
	}
	if left[2] == 0 || right[2] == 0 {
		t.Fatal("room/agent overlap was not preserved across stereo channels")
	}
}

func TestReviewRunWritesCompleteElevenCaseMultimodalBundle(t *testing.T) {
	suite := reviewTestSuite(t)
	if len(suite) != 11 {
		t.Fatalf("scenario suite has %d cases, want the reviewed eleven", len(suite))
	}
	secret := "fixture-session-token-must-not-leak"
	querySecret := "fixture-query-secret-must-not-leak"
	directory := filepath.Join(t.TempDir(), "scenario-review-fixture")
	run, err := NewReviewRun(ReviewOptions{
		Directory: directory, Scenarios: suite, Repeats: 2, Secrets: []string{secret},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, evidence := fixtureScenarioGraphExecution(t, "fixture#1")
	for caseIndex, item := range suite {
		for trial := 1; trial <= 2; trial++ {
			capture := bench.SessionAudioCapture{
				SampleRateHz: 24_000,
				RoomPCM16:    []int16{int16(caseIndex + 1), int16(trial), 0, 0},
			}
			result := Result{
				Scenario: item.Name,
				Passed:   true,
				Transcript: bench.Transcript{Moments: []bench.Moment{
					{AtMS: 1, Kind: bench.MomentTranscript, Text: "review user turn"},
					{AtMS: 2, Kind: bench.MomentAgentText, Text: "review agent turn"},
					{AtMS: 3, Kind: bench.MomentToolCall, Name: "review.tool", Arguments: `{"digit":"2"}`},
					{AtMS: 4, Kind: bench.MomentToolResult, Name: "review.tool", Text: `{"ok":true}`},
					{AtMS: 5, Kind: bench.MomentResponseDone},
				}},
				Latencies: []Latency{{After: "user said line 0", EndedMS: 1, MS: 2, Heard: true}},
			}
			for sightIndex := range item.Sees {
				result.Transcript.Moments = append(result.Transcript.Moments,
					bench.Moment{AtMS: 6, Kind: bench.MomentScheduled,
						Name: "scenario.sight." + strconv.Itoa(sightIndex+1)})
			}
			var runErr error
			if caseIndex == 0 && trial == 1 {
				result.Passed = false
				result.Failures = []string{"fixture failure carried " + secret}
				runErr = errors.New("Bearer " + secret +
					" was refused at wss://fixture.invalid/realtime?api_key=" + querySecret)
			} else {
				capture.Agent = []bench.TimedAudioChunk{{AtMS: 0, PCM16: []int16{500, 600}}}
			}
			if caseIndex == 1 && trial == 1 {
				result.Transcript.Execution = &evidence
			}
			if err := run.Record(item.Name, trial, capture, result, runErr); err != nil {
				t.Fatalf("record %q trial %d: %v", item.Name, trial, err)
			}
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("idempotent complete close: %v", err)
	}

	manifest := readReviewManifest(t, directory)
	if manifest.Format != "openrealtime.review" || manifest.FormatVersion != 1 ||
		!manifest.Complete || manifest.Reportable || manifest.EvidencePolicy != "unattested" ||
		manifest.Expected != 22 || len(manifest.Attempts) != 22 ||
		len(manifest.Cases) != 11 || len(manifest.Missing) != 0 {
		t.Fatalf("review manifest completeness = %+v", manifest)
	}
	for index, item := range suite {
		if manifest.Cases[index].Name != item.Name || manifest.Cases[index].Ordinal != index+1 {
			t.Fatalf("case %d = %+v, want %q", index, manifest.Cases[index], item.Name)
		}
	}
	visualCases, submitted := 0, 0
	for _, item := range manifest.Cases {
		if len(item.Inputs) == 0 {
			continue
		}
		visualCases++
		if len(item.Inputs) != 2 {
			t.Fatalf("visual case input media = %+v", item.Inputs)
		}
		for _, media := range item.Inputs {
			assertReviewMediaHash(t, directory, media)
			if media.Kind != "image" || media.Role != "scripted_visual_input" ||
				media.MediaType != "image/png" || media.CueMS <= 0 {
				t.Fatalf("visual input identity = %+v", media)
			}
		}
	}
	if visualCases != 1 {
		t.Fatalf("visual cases = %d, want 1", visualCases)
	}
	for _, attempt := range manifest.Attempts {
		assertReviewMediaHash(t, directory, attempt.Audio)
		if attempt.Audio.Audio == nil || attempt.Audio.Audio.Channels != 2 ||
			!slices.Equal(attempt.Audio.Audio.ChannelLayout,
				[]string{"left:scripted_room_user", "right:agent_output"}) {
			t.Fatalf("attempt audio identity = %+v", attempt.Audio)
		}
		left, right := decodeStereoReviewWAV(t, mustReadFile(t, filepath.Join(directory, attempt.Audio.Path)))
		if len(left) != len(right) || len(left) != int(attempt.Audio.Audio.Frames) {
			t.Fatalf("attempt channel frames = %d/%d, spec %+v", len(left), len(right), attempt.Audio.Audio)
		}
		if attempt.Scenario == suite[0].Name && attempt.Trial == 1 {
			for _, sample := range right {
				if sample != 0 {
					t.Fatal("failed empty-output attempt has non-silent agent channel")
				}
			}
		}
		for _, input := range attempt.Inputs {
			if input.Status == "submitted" {
				submitted++
			}
		}
	}
	if submitted != 4 {
		t.Fatalf("observed visual submissions = %d, want 2 inputs x 2 trials", submitted)
	}
	if manifest.Attempts[2].Evidence == nil ||
		manifest.Attempts[2].EvidenceStatus != "attested" ||
		manifest.Attempts[2].Evidence.Kind != bench.ExecutionGraphNative ||
		manifest.Attempts[2].Evidence.Fingerprint != evidence.Fingerprint {
		t.Fatalf("execution evidence identity = %+v", manifest.Attempts[2].Evidence)
	}

	review := string(mustReadFile(t, filepath.Join(directory, "REVIEW.md")))
	for _, want := range []string{
		"Complete: yes (22/22 media-complete attempts retained)", "left channel", "right channel",
		"Behavioral reportable: no",
		"Scripted visual inputs", "submitted", "User: review user turn",
		"Agent: review agent turn", "Tool call", "Latency after", "fixture failure",
		"not graph-native attestation",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("review report does not contain %q\n%s", want, review)
		}
	}
	for _, path := range []string{filepath.Join(directory, "manifest.json"), filepath.Join(directory, "REVIEW.md")} {
		contents := string(mustReadFile(t, path))
		if strings.Contains(contents, secret) || strings.Contains(contents, querySecret) {
			t.Fatalf("review artifact %q exposed a configured secret", path)
		}
	}
}

func TestScenarioSightScheduleCarriesExactReviewMediaIdentity(t *testing.T) {
	events, err := sights([]Sight{
		{AtMS: 7, Path: "testdata/build-running.png"},
		{AtMS: 16, Path: "testdata/build-finished.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Name != "scenario.sight.1" ||
		events[1].Name != "scenario.sight.1.response-create" ||
		events[2].Name != "scenario.sight.2" ||
		events[3].Name != "scenario.sight.2.response-create" {
		t.Fatalf("scenario sight identities = %+v", events)
	}
	for index := 0; index < len(events); index += 2 {
		if events[index].AtMS != events[index+1].AtMS ||
			events[index].Event["type"] != "conversation.item.create" ||
			events[index+1].Event["type"] != "response.create" {
			t.Fatalf("scenario sight pair %d = %+v / %+v", index/2+1, events[index], events[index+1])
		}
	}
}

func TestReviewRunRejectsUnsafeOrExistingPaths(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	missingParent := filepath.Join(parent, "missing", "run")
	symlink := filepath.Join(parent, "linked")
	if err := os.Symlink(parent, symlink); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "empty", path: "", want: "must not be empty"},
		{name: "existing", path: existing, want: "exclusively"},
		{name: "traversal", path: parent + string(filepath.Separator) + "child" +
			string(filepath.Separator) + ".." + string(filepath.Separator) + "run", want: "clean path"},
		{name: "missing parent", path: missingParent, want: "inspect review directory parent"},
		{name: "symlink parent", path: filepath.Join(symlink, "run"), want: "must not be a symlink"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewReviewRun(ReviewOptions{
				Directory: test.path, Scenarios: []Scenario{{Name: "fixture"}}, Repeats: 1,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe review path error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReviewRunWritesIncompleteIndexAndRefusesDuplicateAttempts(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "incomplete")
	run, err := NewReviewRun(ReviewOptions{
		Directory: directory,
		Scenarios: []Scenario{{Name: "first"}, {Name: "second"}},
		Repeats:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	capture := bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: []int16{1}}
	result := Result{Scenario: "first", Passed: false}
	if err := run.Record("first", 1, capture, result, errors.New("fixture failure")); err != nil {
		t.Fatal(err)
	}
	if err := run.Record("first", 1, capture, result, nil); err == nil ||
		!strings.Contains(err.Error(), "already retained") {
		t.Fatalf("duplicate record error = %v", err)
	}
	closeErr := run.Close()
	if closeErr == nil || !strings.Contains(closeErr.Error(), "1 of 4") {
		t.Fatalf("incomplete close error = %v", closeErr)
	}
	if again := run.Close(); again == nil || again.Error() != closeErr.Error() {
		t.Fatalf("idempotent incomplete close = %v, want %v", again, closeErr)
	}
	manifest := readReviewManifest(t, directory)
	if manifest.Complete || len(manifest.Attempts) != 1 || len(manifest.Missing) != 3 {
		t.Fatalf("incomplete manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(directory, "REVIEW.md")); err != nil {
		t.Fatalf("incomplete human review: %v", err)
	}
}

func TestReviewManifestIsFinalCommitMarkerWhenReportFinalizationFails(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "report-finalization")
	run, err := NewReviewRun(ReviewOptions{
		Directory: directory, Scenarios: []Scenario{{Name: "fixture"}}, Repeats: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Record("fixture", 1,
		bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: []int16{1}},
		Result{Scenario: "fixture", Passed: true}, nil); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(directory, "REVIEW.md")
	if err := os.WriteFile(reportPath, []byte("caller-owned collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	closeErr := run.Close()
	if closeErr == nil || !strings.Contains(closeErr.Error(), "write review report") {
		t.Fatalf("report finalization error = %v", closeErr)
	}
	if _, err := os.Stat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("manifest commit marker exists after report failure: %v", err)
	}
	if got := string(mustReadFile(t, reportPath)); got != "caller-owned collision" {
		t.Fatalf("report collision was overwritten: %q", got)
	}
	if again := run.Close(); again == nil || again.Error() != closeErr.Error() {
		t.Fatalf("idempotent failed close = %v, want %v", again, closeErr)
	}
}

func TestReviewRunRootContainsWritesAfterDirectoryReplacement(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "review")
	run, err := NewReviewRun(ReviewOptions{
		Directory: directory, Scenarios: []Scenario{{Name: "fixture"}}, Repeats: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "owned-review")
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(parent, "outside")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, directory); err != nil {
		t.Fatal(err)
	}
	if err := run.Record("fixture", 1,
		bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: []int16{1}},
		Result{Scenario: "fixture", Passed: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("contained review write escaped into replacement directory: %+v", entries)
	}
	if _, err := os.Stat(filepath.Join(moved, "manifest.json")); err != nil {
		t.Fatalf("contained review commit marker: %v", err)
	}
}

func TestReviewAudioRejectsInvalidRatesAndTimelineOffsets(t *testing.T) {
	tests := []struct {
		name    string
		capture bench.SessionAudioCapture
		want    string
	}{
		{name: "wrong rate", capture: bench.SessionAudioCapture{SampleRateHz: 16_000}, want: "sample rate"},
		{name: "negative", capture: bench.SessionAudioCapture{SampleRateHz: 24_000,
			Agent: []bench.TimedAudioChunk{{AtMS: -1, PCM16: []int16{1}}}}, want: "invalid start"},
		{name: "nan", capture: bench.SessionAudioCapture{SampleRateHz: 24_000,
			Agent: []bench.TimedAudioChunk{{AtMS: math.NaN(), PCM16: []int16{1}}}}, want: "invalid start"},
		{name: "infinite", capture: bench.SessionAudioCapture{SampleRateHz: 24_000,
			Agent: []bench.TimedAudioChunk{{AtMS: math.Inf(1), PCM16: []int16{1}}}}, want: "invalid start"},
		{name: "container overflow", capture: bench.SessionAudioCapture{SampleRateHz: 24_000,
			Agent: []bench.TimedAudioChunk{{AtMS: float64(math.MaxUint32), PCM16: []int16{1}}}}, want: "WAV container"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := encodeReviewStereoWAV(test.capture)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid review audio error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReviewRunRequiresMatchingExecutionEvidenceForReportability(t *testing.T) {
	requirement, evidence := fixtureScenarioGraphExecution(t, "fixture#1")
	directory := filepath.Join(t.TempDir(), "required-evidence")
	run, err := NewReviewRun(ReviewOptions{
		Directory: directory, Scenarios: []Scenario{{Name: "fixture"}}, Repeats: 1,
		ExecutionRequirement: requirement,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Record("fixture", 1,
		bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: []int16{1}},
		Result{Scenario: "fixture", Passed: false,
			Failures:   []string{"a behavior check failed"},
			Transcript: bench.Transcript{Execution: &evidence}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := readReviewManifest(t, directory)
	if !manifest.Complete || !manifest.Reportable || manifest.EvidencePolicy != "required" ||
		manifest.ExecutionRequirement == nil || !manifest.Attempts[0].Reportable {
		t.Fatalf("required execution reportability = %+v", manifest)
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(directory, "REVIEW.md"))),
		"Behavioral reportable: yes") {
		t.Fatal("human review did not state exact-evidence reportability")
	}
}

func fixtureScenarioGraphExecution(
	t testing.TB, scope string,
) (bench.ExecutionRequirement, bench.ExecutionEvidence) {
	t.Helper()
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	graph := bench.GraphEvidence{
		Graph: bench.GraphIdentity{
			FormatVersion: ir.FormatVersion, ID: "scenario_review", Revision: 1,
			Fingerprint: digest,
		},
		Configuration: bench.ArtifactIdentity{ID: "config://scenario-review", Digest: digest},
		Nodes: []bench.GraphNodeEvidence{{
			Node: "interaction",
			Element: element.Identity{
				Name: "scenario.FixtureInteraction", Revision: 1, Digest: digest,
			},
			Implementation: "fixture.scenario.interaction.v1",
			Config: bench.ArtifactIdentity{
				ID: "config://scenario-review/interaction", Digest: digest,
			},
			Runtime: bench.ArtifactIdentity{
				ID: "runtime://scenario-review/interaction", Revision: "1",
			},
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
		Scope:         scope,
		Graph:         &graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	return requirement, evidence
}

func TestReviewRunRejectsSymlinkedStillBeforeCreatingDirectory(t *testing.T) {
	parent := t.TempDir()
	image := filepath.Join(parent, "image.png")
	if err := os.WriteFile(image, mustReadFile(t, "testdata/build-finished.png"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "linked.png")
	if err := os.Symlink(image, linked); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "review")
	_, err := NewReviewRun(ReviewOptions{
		Directory: directory,
		Scenarios: []Scenario{{Name: "visual", Sees: []Sight{{AtMS: 1, Path: linked}}}},
		Repeats:   1,
	})
	if err == nil || !strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("symlinked still error = %v", err)
	}
	if _, statErr := os.Stat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("review directory exists after input refusal: %v", statErr)
	}
}

func reviewTestSuite(t *testing.T) []Scenario {
	t.Helper()
	suite := Suite()
	for itemIndex := range suite {
		suite[itemIndex].Sees = append([]Sight(nil), suite[itemIndex].Sees...)
		for sightIndex := range suite[itemIndex].Sees {
			suite[itemIndex].Sees[sightIndex].Path = filepath.Join(
				"testdata", filepath.Base(suite[itemIndex].Sees[sightIndex].Path))
		}
	}
	return suite
}

func readReviewManifest(t *testing.T, directory string) ReviewManifest {
	t.Helper()
	var manifest ReviewManifest
	if err := json.Unmarshal(mustReadFile(t, filepath.Join(directory, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertReviewMediaHash(t *testing.T, directory string, media ReviewMedia) {
	t.Helper()
	payload := mustReadFile(t, filepath.Join(directory, media.Path))
	digest := sha256.Sum256(payload)
	if got := "sha256:" + hex.EncodeToString(digest[:]); got != media.SHA256 {
		t.Fatalf("media %q digest = %s, want %s", media.Path, media.SHA256, got)
	}
}

func decodeStereoReviewWAV(t *testing.T, wav []byte) ([]int16, []int16) {
	t.Helper()
	if len(wav) < 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		binary.LittleEndian.Uint16(wav[20:22]) != 1 ||
		binary.LittleEndian.Uint16(wav[22:24]) != 2 ||
		binary.LittleEndian.Uint32(wav[24:28]) != 24_000 ||
		binary.LittleEndian.Uint16(wav[34:36]) != 16 || string(wav[36:40]) != "data" {
		t.Fatalf("invalid canonical stereo review WAV header: %x", wav[:min(len(wav), 44)])
	}
	dataLength := int(binary.LittleEndian.Uint32(wav[40:44]))
	if dataLength != len(wav)-44 || dataLength%4 != 0 {
		t.Fatalf("WAV data length = %d, payload = %d", dataLength, len(wav)-44)
	}
	frames := dataLength / 4
	left, right := make([]int16, frames), make([]int16, frames)
	for frame := 0; frame < frames; frame++ {
		left[frame] = int16(binary.LittleEndian.Uint16(wav[44+frame*4:]))
		right[frame] = int16(binary.LittleEndian.Uint16(wav[46+frame*4:]))
	}
	return left, right
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
