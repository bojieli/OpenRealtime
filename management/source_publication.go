package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

const (
	defaultSourceFileMode  os.FileMode = 0o644
	sourceStagePrefix                  = ".openrealtime-authoring-"
	maximumSourcePathBytes             = 4096
	maximumStageAttempts               = 8
)

var errSourceAbsent = errors.New("authoring source is absent")

// RootedSourcePublisherOptions grants one publisher authority beneath exactly
// one already-existing absolute non-root directory. RootIdentity is an opaque
// deployment identity returned in receipts; the filesystem path is never
// returned by the service.
type RootedSourcePublisherOptions struct {
	Root         string
	RootIdentity string
	MaxBytes     int
	CreateMode   os.FileMode
}

// RootedSourcePublisher mediates create-only and stale-digest-bound source
// updates. Operations are serialized per publisher and use a held os.Root plus
// identity-checked directory handles; the service never creates directories.
type RootedSourcePublisher struct {
	mu           sync.Mutex
	root         *os.Root
	rootIdentity string
	maxBytes     int
	createMode   os.FileMode
	closed       bool
}

// NewRootedSourcePublisher opens and retains the configured root without
// publishing a file. Platforms without both atomic replacement and atomic
// no-replace publication fail closed at construction.
func NewRootedSourcePublisher(
	options RootedSourcePublisherOptions,
) (*RootedSourcePublisher, error) {
	if !atomicSourcePublicationSupported() {
		return nil, fmt.Errorf("create rooted source publisher: %w: atomic publication is unsupported", ErrUnavailable)
	}
	if !CanonicalDigest(options.RootIdentity) {
		return nil, fmt.Errorf("create rooted source publisher: %w: root identity", ErrInvalid)
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = maxManagedSourceBytes
	}
	if options.MaxBytes < 1 || options.MaxBytes > maxManagedSourceBytes {
		return nil, fmt.Errorf("create rooted source publisher: %w: source byte limit", ErrInvalid)
	}
	if options.CreateMode == 0 {
		options.CreateMode = defaultSourceFileMode
	}
	if options.CreateMode != options.CreateMode.Perm() || options.CreateMode&0o111 != 0 ||
		options.CreateMode&0o600 != 0o600 {
		return nil, fmt.Errorf("create rooted source publisher: %w: source file mode", ErrInvalid)
	}
	clean := filepath.Clean(options.Root)
	if options.Root == "" || options.Root != clean || !filepath.IsAbs(clean) || filepath.Dir(clean) == clean {
		return nil, fmt.Errorf("create rooted source publisher: %w: root must be a clean absolute non-root directory", ErrInvalid)
	}
	root, err := os.OpenRoot(clean)
	if err != nil {
		return nil, fmt.Errorf("create rooted source publisher: %w: open root", ErrUnavailable)
	}
	return &RootedSourcePublisher{
		root: root, rootIdentity: options.RootIdentity,
		maxBytes: options.MaxBytes, createMode: options.CreateMode,
	}, nil
}

