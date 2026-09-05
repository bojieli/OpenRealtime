package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
)

func TestScenarioProfileFreezesExactDiagnosticSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	// The caller's order cannot change canonical suite order.
	names := []string{"an acknowledgement is not an interruption", "a recorded menu"}
	if err := runLaunchProfile([]string{
		"scenario", "-case", names[0], "-case", names[1], "-out", path,
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.ParseYAML(path, payload)
	if err != nil {
		t.Fatal(err)
	}
	var configuration struct {
		Cases               []string `json:"cases"`
		ContractFingerprint string   `json:"contract_fingerprint"`
	}
	if err := json.Unmarshal(profile.Application.Configuration, &configuration); err != nil {
		t.Fatal(err)
	}
	contract, err := graphnative.BuildContract(names...)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(configuration.Cases, []string{names[1], names[0]}) ||
		configuration.ContractFingerprint != contract.Fingerprint {
		t.Fatalf("profile did not freeze selected diagnostic cases: %+v", configuration)
	}
}

func TestScenarioListsCanonicalSelectionWithoutResources(t *testing.T) {
	for _, names := range [][]string{nil, {"an acknowledgement is not an interruption", "a recorded menu"}} {
		args := []string{"-list", "-architecture-manifest", "must-not-be-read.json"}
		for _, name := range names {
			args = append(args, "-case", name)
		}
		var output bytes.Buffer
		if err := runScenario(args, &output); err != nil {
			t.Fatal(err)
		}
		contract, err := graphnative.BuildContract(names...)
		if err != nil {
			t.Fatal(err)
		}
		var want strings.Builder
		for _, item := range contract.Cases {
			want.WriteString(item.Name + "\n")
		}
		if output.String() != want.String() {
			t.Fatalf("listed cases = %q, want %q", output.String(), want.String())
		}
	}
}

func TestScenarioSelectionRejectsInvalidNamesBeforeResources(t *testing.T) {
	for _, names := range [][]string{
		{""}, {"unknown"}, {" an ordinary question"}, {"an ordinary question "},
		{"an ordinary question", "an ordinary question"},
		{"an ordinary question,a recorded menu"},
	} {
		for _, profile := range []bool{false, true} {
			directory := filepath.Join(t.TempDir(), "must-not-be-created")
			args := []string{"-review-dir", directory, "-architecture-manifest", "must-not-be-read.json"}
			if profile {
				args = []string{"-out", filepath.Join(directory, "profile.yaml")}
			}
			for _, name := range names {
				args = append(args, "-case", name)
			}
			var err error
			if profile {
				err = runScenarioProfileFreeze(args, &bytes.Buffer{})
			} else {
				err = runScenario(args, &bytes.Buffer{})
			}
			if err == nil || !strings.Contains(err.Error(), "scenario") ||
				strings.Contains(err.Error(), "must-not-be-read") {
				t.Fatalf("profile=%v names=%q error=%v", profile, names, err)
			}
			if _, err := os.Stat(directory); !os.IsNotExist(err) {
				t.Fatalf("invalid selection created output: %v", err)
			}
		}
	}
}

func TestScenarioSelectionRequiresExactFrozenProfilePopulation(t *testing.T) {
	const name = "an acknowledgement is not an interruption"
	selection, requirement, fingerprint := scenarioGraphCommandFixture(t, name)
	path := filepath.Join(t.TempDir(), "subset.yaml")
	payload, err := launchprofile.MarshalYAML(selection.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	cell := archbench.Cell{Architecture: archbench.Architecture{
		RuntimeBinding: selection.Profile.Adapter.ProfileName, Profile: fingerprint,
	}}
	prepared, err := prepareScenarioGraphSelection(path, cell, requirement, 15, name)
	if err != nil || prepared.Contract.Fingerprint != selection.Contract.Fingerprint {
		t.Fatalf("matching subset selection: %+v, %v", prepared, err)
	}
	for _, names := range [][]string{nil, {"an ordinary question"}, {name, "an ordinary question"}} {
		_, err := prepareScenarioGraphSelection(path, cell, requirement, 15, names...)
		if err == nil || !strings.Contains(err.Error(), "differs from the frozen application profile") {
			t.Fatalf("profile/CLI population drift %q: %v", names, err)
		}
	}
}

func TestScenarioDiagnosticSubsetRetainsExactMediaWithoutSuiteCredit(t *testing.T) {
	t.Chdir("../..")
	for _, retainFailure := range []bool{false, true} {
		// Includes visual media and a non-leading case to expose accidental full
		// suite indexing. Fifteen repetitions cannot promote a subset.
		names := []string{"telling them what it saw", "an acknowledgement is not an interruption"}
		directory, receipt, outcome := publishScenarioGraphPopulationFixture(t, 15, retainFailure, names...)
		source, err := graphnative.VerifySourceBundle(context.Background(),
			graphnative.SourceBundleOptions{Directory: directory}, receipt)
		if err != nil {
			t.Fatal(err)
		}
		checklist := source.Checklist
		if !checklist.Complete || checklist.FullSuite || checklist.Reportable || checklist.Passed ||
			checklist.Expected != 30 || checklist.ReportableAttempts != 30 ||
			source.Manifest.Cases != 2 || len(source.Manifest.Attempts) != 30 ||
			len(source.ArchitectureResult.Records) != 30 {
			t.Fatalf("subset mislabeled or wrong population: %+v", checklist)
		}
		wantPassed := 30
		if retainFailure {
			wantPassed--
		}
		if checklist.PassedAttempts != wantPassed || checklist.FailedAttempts != 30-wantPassed {
			t.Fatalf("subset lost behavioral outcomes: %+v", checklist)
		}
		last := source.Manifest.Attempts[29]
		if last.Record.Key.CaseOrdinal != 2 || last.Record.Key.CaseName != names[0] ||
			last.Record.Key.Trial != 15 || last.Audio.Path != "02-telling-them-what-it-saw-trial-15.stereo.wav" ||
			len(last.Record.Media.Submitted) != 2 {
			t.Fatalf("wrong subset media index: %+v", last)
		}
		var output bytes.Buffer
		reportScenarioGraphOutcome(&output, outcome, 15)
		for _, item := range scenario.Suite() {
			selected := item.Name == names[0] || item.Name == names[1]
			if strings.Contains(output.String(), item.Name) != selected {
				t.Fatalf("subset report misrepresents %q: %s", item.Name, output.String())
			}
		}
		if !strings.Contains(output.String(), "NOT REPORTABLE: graph-native checklist") {
			t.Fatalf("subset report omitted refusal: %s", output.String())
		}
	}
}
