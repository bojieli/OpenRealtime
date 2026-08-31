package campaign

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
)

// PublishAggregate seals the human-facing campaign index only after every
// source and per-case evaluation receipt has been independently reopened. A
// receipt is durably committed before the staged directory is promoted, so a
// fresh process can finish that promotion without invoking the provider.
func PublishAggregate(
	ctx context.Context, options AggregateOptions, result Result,
) (AggregateBundle, error) {
	return publishAggregate(ctx, options, result, aggregatePublishOperations{})
}

type aggregatePublishOperations struct {
	afterReceipt func() error
}

func publishAggregate(
	ctx context.Context, options AggregateOptions, result Result, operations aggregatePublishOperations,
) (AggregateBundle, error) {
	if ctx == nil {
		return AggregateBundle{}, errors.New("publish candidate review aggregate: nil context")
	}
	if err := ctx.Err(); err != nil {
		return AggregateBundle{}, err
	}
	configuration, err := prepareAggregateOptions(options, result)
	if err != nil {
		return AggregateBundle{}, err
	}
	receiptPresent, err := aggregateReceiptExists(configuration.ReceiptPath)
	if err != nil {
		return AggregateBundle{}, err
	}
	if receiptPresent {
		receipt, err := readAggregateReceipt(configuration.ReceiptPath)
		if err != nil {
			return AggregateBundle{}, err
		}
		recovered, err := recoverAggregate(ctx, configuration, receipt)
		if err != nil {
			return AggregateBundle{}, err
		}
		if !sameCampaignResult(recovered.Result, result) {
			return AggregateBundle{}, errors.New("recovered candidate review campaign differs from requested result")
		}
		return recovered, nil
	}
	if exists, err := aggregateDirectoryExists(configuration.Directory); err != nil {
		return AggregateBundle{}, err
	} else if exists {
		return AggregateBundle{}, errors.New("candidate review aggregate exists without a receipt")
	}
	evaluations, err := verifyCampaignInputs(
		ctx, configuration.SourceReceiptPath, configuration.SensitiveValues, result,
	)
	if err != nil {
		return AggregateBundle{}, err
	}
	stage := aggregateStageDirectory(configuration.Directory)
	if exists, err := aggregateDirectoryExists(stage); err != nil {
		return AggregateBundle{}, err
	} else if exists {
		if err := ensureDirectory(configuration.QuarantineDirectory); err != nil {
			return AggregateBundle{}, err
		}
		if _, err := quarantineAggregateStage(configuration, stage); err != nil {
			return AggregateBundle{}, err
		}
	}
	if err := ensureDirectory(configuration.QuarantineDirectory); err != nil {
		return AggregateBundle{}, err
	}
	root, _, err := createAggregateDirectory(stage)
	if err != nil {
		return AggregateBundle{}, err
	}
	rootOpen := true
	defer func() {
		if rootOpen {
			_ = root.Close()
		}
	}()
	resultPayload, err := aggregateCanonical(result)
	if err != nil || containsSensitiveBytes(resultPayload, configuration.SensitiveValues) {
		return AggregateBundle{}, errors.New("candidate review campaign result is invalid or sensitive")
	}
	resultFile, err := aggregateWriteExclusive(
		root, aggregateResultName, resultPayload, "candidate campaign result",
	)
	if err != nil {
		return AggregateBundle{}, err
	}
	reviewPayload := aggregateReview(result, evaluations, configuration.Directory)
	if containsSensitiveBytes(reviewPayload, configuration.SensitiveValues) {
		return AggregateBundle{}, errors.New("candidate review document contains a declared sensitive value")
	}
	reviewFile, err := aggregateWriteExclusive(
		root, aggregateReviewName, reviewPayload, "case-by-case human review",
	)
	if err != nil {
		return AggregateBundle{}, err
	}
	files := []AggregateFile{resultFile, reviewFile}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	fileSetSHA, err := aggregateFileSetDigest(files)
	if err != nil {
		return AggregateBundle{}, err
	}
	evaluationSetSHA, err := aggregateEvaluationSetDigest(evaluations)
	if err != nil {
		return AggregateBundle{}, err
	}
	manifest := AggregateManifest{
		Format: AggregateManifestFormat, FormatVersion: AggregateManifestFormatVersion,
		Complete: true, Source: result.Source, Provider: result.Provider,
		Expected: result.Expected, EvaluationCount: len(evaluations),
		EvaluationSetSHA256: evaluationSetSHA,
		ResultPath:          aggregateResultName, ResultSHA256: resultFile.SHA256,
		ReviewPath: aggregateReviewName, ReviewSHA256: reviewFile.SHA256,
		FileSetSHA256: fileSetSHA, Files: files, Evaluations: evaluations,
	}
	manifestPayload, err := aggregateCanonical(manifest)
	if err != nil || containsSensitiveBytes(manifestPayload, configuration.SensitiveValues) {
		return AggregateBundle{}, errors.New("candidate review aggregate manifest is invalid or sensitive")
	}
	manifestFile, err := aggregateWriteExclusive(
		root, aggregateManifestName, manifestPayload, "candidate campaign commit marker",
	)
	if err != nil {
		return AggregateBundle{}, err
	}
	if err := root.Close(); err != nil {
		rootOpen = false
		return AggregateBundle{}, errors.New("close candidate review aggregate stage")
	}
	rootOpen = false
	receipt := AggregateReceipt{
		Format: AggregateReceiptFormat, FormatVersion: AggregateReceiptFormatVersion,
		Directory: configuration.Directory, ManifestSHA256: manifestFile.SHA256,
		FileSetSHA256: fileSetSHA, SourceManifestSHA256: result.Source.ManifestSHA256,
		EvaluationSetSHA256: evaluationSetSHA, EvaluationCount: len(evaluations),
	}
	receiptPayload, err := aggregateReceiptPayload(receipt)
	if err != nil || containsSensitiveBytes(receiptPayload, configuration.SensitiveValues) {
		return AggregateBundle{}, errors.New("candidate review aggregate receipt is invalid or sensitive")
	}
	if err := writeAggregateExternal(configuration.ReceiptPath, receiptPayload); err != nil {
		return AggregateBundle{}, err
	}
	retainedReceipt, err := readAggregateReceipt(configuration.ReceiptPath)
	if err != nil {
		return AggregateBundle{}, err
	}
	if retainedReceipt.Directory != configuration.Directory ||
		retainedReceipt.ManifestSHA256 != manifestFile.SHA256 {
		return AggregateBundle{}, errors.New("candidate review aggregate receipt changed after publication")
	}
	if operations.afterReceipt != nil {
		if err := operations.afterReceipt(); err != nil {
			return AggregateBundle{}, err
		}
	}
	if _, err := verifyAggregateAt(ctx, configuration, retainedReceipt, stage); err != nil {
		return AggregateBundle{}, fmt.Errorf("verify candidate review aggregate stage: %w", err)
	}
	if err := promoteAggregateStage(configuration, stage); err != nil {
		return AggregateBundle{}, err
	}
	return verifyAggregateAt(ctx, configuration, retainedReceipt, configuration.Directory)
}