// RootIdentity returns the configured opaque identity, never the root path.
func (publisher *RootedSourcePublisher) RootIdentity() string {
	if publisher == nil {
		return ""
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed {
		return ""
	}
	return publisher.rootIdentity
}

// Close idempotently erases the publisher's filesystem authority.
func (publisher *RootedSourcePublisher) Close() error {
	if publisher == nil {
		return nil
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed {
		return nil
	}
	publisher.closed = true
	err := publisher.root.Close()
	publisher.root = nil
	if err != nil {
		return fmt.Errorf("close rooted source publisher: %w", ErrUnavailable)
	}
	return nil
}

// Publish validates and commits exactly one source mutation.
func (publisher *RootedSourcePublisher) Publish(
	ctx context.Context, request SourceWriteRequest,
) (SourceWriteReceipt, error) {
	return publisher.publish(ctx, request, sourcePublicationOperations{})
}

type sourcePublicationOperations struct {
	afterInitialCheck  func() error
	beforeAtomicUpdate func() error
}

func (publisher *RootedSourcePublisher) publish(
	ctx context.Context, request SourceWriteRequest, operations sourcePublicationOperations,
) (SourceWriteReceipt, error) {
	if err := ValidateSourceWriteRequest(request); err != nil {
		return SourceWriteReceipt{}, err
	}
	if publisher == nil {
		return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: nil publisher", ErrUnavailable)
	}
	if err := checkAuthoringContext(ctx); err != nil {
		return SourceWriteReceipt{}, err
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed || publisher.root == nil {
		return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: publisher is closed", ErrUnavailable)
	}
	if request.RootIdentity != publisher.rootIdentity {
		return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: root identity", ErrConflict)
	}
	if len(request.Source) > publisher.maxBytes {
		return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: configured source byte limit", ErrInvalid)
	}

	parent, parentIdentity, target, err := publisher.openParent(request.Path)
	if err != nil {
		return SourceWriteReceipt{}, err
	}
	defer func() { _ = parent.Close() }()

	var current *sourceFileSnapshot
	switch request.Mode {
	case SourceCreate:
		if err := requireSourceAbsent(parent, target); err != nil {
			return SourceWriteReceipt{}, err
		}
	case SourceUpdate:
		current, err = readSourceSnapshot(parent, target, publisher.maxBytes)
		if err != nil {
			return SourceWriteReceipt{}, publicationTargetError("inspect update predecessor", err)
		}
		defer closeSourceSnapshot(current)
		if current.digest != request.ExpectedSourceDigest {
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: stale source digest", ErrConflict)
		}
	}
	if operations.afterInitialCheck != nil {
		if err := operations.afterInitialCheck(); err != nil {
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: pre-publication check", ErrUnavailable)
		}
	}
	if err := checkAuthoringContext(ctx); err != nil {
		return SourceWriteReceipt{}, err
	}

	mode := publisher.createMode
	if request.Mode == SourceUpdate {
		fresh, freshErr := readSourceSnapshot(parent, target, publisher.maxBytes)
		if freshErr != nil {
			return SourceWriteReceipt{}, publicationTargetError("recheck update predecessor", freshErr)
		}
		defer closeSourceSnapshot(fresh)
		if fresh.digest != request.ExpectedSourceDigest {
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: source changed before publication", ErrConflict)
		}
		current = fresh
		mode = fresh.info.Mode().Perm()
	}

	stage, stageIdentity, err := createSourceStage(parent, request.Source, mode)
	if err != nil {
		return SourceWriteReceipt{}, err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = parent.Remove(stage)
		}
	}()
	if err := checkAuthoringContext(ctx); err != nil {
		return SourceWriteReceipt{}, err
	}
	staged, err := readSourceSnapshot(parent, stage, publisher.maxBytes)
	if err != nil {
		return SourceWriteReceipt{}, publicationTargetError("verify staged source", err)
	}
	defer closeSourceSnapshot(staged)
	sourceDigest := digestSource(request.Source)
	if !os.SameFile(stageIdentity, staged.info) || staged.digest != sourceDigest ||
		staged.info.Mode().Perm() != mode || fileidentity.RequireSingleLink(staged.file) != nil {
		return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: staged source identity changed", ErrConflict)
	}
	if err := publisher.verifyParentPath(request.Path, parentIdentity, target); err != nil {
		return SourceWriteReceipt{}, err
	}

	cleanupPending := false
	switch request.Mode {
	case SourceCreate:
		if err := publishSourceCreate(parent, parentIdentity, stage, target); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: target already exists", ErrConflict)
			}
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: atomic no-replace publication", ErrUnavailable)
		}
		cleanupStage = false
	case SourceUpdate:
		visible, visibleErr := parent.Lstat(target)
		if visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(visible, current.info) || fileidentity.RequireSingleLink(current.file) != nil {
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: predecessor identity changed", ErrConflict)
		}
		if operations.beforeAtomicUpdate != nil {
			if err := operations.beforeAtomicUpdate(); err != nil {
				return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: final pre-publication check", ErrUnavailable)
			}
		}
		if err := publishSourceUpdate(parent, parentIdentity, stage, target); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: predecessor disappeared", ErrConflict)
			}
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: atomic replacement", ErrUnavailable)
		}
		oldAtStage, oldErr := parent.Lstat(stage)
		newAtTarget, newErr := parent.Lstat(target)
		previousAfterExchange, previousErr := readSourceSnapshot(parent, stage, publisher.maxBytes)
		previousDigest := ""
		if previousErr == nil {
			previousDigest = previousAfterExchange.digest
			closeSourceSnapshot(previousAfterExchange)
		}
		if oldErr != nil || newErr != nil || !os.SameFile(oldAtStage, current.info) ||
			!os.SameFile(newAtTarget, staged.info) || previousErr != nil ||
			previousDigest != request.ExpectedSourceDigest {
			// Exchange again while both names remain private/known. If rollback
			// itself fails, retain both names rather than risking deletion of the
			// predecessor under an uncertain identity.
			if rollbackErr := publishSourceUpdate(parent, parentIdentity, stage, target); rollbackErr != nil {
				cleanupStage = false
				return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: verify atomic replacement and rollback", ErrUnavailable)
			}
			return SourceWriteReceipt{}, fmt.Errorf("publish authoring source: %w: replacement identity changed", ErrConflict)
		}
		if removeErr := parent.Remove(stage); removeErr != nil {
			cleanupPending = true
		}
		cleanupStage = false
	}

	receipt := sourceWriteReceipt(request, cleanupPending)
	return receipt, nil
}

