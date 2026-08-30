package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
)

func canonicalTimeUS(label string, milliseconds float64) (int64, error) {
	if math.IsNaN(milliseconds) || math.IsInf(milliseconds, 0) || milliseconds < 0 {
		return 0, errors.New(label + " is not a finite bounded timestamp")
	}
	scaled := milliseconds * 1000
	rounded := math.Round(scaled)
	// float64(math.MaxInt64) rounds up to 2^63, which is already outside the
	// int64 domain. Reject that boundary before conversion.
	if math.IsInf(scaled, 0) || rounded >= float64(math.MaxInt64) ||
		math.Abs(scaled-rounded) > 1e-6 {
		return 0, errors.New(label + " is not exactly representable in canonical microseconds")
	}
	result := int64(rounded)
	if result < 0 {
		return 0, errors.New(label + " exceeds the canonical microsecond range")
	}
	return result, nil
}

func validateFrame(capture bench.SessionVideoCapture) (int64, error) {
	if err := validateToken("video source", capture.Source, 128); err != nil {
		return 0, err
	}
	if strings.Contains(capture.Source, "..") {
		return 0, errors.New("video source contains a noncanonical dot sequence")
	}
	if capture.Width <= 0 || capture.Height <= 0 || capture.Width > 16_384 || capture.Height > 16_384 {
		return 0, errors.New("video frame dimensions are outside 1..16384")
	}
	episodeUS, err := canonicalTimeUS("video frame episode timestamp", capture.EpisodeAtMS)
	if err != nil {
		return 0, err
	}
	if len(capture.Data) == 0 || len(capture.Data) > maximumFrameBytes {
		return 0, errors.New("video frame byte size is outside the supported bound")
	}
	if capture.MediaType != "image/png" && capture.MediaType != "image/jpeg" {
		return 0, errors.New("video frame media type must be image/png or image/jpeg")
	}
	if detected := http.DetectContentType(capture.Data); detected != capture.MediaType {
		return 0, errors.New("video frame bytes do not match their declared media type")
	}
	configuration, format, err := image.DecodeConfig(bytes.NewReader(capture.Data))
	wantFormat := map[string]string{"image/png": "png", "image/jpeg": "jpeg"}[capture.MediaType]
	if err != nil || format != wantFormat || configuration.Width != capture.Width ||
		configuration.Height != capture.Height ||
		uint64(configuration.Width)*uint64(configuration.Height) > 32_000_000 {
		return 0, errors.New("video frame dimensions or encoded image configuration do not match")
	}
	_, decodedFormat, err := image.Decode(bytes.NewReader(capture.Data))
	if err != nil || decodedFormat != wantFormat {
		return 0, errors.New("video frame image payload is not fully decodable")
	}
	return episodeUS, nil
}

type sensitiveMatcher struct {
	values  []string
	needles [][]byte
}

func newSensitiveMatcher(values []string) (sensitiveMatcher, error) {
	canonical, err := canonicalSensitiveValues(values)
	if err != nil {
		return sensitiveMatcher{}, err
	}
	matcher := sensitiveMatcher{values: canonical}
	seenNeedles := make(map[string]struct{}, len(canonical)*6)
	add := func(payload []byte) {
		if len(payload) == 0 {
			return
		}
		key := string(payload)
		if _, duplicate := seenNeedles[key]; duplicate {
			return
		}
		seenNeedles[key] = struct{}{}
		matcher.needles = append(matcher.needles, slices.Clone(payload))
	}
	for _, value := range canonical {
		raw := []byte(value)
		add(raw)
		if escaped, marshalErr := json.Marshal(value); marshalErr == nil && len(escaped) >= 2 {
			add(escaped[1 : len(escaped)-1])
		}
		for _, encoding := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
		} {
			encoded := make([]byte, encoding.EncodedLen(len(raw)))
			encoding.Encode(encoded, raw)
			add(encoded)
		}
	}
	return matcher, nil
}

