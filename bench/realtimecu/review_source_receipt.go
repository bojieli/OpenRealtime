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
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const maximumReviewSourceReceiptBytes = 64 << 10

// ReviewSourceReceiptAnchor acquires exclusive, crash-released ownership of
// one deterministic-source publication. The lease remains held from staged
// source verification through durable receipt commit and final marker
// promotion, so concurrent finishers cannot publish competing identities.
type ReviewSourceReceiptAnchor interface {
	Acquire(context.Context, string) (ReviewSourceReceiptLease, error)
}

// ReviewSourceReceiptLease is caller-owned durable receipt storage. Publish
// is create-only and a nil return guarantees durable commit. Load returns
// found=false only when no receipt has been committed. Close is idempotent.
type ReviewSourceReceiptLease interface {
	Publish(context.Context, ReviewSourceReceipt) error
	Load(context.Context) (ReviewSourceReceipt, bool, error)
	Close() error
}

// FileReviewSourceReceiptAnchor is the create-only filesystem implementation.
// Path must be absolute, clean, outside the review bundle, and have an existing
// identity-stable parent directory.
type FileReviewSourceReceiptAnchor struct {
	Path string
}

type fileReviewSourceReceiptLease struct {
	path   string
	lock   *os.File
	mu     sync.Mutex
	closed bool
}

type reviewSourcePublicationState struct {
	ReceiptDurable bool
	FinalVerified  bool
}

func (anchor FileReviewSourceReceiptAnchor) Publish(
	ctx context.Context, receipt ReviewSourceReceipt,
) error {
	return WriteReviewSourceReceipt(ctx, anchor.Path, receipt)
}

func (anchor FileReviewSourceReceiptAnchor) Verify(
	ctx context.Context, expected ReviewSourceReceipt,
) error {
	actual, err := ReadReviewSourceReceipt(ctx, anchor.Path)
	if err != nil {
		return err
	}
	if !samePortableReviewSourceReceipt(actual, expected) {
		return errors.New("realtime computer-use source anchor differs from its expected receipt")
	}
	return nil
}

