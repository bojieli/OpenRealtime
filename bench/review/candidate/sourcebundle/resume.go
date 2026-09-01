package sourcebundle

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

// Resume reopens an interrupted, unsealed current-run source bundle. Only an
// attempt with a canonical entry.json commit marker written after every other
// file is recoverable. A directory without that marker is preserved under
// interruptions/ and its identity becomes available for a clean retry.
//
// A final manifest or external receipt is deliberately not a partial source:
// callers must use Verify/review-candidate for a sealed campaign. This keeps
// deterministic execution recovery separate from advisory-review recovery.
func Resume(ctx context.Context, options Options) (*Bundle, error) {
	if ctx == nil {
		return nil, errors.New("resume candidate source bundle: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, receipt, guard, err := validateBundleOptions(options)
	if err != nil {
		return nil, err
	}
	lease, err := acquireBundleLease(ctx, directory)
	if err != nil {
		return nil, err
	}
	root, identity, err := openBundleRoot(directory)
	if err != nil {
		return nil, errors.Join(err, closeBundleLease(lease))
	}
	bundle := &Bundle{
		directory: directory, receipt: receipt, root: root, lease: lease, identity: identity, guard: guard,
		attempts: make(map[string]*attemptState), recovered: make(map[string]candidate.Completion),
		claimed: make(map[string]struct{}),
	}
	if err := loadPartialBundle(ctx, bundle); err != nil {
		closeErr := errors.Join(root.Close(), closeBundleLease(lease))
		bundle.root = nil
		bundle.lease = nil
		return nil, errors.Join(err, closeErr)
	}
	return bundle, nil
}

func loadPartialBundle(ctx context.Context, bundle *Bundle) error {
	if err := verifyBundleRoot(bundle.root, bundle.directory, bundle.identity); err != nil {
		return err
	}
	top, err := fs.ReadDir(bundle.root.FS(), ".")
	if err != nil {
		return errors.New("read candidate source partial root")
	}
	foundAttempts := false
	for _, entry := range top {
		info, infoErr := entry.Info()
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("candidate source partial root contains an invalid entry")
		}
		switch entry.Name() {
		case "attempts":
			if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				return errors.New("candidate source partial attempts entry is not a directory")
			}
			foundAttempts = true
		case "interruptions":
			if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				return errors.New("candidate source interruption entry is not a directory")
			}
		case manifestName, resultName, reviewName:
			return errors.New("candidate source partial tree contains an unfinished suite seal")
		default:
			return errors.New("candidate source partial root contains an undeclared entry")
		}
	}
	if !foundAttempts {
		return errors.New("candidate source partial tree has no attempts directory")
	}
	if err := validateInterruptionDirectories(bundle.root); err != nil {
		return err
	}
	files, err := walkSourceFilesGuarded(bundle.root, bundle.guard)
	if err != nil {
		return err
	}
	fileIndex := make(map[string]SourceFile, len(files))
	for _, file := range files {
		fileIndex[file.Path] = file
	}
	attemptDirectories, err := fs.ReadDir(bundle.root.FS(), "attempts")
	if err != nil {
		return errors.New("read candidate source partial attempts")
	}
	if len(attemptDirectories) > maximumSourceFiles {
		return errors.New("candidate source partial attempt population is oversized")
	}
	for _, visible := range attemptDirectories {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, infoErr := visible.Info()
		name := visible.Name()
		_, nameErr := hex.DecodeString(name)
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
			info.Mode().Perm()&0o077 != 0 ||
			len(name) != 64 || nameErr != nil || strings.ToLower(name) != name {
			return errors.New("candidate source partial attempt directory is invalid")
		}
		markerPath := filepath.ToSlash(filepath.Join("attempts", name, attemptEntryName))
		marker, committed := fileIndex[markerPath]
		if !committed {
			if _, err := archiveInterruptedAttempt(bundle.root, name); err != nil {
				return err
			}
			continue
		}
		payload, err := readRegular(bundle.root, marker.Path, marker.SizeBytes)
		if err != nil || digest(payload) != marker.SHA256 {
			return errors.New("candidate source attempt commit marker changed")
		}
		var entry AttemptEntry
		if err := decodeCanonical(payload, &entry); err != nil {
			return errors.New("candidate source attempt commit marker is invalid")
		}
		attempt, err := validateRecoveredAttemptHeader(bundle.root, fileIndex, name, entry)
		if err != nil {
			return err
		}
		if !entry.EvidenceComplete {
			if _, err := archiveInterruptedAttempt(bundle.root, name); err != nil {
				return err
			}
			continue
		}
		completion, err := validateRecoveredCompletion(
			ctx, bundle.root, bundle.directory, fileIndex, entry, attempt,
		)
		if err != nil {
			return err
		}
		if bundle.suite == "" {
			bundle.suite, bundle.cell = attempt.Suite, attempt.Cell
			bundle.provenance, bundle.origin = attempt.Provenance, attempt.Origin
		} else if attempt.Suite != bundle.suite || !reflect.DeepEqual(attempt.Cell, bundle.cell) ||
			!matchingRunProvenance(attempt.Provenance, bundle.provenance) || attempt.Origin != bundle.origin {
			return errors.New("candidate source recovered attempts have different run identities")
		}
		artifacts := make(map[string]struct{}, len(entry.Artifacts))
		for _, artifact := range entry.Artifacts {
			artifacts[artifact.Name] = struct{}{}
		}
		state := &attemptState{
			bundle: bundle, spec: attempt, directory: entry.Directory,
			absolute: filepath.Join(bundle.directory, filepath.FromSlash(entry.Directory)),
			entry:    cloneEntry(entry), media: cloneMedia(entry.Media), artifacts: artifacts,
			terminal: true, released: true,
		}
		bundle.attempts[entry.AttemptID] = state
		bundle.recovered[entry.AttemptID] = completion
	}
	if _, err := walkSourceFilesGuarded(bundle.root, bundle.guard); err != nil {
		return err
	}
	return verifyBundleRoot(bundle.root, bundle.directory, bundle.identity)
}