func canonicalSensitiveValues(values []string) ([]string, error) {
	if len(values) > maximumSensitiveValues {
		return nil, errors.New("attempt media sensitive value count exceeds the bounded limit")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	total := 0
	for _, value := range values {
		if value == "" || len(value) > maximumSensitiveValueBytes {
			return nil, errors.New("attempt media sensitive values violate the per-value byte limit")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		if len(value) > maximumSensitiveBytes-total {
			return nil, errors.New("attempt media sensitive values exceed the aggregate byte limit")
		}
		seen[value] = struct{}{}
		total += len(value)
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func (matcher sensitiveMatcher) contains(payload []byte) bool {
	for _, needle := range matcher.needles {
		if bytes.Contains(payload, needle) {
			return true
		}
	}
	return false
}

func guardStrings(sensitive sensitiveMatcher, values ...string) error {
	for _, value := range values {
		if sensitive.contains([]byte(value)) {
			return errors.New("attempt media public identity contains a sensitive value")
		}
	}
	return nil
}

func validateNewDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) != path {
		return "", errors.New("attempt media directory must be a non-empty clean path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil || absolute == filepath.Dir(absolute) {
		return "", errors.New("attempt media directory is invalid")
	}
	for current := filepath.Dir(absolute); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("attempt media parent must be a non-symlink directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return absolute, nil
}

func createAttemptRoot(directory string) (*os.Root, os.FileInfo, error) {
	parentPath, base := filepath.Dir(directory), filepath.Base(directory)
	before, err := os.Lstat(parentPath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, errors.New("attempt media parent must be a non-symlink directory")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, nil, errors.New("open attempt media parent")
	}
	defer parent.Close()
	opened, openErr := parent.Stat(".")
	visible, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, visible) {
		return nil, nil, errors.New("attempt media parent changed while it was opened")
	}
	if err := parent.Mkdir(base, 0o700); err != nil {
		return nil, nil, errors.New("create attempt media directory exclusively")
	}
	created := true
	defer func() {
		if created {
			_ = parent.RemoveAll(base)
			_ = syncDirectory(parent, ".")
		}
	}()
	if err := syncDirectory(parent, "."); err != nil {
		return nil, nil, err
	}
	// Open the final absolute name so Root.FS DirEntry.Info operations retain an
	// absolute anchor. Identity checks below still bind it to the parent entry.
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, errors.New("open created attempt media directory")
	}
	identity, identityErr := root.Stat(".")
	entry, entryErr := parent.Lstat(base)
	absolute, absoluteErr := os.Lstat(directory)
	parentAfter, parentAfterErr := os.Lstat(parentPath)
	if identityErr != nil || entryErr != nil || absoluteErr != nil || parentAfterErr != nil ||
		entry.Mode()&os.ModeSymlink != 0 || !entry.IsDir() || !os.SameFile(opened, parentAfter) ||
		!os.SameFile(identity, entry) || !os.SameFile(entry, absolute) {
		_ = root.Close()
		return nil, nil, errors.New("attempt media directory changed while it was created")
	}
	created = false
	return root, identity, nil
}

func removeOwnedAttempt(directory string, identity os.FileInfo) error {
	if identity == nil {
		return errors.New("attempt media directory identity is missing")
	}
	parentPath, base := filepath.Dir(directory), filepath.Base(directory)
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	entry, err := parent.Lstat(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.IsDir() || !os.SameFile(entry, identity) {
		return errors.New("owned attempt directory identity changed before cleanup")
	}
	_ = parent.Chmod(base, 0o700)
	if err := parent.RemoveAll(base); err != nil {
		return err
	}
	return syncDirectory(parent, ".")
}

func writeExclusive(root *os.Root, name string, payload []byte, mode os.FileMode) error {
	if root == nil || len(payload) == 0 || mode.Perm()&0o222 != 0 {
		return errors.New("exclusive artifact request is invalid")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = root.Remove(name)
		}
	}()
	created, err := file.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.New("exclusive artifact is not a regular file")
	}
	count, err := io.Copy(file, bytes.NewReader(payload))
	if err != nil || count != int64(len(payload)) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil || file.Chmod(mode) != nil || file.Sync() != nil {
		return errors.New("sync exclusive artifact")
	}
	afterWrite, err := file.Stat()
	if err != nil || !os.SameFile(created, afterWrite) || afterWrite.Size() != int64(len(payload)) {
		return errors.New("exclusive artifact changed while it was written")
	}
	if err := file.Close(); err != nil {
		return err
	}
	visible, err := root.Lstat(name)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.Mode().IsRegular() ||
		!os.SameFile(afterWrite, visible) || visible.Size() != int64(len(payload)) {
		return errors.New("exclusive artifact identity changed after close")
	}
	retained, retainedSHA, _, err := readRegular(root, name, int64(len(payload)))
	if err != nil || retainedSHA != digest(payload) || !bytes.Equal(retained, payload) {
		return errors.New("exclusive artifact bytes changed after close")
	}
	if err := syncDirectory(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
		return err
	}
	success = true
	return nil
}

func readRegular(root *os.Root, name string, maximum int64) ([]byte, string, string, error) {
	payload, sha, mediaType, _, err := readRegularIdentity(root, name, maximum)
	return payload, sha, mediaType, err
}

func readRegularIdentity(root *os.Root, name string, maximum int64) (
	[]byte, string, string, os.FileInfo, error,
) {
	if root == nil || maximum <= 0 {
		return nil, "", "", nil, errors.New("artifact reader is invalid")
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return nil, "", "", nil, errors.New("artifact is not a bounded regular non-symlink file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, "", "", nil, err
	}
	opened, openErr := file.Stat()
	afterOpen, afterOpenErr := root.Lstat(name)
	if openErr != nil || afterOpenErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!afterOpen.Mode().IsRegular() || !os.SameFile(before, opened) ||
		!os.SameFile(opened, afterOpen) || opened.Size() != before.Size() {
		_ = file.Close()
		return nil, "", "", nil, errors.New("artifact identity changed while it was opened")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) != before.Size() {
		_ = file.Close()
		return nil, "", "", nil, errors.New("artifact size changed while it was read")
	}
	afterRead, afterReadErr := file.Stat()
	visible, visibleErr := root.Lstat(name)
	if afterReadErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(opened, afterRead) ||
		!os.SameFile(afterRead, visible) || afterRead.Size() != before.Size() {
		_ = file.Close()
		return nil, "", "", nil, errors.New("artifact identity changed while it was read")
	}
	if err := file.Close(); err != nil {
		return nil, "", "", nil, errors.New("close verified artifact descriptor")
	}
	mediaType := http.DetectContentType(payload[:min(len(payload), 512)])
	if mediaType == "audio/wave" {
		mediaType = "audio/wav"
	}
	return payload, digest(payload), mediaType, afterRead, nil
}

func syncAndReadRegular(root *os.Root, name string, maximum int64) ([]byte, string, string, *os.File, error) {
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, "", "", nil, errors.New("output is not a regular non-symlink file")
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, "", "", nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, opened) || file.Sync() != nil {
		return nil, "", "", nil, errors.New("sync output artifact")
	}
	payload, sha, mediaType, err := readRegular(root, name, maximum)
	if err != nil {
		return nil, "", "", nil, err
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(opened, after) {
		return nil, "", "", nil, errors.New("output identity changed while it was synced")
	}
	if err := syncDirectory(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
		return nil, "", "", nil, err
	}
	success = true
	return payload, sha, mediaType, file, nil
}

func syncDirectory(root *os.Root, name string) error {
	if name == "" {
		name = "."
	}
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		_ = directory.Close()
		return errors.New("sync target is not a directory")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func regularEntryExists(root *os.Root, name string) (bool, error) {
	_, err := root.Lstat(name)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func removeUnexpectedManifest(root *os.Root) error {
	for _, name := range []string{manifestPath, pendingManifestPath, invalidManifestPath} {
		present, err := regularEntryExists(root, name)
		if err != nil {
			return errors.New("inspect unexpected completion-marker entry")
		}
		if present {
			if err := root.Remove(name); err != nil {
				return errors.New("remove unexpected completion-marker entry")
			}
		}
	}
	return syncDirectory(root, ".")
}

// invalidatePublishedManifestRoot removes the public completion name through
// the recorder's still-anchored directory handle. Rename is attempted first so
// a later cleanup failure cannot leave manifest.json looking reportable.
func invalidatePublishedManifestRoot(root *os.Root) error {
	if root == nil {
		return errors.New("published attempt root is missing")
	}
	directoryHandle, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := directoryHandle.Chmod(0o700); err != nil {
		_ = directoryHandle.Close()
		return errors.New("prepare published completion marker invalidation")
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return errors.New("prepare published completion marker invalidation")
	}
	if err := directoryHandle.Close(); err != nil {
		return errors.New("prepare published completion marker invalidation")
	}
	present, err := regularEntryExists(root, invalidManifestPath)
	if err != nil {
		return errors.New("inspect stale completion-marker quarantine")
	}
	if present {
		if err := root.Remove(invalidManifestPath); err != nil {
			// A stale quarantine name must not prevent removal of the nominal
			// completion marker below.
			present, inspectErr := regularEntryExists(root, manifestPath)
			if inspectErr != nil {
				return errors.New("inspect published completion marker")
			}
			if present {
				if removeErr := root.Remove(manifestPath); removeErr != nil {
					return errors.New("remove published completion marker")
				}
			}
			return errors.New("remove stale completion-marker quarantine")
		}
	}
	present, err = regularEntryExists(root, manifestPath)
	if err != nil {
		return errors.New("inspect published completion marker")
	}
	if present {
		if err := root.Rename(manifestPath, invalidManifestPath); err != nil {
			if removeErr := root.Remove(manifestPath); removeErr != nil {
				return errors.New("quarantine published completion marker")
			}
		}
	}
	present, err = regularEntryExists(root, manifestPath)
	if err != nil {
		return errors.New("inspect published completion marker after invalidation")
	}
	if present {
		return errors.New("published completion marker remains after invalidation")
	}
	var cleanupErr error
	for _, name := range []string{pendingManifestPath, invalidManifestPath} {
		present, inspectErr := regularEntryExists(root, name)
		if inspectErr != nil {
			cleanupErr = joinCanonical(cleanupErr, errors.New("inspect completion-marker quarantine"))
			continue
		}
		if present {
			if err := root.Remove(name); err != nil {
				cleanupErr = joinCanonical(cleanupErr, errors.New("remove completion-marker quarantine"))
			}
		}
	}
	if err := syncDirectory(root, "."); err != nil {
		cleanupErr = joinCanonical(cleanupErr, errors.New("sync completion-marker invalidation"))
	}
	return cleanupErr
}

func hardenBundleBeforeCommit(root *os.Root) error {
	var directories []string
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("attempt evidence contains a symlink")
		}
		if entry.IsDir() {
			if path != "." {
				directories = append(directories, path)
			}
			return nil
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || file.Chmod(0o400) != nil || file.Sync() != nil {
			_ = file.Close()
			return errors.New("make attempt evidence file read-only")
		}
		return file.Close()
	})
	if err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return strings.Count(directories[i], "/") > strings.Count(directories[j], "/") })
	for _, path := range directories {
		directory, err := root.Open(path)
		if err != nil {
			return err
		}
		if err := directory.Chmod(0o500); err != nil || directory.Sync() != nil || directory.Close() != nil {
			_ = directory.Close()
			return errors.New("make attempt evidence directory read-only")
		}
	}
	return syncDirectory(root, ".")
}