func sourceWriteReceipt(
	request SourceWriteRequest, cleanupPending bool,
) SourceWriteReceipt {
	receipt := SourceWriteReceipt{
		FormatVersion:  SourceWriteFormatVersion,
		RootIdentity:   request.RootIdentity,
		Mode:           request.Mode,
		Path:           request.Path,
		SourceDigest:   digestSource(request.Source),
		SourceBytes:    uint64(len(request.Source)),
		CleanupPending: cleanupPending,
	}
	if request.Mode == SourceUpdate {
		receipt.PreviousSourceDigest = request.ExpectedSourceDigest
	}
	receipt.ReceiptDigest = sourceWriteReceiptDigest(receipt)
	return receipt
}

func (publisher *RootedSourcePublisher) verifyParentPath(
	name string, expected os.FileInfo, expectedTarget string,
) error {
	current, identity, target, err := publisher.openParent(name)
	if current != nil {
		_ = current.Close()
	}
	if err != nil || expected == nil || identity == nil || target != expectedTarget ||
		!os.SameFile(identity, expected) {
		return fmt.Errorf("publish authoring source: %w: parent path changed", ErrConflict)
	}
	return nil
}

func (publisher *RootedSourcePublisher) openParent(
	name string,
) (*os.Root, os.FileInfo, string, error) {
	parentName, target := pathpkg.Split(name)
	parentName = strings.TrimSuffix(parentName, "/")
	if parentName == "" {
		parentName = "."
	}
	current, err := publisher.root.OpenRoot(".")
	if err != nil {
		return nil, nil, "", fmt.Errorf("publish authoring source: %w: open held root", ErrUnavailable)
	}
	if parentName != "." {
		for _, component := range strings.Split(parentName, "/") {
			before, statErr := current.Lstat(component)
			if statErr != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
				_ = current.Close()
				return nil, nil, "", fmt.Errorf("publish authoring source: %w: parent directory", ErrInvalid)
			}
			next, openErr := current.OpenRoot(component)
			if openErr != nil {
				_ = current.Close()
				return nil, nil, "", fmt.Errorf("publish authoring source: %w: open parent directory", ErrUnavailable)
			}
			after, statErr := next.Stat(".")
			if statErr != nil || !os.SameFile(before, after) {
				_ = next.Close()
				_ = current.Close()
				return nil, nil, "", fmt.Errorf("publish authoring source: %w: parent identity changed", ErrConflict)
			}
			_ = current.Close()
			current = next
		}
	}
	identity, err := current.Stat(".")
	if err != nil || !identity.IsDir() {
		_ = current.Close()
		return nil, nil, "", fmt.Errorf("publish authoring source: %w: inspect parent directory", ErrUnavailable)
	}
	return current, identity, target, nil
}

