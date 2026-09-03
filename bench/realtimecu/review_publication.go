package realtimecu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	revieweval "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const maximumReviewBundleReceiptBytes = 64 << 10

// ReviewBundleVerificationOptions names the four independently retained
// parts of one Realtime-CU review publication. All paths must be clean,
// absolute, disjoint, and free of symlinked ancestors.
type ReviewBundleVerificationOptions struct {
	Directory                  string
	ReceiptPath                string
	SourceReceiptPath          string
	EvaluationReceiptDirectory string
}

// ReviewBundleVerification is the credential-free result of reopening the
// final receipt, deterministic-source receipt, final sealed tree, and every
// externally retained evaluation receipt. EvidenceComplete describes
// structural population coverage only: it requires the exact sixteen-case
// reportable evidence population, but makes no behavioral acceptance decision.
type ReviewBundleVerification struct {
	Receipt            ReviewBundleReceipt
	SourceReceipt      ReviewSourceReceipt
	Manifest           ReviewManifest
	EvaluationReceipts map[string]revieweval.EvaluationBundleReceipt
	EvidenceComplete   bool
}

// WriteReviewBundleReceipt durably publishes a canonical final receipt
// outside the sealed review tree. It verifies the tree both before and after
// the create-only write; a failed call removes only the file created by this
// invocation.
func WriteReviewBundleReceipt(
	ctx context.Context, path string, receipt ReviewBundleReceipt,
) (resultErr error) {
	if ctx == nil {
		return errors.New("write realtime computer-use review receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err := validateReviewBundleReceipt(receipt); err != nil {
		return err
	}
	if receipt.Directory == filepath.Dir(receipt.Directory) {
		return errors.New("realtime computer-use review receipt directory is a filesystem root")
	}
	if _, err := VerifyReviewBundleReceipt(receipt.Directory, receipt); err != nil {
		return fmt.Errorf("verify realtime computer-use review before receipt publication: %w", err)
	}
	parentPath, name, err := validateExternalReviewBundleReceiptPath(
		path, receipt.Directory, false,
	)
	if err != nil {
		return err
	}
	payload, err := encodeReviewBundleReceipt(receipt)
	if err != nil {
		return err
	}
	parent, parentIdentity, err := openReviewReceiptParent(parentPath)
	if err != nil {
		return err
	}
	owned, committed := false, false
	defer func() {
		if owned && !committed {
			_ = parent.Remove(name)
			_ = syncReviewReceiptParent(parent)
		}
		if closeErr := parent.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use review receipt parent"))
		}
	}()
	file, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return errors.New("create realtime computer-use review receipt exclusively")
	}
	owned = true
	written := 0
	for written < len(payload) {
		count, writeErr := file.Write(payload[written:])
		if writeErr != nil || count <= 0 {
			_ = file.Close()
			return errors.New("write realtime computer-use review receipt")
		}
		written += count
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync realtime computer-use review receipt")
	}
	if err := file.Close(); err != nil {
		_ = file.Close()
		return errors.New("close realtime computer-use review receipt")
	}
	if err := syncReviewReceiptParent(parent); err != nil {
		return errors.New("sync realtime computer-use review receipt parent")
	}
	retainedPayload, retainedInfo, err := readReviewFile(
		parent, name, maximumReviewBundleReceiptBytes,
	)
	if err != nil || !bytes.Equal(retainedPayload, payload) ||
		retainedInfo.Mode()&os.ModeSymlink != 0 || retainedInfo.Mode().Perm()&0o222 != 0 ||
		reviewFileHasMultipleLinks(retainedInfo) {
		return errors.New("reopen realtime computer-use review receipt")
	}
	retained, err := decodeReviewBundleReceipt(retainedPayload)
	if err != nil || retained != receipt {
		return errors.New("verify retained realtime computer-use review receipt")
	}
	if err := verifyReviewReceiptParentIdentity(parentPath, parent, parentIdentity); err != nil {
		return err
	}
	if _, err := VerifyReviewBundleReceipt(receipt.Directory, retained); err != nil {
		return fmt.Errorf("reverify realtime computer-use review after receipt publication: %w", err)
	}
	committed = true
	return nil
}

