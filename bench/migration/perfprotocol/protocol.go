package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

const (
	evidenceVersion = 1
	benchmarkName   = "BenchmarkCompareFDBench6147"
)

type Evidence struct {
	Version    int         `json:"version"`
	Benchmark  string      `json:"benchmark"`
	Protocol   Protocol    `json:"protocol"`
	Source     Source      `json:"source"`
	Samples    []Sample    `json:"samples"`
	Summary    Summary     `json:"summary"`
	Comparison *Comparison `json:"comparison,omitempty"`
}

type Protocol struct {
	GoVersion   string `json:"go_version"`
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
	CPU         string `json:"cpu"`
	GOMAXPROCS  int    `json:"gomaxprocs"`
	Affinity    string `json:"affinity"`
	Benchtime   string `json:"benchtime"`
	WarmupCount int    `json:"warmup_count"`
	SampleCount int    `json:"sample_count"`
}

type Source struct {
	Revision string   `json:"revision"`
	Modified bool     `json:"modified"`
	SHA256   string   `json:"sha256"`
	Files    []string `json:"files"`
}

type Sample struct {
	NSPerOp       uint64 `json:"ns_per_op"`
	BytesPerOp    uint64 `json:"bytes_per_op"`
	AllocsPerOp   uint64 `json:"allocs_per_op"`
	ArtifactBytes uint64 `json:"artifact_bytes"`
}

type MetricSummary struct {
	Minimum uint64  `json:"minimum"`
	Median  float64 `json:"median"`
	Mean    float64 `json:"mean"`
	Maximum uint64  `json:"maximum"`
}

type Summary struct {
	TimeNS        MetricSummary `json:"time_ns"`
	BytesPerOp    MetricSummary `json:"bytes_per_op"`
	AllocsPerOp   MetricSummary `json:"allocs_per_op"`
	ArtifactBytes uint64        `json:"artifact_bytes"`
}

type Comparison struct {
	BaselineSHA256 string             `json:"baseline_sha256"`
	Reportable     bool               `json:"reportable"`
	Accepted       bool               `json:"accepted"`
	Artifact       DeterministicCheck `json:"artifact"`
	Allocations    DeterministicCheck `json:"allocations"`
	Bytes          DescriptiveCheck   `json:"bytes"`
	Timing         DescriptiveCheck   `json:"timing"`
}