func publishManifest(root *os.Root, payload []byte, expectedSHA string) error {
	if present, err := regularEntryExists(root, manifestPath); err != nil || present {
		return errors.New("completion marker already exists")
	}
	if err := writeExclusive(root, pendingManifestPath, payload, 0o400); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = root.Remove(pendingManifestPath)
			_ = root.Remove(manifestPath)
			_ = syncDirectory(root, ".")
		}
	}()
	if err := root.Rename(pendingManifestPath, manifestPath); err != nil {
		return err
	}
	retained, sha, _, err := readRegular(root, manifestPath, maximumManifestBytes)
	if err != nil || sha != expectedSHA || !bytes.Equal(retained, payload) {
		return errors.New("completion marker changed during publication")
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := directory.Chmod(0o500); err != nil || directory.Sync() != nil {
		_ = directory.Chmod(0o700)
		_ = directory.Close()
		return errors.New("make completed attempt directory read-only")
	}
	if err := directory.Close(); err != nil {
		_ = root.Chmod(".", 0o700)
		return errors.New("close completed attempt directory descriptor")
	}
	published = true
	return nil
}

func invalidatePublishedManifest(directory string, expectedIdentity os.FileInfo) (returnErr error) {
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		expectedIdentity == nil || !os.SameFile(visible, expectedIdentity) {
		return errors.New("published attempt directory identity is invalid")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); err != nil {
			returnErr = joinCanonical(returnErr, errors.New("close published attempt root after invalidation"))
		}
	}()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(visible, opened) || !os.SameFile(opened, expectedIdentity) {
		return errors.New("published attempt directory changed before invalidation")
	}
	visibleAfter, err := os.Lstat(directory)
	if err != nil || visibleAfter.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, visibleAfter) {
		return errors.New("published attempt directory changed during invalidation")
	}
	return invalidatePublishedManifestRoot(root)
}