// VerifyAggregate reopens the external receipt, complete aggregate tree,
// source receipt, and every per-case evaluation receipt without a provider.
func VerifyAggregate(
	ctx context.Context, options AggregateOptions,
) (AggregateBundle, error) {
	if ctx == nil {
		return AggregateBundle{}, errors.New("verify candidate review aggregate: nil context")
	}
	if err := ctx.Err(); err != nil {
		return AggregateBundle{}, err
	}
	if err := validateAggregateOptions(options); err != nil {
		return AggregateBundle{}, err
	}
	receipt, err := readAggregateReceipt(options.ReceiptPath)
	if err != nil {
		return AggregateBundle{}, err
	}
	if receipt.Directory != options.Directory {
		return AggregateBundle{}, errors.New("candidate review aggregate receipt names another directory")
	}
	return verifyAggregateAt(ctx, options, receipt, options.Directory)
}

func verifyAggregateAt(
	ctx context.Context, options AggregateOptions, receipt AggregateReceipt, actualDirectory string,
) (AggregateBundle, error) {
	if err := ctx.Err(); err != nil {
		return AggregateBundle{}, err
	}
	if err := validateAggregateReceipt(receipt); err != nil {
		return AggregateBundle{}, err
	}
	root, identity, err := openAggregateDirectory(actualDirectory)
	if err != nil {
		return AggregateBundle{}, err
	}
	defer root.Close()
	manifestInfo, err := root.Lstat(aggregateManifestName)
	if err != nil || manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() ||
		manifestInfo.Size() <= 0 || manifestInfo.Size() > maximumAggregateFileBytes {
		return AggregateBundle{}, errors.New("candidate review aggregate manifest is invalid")
	}
	manifestPayload, err := aggregateReadRegular(root, aggregateManifestName, manifestInfo.Size())
	if err != nil || aggregateDigest(manifestPayload) != receipt.ManifestSHA256 {
		return AggregateBundle{}, errors.New("candidate review aggregate manifest differs from its receipt")
	}
	var manifest AggregateManifest
	if err := decodeAggregateCanonical(manifestPayload, &manifest); err != nil {
		return AggregateBundle{}, err
	}
	if err := validateAggregateManifest(manifest, receipt); err != nil {
		return AggregateBundle{}, err
	}
	actualFiles, err := walkAggregateFiles(root)
	if err != nil {
		return AggregateBundle{}, err
	}
	filtered := actualFiles[:0]
	for _, file := range actualFiles {
		if file.Path != aggregateManifestName {
			filtered = append(filtered, file)
		}
	}
	actualFiles = filtered
	if !reflect.DeepEqual(actualFiles, manifest.Files) {
		return AggregateBundle{}, errors.New("candidate review aggregate file set differs from its manifest")
	}
	fileSetSHA, err := aggregateFileSetDigest(actualFiles)
	if err != nil || fileSetSHA != manifest.FileSetSHA256 || fileSetSHA != receipt.FileSetSHA256 {
		return AggregateBundle{}, errors.New("candidate review aggregate file-set identity is invalid")
	}
	resultFile := aggregateFileByPath(actualFiles, aggregateResultName)
	reviewFile := aggregateFileByPath(actualFiles, aggregateReviewName)
	if resultFile.SHA256 != manifest.ResultSHA256 || reviewFile.SHA256 != manifest.ReviewSHA256 {
		return AggregateBundle{}, errors.New("candidate review aggregate result or review identity is invalid")
	}
	resultPayload, err := aggregateReadRegular(root, aggregateResultName, resultFile.SizeBytes)
	if err != nil {
		return AggregateBundle{}, err
	}
	var result Result
	if err := decodeAggregateCanonical(resultPayload, &result); err != nil {
		return AggregateBundle{}, err
	}
	if containsSensitiveBytes(resultPayload, options.SensitiveValues) {
		return AggregateBundle{}, errors.New("candidate review aggregate result contains a declared sensitive value")
	}
	if err := validateAggregateResultPaths(options, result); err != nil {
		return AggregateBundle{}, err
	}
	if aggregateDigest(resultPayload) != manifest.ResultSHA256 ||
		manifest.Source != result.Source || manifest.Provider != result.Provider ||
		manifest.Expected != result.Expected || manifest.EvaluationCount != len(result.Evaluations) {
		return AggregateBundle{}, errors.New("candidate review aggregate result differs from its manifest")
	}
	evaluations, err := verifyCampaignInputs(
		ctx, options.SourceReceiptPath, options.SensitiveValues, result,
	)
	if err != nil {
		return AggregateBundle{}, err
	}
	if !reflect.DeepEqual(evaluations, manifest.Evaluations) {
		return AggregateBundle{}, errors.New("candidate review aggregate evaluations differ from live receipts")
	}
	evaluationSetSHA, err := aggregateEvaluationSetDigest(evaluations)
	if err != nil || evaluationSetSHA != manifest.EvaluationSetSHA256 ||
		evaluationSetSHA != receipt.EvaluationSetSHA256 {
		return AggregateBundle{}, errors.New("candidate review aggregate evaluation-set identity is invalid")
	}
	reviewPayload, err := aggregateReadRegular(root, aggregateReviewName, reviewFile.SizeBytes)
	if err != nil || !bytes.Equal(reviewPayload, aggregateReview(result, evaluations, receipt.Directory)) {
		return AggregateBundle{}, errors.New("candidate review document differs from verified campaign inputs")
	}
	opened, openErr := root.Stat(".")
	visible, visibleErr := os.Lstat(actualDirectory)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(identity, opened) || !os.SameFile(opened, visible) {
		return AggregateBundle{}, errors.New("candidate review aggregate changed while verifying")
	}
	return AggregateBundle{Manifest: manifest, Result: cloneResult(result), Receipt: receipt}, nil
}

