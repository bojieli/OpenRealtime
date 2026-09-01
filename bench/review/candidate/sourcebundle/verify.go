package sourcebundle

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

func Verify(ctx context.Context, directory, receiptPath string) (Manifest, Receipt, error) {
	if ctx == nil {
		return Manifest{}, Receipt{}, errors.New("verify candidate source bundle: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, Receipt{}, err
	}
	directory, err := validateAbsolutePath("candidate source directory", directory)
	if err != nil {
		return Manifest{}, Receipt{}, err
	}
	receiptPath, err = validateAbsolutePath("candidate source receipt", receiptPath)
	if err != nil {
		return Manifest{}, Receipt{}, err
	}
	receiptBytes, err := readExternalRegular(receiptPath, maximumMetadataBytes)
	if err != nil {
		return Manifest{}, Receipt{}, err
	}
	var receipt Receipt
	if err := decodeCanonical(receiptBytes, &receipt); err != nil {
		return Manifest{}, Receipt{}, errors.New("candidate source receipt is invalid")
	}
	canonicalReceipt, err := receiptPayload(receipt)
	if err != nil || !bytes.Equal(receiptBytes, canonicalReceipt) ||
		receipt.Format != ReceiptFormat || receipt.FormatVersion != ReceiptFormatVersion ||
		receipt.Directory != directory || receipt.AttemptCount <= 0 ||
		!validDigest(receipt.ManifestSHA256) || !validDigest(receipt.FileSetSHA256) ||
		!validDigest(receipt.ReceiptSHA256) {
		return Manifest{}, Receipt{}, errors.New("candidate source receipt identity is invalid")
	}

	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return Manifest{}, Receipt{}, errors.New("candidate source directory is invalid")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Manifest{}, Receipt{}, errors.New("open candidate source directory")
	}
	defer root.Close()
	opened, openErr := root.Stat(".")
	afterOpen, visibleErr := os.Lstat(directory)
	if openErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!afterOpen.IsDir() || !os.SameFile(visible, opened) || !os.SameFile(opened, afterOpen) {
		return Manifest{}, Receipt{}, errors.New("candidate source directory changed while opening")
	}
	manifestInfo, err := root.Lstat(manifestName)
	if err != nil || manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() ||
		manifestInfo.Size() <= 0 || manifestInfo.Size() > maximumPopulationMetadataBytes {
		return Manifest{}, Receipt{}, errors.New("candidate source manifest is invalid")
	}
	manifestBytes, err := readRegular(root, manifestName, manifestInfo.Size())
	if err != nil || digest(manifestBytes) != receipt.ManifestSHA256 {
		return Manifest{}, Receipt{}, errors.New("candidate source manifest differs from its receipt")
	}
	var manifest Manifest
	if err := decodePopulationCanonical(manifestBytes, &manifest); err != nil {
		return Manifest{}, Receipt{}, errors.New("candidate source manifest is invalid")
	}
	if err := validateManifestHeader(manifest, receipt); err != nil {
		return Manifest{}, Receipt{}, err
	}

	actualFiles, err := walkSourceFiles(root)
	if err != nil {
		return Manifest{}, Receipt{}, err
	}
	filtered := actualFiles[:0]
	for _, file := range actualFiles {
		if file.Path == manifestName {
			continue
		}
		switch file.Path {
		case resultName:
			file.Purpose = "authoritative deterministic result"
		case reviewName:
			file.Purpose = "case-by-case human review index"
		default:
			file.Purpose = "candidate attempt source evidence"
		}
		filtered = append(filtered, file)
	}
	actualFiles = filtered
	if !reflect.DeepEqual(actualFiles, manifest.Files) {
		return Manifest{}, Receipt{}, errors.New("candidate source file set differs from its manifest")
	}
	setDigest, err := fileSetDigest(actualFiles)
	if err != nil || setDigest != manifest.FileSetSHA256 || setDigest != receipt.FileSetSHA256 {
		return Manifest{}, Receipt{}, errors.New("candidate source file-set identity is invalid")
	}
	files := make(map[string]SourceFile, len(actualFiles))
	for _, file := range actualFiles {
		if _, duplicate := files[file.Path]; duplicate {
			return Manifest{}, Receipt{}, errors.New("candidate source file path is duplicated")
		}
		files[file.Path] = file
	}
	result, err := verifyResult(root, manifest, files)
	if err != nil {
		return Manifest{}, Receipt{}, err
	}
	if err := verifyAttempts(ctx, root, directory, manifest, result, files); err != nil {
		return Manifest{}, Receipt{}, err
	}
	opened, openErr = root.Stat(".")
	afterRead, visibleErr := os.Lstat(directory)
	if openErr != nil || visibleErr != nil || afterRead.Mode()&os.ModeSymlink != 0 ||
		!afterRead.IsDir() || !os.SameFile(visible, opened) || !os.SameFile(opened, afterRead) {
		return Manifest{}, Receipt{}, errors.New("candidate source directory changed while verifying")
	}
	return cloneManifest(manifest), receipt, nil
}