// completionMarkerReceipt identifies a residual marker through the recorder's
// still-anchored root without treating it as verified. err remains authoritative
// for callers.
func completionMarkerReceipt(root *os.Root) CompletionReceipt {
	_, sha, _, err := readRegular(root, manifestPath, maximumManifestBytes)
	if err != nil {
		return CompletionReceipt{}
	}
	return CompletionReceipt{ManifestSHA256: sha}
}

func makeTreeOwnerWritable(rootPath string) error {
	return filepath.WalkDir(rootPath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return os.Chmod(path, 0o700)
		}
		if info.Mode().IsRegular() {
			return os.Chmod(path, 0o600)
		}
		return nil
	})
}

func prepareReviewMedia(ctx context.Context, directory string, media []review.Media,
	sensitive sensitiveMatcher) ([]review.PreparedMedia, error) {
	prepared, err := review.PrepareContext(ctx, review.Request{
		AttemptID: "capture_validation/attempt/1", Suite: "capture_validation",
		Case: "attempt_media", Trial: 1, RootDirectory: directory,
		Context: []byte(`{"capture_complete":true,"deterministic_pass":true}`), Media: media,
		SensitiveValues: slices.Clone(sensitive.values),
	})
	if err != nil {
		return nil, err
	}
	return prepared.Media, nil
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}