func verifyCampaignInputs(
	ctx context.Context, sourceReceiptPath string, sensitive []string, result Result,
) ([]AggregateEvaluation, error) {
	manifest, receipt, err := sourcebundle.Verify(ctx, result.Source.Directory, sourceReceiptPath)
	if err != nil {
		return nil, fmt.Errorf("verify candidate review source receipt: %w", err)
	}
	if receipt != result.Source || result.Expected != manifest.AttemptCount ||
		len(result.Evaluations) != result.Expected || result.Provider.Validate() != nil {
		return nil, errors.New("candidate review campaign identity differs from its source")
	}
	if len(result.Evaluations) == 0 {
		return nil, errors.New("candidate review campaign contains no evaluations")
	}
	seen := make(map[string]struct{}, len(result.Evaluations))
	evaluations := make([]AggregateEvaluation, len(result.Evaluations))
	for index, item := range result.Evaluations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := manifest.Attempts[index]
		if item.AttemptID != entry.AttemptID || item.Suite != entry.Suite || item.Case != entry.Case ||
			item.Trial != entry.Trial || !sameOutcome(item.Deterministic, entry.Deterministic) {
			return nil, errors.New("candidate review evaluation differs from source attempt order")
		}
		if _, duplicate := seen[item.AttemptID]; duplicate {
			return nil, errors.New("candidate review evaluation identity is duplicated")
		}
		seen[item.AttemptID] = struct{}{}
		external, err := review.ReadEvaluationBundleReceipt(ctx, item.ReceiptPath)
		if err != nil || external != item.Receipt {
			return nil, errors.New("candidate review evaluation receipt differs from its external store")
		}
		opened, err := review.VerifyEvaluationBundle(ctx, review.EvaluationBundleOptions{
			Directory: item.Receipt.Directory, SensitiveValues: slices.Clone(sensitive),
		}, item.Receipt)
		if err != nil {
			return nil, fmt.Errorf("verify candidate review evaluation %s: %w", item.Case, err)
		}
		if opened.Record.AttemptID != item.AttemptID || opened.Record.Suite != item.Suite ||
			opened.Record.Case != item.Case || opened.Record.Trial != item.Trial ||
			opened.Record.Provider != result.Provider ||
			!reflect.DeepEqual(opened.Record.Assessment, item.Assessment) {
			return nil, errors.New("candidate review evaluation record differs from campaign result")
		}
		var media []AggregateMedia
		for _, file := range opened.Manifest.Files {
			if file.MediaOrdinal > 0 {
				media = append(media, AggregateMedia{
					Ordinal:   file.MediaOrdinal,
					Path:      filepath.Join(item.Receipt.Directory, filepath.FromSlash(file.Path)),
					MediaType: file.MediaType, SHA256: file.SHA256, SizeBytes: file.SizeBytes,
				})
			}
		}
		if len(media) == 0 {
			return nil, errors.New("candidate review evaluation contains no retained media")
		}
		evaluations[index] = AggregateEvaluation{
			AttemptID: item.AttemptID, Suite: item.Suite, Case: item.Case, Trial: item.Trial,
			ReceiptPath: item.ReceiptPath, Receipt: item.Receipt,
			RecordSHA256: opened.Receipt.RecordSHA256, Media: media,
			Deterministic: deterministicLabel(item.Deterministic),
			Observed:      item.Assessment.ObservedOutcome, Agrees: item.Assessment.AgreesWithDeterministic,
			MediaUsable: item.Assessment.MediaUsable, Confidence: item.Assessment.Confidence,
			ProblemCount: len(item.Assessment.SignificantProblems), Assessment: item.Assessment,
		}
	}
	return canonicalAggregateEvaluations(evaluations), nil
}