func validateManifestHeader(manifest Manifest, receipt Receipt) error {
	if manifest.Format != ManifestFormat || manifest.FormatVersion != ManifestFormatVersion ||
		!manifest.Complete || manifest.Suite == "" || manifest.AttemptCount <= 0 ||
		manifest.AttemptCount != len(manifest.Attempts) || manifest.AttemptCount != receipt.AttemptCount ||
		manifest.ResultPath != resultName || manifest.ReviewPath != reviewName ||
		manifest.Advisory != "pending" || !validDigest(manifest.ResultSHA256) ||
		!validDigest(manifest.ReviewSHA256) || !validDigest(manifest.FileSetSHA256) ||
		len(manifest.Files) == 0 || len(manifest.Files) > maximumSourceFiles {
		return errors.New("candidate source manifest header is invalid")
	}
	if err := manifest.Origin.Validate(); err != nil {
		return errors.New("candidate source manifest run origin is invalid")
	}
	if err := manifest.Cell.Execution.Validate(); err != nil {
		return errors.New("candidate source manifest cell is invalid")
	}
	previous := ""
	for _, file := range manifest.Files {
		if !safeRelative(file.Path) || !validDigest(file.SHA256) || file.SizeBytes <= 0 ||
			file.SizeBytes > maximumSourceBytesForPath(file.Path) || file.Path <= previous || file.Purpose == "" {
			return errors.New("candidate source manifest file entry is invalid")
		}
		previous = file.Path
	}
	return nil
}

func verifyResult(
	root *os.Root, manifest Manifest, files map[string]SourceFile,
) (bench.Result, error) {
	file, found := files[resultName]
	if !found || file.SHA256 != manifest.ResultSHA256 {
		return bench.Result{}, errors.New("candidate source deterministic result is missing")
	}
	payload, err := readRegular(root, resultName, file.SizeBytes)
	if err != nil || digest(payload) != manifest.ResultSHA256 {
		return bench.Result{}, errors.New("candidate source deterministic result changed")
	}
	var result bench.Result
	if err := decodePopulationCanonical(payload, &result); err != nil ||
		result.Suite != manifest.Suite || !reflect.DeepEqual(result.Cell, manifest.Cell) ||
		!reflect.DeepEqual(result.Provenance, manifest.Provenance) || len(result.Tasks) > manifest.AttemptCount {
		return bench.Result{}, errors.New("candidate source deterministic result identity is invalid")
	}
	derived := result
	derived.Finish()
	derivedSummary, derivedErr := canonicalCompact(derived.Summary)
	retainedSummary, retainedErr := canonicalCompact(result.Summary)
	if derivedErr != nil || retainedErr != nil || !bytes.Equal(derivedSummary, retainedSummary) {
		return bench.Result{}, errors.New("candidate source deterministic summary is not derived from its rows")
	}
	reviewFile, found := files[reviewName]
	if !found || reviewFile.SHA256 != manifest.ReviewSHA256 {
		return bench.Result{}, errors.New("candidate source human review index is missing")
	}
	return result, nil
}

