package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// MaxReportArtifactBytes bounds strict decoding. It is comfortably above a
// complete two-arm FD-Bench report while preventing an untrusted reader from
// supplying an unbounded JSON document.
const MaxReportArtifactBytes int64 = 128 << 20

// Verify checks artifact integrity against an empty campaign history. An
// initial report, or a refusal that was actually produced with no supplied
// predecessors, verifies locally. A report produced from retained history
// must use VerifyWithHistory; this prevents invented lineage findings from
// becoming self-authenticating merely because their text was included in the
// report digest.
func (report Report) Verify() error {
	return report.verifyWithHistory(nil)
}

// verifyLocal checks content-derived state while treating lineage findings as
// external claims. It is deliberately unexported: callers must prove those
// claims through Verify or VerifyWithHistory. Lineage reconstruction uses this
// helper before independently checking the complete supplied history.
func (report Report) verifyLocal() error {
	if report.Version != ReportVersion {
		return fmt.Errorf("migration report version must be %d, got %d", ReportVersion, report.Version)
	}
	if report.Manifest.ID() != report.ManifestID {
		return errors.New("migration report manifest digest does not match its manifest")
	}
	if report.ReportID == "" || reportDigest(report) != report.ReportID {
		return errors.New("migration report digest does not match its content")
	}
	var baseline, candidate []Attempt
	for _, observed := range report.Attempts {
		switch observed.Arm {
		case ArmBaseline:
			baseline = append(baseline, observed.Attempt)
		case ArmCandidate:
			candidate = append(candidate, observed.Attempt)
		default:
			return fmt.Errorf("migration report contains unknown arm %q", observed.Arm)
		}
	}
	rebuilt := compareCore(report.Manifest, baseline, candidate)
	rebuilt.SuppliedHistory = canonicalReportReferences(report.SuppliedHistory)
	for _, finding := range report.Refusals {
		if strings.HasPrefix(finding.Code, "lineage.") {
			rebuilt.Refusals = append(rebuilt.Refusals, finding)
		}
	}
	if hasExternalLineageFinding(report.Refusals) {
		sortFindings(rebuilt.Refusals)
		rebuilt.Reportable = false
		rebuilt.Accepted = false
		rebuilt.Matched = make([]MatchedAttempt, 0)
		rebuilt.Comparisons = make([]SuiteComparison, 0)
	}
	applyCampaignGate(&rebuilt)
	rebuilt.ReportID = reportDigest(rebuilt)
	if canonicalJSON(rebuilt) != canonicalJSON(report) {
		return errors.New("migration report derived comparison does not match its retained attempts")
	}
	return nil
}

// VerifyWithHistory additionally proves every predeclared predecessor,
// diagnosed failure, and lineage edge against supplied immutable reports.
func (report Report) VerifyWithHistory(history []Report) error {
	return report.verifyWithHistory(history)
}

func (report Report) verifyWithHistory(history []Report) error {
	if err := report.verifyLocal(); err != nil {
		return err
	}
	if len(report.Manifest.Campaign.Predecessors) == 0 &&
		len(report.SuppliedHistory) == 0 && !hasExternalLineageFinding(report.Refusals) &&
		len(history) == 0 {
		return nil
	}
	var baseline, candidate []Attempt
	for _, observed := range report.Attempts {
		switch observed.Arm {
		case ArmBaseline:
			baseline = append(baseline, observed.Attempt)
		case ArmCandidate:
			candidate = append(candidate, observed.Attempt)
		default:
			return fmt.Errorf("migration report contains unknown arm %q", observed.Arm)
		}
	}
	rebuilt := CompareWithHistory(report.Manifest, baseline, candidate, history)
	if canonicalJSON(rebuilt) != canonicalJSON(report) {
		return errors.New("migration report does not match the supplied campaign history")
	}
	return nil
}

func hasExternalLineageFinding(findings []Finding) bool {
	for _, finding := range findings {
		if strings.HasPrefix(finding.Code, "lineage.") {
			return true
		}
	}
	return false
}