func requireSourceAbsent(parent *os.Root, name string) error {
	_, err := parent.Lstat(name)
	switch {
	case err == nil:
		return fmt.Errorf("publish authoring source: %w: target already exists", ErrConflict)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("publish authoring source: %w: inspect create target", ErrUnavailable)
	}
}

type sourceFileSnapshot struct {
	file   *os.File
	info   os.FileInfo
	digest string
}

func closeSourceSnapshot(snapshot *sourceFileSnapshot) {
	if snapshot != nil && snapshot.file != nil {
		_ = snapshot.file.Close()
	}
}

func readSourceSnapshot(parent *os.Root, name string, maximum int) (*sourceFileSnapshot, error) {
	visible, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errSourceAbsent
	}
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.Mode().IsRegular() ||
		visible.Size() < 0 || visible.Size() > int64(maximum) {
		return nil, errors.New("source is not a bounded regular file")
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, errors.New("open source")
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(visible, opened) ||
		fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("source identity changed or has another hard link")
	}
	first, err := readBoundedSource(file, maximum)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("rewind source")
	}
	second, err := readBoundedSource(file, maximum)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() ||
		opened.Mode() != after.Mode() || !opened.ModTime().Equal(after.ModTime()) ||
		!bytes.Equal(first, second) {
		return nil, errors.New("source changed while being read")
	}
	failed = false
	return &sourceFileSnapshot{file: file, info: after, digest: digestBytes(first)}, nil
}

func readBoundedSource(file *os.File, maximum int) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, errors.New("read source")
	}
	if len(value) > maximum {
		return nil, errors.New("source exceeds byte limit")
	}
	return value, nil
}

func createSourceStage(
	parent *os.Root, source string, mode os.FileMode,
) (string, os.FileInfo, error) {
	for range maximumStageAttempts {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, fmt.Errorf("publish authoring source: %w: create stage identity", ErrUnavailable)
		}
		name := sourceStagePrefix + hex.EncodeToString(nonce[:])
		file, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("publish authoring source: %w: create private stage", ErrUnavailable)
		}
		stageErr := func() error {
			if _, err := io.Copy(file, strings.NewReader(source)); err != nil {
				return errors.New("write private stage")
			}
			if err := file.Chmod(mode); err != nil {
				return errors.New("set private stage mode")
			}
			if err := file.Sync(); err != nil {
				return errors.New("sync private stage")
			}
			if err := fileidentity.RequireSingleLink(file); err != nil {
				return errors.New("private stage has another hard link")
			}
			return nil
		}()
		info, statErr := file.Stat()
		closeErr := file.Close()
		if stageErr != nil || statErr != nil || closeErr != nil || info == nil ||
			!info.Mode().IsRegular() {
			_ = parent.Remove(name)
			return "", nil, fmt.Errorf("publish authoring source: %w: finalize private stage", ErrUnavailable)
		}
		return name, info, nil
	}
	return "", nil, fmt.Errorf("publish authoring source: %w: private stage collision limit", ErrUnavailable)
}

func publicationTargetError(action string, err error) error {
	detail := "target is not a stable bounded regular file"
	if errors.Is(err, errSourceAbsent) {
		detail = "target is absent"
	}
	return fmt.Errorf("publish authoring source: %w: %s: %s", ErrConflict, action, detail)
}

