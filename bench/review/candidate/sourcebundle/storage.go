package sourcebundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

const (
	maximumSourceFileBytes = int64(128 << 20)
	maximumSourceFiles     = 100_000
	maximumSensitiveValues = 256
	maximumSensitiveBytes  = 256 << 10
	maximumSensitiveValue  = 4 << 10
)

func maximumSourceBytesForPath(name string) int64 {
	name = filepath.ToSlash(name)
	if name == resultName || name == manifestName {
		return int64(maximumPopulationMetadataBytes)
	}
	return maximumSourceFileBytes
}

type sensitiveGuard struct {
	values  []string
	needles [][]byte
}

func newSensitiveGuard(values []string) (sensitiveGuard, error) {
	if len(values) > maximumSensitiveValues {
		return sensitiveGuard{}, errors.New("candidate source sensitive-value count is oversized")
	}
	seen := make(map[string]struct{}, len(values)*6)
	guard := sensitiveGuard{}
	total := 0
	add := func(value []byte) {
		if len(value) == 0 {
			return
		}
		key := string(value)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		guard.needles = append(guard.needles, slices.Clone(value))
	}
	for _, value := range values {
		if value == "" || len(value) > maximumSensitiveValue || len(value) > maximumSensitiveBytes-total {
			return sensitiveGuard{}, errors.New("candidate source sensitive values exceed bounded limits")
		}
		total += len(value)
		guard.values = append(guard.values, value)
		raw := []byte(value)
		add(raw)
		if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
			add(encoded[1 : len(encoded)-1])
		}
		for _, encoding := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding,
			base64.URLEncoding, base64.RawURLEncoding,
		} {
			encoded := make([]byte, encoding.EncodedLen(len(raw)))
			encoding.Encode(encoded, raw)
			add(encoded)
		}
	}
	sort.Strings(guard.values)
	return guard, nil
}

func (guard sensitiveGuard) rejects(payload []byte) bool {
	for _, needle := range guard.needles {
		if bytes.Contains(payload, needle) {
			return true
		}
	}
	return false
}

func validateAbsolutePath(label, path string) (string, error) {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path || path == filepath.Dir(path) {
		return "", errors.New(label + " must be a clean absolute non-root path")
	}
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New(label + " parent must be a non-symlink directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return path, nil
}

func createBundleRoot(path string) (*os.Root, os.FileInfo, error) {
	parentPath, base := filepath.Dir(path), filepath.Base(path)
	before, err := os.Lstat(parentPath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, errors.New("candidate source parent is not a stable directory")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, nil, errors.New("open candidate source parent")
	}
	defer parent.Close()
	opened, openErr := parent.Stat(".")
	visible, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, visible) {
		return nil, nil, errors.New("candidate source parent changed while opening")
	}
	if err := parent.Mkdir(base, 0o700); err != nil {
		return nil, nil, errors.New("create candidate source directory exclusively")
	}
	if err := syncDirectory(parent, "."); err != nil {
		_ = parent.RemoveAll(base)
		return nil, nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		_ = parent.RemoveAll(base)
		_ = syncDirectory(parent, ".")
		return nil, nil, errors.New("open created candidate source directory")
	}
	identity, statErr := root.Stat(".")
	visible, visibleErr = os.Lstat(path)
	if statErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(identity, visible) {
		_ = root.Close()
		_ = parent.RemoveAll(base)
		_ = syncDirectory(parent, ".")
		return nil, nil, errors.New("created candidate source directory changed while opening")
	}
	return root, identity, nil
}

