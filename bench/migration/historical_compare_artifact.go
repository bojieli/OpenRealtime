package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Verify reconstructs every derived field from the embedded manifest,
// owner-accepted registry, and candidate attempts. This prevents a retained
// comparison from becoming self-authenticating merely because its forged
// decision fields were included in ReportID.
func (report HistoricalComparisonReport) Verify() error {
	if report.Version != HistoricalComparisonReportVersion {
		return fmt.Errorf("historical comparison report version must be %d, got %d",
			HistoricalComparisonReportVersion, report.Version)
	}
	if report.Manifest.ID() != report.ManifestID {
		return errors.New("historical comparison report manifest digest does not match its manifest")
	}
	if report.Registry.RegistryID != report.RegistryID {
		return errors.New("historical comparison report registry ID does not match its registry")
	}
	if !validSHA256(report.ReportID) || historicalComparisonDigest(report) != report.ReportID {
		return errors.New("historical comparison report digest does not match its content")
	}
	rebuilt := CompareCandidateToHistorical(report.Manifest, report.Registry, report.Candidates)
	if canonicalJSON(rebuilt) != canonicalJSON(report) {
		return errors.New(
			"historical comparison report does not match its retained candidate attempts and registry")
	}
	return nil
}

// ArtifactSHA256 identifies the exact canonical JSON bytes returned by
// Marshal. It is distinct from ReportID, which identifies the canonical
// report record with its own ID field cleared.
func (report HistoricalComparisonReport) ArtifactSHA256() string {
	payload, err := historicalComparisonArtifactPayload(report)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// Marshal returns stable, indented JSON only after reconstructive
// verification succeeds.
func (report HistoricalComparisonReport) Marshal() ([]byte, error) {
	if err := report.Verify(); err != nil {
		return nil, err
	}
	return historicalComparisonArtifactPayload(report)
}

func historicalComparisonArtifactPayload(report HistoricalComparisonReport) ([]byte, error) {
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > MaxReportArtifactBytes {
		return nil, fmt.Errorf("historical comparison report artifact exceeds %d bytes",
			MaxReportArtifactBytes)
	}
	var roundTrip HistoricalComparisonReport
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		return nil, fmt.Errorf("round-trip historical comparison report artifact: %w", err)
	}
	if !reflect.DeepEqual(roundTrip, report) {
		return nil, errors.New(
			"historical comparison report cannot be represented losslessly as canonical JSON")
	}
	return payload, nil
}

// DecodeHistoricalComparisonReport reads exactly one canonical JSON report,
// rejects unknown fields and trailing values, and reconstructively verifies
// every comparison and gate.
func DecodeHistoricalComparisonReport(reader io.Reader) (HistoricalComparisonReport, error) {
	if reader == nil {
		return HistoricalComparisonReport{},
			errors.New("decode historical comparison report: reader is nil")
	}
	limited := &io.LimitedReader{R: reader, N: MaxReportArtifactBytes + 1}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return HistoricalComparisonReport{},
			fmt.Errorf("read historical comparison report: %w", err)
	}
	if int64(len(payload)) > MaxReportArtifactBytes {
		return HistoricalComparisonReport{}, fmt.Errorf(
			"decode historical comparison report: artifact exceeds %d bytes",
			MaxReportArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var report HistoricalComparisonReport
	if err := decoder.Decode(&report); err != nil {
		return HistoricalComparisonReport{},
			fmt.Errorf("decode historical comparison report: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return HistoricalComparisonReport{},
				errors.New("decode historical comparison report: trailing JSON value")
		}
		return HistoricalComparisonReport{},
			fmt.Errorf("decode historical comparison report trailing content: %w", err)
	}
	if err := report.Verify(); err != nil {
		return HistoricalComparisonReport{}, err
	}
	canonical, err := historicalComparisonArtifactPayload(report)
	if err != nil {
		return HistoricalComparisonReport{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return HistoricalComparisonReport{}, errors.New(
			"decode historical comparison report: artifact bytes are not canonical")
	}
	return report, nil
}

// Write atomically archives a verified historical comparison and refuses to
// replace an existing path. A failed or regressed candidate remains immutable
// evidence; a subsequent rerun must use a distinct campaign run and path.
func (report HistoricalComparisonReport) Write(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("a historical comparison report needs a path")
	}
	payload, err := report.Marshal()
	if err != nil {
		return err
	}
	return writeArtifact(path, payload)
}