func deterministicLabel(outcome bench.TaskOutcome) string {
	if !outcome.Completed {
		return "infrastructure_failure"
	}
	if outcome.Passed {
		return "pass"
	}
	return "fail"
}

func sameOutcome(left, right bench.TaskOutcome) bool {
	leftPayload, leftErr := aggregateCompact(left)
	rightPayload, rightErr := aggregateCompact(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func sameCampaignResult(left, right Result) bool {
	leftPayload, leftErr := aggregateCompact(left)
	rightPayload, rightErr := aggregateCompact(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func aggregateFileByPath(files []AggregateFile, path string) AggregateFile {
	for _, file := range files {
		if file.Path == path {
			return file
		}
	}
	return AggregateFile{}
}

func validateAggregateManifest(manifest AggregateManifest, receipt AggregateReceipt) error {
	if manifest.Format != AggregateManifestFormat ||
		manifest.FormatVersion != AggregateManifestFormatVersion || !manifest.Complete ||
		manifest.Expected <= 0 || manifest.EvaluationCount != manifest.Expected ||
		len(manifest.Evaluations) != manifest.Expected || manifest.Source.AttemptCount != manifest.Expected ||
		manifest.ResultPath != aggregateResultName || manifest.ReviewPath != aggregateReviewName ||
		manifest.ResultSHA256 == "" || manifest.ReviewSHA256 == "" ||
		manifest.FileSetSHA256 != receipt.FileSetSHA256 ||
		manifest.EvaluationSetSHA256 != receipt.EvaluationSetSHA256 ||
		manifest.Source.ManifestSHA256 != receipt.SourceManifestSHA256 ||
		manifest.EvaluationCount != receipt.EvaluationCount || len(manifest.Files) != 2 {
		return errors.New("candidate review aggregate manifest header is invalid")
	}
	if err := manifest.Provider.Validate(); err != nil {
		return errors.New("candidate review aggregate provider identity is invalid")
	}
	previous := ""
	for _, file := range manifest.Files {
		if !safeAggregateRelative(file.Path) || file.Path <= previous || file.Purpose == "" ||
			!validAggregateDigest(file.SHA256) || file.SizeBytes <= 0 ||
			file.SizeBytes > maximumAggregateFileBytes {
			return errors.New("candidate review aggregate manifest file entry is invalid")
		}
		previous = file.Path
	}
	return nil
}

func validateAggregateReceipt(receipt AggregateReceipt) error {
	if receipt.Format != AggregateReceiptFormat ||
		receipt.FormatVersion != AggregateReceiptFormatVersion ||
		receipt.Directory == "" || !filepath.IsAbs(receipt.Directory) ||
		filepath.Clean(receipt.Directory) != receipt.Directory || receipt.EvaluationCount <= 0 {
		return errors.New("candidate review aggregate receipt identity is invalid")
	}
	payload, err := aggregateReceiptPayload(receipt)
	if err != nil {
		return err
	}
	var canonical AggregateReceipt
	if err := decodeAggregateCanonical(payload, &canonical); err != nil ||
		canonical.ReceiptSHA256 != receipt.ReceiptSHA256 {
		return errors.New("candidate review aggregate receipt digest is invalid")
	}
	for _, value := range []string{
		receipt.ManifestSHA256, receipt.FileSetSHA256, receipt.SourceManifestSHA256,
		receipt.EvaluationSetSHA256, receipt.ReceiptSHA256,
	} {
		if !validAggregateDigest(value) {
			return errors.New("candidate review aggregate receipt contains an invalid digest")
		}
	}
	return nil
}

func validAggregateDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32
}

func readAggregateReceipt(path string) (AggregateReceipt, error) {
	payload, err := readAggregateExternal(path)
	if err != nil {
		return AggregateReceipt{}, err
	}
	var receipt AggregateReceipt
	if err := decodeAggregateCanonical(payload, &receipt); err != nil {
		return AggregateReceipt{}, err
	}
	if err := validateAggregateReceipt(receipt); err != nil {
		return AggregateReceipt{}, err
	}
	return receipt, nil
}

func aggregateReceiptExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > maximumAggregateJSON {
		return false, errors.New("candidate review aggregate receipt path is invalid")
	}
	return true, nil
}

func aggregateDirectoryExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("candidate review aggregate path is invalid")
	}
	return true, nil
}

