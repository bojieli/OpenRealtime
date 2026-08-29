package meeting

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
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

func TestOmniCellIsExplicitlyASystemTreatment(t *testing.T) {
	differences := bench.Compare(ReferenceCell(), OmniCell())
	if len(differences) != 2 || differences[0] != bench.FactorBinding ||
		differences[1] != bench.FactorFastModel {
		t.Fatalf("omni treatment differences = %v", differences)
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