func verifyAttempts(
	ctx context.Context, root *os.Root, directory string, manifest Manifest,
	result bench.Result, files map[string]SourceFile,
) error {
	seen := make(map[string]struct{}, len(manifest.Attempts))
	rows := make(map[string][]bench.TaskOutcome, len(result.Tasks))
	for _, outcome := range result.Tasks {
		rows[outcome.ID] = append(rows[outcome.ID], outcome)
	}
	previousCase, previousTrial := "", 0
	for index, entry := range manifest.Attempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.AttemptID == "" || entry.Suite != manifest.Suite || entry.Case == "" || entry.Trial <= 0 ||
			entry.Terminal != "completed" && entry.Terminal != "aborted" ||
			entry.MediaSource != candidate.MediaSharedSession && entry.MediaSource != candidate.MediaExternalHarness {
			return errors.New("candidate source attempt entry is incomplete")
		}
		if index > 0 && (entry.Case < previousCase || entry.Case == previousCase && entry.Trial <= previousTrial) {
			return errors.New("candidate source attempts are not canonically ordered")
		}
		previousCase, previousTrial = entry.Case, entry.Trial
		if _, duplicate := seen[entry.AttemptID]; duplicate {
			return errors.New("candidate source attempt identity is duplicated")
		}
		seen[entry.AttemptID] = struct{}{}
		wantDirectory := filepath.ToSlash(filepath.Join("attempts", digestName(entry.AttemptID)))
		if entry.Directory != wantDirectory ||
			entry.AttemptPath != filepath.ToSlash(filepath.Join(wantDirectory, "attempt.json")) ||
			!validDigest(entry.AttemptSHA256) {
			return errors.New("candidate source attempt paths or digests are invalid")
		}
		attempt, err := verifyAttemptSpecification(root, entry, files)
		if err != nil {
			return err
		}
		if attempt.ID() != entry.AttemptID || attempt.Suite != entry.Suite || attempt.Case != entry.Case ||
			attempt.Trial != entry.Trial || attempt.MediaSource != entry.MediaSource ||
			!reflect.DeepEqual(attempt.Cell, manifest.Cell) ||
			!matchingRunProvenance(attempt.Provenance, manifest.Provenance) ||
			!reflect.DeepEqual(attempt.Origin, manifest.Origin) ||
			!reflect.DeepEqual(attempt.ExecutionRequirement, manifest.Cell.Execution) {
			return errors.New("candidate source attempt metadata differs from its manifest")
		}
		if entry.Terminal == "completed" {
			available := rows[entry.Case]
			match := -1
			for rowIndex, outcome := range available {
				if reflect.DeepEqual(outcome, entry.Deterministic) {
					match = rowIndex
					break
				}
			}
			if match < 0 {
				return errors.New("candidate source attempt outcome differs from deterministic result")
			}
			rows[entry.Case] = append(available[:match], available[match+1:]...)
		}
		hasReview := entry.CompletionPath != "" || entry.ContextPath != "" ||
			entry.CompletionSHA256 != "" || entry.ContextSHA256 != "" || entry.Media != nil
		if entry.EvidenceComplete || hasReview {
			if entry.Terminal != "completed" || entry.Media == nil ||
				entry.CompletionPath != "completion.json" || entry.ContextPath != "review-context.json" ||
				!validDigest(entry.CompletionSHA256) || !validDigest(entry.ContextSHA256) {
				return errors.New("candidate source review inputs are only partially retained")
			}
			completion, contextPayload, err := verifyAttemptCompletion(ctx, root, entry, files)
			if err != nil {
				return err
			}
			if completion.Attempt.ID() != entry.AttemptID ||
				!reflect.DeepEqual(completion.Outcome, entry.Deterministic) {
				return errors.New("candidate source completion differs from its manifest")
			}
			attemptRoot := filepath.Join(directory, filepath.FromSlash(entry.Directory))
			requestMedia := *entry.Media
			requestMedia.Validation = ""
			requestMedia.SizeBytes = 0
			prepared, err := review.PrepareContext(ctx, review.Request{
				AttemptID: entry.AttemptID, Suite: entry.Suite, Case: entry.Case, Trial: entry.Trial,
				RootDirectory: attemptRoot, Context: contextPayload, Media: []review.Media{requestMedia},
			})
			if err != nil || len(prepared.Media) != 1 || !reflect.DeepEqual(prepared.Media[0].Media, *entry.Media) {
				return errors.New("candidate source review request cannot be reconstructed")
			}
			if entry.EvidenceComplete && !reflect.DeepEqual(completion.Attempt, attempt) {
				return errors.New("candidate source completed attempt changed")
			}
		}
		if err := verifyArtifacts(root, entry, files); err != nil {
			return err
		}
	}
	for _, remaining := range rows {
		if len(remaining) != 0 {
			return errors.New("candidate source deterministic result contains an unmatched row")
		}
	}
	return nil
}

