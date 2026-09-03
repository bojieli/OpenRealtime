package fdbv3

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	pinnedReleasedRevision = "3e799c45a045256f47d5f1c9cda90157e2d2ec9e"
	releasedRootMetadata   = ".DS_Store"
	metadataArtifactName   = "metadata.json"
	audioArtifactName      = "input.wav"

	// The pinned release currently tops out at 1,622 metadata bytes and
	// 19,898,412 audio bytes. These conservative ceilings leave ample room for
	// fixtures while keeping both the stat check and the retained read bounded.
	maximumMetadataArtifactBytes int64 = 1 << 20
	maximumAudioArtifactBytes    int64 = 64 << 20

	// The digest is SHA-256 over one sorted line per released directory:
	//
	//   <directory>\t<metadata sha256>\t<input.wav sha256>\n
	//
	// It covers exactly the two task artifacts admitted from every directory.
	// The separately pinned archive digest in datasets/manifests also covers
	// the harmless root .DS_Store that is intentionally excluded here.
	pinnedReleasedArtifactDigest = "6c2b33e5c951432836da26109f723759a11f037c4ab76bfdcabf0c4db60b6cb1"
)

type releasedDatasetInventory struct {
	Revision       string
	ArtifactDigest string
	TaskNames      []string
}

var pinnedReleasedInventory = releasedDatasetInventory{
	Revision:       pinnedReleasedRevision,
	ArtifactDigest: pinnedReleasedArtifactDigest,
	TaskNames: strings.Fields(`
ecommerce_01_65e8cf8f4c7424fa062e54a3
ecommerce_01_69a9cf80f4d7668d5c815038
ecommerce_02_5f4a4da1575d605c43bef871
ecommerce_04_6998abd731d2ec50d067d5bd
ecommerce_05_695bd157114f0d2317f88617
ecommerce_06_66c4f3cb14cbfc4db836bd4e
ecommerce_07_66f59c766e7e22e1f90d08f6
ecommerce_08_5ff07b5ee7a1d23e719e421e
ecommerce_08_61517db6a7589569521b2356
ecommerce_09_695bd157114f0d2317f88617
ecommerce_10_6998abd731d2ec50d067d5bd
ecommerce_11_62a885d5b6af18b3d4579e1b
ecommerce_12_6998abd731d2ec50d067d5bd
ecommerce_13_5ff07b5ee7a1d23e719e421e
ecommerce_13_61517db6a7589569521b2356
ecommerce_14_5ff07b5ee7a1d23e719e421e
ecommerce_14_61517db6a7589569521b2356
ecommerce_15_56780a5a89319e0011644cb4
ecommerce_15_66c4f3cb14cbfc4db836bd4e
ecommerce_16_695bd157114f0d2317f88617
ecommerce_17_5f4a4da1575d605c43bef871
ecommerce_18_62a885d5b6af18b3d4579e1b
ecommerce_19_66f59c766e7e22e1f90d08f6
ecommerce_20_66c4f3cb14cbfc4db836bd4e
ecommerce_21_65e8cf8f4c7424fa062e54a3
ecommerce_21_69a9cf80f4d7668d5c815038
ecommerce_23_5f4a4da1575d605c43bef871
ecommerce_25_65e8cf8f4c7424fa062e54a3
ecommerce_25_69a9cf80f4d7668d5c815038
finance_01_65e8cf8f4c7424fa062e54a3
finance_01_69a9cf80f4d7668d5c815038
finance_02_6998abd731d2ec50d067d5bd
finance_03_5ff07b5ee7a1d23e719e421e
finance_03_61517db6a7589569521b2356
finance_04_62a885d5b6af18b3d4579e1b
finance_06_66f59c766e7e22e1f90d08f6
finance_07_5ff07b5ee7a1d23e719e421e
finance_07_61517db6a7589569521b2356
finance_08_65e8cf8f4c7424fa062e54a3
finance_08_69a9cf80f4d7668d5c815038
finance_10_66c4f3cb14cbfc4db836bd4e
finance_11_695bd157114f0d2317f88617
finance_12_65e8cf8f4c7424fa062e54a3
finance_12_69a9cf80f4d7668d5c815038
finance_14_5f4a4da1575d605c43bef871
finance_15_66c4f3cb14cbfc4db836bd4e
finance_16_62a885d5b6af18b3d4579e1b
finance_17_6998abd731d2ec50d067d5bd
finance_18_6998abd731d2ec50d067d5bd
finance_19_66f59c766e7e22e1f90d08f6
finance_20_66c4f3cb14cbfc4db836bd4e
finance_21_5e3c1fbece3a7b000a6fd95a
finance_22_5f4a4da1575d605c43bef871
finance_23_66f59c766e7e22e1f90d08f6
housing_01_62a885d5b6af18b3d4579e1b
housing_02_5f4a4da1575d605c43bef871
housing_03_5f4a4da1575d605c43bef871
housing_04_695bd157114f0d2317f88617
housing_05_5ff07b5ee7a1d23e719e421e
housing_05_61517db6a7589569521b2356
housing_06_5f4a4da1575d605c43bef871
housing_08_66f59c766e7e22e1f90d08f6
housing_09_695bd157114f0d2317f88617
housing_10_66f59c766e7e22e1f90d08f6
housing_11_62a885d5b6af18b3d4579e1b
housing_13_5f4a4da1575d605c43bef871
housing_14_5ff07b5ee7a1d23e719e421e
housing_14_61517db6a7589569521b2356
housing_15_5ff07b5ee7a1d23e719e421e
housing_15_61517db6a7589569521b2356
housing_17_65e8cf8f4c7424fa062e54a3
housing_17_69a9cf80f4d7668d5c815038
housing_18_66c4f3cb14cbfc4db836bd4e
housing_19_62a885d5b6af18b3d4579e1b
housing_20_62a885d5b6af18b3d4579e1b
housing_21_66c4f3cb14cbfc4db836bd4e
housing_22_695bd157114f0d2317f88617
housing_24_65e8cf8f4c7424fa062e54a3
housing_24_69a9cf80f4d7668d5c815038
housing_25_66f59c766e7e22e1f90d08f6
travel_01_62a885d5b6af18b3d4579e1b
travel_02_5e3c1fbece3a7b000a6fd95a
travel_03_5ff07b5ee7a1d23e719e421e
travel_03_61517db6a7589569521b2356
travel_05_6998abd731d2ec50d067d5bd
travel_07_5ff07b5ee7a1d23e719e421e
travel_07_61517db6a7589569521b2356
travel_08_695bd157114f0d2317f88617
travel_10_5f4a4da1575d605c43bef871
travel_11_66c4f3cb14cbfc4db836bd4e
travel_14_66c4f3cb14cbfc4db836bd4e
travel_16_66f59c766e7e22e1f90d08f6
travel_18_66f59c766e7e22e1f90d08f6
travel_19_695bd157114f0d2317f88617
travel_20_62a885d5b6af18b3d4579e1b
travel_21_65e8cf8f4c7424fa062e54a3
travel_21_69a9cf80f4d7668d5c815038
travel_23_65e8cf8f4c7424fa062e54a3
travel_23_69a9cf80f4d7668d5c815038
travel_24_695bd157114f0d2317f88617
`),
}

