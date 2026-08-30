package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/bench"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	ReviewSourceFormat         = "openrealtime.meeting-review-source"
	ReviewSourceFormatVersion  = 1
	ReviewSourceReceiptFormat  = "openrealtime.meeting-review-source-receipt"
	ReviewSourceReceiptVersion = 1
	reviewSourceDirectory      = "source"
	reviewSourceManifestName   = "manifest.json"
	reviewSourceManifestStage  = ".openrealtime-source-manifest.stage"
	maximumReviewSourceFiles   = 4096
	maximumReviewSourceBytes   = int64(1 << 30)
)

// ReviewSourceAttempt is the provider-independent evidence for one exact
// deterministic row. All paths are relative to the sealed source directory.
// Advisory model output deliberately cannot appear in this structure.
type ReviewSourceAttempt struct {
	Ordinal              int                        `json:"ordinal"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	Deterministic        bench.TaskOutcome          `json:"deterministic"`
	Cell                 bench.Cell                 `json:"cell"`
	Provenance           bench.Provenance           `json:"provenance"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
	Context              ReviewArtifact             `json:"context"`
	MediaManifest        ReviewArtifact             `json:"media_manifest"`
	Media                []ReviewMedia              `json:"media"`
	MediaArtifacts       []ReviewArtifact           `json:"media_artifacts"`
	VideoStatus          string                     `json:"video_status"`
	RunOrigin            EvidenceRunOrigin          `json:"run_origin"`
	ExecutionStatus      string                     `json:"execution_status"`
	Reportable           bool                       `json:"reportable"`
	ReportabilityNote    string                     `json:"reportability_note,omitempty"`
}

// ReviewSourceManifest is the commit marker for deterministic result,
// transcript context, and independently verified retained media. Complete is
// a population property; an incomplete diagnostic population is still sealed
// and receives an external receipt, but can never be reportable.
type ReviewSourceManifest struct {
	Format              string                `json:"format"`
	FormatVersion       int                   `json:"format_version"`
	Suite               string                `json:"suite"`
	Expected            int                   `json:"expected"`
	Complete            bool                  `json:"complete"`
	Reportable          bool                  `json:"reportable"`
	CoreReportable      bool                  `json:"core_reportable"`
	CoreReportability   string                `json:"core_reportability,omitempty"`
	Result              ReviewArtifact        `json:"result"`
	Cell                bench.Cell            `json:"cell"`
	Provenance          bench.Provenance      `json:"provenance"`
	ReportabilityErrors []string              `json:"reportability_errors,omitempty"`
	Missing             []ReviewMissing       `json:"missing,omitempty"`
	Attempts            []ReviewSourceAttempt `json:"attempts"`
	FileSetSHA256       string                `json:"file_set_sha256"`
	Files               []ReviewArtifact      `json:"files"`
}

// ReviewSourceReceipt is portable: ReceiptSHA256 excludes Directory. The
// caller must retain this value outside the source tree and supply it when
// reopening the source.
type ReviewSourceReceipt struct {
	Format         string `json:"format"`
	FormatVersion  int    `json:"format_version"`
	Directory      string `json:"directory"`
	ManifestSHA256 string `json:"manifest_sha256"`
	FileSetSHA256  string `json:"file_set_sha256"`
	ResultSHA256   string `json:"result_sha256"`
	ReceiptSHA256  string `json:"receipt_sha256"`
}

type ReviewSourceBundle struct {
	Manifest ReviewSourceManifest
	Result   bench.Result
	Receipt  ReviewSourceReceipt
}

type meetingSourceVerifyHookKey struct{}

// ReviewSourceReceiptAnchor holds crash-released exclusive ownership from
// staged deterministic-source verification through external receipt commit
// and final marker promotion.
type ReviewSourceReceiptAnchor interface {
	Acquire(context.Context, string) (ReviewSourceReceiptLease, error)
}

type ReviewSourceReceiptLease interface {
	Publish(context.Context, ReviewSourceReceipt) error
	Load(context.Context) (ReviewSourceReceipt, bool, error)
	Close() error
}

// FileReviewSourceReceiptAnchor is the production filesystem anchor. Path is
// the caller-selected source receipt path outside the review bundle.
type FileReviewSourceReceiptAnchor struct{ Path string }

type fileReviewSourceReceiptLease struct {
	path   string
	lock   *os.File
	closed bool
	mu     sync.Mutex
}

func meetingSourceVerifyHook(ctx context.Context, stage string) {
	if hook, ok := ctx.Value(meetingSourceVerifyHookKey{}).(func(string)); ok && hook != nil {
		hook(stage)
	}
}

