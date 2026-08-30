package meeting

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/computeruse"
)

func TestSuiteOwnsValidRecordedScenarios(t *testing.T) {
	tasks := Suite()
	if len(tasks) != 4 {
		t.Fatalf("meeting suite has %d tasks, want 4", len(tasks))
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if err := task.Validate(); err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
		if seen[task.ID] {
			t.Fatalf("duplicate task %q", task.ID)
		}
		seen[task.ID] = true
		if _, err := audioAssets.ReadFile("testdata/audio/" + task.AudioAsset); err != nil {
			t.Fatalf("%s audio: %v", task.ID, err)
		}
	}
}

func TestReferenceCellIsOnlyDirectGraphNativeCandidate(t *testing.T) {
	cell := ReferenceCell()
	want := map[bench.Factor]string{
		bench.FactorBinding:   "graph-native-meeting-v1",
		bench.FactorCognition: "foreground-fast+graph-background",
		bench.FactorObservers: "audio+screen",
		bench.FactorCadence:   "200ms", bench.FactorFloor: "foreground-engine",
		bench.FactorSlowModel:  "gemini-3.7-flash/minimal",
		bench.FactorComponents: "narration-only",
		bench.FactorPolicy:     "foreground-fast-tool-continuations+graph-background-injection",
		bench.FactorFastModel:  "qwen-fast/minimal",
		bench.FactorFastAction: "bounded-execution-via-graph",
		bench.FactorVideoRate:  "5fps", bench.FactorRecognizer: "sensevoice-small",
		bench.FactorTransport: bench.TransportWebSocket,
	}
	if cell.Name != "meeting-assistant-graph-native-candidate" ||
		len(cell.Levels) != len(want) || len(cell.Varies) != 0 {
		t.Fatalf("Meeting candidate cell = %+v", cell)
	}
	for factor, level := range want {
		if cell.Levels[factor] != level {
			t.Fatalf("Meeting candidate %s = %q, want %q", factor, cell.Levels[factor], level)
		}
	}
	for factor, level := range cell.Levels {
		legacy := strings.ToLower(level)
		if strings.Contains(legacy, "cascade") || strings.Contains(legacy, "omni") ||
			strings.Contains(legacy, "qwen3-vl") || strings.Contains(legacy, "gemini-3.5") {
			t.Fatalf("Meeting candidate retained legacy %s level %q", factor, level)
		}
	}
}

func TestMeetingKnowledgeDeclarationsExplicitlyOptIntoBoundedForegroundExecution(t *testing.T) {
	target := computeruse.Target{
		Name: "meeting-browser", Sources: []string{"screen"}, Width: 1280, Height: 720,
	}
	for _, taskID := range []string{"open-share-present", "follow-up-during-analysis"} {
		var task Task
		for _, candidate := range Suite() {
			if candidate.ID == taskID {
				task = candidate
				break
			}
		}
		if task.ID == "" {
			t.Fatalf("suite omitted %s", taskID)
		}
		declared, err := declarations(target, task)
		if err != nil {
			t.Fatal(err)
		}
		wantName := ToolReadLaunchReview
		if taskID == "follow-up-during-analysis" {
			wantName = ToolAnalyzeLaunchReview
		}
		found := false
		for _, raw := range declared {
			var tool struct {
				Name         string `json:"name"`
				OpenRealtime struct {
					Background bool `json:"background"`
				} `json:"openrealtime"`
			}
			if err := json.Unmarshal(raw, &tool); err != nil {
				t.Fatal(err)
			}
			if tool.Name != wantName {
				continue
			}
			found = true
			if !tool.OpenRealtime.Background {
				t.Fatalf("%s did not declare its read-only background-safe execution contract", wantName)
			}
		}
		if !found {
			t.Fatalf("%s declaration was omitted", wantName)
		}
	}
}

func TestFixtureManifestPinsEveryRecordedInput(t *testing.T) {
	var manifest struct {
		Suite    string `json:"suite"`
		Fixtures []struct {
			File     string `json:"file"`
			SHA256   string `json:"sha256"`
			Segments []struct {
				StartMS float64 `json:"start_ms"`
				EndMS   float64 `json:"end_ms"`
			} `json:"segments"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(fixtureManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Suite != SuiteName || len(manifest.Fixtures) != ExpectedTasks() {
		t.Fatalf("manifest does not describe the suite: %+v", manifest)
	}
	known := map[string]bool{}
	for _, fixture := range manifest.Fixtures {
		payload, err := audioAssets.ReadFile("testdata/audio/" + fixture.File)
		if err != nil {
			t.Fatalf("%s: %v", fixture.File, err)
		}
		digest := sha256.Sum256(payload)
		if hex.EncodeToString(digest[:]) != fixture.SHA256 {
			t.Fatalf("%s no longer matches its canonical hash", fixture.File)
		}
		if len(fixture.Segments) == 0 || fixture.Segments[0].StartMS != 0 {
			t.Fatalf("%s has incomplete segment provenance", fixture.File)
		}
		for index, segment := range fixture.Segments {
			if segment.EndMS < segment.StartMS ||
				(index > 0 && segment.StartMS != fixture.Segments[index-1].EndMS) {
				t.Fatalf("%s has discontinuous segments: %+v", fixture.File, fixture.Segments)
			}
		}
		known[fixture.File] = true
	}
	for _, task := range Suite() {
		if !known[task.AudioAsset] {
			t.Errorf("%s is not pinned by fixtures.json", task.AudioAsset)
		}
	}
}

func TestSelectNeverChangesDeclaredSuiteSize(t *testing.T) {
	selected, err := Select([]string{"concurrent-work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != "follow-up-during-analysis" {
		t.Fatalf("selected %+v", selected)
	}
	if ExpectedTasks() != 4 {
		t.Fatalf("expected tasks = %d", ExpectedTasks())
	}
}
