package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseBenchmarkOutput(t *testing.T) {
	raw := strings.Join([]string{
		"goos: linux",
		"BenchmarkOther-32 1 7 ns/op 8 B/op 1 allocs/op",
		"BenchmarkCompareFDBench6147-32 1 101 ns/op 777 artifact-bytes 202 B/op 3 allocs/op",
		"BenchmarkCompareFDBench6147 1 99 ns/op 777 artifact-bytes 200 B/op 2 allocs/op",
		"PASS",
	}, "\n")
	want := []Sample{
		{NSPerOp: 101, BytesPerOp: 202, AllocsPerOp: 3, ArtifactBytes: 777},
		{NSPerOp: 99, BytesPerOp: 200, AllocsPerOp: 2, ArtifactBytes: 777},
	}
	got, err := parseBenchmarkOutput(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse benchmark output: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("samples = %+v, want %+v", got, want)
	}

	for name, raw := range map[string]string{
		"missing":    "BenchmarkOther-32 1 1 ns/op",
		"iterations": "BenchmarkCompareFDBench6147-32 2 101 ns/op 777 artifact-bytes 202 B/op 3 allocs/op",
		"metric":     "BenchmarkCompareFDBench6147-32 1 101 ns/op 777 result-bytes 202 B/op 3 allocs/op",
		"value":      "BenchmarkCompareFDBench6147-32 1 bad ns/op 777 artifact-bytes 202 B/op 3 allocs/op",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBenchmarkOutput(strings.NewReader(raw)); err == nil {
				t.Fatal("malformed benchmark output was accepted")
			}
		})
	}
	if _, err := parseBenchmarkOutput(nil); err == nil {
		t.Fatal("nil benchmark output was accepted")
	}
}

func TestEvidenceRejectsPartialMixedAndTamperedData(t *testing.T) {
	valid := testEvidence(t, []Sample{
		{NSPerOp: 100, BytesPerOp: 200, AllocsPerOp: 10, ArtifactBytes: 777},
		{NSPerOp: 110, BytesPerOp: 210, AllocsPerOp: 11, ArtifactBytes: 777},
		{NSPerOp: 120, BytesPerOp: 220, AllocsPerOp: 12, ArtifactBytes: 777},
	})
	baselineBytes := mustMarshalEvidence(t, valid)
	candidate := valid
	comparison := compareEvidence(valid, candidate, baselineBytes)
	candidate.Comparison = &comparison

	mutations := map[string]func(*Evidence){
		"partial samples": func(evidence *Evidence) {
			evidence.Samples = evidence.Samples[:2]
		},
		"mixed artifact bytes": func(evidence *Evidence) {
			evidence.Samples = slices.Clone(evidence.Samples)
			evidence.Samples[1].ArtifactBytes++
		},
		"summary not derived": func(evidence *Evidence) {
			evidence.Summary.TimeNS.Median++
		},
		"unsorted source files": func(evidence *Evidence) {
			evidence.Source.Files = []string{"z.go", "a.go"}
		},
		"comparison candidate not derived": func(evidence *Evidence) {
			evidence.Comparison.Artifact.Candidate++
		},
		"comparison ratio not derived": func(evidence *Evidence) {
			evidence.Comparison.Timing.CandidateRatio++
		},
		"acceptance inconsistent": func(evidence *Evidence) {
			evidence.Comparison.Accepted = false
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copy := cloneEvidence(candidate)
			mutate(&copy)
			if err := copy.validate(); err == nil {
				t.Fatal("tampered evidence was accepted")
			}
		})
	}

	compact := bytes.TrimSpace(baselineBytes)
	compact = []byte(strings.ReplaceAll(string(compact), "\n", ""))
	if _, err := decodeEvidence(compact); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical JSON error = %v", err)
	}
	unknown := bytes.Replace(baselineBytes, []byte("\n}"), []byte(",\n  \"unknown\": true\n}"), 1)
	if _, err := decodeEvidence(unknown); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown-field error = %v", err)
	}
}