// ArtifactSHA256 identifies the exact indented JSON bytes Write archives.
// It is separate from ReportID, which hashes the canonical report record with
// its own ID field empty.
func (report Report) ArtifactSHA256() string {
	payload, err := artifactPayload(report)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// Marshal returns stable, indented JSON only for a verified report.
func (report Report) Marshal() ([]byte, error) {
	if err := report.Verify(); err != nil {
		return nil, err
	}
	return artifactPayload(report)
}

// MarshalWithHistory returns canonical archived JSON after proving the report
// against its complete transitive campaign history.
func (report Report) MarshalWithHistory(history []Report) ([]byte, error) {
	if err := report.VerifyWithHistory(history); err != nil {
		return nil, err
	}
	return artifactPayload(report)
}

func artifactPayload(report Report) ([]byte, error) {
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > MaxReportArtifactBytes {
		return nil, fmt.Errorf("migration report artifact exceeds %d bytes", MaxReportArtifactBytes)
	}
	var roundTrip Report
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		return nil, fmt.Errorf("round-trip migration report artifact: %w", err)
	}
	if !reflect.DeepEqual(roundTrip, report) {
		return nil, errors.New(
			"migration report cannot be represented losslessly as canonical JSON")
	}
	return payload, nil
}

// Decode reads one strict JSON report and verifies it against an empty
// campaign history. Use DecodeWithHistory for a report produced with retained
// predecessors.
func Decode(reader io.Reader) (Report, error) {
	return decode(reader, nil, false)
}

// DecodeWithHistory reads one strict JSON report and proves its complete
// campaign lineage against the supplied predecessor reports.
func DecodeWithHistory(reader io.Reader, history []Report) (Report, error) {
	return decode(reader, history, true)
}

func decode(reader io.Reader, history []Report, verifyHistory bool) (Report, error) {
	if reader == nil {
		return Report{}, errors.New("decode migration report: reader is nil")
	}
	limited := &io.LimitedReader{R: reader, N: MaxReportArtifactBytes + 1}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return Report{}, fmt.Errorf("read migration report: %w", err)
	}
	if int64(len(payload)) > MaxReportArtifactBytes {
		return Report{}, fmt.Errorf("decode migration report: artifact exceeds %d bytes",
			MaxReportArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode migration report: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Report{}, errors.New("decode migration report: trailing JSON value")
		}
		return Report{}, fmt.Errorf("decode migration report trailing content: %w", err)
	}
	if verifyHistory {
		if err := report.VerifyWithHistory(history); err != nil {
			return Report{}, err
		}
	} else if err := report.Verify(); err != nil {
		return Report{}, err
	}
	canonical, err := artifactPayload(report)
	if err != nil {
		return Report{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return Report{}, errors.New(
			"decode migration report: artifact bytes are not the canonical archived JSON")
	}
	return report, nil
}

// Write atomically archives a verified report as JSON. It refuses to replace
// an existing path: a failed report is evidence and a later rerun receives a
// new campaign run ID and artifact rather than overwriting it.
func (report Report) Write(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("a migration report needs a path")
	}
	payload, err := report.Marshal()
	if err != nil {
		return err
	}
	return writeArtifact(path, payload)
}

// WriteWithHistory atomically archives a history-bearing report only after
// proving the complete supplied campaign lineage. Like Write, it is
// create-only and never replaces an earlier failed or regressed artifact.
func (report Report) WriteWithHistory(path string, history []Report) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("a migration report needs a path")
	}
	payload, err := report.MarshalWithHistory(history)
	if err != nil {
		return err
	}
	return writeArtifact(path, payload)
}

func writeArtifact(path string, payload []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".migration-report-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return err
	}
	if _, err := io.Copy(temporary, bytes.NewReader(payload)); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open migration report directory for sync: %w", err)
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return fmt.Errorf("sync migration report directory: %w", err)
	}
	if err := directoryHandle.Close(); err != nil {
		return fmt.Errorf("close migration report directory: %w", err)
	}
	return nil
}
