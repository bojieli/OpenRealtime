package sourcebundle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/bench/review"
)

// ReviewSource is a verified, credential-free cursor over reviewable attempts
// in one sealed candidate source bundle. It keeps only metadata in memory and
// emits one owned request at a time, allowing an evaluator to use bounded
// worker concurrency even for very large benchmark populations.
type ReviewSource struct {
	mu        sync.Mutex
	directory string
	receipt   string
	root      *os.Root
	identity  os.FileInfo
	manifest  Manifest
	record    Receipt
	files     map[string]SourceFile
	next      int
	closed    bool
}

// OpenReviewSource verifies the complete source receipt before exposing any
// provider-ready input. Advisory provider credentials are deliberately not an
// option here and never become source-bundle state.
func OpenReviewSource(
	ctx context.Context, directory, receiptPath string,
) (*ReviewSource, error) {
	manifest, receipt, err := Verify(ctx, directory, receiptPath)
	if err != nil {
		return nil, err
	}
	visible, err := os.Lstat(receipt.Directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return nil, errors.New("open candidate review source: source directory is invalid")
	}
	root, err := os.OpenRoot(receipt.Directory)
	if err != nil {
		return nil, errors.New("open candidate review source directory")
	}
	opened, openErr := root.Stat(".")
	afterOpen, visibleErr := os.Lstat(receipt.Directory)
	if openErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!afterOpen.IsDir() || !os.SameFile(visible, opened) || !os.SameFile(opened, afterOpen) {
		_ = root.Close()
		return nil, errors.New("candidate review source changed while opening")
	}
	files := make(map[string]SourceFile, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.Path] = file
	}
	return &ReviewSource{
		directory: receipt.Directory, receipt: receiptPath, root: root,
		identity: opened, manifest: manifest, record: receipt, files: files,
	}, nil
}

// Manifest returns an owned metadata snapshot. The authoritative deterministic
// outcome remains result.json in the receipt-bound source tree.
func (source *ReviewSource) Manifest() (Manifest, error) {
	if source == nil {
		return Manifest{}, errors.New("candidate review source is nil")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed || source.root == nil {
		return Manifest{}, errors.New("candidate review source is closed")
	}
	return cloneManifest(source.manifest), nil
}

// Receipt returns the exact portable receipt verified when the cursor opened.
func (source *ReviewSource) Receipt() (Receipt, error) {
	if source == nil {
		return Receipt{}, errors.New("candidate review source is nil")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed || source.root == nil {
		return Receipt{}, errors.New("candidate review source is closed")
	}
	return source.record, nil
}

// Next returns the next reconstructable review request. Attempts without a
// complete context/media pair remain in the source manifest and REVIEW.md but
// are skipped because no multimodal provider can truthfully assess them.
func (source *ReviewSource) Next(
	ctx context.Context, sensitiveValues []string,
) (request review.Request, found bool, resultErr error) {
	if source == nil || ctx == nil {
		return review.Request{}, false, errors.New("read candidate review source: nil source or context")
	}
	if err := ctx.Err(); err != nil {
		return review.Request{}, false, err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed || source.root == nil {
		return review.Request{}, false, errors.New("candidate review source is closed")
	}
	if err := verifyBundleRoot(source.root, source.directory, source.identity); err != nil {
		return review.Request{}, false, err
	}
	for source.next < len(source.manifest.Attempts) {
		entry := source.manifest.Attempts[source.next]
		source.next++
		if entry.Media == nil || entry.ContextPath == "" || entry.ContextSHA256 == "" ||
			entry.Terminal != "completed" {
			continue
		}
		contextPath := filepath.ToSlash(filepath.Join(entry.Directory, entry.ContextPath))
		contextPayload, err := readManifestFile(source.root, source.files, contextPath, "")
		if err != nil {
			return review.Request{}, false, err
		}
		contextSHA, err := review.CanonicalContextSHA256(ctx, contextPayload)
		if err != nil || contextSHA != entry.ContextSHA256 {
			return review.Request{}, false, errors.New("candidate review source context changed")
		}
		media := *entry.Media
		media.Validation = ""
		media.SizeBytes = 0
		request = review.Request{
			AttemptID: entry.AttemptID, Suite: entry.Suite, Case: entry.Case, Trial: entry.Trial,
			RootDirectory: filepath.Join(source.directory, filepath.FromSlash(entry.Directory)),
			Context:       slices.Clone(contextPayload), Media: []review.Media{media},
			SensitiveValues: slices.Clone(sensitiveValues),
		}
		return request, true, nil
	}
	return review.Request{}, false, nil
}

// Close releases the cursor and re-verifies the source receipt after the last
// consumer read. A mutation during a long advisory campaign therefore fails
// the campaign even though each individual Evaluate call also snapshots and
// validates the exact bytes it sent.
func (source *ReviewSource) Close(ctx context.Context) error {
	if source == nil || ctx == nil {
		return errors.New("close candidate review source: nil source or context")
	}
	source.mu.Lock()
	if source.closed {
		source.mu.Unlock()
		return nil
	}
	source.closed = true
	root := source.root
	source.root = nil
	directory, receipt := source.directory, source.receipt
	wantManifest, wantReceipt := cloneManifest(source.manifest), source.record
	source.mu.Unlock()
	var resultErr error
	if root == nil || root.Close() != nil {
		resultErr = errors.New("close candidate review source directory")
	}
	gotManifest, gotReceipt, err := Verify(ctx, directory, receipt)
	if err != nil {
		return errors.Join(resultErr, err)
	}
	wantManifestPayload, wantManifestErr := canonicalCompact(wantManifest)
	gotManifestPayload, gotManifestErr := canonicalCompact(gotManifest)
	wantReceiptPayload, wantReceiptErr := canonicalCompact(wantReceipt)
	gotReceiptPayload, gotReceiptErr := canonicalCompact(gotReceipt)
	if wantManifestErr != nil || gotManifestErr != nil || wantReceiptErr != nil || gotReceiptErr != nil ||
		!slices.Equal(wantManifestPayload, gotManifestPayload) ||
		!slices.Equal(wantReceiptPayload, gotReceiptPayload) {
		resultErr = errors.Join(resultErr, errors.New("candidate review source identity changed"))
	}
	return resultErr
}