// Load reads and validates every task directory it sees before applying a
// diagnostic limit. It is intentionally usable for small catalog fixtures,
// tolerates unrelated task-local sidecars, and leaves each task path-backed;
// its mutable playback cannot support release evidence. Named task artifacts
// remain regular-file and size checked. Benchmark execution additionally
// supplies the immutable released inventory.
func Load(root string, limit int) ([]Task, error) {
	return loadDataset(root, limit, nil)
}

// LoadReleased validates and loads the complete immutable 100-recording
// release. Production benchmark/profile preparation should use this entry
// point; Load remains available for small catalog fixtures and unit tests.
func LoadReleased(root string) ([]Task, error) {
	return loadDataset(root, 0, &pinnedReleasedInventory)
}

func loadDataset(root string, limit int, inventory *releasedDatasetInventory) ([]Task, error) {
	return loadDatasetWithTestHooks(root, limit, inventory, datasetLoadTestHooks{})
}

type datasetLoadTestHooks struct {
	beforeFinalReleasedRootCheck func() error
}

func loadDatasetWithTestHooks(
	root string, limit int, inventory *releasedDatasetInventory, hooks datasetLoadTestHooks,
) ([]Task, error) {
	if limit < 0 {
		return nil, errors.New("FDB v3 task limit cannot be negative")
	}
	var (
		entries          []os.DirEntry
		releasedRootInfo os.FileInfo
		err              error
	)
	if inventory != nil {
		entries, releasedRootInfo, err = readStableDirectory(root, nil)
	} else {
		entries, err = os.ReadDir(root)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var names []string
	if inventory != nil {
		if err := inventory.validate(); err != nil {
			return nil, err
		}
		names, err = releasedRootTaskNames(entries, inventory.TaskNames)
		if err != nil {
			return nil, err
		}
		if err := compareReleasedTaskNames(names, inventory.TaskNames); err != nil {
			return nil, err
		}
	} else {
		names = make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), "__") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
	}

	var artifactDigest hash.Hash
	if inventory != nil {
		artifactDigest = sha256.New()
	}
	var releasedTaskDirectories map[string]os.FileInfo
	if inventory != nil {
		releasedTaskDirectories = make(map[string]os.FileInfo, len(names))
	}
	tasks := make([]Task, 0, len(names))
	for _, name := range names {
		directory := filepath.Join(root, name)
		var releasedDirectoryInfo os.FileInfo
		if inventory != nil {
			releasedDirectoryInfo, err = validateReleasedTaskArtifacts(directory, nil)
			if err != nil {
				return nil, fmt.Errorf("validate FDB v3 released task %q artifacts: %w", name, err)
			}
		}
		metadataPath := filepath.Join(directory, metadataArtifactName)
		payload, metadataSHA256, err := readRegularArtifact(
			metadataPath, true, maximumMetadataArtifactBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("read FDB v3 task %q metadata: %w", name, err)
		}
		if err := strictjson.Validate(payload); err != nil {
			return nil, fmt.Errorf("validate FDB v3 task %q metadata: %w", name, err)
		}
		var task Task
		if err := json.Unmarshal(payload, &task); err != nil {
			return nil, fmt.Errorf("decode FDB v3 task %q metadata: %w", name, err)
		}
		if strings.TrimSpace(task.ID) == "" {
			return nil, fmt.Errorf("FDB v3 task directory %q has no metadata identity", name)
		}
		if inventory != nil && inventory.Revision == pinnedReleasedRevision && !strings.HasPrefix(name, task.ID+"_") {
			return nil, fmt.Errorf("FDB v3 task directory %q does not carry metadata identity %q", name, task.ID)
		}
		if len(task.Expected) == 0 {
			return nil, fmt.Errorf("FDB v3 task %q contains no expected tool calls", name)
		}

		audioPath := filepath.Join(directory, audioArtifactName)
		audioWAV, audioSHA256, err := readRegularArtifact(
			audioPath, inventory != nil, maximumAudioArtifactBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("read FDB v3 task %q audio: %w", name, err)
		}
		if inventory != nil {
			releasedDirectoryInfo, err = validateReleasedTaskArtifacts(directory, releasedDirectoryInfo)
			if err != nil {
				return nil, fmt.Errorf("revalidate FDB v3 released task %q artifacts: %w", name, err)
			}
			releasedTaskDirectories[name] = releasedDirectoryInfo
		}
		if artifactDigest != nil {
			_, _ = fmt.Fprintf(artifactDigest, "%s\t%s\t%s\n", name, metadataSHA256, audioSHA256)
		}

		// The released directory identity distinguishes recordings of one
		// scenario by different speakers; the metadata id alone does not.
		task.ID = name
		task.Directory = directory
		if inventory != nil {
			task.AudioWAV = audioWAV
		} else {
			task.AudioPath = audioPath
		}
		tasks = append(tasks, task)
	}
	if inventory != nil {
		if hooks.beforeFinalReleasedRootCheck != nil {
			if err := hooks.beforeFinalReleasedRootCheck(); err != nil {
				return nil, fmt.Errorf("prepare final FDB v3 released root check: %w", err)
			}
		}
		finalEntries, _, err := readStableDirectory(root, releasedRootInfo)
		if err != nil {
			return nil, fmt.Errorf("revalidate FDB v3 released root: %w", err)
		}
		finalNames, err := releasedRootTaskNames(finalEntries, inventory.TaskNames)
		if err != nil {
			return nil, err
		}
		if err := compareReleasedTaskNames(finalNames, inventory.TaskNames); err != nil {
			return nil, err
		}
		if err := compareReleasedTaskDirectoryIdentities(
			finalEntries, releasedTaskDirectories,
		); err != nil {
			return nil, err
		}
	}
	if artifactDigest != nil {
		got := hex.EncodeToString(artifactDigest.Sum(nil))
		if got != inventory.ArtifactDigest {
			return nil, fmt.Errorf(
				"FDB v3 released artifact digest is %s, want %s at revision %s",
				got, inventory.ArtifactDigest, inventory.Revision,
			)
		}
	}
	if limit > 0 && limit < len(tasks) {
		return tasks[:limit], nil
	}
	return tasks, nil
}

