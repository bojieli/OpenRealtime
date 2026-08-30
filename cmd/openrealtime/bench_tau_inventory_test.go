package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTauVoiceInventoryIsAnExplicitFlagsOnlySubcommand(t *testing.T) {
	var output bytes.Buffer
	err := runTauVoice([]string{"inventory", "unexpected"}, &output)
	if err == nil || !strings.Contains(err.Error(), "flags only") {
		t.Fatalf("inventory positional argument error = %v", err)
	}
}

func TestTauVoiceRunRejectsPositionalArguments(t *testing.T) {
	var output bytes.Buffer
	err := runTauVoice([]string{"unexpected"}, &output)
	if err == nil || !strings.Contains(err.Error(), "accepts flags only") {
		t.Fatalf("tau-voice positional argument error = %v", err)
	}
}