func (anchor FileReviewSourceReceiptAnchor) Acquire(
	ctx context.Context, publicationID string,
) (ReviewSourceReceiptLease, error) {
	if ctx == nil {
		return nil, errors.New("acquire meeting source receipt: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !validReviewDigest(publicationID) || !filepath.IsAbs(anchor.Path) ||
		filepath.Clean(anchor.Path) != anchor.Path || anchor.Path == filepath.Dir(anchor.Path) {
		return nil, errors.New("meeting source receipt publication identity or path is invalid")
	}
	parentPath, name := filepath.Dir(anchor.Path), filepath.Base(anchor.Path)
	root, rootInfo, err := openMeetingSourceRoot(parentPath)
	if err != nil {
		return nil, errors.New("open meeting source receipt lock parent")
	}
	defer root.Close()
	lockName := name + ".publication.lock"
	info, err := root.Lstat(lockName)
	if os.IsNotExist(err) {
		marker, createErr := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return nil, errors.New("create meeting source receipt lock marker")
		}
		if syncErr := marker.Sync(); syncErr != nil {
			_ = marker.Close()
			return nil, errors.New("sync meeting source receipt lock marker")
		}
		if closeErr := marker.Close(); closeErr != nil {
			return nil, errors.New("close meeting source receipt lock marker")
		}
		if syncErr := syncMeetingSourceRoot(root); syncErr != nil {
			return nil, syncErr
		}
		info, err = root.Lstat(lockName)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("meeting source receipt lock marker is invalid")
	}
	lock, err := root.OpenFile(lockName, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("open meeting source receipt lock marker")
	}
	opened, err := lock.Stat()
	if err != nil || !os.SameFile(info, opened) || fileidentity.RequireSingleLink(lock) != nil {
		_ = lock.Close()
		return nil, errors.New("meeting source receipt lock marker changed while opening")
	}
	if err := lockMeetingSourceFileExclusive(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("meeting source publication is already leased: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		_ = lock.Close()
		return nil, err
	}
	after, err := root.Lstat(lockName)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(opened, after) || fileidentity.RequireSingleLink(lock) != nil ||
		verifyMeetingSourceRootIdentity(parentPath, root, rootInfo) != nil {
		_ = lock.Close()
		return nil, errors.New("meeting source receipt lock marker changed after locking")
	}
	return &fileReviewSourceReceiptLease{path: anchor.Path, lock: lock}, nil
}

func (lease *fileReviewSourceReceiptLease) Publish(
	ctx context.Context, receipt ReviewSourceReceipt,
) error {
	if lease == nil {
		return errors.New("publish meeting source receipt: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.lock == nil {
		return errors.New("meeting source receipt lease is closed")
	}
	return WriteReviewSourceReceipt(ctx, lease.path, receipt)
}

func (lease *fileReviewSourceReceiptLease) Load(
	ctx context.Context,
) (ReviewSourceReceipt, bool, error) {
	if lease == nil {
		return ReviewSourceReceipt{}, false, errors.New("load meeting source receipt: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.lock == nil {
		return ReviewSourceReceipt{}, false, errors.New("meeting source receipt lease is closed")
	}
	if ctx == nil {
		return ReviewSourceReceipt{}, false, errors.New("load meeting source receipt: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ReviewSourceReceipt{}, false, err
	}
	if _, err := os.Lstat(lease.path); os.IsNotExist(err) {
		return ReviewSourceReceipt{}, false, nil
	} else if err != nil {
		return ReviewSourceReceipt{}, false, errors.New("inspect meeting source receipt")
	}
	receipt, err := ReadReviewSourceReceipt(ctx, lease.path)
	if err != nil {
		return ReviewSourceReceipt{}, false, err
	}
	return receipt, true, nil
}

func (lease *fileReviewSourceReceiptLease) Close() error {
	if lease == nil {
		return errors.New("close meeting source receipt: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.closed = true
	if lease.lock == nil {
		return errors.New("close meeting source receipt: missing lock")
	}
	err := lease.lock.Close()
	lease.lock = nil
	if err != nil {
		return errors.New("close meeting source receipt lock")
	}
	return nil
}

// WriteReviewSourceReceipt durably retains a portable source receipt outside
// the sealed source directory. The destination is create-only and opened
// through an identity-checked parent handle.
func WriteReviewSourceReceipt(
	ctx context.Context, path string, receipt ReviewSourceReceipt,
) (resultErr error) {
	if ctx == nil {
		return errors.New("write meeting review source receipt: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := validateMeetingSourceReceipt(receipt); err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == filepath.Dir(path) ||
		path == receipt.Directory || strings.HasPrefix(path, receipt.Directory+string(filepath.Separator)) {
		return errors.New("meeting review source receipt path must be clean, absolute, and external")
	}
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) > maximumReviewContextBytes {
		return errors.New("encode meeting review source receipt")
	}
	payload = append(payload, '\n')
	parent, name := filepath.Dir(path), filepath.Base(path)
	root, rootInfo, err := openMeetingSourceRoot(parent)
	if err != nil {
		return errors.New("open meeting review source receipt parent")
	}
	written := false
	defer func() {
		if !written {
			_ = root.Remove(name)
		}
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close meeting review source receipt parent"))
		}
	}()
	if _, err := root.Lstat(name); err == nil {
		return errors.New("meeting review source receipt destination already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("inspect meeting review source receipt destination")
	}
	if err := writeMeetingReviewFile(root, name, payload); err != nil {
		return errors.New("create meeting review source receipt exclusively")
	}
	retained, err := readMeetingSourceFile(ctx, root, ReviewArtifact{
		Path: name, SHA256: reviewDigest(payload), SizeBytes: int64(len(payload)),
		MediaType: "application/json",
	}, false)
	if err != nil || !bytes.Equal(retained, payload) {
		return errors.New("reopen meeting review source receipt")
	}
	if err := syncMeetingSourceRoot(root); err != nil {
		return err
	}
	if err := verifyMeetingSourceRootIdentity(parent, root, rootInfo); err != nil {
		return err
	}
	written = true
	return nil
}

func ReadReviewSourceReceipt(ctx context.Context, path string) (ReviewSourceReceipt, error) {
	if ctx == nil {
		return ReviewSourceReceipt{}, errors.New("read meeting review source receipt: nil context")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == filepath.Dir(path) {
		return ReviewSourceReceipt{}, errors.New("meeting review source receipt path is invalid")
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	root, rootInfo, err := openMeetingSourceRoot(parent)
	if err != nil {
		return ReviewSourceReceipt{}, err
	}
	defer root.Close()
	info, err := meetingSourceFileInfo(root, name, false)
	if err != nil || info.Mode().Perm()&0o077 != 0 || info.Size() > maximumReviewContextBytes {
		return ReviewSourceReceipt{}, errors.New("meeting review source receipt is invalid")
	}
	payload, err := readMeetingSourceFile(ctx, root, ReviewArtifact{
		Path: name, SizeBytes: info.Size(), MediaType: "application/json",
	}, false)
	if err != nil || strictjson.Validate(payload) != nil {
		return ReviewSourceReceipt{}, errors.New("read meeting review source receipt")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var receipt ReviewSourceReceipt
	if err := decoder.Decode(&receipt); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ReviewSourceReceipt{}, errors.New("decode meeting review source receipt")
	}
	canonical, err := json.MarshalIndent(receipt, "", "  ")
	canonical = append(canonical, '\n')
	if err != nil || !bytes.Equal(canonical, payload) || validateMeetingSourceReceipt(receipt) != nil {
		return ReviewSourceReceipt{}, errors.New("meeting review source receipt is noncanonical or invalid")
	}
	if err := verifyMeetingSourceRootIdentity(parent, root, rootInfo); err != nil {
		return ReviewSourceReceipt{}, err
	}
	return receipt, nil
}

func reviewSourceAttempt(source ReviewAttempt) (ReviewSourceAttempt, error) {
	stripArtifact := func(value ReviewArtifact) (ReviewArtifact, error) {
		path, err := stripMeetingSourcePrefix(value.Path)
		if err != nil {
			return ReviewArtifact{}, err
		}
		value.Path = path
		return value, nil
	}
	contextArtifact, err := stripArtifact(source.Context)
	if err != nil {
		return ReviewSourceAttempt{}, err
	}
	mediaManifest, err := stripArtifact(source.MediaManifest)
	if err != nil {
		return ReviewSourceAttempt{}, err
	}
	media := cloneReviewMedia(source.Media)
	for index := range media {
		media[index].Path, err = stripMeetingSourcePrefix(media[index].Path)
		if err != nil {
			return ReviewSourceAttempt{}, err
		}
	}
	mediaArtifacts := make([]ReviewArtifact, len(source.MediaArtifacts))
	for index, artifact := range source.MediaArtifacts {
		mediaArtifacts[index], err = stripArtifact(artifact)
		if err != nil {
			return ReviewSourceAttempt{}, err
		}
	}
	return ReviewSourceAttempt{
		Ordinal: source.Ordinal, Case: source.Case, Trial: source.Trial,
		Deterministic: cloneTaskOutcome(source.Deterministic),
		Cell:          cloneMeetingCell(source.Cell), Provenance: source.Provenance,
		ExecutionRequirement: cloneMeetingExecutionRequirement(source.ExecutionRequirement),
		Context:              contextArtifact, MediaManifest: mediaManifest, Media: media,
		MediaArtifacts: mediaArtifacts, VideoStatus: source.VideoStatus,
		RunOrigin: source.RunOrigin, ExecutionStatus: source.ExecutionStatus,
		Reportable: source.Reportable, ReportabilityNote: source.ReportabilityNote,
	}, nil
}

func stripMeetingSourcePrefix(path string) (string, error) {
	prefix := reviewSourceDirectory + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", errors.New("meeting source artifact is outside the source directory")
	}
	result := strings.TrimPrefix(path, prefix)
	if !validMeetingSourcePath(result) {
		return "", errors.New("meeting source artifact path is invalid")
	}
	return result, nil
}

func prefixMeetingSourceArtifact(value ReviewArtifact) ReviewArtifact {
	value.Path = filepath.ToSlash(filepath.Join(reviewSourceDirectory, value.Path))
	return value
}

func prefixMeetingSourceAttempt(source ReviewSourceAttempt) ReviewAttempt {
	media := cloneReviewMedia(source.Media)
	for index := range media {
		media[index].Path = filepath.ToSlash(filepath.Join(reviewSourceDirectory, media[index].Path))
	}
	artifacts := make([]ReviewArtifact, len(source.MediaArtifacts))
	for index, artifact := range source.MediaArtifacts {
		artifacts[index] = prefixMeetingSourceArtifact(artifact)
	}
	return ReviewAttempt{
		Ordinal: source.Ordinal, Case: source.Case, Trial: source.Trial,
		Deterministic: cloneTaskOutcome(source.Deterministic),
		Cell:          cloneMeetingCell(source.Cell), Provenance: source.Provenance,
		ExecutionRequirement: cloneMeetingExecutionRequirement(source.ExecutionRequirement),
		Context:              prefixMeetingSourceArtifact(source.Context),
		MediaManifest:        prefixMeetingSourceArtifact(source.MediaManifest),
		Media:                media, MediaArtifacts: artifacts, VideoStatus: source.VideoStatus,
		ReviewStatus: "pending", RunOrigin: source.RunOrigin,
		ExecutionStatus: source.ExecutionStatus, Reportable: source.Reportable,
		ReportabilityNote: source.ReportabilityNote,
	}
}

func stageMeetingReviewSource(
	ctx context.Context, sourceDirectory string, manifest ReviewSourceManifest,
) (ReviewSourceBundle, error) {
	if ctx == nil {
		return ReviewSourceBundle{}, errors.New("stage meeting review source: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ReviewSourceBundle{}, err
	}
	root, rootInfo, err := openMeetingSourceRoot(sourceDirectory)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	defer root.Close()
	for _, marker := range []string{reviewSourceManifestStage, reviewSourceManifestName} {
		if _, err := root.Lstat(marker); err == nil {
			return ReviewSourceBundle{}, errors.New("meeting review source marker already exists")
		} else if !os.IsNotExist(err) {
			return ReviewSourceBundle{}, errors.New("inspect meeting review source marker")
		}
	}
	files, err := snapshotMeetingSourceFiles(ctx, root, reviewSourceManifestStage, false)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	manifest.Files = files
	manifest.FileSetSHA256, err = meetingSourceFileSetDigest(files)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	payload, err := marshalMeetingSource(manifest)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	receipt, err := buildMeetingSourceReceipt(sourceDirectory, payload, manifest)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	if err := verifyMeetingSourceRootIdentity(sourceDirectory, root, rootInfo); err != nil {
		return ReviewSourceBundle{}, err
	}
	if err := writeMeetingReviewFile(root, reviewSourceManifestStage, payload); err != nil {
		return ReviewSourceBundle{}, errors.New("write meeting review source staged marker")
	}
	if err := syncMeetingSourceRoot(root); err != nil {
		return ReviewSourceBundle{}, err
	}
	if err := verifyMeetingSourceRootIdentity(sourceDirectory, root, rootInfo); err != nil {
		return ReviewSourceBundle{}, err
	}
	return ReviewSourceBundle{Manifest: manifest, Receipt: receipt}, nil
}

func commitStagedMeetingReviewSource(
	ctx context.Context, sourceDirectory string, expected ReviewSourceReceipt,
	anchor ReviewSourceReceiptAnchor, operations meetingSourcePublicationOperations,
) (bundle ReviewSourceBundle, resultErr error) {
	if ctx == nil {
		return ReviewSourceBundle{}, errors.New("commit meeting review source: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return ReviewSourceBundle{}, err
	}
	if err := validateMeetingSourceReceipt(expected); err != nil || nilInterface(anchor) {
		return ReviewSourceBundle{}, errors.New("meeting review source publication input is invalid")
	}
	root, rootInfo, err := openMeetingSourceRoot(sourceDirectory)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			bundle = ReviewSourceBundle{}
			resultErr = errors.Join(resultErr, errors.New("close meeting source publication root"))
		}
	}()
	stageExists, finalExists, err := meetingSourceMarkerState(root)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	if stageExists && finalExists {
		return ReviewSourceBundle{}, errors.New("meeting source has both staged and final markers")
	}
	publicationID := reviewDigest([]byte(
		"openrealtime.meeting-source-publication/v1\x00" + sourceDirectory,
	))
	lease, err := anchor.Acquire(ctx, publicationID)
	if err != nil {
		return ReviewSourceBundle{}, fmt.Errorf("acquire meeting source publication lease: %w", err)
	}
	if nilInterface(lease) {
		return ReviewSourceBundle{}, errors.New("meeting source anchor returned a nil lease")
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			bundle = ReviewSourceBundle{}
			resultErr = errors.Join(resultErr, errors.New("close meeting source publication lease"))
		}
	}()
	retained, found, err := lease.Load(ctx)
	if err != nil {
		return ReviewSourceBundle{}, fmt.Errorf("load meeting source receipt: %w", err)
	}
	if found {
		if retained.Directory != expected.Directory ||
			!samePortableMeetingSourceReceipt(retained, expected) {
			return ReviewSourceBundle{}, errors.New("durable meeting source receipt differs from staged source")
		}
	} else {
		if finalExists {
			return ReviewSourceBundle{}, errors.New("final meeting source exists without a durable receipt")
		}
		if !stageExists {
			return ReviewSourceBundle{}, errors.New("meeting source has no staged marker to publish")
		}
		if _, err := verifyMeetingReviewSourceAtMarker(
			ctx, sourceDirectory, expected, reviewSourceManifestStage, false,
		); err != nil {
			return ReviewSourceBundle{}, fmt.Errorf("verify staged meeting source: %w", err)
		}
		if err := lease.Publish(ctx, expected); err != nil {
			return ReviewSourceBundle{}, fmt.Errorf("durably publish meeting source receipt: %w", err)
		}
		retained, found, err = lease.Load(ctx)
		if err != nil || !found || retained.Directory != expected.Directory ||
			!samePortableMeetingSourceReceipt(retained, expected) {
			return ReviewSourceBundle{}, errors.New("reopen durable meeting source receipt")
		}
	}
	if finalExists {
		if err := sealMeetingReviewSourceTreeContext(ctx, sourceDirectory); err != nil {
			return ReviewSourceBundle{}, errors.New("reseal recovered final meeting source")
		}
		return VerifyMeetingReviewSource(ctx, sourceDirectory, expected)
	}
	if !stageExists {
		return ReviewSourceBundle{}, errors.New("meeting source marker is unavailable")
	}
	if _, err := verifyMeetingReviewSourceAtMarker(
		ctx, sourceDirectory, expected, reviewSourceManifestStage, false,
	); err != nil {
		return ReviewSourceBundle{}, fmt.Errorf("reverify staged meeting source before promotion: %w", err)
	}
	if operations.beforePromotion != nil {
		if err := operations.beforePromotion(); err != nil {
			return ReviewSourceBundle{}, errors.New("meeting source promotion interrupted after receipt publication")
		}
	}
	if err := promoteMeetingSourceMarker(sourceDirectory, root, rootInfo); err != nil {
		return ReviewSourceBundle{}, err
	}
	if err := sealMeetingReviewSourceTreeContext(ctx, sourceDirectory); err != nil {
		return ReviewSourceBundle{}, errors.New("seal promoted meeting source tree")
	}
	opened, err := VerifyMeetingReviewSource(ctx, sourceDirectory, expected)
	if err != nil {
		return ReviewSourceBundle{}, fmt.Errorf("verify promoted meeting source: %w", err)
	}
	return opened, nil
}

type meetingSourcePublicationOperations struct {
	beforePromotion func() error
}

func meetingSourceMarkerState(root *os.Root) (stage, final bool, resultErr error) {
	if root == nil {
		return false, false, errors.New("meeting source root is unavailable")
	}
	inspect := func(name string) (bool, error) {
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, errors.New("meeting source marker is invalid")
		}
		file, err := root.Open(name)
		if err != nil {
			return false, errors.New("open meeting source marker")
		}
		defer file.Close()
		if fileidentity.RequireSingleLink(file) != nil {
			return false, errors.New("meeting source marker has an external hardlink")
		}
		return true, nil
	}
	stage, err := inspect(reviewSourceManifestStage)
	if err != nil {
		return false, false, err
	}
	final, err = inspect(reviewSourceManifestName)
	return stage, final, err
}

func promoteMeetingSourceMarker(
	directory string, root *os.Root, expectedRoot os.FileInfo,
) error {
	if err := verifyMeetingSourceRootIdentity(directory, root, expectedRoot); err != nil {
		return err
	}
	stageInfo, err := root.Lstat(reviewSourceManifestStage)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.Mode().IsRegular() {
		return errors.New("staged meeting source marker is invalid")
	}
	stageFile, err := root.Open(reviewSourceManifestStage)
	if err != nil {
		return errors.New("open staged meeting source marker")
	}
	if fileidentity.RequireSingleLink(stageFile) != nil {
		stageFile.Close()
		return errors.New("staged meeting source marker has an external hardlink")
	}
	stageFile.Close()
	if _, err := root.Lstat(reviewSourceManifestName); err == nil {
		return errors.New("final meeting source marker already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("inspect final meeting source marker")
	}
	if err := renameMeetingSourceMarkerNoReplace(
		directory, reviewSourceManifestStage, reviewSourceManifestName, expectedRoot,
	); err != nil {
		return fmt.Errorf("promote meeting source marker exclusively: %w", err)
	}
	if err := syncMeetingSourceRoot(root); err != nil {
		return errors.New("sync promoted meeting source marker")
	}
	finalInfo, finalErr := root.Lstat(reviewSourceManifestName)
	_, stageErr := root.Lstat(reviewSourceManifestStage)
	if finalErr != nil || finalInfo.Mode()&os.ModeSymlink != 0 || !finalInfo.Mode().IsRegular() ||
		!os.SameFile(stageInfo, finalInfo) || !os.IsNotExist(stageErr) {
		return errors.New("meeting source marker changed during promotion")
	}
	return verifyMeetingSourceRootIdentity(directory, root, expectedRoot)
}

// VerifyMeetingReviewSource verifies the exact independently sealed source
// population against a caller-retained receipt. It anchors every read to one
// os.Root identity and rejects symlinks, duplicate/hard-linked file identities,
// unexpected nodes, root swaps, and concurrent mutation.
func VerifyMeetingReviewSource(
	ctx context.Context, directory string, expected ReviewSourceReceipt,
) (bundle ReviewSourceBundle, resultErr error) {
	return verifyMeetingReviewSourceAtMarker(
		ctx, directory, expected, reviewSourceManifestName, true,
	)
}

func verifyMeetingReviewSourceAtMarker(
	ctx context.Context, directory string, expected ReviewSourceReceipt,
	marker string, requireSealed bool,
) (bundle ReviewSourceBundle, resultErr error) {
	if ctx == nil {
		return ReviewSourceBundle{}, errors.New("verify meeting review source: nil context")
	}
	if marker != reviewSourceManifestName && marker != reviewSourceManifestStage {
		return ReviewSourceBundle{}, errors.New("meeting review source marker is invalid")
	}
	if err := context.Cause(ctx); err != nil {
		return ReviewSourceBundle{}, err
	}
	if err := validateMeetingSourceReceipt(expected); err != nil {
		return ReviewSourceBundle{}, err
	}
	root, rootInfo, err := openMeetingSourceRoot(directory)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			bundle = ReviewSourceBundle{}
			resultErr = errors.Join(resultErr, errors.New("close verified meeting review source"))
		}
	}()
	meetingSourceVerifyHook(ctx, "opened")
	manifestInfo, err := meetingSourceFileInfo(root, marker, requireSealed)
	if err != nil || manifestInfo.Size() > maximumReviewManifestBytes {
		return ReviewSourceBundle{}, errors.New("meeting review source has no valid commit marker")
	}
	manifestArtifact := ReviewArtifact{
		Path: marker, SizeBytes: manifestInfo.Size(), MediaType: "application/json",
	}
	manifestPayload, err := readMeetingSourceFile(ctx, root, manifestArtifact, requireSealed)
	if err != nil || reviewDigest(manifestPayload) != expected.ManifestSHA256 {
		return ReviewSourceBundle{}, errors.New("meeting review source manifest differs from its receipt")
	}
	manifest, err := decodeMeetingSourceManifest(manifestPayload)
	if err != nil {
		return ReviewSourceBundle{}, err
	}
	files, err := snapshotMeetingSourceFiles(ctx, root, marker, requireSealed)
	if err != nil || !reflect.DeepEqual(files, manifest.Files) {
		return ReviewSourceBundle{}, errors.New("meeting review source exact tree differs from its manifest")
	}
	fileSetSHA256, err := meetingSourceFileSetDigest(files)
	if err != nil || fileSetSHA256 != manifest.FileSetSHA256 {
		return ReviewSourceBundle{}, errors.New("meeting review source file-set digest differs")
	}
	resultPayload, err := readMeetingSourceFile(ctx, root, manifest.Result, requireSealed)
	if err != nil {
		return ReviewSourceBundle{}, errors.New("read meeting review source result")
	}
	result, err := decodeMeetingSourceResult(resultPayload)
	if err != nil || !reflect.DeepEqual(result.Cell, manifest.Cell) ||
		!reflect.DeepEqual(result.Provenance, manifest.Provenance) {
		return ReviewSourceBundle{}, errors.New("meeting review source result, cell, or provenance differs")
	}
	if err := verifyMeetingSourceManifest(directory, root, manifest, result, requireSealed); err != nil {
		return ReviewSourceBundle{}, err
	}
	canonical, err := marshalMeetingSource(manifest)
	if err != nil || !bytes.Equal(canonical, manifestPayload) {
		return ReviewSourceBundle{}, errors.New("meeting review source manifest is noncanonical")
	}
	receipt, err := buildMeetingSourceReceipt(directory, manifestPayload, manifest)
	if err != nil || !samePortableMeetingSourceReceipt(receipt, expected) {
		return ReviewSourceBundle{}, errors.New("meeting review source differs from the expected receipt")
	}
	if err := verifyMeetingSourceRootIdentity(directory, root, rootInfo); err != nil {
		return ReviewSourceBundle{}, err
	}
	meetingSourceVerifyHook(ctx, "before_final_snapshot")
	finalFiles, err := snapshotMeetingSourceFiles(ctx, root, marker, requireSealed)
	if err != nil || !reflect.DeepEqual(finalFiles, files) {
		return ReviewSourceBundle{}, errors.New("meeting review source changed during verification")
	}
	finalManifest, err := readMeetingSourceFile(ctx, root, manifestArtifact, requireSealed)
	if err != nil || !bytes.Equal(finalManifest, manifestPayload) {
		return ReviewSourceBundle{}, errors.New("meeting review source manifest changed during verification")
	}
	if err := verifyMeetingSourceRootIdentity(directory, root, rootInfo); err != nil {
		return ReviewSourceBundle{}, err
	}
	return ReviewSourceBundle{Manifest: manifest, Result: result, Receipt: receipt}, nil
}

func verifyMeetingSourceManifest(
	directory string, root *os.Root, manifest ReviewSourceManifest, result bench.Result,
	requireSealed bool,
) error {
	if manifest.Format != ReviewSourceFormat || manifest.FormatVersion != ReviewSourceFormatVersion ||
		manifest.Suite != SuiteName || manifest.Expected != ExpectedTasks() ||
		result.Suite != SuiteName || result.Expected != ExpectedTasks() ||
		!meetingResultSummaryIsFinal(result) || len(manifest.Attempts) > ExpectedTasks() {
		return errors.New("meeting review source manifest identity is invalid")
	}
	coreErr := result.Reportable()
	if manifest.CoreReportable != (coreErr == nil) ||
		(coreErr != nil && manifest.CoreReportability != coreErr.Error()) ||
		(coreErr == nil && manifest.CoreReportability != "") {
		return errors.New("meeting review source core reportability differs")
	}
	resultTasks := make(map[string]bench.TaskOutcome, len(result.Tasks))
	for _, outcome := range result.Tasks {
		if _, known := meetingCaseOrdinal(outcome.ID); !known {
			return errors.New("meeting review source result contains an unknown task")
		}
		if _, duplicate := resultTasks[outcome.ID]; duplicate {
			return errors.New("meeting review source result duplicates a task")
		}
		resultTasks[outcome.ID] = outcome
	}
	allReportable := true
	for index, attempt := range manifest.Attempts {
		ordinal, known := meetingCaseOrdinal(attempt.Case)
		outcome, found := resultTasks[attempt.Case]
		if !known || attempt.Ordinal != ordinal || attempt.Trial != 1 ||
			(index > 0 && manifest.Attempts[index-1].Ordinal >= attempt.Ordinal) || !found ||
			!reflect.DeepEqual(outcome, attempt.Deterministic) ||
			!reflect.DeepEqual(attempt.Cell, result.Cell) ||
			!reflect.DeepEqual(attempt.Provenance, result.Provenance) ||
			!reflect.DeepEqual(attempt.ExecutionRequirement, result.Cell.Execution) {
			return errors.New("meeting review source attempt differs from the exact result")
		}
		hasVideo := false
		for _, media := range attempt.Media {
			if media.Kind == "video" {
				hasVideo = true
			}
		}
		if hasVideo != (attempt.VideoStatus == "retained-playable-video") ||
			(!hasVideo && attempt.VideoStatus != "audio-only-no-video-factory") {
			return errors.New("meeting review source video status differs from full-decoded media")
		}
		wantReportable, wantNote := meetingAttemptReportability(
			attempt.RunOrigin, attempt.ExecutionRequirement, attempt.Deterministic,
			attempt.Cell, hasVideo,
		)
		if attempt.ExecutionStatus != meetingExecutionStatus(attempt.ExecutionRequirement, attempt.Deterministic) ||
			attempt.Reportable != wantReportable || attempt.ReportabilityNote != wantNote {
			return errors.New("meeting review source attempt reportability differs")
		}
		allReportable = allReportable && attempt.Reportable
		contextPayload, err := readMeetingSourceFile(
			context.Background(), root, attempt.Context, requireSealed,
		)
		if err != nil {
			return errors.New("verify meeting review source context")
		}
		contextValue, err := decodeMeetingReviewContext(contextPayload)
		if err != nil || contextValue.ResultSHA256 != manifest.Result.SHA256 ||
			contextValue.Case != attempt.Case || !reflect.DeepEqual(contextValue.Cell, result.Cell) ||
			!reflect.DeepEqual(contextValue.Provenance, result.Provenance) ||
			!reflect.DeepEqual(contextValue.Outcome, attempt.Deterministic) ||
			!reflect.DeepEqual(contextValue.RunOrigin, attempt.RunOrigin) ||
			!reflect.DeepEqual(contextValue.ExecutionRequirement, attempt.ExecutionRequirement) ||
			!meetingTranscriptExecutionMatchesOutcome(contextValue.Transcript, contextValue.Outcome) {
			return errors.New("meeting review source context differs from exact execution evidence")
		}
		outerMediaDirectory, _, namespaceOK := meetingAttemptArtifactNamespaces(
			attempt.Ordinal, attempt.Case,
		)
		mediaDirectory, stripErr := stripMeetingSourcePrefix(outerMediaDirectory)
		if !namespaceOK || stripErr != nil || attempt.MediaManifest.Path != filepath.ToSlash(filepath.Join(
			mediaDirectory, "manifest.json",
		)) {
			return errors.New("meeting review source media manifest is outside its anchored attempt namespace")
		}
		verified, err := reviewmediaVerifySource(directory, mediaDirectory, attempt.MediaManifest.SHA256)
		if err != nil {
			return err
		}
		media, err := retainedMeetingReviewMedia(verified, attempt.MediaArtifacts, mediaDirectory)
		if err != nil || !reflect.DeepEqual(prefixMeetingReviewMedia(mediaDirectory, media), attempt.Media) {
			return errors.New("meeting review source media differs from its receipt")
		}
	}
	wantComplete := len(manifest.Attempts) == ExpectedTasks() && len(manifest.Missing) == 0 &&
		len(result.Tasks) == ExpectedTasks()
	if manifest.Complete != wantComplete || manifest.Reportable != (wantComplete && manifest.CoreReportable && allReportable) {
		return errors.New("meeting review source population/reportability is inconsistent")
	}
	return nil
}

// Kept as a tiny adapter so review_source.go does not duplicate the media
// package's independent full-decode verifier contract.
func reviewmediaVerifySource(directory, relative, manifestSHA256 string) (reviewmedia.Manifest, error) {
	verified, err := reviewmedia.VerifyBundle(filepath.Join(directory, filepath.FromSlash(relative)), manifestSHA256)
	if err != nil {
		return reviewmedia.Manifest{}, errors.New("verify meeting review source media bundle")
	}
	return verified, nil
}

func decodeMeetingSourceResult(payload []byte) (bench.Result, error) {
	if len(payload) == 0 || len(payload) > maximumReviewManifestBytes || strictjson.Validate(payload) != nil {
		return bench.Result{}, errors.New("meeting review source result is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result bench.Result
	if err := decoder.Decode(&result); err != nil {
		return bench.Result{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return bench.Result{}, errors.New("meeting review source result has trailing data")
	}
	canonical, err := json.MarshalIndent(result, "", "  ")
	canonical = append(canonical, '\n')
	if err != nil || !bytes.Equal(canonical, payload) {
		return bench.Result{}, errors.New("meeting review source result is noncanonical")
	}
	return result, nil
}

func marshalMeetingSource(manifest ReviewSourceManifest) ([]byte, error) {
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) > maximumReviewManifestBytes {
		return nil, errors.New("encode meeting review source manifest")
	}
	payload = append(payload, '\n')
	if strictjson.Validate(payload) != nil {
		return nil, errors.New("meeting review source manifest is not strict JSON")
	}
	return payload, nil
}

func decodeMeetingSourceManifest(payload []byte) (ReviewSourceManifest, error) {
	if len(payload) == 0 || len(payload) > maximumReviewManifestBytes || strictjson.Validate(payload) != nil {
		return ReviewSourceManifest{}, errors.New("meeting review source manifest is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest ReviewSourceManifest
	if err := decoder.Decode(&manifest); err != nil {
		return ReviewSourceManifest{}, errors.New("decode meeting review source manifest")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ReviewSourceManifest{}, errors.New("meeting review source manifest has trailing data")
	}
	return manifest, nil
}

func meetingSourceFileSetDigest(files []ReviewArtifact) (string, error) {
	payload, err := json.Marshal(files)
	if err != nil || len(payload) == 0 || len(payload) > maximumReviewManifestBytes {
		return "", errors.New("encode meeting review source file set")
	}
	return reviewDigest(payload), nil
}

func buildMeetingSourceReceipt(
	directory string, manifestPayload []byte, manifest ReviewSourceManifest,
) (ReviewSourceReceipt, error) {
	receipt := ReviewSourceReceipt{
		Format: ReviewSourceReceiptFormat, FormatVersion: ReviewSourceReceiptVersion,
		Directory: directory, ManifestSHA256: reviewDigest(manifestPayload),
		FileSetSHA256: manifest.FileSetSHA256, ResultSHA256: manifest.Result.SHA256,
	}
	payload, err := json.Marshal(struct {
		Format         string `json:"format"`
		FormatVersion  int    `json:"format_version"`
		ManifestSHA256 string `json:"manifest_sha256"`
		FileSetSHA256  string `json:"file_set_sha256"`
		ResultSHA256   string `json:"result_sha256"`
	}{receipt.Format, receipt.FormatVersion, receipt.ManifestSHA256,
		receipt.FileSetSHA256, receipt.ResultSHA256})
	if err != nil {
		return ReviewSourceReceipt{}, err
	}
	receipt.ReceiptSHA256 = reviewDigest(payload)
	return receipt, nil
}

func validateMeetingSourceReceipt(receipt ReviewSourceReceipt) error {
	if receipt.Format != ReviewSourceReceiptFormat || receipt.FormatVersion != ReviewSourceReceiptVersion ||
		!validReviewDigest(receipt.ManifestSHA256) || !validReviewDigest(receipt.FileSetSHA256) ||
		!validReviewDigest(receipt.ResultSHA256) || !validReviewDigest(receipt.ReceiptSHA256) ||
		!filepath.IsAbs(receipt.Directory) || filepath.Clean(receipt.Directory) != receipt.Directory ||
		receipt.Directory == filepath.Dir(receipt.Directory) {
		return errors.New("meeting review source receipt identity is invalid")
	}
	payload, _ := json.Marshal(struct {
		Format         string `json:"format"`
		FormatVersion  int    `json:"format_version"`
		ManifestSHA256 string `json:"manifest_sha256"`
		FileSetSHA256  string `json:"file_set_sha256"`
		ResultSHA256   string `json:"result_sha256"`
	}{receipt.Format, receipt.FormatVersion, receipt.ManifestSHA256,
		receipt.FileSetSHA256, receipt.ResultSHA256})
	if reviewDigest(payload) != receipt.ReceiptSHA256 {
		return errors.New("meeting review source receipt digest is invalid")
	}
	return nil
}

func samePortableMeetingSourceReceipt(left, right ReviewSourceReceipt) bool {
	return left.Format == right.Format && left.FormatVersion == right.FormatVersion &&
		left.ManifestSHA256 == right.ManifestSHA256 && left.FileSetSHA256 == right.FileSetSHA256 &&
		left.ResultSHA256 == right.ResultSHA256 && left.ReceiptSHA256 == right.ReceiptSHA256
}

func openMeetingSourceRoot(directory string) (*os.Root, os.FileInfo, error) {
	if strings.TrimSpace(directory) == "" || filepath.Clean(directory) != directory ||
		!filepath.IsAbs(directory) || directory == filepath.Dir(directory) {
		return nil, nil, errors.New("meeting review source directory is invalid")
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return nil, nil, errors.New("meeting review source is not an exact directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, errors.New("open meeting review source directory")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(visible, opened) {
		_ = root.Close()
		return nil, nil, errors.New("meeting review source changed while opening")
	}
	return root, opened, nil
}

func verifyMeetingSourceRootIdentity(directory string, root *os.Root, expected os.FileInfo) error {
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		!os.SameFile(visible, expected) {
		return errors.New("meeting review source visible root identity changed")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, expected) {
		return errors.New("meeting review source open root identity changed")
	}
	return nil
}

func snapshotMeetingSourceFiles(
	ctx context.Context, root *os.Root, exclude string, requireSealed bool,
) ([]ReviewArtifact, error) {
	var files []ReviewArtifact
	var identities []os.FileInfo
	total := int64(0)
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		path = filepath.ToSlash(path)
		if path == exclude {
			return nil
		}
		info, err := root.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("meeting review source tree contains a symlink or unstable node")
		}
		if entry.IsDir() {
			if requireSealed && info.Mode().Perm()&0o222 != 0 {
				return errors.New("meeting review source tree contains a writable directory")
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 128<<20 ||
			(requireSealed && info.Mode().Perm()&0o222 != 0) {
			return errors.New("meeting review source tree contains an invalid regular file")
		}
		for _, previous := range identities {
			if os.SameFile(previous, info) {
				return errors.New("meeting review source tree contains duplicate hard-linked identities")
			}
		}
		identities = append(identities, info)
		if total > maximumReviewSourceBytes-info.Size() {
			return errors.New("meeting review source tree exceeds its aggregate bound")
		}
		total += info.Size()
		artifact := ReviewArtifact{Path: path, SizeBytes: info.Size(), MediaType: meetingArtifactMediaType(path)}
		payload, err := readMeetingSourceFile(ctx, root, artifact, requireSealed)
		if err != nil {
			return err
		}
		artifact.SHA256 = reviewDigest(payload)
		files = append(files, artifact)
		if len(files) > maximumReviewSourceFiles {
			return errors.New("meeting review source tree has too many files")
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		return nil, errors.New("snapshot exact meeting review source tree")
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return files, nil
}

func meetingSourceFileInfo(root *os.Root, path string, requireSealed bool) (os.FileInfo, error) {
	if !validMeetingSourcePath(path) {
		return nil, errors.New("meeting review source file path is invalid")
	}
	info, err := root.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > 128<<20 || (requireSealed && info.Mode().Perm()&0o222 != 0) {
		return nil, errors.New("meeting review source file is missing, writable, or invalid")
	}
	return info, nil
}

func readMeetingSourceFile(
	ctx context.Context, root *os.Root, expected ReviewArtifact, requireSealed bool,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("read meeting review source file: nil context")
	}
	before, err := meetingSourceFileInfo(root, expected.Path, requireSealed)
	if err != nil {
		return nil, err
	}
	if expected.SizeBytes > 0 && before.Size() != expected.SizeBytes {
		return nil, errors.New("meeting review source file size differs")
	}
	file, err := root.Open(expected.Path)
	if err != nil {
		return nil, errors.New("open meeting review source file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("meeting review source file changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, before.Size()+1))
	if err != nil || int64(len(payload)) != before.Size() {
		return nil, errors.New("read exact meeting review source file")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	after, err := meetingSourceFileInfo(root, expected.Path, requireSealed)
	if err != nil || !os.SameFile(before, after) || !os.SameFile(opened, after) ||
		before.Mode() != after.Mode() || before.ModTime() != after.ModTime() ||
		fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("meeting review source file changed while reading")
	}
	if expected.SHA256 != "" && reviewDigest(payload) != expected.SHA256 {
		return nil, errors.New("meeting review source file digest differs")
	}
	return payload, nil
}

func validMeetingSourcePath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		filepath.ToSlash(path) == path && !strings.HasPrefix(path, "../") &&
		!strings.Contains(path, "/../") && !strings.ContainsRune(path, '\x00')
}

func syncMeetingSourceRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open meeting review source for sync")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync meeting review source")
	}
	return nil
}