// ReadReviewBundleReceipt strictly reopens one canonical, single-link final
// receipt. It does not verify the named review tree or the other external
// anchors; use VerifyReviewBundlePublication for that.
func ReadReviewBundleReceipt(
	ctx context.Context, path string,
) (receipt ReviewBundleReceipt, resultErr error) {
	if ctx == nil {
		return ReviewBundleReceipt{}, errors.New("read realtime computer-use review receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return ReviewBundleReceipt{}, cause
	}
	parentPath, name, err := validateExternalReviewBundleReceiptPath(path, "", true)
	if err != nil {
		return ReviewBundleReceipt{}, err
	}
	parent, identity, err := openReviewReceiptParent(parentPath)
	if err != nil {
		return ReviewBundleReceipt{}, err
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			receipt = ReviewBundleReceipt{}
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use review receipt parent"))
		}
	}()
	payload, info, err := readReviewFile(parent, name, maximumReviewBundleReceiptBytes)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 ||
		reviewFileHasMultipleLinks(info) {
		return ReviewBundleReceipt{}, errors.New("read realtime computer-use review receipt")
	}
	receipt, err = decodeReviewBundleReceipt(payload)
	if err != nil {
		return ReviewBundleReceipt{}, err
	}
	if _, _, err := validateExternalReviewBundleReceiptPath(path, receipt.Directory, true); err != nil {
		return ReviewBundleReceipt{}, err
	}
	if err := verifyReviewReceiptParentIdentity(parentPath, parent, identity); err != nil {
		return ReviewBundleReceipt{}, err
	}
	return receipt, nil
}

