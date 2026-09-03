// Package sourcebundle implements the provider-neutral recording half of the
// candidate evidence plug-in. It retains only newly executed attempts, seals a
// credential-free source bundle, and leaves advisory model evaluation to a
// separately composed offline review plug-in.
package sourcebundle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

type Options struct {
	Directory       string
	ReceiptPath     string
	SensitiveValues []string
}

type Bundle struct {
	mu         sync.Mutex
	directory  string
	receipt    string
	root       *os.Root
	lease      *os.File
	identity   os.FileInfo
	guard      sensitiveGuard
	attempts   map[string]*attemptState
	recovered  map[string]recoveredAttempt
	claimed    map[string]struct{}
	validated  map[string]struct{}
	active     int
	finishing  bool
	finished   bool
	suite      string
	cell       bench.Cell
	provenance bench.Provenance
	origin     candidate.RunOrigin
}

type attemptState struct {
	mu        sync.Mutex
	bundle    *Bundle
	spec      candidate.Attempt
	directory string
	absolute  string
	entry     AttemptEntry
	media     *review.Media
	artifacts map[string]struct{}
	failure   error
	terminal  bool
	released  bool
}

type recoveredAttempt struct {
	completion     candidate.Completion
	completionFile SourceFile
}

type recoveredEvidence struct {
	bundle    *Bundle
	attemptID string
}

type reviewContext struct {
	Attempt                 candidate.Attempt `json:"attempt"`
	DeterministicOutcome    bench.TaskOutcome `json:"deterministic_outcome"`
	Transcript              bench.Transcript  `json:"transcript"`
	DeterministicAuthority  string            `json:"deterministic_authority"`
	AdvisoryReviewAuthority string            `json:"advisory_review_authority"`
}

func New(options Options) (*Bundle, error) {
	directory, receipt, guard, err := validateBundleOptions(options)
	if err != nil {
		return nil, err
	}
	lease, err := acquireBundleLease(context.Background(), directory)
	if err != nil {
		return nil, err
	}
	root, identity, err := createBundleRoot(directory)
	if err != nil {
		return nil, errors.Join(err, closeBundleLease(lease))
	}
	bundle := &Bundle{
		directory: directory, receipt: receipt, root: root, lease: lease, identity: identity, guard: guard,
		attempts: make(map[string]*attemptState), recovered: make(map[string]recoveredAttempt),
		claimed: make(map[string]struct{}), validated: make(map[string]struct{}),
	}
	if err := makeDirectory(root, "attempts"); err != nil {
		return nil, errors.Join(
			err, removeCreatedBundle(directory, root, identity), closeBundleLease(lease),
		)
	}
	return bundle, nil
}