func openBundleRoot(path string) (*os.Root, os.FileInfo, error) {
	visible, err := os.Lstat(path)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		visible.Mode().Perm()&0o077 != 0 {
		return nil, nil, errors.New("candidate source directory is not a private stable directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, errors.New("open existing candidate source directory")
	}
	identity, statErr := root.Stat(".")
	afterOpen, visibleErr := os.Lstat(path)
	if statErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!afterOpen.IsDir() || afterOpen.Mode().Perm()&0o077 != 0 ||
		!os.SameFile(visible, identity) || !os.SameFile(identity, afterOpen) {
		_ = root.Close()
		return nil, nil, errors.New("candidate source directory changed while reopening")
	}
	return root, identity, nil
}

func acquireBundleLease(ctx context.Context, directory string) (*os.File, error) {
	if ctx == nil {
		return nil, errors.New("acquire candidate source lease: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parentPath := filepath.Dir(directory)
	name := filepath.Base(directory) + bundleLeaseSuffix
	visibleParent, err := os.Lstat(parentPath)
	if err != nil || visibleParent.Mode()&os.ModeSymlink != 0 || !visibleParent.IsDir() {
		return nil, errors.New("candidate source lease parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errors.New("open candidate source lease parent")
	}
	defer parent.Close()
	openedParent, statErr := parent.Stat(".")
	afterOpen, visibleErr := os.Lstat(parentPath)
	if statErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!afterOpen.IsDir() || !os.SameFile(visibleParent, openedParent) ||
		!os.SameFile(openedParent, afterOpen) {
		return nil, errors.New("candidate source lease parent changed while opening")
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		created, createErr := parent.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return nil, errors.New("create candidate source lease marker")
		}
		if syncErr := created.Sync(); syncErr != nil {
			_ = created.Close()
			return nil, errors.New("sync candidate source lease marker")
		}
		if closeErr := created.Close(); closeErr != nil {
			return nil, errors.New("close candidate source lease marker")
		}
		if syncErr := syncDirectory(parent, "."); syncErr != nil {
			return nil, syncErr
		}
		info, err = parent.Lstat(name)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("candidate source lease marker is invalid")
	}
	lease, err := parent.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("open candidate source lease marker")
	}
	opened, statErr := lease.Stat()
	if statErr != nil || !os.SameFile(info, opened) || fileidentity.RequireSingleLink(lease) != nil {
		_ = lease.Close()
		return nil, errors.New("candidate source lease marker changed while opening")
	}
	if err := lockSourceBundleFile(lease); err != nil {
		_ = lease.Close()
		return nil, errors.New("candidate source bundle is already open or resume is unsupported")
	}
	if err := ctx.Err(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	after, err := parent.Lstat(name)
	visibleAfterLock, parentErr := os.Lstat(parentPath)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(opened, after) || fileidentity.RequireSingleLink(lease) != nil ||
		parentErr != nil || visibleAfterLock.Mode()&os.ModeSymlink != 0 ||
		!visibleAfterLock.IsDir() || !os.SameFile(openedParent, visibleAfterLock) {
		_ = lease.Close()
		return nil, errors.New("candidate source lease marker changed after locking")
	}
	return lease, nil
}

func closeBundleLease(lease *os.File) error {
	if lease == nil {
		return nil
	}
	if err := lease.Close(); err != nil {
		return errors.New("close candidate source exclusive lease")
	}
	return nil
}

func verifyBundleRoot(root *os.Root, path string, identity os.FileInfo) error {
	if root == nil || identity == nil {
		return errors.New("candidate source directory is unavailable")
	}
	opened, openErr := root.Stat(".")
	visible, visibleErr := os.Lstat(path)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(identity, opened) || !os.SameFile(opened, visible) {
		return errors.New("candidate source directory identity changed")
	}
	return nil
}

func removeCreatedBundle(path string, root *os.Root, identity os.FileInfo) error {
	if err := verifyBundleRoot(root, path, identity); err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return errors.New("close failed candidate source directory")
	}
	parentPath, base := filepath.Dir(path), filepath.Base(path)
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return errors.New("open candidate source parent for cleanup")
	}
	defer parent.Close()
	visible, err := parent.Lstat(base)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		!os.SameFile(identity, visible) {
		return errors.New("candidate source directory changed before cleanup")
	}
	if err := parent.RemoveAll(base); err != nil {
		return errors.New("remove failed candidate source directory")
	}
	return syncDirectory(parent, ".")
}

func makeDirectory(root *os.Root, name string) error {
	if root == nil {
		return errors.New("candidate source directory is unavailable")
	}
	if err := root.Mkdir(name, 0o700); err != nil {
		return errors.New("create candidate source subdirectory exclusively")
	}
	return syncDirectory(root, filepath.ToSlash(filepath.Dir(name)))
}

func writeExclusive(root *os.Root, name string, payload []byte) (SourceFile, error) {
	if root == nil || len(payload) == 0 || int64(len(payload)) > maximumSourceBytesForPath(name) {
		return SourceFile{}, errors.New("candidate source file is empty, oversized, or unavailable")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return SourceFile{}, errors.New("create candidate source file exclusively")
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return SourceFile{}, errors.New("write candidate source file")
	}
	if err := file.Sync(); err != nil {
		return SourceFile{}, errors.New("sync candidate source file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(payload)) ||
		fileidentity.RequireSingleLink(file) != nil {
		return SourceFile{}, errors.New("candidate source file identity is invalid")
	}
	if err := file.Close(); err != nil {
		return SourceFile{}, errors.New("close candidate source file")
	}
	failed = false
	if err := syncDirectory(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
		return SourceFile{}, err
	}
	return SourceFile{
		Path: filepath.ToSlash(name), SHA256: digest(payload), SizeBytes: int64(len(payload)),
	}, nil
}

// publishAttemptEntry makes the final entry name the commit point. The
// payload is fully written and synced under an undeclared pending name first;
// a crash before the rename therefore leaves a markerless attempt that Resume
// can preserve and retry, never a partially written authoritative marker.
func publishAttemptEntry(root *os.Root, directory string, payload []byte) (SourceFile, error) {
	finalName := filepath.ToSlash(filepath.Join(directory, attemptEntryName))
	pendingName := filepath.ToSlash(filepath.Join(directory, attemptEntryPending))
	if _, err := root.Lstat(finalName); err == nil {
		return SourceFile{}, errors.New("candidate source attempt commit marker already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return SourceFile{}, errors.New("inspect candidate source attempt commit marker")
	}
	pending, err := writeExclusive(root, pendingName, payload)
	if err != nil {
		return SourceFile{}, err
	}
	renamed := false
	published := false
	defer func() {
		if !published {
			_ = root.Remove(pendingName)
			if renamed {
				_ = root.Remove(finalName)
			}
			_ = syncDirectory(root, directory)
		}
	}()
	if err := root.Rename(pendingName, finalName); err != nil {
		return SourceFile{}, errors.New("publish candidate source attempt commit marker")
	}
	renamed = true
	if err := syncDirectory(root, directory); err != nil {
		return SourceFile{}, err
	}
	retained, err := readRegular(root, finalName, pending.SizeBytes)
	if err != nil || digest(retained) != pending.SHA256 || !bytes.Equal(retained, payload) {
		return SourceFile{}, errors.New("candidate source attempt commit marker changed during publication")
	}
	published = true
	pending.Path = finalName
	return pending, nil
}

func syncDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return errors.New("open candidate source directory for sync")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync candidate source directory")
	}
	return nil
}

func walkSourceFiles(root *os.Root) ([]SourceFile, error) {
	return walkSourceFilesGuarded(root, sensitiveGuard{})
}

func walkSourceFilesGuarded(root *os.Root, guard sensitiveGuard) ([]SourceFile, error) {
	if root == nil {
		return nil, errors.New("candidate source directory is unavailable")
	}
	result := make([]SourceFile, 0, 32)
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("walk candidate source files")
		}
		if path == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("candidate source tree contains an unreadable or symlink entry")
		}
		if info.IsDir() {
			if info.Mode().Perm()&0o022 != 0 {
				return errors.New("candidate source tree contains a writable directory")
			}
			return nil
		}
		if !info.Mode().IsRegular() || len(result) >= maximumSourceFiles ||
			info.Size() <= 0 || info.Size() > maximumSourceBytesForPath(path) {
			return errors.New("candidate source tree contains an invalid file")
		}
		payload, err := readRegular(root, filepath.ToSlash(path), info.Size())
		if err != nil {
			return err
		}
		if guard.rejects(payload) {
			return errors.New("candidate source partial tree contains a declared sensitive value")
		}
		result = append(result, SourceFile{
			Path: filepath.ToSlash(path), SHA256: digest(payload), SizeBytes: int64(len(payload)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Path < result[right].Path })
	return result, nil
}

func archiveInterruptedAttempt(root *os.Root, name string) (string, error) {
	if root == nil || len(name) != sha256.Size*2 {
		return "", errors.New("archive candidate source interruption: invalid directory")
	}
	if _, err := hex.DecodeString(name); err != nil {
		return "", errors.New("archive candidate source interruption: invalid directory")
	}
	if _, err := root.Lstat("interruptions"); errors.Is(err, os.ErrNotExist) {
		if err := makeDirectory(root, "interruptions"); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", errors.New("inspect candidate source interruption directory")
	} else if info, err := root.Lstat("interruptions"); err != nil ||
		info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("candidate source interruption directory is invalid")
	}
	for sequence := 1; sequence <= maximumSourceFiles; sequence++ {
		destination := filepath.ToSlash(filepath.Join(
			"interruptions", name+"-"+sixDigitSequence(sequence),
		))
		if _, err := root.Lstat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("inspect candidate source interruption destination")
		}
		source := filepath.ToSlash(filepath.Join("attempts", name))
		if err := root.Rename(source, destination); err != nil {
			return "", errors.New("archive interrupted candidate source attempt")
		}
		if err := syncDirectory(root, "attempts"); err != nil {
			return "", err
		}
		if err := syncDirectory(root, "interruptions"); err != nil {
			return "", err
		}
		return destination, nil
	}
	return "", errors.New("candidate source interruption namespace is exhausted")
}