// VerifyReviewBundlePublication performs credential-free, fail-closed review
// verification. It reopens both external receipt levels, verifies the final
// manifest and all nested media/evaluation bundles, requires the external
// evaluation directory to contain exactly the manifest's receipt population
// (plus its optional canonical publication-lock markers), and then repeats
// the anchors and sealed-tree checks to detect inter-pass replacement.
func VerifyReviewBundlePublication(
	ctx context.Context, options ReviewBundleVerificationOptions,
) (ReviewBundleVerification, error) {
	if ctx == nil {
		return ReviewBundleVerification{},
			errors.New("verify realtime computer-use review publication: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return ReviewBundleVerification{}, cause
	}
	if err := validateReviewBundleVerificationOptions(options); err != nil {
		return ReviewBundleVerification{}, err
	}
	receipt, err := ReadReviewBundleReceipt(ctx, options.ReceiptPath)
	if err != nil {
		return ReviewBundleVerification{}, fmt.Errorf("read final realtime computer-use review receipt: %w", err)
	}
	return verifyReviewBundlePublicationAgainstReceipt(ctx, options, receipt, true)
}

// PublishReviewBundleReceipt derives the final receipt from an already sealed
// review bundle and its existing external source/evaluation anchors. It runs a
// complete credential-free verification before publishing the receipt
// create-only, then reopens the newly anchored publication. This is the safe
// migration path for review bundles sealed before outer receipts were emitted.
func PublishReviewBundleReceipt(
	ctx context.Context, options ReviewBundleVerificationOptions,
) (ReviewBundleVerification, error) {
	if ctx == nil {
		return ReviewBundleVerification{},
			errors.New("publish realtime computer-use review receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return ReviewBundleVerification{}, cause
	}
	if err := validateReviewBundleVerificationOptions(options); err != nil {
		return ReviewBundleVerification{}, err
	}
	if _, _, err := validateExternalReviewBundleReceiptPath(
		options.ReceiptPath, options.Directory, false,
	); err != nil {
		return ReviewBundleVerification{}, err
	}
	receipt, err := deriveReviewBundleReceipt(ctx, options.Directory, options.SourceReceiptPath)
	if err != nil {
		return ReviewBundleVerification{}, err
	}
	if _, err := verifyReviewBundlePublicationAgainstReceipt(
		ctx, options, receipt, false,
	); err != nil {
		return ReviewBundleVerification{},
			fmt.Errorf("verify realtime computer-use review before outer receipt publication: %w", err)
	}
	if err := WriteReviewBundleReceipt(ctx, options.ReceiptPath, receipt); err != nil {
		return ReviewBundleVerification{}, err
	}
	return VerifyReviewBundlePublication(ctx, options)
}

func verifyReviewBundlePublicationAgainstReceipt(
	ctx context.Context, options ReviewBundleVerificationOptions,
	receipt ReviewBundleReceipt, requireOuterReceipt bool,
) (verified ReviewBundleVerification, resultErr error) {
	if err := validateReviewBundleReceipt(receipt); err != nil {
		return ReviewBundleVerification{}, err
	}

	reviewInfo, err := os.Lstat(options.Directory)
	if err != nil || reviewInfo.Mode()&os.ModeSymlink != 0 || !reviewInfo.IsDir() {
		return ReviewBundleVerification{}, errors.New("realtime computer-use review directory is invalid")
	}
	evaluationRoot, evaluationIdentity, err := openReviewDirectoryRoot(
		options.EvaluationReceiptDirectory,
	)
	if err != nil {
		return ReviewBundleVerification{},
			errors.New("open realtime computer-use external evaluation receipt directory")
	}
	defer func() {
		if closeErr := evaluationRoot.Close(); closeErr != nil {
			verified = ReviewBundleVerification{}
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use external evaluation receipt directory"))
		}
	}()
	if os.SameFile(reviewInfo, evaluationIdentity) {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use review and evaluation receipt directories alias")
	}

	if receipt.Directory != options.Directory {
		return ReviewBundleVerification{}, errors.New("realtime computer-use review receipt names another directory")
	}
	var outerIdentity os.FileInfo
	if requireOuterReceipt {
		before, statErr := os.Lstat(options.ReceiptPath)
		retained, readErr := ReadReviewBundleReceipt(ctx, options.ReceiptPath)
		outerIdentity, err = os.Lstat(options.ReceiptPath)
		if statErr != nil || readErr != nil || err != nil || retained != receipt ||
			!os.SameFile(before, outerIdentity) {
			return ReviewBundleVerification{},
				errors.New("realtime computer-use final review receipt changed before verification")
		}
	}
	sourceBefore, sourceBeforeErr := os.Lstat(options.SourceReceiptPath)
	sourceReceipt, err := ReadReviewSourceReceipt(ctx, options.SourceReceiptPath)
	if sourceBeforeErr != nil || err != nil {
		return ReviewBundleVerification{}, fmt.Errorf("read realtime computer-use source receipt: %w", err)
	}
	if sourceReceipt.Directory != options.Directory ||
		sourceReceipt.ManifestSHA256 != receipt.SourceManifestSHA256 ||
		sourceReceipt.ReceiptSHA256 != receipt.SourceReceiptSHA256 {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use source receipt differs from the final receipt")
	}
	sourceIdentity, err := os.Lstat(options.SourceReceiptPath)
	if err != nil || !os.SameFile(sourceBefore, sourceIdentity) {
		return ReviewBundleVerification{}, errors.New("inspect realtime computer-use source receipt identity")
	}
	if requireOuterReceipt {
		if same, err := sameVisibleReviewFile(options.ReceiptPath, options.SourceReceiptPath); err != nil {
			return ReviewBundleVerification{}, err
		} else if same {
			return ReviewBundleVerification{},
				errors.New("realtime computer-use final and source receipts alias one file")
		}
	}

	manifest, err := VerifyReviewBundleReceipt(options.Directory, receipt)
	if err != nil {
		return ReviewBundleVerification{}, fmt.Errorf("verify final realtime computer-use review bundle: %w", err)
	}
	sourceManifest, err := VerifyReviewSourceReceipt(options.Directory, sourceReceipt)
	if err != nil {
		return ReviewBundleVerification{}, fmt.Errorf("verify realtime computer-use source bundle: %w", err)
	}
	if manifest.SourceManifest == nil ||
		manifest.SourceManifest.SHA256 != sourceReceipt.ManifestSHA256 ||
		!reflect.DeepEqual(sourceManifest.Cell, manifest.Cell) ||
		!reflect.DeepEqual(sourceManifest.Provenance, manifest.Provenance) ||
		!reflect.DeepEqual(sourceManifest.Result, manifest.Result) {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use final manifest differs from its source receipt")
	}

	expectedNames, expectedReceipts, err := expectedExternalReviewEvaluations(
		options.Directory, manifest,
	)
	if err != nil {
		return ReviewBundleVerification{}, err
	}
	if err := verifyExternalEvaluationDirectory(
		options.EvaluationReceiptDirectory, evaluationRoot, evaluationIdentity, expectedNames,
	); err != nil {
		return ReviewBundleVerification{}, err
	}
	openedReceipts := make(map[string]revieweval.EvaluationBundleReceipt, len(expectedReceipts))
	receiptIdentities := make(map[string]os.FileInfo, len(expectedReceipts))
	for caseID, expected := range expectedReceipts {
		if cause := context.Cause(ctx); cause != nil {
			return ReviewBundleVerification{}, cause
		}
		name := expectedNames[caseID]
		path := filepath.Join(options.EvaluationReceiptDirectory, name)
		before, beforeErr := os.Lstat(path)
		external, err := revieweval.ReadEvaluationBundleReceipt(ctx, path)
		after, afterErr := os.Lstat(path)
		if beforeErr != nil || err != nil || afterErr != nil || !os.SameFile(before, after) {
			return ReviewBundleVerification{},
				fmt.Errorf("read external realtime computer-use evaluation receipt for %s: %w", caseID, err)
		}
		if external != expected {
			return ReviewBundleVerification{},
				fmt.Errorf("external realtime computer-use evaluation receipt differs for %s", caseID)
		}
		opened, err := revieweval.VerifyEvaluationBundle(
			ctx, revieweval.EvaluationBundleOptions{Directory: expected.Directory}, external,
		)
		if err != nil {
			return ReviewBundleVerification{},
				fmt.Errorf("verify nested realtime computer-use evaluation bundle for %s: %w", caseID, err)
		}
		if opened.Receipt != external {
			return ReviewBundleVerification{},
				fmt.Errorf("nested realtime computer-use evaluation receipt differs for %s", caseID)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			reviewFileHasMultipleLinks(info) || !os.SameFile(after, info) {
			return ReviewBundleVerification{},
				fmt.Errorf("external realtime computer-use evaluation receipt identity is invalid for %s", caseID)
		}
		for otherCase, otherInfo := range receiptIdentities {
			if os.SameFile(info, otherInfo) {
				return ReviewBundleVerification{}, fmt.Errorf(
					"external realtime computer-use evaluation receipts for %s and %s alias",
					caseID, otherCase,
				)
			}
		}
		receiptIdentities[caseID] = info
		openedReceipts[caseID] = external
	}

	// Repeat all independently mutable anchors after nested verification. A
	// receipt or directory swapped between the two passes cannot certify a
	// mixture of states.
	if err := verifyExternalEvaluationDirectory(
		options.EvaluationReceiptDirectory, evaluationRoot, evaluationIdentity, expectedNames,
	); err != nil {
		return ReviewBundleVerification{}, err
	}
	for caseID, expected := range openedReceipts {
		path := filepath.Join(options.EvaluationReceiptDirectory, expectedNames[caseID])
		reopened, err := revieweval.ReadEvaluationBundleReceipt(ctx, path)
		info, statErr := os.Lstat(path)
		if err != nil || statErr != nil || reopened != expected ||
			!os.SameFile(info, receiptIdentities[caseID]) {
			return ReviewBundleVerification{},
				fmt.Errorf("external realtime computer-use evaluation receipt changed for %s", caseID)
		}
	}
	reopenedSource, sourceErr := ReadReviewSourceReceipt(ctx, options.SourceReceiptPath)
	reopenedSourceInfo, sourceStatErr := os.Lstat(options.SourceReceiptPath)
	if sourceErr != nil || sourceStatErr != nil || reopenedSource != sourceReceipt ||
		!os.SameFile(sourceIdentity, reopenedSourceInfo) {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use external source receipt changed during verification")
	}
	if requireOuterReceipt {
		reopenedReceipt, receiptErr := ReadReviewBundleReceipt(ctx, options.ReceiptPath)
		reopenedOuterInfo, outerStatErr := os.Lstat(options.ReceiptPath)
		if receiptErr != nil || outerStatErr != nil || reopenedReceipt != receipt ||
			!os.SameFile(outerIdentity, reopenedOuterInfo) {
			return ReviewBundleVerification{},
				errors.New("realtime computer-use external final receipt changed during verification")
		}
	}
	reopenedManifest, err := VerifyReviewBundleReceipt(options.Directory, receipt)
	if err != nil || !reflect.DeepEqual(reopenedManifest, manifest) {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use final review bundle changed during external verification")
	}
	if _, err := VerifyReviewSourceReceipt(options.Directory, sourceReceipt); err != nil {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use source bundle changed during external verification")
	}
	reviewAfter, err := os.Lstat(options.Directory)
	if err != nil || reviewAfter.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(reviewInfo, reviewAfter) {
		return ReviewBundleVerification{},
			errors.New("realtime computer-use review directory changed during external verification")
	}

	return ReviewBundleVerification{
		Receipt: receipt, SourceReceipt: sourceReceipt, Manifest: manifest,
		EvaluationReceipts: cloneReviewEvaluationReceiptMap(openedReceipts),
		EvidenceComplete:   exactReviewPublicationComplete(manifest, openedReceipts),
	}, nil
}

func deriveReviewBundleReceipt(
	ctx context.Context, directory, sourceReceiptPath string,
) (ReviewBundleReceipt, error) {
	if cause := context.Cause(ctx); cause != nil {
		return ReviewBundleReceipt{}, cause
	}
	sourceReceipt, err := ReadReviewSourceReceipt(ctx, sourceReceiptPath)
	if err != nil {
		return ReviewBundleReceipt{}, fmt.Errorf("read realtime computer-use source receipt for anchoring: %w", err)
	}
	if sourceReceipt.Directory != directory {
		return ReviewBundleReceipt{}, errors.New("realtime computer-use source receipt names another review directory")
	}
	root, identity, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return ReviewBundleReceipt{}, err
	}
	payload, _, readErr := readReviewFile(root, "manifest.json", maximumReviewManifestBytes)
	closeErr := root.Close()
	if readErr != nil || closeErr != nil {
		return ReviewBundleReceipt{},
			errors.New("read sealed realtime computer-use final manifest for receipt anchoring")
	}
	manifestSHA := reviewDigest(payload)
	receipt := ReviewBundleReceipt{
		Directory: directory, ManifestSHA256: manifestSHA,
		SourceManifestSHA256: sourceReceipt.ManifestSHA256,
		SourceReceiptSHA256:  sourceReceipt.ReceiptSHA256,
	}
	if _, err := VerifyReviewBundleReceipt(directory, receipt); err != nil {
		return ReviewBundleReceipt{},
			fmt.Errorf("derive realtime computer-use review receipt from sealed bundle: %w", err)
	}
	current, err := os.Lstat(directory)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, current) {
		return ReviewBundleReceipt{}, errors.New("realtime computer-use review directory changed while deriving receipt")
	}
	return receipt, nil
}

func validateReviewBundleReceipt(receipt ReviewBundleReceipt) error {
	if _, err := validateNewReviewDirectory(receipt.Directory); err != nil {
		return errors.New("realtime computer-use review receipt directory is invalid")
	}
	for _, value := range []string{
		receipt.ManifestSHA256, receipt.SourceManifestSHA256, receipt.SourceReceiptSHA256,
	} {
		if err := validateReviewDigest(value); err != nil {
			return errors.New("realtime computer-use review receipt digest is invalid")
		}
	}
	return nil
}

func encodeReviewBundleReceipt(receipt ReviewBundleReceipt) ([]byte, error) {
	if err := validateReviewBundleReceipt(receipt); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) >= maximumReviewBundleReceiptBytes {
		return nil, errors.New("encode realtime computer-use review receipt")
	}
	payload = append(payload, '\n')
	if strictjson.Validate(payload) != nil {
		return nil, errors.New("encode strict realtime computer-use review receipt")
	}
	return payload, nil
}