// validateReleasedTaskArtifacts rejects every task-local entry except the two
// regular artifacts covered by pinnedReleasedArtifactDigest. Diagnostic loads
// deliberately do not call it so fixtures may retain unrelated sidecars. A
// strict load calls it before and after reading, carrying the directory
// identity across both enumerations.
func validateReleasedTaskArtifacts(
	directory string, expectedDirectory os.FileInfo,
) (os.FileInfo, error) {
	entries, directoryInfo, err := readStableDirectory(directory, expectedDirectory)
	if err != nil {
		return nil, err
	}

	required := map[string]bool{
		metadataArtifactName: false,
		audioArtifactName:    false,
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, allowed := required[name]; !allowed {
			return nil, fmt.Errorf("unexpected task entry %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect task artifact %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("task artifact %q is not a regular file", name)
		}
		required[name] = true
	}
	for _, name := range []string{metadataArtifactName, audioArtifactName} {
		if !required[name] {
			return nil, fmt.Errorf("required task artifact %q is missing", name)
		}
	}
	return directoryInfo, nil
}

func readStableDirectory(
	directory string, expectedDirectory os.FileInfo,
) ([]os.DirEntry, os.FileInfo, error) {
	before, err := os.Lstat(directory)
	if err != nil {
		return nil, nil, err
	}
	if !before.IsDir() {
		return nil, nil, errors.New("path is not a directory")
	}
	if expectedDirectory != nil && !os.SameFile(expectedDirectory, before) {
		return nil, nil, errors.New("directory identity changed between checks")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil, err
	}
	after, err := os.Lstat(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("reinspect directory: %w", err)
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		return nil, nil, errors.New("directory identity changed while it was enumerated")
	}
	return entries, after, nil
}

// releasedRootTaskNames enforces the complete released-root allowlist. The
// upstream archive contains one harmless Finder metadata file; no other
// non-task entry is part of the pinned release.
func releasedRootTaskNames(entries []os.DirEntry, want []string) ([]string, error) {
	expected := make(map[string]struct{}, len(want))
	for _, name := range want {
		expected[name] = struct{}{}
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == releasedRootMetadata {
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("inspect FDB v3 released root entry %q: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("FDB v3 released root entry %q is not a regular file", name)
			}
			continue
		}

		names = append(names, name)
		if _, found := expected[name]; found && !entry.IsDir() {
			return nil, fmt.Errorf("FDB v3 released inventory task %q is not a directory", name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func compareReleasedTaskDirectoryIdentities(
	entries []os.DirEntry, expected map[string]os.FileInfo,
) error {
	for _, entry := range entries {
		want, found := expected[entry.Name()]
		if !found {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("reinspect FDB v3 released task %q: %w", entry.Name(), err)
		}
		if !info.IsDir() || !os.SameFile(want, info) {
			return fmt.Errorf(
				"FDB v3 released task directory %q identity changed after its artifacts were read",
				entry.Name(),
			)
		}
	}
	return nil
}

func (inventory releasedDatasetInventory) validate() error {
	if strings.TrimSpace(inventory.Revision) == "" {
		return errors.New("FDB v3 released inventory has no revision")
	}
	if len(inventory.ArtifactDigest) != sha256.Size*2 {
		return errors.New("FDB v3 released inventory has an invalid artifact digest")
	}
	if _, err := hex.DecodeString(inventory.ArtifactDigest); err != nil {
		return errors.New("FDB v3 released inventory has an invalid artifact digest")
	}
	if len(inventory.TaskNames) == 0 {
		return errors.New("FDB v3 released inventory contains no task identities")
	}
	for index, name := range inventory.TaskNames {
		if name == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("FDB v3 released inventory task %d is not canonical", index)
		}
		if index > 0 && inventory.TaskNames[index-1] >= name {
			return fmt.Errorf("FDB v3 released inventory is not strictly ordered at task %q", name)
		}
	}
	return nil
}

func compareReleasedTaskNames(got, want []string) error {
	if len(got) == len(want) {
		match := true
		for index := range want {
			match = match && got[index] == want[index]
		}
		if match {
			return nil
		}
	}
	present := make(map[string]struct{}, len(got))
	for _, name := range got {
		present[name] = struct{}{}
	}
	expected := make(map[string]struct{}, len(want))
	for _, name := range want {
		expected[name] = struct{}{}
	}
	var missing, unexpected []string
	for _, name := range want {
		if _, found := present[name]; !found {
			missing = append(missing, name)
		}
	}
	for _, name := range got {
		if _, found := expected[name]; !found {
			unexpected = append(unexpected, name)
		}
	}
	return fmt.Errorf(
		"FDB v3 released inventory differs: found %d tasks, want %d; missing=%v unexpected=%v",
		len(got), len(want), missing, unexpected,
	)
}

func releasedArtifactIdentity(root string) ([]string, string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, "", err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), "__") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		directory := filepath.Join(root, name)
		_, metadataDigest, err := readRegularArtifact(
			filepath.Join(directory, metadataArtifactName), true, maximumMetadataArtifactBytes,
		)
		if err != nil {
			return nil, "", err
		}
		_, audioDigest, err := readRegularArtifact(
			filepath.Join(directory, audioArtifactName), true, maximumAudioArtifactBytes,
		)
		if err != nil {
			return nil, "", err
		}
		_, _ = fmt.Fprintf(digest, "%s\t%s\t%s\n", name, metadataDigest, audioDigest)
	}
	return names, hex.EncodeToString(digest.Sum(nil)), nil
}

// readRegularArtifact bounds every artifact from its opened-file stat before
// reading. Retained artifacts are then read through a second hard bound and
// must contain exactly the number of bytes reported by that stat. Even
// catalog-only loads open the audio file and reject directories, empty
// placeholders, unreadable paths, and path replacement during the read.
func readRegularArtifact(path string, retain bool, maximumBytes int64) ([]byte, string, error) {
	if maximumBytes <= 0 {
		return nil, "", errors.New("artifact size limit must be positive")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, "", errors.New("artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", errors.New("artifact is not a regular file")
	}
	if !os.SameFile(pathInfo, info) {
		return nil, "", errors.New("artifact identity changed while it was opened")
	}
	if info.Size() == 0 {
		return nil, "", errors.New("artifact is empty")
	}
	if info.Size() < 0 || info.Size() > maximumBytes {
		return nil, "", fmt.Errorf(
			"artifact size %d exceeds %d-byte limit", info.Size(), maximumBytes,
		)
	}

	if retain {
		payload, digest, err := readRetainedArtifact(file, info.Size(), maximumBytes)
		if err != nil {
			return nil, "", err
		}
		if err := verifyArtifactIdentity(path, info); err != nil {
			return nil, "", err
		}
		return payload, digest, nil
	}
	// A one-byte read proves that an existing regular audio path is readable
	// without loading a large recording for catalog-only callers.
	var probe [1]byte
	if _, err := io.ReadFull(file, probe[:]); err != nil {
		return nil, "", err
	}
	if err := verifyArtifactIdentity(path, info); err != nil {
		return nil, "", err
	}
	return nil, "", nil
}

func readRetainedArtifact(
	reader io.Reader, openedSize, maximumBytes int64,
) ([]byte, string, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maximumBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(payload)) > maximumBytes {
		return nil, "", fmt.Errorf("artifact exceeds %d-byte limit while being read", maximumBytes)
	}
	if int64(len(payload)) != openedSize {
		return nil, "", fmt.Errorf(
			"artifact retained byte count %d differs from opened-file stat %d",
			len(payload), openedSize,
		)
	}
	digest := sha256.Sum256(payload)
	return payload, hex.EncodeToString(digest[:]), nil
}

func verifyArtifactIdentity(path string, opened os.FileInfo) error {
	after, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("reinspect artifact after read: %w", err)
	}
	if !after.Mode().IsRegular() {
		return errors.New("artifact replacement is not a regular file")
	}
	if !os.SameFile(opened, after) {
		return errors.New("artifact identity changed while it was read")
	}
	return nil
}
