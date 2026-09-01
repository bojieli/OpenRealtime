package management

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

// ValidateSourceReadRequest validates the complete rooted lookup envelope
// without opening a filesystem path or granting authority.
func ValidateSourceReadRequest(request SourceReadRequest) error {
	if request.FormatVersion != SourceReadFormatVersion || !CanonicalDigest(request.RootIdentity) {
		return fmt.Errorf("%w: source-read version or root identity", ErrInvalid)
	}
	if !canonicalSourceWritePath(request.Path) {
		return fmt.Errorf("%w: source-read path", ErrInvalid)
	}
	return nil
}

// ValidateSourceReadResult binds an untrusted provider response to the exact
// request, source bytes, and deterministic result digest.
func ValidateSourceReadResult(request SourceReadRequest, result SourceReadResult) error {
	if err := ValidateSourceReadRequest(request); err != nil {
		return err
	}
	if result.FormatVersion != SourceReadFormatVersion ||
		result.RootIdentity != request.RootIdentity || result.Path != request.Path ||
		len(result.Source) == 0 || len(result.Source) > maxManagedSourceBytes ||
		!utf8.ValidString(result.Source) || result.SourceBytes != uint64(len(result.Source)) ||
		result.SourceDigest != digestSource(result.Source) || !CanonicalDigest(result.SourceDigest) ||
		!CanonicalDigest(result.ResultDigest) {
		return fmt.Errorf("%w: source-read result identity", ErrConflict)
	}
	if result.ResultDigest != sourceReadResultDigest(result) {
		return fmt.Errorf("%w: source-read result digest", ErrConflict)
	}
	return nil
}

// NewSourceReadResult constructs canonical response evidence for an alternate
// provider that has already acquired the exact requested source bytes.
func NewSourceReadResult(request SourceReadRequest, source string) (SourceReadResult, error) {
	if err := ValidateSourceReadRequest(request); err != nil {
		return SourceReadResult{}, err
	}
	if len(source) == 0 || len(source) > maxManagedSourceBytes || !utf8.ValidString(source) {
		return SourceReadResult{}, fmt.Errorf("%w: source-read payload", ErrInvalid)
	}
	result := SourceReadResult{
		FormatVersion: SourceReadFormatVersion,
		RootIdentity:  request.RootIdentity,
		Path:          request.Path,
		Source:        source,
		SourceDigest:  digestSource(source),
		SourceBytes:   uint64(len(source)),
	}
	result.ResultDigest = sourceReadResultDigest(result)
	return result, nil
}

func sourceReadResultDigest(result SourceReadResult) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("openrealtime.management.source-read-result/v1\x00"))
	for _, field := range []string{result.RootIdentity, result.Path, result.SourceDigest} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(field))
	}
	var sourceBytes [8]byte
	binary.BigEndian.PutUint64(sourceBytes[:], result.SourceBytes)
	_, _ = digest.Write(sourceBytes[:])
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// Read returns one stable bounded source snapshot beneath the held root. It is
// serialized with publication so a single RootedSourcePublisher has one
// linear source-access history.
func (publisher *RootedSourcePublisher) Read(
	ctx context.Context, request SourceReadRequest,
) (SourceReadResult, error) {
	return publisher.read(ctx, request, sourceReadingOperations{})
}

type sourceReadingOperations struct {
	afterInitialSnapshot func() error
}

func (publisher *RootedSourcePublisher) read(
	ctx context.Context, request SourceReadRequest, operations sourceReadingOperations,
) (SourceReadResult, error) {
	if err := ValidateSourceReadRequest(request); err != nil {
		return SourceReadResult{}, err
	}
	if publisher == nil {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w: nil rooted source service", ErrUnavailable)
	}
	if err := checkAuthoringContext(ctx); err != nil {
		return SourceReadResult{}, err
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed || publisher.root == nil {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w: rooted source service is closed", ErrUnavailable)
	}
	if request.RootIdentity != publisher.rootIdentity {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w: root identity", ErrConflict)
	}

	parent, parentIdentity, target, err := publisher.openSourceParent("read authoring source", request.Path)
	if err != nil {
		return SourceReadResult{}, err
	}
	defer func() { _ = parent.Close() }()
	initial, err := readSourceSnapshot(parent, target, publisher.maxBytes)
	if err != nil {
		return SourceReadResult{}, sourceReadTargetError("inspect source", err)
	}
	defer closeSourceSnapshot(initial)
	if operations.afterInitialSnapshot != nil {
		if err := operations.afterInitialSnapshot(); err != nil {
			return SourceReadResult{}, fmt.Errorf("read authoring source: %w: snapshot check", ErrUnavailable)
		}
	}
	if err := checkAuthoringContext(ctx); err != nil {
		return SourceReadResult{}, err
	}

	final, err := readSourceSnapshot(parent, target, publisher.maxBytes)
	if err != nil {
		return SourceReadResult{}, sourceReadTargetError("recheck source", err)
	}
	defer closeSourceSnapshot(final)
	if !os.SameFile(initial.info, final.info) || initial.digest != final.digest {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w: source changed during acquisition", ErrConflict)
	}
	visible, err := parent.Lstat(target)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.Mode().IsRegular() ||
		!os.SameFile(visible, final.info) || fileidentity.RequireSingleLink(final.file) != nil {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w: source identity changed", ErrConflict)
	}
	if err := publisher.verifySourceParentPath(
		"read authoring source", request.Path, parentIdentity, target,
	); err != nil {
		return SourceReadResult{}, err
	}
	if err := checkAuthoringContext(ctx); err != nil {
		return SourceReadResult{}, err
	}
	if len(final.content) == 0 || !utf8.Valid(final.content) {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w: source is not non-empty UTF-8", ErrConflict)
	}
	result, err := NewSourceReadResult(request, string(final.content))
	if err != nil {
		return SourceReadResult{}, fmt.Errorf("read authoring source: %w", err)
	}
	return result, nil
}

func sourceReadTargetError(action string, err error) error {
	detail := "source is not a stable bounded regular file"
	if errors.Is(err, errSourceAbsent) || errors.Is(err, fs.ErrNotExist) {
		detail = "source is absent"
	}
	return fmt.Errorf("read authoring source: %w: %s: %s", ErrConflict, action, detail)
}

var _ SourceReading = (*RootedSourcePublisher)(nil)