func TestCompareEvidenceDeterministicGates(t *testing.T) {
	baseline := testEvidence(t, []Sample{
		{NSPerOp: 100, BytesPerOp: 200, AllocsPerOp: 10, ArtifactBytes: 777},
		{NSPerOp: 110, BytesPerOp: 210, AllocsPerOp: 10, ArtifactBytes: 777},
		{NSPerOp: 120, BytesPerOp: 220, AllocsPerOp: 11, ArtifactBytes: 777},
	})
	baselineBytes := mustMarshalEvidence(t, baseline)

	t.Run("timing and bytes are descriptive", func(t *testing.T) {
		candidate := withSamples(t, baseline, []Sample{
			{NSPerOp: 1000, BytesPerOp: 2000, AllocsPerOp: 9, ArtifactBytes: 777},
			{NSPerOp: 1100, BytesPerOp: 2100, AllocsPerOp: 10, ArtifactBytes: 777},
			{NSPerOp: 1200, BytesPerOp: 2200, AllocsPerOp: 10, ArtifactBytes: 777},
		})
		got := compareEvidence(baseline, candidate, baselineBytes)
		if !got.Reportable || !got.Accepted || got.Artifact.Status != "pass" ||
			got.Allocations.Status != "pass" || got.Timing.Status != "descriptive_only" ||
			got.Bytes.Status != "descriptive_only" {
			t.Fatalf("comparison = %+v", got)
		}
	})

	t.Run("allocation regression rejects", func(t *testing.T) {
		candidate := withSamples(t, baseline, []Sample{
			{NSPerOp: 100, BytesPerOp: 200, AllocsPerOp: 11, ArtifactBytes: 777},
			{NSPerOp: 110, BytesPerOp: 210, AllocsPerOp: 11, ArtifactBytes: 777},
			{NSPerOp: 120, BytesPerOp: 220, AllocsPerOp: 11, ArtifactBytes: 777},
		})
		got := compareEvidence(baseline, candidate, baselineBytes)
		if !got.Reportable || got.Accepted || got.Allocations.Status != "fail" {
			t.Fatalf("comparison = %+v", got)
		}
	})

	t.Run("artifact regression rejects", func(t *testing.T) {
		candidate := withSamples(t, baseline, []Sample{
			{NSPerOp: 100, BytesPerOp: 200, AllocsPerOp: 10, ArtifactBytes: 778},
			{NSPerOp: 110, BytesPerOp: 210, AllocsPerOp: 10, ArtifactBytes: 778},
			{NSPerOp: 120, BytesPerOp: 220, AllocsPerOp: 10, ArtifactBytes: 778},
		})
		got := compareEvidence(baseline, candidate, baselineBytes)
		if !got.Reportable || got.Accepted || got.Artifact.Status != "fail" {
			t.Fatalf("comparison = %+v", got)
		}
	})

	t.Run("toolchain mismatch is unreportable", func(t *testing.T) {
		candidate := cloneEvidence(baseline)
		candidate.Protocol.GoVersion = "go9.9.9"
		got := compareEvidence(baseline, candidate, baselineBytes)
		if got.Reportable || got.Accepted || got.Allocations.Status != "not_comparable" ||
			got.Timing.Status != "not_comparable" {
			t.Fatalf("comparison = %+v", got)
		}
	})

	t.Run("CPU mismatch only makes timing incomparable", func(t *testing.T) {
		candidate := cloneEvidence(baseline)
		candidate.Protocol.CPU = "another CPU"
		got := compareEvidence(baseline, candidate, baselineBytes)
		if !got.Reportable || !got.Accepted || got.Allocations.Status != "pass" ||
			got.Timing.Status != "not_comparable" {
			t.Fatalf("comparison = %+v", got)
		}
	})

	t.Run("source scope mismatch is unreportable", func(t *testing.T) {
		candidate := cloneEvidence(baseline)
		candidate.Source.Files = []string{"different.go"}
		got := compareEvidence(baseline, candidate, baselineBytes)
		if got.Reportable || got.Accepted || got.Allocations.Status != "not_comparable" ||
			got.Timing.Status != "not_comparable" {
			t.Fatalf("comparison = %+v", got)
		}
	})
}

