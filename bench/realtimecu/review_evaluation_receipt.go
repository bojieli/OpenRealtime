package realtimecu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	revieweval "github.com/bojieli/OpenRealtime/bench/review"
)

// ReviewEvaluationReceiptStoreFactory supplies caller-owned durable receipt
// storage for each case's generic evaluation publication. The generic review
// package writes and reopens this receipt before the evaluation directory is
// atomically promoted to its final, reportable name.
type ReviewEvaluationReceiptStoreFactory interface {
	Store(caseID, finalDirectory string) (revieweval.EvaluationBundleReceiptStore, error)
	Quarantine(caseID, finalDirectory string) (string, error)
}

// FileReviewEvaluationReceiptStoreFactory stores one create-only receipt per
// canonical Realtime-CU case in an external existing directory.
type FileReviewEvaluationReceiptStoreFactory struct {
	Directory      string
	QuarantineRoot string
}

func (factory FileReviewEvaluationReceiptStoreFactory) Quarantine(
	caseID, finalDirectory string,
) (string, error) {
	if _, known := caseOrdinal(caseID); !known {
		return "", errors.New("realtime computer-use evaluation quarantine names an unknown case")
	}
	if _, err := factory.path(caseID, finalDirectory); err != nil {
		return "", err
	}
	directory, err := validateNewReviewDirectory(factory.QuarantineRoot)
	if err != nil {
		return "", errors.New("realtime computer-use evaluation quarantine directory is invalid")
	}
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return "", errors.New("realtime computer-use evaluation quarantine has a symlinked or invalid ancestor")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("realtime computer-use evaluation quarantine directory is unavailable")
	}
	bundleDirectory := filepath.Dir(filepath.Dir(finalDirectory))
	relative, err := filepath.Rel(bundleDirectory, directory)
	if err != nil || relative == "." ||
		(relative != ".." && !startsWithParentTraversal(relative)) {
		return "", errors.New("realtime computer-use evaluation quarantine must be outside the review bundle")
	}
	return directory, nil
}

func (factory FileReviewEvaluationReceiptStoreFactory) Store(
	caseID, finalDirectory string,
) (revieweval.EvaluationBundleReceiptStore, error) {
	path, err := factory.path(caseID, finalDirectory)
	if err != nil {
		return nil, err
	}
	return revieweval.FileEvaluationBundleReceiptStore{Path: path}, nil
}

func (factory FileReviewEvaluationReceiptStoreFactory) path(
	caseID, finalDirectory string,
) (string, error) {
	ordinal, known := caseOrdinal(caseID)
	if !known {
		return "", errors.New("realtime computer-use evaluation anchor names an unknown case")
	}
	directory, err := validateNewReviewDirectory(factory.Directory)
	if err != nil {
		return "", errors.New("realtime computer-use evaluation anchor directory is invalid")
	}
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return "", errors.New("realtime computer-use evaluation anchor has a symlinked or invalid ancestor")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("realtime computer-use evaluation anchor directory is unavailable")
	}
	if finalDirectory == "" || !filepath.IsAbs(finalDirectory) ||
		filepath.Clean(finalDirectory) != finalDirectory {
		return "", errors.New("realtime computer-use evaluation final directory is invalid")
	}
	if err := verifyReviewNoSymlinkAncestors(filepath.Dir(finalDirectory)); err != nil {
		return "", errors.New("realtime computer-use evaluation final directory has a symlinked or invalid ancestor")
	}
	// Evaluation bundles are always bundle/reviews/<case>. The external anchor
	// must be outside bundle so it cannot perturb the create-only final tree.
	bundleDirectory := filepath.Dir(filepath.Dir(finalDirectory))
	relative, err := filepath.Rel(bundleDirectory, directory)
	if err != nil || relative == "." ||
		(relative != ".." && !startsWithParentTraversal(relative)) {
		return "", errors.New("realtime computer-use evaluation anchors must be outside the review bundle")
	}
	name := fmt.Sprintf("%02d-%s-trial-01.receipt.json", ordinal, reviewSlug(caseID))
	return filepath.Join(directory, name), nil
}

func startsWithParentTraversal(path string) bool {
	return len(path) > 3 && path[:3] == ".."+string(filepath.Separator)
}