func verifyAttemptSpecification(
	root *os.Root, entry AttemptEntry, files map[string]SourceFile,
) (candidate.Attempt, error) {
	attemptPayload, err := readManifestFile(root, files, entry.AttemptPath, entry.AttemptSHA256)
	if err != nil {
		return candidate.Attempt{}, err
	}
	var attempt candidate.Attempt
	if err := decodeCanonical(attemptPayload, &attempt); err != nil || normalizeAttemptContext(&attempt) != nil {
		return candidate.Attempt{}, errors.New("candidate source attempt specification is invalid")
	}
	return attempt, nil
}

func verifyAttemptCompletion(
	ctx context.Context, root *os.Root, entry AttemptEntry, files map[string]SourceFile,
) (candidate.Completion, []byte, error) {
	completionPath := filepath.ToSlash(filepath.Join(entry.Directory, entry.CompletionPath))
	completionPayload, err := readManifestFile(root, files, completionPath, entry.CompletionSHA256)
	if err != nil {
		return candidate.Completion{}, nil, err
	}
	var completion candidate.Completion
	if err := decodeCanonical(completionPayload, &completion); err != nil ||
		normalizeAttemptContext(&completion.Attempt) != nil || completion.Validate() != nil {
		return candidate.Completion{}, nil, errors.New("candidate source completion is invalid")
	}
	contextPath := filepath.ToSlash(filepath.Join(entry.Directory, entry.ContextPath))
	contextPayload, err := readManifestFile(root, files, contextPath, "")
	if err != nil {
		return candidate.Completion{}, nil, err
	}
	contextSHA, err := review.CanonicalContextSHA256(ctx, contextPayload)
	if err != nil || contextSHA != entry.ContextSHA256 {
		return candidate.Completion{}, nil, errors.New("candidate source review context is invalid")
	}
	var retained reviewContext
	if err := decodeCompact(contextPayload, &retained); err != nil ||
		!reflect.DeepEqual(retained.Attempt, completion.Attempt) ||
		!reflect.DeepEqual(retained.DeterministicOutcome, completion.Outcome) ||
		!reflect.DeepEqual(retained.Transcript, completion.Transcript) ||
		retained.DeterministicAuthority != "benchmark scorer is authoritative" ||
		retained.AdvisoryReviewAuthority != "offline multimodal review is advisory" {
		return candidate.Completion{}, nil, errors.New("candidate source review context differs from completion")
	}
	return completion, contextPayload, nil
}

