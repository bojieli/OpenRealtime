package main

import (
	"flag"
	"testing"
)

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