func validateInterruptionDirectories(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), "interruptions")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(entries) > maximumSourceFiles {
		return errors.New("read candidate source interruption population")
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		name := entry.Name()
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
			info.Mode().Perm()&0o077 != 0 || len(name) != 71 || name[64] != '-' {
			return errors.New("candidate source interruption namespace is invalid")
		}
		if _, err := hex.DecodeString(name[:64]); err != nil || strings.ToLower(name[:64]) != name[:64] {
			return errors.New("candidate source interruption namespace is invalid")
		}
		sequence := name[65:]
		if sequence == "000000" || sequence > sixDigitSequence(maximumSourceFiles) ||
			strings.IndexFunc(sequence, func(symbol rune) bool {
				return symbol < '0' || symbol > '9'
			}) >= 0 {
			return errors.New("candidate source interruption namespace is invalid")
		}
	}
	return nil
}

func validateRecoveredAttemptHeader(
	root *os.Root, files map[string]SourceFile, directoryName string, entry AttemptEntry,
) (candidate.Attempt, error) {
	wantDirectory := filepath.ToSlash(filepath.Join("attempts", directoryName))
	if entry.AttemptID == "" || digestName(entry.AttemptID) != directoryName ||
		entry.Directory != wantDirectory || entry.Suite == "" || entry.Case == "" || entry.Trial <= 0 ||
		entry.AttemptPath != filepath.ToSlash(filepath.Join(wantDirectory, "attempt.json")) ||
		!validDigest(entry.AttemptSHA256) || entry.Terminal != "completed" ||
		(entry.MediaSource != candidate.MediaSharedSession &&
			entry.MediaSource != candidate.MediaExternalHarness) {
		return candidate.Attempt{}, errors.New("candidate source recovered attempt header is invalid")
	}
	attempt, err := verifyAttemptSpecification(root, entry, files)
	if err != nil {
		return candidate.Attempt{}, err
	}
	if attempt.ID() != entry.AttemptID || attempt.Suite != entry.Suite ||
		attempt.Case != entry.Case || attempt.Trial != entry.Trial ||
		attempt.MediaSource != entry.MediaSource {
		return candidate.Attempt{}, errors.New("candidate source recovered attempt differs from its marker")
	}
	return attempt, nil
}

