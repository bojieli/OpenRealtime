package graphnative

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	SourceBundleFormat        = "openrealtime.scenario-source-bundle"
	SourceBundleFormatVersion = 2
	SourceReceiptFormat       = "openrealtime.scenario-source-receipt"
	SourceReceiptVersion      = 1
	SourceManifestName        = "source-manifest.json"
	SourceArchitectureName    = "architecture-result.json"

	maximumSourceFiles      = 16_384
	maximumSourceAttempts   = 11_000
	maximumSourceManifest   = 16 << 20
	maximumSourceReceipt    = 64 << 10
	maximumSourceFileBytes  = 256 << 20
	maximumSourceTotalBytes = int64(8 << 30)
	maximumSourcePathBytes  = 4096
)

// SourceOrigin distinguishes a live endpoint run from a hermetic fixture.
// EndpointSHA256 binds the normalized endpoint without retaining credentials,
// query parameters, or a deployment hostname in the portable evidence index.
type SourceOrigin struct {
	Kind           string `json:"kind"`
	Transport      string `json:"transport"`
	EndpointSHA256 string `json:"endpoint_sha256"`
}

// SourceFile is one exact regular file below the source root. Purpose is
// semantic and stable; Path is always a flat root-relative name.
type SourceFile struct {
	Path      string `json:"path"`
	Purpose   string `json:"purpose"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type SourceSubmittedInput struct {
	Receipt SubmittedInputReceipt `json:"receipt"`
	File    SourceFile            `json:"file"`
}

// SourceAttempt cross-binds the deterministic checklist row to the exact
// result, media manifest, stereo recording, and submitted visual inputs.
// Diagnostic attempts may have a nil checklist Media receipt, but their
// retained files still remain attributable and cannot become reportable.
type SourceAttempt struct {
	Record        AttemptRecord          `json:"record"`
	Result        SourceFile             `json:"result"`
	MediaManifest *SourceFile            `json:"media_manifest,omitempty"`
	Audio         *SourceFile            `json:"audio,omitempty"`
	Submitted     []SourceSubmittedInput `json:"submitted_inputs,omitempty"`
}

// SourceManifest is published last inside the source root. Files names the
// complete pre-manifest tree, so an added, removed, swapped, or hard-linked
// artifact invalidates verification.
type SourceManifest struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	// Complete is the create-only publication commit marker. PopulationComplete
	// separately states whether every planned checklist row was attempted.
	Complete                  bool            `json:"complete"`
	PopulationComplete        bool            `json:"population_complete"`
	Suite                     string          `json:"suite"`
	Cases                     int             `json:"cases"`
	Trials                    int             `json:"trials"`
	ExpectedAttempts          int             `json:"expected_attempts"`
	Origin                    SourceOrigin    `json:"origin"`
	ChecklistFingerprint      string          `json:"checklist_fingerprint"`
	Checklist                 SourceFile      `json:"checklist"`
	ArchitectureResult        SourceFile      `json:"architecture_result"`
	ArchitectureReportable    bool            `json:"architecture_reportable"`
	ArchitectureReportability string          `json:"architecture_reportability,omitempty"`
	Attempts                  []SourceAttempt `json:"attempts"`
	FileSetSHA256             string          `json:"file_set_sha256"`
	Files                     []SourceFile    `json:"files"`
}

// SourceReceipt is portable: ReceiptSHA256 excludes Directory. A caller must
// retain it outside the mutable source root and supply it to VerifySourceBundle.
type SourceReceipt struct {
	Format             string `json:"format"`
	FormatVersion      int    `json:"format_version"`
	Directory          string `json:"directory"`
	ManifestSHA256     string `json:"manifest_sha256"`
	FileSetSHA256      string `json:"file_set_sha256"`
	ChecklistSHA256    string `json:"checklist_sha256"`
	ArchitectureSHA256 string `json:"architecture_sha256"`
	ReceiptSHA256      string `json:"receipt_sha256"`
}

type SourceBundleOptions struct {
	Directory       string
	SensitiveValues []string
}

type SourcePublication struct {
	SourceBundleOptions
	ReceiptPath        string
	Origin             SourceOrigin
	Checklist          Checklist
	ArchitectureResult json.RawMessage
	Attempts           []SourceAttempt
}

type SourceBundle struct {
	Manifest           SourceManifest
	Checklist          Checklist
	ArchitectureResult archbench.Result
	Receipt            SourceReceipt
}

// PublishSourceBundle writes the final architecture result, snapshots the
// exact existing evidence tree, publishes SourceManifestName last, reopens it
// against the expected receipt, then writes the caller-owned external receipt
// create-only. Existing paths are never replaced.
func PublishSourceBundle(
	ctx context.Context, publication SourcePublication,
) (receipt SourceReceipt, resultErr error) {
	if ctx == nil {
		return SourceReceipt{}, errors.New("publish scenario source bundle: nil context")
	}
	if err := ctx.Err(); err != nil {
		return SourceReceipt{}, err
	}
	directory, err := validateSourceDirectory(publication.Directory, true)
	if err != nil {
		return SourceReceipt{}, err
	}
	receiptPath, err := validateSourceReceiptDestination(directory, publication.ReceiptPath)
	if err != nil {
		return SourceReceipt{}, err
	}
	if err := publication.Origin.Validate(); err != nil {
		return SourceReceipt{}, err
	}
	if err := publication.Checklist.Validate(); err != nil {
		return SourceReceipt{}, fmt.Errorf("publish scenario source checklist: %w", err)
	}
	architecture, architecturePayload, reportable, reportability, err :=
		validateSourceArchitecture(publication.ArchitectureResult, publication.Checklist)
	if err != nil {
		return SourceReceipt{}, err
	}
	_ = architecture
	attempts, err := validateSourceAttempts(publication.Checklist, publication.Attempts)
	if err != nil {
		return SourceReceipt{}, err
	}
	guard := sourceSensitiveValues(publication.SensitiveValues)
	if sourceContainsSensitive(architecturePayload, guard) {
		return SourceReceipt{}, errors.New("scenario source architecture result contains a declared sensitive value")
	}
	root, rootInfo, err := openSourceRoot(directory)
	if err != nil {
		return SourceReceipt{}, err
	}
	rootClosed := false
	defer func() {
		if !rootClosed {
			if closeErr := root.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, errors.New("close scenario source directory"))
				receipt = SourceReceipt{}
			}
		}
	}()
	if _, err := root.Lstat(SourceManifestName); err == nil {
		return SourceReceipt{}, errors.New("scenario source bundle is already published")
	} else if !os.IsNotExist(err) {
		return SourceReceipt{}, errors.New("inspect scenario source commit marker")
	}
	if err := writeSourceFile(ctx, root, SourceArchitectureName, architecturePayload); err != nil {
		return SourceReceipt{}, fmt.Errorf("retain final scenario architecture result: %w", err)
	}
	files, err := snapshotSourceFiles(ctx, root, guard, SourceManifestName)
	if err != nil {
		return SourceReceipt{}, err
	}
	checklistFile, found := sourceFileByPath(files, "checklist.json")
	if !found {
		return SourceReceipt{}, errors.New("scenario source tree has no checklist.json")
	}
	architectureFile, found := sourceFileByPath(files, SourceArchitectureName)
	if !found {
		return SourceReceipt{}, errors.New("scenario source tree has no final architecture result")
	}
	if err := validateSourceAttemptFiles(attempts, files); err != nil {
		return SourceReceipt{}, err
	}
	checklistPayload, err := readSourceFile(ctx, root, checklistFile)
	if err != nil {
		return SourceReceipt{}, err
	}
	wantChecklist, err := MarshalChecklist(publication.Checklist)
	if err != nil || !bytes.Equal(checklistPayload, wantChecklist) {
		return SourceReceipt{}, errors.New("retained scenario checklist differs from the final checklist")
	}
	fileSetSHA256, err := sourceFileSetDigest(files)
	if err != nil {
		return SourceReceipt{}, err
	}
	manifest := SourceManifest{
		Format: SourceBundleFormat, FormatVersion: SourceBundleFormatVersion,
		Complete: true, PopulationComplete: publication.Checklist.Complete,
		Suite: SuiteName, Cases: len(publication.Checklist.Cases),
		Trials:           publication.Checklist.Repetitions,
		ExpectedAttempts: publication.Checklist.Expected, Origin: publication.Origin,
		ChecklistFingerprint: publication.Checklist.Fingerprint,
		Checklist:            checklistFile, ArchitectureResult: architectureFile,
		ArchitectureReportable: reportable, ArchitectureReportability: reportability,
		Attempts: attempts, FileSetSHA256: fileSetSHA256, Files: files,
	}
	manifestPayload, err := marshalSourceIndented(manifest, maximumSourceManifest)
	if err != nil {
		return SourceReceipt{}, err
	}
	if sourceContainsSensitive(manifestPayload, guard) {
		return SourceReceipt{}, errors.New("scenario source manifest contains a declared sensitive value")
	}
	expected, err := buildSourceReceipt(directory, manifestPayload, manifest)
	if err != nil {
		return SourceReceipt{}, err
	}
	if err := verifySourceRootIdentity(directory, root, rootInfo); err != nil {
		return SourceReceipt{}, err
	}
	if err := writeSourceFile(ctx, root, SourceManifestName, manifestPayload); err != nil {
		return SourceReceipt{}, errors.New("publish scenario source manifest")
	}
	if err := syncSourceRoot(root); err != nil {
		return SourceReceipt{}, err
	}
	if err := verifySourceRootIdentity(directory, root, rootInfo); err != nil {
		return SourceReceipt{}, err
	}
	if err := root.Close(); err != nil {
		return SourceReceipt{}, errors.New("close published scenario source directory")
	}
	rootClosed = true
	opened, err := VerifySourceBundle(ctx, SourceBundleOptions{
		Directory: directory, SensitiveValues: guard,
	}, expected)
	if err != nil {
		return SourceReceipt{}, fmt.Errorf("reopen published scenario source bundle: %w", err)
	}
	if err := writeSourceReceipt(ctx, receiptPath, opened.Receipt, guard); err != nil {
		return SourceReceipt{}, err
	}
	// Reverify after the external anchor is durable so a mutation between the
	// first verification and receipt publication cannot be silently accepted.
	if _, err := VerifySourceBundle(ctx, SourceBundleOptions{
		Directory: directory, SensitiveValues: guard,
	}, opened.Receipt); err != nil {
		return SourceReceipt{}, fmt.Errorf("verify scenario source after receipt publication: %w", err)
	}
	return opened.Receipt, nil
}

// VerifySourceBundle reopens a committed source tree and requires the exact
// portable receipt retained outside it.
func VerifySourceBundle(
	ctx context.Context, options SourceBundleOptions, expected SourceReceipt,
) (bundle SourceBundle, resultErr error) {
	if ctx == nil {
		return SourceBundle{}, errors.New("verify scenario source bundle: nil context")
	}
	if err := ctx.Err(); err != nil {
		return SourceBundle{}, err
	}
	directory, err := validateSourceDirectory(options.Directory, true)
	if err != nil {
		return SourceBundle{}, err
	}
	if err := validateSourceReceipt(expected); err != nil {
		return SourceBundle{}, err
	}
	guard := sourceSensitiveValues(options.SensitiveValues)
	root, rootInfo, err := openSourceRoot(directory)
	if err != nil {
		return SourceBundle{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			bundle = SourceBundle{}
			resultErr = errors.Join(resultErr, errors.New("close verified scenario source directory"))
		}
	}()
	manifestInfo, err := sourceFileInfo(root, SourceManifestName)
	if err != nil || manifestInfo.Size() <= 0 || manifestInfo.Size() > maximumSourceManifest {
		return SourceBundle{}, errors.New("scenario source bundle has no valid commit marker")
	}
	manifestFile := SourceFile{
		Path: SourceManifestName, Purpose: "source_manifest", SizeBytes: manifestInfo.Size(),
	}
	manifestPayload, err := readSourceFile(ctx, root, manifestFile)
	if err != nil {
		return SourceBundle{}, err
	}
	manifestFile.SHA256 = sourceDigest(manifestPayload)
	if sourceContainsSensitive(manifestPayload, guard) {
		return SourceBundle{}, errors.New("scenario source manifest contains a declared sensitive value")
	}
	manifest, err := decodeSourceManifest(manifestPayload)
	if err != nil {
		return SourceBundle{}, err
	}
	files, err := snapshotSourceFiles(ctx, root, guard, SourceManifestName)
	if err != nil {
		return SourceBundle{}, err
	}
	if !reflect.DeepEqual(files, manifest.Files) {
		return SourceBundle{}, errors.New("scenario source file tree differs from its manifest")
	}
	fileSetSHA256, err := sourceFileSetDigest(files)
	if err != nil || fileSetSHA256 != manifest.FileSetSHA256 {
		return SourceBundle{}, errors.New("scenario source file-set digest differs from its manifest")
	}
	checklistPayload, err := readSourceFile(ctx, root, manifest.Checklist)
	if err != nil {
		return SourceBundle{}, err
	}
	checklist, err := decodeSourceChecklist(checklistPayload)
	if err != nil {
		return SourceBundle{}, err
	}
	architecturePayload, err := readSourceFile(ctx, root, manifest.ArchitectureResult)
	if err != nil {
		return SourceBundle{}, err
	}
	architecture, canonicalArchitecture, reportable, reportability, err :=
		validateSourceArchitecture(architecturePayload, checklist)
	if err != nil || !bytes.Equal(architecturePayload, canonicalArchitecture) {
		return SourceBundle{}, errors.New("scenario source architecture result is invalid or noncanonical")
	}
	if manifest.ArchitectureReportable != reportable ||
		manifest.ArchitectureReportability != reportability {
		return SourceBundle{}, errors.New("scenario source architecture reportability changed")
	}
	if err := validateSourceManifest(manifest, checklist); err != nil {
		return SourceBundle{}, err
	}
	if err := validateSourceAttemptFiles(manifest.Attempts, files); err != nil {
		return SourceBundle{}, err
	}
	for _, attempt := range manifest.Attempts {
		if err := verifySourceAttemptPayloads(ctx, root, attempt); err != nil {
			return SourceBundle{}, err
		}
	}
	canonicalManifest, err := marshalSourceIndented(manifest, maximumSourceManifest)
	if err != nil || !bytes.Equal(canonicalManifest, manifestPayload) {
		return SourceBundle{}, errors.New("scenario source manifest is noncanonical")
	}
	receipt, err := buildSourceReceipt(directory, manifestPayload, manifest)
	if err != nil || !samePortableSourceReceipt(receipt, expected) {
		return SourceBundle{}, errors.New("scenario source bundle differs from its expected receipt")
	}
	if err := verifySourceRootIdentity(directory, root, rootInfo); err != nil {
		return SourceBundle{}, err
	}
	// Reread the entire manifest-declared tree immediately before success.
	finalFiles, err := snapshotSourceFiles(ctx, root, guard, SourceManifestName)
	if err != nil || !reflect.DeepEqual(finalFiles, files) {
		return SourceBundle{}, errors.New("scenario source tree changed during verification")
	}
	finalManifest, err := readSourceFile(ctx, root, manifestFile)
	if err != nil || !bytes.Equal(finalManifest, manifestPayload) {
		return SourceBundle{}, errors.New("scenario source manifest changed during verification")
	}
	if err := verifySourceRootIdentity(directory, root, rootInfo); err != nil {
		return SourceBundle{}, err
	}
	return SourceBundle{
		Manifest: manifest, Checklist: checklist,
		ArchitectureResult: architecture, Receipt: receipt,
	}, nil
}

func ReadSourceReceipt(path string) (SourceReceipt, error) {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return SourceReceipt{}, errors.New("scenario source receipt path must be clean and absolute")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return SourceReceipt{}, err
	}
	return decodeSourceReceipt(payload)
}

func (origin SourceOrigin) Validate() error {
	if origin.Kind != "live_realtime_endpoint" && origin.Kind != "hermetic_fixture" {
		return errors.New("scenario source origin kind is invalid")
	}
	if origin.Transport != "websocket" && origin.Transport != "webrtc" {
		return errors.New("scenario source origin transport is invalid")
	}
	if !validChecklistDigest(origin.EndpointSHA256) {
		return errors.New("scenario source endpoint digest is invalid")
	}
	return nil
}

func validateSourceAttempts(checklist Checklist, source []SourceAttempt) ([]SourceAttempt, error) {
	if len(source) > maximumSourceAttempts || len(source) != len(checklist.Attempts) {
		return nil, errors.New("scenario source attempt count differs from the final checklist")
	}
	result := make([]SourceAttempt, len(source))
	for index := range source {
		attempt := cloneSourceAttempt(source[index])
		if !reflect.DeepEqual(attempt.Record, checklist.Attempts[index]) {
			return nil, fmt.Errorf("scenario source attempt %d differs from the final checklist", index+1)
		}
		if err := validateSourceAttempt(attempt); err != nil {
			return nil, fmt.Errorf("scenario source attempt %s: %w", attempt.Record.Key.TaskID, err)
		}
		result[index] = attempt
	}
	return result, nil
}

func validateSourceAttempt(attempt SourceAttempt) error {
	if err := validateSourceFile(attempt.Result, "scorer_result"); err != nil {
		return err
	}
	if attempt.Result.SHA256 != attempt.Record.Execution.ResultSHA256 {
		return errors.New("result file digest differs from the checklist result")
	}
	if (attempt.MediaManifest == nil) != (attempt.Audio == nil) {
		return errors.New("media manifest and stereo audio availability differ")
	}
	if attempt.MediaManifest == nil {
		if attempt.Record.Media != nil || len(attempt.Submitted) != 0 {
			return errors.New("unavailable attempt media disagrees with the checklist or submitted inputs")
		}
		return nil
	}
	if err := validateSourceFile(*attempt.MediaManifest, "media_manifest"); err != nil {
		return err
	}
	if err := validateSourceFile(*attempt.Audio, "stereo_audio"); err != nil {
		return err
	}
	if attempt.Record.Media != nil {
		if attempt.Record.Media.Handle != attempt.MediaManifest.Path ||
			attempt.Record.Media.ManifestSHA256 != attempt.MediaManifest.SHA256 {
			return errors.New("media manifest differs from the checklist receipt")
		}
		if len(attempt.Submitted) != len(attempt.Record.Media.Submitted) {
			return errors.New("submitted inputs differ from the checklist receipt")
		}
	}
	for index, submitted := range attempt.Submitted {
		if err := validateSourceFile(submitted.File, "submitted_input"); err != nil {
			return err
		}
		if submitted.File.SHA256 != submitted.Receipt.SHA256 ||
			submitted.File.SizeBytes != submitted.Receipt.SizeBytes {
			return errors.New("submitted input file differs from its transport receipt")
		}
		if attempt.Record.Media != nil && submitted.Receipt != attempt.Record.Media.Submitted[index] {
			return errors.New("submitted input receipt differs from the checklist")
		}
	}
	return nil
}

func validateSourceAttemptFiles(attempts []SourceAttempt, files []SourceFile) error {
	for _, attempt := range attempts {
		wantedFiles := []SourceFile{attempt.Result}
		if attempt.MediaManifest != nil {
			wantedFiles = append(wantedFiles, *attempt.MediaManifest, *attempt.Audio)
		}
		for _, wanted := range append(wantedFiles, sourceSubmittedFiles(attempt.Submitted)...) {
			found, ok := sourceFileByPath(files, wanted.Path)
			if !ok || found != wanted {
				return fmt.Errorf("scenario source attempt %s file %q differs from the tree",
					attempt.Record.Key.TaskID, wanted.Path)
			}
		}
	}
	return nil
}

func verifySourceAttemptPayloads(
	ctx context.Context, root *os.Root, attempt SourceAttempt,
) error {
	resultPayload, err := readSourceFile(ctx, root, attempt.Result)
	if err != nil {
		return err
	}
	var result scenario.Result
	decoder := json.NewDecoder(bytes.NewReader(resultPayload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return errors.New("decode retained scenario source result")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	if sourceDigest(resultPayload) != attempt.Record.Execution.ResultSHA256 {
		return errors.New("retained scenario source result differs from its checklist row")
	}
	return nil
}

func validateSourceArchitecture(
	source json.RawMessage, checklist Checklist,
) (archbench.Result, []byte, bool, string, error) {
	if len(source) == 0 || len(source) > maximumSourceManifest {
		return archbench.Result{}, nil, false, "", errors.New("scenario architecture result is empty or oversized")
	}
	if err := strictjson.Validate(source); err != nil {
		return archbench.Result{}, nil, false, "", errors.New("scenario architecture result is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var result archbench.Result
	if err := decoder.Decode(&result); err != nil {
		return archbench.Result{}, nil, false, "", fmt.Errorf("decode scenario architecture result: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return archbench.Result{}, nil, false, "", err
	}
	if result.Version != archbench.ResultVersion || result.Measurement.Suite != SuiteName ||
		result.Measurement.Expected != checklist.Expected ||
		len(result.Measurement.Tasks) != len(checklist.Attempts) ||
		len(result.Records) != len(checklist.Attempts) ||
		strings.TrimSpace(result.Measurement.Provenance.FinishedAt) == "" {
		return archbench.Result{}, nil, false, "", errors.New(
			"scenario architecture result is not the exact finished checklist population",
		)
	}
	if _, err := time.Parse(time.RFC3339, result.Measurement.Provenance.FinishedAt); err != nil {
		return archbench.Result{}, nil, false, "", errors.New("scenario architecture finish time is invalid")
	}
	derived := result.Measurement
	derived.Finish()
	retainedSummary := result.Measurement.Summary
	if len(derived.Summary.Distributions) == 0 {
		derived.Summary.Distributions = nil
	}
	if len(retainedSummary.Distributions) == 0 {
		retainedSummary.Distributions = nil
	}
	if !reflect.DeepEqual(derived.Summary, retainedSummary) {
		return archbench.Result{}, nil, false, "", errors.New("scenario architecture summary is stale")
	}
	for index, attempt := range checklist.Attempts {
		if result.Measurement.Tasks[index].ID != attempt.Key.TaskID {
			return archbench.Result{}, nil, false, "", errors.New("scenario architecture task order differs from the checklist")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, result.Records[index]); err != nil {
			return archbench.Result{}, nil, false, "", errors.New("scenario architecture record is invalid")
		}
		if sourceDigest(compact.Bytes()) != attempt.Execution.ResultSHA256 {
			return archbench.Result{}, nil, false, "", errors.New("scenario architecture record differs from the checklist result")
		}
	}
	canonical, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return archbench.Result{}, nil, false, "", err
	}
	canonical = append(canonical, '\n')
	reportable := true
	reportability := ""
	if reportErr := result.Reportable(); reportErr != nil {
		reportable = false
		reportability = reportErr.Error()
	}
	return result, canonical, reportable, reportability, nil
}

func validateSourceManifest(manifest SourceManifest, checklist Checklist) error {
	if manifest.Format != SourceBundleFormat || manifest.FormatVersion != SourceBundleFormatVersion ||
		!manifest.Complete || manifest.Suite != SuiteName || manifest.Cases != len(checklist.Cases) ||
		manifest.PopulationComplete != checklist.Complete ||
		manifest.Trials != checklist.Repetitions || manifest.ExpectedAttempts != checklist.Expected ||
		manifest.ChecklistFingerprint != checklist.Fingerprint {
		return errors.New("scenario source manifest identity is invalid")
	}
	if err := manifest.Origin.Validate(); err != nil {
		return err
	}
	if err := validateSourceFile(manifest.Checklist, "checklist"); err != nil ||
		manifest.Checklist.Path != "checklist.json" {
		return errors.New("scenario source checklist file identity is invalid")
	}
	if err := validateSourceFile(manifest.ArchitectureResult, "architecture_result"); err != nil ||
		manifest.ArchitectureResult.Path != SourceArchitectureName {
		return errors.New("scenario source architecture file identity is invalid")
	}
	if len(manifest.Attempts) != len(checklist.Attempts) ||
		len(manifest.Files) == 0 ||
		len(manifest.Files) > maximumSourceFiles {
		return errors.New("scenario source manifest counts are invalid")
	}
	for index, attempt := range manifest.Attempts {
		if !reflect.DeepEqual(attempt.Record, checklist.Attempts[index]) {
			return errors.New("scenario source manifest attempt differs from the checklist")
		}
		if err := validateSourceAttempt(attempt); err != nil {
			return err
		}
	}
	for index, file := range manifest.Files {
		if err := validateSourceFile(file, sourcePurpose(file.Path)); err != nil {
			return err
		}
		if index > 0 && manifest.Files[index-1].Path >= file.Path {
			return errors.New("scenario source files are not sorted and unique")
		}
	}
	digest, err := sourceFileSetDigest(manifest.Files)
	if err != nil || digest != manifest.FileSetSHA256 {
		return errors.New("scenario source manifest file-set digest is invalid")
	}
	return nil
}

func decodeSourceChecklist(payload []byte) (Checklist, error) {
	if err := strictjson.Validate(payload); err != nil {
		return Checklist{}, errors.New("scenario source checklist is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var checklist Checklist
	if err := decoder.Decode(&checklist); err != nil {
		return Checklist{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Checklist{}, err
	}
	canonical, err := MarshalChecklist(checklist)
	if err != nil || !bytes.Equal(payload, canonical) {
		return Checklist{}, errors.New("scenario source checklist is invalid or noncanonical")
	}
	return checklist, nil
}

func decodeSourceManifest(payload []byte) (SourceManifest, error) {
	if len(payload) == 0 || len(payload) > maximumSourceManifest || strictjson.Validate(payload) != nil {
		return SourceManifest{}, errors.New("scenario source manifest is empty, oversized, or not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest SourceManifest
	if err := decoder.Decode(&manifest); err != nil {
		return SourceManifest{}, errors.New("decode scenario source manifest")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return SourceManifest{}, err
	}
	return manifest, nil
}

func buildSourceReceipt(
	directory string, manifestPayload []byte, manifest SourceManifest,
) (SourceReceipt, error) {
	receipt := SourceReceipt{
		Format: SourceReceiptFormat, FormatVersion: SourceReceiptVersion,
		Directory: directory, ManifestSHA256: sourceDigest(manifestPayload),
		FileSetSHA256: manifest.FileSetSHA256, ChecklistSHA256: manifest.Checklist.SHA256,
		ArchitectureSHA256: manifest.ArchitectureResult.SHA256,
	}
	payload, err := marshalSourceCompact(struct {
		Format             string `json:"format"`
		FormatVersion      int    `json:"format_version"`
		ManifestSHA256     string `json:"manifest_sha256"`
		FileSetSHA256      string `json:"file_set_sha256"`
		ChecklistSHA256    string `json:"checklist_sha256"`
		ArchitectureSHA256 string `json:"architecture_sha256"`
	}{receipt.Format, receipt.FormatVersion, receipt.ManifestSHA256,
		receipt.FileSetSHA256, receipt.ChecklistSHA256, receipt.ArchitectureSHA256},
		maximumSourceReceipt,
	)
	if err != nil {
		return SourceReceipt{}, err
	}
	receipt.ReceiptSHA256 = sourceDigest(payload)
	return receipt, nil
}

func validateSourceReceipt(receipt SourceReceipt) error {
	if receipt.Format != SourceReceiptFormat || receipt.FormatVersion != SourceReceiptVersion ||
		!validChecklistDigest(receipt.ManifestSHA256) ||
		!validChecklistDigest(receipt.FileSetSHA256) ||
		!validChecklistDigest(receipt.ChecklistSHA256) ||
		!validChecklistDigest(receipt.ArchitectureSHA256) ||
		!validChecklistDigest(receipt.ReceiptSHA256) {
		return errors.New("scenario source receipt identity is invalid")
	}
	payload, err := marshalSourceCompact(struct {
		Format             string `json:"format"`
		FormatVersion      int    `json:"format_version"`
		ManifestSHA256     string `json:"manifest_sha256"`
		FileSetSHA256      string `json:"file_set_sha256"`
		ChecklistSHA256    string `json:"checklist_sha256"`
		ArchitectureSHA256 string `json:"architecture_sha256"`
	}{receipt.Format, receipt.FormatVersion, receipt.ManifestSHA256,
		receipt.FileSetSHA256, receipt.ChecklistSHA256, receipt.ArchitectureSHA256},
		maximumSourceReceipt,
	)
	if err != nil || sourceDigest(payload) != receipt.ReceiptSHA256 {
		return errors.New("scenario source receipt digest is invalid")
	}
	return nil
}

func samePortableSourceReceipt(left, right SourceReceipt) bool {
	return left.Format == right.Format && left.FormatVersion == right.FormatVersion &&
		left.ManifestSHA256 == right.ManifestSHA256 && left.FileSetSHA256 == right.FileSetSHA256 &&
		left.ChecklistSHA256 == right.ChecklistSHA256 &&
		left.ArchitectureSHA256 == right.ArchitectureSHA256 &&
		left.ReceiptSHA256 == right.ReceiptSHA256
}

func writeSourceReceipt(
	ctx context.Context, path string, receipt SourceReceipt, guard []string,
) error {
	if err := validateSourceReceipt(receipt); err != nil {
		return err
	}
	payload, err := marshalSourceIndented(receipt, maximumSourceReceipt)
	if err != nil {
		return err
	}
	if sourceContainsSensitive(payload, guard) {
		return errors.New("scenario source receipt contains a declared sensitive value")
	}
	parent := filepath.Dir(path)
	name := filepath.Base(path)
	if !validSourcePath(name) {
		return errors.New("scenario source receipt filename is invalid")
	}
	root, rootInfo, err := openSourceRoot(parent)
	if err != nil {
		return fmt.Errorf("open scenario source receipt parent: %w", err)
	}
	written := false
	defer func() {
		if !written {
			_ = root.Remove(name)
		}
		_ = root.Close()
	}()
	if err := verifySourceRootIdentity(parent, root, rootInfo); err != nil {
		return err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write scenario source receipt create-only: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	identity := SourceFile{
		Path: name, Purpose: "source_receipt", SHA256: sourceDigest(payload),
		SizeBytes: int64(len(payload)),
	}
	retained, err := readSourceFile(ctx, root, identity)
	if err != nil || !bytes.Equal(retained, payload) {
		return errors.New("reopen scenario source receipt")
	}
	decoded, err := decodeSourceReceipt(retained)
	if err != nil || !samePortableSourceReceipt(decoded, receipt) {
		return errors.New("reopen scenario source receipt")
	}
	if err := syncSourceRoot(root); err != nil {
		return err
	}
	if err := verifySourceRootIdentity(parent, root, rootInfo); err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return errors.New("close scenario source receipt parent")
	}
	written = true
	return nil
}

func decodeSourceReceipt(payload []byte) (SourceReceipt, error) {
	if len(payload) == 0 || len(payload) > maximumSourceReceipt || strictjson.Validate(payload) != nil {
		return SourceReceipt{}, errors.New("scenario source receipt is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var receipt SourceReceipt
	if err := decoder.Decode(&receipt); err != nil || requireJSONEOF(decoder) != nil {
		return SourceReceipt{}, errors.New("decode scenario source receipt")
	}
	canonical, err := marshalSourceIndented(receipt, maximumSourceReceipt)
	if err != nil || !bytes.Equal(payload, canonical) {
		return SourceReceipt{}, errors.New("scenario source receipt is noncanonical")
	}
	if err := validateSourceReceipt(receipt); err != nil {
		return SourceReceipt{}, err
	}
	return receipt, nil
}

func snapshotSourceFiles(
	ctx context.Context, root *os.Root, guard []string, exclude string,
) ([]SourceFile, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, errors.New("enumerate scenario source tree")
	}
	if len(entries) == 0 || len(entries) > maximumSourceFiles+1 {
		return nil, errors.New("scenario source tree file count is invalid")
	}
	files := make([]SourceFile, 0, len(entries))
	identities := make([]os.FileInfo, 0, len(entries))
	total := int64(0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Name() == exclude {
			continue
		}
		if entry.IsDir() {
			return nil, errors.New("scenario source tree must be flat")
		}
		info, err := sourceFileInfo(root, entry.Name())
		if err != nil {
			return nil, err
		}
		for _, previous := range identities {
			if os.SameFile(previous, info) {
				return nil, errors.New("scenario source tree contains duplicate file identities")
			}
		}
		identities = append(identities, info)
		if total > maximumSourceTotalBytes-info.Size() {
			return nil, errors.New("scenario source tree exceeds its aggregate bound")
		}
		total += info.Size()
		file := SourceFile{
			Path: entry.Name(), Purpose: sourcePurpose(entry.Name()), SizeBytes: info.Size(),
		}
		payload, err := readSourceFile(ctx, root, file)
		if err != nil {
			return nil, err
		}
		if sourceContainsSensitive(payload, guard) {
			return nil, fmt.Errorf("scenario source file %q contains a declared sensitive value", file.Path)
		}
		file.SHA256 = sourceDigest(payload)
		files = append(files, file)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return files, nil
}

func sourceFileInfo(root *os.Root, path string) (os.FileInfo, error) {
	if !validSourcePath(path) {
		return nil, errors.New("scenario source file path is invalid")
	}
	info, err := root.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect scenario source file %q", path)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > maximumSourceFileBytes {
		return nil, fmt.Errorf("scenario source file %q is not an exact bounded regular file", path)
	}
	return info, nil
}

func readSourceFile(
	ctx context.Context, root *os.Root, expected SourceFile,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("read scenario source file: nil context")
	}
	before, err := sourceFileInfo(root, expected.Path)
	if err != nil {
		return nil, err
	}
	if expected.SizeBytes > 0 && before.Size() != expected.SizeBytes {
		return nil, errors.New("scenario source file size differs from its identity")
	}
	file, err := root.Open(expected.Path)
	if err != nil {
		return nil, fmt.Errorf("open scenario source file %q", expected.Path)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("scenario source file changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, before.Size()+1))
	if err != nil || int64(len(payload)) != before.Size() {
		return nil, errors.New("read exact scenario source file")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := sourceFileInfo(root, expected.Path)
	if err != nil || !os.SameFile(before, after) || !os.SameFile(opened, after) {
		return nil, errors.New("scenario source file changed while reading")
	}
	if expected.SHA256 != "" && sourceDigest(payload) != expected.SHA256 {
		return nil, errors.New("scenario source file digest differs from its identity")
	}
	return payload, nil
}

func writeSourceFile(ctx context.Context, root *os.Root, path string, payload []byte) error {
	if ctx == nil || root == nil || !validSourcePath(path) || len(payload) == 0 ||
		len(payload) > maximumSourceFileBytes {
		return errors.New("scenario source write request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = root.Remove(path)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	written = true
	return nil
}

func openSourceRoot(directory string) (*os.Root, os.FileInfo, error) {
	pathInfo, err := os.Lstat(directory)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() {
		return nil, nil, errors.New("scenario source directory is not an exact directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, errors.New("open scenario source directory")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(pathInfo, rootInfo) {
		_ = root.Close()
		return nil, nil, errors.New("scenario source directory changed while opening")
	}
	return root, rootInfo, nil
}

func verifySourceRootIdentity(directory string, root *os.Root, expected os.FileInfo) error {
	pathInfo, err := os.Lstat(directory)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() ||
		!os.SameFile(pathInfo, expected) {
		return errors.New("scenario source directory identity changed")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(rootInfo, expected) {
		return errors.New("scenario source root identity changed")
	}
	return nil
}

func syncSourceRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open scenario source directory for sync")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync scenario source directory")
	}
	return nil
}

func validateSourceDirectory(path string, mustExist bool) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return "", errors.New("scenario source directory must be clean and absolute")
	}
	if path == filepath.Dir(path) {
		return "", errors.New("scenario source directory must not be a filesystem root")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			if !mustExist && current == path && os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("inspect scenario source path %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("scenario source path %q is not an exact directory", current)
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return path, nil
}

func validateSourceReceiptDestination(directory, path string) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return "", errors.New("scenario source receipt destination must be clean and absolute")
	}
	if path == filepath.Dir(path) || path == directory ||
		strings.HasPrefix(path, directory+string(filepath.Separator)) {
		return "", errors.New("scenario source receipt must be outside the source directory")
	}
	if _, err := os.Lstat(path); err == nil {
		return "", errors.New("scenario source receipt destination already exists")
	} else if !os.IsNotExist(err) {
		return "", errors.New("inspect scenario source receipt destination")
	}
	parent, err := validateSourceDirectory(filepath.Dir(path), true)
	if err != nil || parent == "" {
		return "", errors.New("scenario source receipt parent is invalid")
	}
	return path, nil
}

func validateSourceFile(file SourceFile, purpose string) error {
	if !validSourcePath(file.Path) || file.Purpose != purpose ||
		!validChecklistDigest(file.SHA256) || file.SizeBytes <= 0 ||
		file.SizeBytes > maximumSourceFileBytes {
		return errors.New("scenario source file identity is invalid")
	}
	return nil
}

func validSourcePath(path string) bool {
	return path != "" && len(path) <= maximumSourcePathBytes && filepath.Base(path) == path &&
		filepath.Clean(path) == path && path != "." && path != ".." &&
		!strings.ContainsAny(path, "\x00\r\n")
}

func sourcePurpose(path string) string {
	switch {
	case path == "checklist.json":
		return "checklist"
	case path == "CHECKLIST.md":
		return "checklist_review"
	case path == "manifest.json":
		return "legacy_review_manifest"
	case path == "REVIEW.md":
		return "human_review"
	case path == SourceArchitectureName:
		return "architecture_result"
	case strings.HasSuffix(path, ".result.json"):
		return "scorer_result"
	case strings.HasSuffix(path, ".media.json"):
		return "media_manifest"
	case strings.HasSuffix(path, ".checklist.json"):
		return "checklist_attempt"
	case strings.HasSuffix(path, ".stereo.wav"):
		return "stereo_audio"
	case strings.Contains(path, "submitted-"):
		return "submitted_input"
	case strings.Contains(path, "-input-"):
		return "authored_input"
	default:
		return "supporting_evidence"
	}
}

func sourceFileByPath(files []SourceFile, path string) (SourceFile, bool) {
	index, found := slices.BinarySearchFunc(files, path, func(file SourceFile, wanted string) int {
		return strings.Compare(file.Path, wanted)
	})
	if !found {
		return SourceFile{}, false
	}
	return files[index], true
}

func sourceFileSetDigest(files []SourceFile) (string, error) {
	payload, err := marshalSourceCompact(files, maximumSourceManifest)
	if err != nil {
		return "", err
	}
	return sourceDigest(payload), nil
}

func sourceDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func marshalSourceIndented(value any, maximum int) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(payload) == 0 || len(payload)+1 > maximum {
		return nil, errors.New("encode scenario source artifact")
	}
	return append(payload, '\n'), nil
}

func marshalSourceCompact(value any, maximum int) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil || len(payload) == 0 || len(payload) > maximum {
		return nil, errors.New("encode scenario source identity")
	}
	return payload, nil
}

func sourceSensitiveValues(source []string) []string {
	result := make([]string, 0, len(source))
	for _, value := range source {
		if len(strings.TrimSpace(value)) >= 8 {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return slices.Compact(result)
}

func sourceContainsSensitive(payload []byte, values []string) bool {
	for _, value := range values {
		if bytes.Contains(payload, []byte(value)) {
			return true
		}
	}
	return false
}

func sourceSubmittedFiles(source []SourceSubmittedInput) []SourceFile {
	result := make([]SourceFile, len(source))
	for index := range source {
		result[index] = source[index].File
	}
	return result
}

func cloneSourceAttempt(source SourceAttempt) SourceAttempt {
	source.Record = source.Record.Clone()
	if source.MediaManifest != nil {
		copy := *source.MediaManifest
		source.MediaManifest = &copy
	}
	if source.Audio != nil {
		copy := *source.Audio
		source.Audio = &copy
	}
	source.Submitted = slices.Clone(source.Submitted)
	return source
}