func prepareAggregateOptions(options AggregateOptions, result Result) (AggregateOptions, error) {
	if err := validateAggregateOptions(options); err != nil {
		return AggregateOptions{}, err
	}
	if result.Source.Directory == "" || len(result.Evaluations) == 0 {
		return AggregateOptions{}, errors.New("candidate review campaign result is empty")
	}
	if err := validateAggregateResultPaths(options, result); err != nil {
		return AggregateOptions{}, err
	}
	options.SensitiveValues = slices.Clone(options.SensitiveValues)
	return options, nil
}

func validateAggregateResultPaths(options AggregateOptions, result Result) error {
	if err := validateAbsolutePath("aggregate source directory", result.Source.Directory); err != nil {
		return err
	}
	paths := []string{
		options.Directory, options.ReceiptPath, options.SourceReceiptPath,
		options.QuarantineDirectory, result.Source.Directory,
	}
	for _, evaluation := range result.Evaluations {
		for _, item := range []struct{ label, path string }{
			{"aggregate evaluation receipt", evaluation.ReceiptPath},
			{"aggregate evaluation directory", evaluation.Receipt.Directory},
		} {
			if err := validateAbsolutePath(item.label, item.path); err != nil {
				return err
			}
			if containsSensitive(item.path, options.SensitiveValues) {
				return errors.New("candidate review aggregate evaluation path contains a declared sensitive value")
			}
			paths = append(paths, item.path)
		}
	}
	if containsSensitive(result.Source.Directory, options.SensitiveValues) {
		return errors.New("candidate review aggregate source path contains a declared sensitive value")
	}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if pathsOverlap(paths[left], paths[right]) {
				return errors.New("candidate review aggregate source, receipt, evaluation, and publication paths must be disjoint")
			}
		}
	}
	return nil
}

