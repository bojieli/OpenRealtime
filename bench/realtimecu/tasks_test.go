package realtimecu

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/computeruse"
)

func TestSuiteOwnsEveryAxisAndBothGroundingModes(t *testing.T) {
	seenAxes := map[Axis]bool{}
	seenGrounding := map[Grounding]bool{}
	ids := map[string]bool{}
	tasks := Suite()
	if len(tasks) != 8 {
		t.Fatalf("Realtime-CU v1 declares eight task families, got %d", len(tasks))
	}
	for _, task := range tasks {
		if err := task.Validate(); err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
		if _, err := audioAssets.ReadFile("testdata/audio/" + task.AudioAsset); err != nil {
			t.Fatalf("%s audio asset %q: %v", task.ID, task.AudioAsset, err)
		}
		if ids[task.ID] {
			t.Fatalf("duplicate task %q", task.ID)
		}
		ids[task.ID] = true
		for _, axis := range task.Axes {
			seenAxes[axis] = true
		}
		for _, grounding := range task.Groundings {
			seenGrounding[grounding] = true
		}
	}
	for _, axis := range []Axis{AxisAudio, AxisVision, AxisInteraction, AxisCamera, AxisAuthorization} {
		if !seenAxes[axis] {
			t.Fatalf("the suite has no %s task", axis)
		}
	}
	for _, grounding := range []Grounding{GroundingPixel, GroundingSetOfMark} {
		if !seenGrounding[grounding] {
			t.Fatalf("the suite has no %s case", grounding)
		}
	}
	cases, err := Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 16 {
		t.Fatalf("eight families under two grounding conditions must produce 16 cases, got %d", len(cases))
	}
}

func TestSelectionExpandsGroundingAsACondition(t *testing.T) {
	cases, err := Select([]string{"control"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].ID() == cases[1].ID() {
		t.Fatalf("expected one task under two conditions, got %+v", cases)
	}
}

func TestRestrictedSelectionCannotBecomeAReportableSuite(t *testing.T) {
	selected, err := Select([]string{"control"}, []Grounding{GroundingPixel})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{Suite: SuiteName, Cell: bench.Reference(), Expected: len(complete)}
	for _, item := range selected {
		result.Tasks = append(result.Tasks, bench.TaskOutcome{ID: item.ID(), Completed: true, Passed: true})
	}
	result.Finish()
	if result.Summary.Complete || result.Reportable() == nil {
		t.Fatal("a category/grounding smoke run became a publishable capability result")
	}
}

func TestRealtimeCUReferenceNamesTheConfigurationItRuns(t *testing.T) {
	cell := ReferenceCell()
	want := map[bench.Factor]string{
		bench.FactorObservers: "audio+video", bench.FactorComponents: "keyframe",
		bench.FactorFastModel: "hosted-vision", bench.FactorFastAction: "slow-only",
		bench.FactorVideoRate: "3fps", bench.FactorRecognizer: "whisper-large-v3-turbo",
	}
	for factor, level := range want {
		if cell.Levels[factor] != level {
			t.Errorf("%s: got %q, want %q", factor, cell.Levels[factor], level)
		}
	}
}

func TestPixelInstructionStatesTheTargetCoordinateSpace(t *testing.T) {
	instruction := taskInstruction(Case{Grounding: GroundingPixel}, computeruse.Target{
		Name: "browser", Sources: []string{"screen"}, Width: 1280, Height: 577,
	})
	for _, wanted := range []string{"1280 by 577 CSS pixels", "x is 0 through 1279", "y is 0 through 576"} {
		if !strings.Contains(instruction, wanted) {
			t.Errorf("pixel instruction does not contain %q: %s", wanted, instruction)
		}
	}
	for _, wanted := range []string{
		"take no placeholder or precondition action", "exactly one computer action at a time",
		"wait for its result and changed screen", "focus the intended input with a click",
		"match its sounds against visible labels", "stop immediately when the page reports success",
	} {
		if !strings.Contains(instruction, wanted) {
			t.Errorf("pixel instruction does not contain behavior contract %q: %s", wanted, instruction)
		}
	}

	marked := taskInstruction(Case{Grounding: GroundingSetOfMark}, computeruse.Target{
		Name: "browser", Sources: []string{"screen"}, Width: 1280, Height: 577,
	})
	if strings.Contains(marked, "x is 0 through") || !strings.Contains(marked, computeruse.ClickElement) {
		t.Fatalf("set-of-mark instruction must name marks without inviting coordinates: %s", marked)
	}
}
