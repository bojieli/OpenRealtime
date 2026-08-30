package migration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func acceptedHistoricalComparisonFixture(t *testing.T) HistoricalComparisonReport {
	t.Helper()
	manifest := testManifest()
	baseline, candidate := testAttempts()
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	report := CompareCandidateToHistorical(manifest, registry, candidate)
	if !report.Accepted {
		t.Fatalf("fixture comparison was not accepted: %+v", report)
	}
	return report
}

func TestHistoricalComparisonReportRoundTripsCanonicalArtifact(t *testing.T) {
	report := acceptedHistoricalComparisonFixture(t)
	payload, err := report.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 || payload[len(payload)-1] != '\n' ||
		report.ArtifactSHA256() == "" {
		t.Fatalf("invalid artifact framing or digest")
	}
	decoded, err := DecodeHistoricalComparisonReport(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, report) {
		t.Fatalf("decoded report differs from source")
	}
}

func TestHistoricalComparisonReportVerifyRejectsForgedDerivedState(t *testing.T) {
	report := acceptedHistoricalComparisonFixture(t)
	tests := map[string]func(*HistoricalComparisonReport){
		"report digest": func(value *HistoricalComparisonReport) {
			value.ReportID = strings.Repeat("a", 64)
		},
		"manifest digest": func(value *HistoricalComparisonReport) {
			value.ManifestID = strings.Repeat("b", 64)
			value.ReportID = historicalComparisonDigest(*value)
		},
		"registry identity": func(value *HistoricalComparisonReport) {
			value.RegistryID = strings.Repeat("c", 64)
			value.ReportID = historicalComparisonDigest(*value)
		},
		"acceptance": func(value *HistoricalComparisonReport) {
			value.Accepted = false
			value.ReportID = historicalComparisonDigest(*value)
		},
		"gate": func(value *HistoricalComparisonReport) {
			value.Comparisons[0].Pass.Gate.Passed = false
			value.ReportID = historicalComparisonDigest(*value)
		},
		"candidate": func(value *HistoricalComparisonReport) {
			value.Candidates[0].Passed = false
			value.ReportID = historicalComparisonDigest(*value)
		},
		"registry": func(value *HistoricalComparisonReport) {
			value.Registry.Suites[0].Pass.Successes--
			value.ReportID = historicalComparisonDigest(*value)
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var forged HistoricalComparisonReport
			if err := json.Unmarshal(payload, &forged); err != nil {
				t.Fatal(err)
			}
			edit(&forged)
			if err := forged.Verify(); err == nil {
				t.Fatal("forged historical comparison verified")
			}
		})
	}
}

func TestDecodeHistoricalComparisonReportRejectsUnknownTrailingAndNoncanonicalJSON(t *testing.T) {
	report := acceptedHistoricalComparisonFixture(t)
	payload, err := report.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(payload, []byte("\n}"), []byte(",\n  \"unknown\": true\n}"), 1)
	compact, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"unknown":      unknown,
		"trailing":     append(append([]byte(nil), payload...), []byte("{}\n")...),
		"noncanonical": compact,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeHistoricalComparisonReport(bytes.NewReader(candidate)); err == nil {
				t.Fatalf("accepted %s JSON", name)
			}
		})
	}
	if _, err := DecodeHistoricalComparisonReport(nil); err == nil {
		t.Fatal("accepted nil reader")
	}
}

func TestHistoricalComparisonReportWriteIsCreateOnly(t *testing.T) {
	report := acceptedHistoricalComparisonFixture(t)
	path := filepath.Join(t.TempDir(), "nested", "historical.json")
	if err := report.Write(path); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeHistoricalComparisonReport(bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := report.Write(path); err == nil {
		t.Fatal("create-only historical report write replaced an existing artifact")
	}
	if err := report.Write(" "); err == nil {
		t.Fatal("accepted empty report path")
	}
}

func TestDecodeHistoricalComparisonReportIsSizeBounded(t *testing.T) {
	oversized := bytes.NewReader(bytes.Repeat([]byte{' '}, int(MaxReportArtifactBytes)+1))
	if _, err := DecodeHistoricalComparisonReport(oversized); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized artifact error = %v", err)
	}
}