func decodeReviewBundleReceipt(payload []byte) (ReviewBundleReceipt, error) {
	if len(payload) == 0 || len(payload) > maximumReviewBundleReceiptBytes ||
		strictjson.Validate(payload) != nil {
		return ReviewBundleReceipt{}, errors.New("realtime computer-use review receipt is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var receipt ReviewBundleReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return ReviewBundleReceipt{}, errors.New("decode realtime computer-use review receipt")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ReviewBundleReceipt{}, errors.New("realtime computer-use review receipt has trailing data")
	}
	canonical, err := encodeReviewBundleReceipt(receipt)
	if err != nil || !bytes.Equal(canonical, payload) {
		return ReviewBundleReceipt{}, errors.New("realtime computer-use review receipt is noncanonical or invalid")
	}
	return receipt, nil
}

func validateExternalReviewBundleReceiptPath(
	path, reviewDirectory string, mustExist bool,
) (string, string, error) {
	if _, err := validateNewReviewDirectory(path); err != nil || path == filepath.Dir(path) {
		return "", "", errors.New("realtime computer-use review receipt path must be clean, absolute, and non-root")
	}
	if reviewDirectory != "" {
		if _, err := validateNewReviewDirectory(reviewDirectory); err != nil {
			return "", "", errors.New("realtime computer-use review receipt bundle path is invalid")
		}
		if reviewPathsOverlap(path, reviewDirectory) {
			return "", "", errors.New("realtime computer-use review receipt must be outside its bundle")
		}
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	if _, _, err := openAndCloseReviewReceiptParent(parent); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(path)
	if mustExist {
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			reviewFileHasMultipleLinks(info) {
			return "", "", errors.New("realtime computer-use review receipt must be a single-link regular file")
		}
	} else if err == nil {
		return "", "", errors.New("realtime computer-use review receipt already exists")
	} else if !os.IsNotExist(err) {
		return "", "", errors.New("inspect realtime computer-use review receipt destination")
	}
	return parent, name, nil
}

func openAndCloseReviewReceiptParent(path string) (*os.Root, os.FileInfo, error) {
	root, identity, err := openReviewReceiptParent(path)
	if err != nil {
		return nil, nil, errors.New("realtime computer-use review receipt parent is invalid")
	}
	if err := root.Close(); err != nil {
		return nil, nil, errors.New("close realtime computer-use review receipt parent validation")
	}
	return nil, identity, nil
}

func validateReviewBundleVerificationOptions(options ReviewBundleVerificationOptions) error {
	paths := []string{
		options.Directory, options.ReceiptPath, options.SourceReceiptPath,
		options.EvaluationReceiptDirectory,
	}
	for _, path := range paths {
		if _, err := validateNewReviewDirectory(path); err != nil || path == filepath.Dir(path) {
			return errors.New("realtime computer-use review verification paths must be clean, absolute, and non-root")
		}
	}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if reviewPathsOverlap(paths[left], paths[right]) {
				return errors.New("realtime computer-use review verification paths must be disjoint")
			}
		}
	}
	if err := verifyReviewNoSymlinkAncestors(options.Directory); err != nil {
		return errors.New("realtime computer-use review directory has a symlinked or invalid ancestor")
	}
	if err := verifyReviewNoSymlinkAncestors(options.EvaluationReceiptDirectory); err != nil {
		return errors.New("realtime computer-use evaluation receipts have a symlinked or invalid ancestor")
	}
	return nil
}

