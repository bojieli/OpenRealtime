package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleavebench"
)

func TestRunRequiresTaskAndAudio(t *testing.T) {
	err := run([]string{"--output", t.TempDir() + "/report.json"})
	if err == nil || !strings.Contains(err.Error(), "--task-id and --audio") {
		t.Fatalf("error = %v", err)
	}
}

func TestRuntimeValuesRejectSecretAndDuplicateKeys(t *testing.T) {
	var values runtimeValues
	if err := values.Set("gpu=RTX-PRO-6000"); err != nil {
		t.Fatal(err)
	}
	if err := values.Set("gpu=duplicate"); err == nil {
		t.Fatal("expected duplicate key failure")
	}
	if err := values.Set("api_token=secret"); err == nil {
		t.Fatal("expected secret-like key failure")
	}
	if values.Map()["gpu"] != "RTX-PRO-6000" {
		t.Fatalf("values = %+v", values.Map())
	}
}

func TestSelectTaskAndParseEffort(t *testing.T) {
	tasks := []interleavebench.Task{{ID: "a"}, {ID: "b"}}
	if task, err := selectTask(tasks, "b"); err != nil || task.ID != "b" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	if _, err := selectTask(tasks, "missing"); err == nil {
		t.Fatal("expected missing task failure")
	}
	if effort, err := parseEffort("HIGH"); err != nil || effort != continuation.EffortHigh {
		t.Fatalf("effort=%q err=%v", effort, err)
	}
	if _, err := parseEffort("extreme"); err == nil {
		t.Fatal("expected invalid effort failure")
	}
}

func TestSafeName(t *testing.T) {
	if got := safeName("task/../../one"); got != "task-------one" {
		t.Fatalf("safe name = %q", got)
	}
}

func TestValidateOptionsRequiresFastPreparationForSlowPreparation(t *testing.T) {
	t.Parallel()
	err := validateOptions(options{
		taskID: "task", audio: "input.wav", totalTimeout: time.Second,
		requestTimeout: time.Second, asrFrame: 50 * time.Millisecond,
		fastTokens: 1, slowTokens: 1, maxSlowInvocations: 1,
		prepareFast: false, prepareSlow: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--prepare-slow requires --prepare-fast") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateOptionsChecksSlowPreparationPacing(t *testing.T) {
	t.Parallel()
	base := options{
		taskID: "task", audio: "input.wav", totalTimeout: time.Second,
		requestTimeout: time.Second, asrFrame: 50 * time.Millisecond,
		fastTokens: 1, slowTokens: 1, maxSlowInvocations: 1,
		prepareFast: true,
	}
	negative := base
	negative.slowPreparationMinInterval = -time.Millisecond
	if err := validateOptions(negative); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("negative interval error = %v", err)
	}
	withoutSlow := base
	withoutSlow.slowPreparationMinInterval = time.Second
	if err := validateOptions(withoutSlow); err == nil || !strings.Contains(err.Error(), "requires --prepare-slow") {
		t.Fatalf("disabled slow preparation error = %v", err)
	}
	withSlow := base
	withSlow.prepareSlow = true
	withSlow.slowPreparationMinInterval = time.Second
	if err := validateOptions(withSlow); err != nil {
		t.Fatalf("valid paced configuration: %v", err)
	}
}