func validateRecoveredCompletion(
	ctx context.Context, root *os.Root, sourceDirectory string,
	files map[string]SourceFile, entry AttemptEntry, attempt candidate.Attempt,
) (candidate.Completion, error) {
	if entry.Media == nil || entry.CompletionPath != "completion.json" ||
		entry.ContextPath != "review-context.json" ||
		!validDigest(entry.CompletionSHA256) || !validDigest(entry.ContextSHA256) {
		return candidate.Completion{}, errors.New("candidate source recovered completion is incomplete")
	}
	wantMedia := "audio.stereo.wav"
	if entry.MediaSource == candidate.MediaExternalHarness {
		wantMedia = "audio.external.wav"
	}
	if entry.Media.Path != wantMedia || entry.Media.Kind != "audio" ||
		entry.Media.MediaType != "audio/wav" || entry.Media.SizeBytes <= 0 ||
		!validDigest(entry.Media.SHA256) {
		return candidate.Completion{}, errors.New("candidate source recovered media identity is invalid")
	}
	if entry.MediaSource == candidate.MediaSharedSession &&
		entry.Media.Role != "time_aligned_room_and_agent" {
		return candidate.Completion{}, errors.New("candidate source recovered shared media role is invalid")
	}
	completion, contextPayload, err := verifyAttemptCompletion(ctx, root, entry, files)
	if err != nil {
		return candidate.Completion{}, err
	}
	if !reflect.DeepEqual(completion.Attempt, attempt) ||
		!reflect.DeepEqual(completion.Outcome, entry.Deterministic) {
		return candidate.Completion{}, errors.New("candidate source recovered completion differs from its marker")
	}
	requestMedia := *entry.Media
	requestMedia.Validation = ""
	requestMedia.SizeBytes = 0
	prepared, err := review.PrepareContext(ctx, review.Request{
		AttemptID: entry.AttemptID, Suite: entry.Suite, Case: entry.Case, Trial: entry.Trial,
		RootDirectory: filepath.Join(sourceDirectory, filepath.FromSlash(entry.Directory)),
		Context:       contextPayload, Media: []review.Media{requestMedia},
	})
	if err != nil || len(prepared.Media) != 1 ||
		!reflect.DeepEqual(prepared.Media[0].Media, *entry.Media) {
		return candidate.Completion{}, errors.New("candidate source recovered review request is invalid")
	}
	for _, artifact := range entry.Artifacts {
		if !validArtifactIdentity(candidate.CapturedArtifact{
			Name: artifact.Name, Kind: artifact.Kind, Role: artifact.Role,
			ContentType: artifact.ContentType, Bytes: []byte{1},
		}) {
			return candidate.Completion{}, errors.New("candidate source recovered artifact identity is invalid")
		}
	}
	if err := verifyArtifacts(root, entry, files); err != nil {
		return candidate.Completion{}, err
	}
	expected := map[string]struct{}{
		filepath.ToSlash(filepath.Join(entry.Directory, attemptEntryName)): {},
		entry.AttemptPath: {},
		filepath.ToSlash(filepath.Join(entry.Directory, entry.CompletionPath)): {},
		filepath.ToSlash(filepath.Join(entry.Directory, entry.ContextPath)):    {},
		filepath.ToSlash(filepath.Join(entry.Directory, entry.Media.Path)):     {},
	}
	for _, artifact := range entry.Artifacts {
		expected[filepath.ToSlash(filepath.Join(entry.Directory, artifact.Path))] = struct{}{}
	}
	prefix := entry.Directory + "/"
	for path := range files {
		if strings.HasPrefix(path, prefix) {
			if _, declared := expected[path]; !declared {
				return candidate.Completion{}, errors.New(
					"candidate source recovered attempt contains an undeclared file",
				)
			}
		}
	}
	for path := range expected {
		if _, found := files[path]; !found {
			return candidate.Completion{}, errors.New(
				"candidate source recovered attempt is missing a declared file",
			)
		}
	}
	return candidate.CloneCompletion(completion)
}