func TestWriteEvidenceIsCreateOnlyAndStdoutIsClean(t *testing.T) {
	evidence := testEvidence(t, []Sample{{
		NSPerOp: 100, BytesPerOp: 200, AllocsPerOp: 10, ArtifactBytes: 777,
	}})
	payload := mustMarshalEvidence(t, evidence)
	path := filepath.Join(t.TempDir(), "candidate.json")
	if err := writeEvidence(path, payload, nil); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	if err := writeEvidence(path, payload, nil); err == nil {
		t.Fatal("writeEvidence overwrote existing evidence")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("stored evidence bytes changed")
	}
	var stdout bytes.Buffer
	if err := writeEvidence("-", payload, &stdout); err != nil {
		t.Fatalf("write stdout evidence: %v", err)
	}
	if !bytes.Equal(stdout.Bytes(), payload) {
		t.Fatal("stdout evidence bytes changed")
	}
}

func TestCheckedBaselineIsCanonicalAndMatchesSources(t *testing.T) {
	path := filepath.Join("..", "testdata", "compare_fdbench6147_performance_baseline.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checked baseline: %v", err)
	}
	evidence, err := decodeEvidence(payload)
	if err != nil {
		t.Fatalf("decode checked baseline: %v", err)
	}
	if evidence.Comparison != nil || evidence.Protocol.SampleCount != 12 ||
		evidence.Summary.ArtifactBytes != 23462253 || evidence.Summary.AllocsPerOp.Median != 289875 {
		t.Fatalf("checked baseline identity = %+v", evidence)
	}
	wantDigest := digestSourceFiles(t, filepath.Join("..", "..", ".."), evidence.Source.Files)
	if evidence.Source.SHA256 != wantDigest {
		t.Fatalf("checked source digest = %s, want %s", evidence.Source.SHA256, wantDigest)
	}
}