type DeterministicCheck struct {
	Status    string  `json:"status"`
	Rule      string  `json:"rule"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Reason    string  `json:"reason,omitempty"`
}

type DescriptiveCheck struct {
	Status          string  `json:"status"`
	BaselineMedian  float64 `json:"baseline_median"`
	CandidateMedian float64 `json:"candidate_median"`
	CandidateRatio  float64 `json:"candidate_ratio,omitempty"`
	Reason          string  `json:"reason"`
}

func parseBenchmarkOutput(reader io.Reader) ([]Sample, error) {
	if reader == nil {
		return nil, errors.New("benchmark output reader is nil")
	}
	var samples []Sample
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 64<<10)
	scanner.Buffer(buffer, 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !benchmarkField(fields[0]) {
			continue
		}
		if len(fields) != 10 || fields[1] != "1" || fields[3] != "ns/op" ||
			fields[5] != "artifact-bytes" || fields[7] != "B/op" || fields[9] != "allocs/op" {
			return nil, fmt.Errorf("malformed %s output line %q", benchmarkName, scanner.Text())
		}
		values := make([]uint64, 4)
		for index, fieldIndex := range []int{2, 6, 8, 4} {
			value, err := strconv.ParseUint(fields[fieldIndex], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse %s metric %q: %w", benchmarkName, fields[fieldIndex], err)
			}
			values[index] = value
		}
		samples = append(samples, Sample{
			NSPerOp: values[0], BytesPerOp: values[1], AllocsPerOp: values[2], ArtifactBytes: values[3],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan benchmark output: %w", err)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("benchmark output contains no %s samples", benchmarkName)
	}
	return samples, nil
}

func benchmarkField(value string) bool {
	if value == benchmarkName {
		return true
	}
	prefix := benchmarkName + "-"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := strconv.Atoi(strings.TrimPrefix(value, prefix))
	return err == nil
}

func summarize(samples []Sample) (Summary, error) {
	if len(samples) == 0 {
		return Summary{}, errors.New("cannot summarize an empty sample set")
	}
	times := make([]uint64, len(samples))
	bytesPerOp := make([]uint64, len(samples))
	allocs := make([]uint64, len(samples))
	artifactBytes := samples[0].ArtifactBytes
	if artifactBytes == 0 {
		return Summary{}, errors.New("artifact byte count must be positive")
	}
	for index, sample := range samples {
		if sample.NSPerOp == 0 || sample.BytesPerOp == 0 || sample.AllocsPerOp == 0 {
			return Summary{}, fmt.Errorf("sample %d contains a zero benchmark metric", index)
		}
		if sample.ArtifactBytes != artifactBytes {
			return Summary{}, fmt.Errorf(
				"sample %d artifact bytes are %d, want invariant %d",
				index, sample.ArtifactBytes, artifactBytes)
		}
		times[index], bytesPerOp[index], allocs[index] =
			sample.NSPerOp, sample.BytesPerOp, sample.AllocsPerOp
	}
	return Summary{
		TimeNS: metricSummary(times), BytesPerOp: metricSummary(bytesPerOp),
		AllocsPerOp: metricSummary(allocs), ArtifactBytes: artifactBytes,
	}, nil
}

func metricSummary(values []uint64) MetricSummary {
	sorted := append([]uint64(nil), values...)
	slices.Sort(sorted)
	median := float64(sorted[len(sorted)/2])
	if len(sorted)%2 == 0 {
		median = float64(sorted[len(sorted)/2-1])/2 + float64(sorted[len(sorted)/2])/2
	}
	mean := 0.0
	for index, value := range values {
		mean += (float64(value) - mean) / float64(index+1)
	}
	return MetricSummary{
		Minimum: sorted[0], Median: median, Mean: mean, Maximum: sorted[len(sorted)-1],
	}
}

func (evidence Evidence) validate() error {
	if evidence.Version != evidenceVersion {
		return fmt.Errorf("evidence version must be %d", evidenceVersion)
	}
	if evidence.Benchmark != benchmarkName {
		return fmt.Errorf("benchmark must be %q", benchmarkName)
	}
	protocol := evidence.Protocol
	if strings.TrimSpace(protocol.GoVersion) == "" || strings.TrimSpace(protocol.GOOS) == "" ||
		strings.TrimSpace(protocol.GOARCH) == "" || strings.TrimSpace(protocol.CPU) == "" ||
		strings.TrimSpace(protocol.Affinity) == "" || strings.TrimSpace(protocol.Benchtime) == "" {
		return errors.New("performance protocol identity is incomplete")
	}
	if protocol.GOMAXPROCS <= 0 || protocol.WarmupCount < 0 || protocol.SampleCount <= 0 {
		return errors.New("performance protocol counts are invalid")
	}
	if protocol.SampleCount != len(evidence.Samples) {
		return fmt.Errorf("protocol declares %d samples, evidence contains %d",
			protocol.SampleCount, len(evidence.Samples))
	}
	if strings.TrimSpace(evidence.Source.Revision) == "" || !validSHA256(evidence.Source.SHA256) ||
		len(evidence.Source.Files) == 0 {
		return errors.New("source identity is incomplete")
	}
	for index, file := range evidence.Source.Files {
		if strings.TrimSpace(file) == "" {
			return errors.New("source identity contains an empty file")
		}
		if index > 0 && evidence.Source.Files[index-1] >= file {
			return errors.New("source identity files must be unique and sorted")
		}
	}
	want, err := summarize(evidence.Samples)
	if err != nil {
		return err
	}
	if evidence.Summary != want {
		return errors.New("stored summary is not derived from raw samples")
	}
	if evidence.Comparison != nil {
		comparison := evidence.Comparison
		if !validSHA256(comparison.BaselineSHA256) {
			return errors.New("comparison baseline digest is invalid")
		}
		for _, item := range []struct {
			name  string
			check DeterministicCheck
		}{
			{name: "artifact", check: comparison.Artifact},
			{name: "allocations", check: comparison.Allocations},
		} {
			name, check := item.name, item.check
			validStatus := check.Status == "pass" || check.Status == "fail"
			if name == "allocations" && check.Status == "not_comparable" {
				validStatus = true
			}
			if !validStatus {
				return fmt.Errorf("comparison %s status %q is invalid", name, check.Status)
			}
			if strings.TrimSpace(check.Rule) == "" || !finite(check.Baseline) ||
				!finite(check.Candidate) || check.Baseline < 0 || check.Candidate < 0 {
				return fmt.Errorf("comparison %s values are invalid", name)
			}
		}
		for _, item := range []struct {
			name  string
			check DescriptiveCheck
		}{
			{name: "bytes", check: comparison.Bytes},
			{name: "timing", check: comparison.Timing},
		} {
			name, check := item.name, item.check
			if check.Status != "descriptive_only" && check.Status != "not_comparable" {
				return fmt.Errorf("comparison %s status %q is invalid", name, check.Status)
			}
			if strings.TrimSpace(check.Reason) == "" || !finite(check.BaselineMedian) ||
				!finite(check.CandidateMedian) || !finite(check.CandidateRatio) ||
				check.BaselineMedian < 0 || check.CandidateMedian < 0 || check.CandidateRatio < 0 {
				return fmt.Errorf("comparison %s values are invalid", name)
			}
		}
		if comparison.Artifact.Candidate != float64(evidence.Summary.ArtifactBytes) ||
			comparison.Allocations.Candidate != evidence.Summary.AllocsPerOp.Median ||
			comparison.Bytes.CandidateMedian != evidence.Summary.BytesPerOp.Median ||
			comparison.Timing.CandidateMedian != evidence.Summary.TimeNS.Median {
			return errors.New("comparison candidate values are not derived from the stored summary")
		}
		for _, item := range []struct {
			name  string
			check DescriptiveCheck
		}{
			{name: "bytes", check: comparison.Bytes},
			{name: "timing", check: comparison.Timing},
		} {
			wantRatio := 0.0
			if item.check.BaselineMedian > 0 {
				wantRatio = item.check.CandidateMedian / item.check.BaselineMedian
			}
			if item.check.CandidateRatio != wantRatio {
				return fmt.Errorf("comparison %s ratio is not derived from its medians", item.name)
			}
		}
		wantReportable := comparison.Allocations.Status != "not_comparable"
		if comparison.Reportable != wantReportable {
			return errors.New("comparison reportability is inconsistent with deterministic gates")
		}
		wantAccepted := comparison.Reportable && comparison.Artifact.Status == "pass" &&
			comparison.Allocations.Status == "pass"
		if comparison.Accepted != wantAccepted {
			return errors.New("comparison acceptance is inconsistent with deterministic gates")
		}
	}
	return nil
}

func compareEvidence(baseline, candidate Evidence, baselineBytes []byte) Comparison {
	digest := sha256.Sum256(baselineBytes)
	result := Comparison{
		BaselineSHA256: hex.EncodeToString(digest[:]),
		Artifact: DeterministicCheck{
			Rule:      "candidate artifact bytes must equal the checked baseline",
			Baseline:  float64(baseline.Summary.ArtifactBytes),
			Candidate: float64(candidate.Summary.ArtifactBytes),
		},
		Allocations: DeterministicCheck{
			Rule:      "candidate median allocs/op must not exceed the checked baseline median under the same deterministic protocol",
			Baseline:  baseline.Summary.AllocsPerOp.Median,
			Candidate: candidate.Summary.AllocsPerOp.Median,
		},
		Bytes: descriptive("bytes/op includes GC-cycle accounting and is descriptive only",
			baseline.Summary.BytesPerOp.Median, candidate.Summary.BytesPerOp.Median),
		Timing: descriptive("wall-clock timing is descriptive only; no release threshold is inferred from these samples",
			baseline.Summary.TimeNS.Median, candidate.Summary.TimeNS.Median),
	}
	if baseline.Summary.ArtifactBytes == candidate.Summary.ArtifactBytes {
		result.Artifact.Status = "pass"
	} else {
		result.Artifact.Status = "fail"
		result.Artifact.Reason = "artifact size changed"
	}
	deterministicMatch, deterministicReason := deterministicProtocolMatch(
		baseline.Protocol, candidate.Protocol)
	if deterministicMatch && !slices.Equal(baseline.Source.Files, candidate.Source.Files) {
		deterministicMatch = false
		deterministicReason = "benchmark source file scope differs from the checked baseline"
	}
	if !deterministicMatch {
		result.Allocations.Status = "not_comparable"
		result.Allocations.Reason = deterministicReason
	} else if candidate.Summary.AllocsPerOp.Median <= baseline.Summary.AllocsPerOp.Median {
		result.Allocations.Status = "pass"
	} else {
		result.Allocations.Status = "fail"
		result.Allocations.Reason = "median allocations increased"
	}
	timingMatch, timingReason := timingProtocolMatch(baseline.Protocol, candidate.Protocol)
	if timingMatch && !slices.Equal(baseline.Source.Files, candidate.Source.Files) {
		timingMatch = false
		timingReason = "benchmark source file scope differs from the checked baseline"
	}
	if timingMatch {
		result.Timing.Status = "descriptive_only"
	} else {
		result.Timing.Status = "not_comparable"
		result.Timing.Reason = timingReason
	}
	result.Bytes.Status = "descriptive_only"
	result.Reportable = deterministicMatch
	result.Accepted = result.Reportable && result.Artifact.Status == "pass" &&
		result.Allocations.Status == "pass"
	return result
}

func descriptive(reason string, baseline, candidate float64) DescriptiveCheck {
	result := DescriptiveCheck{
		BaselineMedian: baseline, CandidateMedian: candidate, Reason: reason,
	}
	if baseline > 0 {
		result.CandidateRatio = candidate / baseline
	}
	return result
}

func deterministicProtocolMatch(baseline, candidate Protocol) (bool, string) {
	if baseline.GoVersion != candidate.GoVersion || baseline.GOOS != candidate.GOOS ||
		baseline.GOARCH != candidate.GOARCH || baseline.GOMAXPROCS != candidate.GOMAXPROCS ||
		baseline.Benchtime != candidate.Benchtime || baseline.WarmupCount != candidate.WarmupCount ||
		baseline.SampleCount != candidate.SampleCount {
		return false, "toolchain, platform, GOMAXPROCS, benchtime, warmup, or sample count differs"
	}
	return true, ""
}

func timingProtocolMatch(baseline, candidate Protocol) (bool, string) {
	if matched, reason := deterministicProtocolMatch(baseline, candidate); !matched {
		return false, reason
	}
	if baseline.CPU != candidate.CPU || baseline.Affinity != candidate.Affinity {
		return false, "CPU or affinity differs from the checked baseline"
	}
	return true, ""
}

func decodeEvidence(payload []byte) (Evidence, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var evidence Evidence
	if err := decoder.Decode(&evidence); err != nil {
		return Evidence{}, fmt.Errorf("decode performance evidence: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Evidence{}, errors.New("decode performance evidence: trailing JSON value")
		}
		return Evidence{}, fmt.Errorf("decode performance evidence trailing content: %w", err)
	}
	if err := evidence.validate(); err != nil {
		return Evidence{}, err
	}
	canonical, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return Evidence{}, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(payload, canonical) {
		return Evidence{}, errors.New("performance evidence bytes are not canonical JSON")
	}
	return evidence, nil
}

func marshalEvidence(evidence Evidence) ([]byte, error) {
	if err := evidence.validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func writeEvidence(path string, payload []byte, stdout io.Writer) error {
	if path == "-" {
		_, err := stdout.Write(payload)
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(payload)
	if writeErr == nil && written != len(payload) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if character := value[index]; (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