func validateBundleOptions(options Options) (string, string, sensitiveGuard, error) {
	directory, err := validateAbsolutePath("candidate source directory", options.Directory)
	if err != nil {
		return "", "", sensitiveGuard{}, err
	}
	receipt, err := validateAbsolutePath("candidate source receipt", options.ReceiptPath)
	if err != nil {
		return "", "", sensitiveGuard{}, err
	}
	if withinPath(receipt, directory) || withinPath(directory, receipt) {
		return "", "", sensitiveGuard{}, errors.New(
			"candidate source directory and receipt must be independent paths",
		)
	}
	if receipt == directory+bundleLeaseSuffix {
		return "", "", sensitiveGuard{}, errors.New(
			"candidate source receipt conflicts with its exclusive lease path",
		)
	}
	if _, err := os.Lstat(receipt); err == nil {
		return "", "", sensitiveGuard{}, errors.New("candidate source receipt already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", sensitiveGuard{}, errors.New("inspect candidate source receipt destination")
	}
	guard, err := newSensitiveGuard(options.SensitiveValues)
	if err != nil {
		return "", "", sensitiveGuard{}, err
	}
	if guard.rejects([]byte(directory)) || guard.rejects([]byte(receipt)) {
		return "", "", sensitiveGuard{}, errors.New(
			"candidate source public path contains a declared sensitive value",
		)
	}
	return directory, receipt, guard, nil
}

func withinPath(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (bundle *Bundle) BeginAttempt(
	ctx context.Context, specification candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	if bundle == nil || ctx == nil {
		return nil, errors.New("begin candidate source attempt: nil bundle or context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := candidate.CloneAttempt(specification)
	if err != nil {
		return nil, err
	}
	attemptPayload, err := canonicalIndented(snapshot)
	if err != nil {
		return nil, err
	}
	if bundle.guard.rejects(attemptPayload) {
		return nil, errors.New("candidate source attempt contains a declared sensitive value")
	}
	identity := snapshot.ID()
	directory := filepath.ToSlash(filepath.Join("attempts", digestName(identity)))

	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finishing || bundle.finished || bundle.root == nil {
		return nil, errors.New("candidate source bundle is finishing")
	}
	if err := verifyBundleRoot(bundle.root, bundle.directory, bundle.identity); err != nil {
		return nil, err
	}
	if _, duplicate := bundle.attempts[identity]; duplicate {
		return nil, errors.New("candidate source attempt is duplicated")
	}
	if bundle.suite == "" {
		bundle.suite = snapshot.Suite
		bundle.cell = snapshot.Cell
		bundle.provenance = snapshot.Provenance
		bundle.origin = snapshot.Origin
	} else if snapshot.Suite != bundle.suite || !reflect.DeepEqual(snapshot.Cell, bundle.cell) ||
		!reflect.DeepEqual(snapshot.Provenance, bundle.provenance) || snapshot.Origin != bundle.origin {
		return nil, errors.New("candidate source attempt differs from its bundle identity")
	}
	if err := makeDirectory(bundle.root, directory); err != nil {
		return nil, err
	}
	attemptFile, err := writeExclusive(bundle.root, filepath.ToSlash(filepath.Join(directory, "attempt.json")), attemptPayload)
	if err != nil {
		return nil, err
	}
	attemptFile.Purpose = "candidate attempt specification"
	state := &attemptState{
		bundle: bundle, spec: snapshot, directory: directory,
		absolute:  filepath.Join(bundle.directory, filepath.FromSlash(directory)),
		artifacts: make(map[string]struct{}),
		entry: AttemptEntry{
			AttemptID: identity, Suite: snapshot.Suite, Case: snapshot.Case, Trial: snapshot.Trial,
			Directory: directory, MediaSource: snapshot.MediaSource,
			AttemptPath: attemptFile.Path, AttemptSHA256: attemptFile.SHA256,
			Artifacts: []Artifact{}, Terminal: "active",
		},
	}
	bundle.attempts[identity] = state
	bundle.active++
	return state, nil
}

func (attempt *attemptState) CaptureAudio(capture bench.SessionAudioCapture) error {
	if attempt == nil {
		return errors.New("candidate source attempt is nil")
	}
	if attempt.spec.MediaSource != candidate.MediaSharedSession {
		return attempt.recordFailure(errors.New("candidate source session audio requires a shared-session attempt"))
	}
	wav, _, err := reviewmedia.EncodeStereoWAV(capture)
	if err != nil {
		return attempt.recordFailure(errors.New("encode candidate source audio"))
	}
	return attempt.retainAudio(
		"audio.stereo.wav", "time_aligned_room_and_agent", "audio/wav", wav,
	)
}

func (attempt *attemptState) CaptureVideo(bench.SessionVideoCapture) error {
	if attempt == nil {
		return errors.New("candidate source attempt is nil")
	}
	return attempt.recordFailure(errors.New("candidate audio source bundle does not accept video"))
}

func (attempt *attemptState) CaptureMedia(media candidate.CapturedMedia) error {
	if attempt == nil {
		return errors.New("candidate source attempt is nil")
	}
	if attempt.spec.MediaSource != candidate.MediaExternalHarness {
		return attempt.recordFailure(errors.New("candidate source external audio requires an external-harness attempt"))
	}
	if !validCapturedAudio(media) {
		return attempt.recordFailure(errors.New("candidate audio source bundle accepts only external WAV media"))
	}
	return attempt.retainAudio(
		"audio.external.wav", media.Role, media.MediaType, slices.Clone(media.Bytes),
	)
}

func (attempt *attemptState) retainAudio(name, role, mediaType string, payload []byte) error {
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate source attempt is terminal")
	}
	if attempt.media != nil {
		err := errors.New("candidate source audio was already retained")
		attempt.failure = errors.Join(attempt.failure, err)
		return err
	}
	if attempt.bundle.guard.rejects(payload) {
		err := errors.New("candidate source audio contains a declared sensitive value")
		attempt.failure = errors.Join(attempt.failure, err)
		return err
	}
	file, err := writeExclusive(attempt.bundle.root, filepath.ToSlash(filepath.Join(attempt.directory, name)), payload)
	if err != nil {
		attempt.failure = errors.Join(attempt.failure, err)
		return err
	}
	attempt.media = &review.Media{
		Kind: "audio", Role: role, Path: name, SHA256: file.SHA256,
		MediaType: mediaType, SizeBytes: file.SizeBytes,
	}
	return nil
}

func (attempt *attemptState) CaptureArtifact(artifact candidate.CapturedArtifact) error {
	if attempt == nil {
		return errors.New("candidate source attempt is nil")
	}
	if attempt.spec.MediaSource != candidate.MediaExternalHarness {
		return attempt.recordFailure(errors.New("candidate source artifacts require an external-harness attempt"))
	}
	if !validArtifactIdentity(artifact) || len(artifact.Bytes) == 0 || len(artifact.Bytes) > 64<<20 {
		return attempt.recordFailure(errors.New("candidate source artifact identity is invalid"))
	}
	payload := slices.Clone(artifact.Bytes)
	if artifact.ContentType == "application/json" {
		if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
			MaxInputBytes: 64 << 20, MaxDepth: 128, MaxTokens: 5_000_000,
			MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
			MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: 64 << 20, MaxWorkBytes: 256 << 20,
		}); err != nil {
			return attempt.recordFailure(errors.New("candidate source JSON artifact is invalid"))
		}
	} else if artifact.ContentType == "text/plain; charset=utf-8" && !utf8.Valid(payload) {
		return attempt.recordFailure(errors.New("candidate source text artifact is invalid UTF-8"))
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate source attempt is terminal")
	}
	if _, duplicate := attempt.artifacts[artifact.Name]; duplicate {
		err := errors.New("candidate source artifact name is duplicated")
		attempt.failure = errors.Join(attempt.failure, err)
		return err
	}
	if attempt.bundle.guard.rejects(payload) {
		err := errors.New("candidate source artifact contains a declared sensitive value")
		attempt.failure = errors.Join(attempt.failure, err)
		return err
	}
	if len(attempt.artifacts) == 0 {
		if err := makeDirectory(attempt.bundle.root, filepath.ToSlash(filepath.Join(attempt.directory, "artifacts"))); err != nil {
			attempt.failure = errors.Join(attempt.failure, err)
			return err
		}
	}
	local := filepath.ToSlash(filepath.Join("artifacts", artifact.Name))
	file, err := writeExclusive(attempt.bundle.root, filepath.ToSlash(filepath.Join(attempt.directory, local)), payload)
	if err != nil {
		attempt.failure = errors.Join(attempt.failure, err)
		return err
	}
	attempt.artifacts[artifact.Name] = struct{}{}
	attempt.entry.Artifacts = append(attempt.entry.Artifacts, Artifact{
		Name: artifact.Name, Kind: artifact.Kind, Role: artifact.Role,
		ContentType: artifact.ContentType, Path: local,
		SHA256: file.SHA256, SizeBytes: file.SizeBytes,
	})
	return nil
}

func validCapturedAudio(media candidate.CapturedMedia) bool {
	if media.Name == "" || len(media.Name) > 256 || !utf8.ValidString(media.Name) ||
		filepath.Base(media.Name) != media.Name || filepath.Clean(media.Name) != media.Name ||
		strings.ContainsAny(media.Name, `/\`) || media.Name == "." || media.Name == ".." ||
		media.Kind != "audio" || media.MediaType != "audio/wav" ||
		len(media.Bytes) == 0 || len(media.Bytes) > 128<<20 {
		return false
	}
	for _, value := range []string{media.Name, media.Kind, media.Role, media.MediaType} {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 256 || !utf8.ValidString(value) {
			return false
		}
		for _, symbol := range value {
			if unicode.IsControl(symbol) {
				return false
			}
		}
	}
	return true
}

func validArtifactIdentity(artifact candidate.CapturedArtifact) bool {
	if artifact.Name == "" || len(artifact.Name) > 256 || !utf8.ValidString(artifact.Name) ||
		filepath.Base(artifact.Name) != artifact.Name || filepath.Clean(artifact.Name) != artifact.Name ||
		strings.ContainsAny(artifact.Name, `/\`) || artifact.Name == "." || artifact.Name == ".." {
		return false
	}
	for _, value := range []string{artifact.Name, artifact.Kind, artifact.Role, artifact.ContentType} {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 256 || !utf8.ValidString(value) {
			return false
		}
		for _, symbol := range value {
			if unicode.IsControl(symbol) {
				return false
			}
		}
	}
	return (artifact.Kind == "trace" && artifact.ContentType == "application/json") ||
		(artifact.Kind == "labels" && artifact.ContentType == "text/plain; charset=utf-8")
}

func (attempt *attemptState) recordFailure(err error) error {
	if err == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate source attempt is terminal")
	}
	attempt.failure = errors.Join(attempt.failure, err)
	return err
}

func (attempt *attemptState) Complete(
	ctx context.Context, completion candidate.Completion,
) (resultErr error) {
	if attempt == nil || ctx == nil {
		return errors.New("complete candidate source attempt: nil attempt or context")
	}
	snapshot, err := candidate.CloneCompletion(completion)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(snapshot.Attempt, attempt.spec) {
		return attempt.recordFailure(errors.New("candidate source completion differs from its preregistered attempt"))
	}
	attempt.mu.Lock()
	if attempt.terminal {
		attempt.mu.Unlock()
		return errors.New("candidate source attempt is terminal")
	}
	attempt.terminal = true
	prior := attempt.failure
	media := cloneMedia(attempt.media)
	attempt.mu.Unlock()
	defer func() { attempt.release() }()

	if media == nil {
		resultErr = errors.Join(resultErr, errors.New("candidate source attempt is missing audio"))
	}
	contextValue := reviewContext{
		Attempt: snapshot.Attempt, DeterministicOutcome: snapshot.Outcome, Transcript: snapshot.Transcript,
		DeterministicAuthority:  "benchmark scorer is authoritative",
		AdvisoryReviewAuthority: "offline multimodal review is advisory",
	}
	contextPayload, err := json.Marshal(contextValue)
	if err != nil {
		resultErr = errors.Join(resultErr, errors.New("encode candidate review context"))
	}
	completionPayload, err := canonicalIndented(snapshot)
	if err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	if attempt.bundle.guard.rejects(contextPayload) || attempt.bundle.guard.rejects(completionPayload) {
		resultErr = errors.Join(resultErr, errors.New("candidate source completion contains a declared sensitive value"))
	}

	var prepared review.PreparedRequest
	var contextSHA string
	if resultErr == nil {
		requestMedia := *media
		requestMedia.Validation = ""
		requestMedia.SizeBytes = 0
		prepared, err = review.PrepareContext(ctx, review.Request{
			AttemptID: snapshot.Attempt.ID(), Suite: snapshot.Attempt.Suite,
			Case: snapshot.Attempt.Case, Trial: snapshot.Attempt.Trial,
			RootDirectory: attempt.absolute, Context: contextPayload,
			Media: []review.Media{requestMedia}, SensitiveValues: slices.Clone(attempt.bundle.guardValues()),
		})
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}
	if resultErr == nil {
		contextSHA, err = review.CanonicalContextSHA256(ctx, contextPayload)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}
	if resultErr == nil {
		contextFile, writeErr := writeExclusive(
			attempt.bundle.root, filepath.ToSlash(filepath.Join(attempt.directory, "review-context.json")), contextPayload,
		)
		if writeErr != nil {
			resultErr = errors.Join(resultErr, writeErr)
		} else {
			completionFile, writeErr := writeExclusive(
				attempt.bundle.root, filepath.ToSlash(filepath.Join(attempt.directory, "completion.json")), completionPayload,
			)
			if writeErr != nil {
				resultErr = errors.Join(resultErr, writeErr)
			} else {
				attempt.mu.Lock()
				attempt.entry.ContextPath = filepath.Base(contextFile.Path)
				attempt.entry.ContextSHA256 = contextSHA
				attempt.entry.CompletionPath = filepath.Base(completionFile.Path)
				attempt.entry.CompletionSHA256 = completionFile.SHA256
				validated := prepared.Media[0].Media
				attempt.media = &validated
				attempt.entry.Media = &validated
				attempt.mu.Unlock()
			}
		}
	}
	attempt.mu.Lock()
	attempt.entry.Deterministic = snapshot.Outcome
	attempt.entry.Terminal = "completed"
	attempt.entry.EvidenceComplete = prior == nil && resultErr == nil
	entry := cloneEntry(attempt.entry)
	attempt.mu.Unlock()
	entryPayload, markerErr := canonicalIndented(entry)
	if markerErr != nil || attempt.bundle.guard.rejects(entryPayload) {
		markerErr = errors.New("encode candidate source attempt commit marker")
	} else {
		_, markerErr = publishAttemptEntry(attempt.bundle.root, attempt.directory, entryPayload)
	}
	if markerErr != nil {
		resultErr = errors.Join(resultErr, markerErr)
		attempt.mu.Lock()
		attempt.entry.EvidenceComplete = false
		attempt.mu.Unlock()
	}
	return errors.Join(prior, resultErr)
}

func (attempt *attemptState) Abort() error {
	if attempt == nil {
		return errors.New("abort candidate source attempt: nil attempt")
	}
	attempt.mu.Lock()
	if attempt.terminal {
		attempt.mu.Unlock()
		return nil
	}
	attempt.terminal = true
	attempt.entry.Terminal = "aborted"
	prior := attempt.failure
	attempt.mu.Unlock()
	payload, err := canonicalIndented(struct {
		AttemptID string `json:"attempt_id"`
		Terminal  string `json:"terminal"`
	}{attempt.spec.ID(), "aborted"})
	if err == nil && !attempt.bundle.guard.rejects(payload) {
		_, err = writeExclusive(attempt.bundle.root, filepath.ToSlash(filepath.Join(attempt.directory, "aborted.json")), payload)
	}
	attempt.release()
	return errors.Join(prior, err)
}

func (attempt *attemptState) release() {
	attempt.mu.Lock()
	if attempt.released {
		attempt.mu.Unlock()
		return
	}
	attempt.released = true
	attempt.mu.Unlock()
	attempt.bundle.mu.Lock()
	if attempt.bundle.active > 0 {
		attempt.bundle.active--
	}
	attempt.bundle.mu.Unlock()
}

func (bundle *Bundle) FinishSuite(ctx context.Context, result bench.Result) (resultErr error) {
	if bundle == nil || ctx == nil {
		return errors.New("finish candidate source bundle: nil bundle or context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot, err := candidate.CloneResult(result)
	if err != nil {
		return err
	}
	bundle.mu.Lock()
	if bundle.finished || bundle.finishing || bundle.root == nil {
		bundle.mu.Unlock()
		return errors.New("candidate source bundle is already finishing or closed")
	}
	if bundle.active != 0 {
		bundle.mu.Unlock()
		return errors.New("candidate source bundle has active attempts")
	}
	bundle.finishing = true
	states := make([]*attemptState, 0, len(bundle.attempts))
	for _, state := range bundle.attempts {
		states = append(states, state)
	}
	recoveredCount, claimedCount, validatedCount :=
		len(bundle.recovered), len(bundle.claimed), len(bundle.validated)
	bundle.mu.Unlock()
	defer func() {
		bundle.mu.Lock()
		bundle.finishing = false
		bundle.finished = true
		bundle.mu.Unlock()
	}()
	defer func() {
		if err := closeBundleLease(bundle.lease); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		bundle.lease = nil
	}()
	defer func() {
		if bundle.root != nil {
			if err := bundle.root.Close(); err != nil {
				resultErr = errors.Join(resultErr, errors.New("close candidate source bundle"))
			}
			bundle.root = nil
		}
	}()

	entries := make([]AttemptEntry, 0, len(states))
	for _, state := range states {
		state.mu.Lock()
		entry := cloneEntry(state.entry)
		state.mu.Unlock()
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Case != entries[right].Case {
			return entries[left].Case < entries[right].Case
		}
		return entries[left].Trial < entries[right].Trial
	})
	if len(entries) == 0 || snapshot.Suite != bundle.suite || !reflect.DeepEqual(snapshot.Cell, bundle.cell) ||
		!matchingRunProvenance(snapshot.Provenance, bundle.provenance) {
		resultErr = errors.Join(resultErr, errors.New("candidate source final result differs from its attempts"))
	}
	if len(entries) != len(snapshot.Tasks) {
		resultErr = errors.Join(resultErr, errors.New("candidate source population differs from deterministic result"))
	}
	if claimedCount != recoveredCount {
		resultErr = errors.Join(resultErr, errors.New(
			"candidate source resumed population contains an unclaimed durable attempt",
		))
	}
	if validatedCount != recoveredCount {
		return errors.Join(resultErr, errors.New(
			"candidate source resumed population contains a recovery without deterministic rescore",
		))
	}
	for _, entry := range entries {
		if !entry.EvidenceComplete {
			resultErr = errors.Join(resultErr, errors.New("candidate source bundle contains incomplete attempt evidence"))
			break
		}
	}

	resultPayload, err := canonicalPopulationIndented(snapshot)
	if err != nil || bundle.guard.rejects(resultPayload) {
		resultErr = errors.Join(resultErr, errors.New("candidate source result is invalid or sensitive"))
		return resultErr
	}
	resultFile, err := writeExclusive(bundle.root, resultName, resultPayload)
	if err != nil {
		return errors.Join(resultErr, err)
	}
	reviewPayload := sourceReview(snapshot, entries, bundle.guard)
	reviewFile, err := writeExclusive(bundle.root, reviewName, reviewPayload)
	if err != nil {
		return errors.Join(resultErr, err)
	}
	files, err := walkSourceFiles(bundle.root)
	if err != nil {
		return errors.Join(resultErr, err)
	}
	for index := range files {
		files[index].Purpose = sourceFilePurpose(files[index].Path)
	}
	setDigest, err := fileSetDigest(files)
	if err != nil {
		return errors.Join(resultErr, err)
	}
	manifest := Manifest{
		Format: ManifestFormat, FormatVersion: ManifestFormatVersion, Complete: true,
		Suite: bundle.suite, Cell: bundle.cell, Provenance: snapshot.Provenance, Origin: bundle.origin,
		AttemptCount: len(entries), ResultPath: resultName, ResultSHA256: resultFile.SHA256,
		ReviewPath: reviewName, ReviewSHA256: reviewFile.SHA256,
		FileSetSHA256: setDigest, Files: files, Attempts: entries, Advisory: "pending",
	}
	manifestPayload, err := canonicalPopulationIndented(manifest)
	if err != nil || bundle.guard.rejects(manifestPayload) {
		return errors.Join(resultErr, errors.New("candidate source manifest is invalid or sensitive"))
	}
	manifestFile, err := writeExclusive(bundle.root, manifestName, manifestPayload)
	if err != nil {
		return errors.Join(resultErr, err)
	}
	if err := verifyBundleRoot(bundle.root, bundle.directory, bundle.identity); err != nil {
		return errors.Join(resultErr, err)
	}
	if err := bundle.root.Close(); err != nil {
		bundle.root = nil
		return errors.Join(resultErr, errors.New("close sealed candidate source bundle"))
	}
	bundle.root = nil
	receipt := Receipt{
		Format: ReceiptFormat, FormatVersion: ReceiptFormatVersion, Directory: bundle.directory,
		ManifestSHA256: manifestFile.SHA256, FileSetSHA256: setDigest, AttemptCount: len(entries),
	}
	receiptBytes, err := receiptPayload(receipt)
	if err != nil || bundle.guard.rejects(receiptBytes) {
		return errors.Join(resultErr, errors.New("candidate source receipt is invalid or sensitive"))
	}
	if err := writeExternalReceipt(bundle.receipt, receiptBytes); err != nil {
		return errors.Join(resultErr, err)
	}
	return resultErr
}

// Close releases an unsealed bundle after a pre-run or attempt-admission
// failure. It never deletes retained partial evidence and cannot race an
// active attempt or a suite seal.
func (bundle *Bundle) Close() error {
	if bundle == nil {
		return errors.New("close candidate source bundle: nil bundle")
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finished {
		return nil
	}
	if bundle.finishing || bundle.active != 0 {
		return errors.New("candidate source bundle is active or finishing")
	}
	bundle.finished = true
	var resultErr error
	if bundle.root != nil {
		if err := bundle.root.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("close candidate source bundle"))
		}
		bundle.root = nil
	}
	if err := closeBundleLease(bundle.lease); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	bundle.lease = nil
	return resultErr
}

func (bundle *Bundle) guardValues() []string {
	if bundle == nil {
		return nil
	}
	return slices.Clone(bundle.guard.values)
}

func cloneMedia(source *review.Media) *review.Media {
	if source == nil {
		return nil
	}
	result := *source
	return &result
}

func cloneEntry(source AttemptEntry) AttemptEntry {
	result := source
	result.Media = cloneMedia(source.Media)
	result.Artifacts = slices.Clone(source.Artifacts)
	result.Deterministic = cloneOutcome(source.Deterministic)
	return result
}

func cloneOutcome(source bench.TaskOutcome) bench.TaskOutcome {
	payload, err := json.Marshal(source)
	if err != nil {
		return bench.TaskOutcome{ID: source.ID, Error: "unavailable"}
	}
	var result bench.TaskOutcome
	if json.Unmarshal(payload, &result) != nil {
		return bench.TaskOutcome{ID: source.ID, Error: "unavailable"}
	}
	return result
}

func matchingRunProvenance(left, right bench.Provenance) bool {
	left.FinishedAt = ""
	right.FinishedAt = ""
	return reflect.DeepEqual(left, right)
}

// BindRun binds a fresh or resumed source bundle to exactly one candidate run.
// A resumed process may have a new wall-clock start observation, but it must be
// the same immutable build on the same host. The original start time remains
// authoritative for the final campaign provenance.
func (bundle *Bundle) BindRun(
	ctx context.Context, suite string, cell bench.Cell, provenance bench.Provenance,
	origin candidate.RunOrigin,
) (bench.Provenance, error) {
	if bundle == nil || ctx == nil {
		return bench.Provenance{}, errors.New("bind candidate source run: nil bundle or context")
	}
	if err := ctx.Err(); err != nil {
		return bench.Provenance{}, err
	}
	probe, err := candidate.NewAttempt(
		suite, "source-bundle-run-binding", 1, cell, provenance, origin,
		map[string]string{"purpose": "validate resumable run identity"},
	)
	if err != nil {
		return bench.Provenance{}, err
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finishing || bundle.finished || bundle.root == nil {
		return bench.Provenance{}, errors.New("candidate source bundle is finishing")
	}
	if err := verifyBundleRoot(bundle.root, bundle.directory, bundle.identity); err != nil {
		return bench.Provenance{}, err
	}
	if bundle.suite == "" {
		bundle.suite, bundle.cell = probe.Suite, probe.Cell
		bundle.provenance, bundle.origin = probe.Provenance, probe.Origin
		return bundle.provenance, nil
	}
	if probe.Suite != bundle.suite || !reflect.DeepEqual(probe.Cell, bundle.cell) ||
		probe.Origin != bundle.origin || !matchingResumeBuild(probe.Provenance, bundle.provenance) {
		return bench.Provenance{}, errors.New("candidate source resumed run identity differs from retained attempts")
	}
	retained := bundle.provenance
	retained.FinishedAt = ""
	return retained, nil
}

func matchingResumeBuild(left, right bench.Provenance) bool {
	left.StartedAt, left.FinishedAt = "", ""
	right.StartedAt, right.FinishedAt = "", ""
	return reflect.DeepEqual(left, right)
}

// RecoverAttempt returns one exact durable completion from a reopened bundle.
// Each retained identity can be claimed only once by the new lifecycle.
func (bundle *Bundle) RecoverAttempt(
	ctx context.Context, specification candidate.Attempt,
) (candidate.Recovery, bool, error) {
	if bundle == nil || ctx == nil {
		return candidate.Recovery{}, false, errors.New(
			"recover candidate source attempt: nil bundle or context",
		)
	}
	if err := ctx.Err(); err != nil {
		return candidate.Recovery{}, false, err
	}
	snapshot, err := candidate.CloneAttempt(specification)
	if err != nil {
		return candidate.Recovery{}, false, err
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finishing || bundle.finished || bundle.root == nil {
		return candidate.Recovery{}, false, errors.New("candidate source bundle is finishing")
	}
	if err := verifyBundleRoot(bundle.root, bundle.directory, bundle.identity); err != nil {
		return candidate.Recovery{}, false, err
	}
	recovered, found := bundle.recovered[snapshot.ID()]
	if !found {
		return candidate.Recovery{}, false, nil
	}
	if _, duplicate := bundle.claimed[snapshot.ID()]; duplicate {
		return candidate.Recovery{}, false, errors.New("candidate source recovered attempt was already claimed")
	}
	if !reflect.DeepEqual(recovered.completion.Attempt, snapshot) {
		return candidate.Recovery{}, false, errors.New(
			"candidate source recovered attempt differs from the requested contract",
		)
	}
	owned, err := candidate.CloneCompletion(recovered.completion)
	if err != nil {
		return candidate.Recovery{}, false, err
	}
	bundle.claimed[snapshot.ID()] = struct{}{}
	return candidate.Recovery{
		Completion: owned,
		Evidence:   &recoveredEvidence{bundle: bundle, attemptID: snapshot.ID()},
	}, true, nil
}

// ReopenTranscript reads and authenticates completion.json again after the
// recovery claim. Discovery-time decoded state is deliberately insufficient:
// the suite validator must receive the durable transcript that exists at the
// moment the lifecycle decides whether to commit the recovered attempt.
func (evidence *recoveredEvidence) ReopenTranscript(ctx context.Context) (bench.Transcript, error) {
	if evidence == nil || evidence.bundle == nil || ctx == nil {
		return bench.Transcript{}, errors.New("reopen candidate source transcript: nil evidence or context")
	}
	if err := ctx.Err(); err != nil {
		return bench.Transcript{}, err
	}
	bundle := evidence.bundle
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finishing || bundle.finished || bundle.root == nil {
		return bench.Transcript{}, errors.New("candidate source bundle is finishing")
	}
	if err := verifyBundleRoot(bundle.root, bundle.directory, bundle.identity); err != nil {
		return bench.Transcript{}, err
	}
	recovered, found := bundle.recovered[evidence.attemptID]
	if !found {
		return bench.Transcript{}, errors.New("candidate source recovered evidence is missing")
	}
	payload, err := readRegular(
		bundle.root, recovered.completionFile.Path, recovered.completionFile.SizeBytes,
	)
	if err != nil || digest(payload) != recovered.completionFile.SHA256 {
		return bench.Transcript{}, errors.New("candidate source recovered completion changed before rescore")
	}
	var reopened candidate.Completion
	if err := decodeCanonical(payload, &reopened); err != nil ||
		normalizeAttemptContext(&reopened.Attempt) != nil || reopened.Validate() != nil {
		return bench.Transcript{}, errors.New("candidate source recovered completion is invalid during rescore")
	}
	if !reflect.DeepEqual(reopened.Attempt, recovered.completion.Attempt) {
		return bench.Transcript{}, errors.New("candidate source recovered attempt changed before rescore")
	}
	owned, err := candidate.CloneCompletion(reopened)
	if err != nil {
		return bench.Transcript{}, err
	}
	return owned.Transcript, nil
}

// CommitValidated records the lifecycle's successful exact rescore. It is the
// only path by which a recovered source entry becomes eligible for final
// bundle sealing.
func (evidence *recoveredEvidence) CommitValidated(ctx context.Context) error {
	if evidence == nil || evidence.bundle == nil || ctx == nil {
		return errors.New("commit candidate source recovery: nil evidence or context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bundle := evidence.bundle
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finishing || bundle.finished || bundle.root == nil {
		return errors.New("candidate source bundle is finishing")
	}
	if _, found := bundle.recovered[evidence.attemptID]; !found {
		return errors.New("candidate source recovered evidence is missing")
	}
	if _, claimed := bundle.claimed[evidence.attemptID]; !claimed {
		return errors.New("candidate source recovered evidence was not claimed")
	}
	if _, duplicate := bundle.validated[evidence.attemptID]; duplicate {
		return errors.New("candidate source recovered evidence was already validated")
	}
	bundle.validated[evidence.attemptID] = struct{}{}
	return nil
}

var _ candidate.Plugin = (*Bundle)(nil)
var _ candidate.AttemptEvidence = (*attemptState)(nil)
var _ candidate.CapturedMediaEvidence = (*attemptState)(nil)
var _ candidate.CapturedArtifactEvidence = (*attemptState)(nil)
var _ candidate.RunBinder = (*Bundle)(nil)
var _ candidate.AttemptRecoverer = (*Bundle)(nil)
var _ candidate.RecoveredEvidence = (*recoveredEvidence)(nil)