func reviewPathsOverlap(left, right string) bool {
	separator := string(filepath.Separator)
	return left == right || strings.HasPrefix(left, right+separator) ||
		strings.HasPrefix(right, left+separator)
}

func sameVisibleReviewFile(left, right string) (bool, error) {
	leftInfo, leftErr := os.Lstat(left)
	rightInfo, rightErr := os.Lstat(right)
	if leftErr != nil || rightErr != nil || leftInfo.Mode()&os.ModeSymlink != 0 ||
		rightInfo.Mode()&os.ModeSymlink != 0 || !leftInfo.Mode().IsRegular() ||
		!rightInfo.Mode().IsRegular() {
		return false, errors.New("realtime computer-use external receipt identity is invalid")
	}
	return os.SameFile(leftInfo, rightInfo), nil
}

func expectedExternalReviewEvaluations(
	directory string, manifest ReviewManifest,
) (map[string]string, map[string]revieweval.EvaluationBundleReceipt, error) {
	names := make(map[string]string, len(manifest.Attempts))
	receipts := make(map[string]revieweval.EvaluationBundleReceipt, len(manifest.Attempts))
	seenNames := make(map[string]struct{}, len(manifest.Attempts))
	for _, attempt := range manifest.Attempts {
		if attempt.EvaluationBundle == nil {
			if attempt.ReviewStatus == "complete" {
				return nil, nil, errors.New("complete realtime computer-use review has no evaluation receipt")
			}
			continue
		}
		if attempt.ReviewStatus != "complete" {
			return nil, nil, errors.New("realtime computer-use diagnostic attempt claims an evaluation receipt")
		}
		directoryPath := filepath.Join(
			directory, filepath.FromSlash(attempt.EvaluationBundle.Path),
		)
		name := filepath.Base(directoryPath) + ".receipt.json"
		if _, duplicate := seenNames[name]; duplicate {
			return nil, nil, errors.New("realtime computer-use evaluation receipt name is duplicated")
		}
		seenNames[name] = struct{}{}
		names[attempt.Case] = name
		receipts[attempt.Case] = evaluationReceiptAt(directoryPath, *attempt.EvaluationBundle)
	}
	return names, receipts, nil
}