func TestRunProducesCanonicalComparisonAndDoesNotOverwrite(t *testing.T) {
	baseline := testEvidence(t, []Sample{
		{NSPerOp: 100, BytesPerOp: 200, AllocsPerOp: 10, ArtifactBytes: 777},
		{NSPerOp: 110, BytesPerOp: 210, AllocsPerOp: 10, ArtifactBytes: 777},
		{NSPerOp: 120, BytesPerOp: 220, AllocsPerOp: 11, ArtifactBytes: 777},
	})
	directory := t.TempDir()
	baselinePath := filepath.Join(directory, "baseline.json")
	if err := os.WriteFile(baselinePath, mustMarshalEvidence(t, baseline), 0o644); err != nil {
		t.Fatalf("write baseline fixture: %v", err)
	}
	rawPath := filepath.Join(directory, "raw.txt")
	raw := strings.Join([]string{
		"cpu: test CPU",
		"BenchmarkCompareFDBench6147-8 1 1000 ns/op 777 artifact-bytes 2000 B/op 9 allocs/op",
		"BenchmarkCompareFDBench6147-8 1 1100 ns/op 777 artifact-bytes 2100 B/op 10 allocs/op",
		"BenchmarkCompareFDBench6147-8 1 1200 ns/op 777 artifact-bytes 2200 B/op 10 allocs/op",
	}, "\n")
	if err := os.WriteFile(rawPath, []byte(raw), 0o644); err != nil {
		t.Fatalf("write raw fixture: %v", err)
	}
	outputPath := filepath.Join(directory, "candidate.json")
	arguments := testRunArguments(baselinePath, rawPath, outputPath)
	var stdout, stderr bytes.Buffer
	if code := run(arguments, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "reportable=true accepted=true") {
		t.Fatalf("status = %q", stdout.String())
	}
	payload, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	candidate, err := decodeEvidence(payload)
	if err != nil {
		t.Fatalf("decode candidate: %v", err)
	}
	if candidate.Comparison == nil || !candidate.Comparison.Accepted ||
		candidate.Comparison.Timing.Status != "descriptive_only" {
		t.Fatalf("candidate comparison = %+v", candidate.Comparison)
	}
	if code := run(arguments, &stdout, &stderr); code != 2 {
		t.Fatalf("overwrite exit = %d, want 2", code)
	}

	arguments = testRunArguments(baselinePath, rawPath, "-")
	stdout.Reset()
	stderr.Reset()
	if code := run(arguments, &stdout, &stderr); code != 0 {
		t.Fatalf("stdout run exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := decodeEvidence(stdout.Bytes()); err != nil {
		t.Fatalf("stdout was not clean evidence JSON: %v", err)
	}
	if !strings.Contains(stderr.String(), "reportable=true accepted=true") {
		t.Fatalf("stderr status = %q", stderr.String())
	}
}

func testEvidence(t *testing.T, samples []Sample) Evidence {
	t.Helper()
	summary, err := summarize(samples)
	if err != nil {
		t.Fatalf("summarize test evidence: %v", err)
	}
	return Evidence{
		Version:   evidenceVersion,
		Benchmark: benchmarkName,
		Protocol: Protocol{
			GoVersion: "go1.25.0", GOOS: "linux", GOARCH: "amd64", CPU: "test CPU",
			GOMAXPROCS: 1, Affinity: "8", Benchtime: "1x", WarmupCount: 1,
			SampleCount: len(samples),
		},
		Source: Source{
			Revision: strings.Repeat("b", 40), SHA256: strings.Repeat("a", 64),
			Files: []string{"a.go", "b.go"},
		},
		Samples: slices.Clone(samples),
		Summary: summary,
	}
}

func cloneEvidence(evidence Evidence) Evidence {
	clone := evidence
	clone.Source.Files = slices.Clone(evidence.Source.Files)
	clone.Samples = slices.Clone(evidence.Samples)
	if evidence.Comparison != nil {
		comparison := *evidence.Comparison
		clone.Comparison = &comparison
	}
	return clone
}

func withSamples(t *testing.T, evidence Evidence, samples []Sample) Evidence {
	t.Helper()
	clone := cloneEvidence(evidence)
	clone.Samples = slices.Clone(samples)
	clone.Protocol.SampleCount = len(samples)
	var err error
	clone.Summary, err = summarize(samples)
	if err != nil {
		t.Fatalf("summarize candidate: %v", err)
	}
	return clone
}

func mustMarshalEvidence(t *testing.T, evidence Evidence) []byte {
	t.Helper()
	payload, err := marshalEvidence(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	return payload
}

func digestSourceFiles(t *testing.T, root string, files []string) string {
	t.Helper()
	outer := sha256.New()
	for _, file := range files {
		payload, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Fatalf("read source digest input %s: %v", file, err)
		}
		inner := sha256.Sum256(payload)
		fmt.Fprintf(outer, "%s  %s\n", hex.EncodeToString(inner[:]), file)
	}
	return hex.EncodeToString(outer.Sum(nil))
}

func testRunArguments(baselinePath, rawPath, outputPath string) []string {
	return []string{
		"-baseline", baselinePath,
		"-benchmark-output", rawPath,
		"-output", outputPath,
		"-go-version", "go1.25.0",
		"-goos", "linux",
		"-goarch", "amd64",
		"-cpu", "test CPU",
		"-gomaxprocs", "1",
		"-affinity", "8",
		"-benchtime", "1x",
		"-warmups", "1",
		"-samples", "3",
		"-source-revision", strings.Repeat("c", 40),
		"-source-sha256", strings.Repeat("d", 64),
		"-source-files", "b.go,a.go",
	}
}
