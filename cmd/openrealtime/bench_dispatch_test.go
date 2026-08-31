package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkDispatchAliasesHaveOneCanonicalClassification(t *testing.T) {
	tests := []struct {
		canonical string
		kind      benchmarkInvocationKind
		aliases   []string
	}{
		{"execution", benchmarkInvocationReadOnly, []string{"execution", "attestation"}},
		{"architecture", benchmarkInvocationReadOnly, []string{"architecture", "architecture-pair", "f52"}},
		{"fdb-v1.5", benchmarkInvocationAttempt, []string{"fdb", "fdb-v1.5"}},
		{"fd-bench", benchmarkInvocationAttempt, []string{"fdbench", "fd-bench"}},
		{"fdb-v3", benchmarkInvocationAttempt, []string{"fdbv3", "fdb-v3"}},
		{"tau-voice", benchmarkInvocationAttempt, []string{"tau-voice", "tauvoice", "tau"}},
		{"realtime-cu", benchmarkInvocationAttempt, []string{"realtime-cu", "realtime-computer-use", "computer-use"}},
		{"meeting", benchmarkInvocationAttempt, []string{"meeting", "meeting-assistant", "live-meeting"}},
		{"dynacu", benchmarkInvocationBlocked, []string{"dynacu"}},
		{"review-candidate", benchmarkInvocationRecovery, []string{"review-candidate"}},
		{"verify-candidate-review", benchmarkInvocationReadOnly, []string{"verify-candidate-review"}},
	}
	for _, test := range tests {
		for _, alias := range test.aliases {
			t.Run(alias, func(t *testing.T) {
				dispatch, found := resolveBenchmarkDispatch(strings.ToUpper(alias))
				if !found || dispatch.run == nil || dispatch.canonical != test.canonical || dispatch.kind != test.kind {
					t.Fatalf("dispatch = %+v, found=%v", dispatch, found)
				}
			})
		}
	}
	if _, found := resolveBenchmarkDispatch("vision-latest"); found {
		t.Fatal("undeclared vision alias was accepted")
	}
}

func TestBenchmarkInvocationClassificationIncludesNestedReadOnlyAndRecoveryBranches(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      benchmarkInvocationKind
	}{
		{name: "FDB attempt", arguments: []string{"fdb"}, want: benchmarkInvocationAttempt},
		{name: "FD list", arguments: []string{"fd-bench", "-list"}, want: benchmarkInvocationReadOnly},
		{name: "FD explicit false list", arguments: []string{"fdbench", "-list=false"}, want: benchmarkInvocationAttempt},
		{name: "tau inventory", arguments: []string{"tau", "inventory", "-out", "inventory.json"}, want: benchmarkInvocationReadOnly},
		{name: "tau verify", arguments: []string{"tauvoice", "--verify=true"}, want: benchmarkInvocationReadOnly},
		{name: "tau attempt", arguments: []string{"tau-voice", "-verify=false"}, want: benchmarkInvocationAttempt},
		{name: "adaptive computer-use alias", arguments: []string{"realtime-computer-use"}, want: benchmarkInvocationAttempt},
		{name: "vision computer-use list", arguments: []string{"computer-use", "-list"}, want: benchmarkInvocationReadOnly},
		{name: "computer-use recovery", arguments: []string{"realtime-cu", "-review-resume", "-review-dir", "sealed"}, want: benchmarkInvocationRecovery},
		{name: "meeting list", arguments: []string{"live-meeting", "--list=true"}, want: benchmarkInvocationReadOnly},
		{name: "meeting recovery", arguments: []string{"meeting-assistant", "-review-resume=true"}, want: benchmarkInvocationRecovery},
		{name: "Dyna verify", arguments: []string{"dynacu", "-verify"}, want: benchmarkInvocationReadOnly},
		{name: "Dyna attempt blocked", arguments: []string{"dynacu"}, want: benchmarkInvocationBlocked},
		{name: "candidate recovery", arguments: []string{"review-candidate"}, want: benchmarkInvocationRecovery},
		{name: "candidate verification", arguments: []string{"verify-candidate-review"}, want: benchmarkInvocationReadOnly},
		{name: "architecture live inspection authoring", arguments: []string{"architecture-pair", "inspect"}, want: benchmarkInvocationReadOnly},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := classifyBenchmarkInvocation(test.arguments)
			if err != nil || got != test.want {
				t.Fatalf("classification = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if _, err := classifyBenchmarkInvocation([]string{"meeting", "-list=perhaps"}); err == nil {
		t.Fatal("invalid branch boolean was classified")
	}
}

func TestEvalAndSimulateRemainDiagnosticsNotBenchmarkAttemptProducers(t *testing.T) {
	for _, command := range []string{"eval", "simulate"} {
		kind, err := classifyTopLevelMeasurementInvocation(command, nil)
		if err != nil || kind != benchmarkInvocationDiagnostic {
			t.Fatalf("%s classification = %q, %v", command, kind, err)
		}
	}
	if kind, err := classifyTopLevelMeasurementInvocation("scenario", nil); err != nil ||
		kind != benchmarkInvocationAttempt {
		t.Fatalf("scenario classification = %q, %v", kind, err)
	}
	if kind, err := classifyTopLevelMeasurementInvocation(
		"bench", []string{"computer-use", "-list"},
	); err != nil || kind != benchmarkInvocationReadOnly {
		t.Fatalf("bench wrapper classification = %q, %v", kind, err)
	}
	if kind, err := classifyTopLevelMeasurementInvocation(
		"review", []string{"scenario"},
	); err != nil || kind != benchmarkInvocationRecovery {
		t.Fatalf("scenario recovery classification = %q, %v", kind, err)
	}
	if kind, err := classifyTopLevelMeasurementInvocation(
		"review", []string{"verify-scenario"},
	); err != nil || kind != benchmarkInvocationReadOnly {
		t.Fatalf("scenario verification classification = %q, %v", kind, err)
	}
}

func TestNonAttemptBranchesNeverReserveReviewArtifacts(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	tests := []struct {
		name      string
		arguments []string
		wantError string
	}{
		{name: "meeting list", arguments: []string{"meeting", "-list"}},
		{name: "computer-use list", arguments: []string{"computer-use", "-list"}},
		{name: "meeting list review flag", arguments: []string{"meeting", "-list", "-review-dir=ignored"}, wantError: "does not execute benchmark attempts"},
		{name: "computer-use list review flag", arguments: []string{"realtime-computer-use", "-list", "-review-provider=ignored"}, wantError: "does not execute benchmark attempts"},
		{name: "FD list review flag", arguments: []string{"fdbench", "-list", "-review-prefix=ignored"}, wantError: "does not execute benchmark attempts"},
		{name: "tau verify review flag", arguments: []string{"tau", "-verify", "-review-concurrency=2"}, wantError: "does not execute benchmark attempts"},
		{name: "Dyna attempt fail closed", arguments: []string{"dynacu"}, wantError: "attempts are disabled"},
		{name: "candidate recovery missing path", arguments: []string{"review-candidate"}, wantError: "requires -review-prefix"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := runBench(test.arguments, &output)
			if test.wantError == "" && err != nil {
				t.Fatalf("non-attempt command error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("non-attempt command error = %v, want %q", err, test.wantError)
			}
			if _, statErr := os.Lstat(filepath.Join(working, benchmarkArtifactDirectory)); !os.IsNotExist(statErr) {
				t.Fatalf("non-attempt command reserved benchmark artifacts: %v", statErr)
			}
		})
	}
}