func verifyExternalEvaluationDirectory(
	path string, root *os.Root, expectedIdentity os.FileInfo, expectedNames map[string]string,
) error {
	if root == nil || expectedIdentity == nil {
		return errors.New("realtime computer-use external evaluation receipt directory is unavailable")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open realtime computer-use external evaluation receipt directory")
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("read realtime computer-use external evaluation receipt directory")
	}
	wanted := make(map[string]struct{}, len(expectedNames))
	lockNames := make(map[string]struct{}, len(expectedNames))
	for _, name := range expectedNames {
		wanted[name] = struct{}{}
		lockNames[name+".publication.lock"] = struct{}{}
	}
	seen := make(map[string]struct{}, len(expectedNames))
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			info.Mode().Perm()&0o077 != 0 || reviewFileHasMultipleLinks(info) {
			return errors.New("realtime computer-use external evaluation receipt entry is invalid")
		}
		if _, expected := wanted[name]; expected {
			seen[name] = struct{}{}
			continue
		}
		if _, lock := lockNames[name]; lock && info.Size() == 0 {
			continue
		}
		return errors.New("realtime computer-use external evaluation receipt directory has an extra artifact")
	}
	if len(seen) != len(wanted) {
		return errors.New("realtime computer-use external evaluation receipt directory is missing an artifact")
	}
	opened, openErr := root.Stat(".")
	visible, visibleErr := os.Lstat(path)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(expectedIdentity, opened) ||
		!os.SameFile(opened, visible) {
		return errors.New("realtime computer-use external evaluation receipt directory changed")
	}
	return nil
}

func exactReviewPublicationComplete(
	manifest ReviewManifest, receipts map[string]revieweval.EvaluationBundleReceipt,
) bool {
	if manifest.Expected != 16 || !manifest.Complete || !manifest.Reportable ||
		!manifest.CoreReportable || len(manifest.Attempts) != 16 || len(manifest.Missing) != 0 ||
		len(receipts) != 16 {
		return false
	}
	seen := make(map[string]struct{}, 16)
	for _, attempt := range manifest.Attempts {
		if attempt.ReviewStatus != "complete" || attempt.EvaluationBundle == nil ||
			!attempt.Reportable {
			return false
		}
		if _, present := receipts[attempt.Case]; !present {
			return false
		}
		seen[attempt.Case] = struct{}{}
	}
	return len(seen) == 16
}

func cloneReviewEvaluationReceiptMap(
	source map[string]revieweval.EvaluationBundleReceipt,
) map[string]revieweval.EvaluationBundleReceipt {
	result := make(map[string]revieweval.EvaluationBundleReceipt, len(source))
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[key] = source[key]
	}
	return result
}