// ValidateSourceWriteRequest validates the complete mutation envelope without
// reading a filesystem or granting authority.
func ValidateSourceWriteRequest(request SourceWriteRequest) error {
	if request.FormatVersion != SourceWriteFormatVersion || !CanonicalDigest(request.RootIdentity) {
		return fmt.Errorf("%w: source-write version or root identity", ErrInvalid)
	}
	if !canonicalSourceWritePath(request.Path) {
		return fmt.Errorf("%w: source-write path", ErrInvalid)
	}
	if len(request.Source) == 0 || len(request.Source) > maxManagedSourceBytes ||
		!utf8.ValidString(request.Source) {
		return fmt.Errorf("%w: source-write payload", ErrInvalid)
	}
	sourceDigest := digestSource(request.Source)
	switch request.Mode {
	case SourceCreate:
		if request.ExpectedSourceDigest != "" {
			return fmt.Errorf("%w: create request has an expected digest", ErrInvalid)
		}
	case SourceUpdate:
		if !CanonicalDigest(request.ExpectedSourceDigest) || request.ExpectedSourceDigest == sourceDigest {
			return fmt.Errorf("%w: update request has an invalid or unchanged expected digest", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: source-write mode", ErrInvalid)
	}
	return nil
}

func canonicalSourceWritePath(value string) bool {
	if value == "" || len(value) > maximumSourcePathBytes || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\\\x00\r\n") || pathpkg.IsAbs(value) ||
		pathpkg.Clean(value) != value || value == "." {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	base := pathpkg.Base(value)
	if strings.HasPrefix(base, sourceStagePrefix) {
		return false
	}
	switch strings.ToLower(pathpkg.Ext(base)) {
	case ".ortg", ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

// ValidateSourceWriteReceipt rebinds a payload-free receipt to the exact
// request and recomputes its tamper-evident digest.
func ValidateSourceWriteReceipt(request SourceWriteRequest, receipt SourceWriteReceipt) error {
	if err := ValidateSourceWriteRequest(request); err != nil {
		return err
	}
	if receipt.FormatVersion != SourceWriteFormatVersion ||
		receipt.RootIdentity != request.RootIdentity || receipt.Mode != request.Mode ||
		receipt.Path != request.Path || receipt.SourceDigest != digestSource(request.Source) ||
		receipt.SourceBytes != uint64(len(request.Source)) || !CanonicalDigest(receipt.SourceDigest) ||
		!CanonicalDigest(receipt.ReceiptDigest) {
		return fmt.Errorf("%w: source-write receipt identity", ErrConflict)
	}
	wantPrevious := ""
	if request.Mode == SourceUpdate {
		wantPrevious = request.ExpectedSourceDigest
	}
	if receipt.PreviousSourceDigest != wantPrevious ||
		receipt.ReceiptDigest != sourceWriteReceiptDigest(receipt) {
		return fmt.Errorf("%w: source-write receipt digest", ErrConflict)
	}
	return nil
}

// NewSourceWriteReceipt constructs the canonical payload-free receipt for a
// provider that has already committed request. Alternate SourcePublication
// implementations use this instead of duplicating the receipt digest format.
func NewSourceWriteReceipt(
	request SourceWriteRequest, cleanupPending bool,
) (SourceWriteReceipt, error) {
	if err := ValidateSourceWriteRequest(request); err != nil {
		return SourceWriteReceipt{}, err
	}
	return sourceWriteReceipt(request, cleanupPending), nil
}

func sourceWriteReceiptDigest(receipt SourceWriteReceipt) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("openrealtime.management.source-write-receipt/v1\x00"))
	for _, field := range []string{
		receipt.RootIdentity, string(receipt.Mode), receipt.Path,
		receipt.PreviousSourceDigest, receipt.SourceDigest,
	} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(field))
	}
	var sourceBytes [8]byte
	binary.BigEndian.PutUint64(sourceBytes[:], receipt.SourceBytes)
	_, _ = digest.Write(sourceBytes[:])
	if receipt.CleanupPending {
		_, _ = digest.Write([]byte{1})
	} else {
		_, _ = digest.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func digestSource(source string) string { return digestBytes([]byte(source)) }

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

var _ SourcePublication = (*RootedSourcePublisher)(nil)
