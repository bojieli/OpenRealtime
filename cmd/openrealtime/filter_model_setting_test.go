package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrozenProfileRetainsFIRFilter(t *testing.T) {
	directory := t.TempDir()
	profile := filepath.Join(directory, "profile.yaml")
	values := filepath.Join(directory, "values.json")
	var output bytes.Buffer
	err := runLaunchProfile([]string{
		"scenario", "-out", profile,
		"-graph-out", filepath.Join(directory, "graph.json"), "-values-out", values,
		"-noise-filter-url", "http://127.0.0.1:9167",
		"-noise-filter-model", "deepfilternet-fir", "-noise-filter-timeout-ms", "50",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deepfilternet-fir", "http://127.0.0.1:9167"} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("frozen values omitted %q", want)
		}
	}
}

func TestFilterModelSettingPreservesDefaultsAndAcceptsFIR(t *testing.T) {
	options := defaultTargetRoomProfileOptions()
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	bindScenarioProfileSettings(flags, &options)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if options.noiseFilterModel != "real-tse" {
		t.Fatal("binding flags changed the target-room default")
	}
	if err := flags.Parse([]string{"-noise-filter-model", "deepfilternet-fir"}); err != nil {
		t.Fatal(err)
	}
	if options.noiseFilterModel != "deepfilternet-fir" {
		t.Fatal("filter selection was not retained")
	}
}