func validateAggregateOptions(options AggregateOptions) error {
	if err := validateAggregateOptionPaths(options); err != nil {
		return err
	}
	if err := validateSensitiveValues(options.SensitiveValues); err != nil {
		return err
	}
	for _, path := range []string{
		options.Directory, options.ReceiptPath, options.SourceReceiptPath, options.QuarantineDirectory,
	} {
		if containsSensitive(path, options.SensitiveValues) {
			return errors.New("candidate review aggregate path contains a declared sensitive value")
		}
	}
	return nil
}

func validateAggregateOptionPaths(options AggregateOptions) error {
	for _, item := range []struct{ label, path string }{
		{"directory", options.Directory}, {"receipt", options.ReceiptPath},
		{"source receipt", options.SourceReceiptPath}, {"quarantine", options.QuarantineDirectory},
	} {
		if err := validateAbsolutePath("aggregate "+item.label, item.path); err != nil {
			return err
		}
	}
	paths := []string{options.Directory, options.ReceiptPath, options.SourceReceiptPath, options.QuarantineDirectory}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if pathsOverlap(paths[left], paths[right]) {
				return errors.New("candidate review aggregate paths must be disjoint")
			}
		}
	}
	return nil
}

func containsSensitiveBytes(payload []byte, sensitive []string) bool {
	return containsSensitive(string(payload), sensitive)
}