func normalizeAttemptContext(attempt *candidate.Attempt) error {
	if attempt == nil {
		return errors.New("candidate source attempt is nil")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, attempt.Context); err != nil {
		return err
	}
	attempt.Context = compact.Bytes()
	return attempt.Validate()
}

func verifyArtifacts(root *os.Root, entry AttemptEntry, files map[string]SourceFile) error {
	seen := make(map[string]struct{}, len(entry.Artifacts))
	for _, artifact := range entry.Artifacts {
		if artifact.Name == "" || artifact.Path != filepath.ToSlash(filepath.Join("artifacts", artifact.Name)) ||
			!validDigest(artifact.SHA256) || artifact.SizeBytes <= 0 ||
			artifact.SizeBytes > maximumSourceFileBytes {
			return errors.New("candidate source artifact entry is invalid")
		}
		if _, duplicate := seen[artifact.Name]; duplicate {
			return errors.New("candidate source artifact name is duplicated")
		}
		seen[artifact.Name] = struct{}{}
		path := filepath.ToSlash(filepath.Join(entry.Directory, artifact.Path))
		payload, err := readManifestFile(root, files, path, artifact.SHA256)
		if err != nil || int64(len(payload)) != artifact.SizeBytes {
			return errors.New("candidate source artifact differs from its manifest")
		}
		if artifact.ContentType == "application/json" {
			if err := strictjson.Validate(payload); err != nil {
				return errors.New("candidate source JSON artifact is invalid")
			}
		}
	}
	return nil
}

func readManifestFile(
	root *os.Root, files map[string]SourceFile, path, expectedDigest string,
) ([]byte, error) {
	file, found := files[path]
	if !found || expectedDigest != "" && file.SHA256 != expectedDigest {
		return nil, errors.New("candidate source manifest references a missing file")
	}
	payload, err := readRegular(root, path, file.SizeBytes)
	if err != nil || digest(payload) != file.SHA256 {
		return nil, errors.New("candidate source file differs from its manifest")
	}
	return payload, nil
}

func decodeCanonical(payload []byte, destination any) error {
	return decodeCanonicalBounded(payload, destination, maximumMetadataBytes)
}

func decodePopulationCanonical(payload []byte, destination any) error {
	return decodeCanonicalBounded(payload, destination, maximumPopulationMetadataBytes)
}

func decodeCanonicalBounded(payload []byte, destination any, maximum int) error {
	if len(payload) == 0 || len(payload) > maximum {
		return errors.New("candidate source JSON is empty or oversized")
	}
	if err := validateSourceJSON(payload, maximum); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	canonical, err := canonicalIndentedBounded(destination, maximum)
	if err != nil || !bytes.Equal(payload, canonical) {
		return errors.New("candidate source JSON is noncanonical")
	}
	return nil
}

func decodeCompact(payload []byte, destination any) error {
	if len(payload) == 0 || len(payload) > maximumMetadataBytes {
		return errors.New("candidate source compact JSON is empty or oversized")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumMetadataBytes, MaxDepth: 128, MaxTokens: 5_000_000,
		MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: maximumMetadataBytes,
		MaxWorkBytes: 256 << 20,
	}); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	canonical, err := canonicalCompact(destination)
	if err != nil || !bytes.Equal(payload, canonical) {
		return errors.New("candidate source compact JSON is noncanonical")
	}
	return nil
}

func safeRelative(path string) bool {
	return path != "" && len(path) <= 4096 && !filepath.IsAbs(path) &&
		filepath.Clean(path) == path && path != "." && path != ".." &&
		!strings.Contains(path, `\`) && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func cloneManifest(source Manifest) Manifest {
	result := source
	result.Files = cloneFiles(source.Files)
	result.Attempts = make([]AttemptEntry, len(source.Attempts))
	for index := range source.Attempts {
		result.Attempts[index] = cloneEntry(source.Attempts[index])
	}
	return result
}
