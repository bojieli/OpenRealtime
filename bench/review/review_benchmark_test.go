package review

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkSensitiveMatcherScan(b *testing.B) {
	values := make([][]byte, maximumSensitiveValues)
	for index := range values {
		values[index] = []byte(fmt.Sprintf("review-sensitive-value-%04d-credential", index))
	}
	matcher, err := newSensitiveMatcher(values)
	if err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte(`{"safe":"retained benchmark evidence"}`), 1<<15)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if matcher.contains(payload) {
			b.Fatal("safe benchmark payload matched a declared value")
		}
	}
}

func BenchmarkPreparedRequestValidateSeal(b *testing.B) {
	root := b.TempDir()
	mediaDirectory := filepath.Join(root, "media")
	if err := os.Mkdir(mediaDirectory, 0o700); err != nil {
		b.Fatal(err)
	}
	payload := benchmarkWAVPayload(4 << 20)
	path := filepath.Join(mediaDirectory, "review.wav")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		b.Fatal(err)
	}
	prepared, err := Prepare(Request{
		AttemptID: "benchmark/reviewer/1", Suite: "benchmark", Case: "reviewer", Trial: 1,
		RootDirectory: root, Context: json.RawMessage(`{"outcome":"pass","score":1}`),
		Media: []Media{{
			Kind: "audio", Role: "room_and_agent", Path: "media/review.wav",
			SHA256: digest(payload), MediaType: "audio/wav",
		}},
		SensitiveValues: []string{"benchmark-sensitive-credential"},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if err := prepared.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNormalizeAssessmentAtCardinalityLimit(b *testing.B) {
	assessment := Assessment{
		MediaUsable: true, ObservedOutcome: "pass", AgreesWithDeterministic: true,
		Confidence: 1, Summary: "bounded benchmark assessment",
		SignificantProblems: make([]Finding, 128), MinorObservations: make([]Finding, 128),
		Limitations: make([]string, 128),
	}
	for index := range 128 {
		finding := Finding{
			Category: fmt.Sprintf("observation_%d", index),
			Evidence: "observable retained evidence", Impact: "bounded review impact",
		}
		assessment.SignificantProblems[index] = finding
		assessment.MinorObservations[index] = finding
		assessment.Limitations[index] = fmt.Sprintf("limitation %d", index)
	}
	payload, err := json.Marshal(assessment)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if _, _, err := normalizeAssessment(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateAssessmentTimestampEvidenceAtCardinalityLimit(b *testing.B) {
	timestamp := int64(12_345)
	finding := Finding{
		Category: "timed_observation", StartMS: &timestamp, EndMS: &timestamp,
		Evidence: strings.Repeat("observable retained evidence ", 140) + "at 12345 ms",
		Impact:   "bounded review impact",
	}
	assessment := Assessment{
		SignificantProblems: make([]Finding, 128), MinorObservations: make([]Finding, 128),
	}
	for index := range 128 {
		assessment.SignificantProblems[index] = finding
		assessment.MinorObservations[index] = finding
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(finding.Evidence) * 256))
	b.ResetTimer()
	for range b.N {
		if err := validateAssessmentTimestampMaximum(assessment, timestamp); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkWAVPayload(dataBytes int) []byte {
	if dataBytes < 2 {
		dataBytes = 2
	}
	if dataBytes%2 != 0 {
		dataBytes++
	}
	payload := make([]byte, 44+dataBytes)
	copy(payload[0:4], "RIFF")
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	copy(payload[8:12], "WAVE")
	copy(payload[12:16], "fmt ")
	binary.LittleEndian.PutUint32(payload[16:20], 16)
	binary.LittleEndian.PutUint16(payload[20:22], 1)
	binary.LittleEndian.PutUint16(payload[22:24], 1)
	binary.LittleEndian.PutUint32(payload[24:28], 24_000)
	binary.LittleEndian.PutUint32(payload[28:32], 48_000)
	binary.LittleEndian.PutUint16(payload[32:34], 2)
	binary.LittleEndian.PutUint16(payload[34:36], 16)
	copy(payload[36:40], "data")
	binary.LittleEndian.PutUint32(payload[40:44], uint32(dataBytes))
	return payload
}