func recoverAggregate(
	ctx context.Context, options AggregateOptions, receipt AggregateReceipt,
) (AggregateBundle, error) {
	if receipt.Directory != options.Directory {
		return AggregateBundle{}, errors.New("candidate review aggregate receipt names another target")
	}
	stage := aggregateStageDirectory(options.Directory)
	finalExists, err := aggregateDirectoryExists(options.Directory)
	if err != nil {
		return AggregateBundle{}, err
	}
	stageExists, err := aggregateDirectoryExists(stage)
	if err != nil {
		return AggregateBundle{}, err
	}
	if finalExists && stageExists {
		return AggregateBundle{}, errors.New("candidate review aggregate final and stage both exist")
	}
	if finalExists {
		return verifyAggregateAt(ctx, options, receipt, options.Directory)
	}
	if !stageExists {
		return AggregateBundle{}, errors.New("candidate review aggregate receipt has no final or staged directory")
	}
	if _, err := verifyAggregateAt(ctx, options, receipt, stage); err != nil {
		return AggregateBundle{}, err
	}
	if err := promoteAggregateStage(options, stage); err != nil {
		return AggregateBundle{}, err
	}
	return verifyAggregateAt(ctx, options, receipt, options.Directory)
}

func promoteAggregateStage(options AggregateOptions, stage string) error {
	parentPath := filepath.Dir(options.Directory)
	if filepath.Dir(stage) != parentPath {
		return errors.New("candidate review aggregate stage is not a sibling")
	}
	parent, identity, err := openAggregateParent(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	stageName, finalName := filepath.Base(stage), filepath.Base(options.Directory)
	stageInfo, err := parent.Lstat(stageName)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() {
		return errors.New("candidate review aggregate stage is invalid")
	}
	if _, err := parent.Lstat(finalName); !os.IsNotExist(err) {
		return errors.New("candidate review aggregate final appeared before promotion")
	}
	if err := renameAggregateNoReplace(
		parentPath, stageName, identity, parentPath, finalName, identity,
	); err != nil {
		return errors.New("promote candidate review aggregate exclusively")
	}
	if err := syncAggregateDirectory(parent, "."); err != nil {
		return err
	}
	finalInfo, finalErr := parent.Lstat(finalName)
	_, stageErr := parent.Lstat(stageName)
	if finalErr != nil || finalInfo.Mode()&os.ModeSymlink != 0 || !finalInfo.IsDir() ||
		!os.SameFile(stageInfo, finalInfo) || !os.IsNotExist(stageErr) {
		return errors.New("candidate review aggregate identity changed during promotion")
	}
	return nil
}

func quarantineAggregateStage(options AggregateOptions, stage string) (string, error) {
	sourceParentPath := filepath.Dir(options.Directory)
	sourceParent, sourceIdentity, err := openAggregateParent(sourceParentPath)
	if err != nil {
		return "", err
	}
	defer sourceParent.Close()
	destination, destinationIdentity, err := openAggregateParent(options.QuarantineDirectory)
	if err != nil {
		return "", err
	}
	defer destination.Close()
	stageName := filepath.Base(stage)
	stageInfo, err := sourceParent.Lstat(stageName)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() {
		return "", errors.New("candidate review aggregate abandoned stage is invalid")
	}
	name := ""
	for index := 1; index <= 10_000; index++ {
		candidate := fmt.Sprintf("candidate-campaign-abandoned-%s-%06d", digestName(stage)[:24], index)
		if _, err := destination.Lstat(candidate); os.IsNotExist(err) {
			name = candidate
			break
		} else if err != nil {
			return "", errors.New("inspect candidate review aggregate quarantine")
		}
	}
	if name == "" {
		return "", errors.New("candidate review aggregate quarantine namespace is exhausted")
	}
	if err := renameAggregateNoReplace(
		sourceParentPath, stageName, sourceIdentity,
		options.QuarantineDirectory, name, destinationIdentity,
	); err != nil {
		return "", errors.New("quarantine candidate review aggregate stage exclusively")
	}
	if err := errors.Join(
		syncAggregateDirectory(sourceParent, "."), syncAggregateDirectory(destination, "."),
	); err != nil {
		return "", err
	}
	retained, retainedErr := destination.Lstat(name)
	_, stageErr := sourceParent.Lstat(stageName)
	if retainedErr != nil || !os.SameFile(stageInfo, retained) || !os.IsNotExist(stageErr) {
		return "", errors.New("candidate review aggregate quarantine identity changed")
	}
	return filepath.Join(options.QuarantineDirectory, name), nil
}