func (anchor FileReviewSourceReceiptAnchor) Acquire(
	ctx context.Context, publicationID string,
) (ReviewSourceReceiptLease, error) {
	if ctx == nil {
		return nil, errors.New("acquire realtime computer-use source receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if err := validateReviewDigest(publicationID); err != nil {
		return nil, errors.New("realtime computer-use source publication identity is invalid")
	}
	parent, name, err := validateExternalReviewSourceReceiptPath(anchor.Path, "")
	if err != nil {
		return nil, err
	}
	lock, err := acquireReviewSourceReceiptLock(ctx, parent, name+".publication.lock")
	if err != nil {
		return nil, err
	}
	return &fileReviewSourceReceiptLease{path: anchor.Path, lock: lock}, nil
}

func (lease *fileReviewSourceReceiptLease) Publish(
	ctx context.Context, receipt ReviewSourceReceipt,
) error {
	if lease == nil {
		return errors.New("publish realtime computer-use source receipt: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.lock == nil {
		return errors.New("realtime computer-use source receipt lease is closed")
	}
	return WriteReviewSourceReceipt(ctx, lease.path, receipt)
}

func (lease *fileReviewSourceReceiptLease) Load(
	ctx context.Context,
) (ReviewSourceReceipt, bool, error) {
	if lease == nil {
		return ReviewSourceReceipt{}, false,
			errors.New("load realtime computer-use source receipt: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.lock == nil {
		return ReviewSourceReceipt{}, false,
			errors.New("realtime computer-use source receipt lease is closed")
	}
	if ctx == nil {
		return ReviewSourceReceipt{}, false,
			errors.New("load realtime computer-use source receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return ReviewSourceReceipt{}, false, cause
	}
	info, err := os.Lstat(lease.path)
	if os.IsNotExist(err) {
		if _, _, validationErr := validateExternalReviewSourceReceiptPath(lease.path, ""); validationErr != nil {
			return ReviewSourceReceipt{}, false, validationErr
		}
		return ReviewSourceReceipt{}, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ReviewSourceReceipt{}, false,
			errors.New("realtime computer-use source receipt path is not a regular file")
	}
	receipt, err := ReadReviewSourceReceipt(ctx, lease.path)
	if err != nil {
		return ReviewSourceReceipt{}, false, err
	}
	return receipt, true, nil
}

func (lease *fileReviewSourceReceiptLease) Close() error {
	if lease == nil {
		return errors.New("close realtime computer-use source receipt: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.closed = true
	if lease.lock == nil {
		return errors.New("close realtime computer-use source receipt: missing lock")
	}
	err := lease.lock.Close()
	lease.lock = nil
	if err != nil {
		return errors.New("close realtime computer-use source receipt lock")
	}
	return nil
}

func acquireReviewSourceReceiptLock(
	ctx context.Context, parent, name string,
) (result *os.File, resultErr error) {
	root, identity, err := openReviewReceiptParent(parent)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			if result != nil {
				_ = result.Close()
				result = nil
			}
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use source receipt lock parent"))
		}
	}()
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		marker, createErr := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return nil, errors.New("create realtime computer-use source receipt lock marker")
		}
		if syncErr := marker.Sync(); syncErr != nil {
			_ = marker.Close()
			return nil, errors.New("sync realtime computer-use source receipt lock marker")
		}
		if closeErr := marker.Close(); closeErr != nil {
			return nil, errors.New("close realtime computer-use source receipt lock marker")
		}
		if syncErr := syncReviewReceiptParent(root); syncErr != nil {
			return nil, syncErr
		}
		info, err = root.Lstat(name)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 || reviewFileHasMultipleLinks(info) {
		return nil, errors.New("realtime computer-use source receipt lock marker is invalid")
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("open realtime computer-use source receipt lock marker")
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || reviewFileHasMultipleLinks(opened) {
		_ = file.Close()
		return nil, errors.New("realtime computer-use source receipt lock marker changed while opening")
	}
	if err := lockReviewSourceFileExclusive(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("realtime computer-use source publication is already leased: %w", err)
	}
	if cause := context.Cause(ctx); cause != nil {
		_ = file.Close()
		return nil, cause
	}
	after, err := root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(opened, after) || reviewFileHasMultipleLinks(after) ||
		verifyReviewReceiptParentIdentity(parent, root, identity) != nil {
		_ = file.Close()
		return nil, errors.New("realtime computer-use source receipt lock marker changed after locking")
	}
	return file, nil
}

func commitStagedReviewSource(
	ctx context.Context, directory string, expected ReviewSourceReceipt,
	anchor ReviewSourceReceiptAnchor,
) (state reviewSourcePublicationState, resultErr error) {
	if ctx == nil {
		return state, errors.New("commit realtime computer-use source: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return state, cause
	}
	if err := validateReviewSourceReceipt(expected); err != nil {
		return state, err
	}
	root, identity, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return state, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use staged source root"))
		}
	}()
	stageExists, finalExists, err := reviewSourceMarkerState(root)
	if err != nil {
		return state, err
	}
	if stageExists && finalExists {
		return state, errors.New("realtime computer-use source has both staged and final markers")
	}
	var lease ReviewSourceReceiptLease
	if !nilInterface(anchor) {
		publicationID := reviewDigest([]byte(
			"openrealtime.realtime-cu-source-publication/v1\x00" + directory,
		))
		lease, err = anchor.Acquire(ctx, publicationID)
		if err != nil {
			return state, fmt.Errorf("acquire realtime computer-use source publication: %w", err)
		}
		if nilInterface(lease) {
			return state, errors.New("realtime computer-use source anchor returned a nil lease")
		}
		defer func() {
			if closeErr := lease.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr,
					errors.New("close realtime computer-use source publication"), closeErr)
			}
		}()
		stageExists, finalExists, err = reviewSourceMarkerState(root)
		if err != nil {
			return state, err
		}
		if stageExists && finalExists {
			return state, errors.New("realtime computer-use source has both staged and final markers")
		}
		retained, found, loadErr := lease.Load(ctx)
		if loadErr != nil {
			return state, fmt.Errorf("load realtime computer-use source receipt: %w", loadErr)
		}
		if found {
			if !samePortableReviewSourceReceipt(retained, expected) {
				return state, errors.New("durable realtime computer-use source receipt differs from staged source")
			}
			state.ReceiptDurable = true
		} else {
			if finalExists {
				return state, errors.New("final realtime computer-use source exists without a durable receipt")
			}
			if !stageExists {
				return state, errors.New("realtime computer-use source has no staged marker to publish")
			}
			if _, err := verifyReviewSourceReceiptAtMarker(
				directory, reviewSourceManifestStage, expected,
			); err != nil {
				return state, fmt.Errorf("verify staged realtime computer-use source: %w", err)
			}
			if err := lease.Publish(ctx, expected); err != nil {
				return state, fmt.Errorf("durably publish realtime computer-use source receipt: %w", err)
			}
			retained, found, err = lease.Load(ctx)
			if err != nil || !found || !samePortableReviewSourceReceipt(retained, expected) {
				return state, errors.New("reopen durable realtime computer-use source receipt")
			}
			state.ReceiptDurable = true
		}
	}
	if finalExists {
		if _, err := VerifyReviewSourceReceipt(directory, expected); err != nil {
			return state, fmt.Errorf("verify final realtime computer-use source: %w", err)
		}
		state.FinalVerified = true
		return state, nil
	}
	if !stageExists {
		return state, errors.New("realtime computer-use source marker is unavailable")
	}
	if _, err := verifyReviewSourceReceiptAtMarker(
		directory, reviewSourceManifestStage, expected,
	); err != nil {
		return state, fmt.Errorf("reverify staged realtime computer-use source before promotion: %w", err)
	}
	if !nilInterface(anchor) && !state.ReceiptDurable {
		return state, errors.New("realtime computer-use source receipt is not durable before promotion")
	}
	if err := promoteReviewSourceMarker(directory, root, identity); err != nil {
		return state, err
	}
	if _, err := VerifyReviewSourceReceipt(directory, expected); err != nil {
		return state, fmt.Errorf("verify promoted realtime computer-use source: %w", err)
	}
	state.FinalVerified = true
	return state, nil
}

func reviewSourceMarkerState(root *os.Root) (stage, final bool, resultErr error) {
	if root == nil {
		return false, false, errors.New("realtime computer-use source root is unavailable")
	}
	inspect := func(name string) (bool, error) {
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			reviewFileHasMultipleLinks(info) {
			return false, errors.New("realtime computer-use source marker is invalid")
		}
		return true, nil
	}
	stage, err := inspect(reviewSourceManifestStage)
	if err != nil {
		return false, false, err
	}
	final, err = inspect(reviewSourceManifest)
	return stage, final, err
}

func promoteReviewSourceMarker(
	directory string, root *os.Root, expectedRoot os.FileInfo,
) error {
	if _, err := verifyReviewRootIdentity(directory, root, expectedRoot); err != nil {
		return err
	}
	stageInfo, err := root.Lstat(reviewSourceManifestStage)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.Mode().IsRegular() ||
		reviewFileHasMultipleLinks(stageInfo) {
		return errors.New("staged realtime computer-use source marker is invalid")
	}
	if _, err := root.Lstat(reviewSourceManifest); err == nil {
		return errors.New("final realtime computer-use source marker already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("inspect final realtime computer-use source marker")
	}
	if err := renameReviewSourceNoReplace(
		directory, reviewSourceManifestStage, reviewSourceManifest, expectedRoot,
	); err != nil {
		return fmt.Errorf("promote realtime computer-use source marker exclusively: %w", err)
	}
	if err := syncReviewRoot(root); err != nil {
		return errors.New("sync promoted realtime computer-use source marker")
	}
	finalInfo, finalErr := root.Lstat(reviewSourceManifest)
	_, stageErr := root.Lstat(reviewSourceManifestStage)
	if finalErr != nil || finalInfo.Mode()&os.ModeSymlink != 0 ||
		!finalInfo.Mode().IsRegular() || !os.SameFile(stageInfo, finalInfo) ||
		!os.IsNotExist(stageErr) || reviewFileHasMultipleLinks(finalInfo) {
		return errors.New("realtime computer-use source marker changed during promotion")
	}
	_, err = verifyReviewRootIdentity(directory, root, expectedRoot)
	return err
}

func buildReviewSourceReceipt(
	directory string, manifest ReviewManifest, manifestSHA256 string,
) (ReviewSourceReceipt, error) {
	if _, err := validateNewReviewDirectory(directory); err != nil {
		return ReviewSourceReceipt{}, err
	}
	if err := validateReviewDigest(manifestSHA256); err != nil ||
		manifest.Result.Path != "result.json" ||
		validateReviewDigest(manifest.Result.SHA256) != nil {
		return ReviewSourceReceipt{}, errors.New("build realtime computer-use source receipt from invalid manifest")
	}
	receipt := ReviewSourceReceipt{
		Format: ReviewSourceReceiptFormat, FormatVersion: ReviewSourceReceiptVersion,
		Directory: directory, ManifestSHA256: manifestSHA256,
		ResultSHA256: manifest.Result.SHA256,
	}
	digest, err := reviewSourceReceiptDigest(receipt)
	if err != nil {
		return ReviewSourceReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	return receipt, nil
}

func reviewSourceReceiptDigest(receipt ReviewSourceReceipt) (string, error) {
	payload, err := json.Marshal(struct {
		Format         string `json:"format"`
		FormatVersion  int    `json:"format_version"`
		ManifestSHA256 string `json:"manifest_sha256"`
		ResultSHA256   string `json:"result_sha256"`
	}{receipt.Format, receipt.FormatVersion, receipt.ManifestSHA256, receipt.ResultSHA256})
	if err != nil {
		return "", errors.New("encode realtime computer-use source receipt identity")
	}
	return reviewDigest(payload), nil
}

func validateReviewSourceReceipt(receipt ReviewSourceReceipt) error {
	if receipt.Format != ReviewSourceReceiptFormat ||
		receipt.FormatVersion != ReviewSourceReceiptVersion {
		return errors.New("realtime computer-use source receipt format is invalid")
	}
	if _, err := validateNewReviewDirectory(receipt.Directory); err != nil {
		return errors.New("realtime computer-use source receipt directory is invalid")
	}
	for _, digest := range []string{
		receipt.ManifestSHA256, receipt.ResultSHA256, receipt.ReceiptSHA256,
	} {
		if err := validateReviewDigest(digest); err != nil {
			return errors.New("realtime computer-use source receipt digest is invalid")
		}
	}
	want, err := reviewSourceReceiptDigest(receipt)
	if err != nil || want != receipt.ReceiptSHA256 {
		return errors.New("realtime computer-use source receipt identity differs from its fields")
	}
	return nil
}

func samePortableReviewSourceReceipt(left, right ReviewSourceReceipt) bool {
	return left.Format == right.Format && left.FormatVersion == right.FormatVersion &&
		left.ManifestSHA256 == right.ManifestSHA256 &&
		left.ResultSHA256 == right.ResultSHA256 &&
		left.ReceiptSHA256 == right.ReceiptSHA256
}

// WriteReviewSourceReceipt durably publishes a canonical receipt outside its
// source bundle. The destination is create-only; a failed write removes only
// the file this call created.
func WriteReviewSourceReceipt(
	ctx context.Context, path string, receipt ReviewSourceReceipt,
) (resultErr error) {
	if ctx == nil {
		return errors.New("write realtime computer-use source receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err := validateReviewSourceReceipt(receipt); err != nil {
		return err
	}
	parent, name, err := validateExternalReviewSourceReceiptPath(path, receipt.Directory)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) >= maximumReviewSourceReceiptBytes {
		return errors.New("encode realtime computer-use source receipt")
	}
	payload = append(payload, '\n')
	root, identity, err := openReviewReceiptParent(parent)
	if err != nil {
		return err
	}
	owned, committed := false, false
	defer func() {
		if owned && !committed {
			_ = root.Remove(name)
		}
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use source receipt parent"))
		}
	}()
	if _, err := root.Lstat(name); err == nil {
		return errors.New("realtime computer-use source receipt already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("inspect realtime computer-use source receipt destination")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return errors.New("create realtime computer-use source receipt exclusively")
	}
	owned = true
	written := 0
	for written < len(payload) {
		count, writeErr := file.Write(payload[written:])
		if writeErr != nil || count <= 0 {
			_ = file.Close()
			return errors.New("write realtime computer-use source receipt")
		}
		written += count
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync realtime computer-use source receipt")
	}
	if err := file.Close(); err != nil {
		_ = file.Close()
		return errors.New("close realtime computer-use source receipt")
	}
	if err := syncReviewReceiptParent(root); err != nil {
		return err
	}
	retained, info, err := readReviewFile(root, name, maximumReviewSourceReceiptBytes)
	if err != nil || !bytes.Equal(retained, payload) || info.Mode().Perm()&0o222 != 0 ||
		reviewFileHasMultipleLinks(info) {
		return errors.New("reopen realtime computer-use source receipt")
	}
	if err := verifyReviewReceiptParentIdentity(parent, root, identity); err != nil {
		return err
	}
	committed = true
	return nil
}

// ReadReviewSourceReceipt reopens a canonical external receipt. It does not
// authenticate source bytes by itself; use VerifyReviewSourceReceipt for that.
func ReadReviewSourceReceipt(ctx context.Context, path string) (
	receipt ReviewSourceReceipt, resultErr error,
) {
	if ctx == nil {
		return ReviewSourceReceipt{}, errors.New("read realtime computer-use source receipt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return ReviewSourceReceipt{}, cause
	}
	parent, name, err := validateExternalReviewSourceReceiptPath(path, "")
	if err != nil {
		return ReviewSourceReceipt{}, err
	}
	root, identity, err := openReviewReceiptParent(parent)
	if err != nil {
		return ReviewSourceReceipt{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			receipt = ReviewSourceReceipt{}
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use source receipt parent"))
		}
	}()
	payload, info, err := readReviewFile(root, name, maximumReviewSourceReceiptBytes)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 ||
		reviewFileHasMultipleLinks(info) || strictjson.Validate(payload) != nil {
		return ReviewSourceReceipt{}, errors.New("read realtime computer-use source receipt")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ReviewSourceReceipt{}, errors.New("decode realtime computer-use source receipt")
	}
	canonical, err := json.MarshalIndent(receipt, "", "  ")
	canonical = append(canonical, '\n')
	if err != nil || !bytes.Equal(canonical, payload) || validateReviewSourceReceipt(receipt) != nil {
		return ReviewSourceReceipt{}, errors.New("realtime computer-use source receipt is noncanonical or invalid")
	}
	if err := verifyReviewReceiptParentIdentity(parent, root, identity); err != nil {
		return ReviewSourceReceipt{}, err
	}
	return receipt, nil
}

// VerifyReviewSourceReceipt cross-binds the external receipt to the current
// independently verified deterministic source bytes.
func VerifyReviewSourceReceipt(
	directory string, expected ReviewSourceReceipt,
) (ReviewManifest, error) {
	return verifyReviewSourceReceiptAtMarker(directory, reviewSourceManifest, expected)
}

func verifyReviewSourceReceiptAtMarker(
	directory, marker string, expected ReviewSourceReceipt,
) (ReviewManifest, error) {
	if err := validateReviewSourceReceipt(expected); err != nil {
		return ReviewManifest{}, err
	}
	manifest, err := verifyReviewSourceBundleAtMarkerWithOperations(
		directory, marker, expected.ManifestSHA256, reviewBundleVerifyOperations{},
	)
	if err != nil {
		return ReviewManifest{}, err
	}
	actual, err := buildReviewSourceReceipt(directory, manifest, expected.ManifestSHA256)
	if err != nil || !samePortableReviewSourceReceipt(actual, expected) {
		return ReviewManifest{}, errors.New("realtime computer-use source differs from its external receipt")
	}
	return manifest, nil
}

func validateExternalReviewSourceReceiptPath(path, sourceDirectory string) (string, string, error) {
	if _, err := validateNewReviewDirectory(path); err != nil || path == filepath.Dir(path) {
		return "", "", errors.New("realtime computer-use source receipt path must be clean and absolute")
	}
	if sourceDirectory != "" {
		if err := verifyReviewNoSymlinkAncestors(sourceDirectory); err != nil {
			return "", "", errors.New("realtime computer-use source receipt bundle path is invalid")
		}
		relative, err := filepath.Rel(sourceDirectory, path)
		if err != nil || relative == "." ||
			(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return "", "", errors.New("realtime computer-use source receipt must be outside its bundle")
		}
	}
	return filepath.Dir(path), filepath.Base(path), nil
}

func openReviewReceiptParent(path string) (*os.Root, os.FileInfo, error) {
	if err := verifyReviewNoSymlinkAncestors(path); err != nil {
		return nil, nil, errors.New("realtime computer-use source receipt parent has a symlinked or invalid ancestor")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("realtime computer-use source receipt parent is invalid")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, errors.New("open realtime computer-use source receipt parent")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, rootInfo) {
		_ = root.Close()
		return nil, nil, errors.New("realtime computer-use source receipt parent identity changed")
	}
	return root, rootInfo, nil
}

func verifyReviewReceiptParentIdentity(path string, root *os.Root, expected os.FileInfo) error {
	if err := verifyReviewNoSymlinkAncestors(path); err != nil {
		return errors.New("realtime computer-use source receipt parent ancestor changed")
	}
	current, pathErr := os.Lstat(path)
	anchored, rootErr := root.Stat(".")
	if pathErr != nil || rootErr != nil || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(current, anchored) || !os.SameFile(anchored, expected) {
		return errors.New("realtime computer-use source receipt parent identity changed")
	}
	return nil
}

func syncReviewReceiptParent(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open realtime computer-use source receipt parent for sync")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("sync realtime computer-use source receipt parent")
	}
	return nil
}