func sixDigitSequence(value int) string {
	const digits = "0123456789"
	var result [6]byte
	for index := len(result) - 1; index >= 0; index-- {
		result[index] = digits[value%10]
		value /= 10
	}
	return string(result[:])
}

func readRegular(root *os.Root, name string, expected int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o022 != 0 || before.Size() != expected || expected <= 0 ||
		expected > maximumSourceBytesForPath(name) {
		return nil, errors.New("candidate source file is not a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("open candidate source file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("candidate source file changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, expected+1))
	if err != nil || int64(len(payload)) != expected {
		return nil, errors.New("read exact candidate source file")
	}
	after, statErr := file.Stat()
	visible, visibleErr := root.Lstat(name)
	if statErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(opened, after) ||
		!os.SameFile(after, visible) || after.Size() != expected ||
		fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("candidate source file changed while reading")
	}
	return payload, nil
}

func writeExternalReceipt(path string, payload []byte) error {
	parentPath, base := filepath.Dir(path), filepath.Base(path)
	before, err := os.Lstat(parentPath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return errors.New("candidate source receipt parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return errors.New("open candidate source receipt parent")
	}
	defer parent.Close()
	opened, openErr := parent.Stat(".")
	visible, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, visible) {
		return errors.New("candidate source receipt parent changed while opening")
	}
	if _, err := writeExclusive(parent, base, payload); err != nil {
		return errors.New("retain candidate source receipt")
	}
	return nil
}

func readExternalRegular(path string, maximum int64) ([]byte, error) {
	if maximum <= 0 || maximum > maximumSourceFileBytes {
		return nil, errors.New("candidate source external read bound is invalid")
	}
	parentPath, base := filepath.Dir(path), filepath.Base(path)
	parentBefore, err := os.Lstat(parentPath)
	if err != nil || parentBefore.Mode()&os.ModeSymlink != 0 || !parentBefore.IsDir() {
		return nil, errors.New("candidate source external file parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errors.New("open candidate source external file parent")
	}
	defer parent.Close()
	openedParent, openErr := parent.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentBefore, openedParent) ||
		!os.SameFile(openedParent, visibleParent) {
		return nil, errors.New("candidate source external file parent changed while opening")
	}
	info, err := parent.Lstat(base)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("candidate source external file is invalid")
	}
	payload, err := readRegular(parent, base, info.Size())
	if err != nil {
		return nil, err
	}
	openedParent, openErr = parent.Stat(".")
	visibleParent, visibleErr = os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(parentBefore, openedParent) || !os.SameFile(openedParent, visibleParent) {
		return nil, errors.New("candidate source external file parent changed while reading")
	}
	return payload, nil
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestName(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cloneFiles(source []SourceFile) []SourceFile { return slices.Clone(source) }
