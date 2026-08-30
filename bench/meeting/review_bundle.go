package meeting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"math"
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
	revieweval "github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	ReviewBundleFormat        = "openrealtime.meeting-review"
	ReviewBundleFormatVersion = 2
	ReviewContextFormat       = "openrealtime.meeting-review-context"
	ReviewContextVersion      = 1

	maximumReviewContextBytes  = 4 << 20
	maximumReviewManifestBytes = 16 << 20
	maximumReviewSecrets       = 256
	maximumReviewSecretBytes   = 4 << 10
	minimumReviewSecretBytes   = 8
)

// ReviewVideoFactory supplies fresh, attempt-scoped encoder and independent
// full-decode attestor plug-ins. Omitting it produces the always-required
// playable stereo WAV only. Supplying it additionally retains synchronized
// screen+room+agent MP4 without selecting an encoder inside this package.
type ReviewVideoFactory interface {
	NewReviewVideo(context.Context, EvidenceAttempt) (reviewmedia.Encoder, reviewmedia.Attestor, error)
}

// ReviewMedia identifies exact, independently verified retained content.
type ReviewMedia struct {
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

type ReviewFinding = revieweval.Finding

// ReviewResponse is the normalized index of a response that passed the shared
// provider-neutral Evaluate+Verify contract. The complete provider exchange
// and its immutable identities are retained separately under ReviewArtifacts.
type ReviewResponse struct {
	Reviewer                revieweval.ProviderDescriptor `json:"reviewer"`
	RequestSHA256           string                        `json:"request_sha256"`
	MediaUsable             bool                          `json:"media_usable"`
	ObservedOutcome         string                        `json:"observed_outcome"`
	AgreesWithDeterministic bool                          `json:"agrees_with_deterministic"`
	Confidence              float64                       `json:"confidence"`
	Summary                 string                        `json:"summary"`
	SignificantProblems     []ReviewFinding               `json:"significant_problems"`
	MinorObservations       []ReviewFinding               `json:"minor_observations"`
	Limitations             []string                      `json:"limitations"`
}

type ReviewBundleOptions struct {
	Directory string
	// SourceReceiptPath and EvaluationReceiptDirectory are caller-selectable,
	// create-only anchors outside Directory. Defaults are sibling paths. The
	// deterministic source receipt is durably written and reopened before any
	// reviewer invocation; evaluation receipts are written immediately after
	// each sealed sibling evaluation so a retry or new process can adopt it.
	SourceReceiptPath          string
	EvaluationReceiptDirectory string
	// EvaluationQuarantineDirectory is a caller-owned, pre-created directory
	// outside the reportable review tree. Interrupted pre-receipt evaluation
	// stages are preserved there and can never be mistaken for committed
	// evidence. The default is a sibling of Directory.
	EvaluationQuarantineDirectory string
	// Reviewer is an already-open, caller-owned provider lease from the shared
	// review registry. The bundle invokes review.Evaluate, which pins provider
	// identity/configuration/capabilities and requires private response
	// verification. The caller remains responsible for closing the lease.
	Reviewer                      *revieweval.ProviderLease
	VideoFactory                  ReviewVideoFactory
	SensitiveValues               []string
	afterEvaluationPublication    func(string) error
	afterSourceReceiptPublication func() error
	beforeAbandonRootClose        func()
}

type ReviewArtifact struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

type ReviewAttempt struct {
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
	SecondaryReview      *ReviewArtifact            `json:"secondary_review,omitempty"`
	EvaluationManifest   *ReviewArtifact            `json:"evaluation_manifest,omitempty"`
	EvaluationReceipt    *ReviewEvaluationReceipt   `json:"evaluation_receipt,omitempty"`
	ReviewArtifacts      []ReviewArtifact           `json:"review_artifacts,omitempty"`
	ReviewStatus         string                     `json:"review_status"`
	RunOrigin            EvidenceRunOrigin          `json:"run_origin"`
	ExecutionStatus      string                     `json:"execution_status"`
	Reportable           bool                       `json:"reportable"`
	ReportabilityNote    string                     `json:"reportability_note,omitempty"`
	Assessment           *ReviewResponse            `json:"assessment,omitempty"`
}

// ReviewEvaluationReceipt is the portable, directory-relative projection of
// review.EvaluationBundleReceipt retained by the enclosing Meeting bundle.
// The four digests are minted by WriteEvaluationBundle; Directory is excluded
// from their portable identity and is resolved beneath this bundle at verify.
type ReviewEvaluationReceipt struct {
	Directory                    string `json:"directory"`
	ManifestSHA256               string `json:"manifest_sha256"`
	RecordSHA256                 string `json:"record_sha256"`
	FileSetSHA256                string `json:"file_set_sha256"`
	ReceiptSHA256                string `json:"receipt_sha256"`
	EvidenceScope                string `json:"evidence_scope"`
	IndependentRemoteAttestation bool   `json:"independent_remote_attestation"`
	AuthenticityCaveat           string `json:"authenticity_caveat"`
}

type ReviewMissing struct {
	Case   string `json:"case"`
	Reason string `json:"reason"`
}

type ReviewManifest struct {
	Format                       string                        `json:"format"`
	FormatVersion                int                           `json:"format_version"`
	Suite                        string                        `json:"suite"`
	Expected                     int                           `json:"expected"`
	Complete                     bool                          `json:"complete"`
	Reportable                   bool                          `json:"reportable"`
	CoreReportable               bool                          `json:"core_reportable"`
	CoreReportability            string                        `json:"core_reportability,omitempty"`
	SourceManifest               ReviewArtifact                `json:"source_manifest"`
	SourceReceipt                ReviewSourceReceipt           `json:"source_receipt"`
	Result                       ReviewArtifact                `json:"result"`
	ReviewDocument               ReviewArtifact                `json:"review_document"`
	Cell                         bench.Cell                    `json:"cell"`
	Provenance                   bench.Provenance              `json:"provenance"`
	Reviewer                     revieweval.ProviderDescriptor `json:"reviewer"`
	ReviewEvidenceScope          string                        `json:"review_evidence_scope"`
	IndependentRemoteAttestation bool                          `json:"independent_remote_attestation"`
	ReviewAuthenticityCaveat     string                        `json:"review_authenticity_caveat"`
	ReportabilityErrors          []string                      `json:"reportability_errors,omitempty"`
	Missing                      []ReviewMissing               `json:"missing,omitempty"`
	Attempts                     []ReviewAttempt               `json:"attempts"`
}

// ReviewBundle is a create-only implementation of EvidencePlugin. It owns no
// model, API key, encoder binary, listener, or UI: all such capabilities enter
// through Reviewer and ReviewVideoFactory.
type ReviewBundle struct {
	mu                            sync.Mutex
	directory                     string
	root                          *os.Root
	sourceReceiptPath             string
	evaluationReceiptDirectory    string
	evaluationQuarantineDirectory string
	reviewer                      *revieweval.ProviderLease
	reviewerID                    revieweval.ProviderDescriptor
	videoFactory                  ReviewVideoFactory
	afterEvaluationPublication    func(string) error
	afterSourceReceiptPublication func() error
	beforeAbandonRootClose        func()
	sensitive                     []string
	attempts                      map[string]ReviewAttempt
	pending                       map[string]pendingMeetingReview
	failures                      map[string]string
	media                         map[string]retainedMeetingMediaReceipt
	active                        int
	closing                       bool
	finishing                     bool
	finished                      bool
	drainDone                     chan struct{}
	drainOnce                     sync.Once
	closeErr                      error
	receipt                       *ReviewBundleReceipt
	sourceReceipt                 *ReviewSourceReceipt
	sourceManifest                *ReviewSourceManifest
	sourceExternallyAnchored      bool
	sourceInputSHA256             string
	finishDone                    chan struct{}
	finishInputSHA256             string
	aborted                       bool
}

// ReviewBundleReceipt is the external anchor for a successfully sealed
// bundle. Its manifest digest is deliberately not stored inside manifest.json.
type ReviewBundleReceipt struct {
	Directory      string `json:"directory"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type retainedMeetingMediaReceipt struct {
	Directory      string
	ManifestSHA256 string
	Manifest       reviewmedia.Manifest
}

type pendingMeetingReview struct {
	Specification  EvidenceAttempt
	Outcome        bench.TaskOutcome
	Transcript     bench.Transcript
	Media          []ReviewMedia
	MediaArtifacts []ReviewArtifact
	MediaReceipt   retainedMeetingMediaReceipt
}

type preparedMeetingReview struct {
	Attempt        ReviewAttempt
	Pending        pendingMeetingReview
	ContextPayload []byte
	Evaluation     *revieweval.Evaluation
	Publication    *revieweval.EvaluationBundlePublication
}

func (pending pendingMeetingReview) clone() pendingMeetingReview {
	return pendingMeetingReview{
		Specification: cloneEvidenceAttempt(pending.Specification),
		Outcome:       cloneTaskOutcome(pending.Outcome), Transcript: cloneTranscript(pending.Transcript),
		Media: cloneReviewMedia(pending.Media), MediaArtifacts: slices.Clone(pending.MediaArtifacts),
		MediaReceipt: pending.MediaReceipt.clone(),
	}
}

var errMeetingReviewBundleAborted = errors.New("meeting review bundle was closed without a sealed commit")

func (receipt retainedMeetingMediaReceipt) clone() retainedMeetingMediaReceipt {
	result := receipt
	if receipt.Manifest.Audio != nil {
		audio := *receipt.Manifest.Audio
		result.Manifest.Audio = &audio
	}
	result.Manifest.Video = slices.Clone(receipt.Manifest.Video)
	if receipt.Manifest.Encoder != nil {
		encoder := *receipt.Manifest.Encoder
		result.Manifest.Encoder = &encoder
	}
	if receipt.Manifest.Attestor != nil {
		attestor := *receipt.Manifest.Attestor
		result.Manifest.Attestor = &attestor
	}
	return result
}

func NewReviewBundle(options ReviewBundleOptions) (*ReviewBundle, error) {
	if options.Reviewer == nil {
		return nil, errors.New("meeting review bundle requires a caller-supplied reviewer")
	}
	reviewerID := options.Reviewer.Descriptor()
	if err := reviewerID.Validate(); err != nil {
		return nil, errors.New("meeting review bundle reviewer lease is closed or invalid")
	}
	sensitive, err := canonicalReviewSecrets(options.SensitiveValues)
	if err != nil {
		return nil, err
	}
	directory, err := validateNewMeetingReviewDirectory(options.Directory)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(directory); err == nil {
		return nil, errors.New("meeting review bundle directory already exists")
	} else if !os.IsNotExist(err) {
		return nil, errors.New("inspect meeting review bundle directory")
	}
	sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory, err :=
		meetingExternalEvidencePaths(
			directory, options.SourceReceiptPath, options.EvaluationReceiptDirectory,
			options.EvaluationQuarantineDirectory, true,
		)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, errors.New("create meeting review bundle directory exclusively")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.Remove(directory)
		return nil, errors.New("open meeting review bundle directory")
	}
	bundle := &ReviewBundle{
		directory: directory, root: root, reviewer: options.Reviewer,
		sourceReceiptPath: sourceReceiptPath, evaluationReceiptDirectory: evaluationReceiptDirectory,
		evaluationQuarantineDirectory: evaluationQuarantineDirectory,
		reviewerID:                    reviewerID,
		videoFactory:                  options.VideoFactory, sensitive: sensitive,
		afterEvaluationPublication:    options.afterEvaluationPublication,
		afterSourceReceiptPublication: options.afterSourceReceiptPublication,
		beforeAbandonRootClose:        options.beforeAbandonRootClose,
		attempts:                      make(map[string]ReviewAttempt, ExpectedTasks()),
		pending:                       make(map[string]pendingMeetingReview, ExpectedTasks()),
		failures:                      make(map[string]string, ExpectedTasks()),
		media:                         make(map[string]retainedMeetingMediaReceipt, ExpectedTasks()),
		drainDone:                     make(chan struct{}),
		finishDone:                    make(chan struct{}),
	}
	for _, name := range []string{reviewSourceDirectory, "evaluations"} {
		if err := root.Mkdir(name, 0o700); err != nil {
			_ = root.Close()
			_ = os.RemoveAll(directory)
			return nil, errors.New("create meeting review bundle layout")
		}
	}
	for _, name := range []string{
		filepath.ToSlash(filepath.Join(reviewSourceDirectory, "media")),
		filepath.ToSlash(filepath.Join(reviewSourceDirectory, "contexts")),
	} {
		if err := root.Mkdir(name, 0o700); err != nil {
			_ = root.Close()
			_ = os.RemoveAll(directory)
			return nil, errors.New("create meeting review source layout")
		}
	}
	if err := os.Mkdir(evaluationReceiptDirectory, 0o700); err != nil {
		_ = root.Close()
		_ = os.RemoveAll(directory)
		return nil, errors.New("create external meeting evaluation receipt directory exclusively")
	}
	if err := os.Mkdir(evaluationQuarantineDirectory, 0o700); err != nil {
		_ = root.Close()
		_ = os.RemoveAll(directory)
		_ = os.Remove(evaluationReceiptDirectory)
		return nil, errors.New("create external meeting evaluation quarantine directory exclusively")
	}
	return bundle, nil
}

// ResumeReviewBundle opens a receipt-anchored interrupted review campaign
// without re-running deterministic Meeting tasks or regenerating media. It
// recovers the staged source marker when necessary, reconstructs exact
// advisory requests from the sealed source contexts/media/result, and lets a
// later FinishSuite adopt already receipted evaluations before invoking only
// the missing reviewer calls.
func ResumeReviewBundle(
	ctx context.Context, options ReviewBundleOptions,
) (*ReviewBundle, bench.Result, error) {
	if ctx == nil {
		return nil, bench.Result{}, errors.New("resume meeting review bundle: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, bench.Result{}, err
	}
	if options.Reviewer == nil {
		return nil, bench.Result{}, errors.New("resume meeting review bundle requires a caller-supplied reviewer")
	}
	reviewerID := options.Reviewer.Descriptor()
	if err := reviewerID.Validate(); err != nil {
		return nil, bench.Result{}, errors.New("resume meeting reviewer lease is closed or invalid")
	}
	sensitive, err := canonicalReviewSecrets(options.SensitiveValues)
	if err != nil {
		return nil, bench.Result{}, err
	}
	directory, err := validateNewMeetingReviewDirectory(options.Directory)
	if err != nil {
		return nil, bench.Result{}, err
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return nil, bench.Result{}, errors.New("interrupted meeting review bundle is not an exact directory")
	}
	sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory, err :=
		meetingExternalEvidencePaths(
			directory, options.SourceReceiptPath, options.EvaluationReceiptDirectory,
			options.EvaluationQuarantineDirectory, false,
		)
	if err != nil {
		return nil, bench.Result{}, err
	}
	for _, path := range []string{evaluationReceiptDirectory, evaluationQuarantineDirectory} {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, bench.Result{}, errors.New("meeting review resume directory is missing or invalid")
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, bench.Result{}, errors.New("open interrupted meeting review bundle")
	}
	cleanupRoot := true
	defer func() {
		if cleanupRoot {
			_ = root.Close()
		}
	}()
	openedRoot, err := root.Stat(".")
	if err != nil || !os.SameFile(visible, openedRoot) {
		return nil, bench.Result{}, errors.New("interrupted meeting review bundle changed while opening")
	}
	for _, name := range []string{reviewSourceDirectory, "evaluations"} {
		info, statErr := root.Lstat(name)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, bench.Result{}, errors.New("interrupted meeting review layout is invalid")
		}
	}
	if _, err := root.Lstat("manifest.json"); err == nil {
		return nil, bench.Result{}, errors.New("meeting review bundle is already finally committed")
	} else if !os.IsNotExist(err) {
		return nil, bench.Result{}, errors.New("inspect interrupted meeting review commit marker")
	}
	sourceReceipt, err := ReadReviewSourceReceipt(ctx, sourceReceiptPath)
	if err != nil {
		return nil, bench.Result{}, errors.New("read interrupted meeting source receipt")
	}
	expectedSourceDirectory := filepath.Join(directory, reviewSourceDirectory)
	if sourceReceipt.Directory != expectedSourceDirectory {
		return nil, bench.Result{}, errors.New("interrupted meeting source receipt is outside the requested bundle")
	}
	openedSource, err := commitStagedMeetingReviewSource(
		ctx, sourceReceipt.Directory, sourceReceipt,
		FileReviewSourceReceiptAnchor{Path: sourceReceiptPath},
		meetingSourcePublicationOperations{},
	)
	if err != nil {
		return nil, bench.Result{}, fmt.Errorf("recover interrupted meeting source: %w", err)
	}
	attempts, pending, media, err := reconstructMeetingReviewCampaign(
		ctx, directory, openedSource,
	)
	if err != nil {
		return nil, bench.Result{}, err
	}
	resultPayload, err := json.MarshalIndent(openedSource.Result, "", "  ")
	if err != nil {
		return nil, bench.Result{}, errors.New("encode resumed meeting result")
	}
	resultPayload = append(resultPayload, '\n')
	if reviewDigest(resultPayload) != openedSource.Receipt.ResultSHA256 {
		return nil, bench.Result{}, errors.New("resumed meeting result differs from the sealed source receipt")
	}
	bundle := &ReviewBundle{
		directory: directory, root: root,
		sourceReceiptPath:             sourceReceiptPath,
		evaluationReceiptDirectory:    evaluationReceiptDirectory,
		evaluationQuarantineDirectory: evaluationQuarantineDirectory,
		reviewer:                      options.Reviewer, reviewerID: reviewerID,
		videoFactory: options.VideoFactory, sensitive: sensitive,
		afterEvaluationPublication:    options.afterEvaluationPublication,
		afterSourceReceiptPublication: options.afterSourceReceiptPublication,
		beforeAbandonRootClose:        options.beforeAbandonRootClose,
		attempts:                      attempts, pending: pending, media: media,
		failures:  make(map[string]string, ExpectedTasks()),
		drainDone: make(chan struct{}), finishDone: make(chan struct{}),
		sourceReceipt: &openedSource.Receipt, sourceManifest: &openedSource.Manifest,
		sourceExternallyAnchored: true, sourceInputSHA256: reviewDigest(resultPayload),
	}
	cleanupRoot = false
	return bundle, openedSource.Result, nil
}

func reconstructMeetingReviewCampaign(
	ctx context.Context, bundleDirectory string, source ReviewSourceBundle,
) (map[string]ReviewAttempt, map[string]pendingMeetingReview,
	map[string]retainedMeetingMediaReceipt, error) {
	attempts := make(map[string]ReviewAttempt, len(source.Manifest.Attempts))
	pending := make(map[string]pendingMeetingReview, len(source.Manifest.Attempts))
	mediaReceipts := make(map[string]retainedMeetingMediaReceipt, len(source.Manifest.Attempts))
	sourceDirectory := source.Receipt.Directory
	for _, sourceAttempt := range source.Manifest.Attempts {
		outer := prefixMeetingSourceAttempt(sourceAttempt)
		contextPayload, _, err := readMeetingReviewFileContext(
			ctx, filepath.Join(sourceDirectory, filepath.FromSlash(sourceAttempt.Context.Path)), true,
		)
		contextValue, contextErr := decodeMeetingReviewContext(contextPayload)
		if err != nil || contextErr != nil || contextValue.Case != sourceAttempt.Case ||
			contextValue.ResultSHA256 != source.Receipt.ResultSHA256 ||
			!reflect.DeepEqual(contextValue.Outcome, sourceAttempt.Deterministic) ||
			!reflect.DeepEqual(contextValue.Cell, source.Result.Cell) ||
			!reflect.DeepEqual(contextValue.Provenance, source.Result.Provenance) ||
			!reflect.DeepEqual(contextValue.RunOrigin, sourceAttempt.RunOrigin) ||
			!reflect.DeepEqual(contextValue.ExecutionRequirement, sourceAttempt.ExecutionRequirement) {
			return nil, nil, nil, errors.New("reconstruct exact meeting review context")
		}
		outerMediaDirectory, _, ok := meetingAttemptArtifactNamespaces(
			sourceAttempt.Ordinal, sourceAttempt.Case,
		)
		mediaRelative, stripErr := stripMeetingSourcePrefix(outerMediaDirectory)
		mediaDirectory := filepath.Join(sourceDirectory, filepath.FromSlash(mediaRelative))
		verified, verifyErr := reviewmedia.VerifyBundle(
			mediaDirectory, sourceAttempt.MediaManifest.SHA256,
		)
		media, mediaErr := retainedMeetingReviewMedia(
			verified, outer.MediaArtifacts, outerMediaDirectory,
		)
		if !ok || stripErr != nil || verifyErr != nil || mediaErr != nil {
			return nil, nil, nil, errors.New("reconstruct exact meeting review media")
		}
		initialProvenance := source.Result.Provenance
		initialProvenance.FinishedAt = ""
		specification := EvidenceAttempt{
			Suite: SuiteName, Case: sourceAttempt.Case, Trial: sourceAttempt.Trial,
			Task: contextValue.Task, Cell: cloneMeetingCell(source.Result.Cell),
			Provenance: initialProvenance, Origin: sourceAttempt.RunOrigin,
			ExecutionRequirement: cloneMeetingExecutionRequirement(sourceAttempt.ExecutionRequirement),
		}
		if err := specification.validate(); err != nil ||
			!meetingAttemptRunMatchesResult(specification, source.Result) ||
			!meetingTranscriptExecutionMatchesOutcome(contextValue.Transcript, sourceAttempt.Deterministic) {
			return nil, nil, nil, errors.New("reconstruct exact meeting review attempt")
		}
		mediaReceipt := retainedMeetingMediaReceipt{
			Directory: mediaDirectory, ManifestSHA256: sourceAttempt.MediaManifest.SHA256,
			Manifest: verified,
		}
		attempts[sourceAttempt.Case] = outer
		pending[sourceAttempt.Case] = pendingMeetingReview{
			Specification: specification,
			Outcome:       cloneTaskOutcome(sourceAttempt.Deterministic),
			Transcript:    cloneTranscript(contextValue.Transcript),
			Media:         media, MediaArtifacts: slices.Clone(outer.MediaArtifacts),
			MediaReceipt: mediaReceipt,
		}
		mediaReceipts[sourceAttempt.Case] = mediaReceipt
	}
	if len(attempts) != len(source.Manifest.Attempts) {
		return nil, nil, nil, errors.New("reconstructed meeting review campaign has duplicate cases")
	}
	return attempts, pending, mediaReceipts, nil
}

func (bundle *ReviewBundle) Directory() string {
	if bundle == nil {
		return ""
	}
	return bundle.directory
}

// Receipt returns the external manifest anchor only after a successful sealed
// commit. It never manufactures a receipt for an incomplete or failed bundle.
func (bundle *ReviewBundle) Receipt() (ReviewBundleReceipt, error) {
	if bundle == nil {
		return ReviewBundleReceipt{}, errors.New("meeting review bundle is nil")
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if !bundle.finished || bundle.closeErr != nil || bundle.receipt == nil {
		return ReviewBundleReceipt{}, errors.New("meeting review bundle has no successful completion receipt")
	}
	return *bundle.receipt, nil
}

// SourceReceipt returns the independently sealed deterministic source anchor
// as soon as source publication succeeds. It remains available when an
// advisory reviewer is unavailable and the enclosing review index is not yet
// complete.
func (bundle *ReviewBundle) SourceReceipt() (ReviewSourceReceipt, error) {
	if bundle == nil {
		return ReviewSourceReceipt{}, errors.New("meeting review bundle is nil")
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.sourceReceipt == nil || !bundle.sourceExternallyAnchored {
		return ReviewSourceReceipt{}, errors.New("meeting review bundle has no sealed source receipt")
	}
	return *bundle.sourceReceipt, nil
}

// EvaluationReceipts returns the successfully sealed sibling evaluation
// receipts. Advisory failures are retryable and do not remove prior receipts.
func (bundle *ReviewBundle) EvaluationReceipts() []revieweval.EvaluationBundleReceipt {
	if bundle == nil {
		return nil
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	var receipts []revieweval.EvaluationBundleReceipt
	for _, task := range Suite() {
		attempt, ok := bundle.attempts[task.ID]
		if !ok || attempt.EvaluationReceipt == nil {
			continue
		}
		portable := attempt.EvaluationReceipt
		receipts = append(receipts, revieweval.EvaluationBundleReceipt{
			Directory:      filepath.Join(bundle.directory, filepath.FromSlash(portable.Directory)),
			ManifestSHA256: portable.ManifestSHA256, RecordSHA256: portable.RecordSHA256,
			FileSetSHA256: portable.FileSetSHA256, ReceiptSHA256: portable.ReceiptSHA256,
		})
	}
	return receipts
}

// Close abandons an unused or already-drained incomplete bundle and releases
// its owned directory handle. It is idempotent. Callers should defer Close
// immediately after NewReviewBundle so validation failures that happen before
// Run installs FinishSuite cannot leak the handle. Active attempts must first
// reach Complete or Abort; a concurrent successful FinishSuite owns closure.
func (bundle *ReviewBundle) Close() error {
	if bundle == nil {
		return nil
	}
	bundle.mu.Lock()
	if bundle.finished {
		aborted, closeErr := bundle.aborted, bundle.closeErr
		bundle.mu.Unlock()
		if aborted {
			return nil
		}
		return closeErr
	}
	if bundle.active != 0 {
		bundle.mu.Unlock()
		return errors.New("meeting review bundle still owns active attempts")
	}
	if bundle.finishing {
		done := bundle.finishDone
		bundle.mu.Unlock()
		<-done
		return bundle.Close()
	}
	bundle.closing = true
	bundle.finishing = true
	// No active attempt can release the drain after this point. Publish the
	// terminal drain edge before detaching the root so a concurrent or later
	// FinishSuite cannot wait forever ahead of its finished/aborted check.
	bundle.drainOnce.Do(func() { close(bundle.drainDone) })
	root := bundle.root
	bundle.root = nil
	bundle.mu.Unlock()

	if bundle.beforeAbandonRootClose != nil {
		bundle.beforeAbandonRootClose()
	}
	var closeErr error
	if root != nil {
		closeErr = root.Close()
	}
	bundle.mu.Lock()
	bundle.aborted = true
	bundle.finished = true
	bundle.finishing = false
	bundle.closeErr = errMeetingReviewBundleAborted
	if closeErr != nil {
		bundle.closeErr = errors.Join(bundle.closeErr, errors.New("close abandoned meeting review bundle directory"))
	}
	close(bundle.finishDone)
	bundle.mu.Unlock()
	return closeErr
}

func (bundle *ReviewBundle) startAttempt(caseID string) error {
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.closing || bundle.finishing || bundle.finished || bundle.root == nil {
		return errors.New("meeting review bundle is closing or closed")
	}
	if _, exists := bundle.attempts[caseID]; exists {
		return errors.New("meeting review attempt was already completed")
	}
	if _, exists := bundle.failures[caseID]; exists {
		return errors.New("meeting review attempt was already started")
	}
	bundle.failures[caseID] = "attempt did not reach terminal evidence"
	bundle.active++
	return nil
}

func (bundle *ReviewBundle) releaseAttempt() {
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.active <= 0 {
		return
	}
	bundle.active--
	if bundle.closing && bundle.active == 0 {
		bundle.drainOnce.Do(func() { close(bundle.drainDone) })
	}
}

func (bundle *ReviewBundle) BeginAttempt(
	ctx context.Context, specification EvidenceAttempt,
) (AttemptEvidence, error) {
	if bundle == nil {
		return nil, errors.New("meeting review bundle is nil")
	}
	if ctx == nil {
		return nil, errors.New("begin meeting review attempt: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := specification.validate(); err != nil {
		return nil, err
	}
	specification = cloneEvidenceAttempt(specification)
	if err := bundle.startAttempt(specification.Case); err != nil {
		return nil, err
	}
	ownedByAttempt := false
	defer func() {
		if !ownedByAttempt {
			bundle.releaseAttempt()
		}
	}()

	var encoder reviewmedia.Encoder
	var attestor reviewmedia.Attestor
	var expectedVideo []string
	if !nilInterface(bundle.videoFactory) {
		rawEncoder, rawAttestor, factoryErr := bundle.videoFactory.NewReviewVideo(
			ctx, cloneEvidenceAttempt(specification),
		)
		encoder = wrapMeetingReviewEncoder(rawEncoder)
		attestor = wrapMeetingReviewAttestor(rawAttestor)
		if factoryErr != nil {
			closeErr := closeMeetingReviewVideoPlugins(encoder, attestor)
			bundle.recordFailure(specification.Case, "video evidence plug-in creation failed")
			return nil, errors.Join(
				errors.New("create meeting review video plug-ins"), closeErr,
			)
		}
		expectedVideo = []string{"screen"}
	}
	ordinal, ok := meetingCaseOrdinal(specification.Case)
	if !ok {
		return nil, errors.New("meeting review attempt is outside the v1 suite")
	}
	relativeDirectory, _, ok := meetingAttemptArtifactNamespaces(ordinal, specification.Case)
	if !ok {
		return nil, errors.New("meeting review attempt artifact namespace is invalid")
	}
	recorder, err := reviewmedia.New(ctx, reviewmedia.Config{
		Directory:            filepath.Join(bundle.directory, filepath.FromSlash(relativeDirectory)),
		RequireAudio:         true,
		ExpectedVideoSources: expectedVideo,
		Encoder:              encoder,
		Attestor:             attestor,
		SensitiveValues:      slices.Clone(bundle.sensitive),
	})
	if err != nil {
		closeErr := closeMeetingReviewVideoPlugins(encoder, attestor)
		bundle.recordFailure(specification.Case, "media recorder creation failed")
		return nil, errors.Join(errors.New("create meeting review media recorder"), closeErr)
	}
	result := &reviewBundleAttempt{
		bundle: bundle, specification: cloneEvidenceAttempt(specification), recorder: recorder,
		ordinal: ordinal, relativeDirectory: relativeDirectory,
	}
	ownedByAttempt = true
	return result, nil
}

// The media recorder closes only plug-ins whose Claim succeeded. A factory,
// however, transfers two fresh values to this bundle even when creation or a
// later pre-claim validation fails. These once-closing adapters let every
// failure path release both values exactly once without double-closing a value
// already released by reviewmedia.New after a partial claim.
type meetingReviewEncoderOwner struct {
	delegate reviewmedia.Encoder
	close    sync.Once
	closeErr error
}

func wrapMeetingReviewEncoder(value reviewmedia.Encoder) reviewmedia.Encoder {
	if nilInterface(value) {
		return nil
	}
	return &meetingReviewEncoderOwner{delegate: value}
}

func (owner *meetingReviewEncoderOwner) Descriptor() reviewmedia.EncoderDescriptor {
	return owner.delegate.Descriptor()
}

func (owner *meetingReviewEncoderOwner) Implementation() []byte {
	return owner.delegate.Implementation()
}

func (owner *meetingReviewEncoderOwner) Configuration() []byte {
	return owner.delegate.Configuration()
}

func (owner *meetingReviewEncoderOwner) Claim(ctx context.Context) error {
	return owner.delegate.Claim(ctx)
}

func (owner *meetingReviewEncoderOwner) Encode(
	ctx context.Context, request reviewmedia.EncodeRequest,
) error {
	return owner.delegate.Encode(ctx, request)
}

func (owner *meetingReviewEncoderOwner) Close() error {
	if owner == nil || nilInterface(owner.delegate) {
		return nil
	}
	owner.close.Do(func() { owner.closeErr = owner.delegate.Close() })
	return owner.closeErr
}

type meetingReviewAttestorOwner struct {
	delegate reviewmedia.Attestor
	close    sync.Once
	closeErr error
}

func wrapMeetingReviewAttestor(value reviewmedia.Attestor) reviewmedia.Attestor {
	if nilInterface(value) {
		return nil
	}
	return &meetingReviewAttestorOwner{delegate: value}
}

func (owner *meetingReviewAttestorOwner) Descriptor() reviewmedia.AttestorDescriptor {
	return owner.delegate.Descriptor()
}

func (owner *meetingReviewAttestorOwner) Implementation() []byte {
	return owner.delegate.Implementation()
}

func (owner *meetingReviewAttestorOwner) Configuration() []byte {
	return owner.delegate.Configuration()
}

func (owner *meetingReviewAttestorOwner) Claim(ctx context.Context) error {
	return owner.delegate.Claim(ctx)
}

func (owner *meetingReviewAttestorOwner) Attest(
	ctx context.Context, request reviewmedia.AttestationRequest,
) (reviewmedia.Attestation, error) {
	return owner.delegate.Attest(ctx, request)
}

func (owner *meetingReviewAttestorOwner) Close() error {
	if owner == nil || nilInterface(owner.delegate) {
		return nil
	}
	owner.close.Do(func() { owner.closeErr = owner.delegate.Close() })
	return owner.closeErr
}

func closeMeetingReviewVideoPlugins(
	encoder reviewmedia.Encoder, attestor reviewmedia.Attestor,
) error {
	var closeErr error
	if !nilInterface(attestor) {
		if err := attestor.Close(); err != nil {
			closeErr = errors.Join(closeErr, errors.New("close meeting review video attestor"))
		}
	}
	if !nilInterface(encoder) {
		if err := encoder.Close(); err != nil {
			closeErr = errors.Join(closeErr, errors.New("close meeting review video encoder"))
		}
	}
	return closeErr
}

func (bundle *ReviewBundle) FinishSuite(ctx context.Context, result bench.Result) error {
	if bundle == nil {
		return errors.New("meeting review bundle is nil")
	}
	if ctx == nil {
		return errors.New("finish meeting review bundle: nil context")
	}
	result, err := cloneMeetingResult(result)
	if err != nil {
		return errors.New("snapshot meeting result for review bundle")
	}
	resultPayload, err := json.MarshalIndent(result, "", "  ")
	if err != nil || len(resultPayload) == 0 || len(resultPayload) > maximumReviewManifestBytes ||
		strictjson.Validate(resultPayload) != nil {
		return errors.New("encode exact meeting benchmark result")
	}
	resultPayload = append(resultPayload, '\n')
	if containsReviewSecret(bundle.sensitive, resultPayload) {
		return errors.New("exact meeting benchmark result contains a sensitive value")
	}
	finishInputSHA256 := reviewDigest(resultPayload)
	knownTasks := make(map[string]struct{}, ExpectedTasks())
	for _, task := range Suite() {
		knownTasks[task.ID] = struct{}{}
	}
	resultTasks := make(map[string]bench.TaskOutcome, len(result.Tasks))
	resultTaskIdentitiesValid := true
	for _, outcome := range result.Tasks {
		_, known := knownTasks[outcome.ID]
		if _, duplicate := resultTasks[outcome.ID]; duplicate || !known {
			resultTaskIdentitiesValid = false
			continue
		}
		resultTasks[outcome.ID] = outcome
	}
	resultSummaryValid := meetingResultSummaryIsFinal(result)
	reviewInputValid := result.Suite == SuiteName && result.Expected == ExpectedTasks() &&
		len(result.Tasks) <= ExpectedTasks() && resultTaskIdentitiesValid && resultSummaryValid

	// Closing rejects new attempts. The first caller that observes every owned
	// attempt at a terminal state becomes the sole committer; concurrent callers
	// wait for that exact result. Cancellation while draining does not tear the
	// bundle away from an active Complete/Abort call.
	bundle.mu.Lock()
	if !bundle.closing {
		bundle.closing = true
		if bundle.active == 0 {
			bundle.drainOnce.Do(func() { close(bundle.drainDone) })
		}
	}
	drainDone := bundle.drainDone
	bundle.mu.Unlock()
	select {
	case <-drainDone:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}

	bundle.mu.Lock()
	if bundle.sourceInputSHA256 != "" && bundle.sourceInputSHA256 != finishInputSHA256 {
		bundle.mu.Unlock()
		return errors.New("meeting review bundle finish result differs from its sealed source result")
	}
	if bundle.finished || bundle.finishing {
		boundInput := bundle.finishInputSHA256
		done := bundle.finishDone
		bundle.mu.Unlock()
		if boundInput != "" && boundInput != finishInputSHA256 {
			return errors.New("meeting review bundle finish result differs from the committing result")
		}
		select {
		case <-done:
			bundle.mu.Lock()
			err := bundle.closeErr
			bundle.mu.Unlock()
			return err
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	bundle.finishing = true
	bundle.finishInputSHA256 = finishInputSHA256
	root := bundle.root
	var sourceReceipt *ReviewSourceReceipt
	if bundle.sourceReceipt != nil {
		copy := *bundle.sourceReceipt
		sourceReceipt = &copy
	}
	var sourceManifest *ReviewSourceManifest
	if bundle.sourceManifest != nil {
		copy := *bundle.sourceManifest
		sourceManifest = &copy
	}
	attempts := make(map[string]ReviewAttempt, len(bundle.attempts))
	for name, attempt := range bundle.attempts {
		attempts[name] = cloneReviewAttempt(attempt)
	}
	failures := make(map[string]string, len(bundle.failures))
	for name, failure := range bundle.failures {
		failures[name] = failure
	}
	mediaReceipts := make(map[string]retainedMeetingMediaReceipt, len(bundle.media))
	for name, receipt := range bundle.media {
		mediaReceipts[name] = receipt.clone()
	}
	pending := make(map[string]pendingMeetingReview, len(bundle.pending))
	for name, input := range bundle.pending {
		pending[name] = input.clone()
	}
	bundle.mu.Unlock()

	retryFinish := func(finishErr error) error {
		bundle.mu.Lock()
		bundle.closeErr = finishErr
		bundle.finishing = false
		if bundle.sourceReceipt == nil {
			bundle.finishInputSHA256 = ""
		}
		close(bundle.finishDone)
		bundle.finishDone = make(chan struct{})
		bundle.mu.Unlock()
		return finishErr
	}
	if !reviewInputValid {
		return retryFinish(errors.New("meeting review source result identity or summary is invalid"))
	}
	if sourceReceipt == nil || sourceManifest == nil {
		var sourceErr error
		attempts, sourceValue, receiptValue, sourceErr := bundle.publishDeterministicMeetingSource(
			ctx, root, result, resultPayload, finishInputSHA256,
			attempts, pending, failures, resultTasks,
		)
		if sourceErr != nil {
			return retryFinish(sourceErr)
		}
		sourceReceipt = &receiptValue
		sourceManifest = &sourceValue
		bundle.mu.Lock()
		bundle.sourceReceipt = &receiptValue
		bundle.sourceManifest = &sourceValue
		bundle.sourceInputSHA256 = finishInputSHA256
		for name, attempt := range attempts {
			bundle.attempts[name] = cloneReviewAttempt(attempt)
		}
		bundle.mu.Unlock()
	} else {
		opened, sourceErr := VerifyMeetingReviewSource(
			ctx, sourceReceipt.Directory, *sourceReceipt,
		)
		if sourceErr != nil || opened.Receipt.ResultSHA256 != finishInputSHA256 ||
			!reflect.DeepEqual(opened.Manifest, *sourceManifest) {
			return retryFinish(errors.New("sealed meeting review source failed retry verification"))
		}
	}
	externalSourceReceipt, externalSourceErr := ReadReviewSourceReceipt(ctx, bundle.sourceReceiptPath)
	if externalSourceErr != nil || externalSourceReceipt.Directory != sourceReceipt.Directory ||
		!samePortableMeetingSourceReceipt(externalSourceReceipt, *sourceReceipt) {
		return retryFinish(errors.New("external meeting source receipt changed before secondary review"))
	}
	bundle.mu.Lock()
	bundle.sourceExternallyAnchored = true
	bundle.mu.Unlock()

	prepared, prepareErr := bundle.prepareMeetingReviews(
		ctx, result, finishInputSHA256, attempts, pending, resultTasks,
	)
	publicationContext := ctx
	var publicationErr error
	for _, task := range Suite() {
		preparedAttempt, found := prepared[task.ID]
		if !found {
			continue
		}
		published, publishErr := bundle.publishPreparedMeetingReview(
			publicationContext, root, preparedAttempt,
		)
		if publishErr != nil {
			publicationErr = errors.Join(publicationErr,
				fmt.Errorf("%s: secondary review publication failed: %w", task.ID, publishErr))
			continue
		}
		attempts[task.ID] = cloneReviewAttempt(published)
		bundle.mu.Lock()
		bundle.attempts[task.ID] = cloneReviewAttempt(published)
		bundle.mu.Unlock()
	}
	if prepareErr != nil || publicationErr != nil {
		return retryFinish(errors.Join(prepareErr, publicationErr))
	}
	if err := context.Cause(ctx); err != nil {
		return retryFinish(err)
	}
	bundle.mu.Lock()
	bundle.root = nil
	bundle.mu.Unlock()

	finish := func(err error) error {
		if root != nil {
			if closeErr := root.Close(); closeErr != nil && err == nil {
				err = errors.New("close meeting review bundle directory")
			}
		}
		bundle.mu.Lock()
		bundle.closeErr = err
		bundle.finishing = false
		bundle.finished = true
		close(bundle.finishDone)
		bundle.mu.Unlock()
		return err
	}
	sourceManifestPayload, _, err := readSealedMeetingReviewFile(filepath.Join(
		sourceReceipt.Directory, reviewSourceManifestName,
	))
	if err != nil || reviewDigest(sourceManifestPayload) != sourceReceipt.ManifestSHA256 {
		return finish(errors.New("reopen exact meeting review source manifest"))
	}
	sourceProjection := *sourceReceipt
	sourceProjection.Directory = reviewSourceDirectory
	manifest := ReviewManifest{
		Format: ReviewBundleFormat, FormatVersion: ReviewBundleFormatVersion,
		Suite: SuiteName, Expected: ExpectedTasks(), Reportable: true,
		SourceManifest: ReviewArtifact{
			Path:   filepath.ToSlash(filepath.Join(reviewSourceDirectory, reviewSourceManifestName)),
			SHA256: sourceReceipt.ManifestSHA256, SizeBytes: int64(len(sourceManifestPayload)),
			MediaType: "application/json",
		},
		SourceReceipt: sourceProjection,
		Result: ReviewArtifact{Path: filepath.ToSlash(filepath.Join(reviewSourceDirectory, "result.json")),
			SHA256:    reviewDigest(resultPayload),
			SizeBytes: int64(len(resultPayload)), MediaType: "application/json"},
		Cell: result.Cell, Provenance: result.Provenance, Reviewer: bundle.reviewerID,
	}
	if reportabilityErr := result.Reportable(); reportabilityErr != nil {
		manifest.Reportable = false
		manifest.CoreReportability = reportabilityErr.Error()
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"core benchmark reportability refused this result")
	} else {
		manifest.CoreReportable = true
	}
	if result.Suite != SuiteName || result.Expected != ExpectedTasks() {
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"the benchmark result does not identify the complete Meeting Assistant v1 suite")
	}
	if len(result.Tasks) != ExpectedTasks() {
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"the benchmark result does not contain exactly four Meeting Assistant cases")
	}
	if !resultTaskIdentitiesValid {
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"the benchmark result has duplicate or unknown task identities")
	}
	if !resultSummaryValid {
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"the benchmark result summary is not the exact final derivation of its task rows")
	}
	for _, task := range Suite() {
		attempt, exists := attempts[task.ID]
		if !exists {
			reason := failures[task.ID]
			if reason == "" {
				reason = "attempt was not started"
			}
			manifest.Missing = append(manifest.Missing, ReviewMissing{Case: task.ID, Reason: reason})
			continue
		}
		manifest.Attempts = append(manifest.Attempts, attempt)
		resultOutcome, found := resultTasks[task.ID]
		if !found || !reflect.DeepEqual(resultOutcome, attempt.Deterministic) {
			manifest.Missing = append(manifest.Missing, ReviewMissing{
				Case: task.ID, Reason: "retained deterministic outcome is not exactly equal to the suite result",
			})
		}
		if attempt.ReviewStatus != "complete" || attempt.SecondaryReview == nil {
			manifest.Missing = append(manifest.Missing, ReviewMissing{
				Case: task.ID, Reason: fmt.Sprintf(
					"secondary review evidence is incomplete (%s): %s",
					attempt.ReviewStatus, attempt.ReportabilityNote,
				),
			})
		}
		if attempt.EvaluationReceipt != nil {
			receipt := attempt.EvaluationReceipt
			if manifest.ReviewEvidenceScope == "" {
				manifest.ReviewEvidenceScope = receipt.EvidenceScope
				manifest.IndependentRemoteAttestation = receipt.IndependentRemoteAttestation
				manifest.ReviewAuthenticityCaveat = receipt.AuthenticityCaveat
			} else if manifest.ReviewEvidenceScope != receipt.EvidenceScope ||
				manifest.IndependentRemoteAttestation != receipt.IndependentRemoteAttestation ||
				manifest.ReviewAuthenticityCaveat != receipt.AuthenticityCaveat {
				manifest.Missing = append(manifest.Missing, ReviewMissing{
					Case: task.ID, Reason: "secondary review provenance scope differs across attempts",
				})
			}
		}
		if !attempt.Reportable {
			manifest.Reportable = false
			manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
				fmt.Sprintf("%s: %s", task.ID, attempt.ReportabilityNote))
		}
		receipt, retained := mediaReceipts[task.ID]
		verified, verifyErr := reviewmedia.VerifyBundle(receipt.Directory, receipt.ManifestSHA256)
		if !retained || verifyErr != nil || !reflect.DeepEqual(verified, receipt.Manifest) {
			manifest.Missing = append(manifest.Missing, ReviewMissing{
				Case: task.ID, Reason: "retained media failed final identity verification",
			})
			manifest.Reportable = false
		}
	}
	manifest.Complete = len(manifest.Missing) == 0 &&
		len(manifest.Attempts) == manifest.Expected
	if !manifest.Complete {
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"the review bundle is missing expected attempts")
	}
	markdown := renderMeetingReview(manifest)
	manifest.ReviewDocument = ReviewArtifact{
		Path: "REVIEW.md", SHA256: reviewDigest([]byte(markdown)),
		SizeBytes: int64(len(markdown)), MediaType: "text/markdown; charset=utf-8",
	}
	manifestPayload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || len(manifestPayload) > maximumReviewManifestBytes {
		return finish(errors.New("encode meeting review manifest"))
	}
	manifestPayload = append(manifestPayload, '\n')
	if containsReviewSecret(bundle.sensitive, manifestPayload) ||
		containsReviewSecret(bundle.sensitive, []byte(markdown)) {
		return finish(errors.New("meeting review bundle output contains a sensitive value"))
	}
	if err := writeMeetingReviewFile(root, "REVIEW.md", []byte(markdown)); err != nil {
		return finish(errors.New("write meeting review document"))
	}
	if err := writeMeetingReviewFile(root, "manifest.json", manifestPayload); err != nil {
		return finish(errors.New("write meeting review manifest commit marker"))
	}
	if !manifest.Complete {
		return finish(fmt.Errorf("meeting review bundle retained %d of %d attempts: %v",
			len(manifest.Attempts), manifest.Expected, manifest.Missing))
	}
	manifestSHA256 := reviewDigest(manifestPayload)
	if err := sealMeetingReviewTree(bundle.directory); err != nil {
		return finish(errors.New("seal meeting review bundle"))
	}
	if _, err := VerifyReviewBundle(bundle.directory, manifestSHA256); err != nil {
		return finish(fmt.Errorf("verify sealed meeting review bundle: %w", err))
	}
	bundle.mu.Lock()
	bundle.receipt = &ReviewBundleReceipt{
		Directory: bundle.directory, ManifestSHA256: manifestSHA256,
	}
	bundle.mu.Unlock()
	return finish(nil)
}

type reviewBundleAttempt struct {
	mu                sync.Mutex
	bundle            *ReviewBundle
	specification     EvidenceAttempt
	recorder          *reviewmedia.Recorder
	ordinal           int
	relativeDirectory string
	audioCapture      bench.SessionAudioCapture
	audioEndUS        int64
	latestVideoUS     int64
	audioCaptured     bool
	videoCaptured     bool
	terminal          bool
}

func (attempt *reviewBundleAttempt) CaptureAudio(capture bench.SessionAudioCapture) error {
	if attempt == nil || attempt.recorder == nil {
		return errors.New("meeting review attempt is nil")
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("meeting review attempt is already terminal")
	}
	if attempt.audioCaptured {
		return errors.New("meeting review attempt already captured audio")
	}
	endUS, err := alignedMeetingAudioEndUS(capture)
	if err != nil {
		return err
	}
	attempt.audioCapture = cloneMeetingAudioCapture(capture)
	attempt.audioEndUS = endUS
	attempt.audioCaptured = true
	return nil
}

func (attempt *reviewBundleAttempt) CaptureVideo(capture bench.SessionVideoCapture) error {
	if attempt == nil || attempt.recorder == nil {
		return errors.New("meeting review attempt is nil")
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("meeting review attempt is already terminal")
	}
	if nilInterface(attempt.bundle.videoFactory) {
		return nil
	}
	if err := attempt.recorder.CaptureVideo(capture); err != nil {
		return err
	}
	episodeUS := int64(math.Round(capture.EpisodeAtMS * 1000))
	if !attempt.videoCaptured || episodeUS > attempt.latestVideoUS {
		attempt.latestVideoUS = episodeUS
	}
	attempt.videoCaptured = true
	return nil
}

func (attempt *reviewBundleAttempt) Complete(
	ctx context.Context, completion EvidenceCompletion,
) error {
	if attempt == nil || attempt.recorder == nil {
		return errors.New("meeting review attempt is nil")
	}
	if ctx == nil {
		return errors.New("complete meeting review attempt: nil context")
	}
	attempt.mu.Lock()
	if attempt.terminal {
		attempt.mu.Unlock()
		return errors.New("meeting review attempt is already terminal")
	}
	attempt.terminal = true
	audioCaptured := attempt.audioCaptured
	audioCapture := cloneMeetingAudioCapture(attempt.audioCapture)
	audioEndUS, latestVideoUS, videoCaptured := attempt.audioEndUS, attempt.latestVideoUS, attempt.videoCaptured
	attempt.mu.Unlock()
	defer attempt.bundle.releaseAttempt()
	if !reflect.DeepEqual(completion.Attempt, attempt.specification) ||
		completion.Outcome.ID != attempt.specification.Case ||
		!meetingTranscriptExecutionMatchesOutcome(completion.Transcript, completion.Outcome) {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "terminal outcome identity drifted")
		return errors.New("meeting review completion identity drifted")
	}
	if !audioCaptured {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "required audio was not captured")
		return errors.New("meeting review attempt is missing required audio")
	}
	attemptEndUS, err := meetingAttemptEndUS(audioEndUS, latestVideoUS, videoCaptured)
	if err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "media timeline finalization failed")
		return err
	}
	audioCapture, err = padMeetingAudioCapture(audioCapture, attemptEndUS)
	if err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "time-aligned audio retention failed")
		return errors.New("retain time-aligned meeting review audio")
	}
	if err := attempt.recorder.CaptureAudio(audioCapture); err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "time-aligned audio retention failed")
		return errors.New("retain time-aligned meeting review audio")
	}
	receipt, err := attempt.recorder.Finalize(ctx, float64(attemptEndUS)/1000)
	if err != nil {
		attempt.bundle.recordFailure(attempt.specification.Case, "media finalization failed")
		return fmt.Errorf("finalize meeting review media: %w", err)
	}
	// Finalize returns only after the recorder has reopened and verified the
	// committed tree against this exact receipt. The anchored subtree snapshot
	// below cross-binds those bytes, and source publication independently runs
	// VerifyBundle again before any reviewer can observe them.
	verifiedManifest := receipt.Manifest
	mediaArtifacts, err := snapshotMeetingReviewSubtree(
		attempt.recorder.Directory(), attempt.relativeDirectory,
	)
	manifestArtifact := ReviewArtifact{
		Path:   filepath.ToSlash(filepath.Join(attempt.relativeDirectory, "manifest.json")),
		SHA256: receipt.ManifestSHA256, MediaType: "application/json",
	}
	for _, artifact := range mediaArtifacts {
		if artifact.Path == manifestArtifact.Path && artifact.SHA256 == manifestArtifact.SHA256 &&
			artifact.MediaType == manifestArtifact.MediaType {
			manifestArtifact.SizeBytes = artifact.SizeBytes
			break
		}
	}
	if err != nil || manifestArtifact.SizeBytes <= 0 ||
		!reviewArtifactsContain(mediaArtifacts, manifestArtifact) {
		attempt.bundle.recordFailure(attempt.specification.Case, "media artifact tree snapshot failed")
		return errors.New("snapshot finalized meeting review media tree")
	}
	media, err := retainedMeetingReviewMedia(verifiedManifest, mediaArtifacts, attempt.relativeDirectory)
	if err != nil {
		attempt.bundle.recordFailure(attempt.specification.Case, "retained media identity failed")
		return err
	}
	reviewAttempt := ReviewAttempt{
		Ordinal: attempt.ordinal, Case: attempt.specification.Case, Trial: 1,
		Deterministic: cloneTaskOutcome(completion.Outcome),
		Cell:          attempt.specification.Cell, Provenance: attempt.specification.Provenance,
		ExecutionRequirement: attempt.specification.ExecutionRequirement,
		MediaManifest:        manifestArtifact, Media: prefixMeetingReviewMedia(attempt.relativeDirectory, media),
		MediaArtifacts: mediaArtifacts, ReviewStatus: "pending", RunOrigin: attempt.specification.Origin,
		ExecutionStatus: meetingExecutionStatus(
			attempt.specification.ExecutionRequirement, completion.Outcome,
		),
	}
	reviewAttempt.VideoStatus = "audio-only-no-video-factory"
	if len(verifiedManifest.Video) != 0 {
		reviewAttempt.VideoStatus = "retained-playable-video"
	}
	reviewAttempt.Reportable, reviewAttempt.ReportabilityNote = meetingAttemptReportability(
		attempt.specification.Origin, attempt.specification.ExecutionRequirement, completion.Outcome,
		attempt.specification.Cell, len(verifiedManifest.Video) > 0,
	)
	mediaReceipt := retainedMeetingMediaReceipt{
		Directory: attempt.recorder.Directory(), ManifestSHA256: receipt.ManifestSHA256,
		Manifest: verifiedManifest,
	}
	attempt.bundle.recordPendingAttempt(reviewAttempt, pendingMeetingReview{
		Specification: cloneEvidenceAttempt(attempt.specification),
		Outcome:       cloneTaskOutcome(completion.Outcome), Transcript: cloneTranscript(completion.Transcript),
		Media: cloneReviewMedia(media), MediaArtifacts: slices.Clone(mediaArtifacts),
		MediaReceipt: mediaReceipt,
	})
	return nil
}

func (attempt *reviewBundleAttempt) Abort() error {
	if attempt == nil || attempt.recorder == nil {
		return nil
	}
	attempt.mu.Lock()
	alreadyTerminal := attempt.terminal
	attempt.terminal = true
	attempt.mu.Unlock()
	if alreadyTerminal {
		return nil
	}
	defer attempt.bundle.releaseAttempt()
	attempt.bundle.recordFailure(attempt.specification.Case, "attempt aborted before evidence completion")
	return attempt.recorder.Abort()
}

type meetingReviewContext struct {
	Format               string                     `json:"format"`
	Version              int                        `json:"version"`
	Suite                string                     `json:"suite"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	ResultSHA256         string                     `json:"result_sha256"`
	Task                 Task                       `json:"task"`
	Cell                 bench.Cell                 `json:"cell"`
	Provenance           bench.Provenance           `json:"provenance"`
	RunOrigin            EvidenceRunOrigin          `json:"run_origin"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
	Outcome              bench.TaskOutcome          `json:"deterministic_outcome"`
	Transcript           bench.Transcript           `json:"transcript"`
}

func buildMeetingReviewContext(
	specification EvidenceAttempt, outcome bench.TaskOutcome, transcript bench.Transcript,
	result bench.Result, resultSHA256 string, secrets []string,
) ([]byte, meetingReviewContext, error) {
	contextValue := meetingReviewContext{
		Format: ReviewContextFormat, Version: ReviewContextVersion,
		Suite: SuiteName, Case: specification.Case, Trial: 1, ResultSHA256: resultSHA256,
		Task: specification.Task, Cell: result.Cell,
		Provenance: result.Provenance, RunOrigin: specification.Origin,
		ExecutionRequirement: specification.ExecutionRequirement,
		Outcome:              cloneTaskOutcome(outcome),
		Transcript:           cloneTranscript(transcript),
	}
	redactMeetingReviewContext(&contextValue, secrets)
	payload, err := json.MarshalIndent(contextValue, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) > maximumReviewContextBytes ||
		strictjson.Validate(payload) != nil || containsReviewSecret(secrets, payload) {
		return nil, meetingReviewContext{}, errors.New("meeting review context is invalid, oversized, or sensitive")
	}
	return append(payload, '\n'), contextValue, nil
}

func (bundle *ReviewBundle) publishDeterministicMeetingSource(
	ctx context.Context, root *os.Root, result bench.Result, resultPayload []byte,
	resultSHA256 string, attempts map[string]ReviewAttempt,
	pending map[string]pendingMeetingReview, failures map[string]string,
	resultTasks map[string]bench.TaskOutcome,
) (map[string]ReviewAttempt, ReviewSourceManifest, ReviewSourceReceipt, error) {
	if root == nil {
		return nil, ReviewSourceManifest{}, ReviewSourceReceipt{},
			errors.New("meeting review source has no owned bundle root")
	}
	sourceDirectory := filepath.Join(bundle.directory, reviewSourceDirectory)
	if recoveredAttempts, opened, found, recoverErr := bundle.recoverMeetingReviewSource(
		ctx, sourceDirectory, result, resultSHA256,
	); found || recoverErr != nil {
		if recoverErr != nil {
			return nil, ReviewSourceManifest{}, ReviewSourceReceipt{}, recoverErr
		}
		return recoveredAttempts, opened.Manifest, opened.Receipt, nil
	}
	resultArtifact := ReviewArtifact{
		Path:   filepath.ToSlash(filepath.Join(reviewSourceDirectory, "result.json")),
		SHA256: resultSHA256, SizeBytes: int64(len(resultPayload)), MediaType: "application/json",
	}
	if err := writeMeetingReviewFile(root, resultArtifact.Path, resultPayload); err != nil {
		return nil, ReviewSourceManifest{}, ReviewSourceReceipt{},
			errors.New("retain exact meeting source benchmark result")
	}
	manifest := ReviewSourceManifest{
		Format: ReviewSourceFormat, FormatVersion: ReviewSourceFormatVersion,
		Suite: SuiteName, Expected: ExpectedTasks(), Reportable: true,
		Result: ReviewArtifact{
			Path: "result.json", SHA256: resultArtifact.SHA256,
			SizeBytes: resultArtifact.SizeBytes, MediaType: resultArtifact.MediaType,
		},
		Cell: cloneMeetingCell(result.Cell), Provenance: result.Provenance,
	}
	if reportabilityErr := result.Reportable(); reportabilityErr != nil {
		manifest.Reportable = false
		manifest.CoreReportability = reportabilityErr.Error()
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"core benchmark reportability refused this exact result")
	} else {
		manifest.CoreReportable = true
	}
	updated := make(map[string]ReviewAttempt, len(attempts))
	for name, attempt := range attempts {
		updated[name] = cloneReviewAttempt(attempt)
	}
	for _, task := range Suite() {
		attempt, attemptOK := attempts[task.ID]
		input, pendingOK := pending[task.ID]
		outcome, resultOK := resultTasks[task.ID]
		if !attemptOK || !pendingOK || !resultOK || !reflect.DeepEqual(outcome, input.Outcome) {
			reason := failures[task.ID]
			if reason == "" {
				reason = "no exact terminal media/context evidence matched the deterministic row"
			}
			manifest.Missing = append(manifest.Missing, ReviewMissing{Case: task.ID, Reason: reason})
			continue
		}
		attempt.Cell = cloneMeetingCell(result.Cell)
		attempt.Provenance = result.Provenance
		attempt.ExecutionRequirement = cloneMeetingExecutionRequirement(input.Specification.ExecutionRequirement)
		attempt.ExecutionStatus = meetingExecutionStatus(input.Specification.ExecutionRequirement, outcome)
		attempt.Reportable, attempt.ReportabilityNote = meetingAttemptReportability(
			input.Specification.Origin, input.Specification.ExecutionRequirement, outcome,
			result.Cell, attempt.VideoStatus == "retained-playable-video",
		)
		contextPayload, _, err := buildMeetingReviewContext(
			input.Specification, outcome, input.Transcript, result, resultSHA256, bundle.sensitive,
		)
		if err != nil || !meetingAttemptRunMatchesResult(input.Specification, result) ||
			!meetingTranscriptExecutionMatchesOutcome(input.Transcript, outcome) {
			manifest.Missing = append(manifest.Missing, ReviewMissing{
				Case: task.ID, Reason: "terminal context/cell/provenance/execution evidence drifted",
			})
			continue
		}
		attempt.Context = ReviewArtifact{
			Path: filepath.ToSlash(filepath.Join(reviewSourceDirectory, "contexts", fmt.Sprintf(
				"%02d-%s-trial-01.json", attempt.Ordinal, meetingReviewSlug(task.ID),
			))),
			SHA256: reviewDigest(contextPayload), SizeBytes: int64(len(contextPayload)),
			MediaType: "application/json",
		}
		currentArtifacts, snapshotErr := snapshotMeetingReviewSubtree(
			input.MediaReceipt.Directory, filepath.ToSlash(filepath.Dir(attempt.MediaManifest.Path)),
		)
		if snapshotErr != nil || !reflect.DeepEqual(currentArtifacts, input.MediaArtifacts) {
			return nil, ReviewSourceManifest{}, ReviewSourceReceipt{},
				errors.New("exact meeting source media tree changed before publication")
		}
		preparedRequest, err := revieweval.PrepareContext(ctx, revieweval.Request{
			AttemptID: meetingReviewAttemptID(task.ID, resultSHA256),
			Suite:     SuiteName, Case: task.ID, Trial: attempt.Trial,
			RootDirectory: input.MediaReceipt.Directory, Context: slices.Clone(contextPayload),
			Media: input.MediaReceipt.Manifest.ReviewMedia(), SensitiveValues: slices.Clone(bundle.sensitive),
		})
		if err != nil || !meetingReviewContextsEqual(preparedRequest.Context, contextPayload) {
			manifest.Missing = append(manifest.Missing, ReviewMissing{
				Case: task.ID, Reason: "provider-neutral review request preparation failed before source publication",
			})
			continue
		}
		contextPayload = slices.Clone(preparedRequest.Context)
		attempt.Context.SHA256 = reviewDigest(contextPayload)
		attempt.Context.SizeBytes = int64(len(contextPayload))
		if err := writeMeetingReviewFile(root, attempt.Context.Path, contextPayload); err != nil {
			return nil, ReviewSourceManifest{}, ReviewSourceReceipt{},
				errors.New("retain exact meeting review source context")
		}
		sourceAttempt, err := reviewSourceAttempt(attempt)
		if err != nil {
			return nil, ReviewSourceManifest{}, ReviewSourceReceipt{}, err
		}
		manifest.Attempts = append(manifest.Attempts, sourceAttempt)
		updated[task.ID] = cloneReviewAttempt(attempt)
		if !attempt.Reportable {
			manifest.Reportable = false
			manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
				fmt.Sprintf("%s: %s", task.ID, attempt.ReportabilityNote))
		}
	}
	manifest.Complete = len(manifest.Attempts) == ExpectedTasks() && len(manifest.Missing) == 0 &&
		len(result.Tasks) == ExpectedTasks()
	if !manifest.Complete {
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"deterministic source population is diagnostic and incomplete")
	}
	staged, err := stageMeetingReviewSource(ctx, sourceDirectory, manifest)
	if err != nil {
		return nil, ReviewSourceManifest{}, ReviewSourceReceipt{}, err
	}
	opened, err := commitStagedMeetingReviewSource(
		ctx, sourceDirectory, staged.Receipt,
		FileReviewSourceReceiptAnchor{Path: bundle.sourceReceiptPath},
		meetingSourcePublicationOperations{beforePromotion: bundle.afterSourceReceiptPublication},
	)
	if err != nil {
		return nil, ReviewSourceManifest{}, ReviewSourceReceipt{}, err
	}
	return updated, opened.Manifest, opened.Receipt, nil
}

func (bundle *ReviewBundle) recoverMeetingReviewSource(
	ctx context.Context, sourceDirectory string, result bench.Result, resultSHA256 string,
) (map[string]ReviewAttempt, ReviewSourceBundle, bool, error) {
	root, _, err := openMeetingSourceRoot(sourceDirectory)
	if err != nil {
		return nil, ReviewSourceBundle{}, false, err
	}
	stageExists, finalExists, markerErr := meetingSourceMarkerState(root)
	closeErr := root.Close()
	if markerErr != nil || closeErr != nil {
		return nil, ReviewSourceBundle{}, false,
			errors.New("inspect recoverable meeting source markers")
	}
	_, receiptErr := os.Lstat(bundle.sourceReceiptPath)
	receiptExists := receiptErr == nil
	if receiptErr != nil && !os.IsNotExist(receiptErr) {
		return nil, ReviewSourceBundle{}, false, errors.New("inspect recoverable meeting source receipt")
	}
	if !stageExists && !finalExists {
		if receiptExists {
			return nil, ReviewSourceBundle{}, true,
				errors.New("meeting source receipt exists without a staged or final marker")
		}
		return nil, ReviewSourceBundle{}, false, nil
	}
	if !receiptExists {
		return nil, ReviewSourceBundle{}, true,
			errors.New("meeting source marker exists without a durable external receipt")
	}
	receipt, err := ReadReviewSourceReceipt(ctx, bundle.sourceReceiptPath)
	if err != nil || receipt.Directory != sourceDirectory || receipt.ResultSHA256 != resultSHA256 {
		return nil, ReviewSourceBundle{}, true,
			errors.New("recoverable meeting source receipt differs from the exact result")
	}
	opened, err := commitStagedMeetingReviewSource(
		ctx, sourceDirectory, receipt,
		FileReviewSourceReceiptAnchor{Path: bundle.sourceReceiptPath},
		meetingSourcePublicationOperations{},
	)
	if err != nil || !reflect.DeepEqual(opened.Result, result) {
		return nil, ReviewSourceBundle{}, true,
			errors.New("recover staged meeting source for the exact result")
	}
	attempts := make(map[string]ReviewAttempt, len(opened.Manifest.Attempts))
	for _, sourceAttempt := range opened.Manifest.Attempts {
		attempts[sourceAttempt.Case] = prefixMeetingSourceAttempt(sourceAttempt)
	}
	return attempts, opened, true, nil
}

func (bundle *ReviewBundle) prepareMeetingReviews(
	ctx context.Context, result bench.Result, resultSHA256 string,
	attempts map[string]ReviewAttempt, pending map[string]pendingMeetingReview,
	resultTasks map[string]bench.TaskOutcome,
) (map[string]preparedMeetingReview, error) {
	prepared := make(map[string]preparedMeetingReview, len(pending))
	var evaluationFailures error
	for _, task := range Suite() {
		attempt, attemptOK := attempts[task.ID]
		input, pendingOK := pending[task.ID]
		outcome, resultOK := resultTasks[task.ID]
		if !attemptOK || !pendingOK || !resultOK || !reflect.DeepEqual(outcome, input.Outcome) {
			continue
		}
		if attempt.ReviewStatus == "complete" && attempt.EvaluationReceipt != nil {
			continue
		}
		attempt.Cell = result.Cell
		attempt.Provenance = result.Provenance
		attempt.ExecutionRequirement = input.Specification.ExecutionRequirement
		attempt.ExecutionStatus = meetingExecutionStatus(
			input.Specification.ExecutionRequirement, outcome,
		)
		attempt.Reportable, attempt.ReportabilityNote = meetingAttemptReportability(
			input.Specification.Origin, input.Specification.ExecutionRequirement, outcome,
			result.Cell, attempt.VideoStatus == "retained-playable-video",
		)
		contextPayload, _, err := buildMeetingReviewContext(
			input.Specification, outcome, input.Transcript, result, resultSHA256, bundle.sensitive,
		)
		if err != nil || !meetingAttemptRunMatchesResult(input.Specification, result) {
			attempt = failedMeetingReviewAttempt(attempt, "reviewed run identity differs from the final result")
			prepared[task.ID] = preparedMeetingReview{Attempt: attempt, Pending: input.clone()}
			continue
		}
		contextPath := filepath.ToSlash(filepath.Join(reviewSourceDirectory, "contexts", fmt.Sprintf(
			"%02d-%s-trial-01.json", attempt.Ordinal, meetingReviewSlug(task.ID),
		)))
		attempt.Context = ReviewArtifact{
			Path: contextPath, SHA256: reviewDigest(contextPayload), SizeBytes: int64(len(contextPayload)),
			MediaType: "application/json",
		}
		retainedContext, _, contextErr := readSealedMeetingReviewFile(
			filepath.Join(bundle.directory, filepath.FromSlash(contextPath)),
		)
		if contextErr != nil || !meetingReviewContextsEqual(retainedContext, contextPayload) {
			attempt = failedMeetingReviewAttempt(attempt, "sealed source context changed before secondary review")
			prepared[task.ID] = preparedMeetingReview{Attempt: attempt, Pending: input.clone()}
			evaluationFailures = errors.Join(evaluationFailures,
				errors.New("sealed meeting source context failed verification"))
			continue
		}
		contextPayload = retainedContext
		attempt.Context.SHA256 = reviewDigest(contextPayload)
		attempt.Context.SizeBytes = int64(len(contextPayload))
		verified, verifyErr := reviewmedia.VerifyBundle(
			input.MediaReceipt.Directory, input.MediaReceipt.ManifestSHA256,
		)
		currentArtifacts, snapshotErr := snapshotMeetingReviewSubtree(
			input.MediaReceipt.Directory, filepath.ToSlash(filepath.Dir(attempt.MediaManifest.Path)),
		)
		if verifyErr != nil || !reflect.DeepEqual(verified, input.MediaReceipt.Manifest) ||
			snapshotErr != nil || !reflect.DeepEqual(currentArtifacts, input.MediaArtifacts) {
			attempt = failedMeetingReviewAttempt(attempt, "source media changed before secondary review")
			prepared[task.ID] = preparedMeetingReview{
				Attempt: attempt, Pending: input.clone(), ContextPayload: contextPayload,
			}
			continue
		}
		request := revieweval.Request{
			AttemptID: meetingReviewAttemptID(task.ID, resultSHA256),
			Suite:     SuiteName, Case: task.ID, Trial: attempt.Trial,
			RootDirectory: input.MediaReceipt.Directory, Context: slices.Clone(contextPayload),
			Media: verified.ReviewMedia(), SensitiveValues: slices.Clone(bundle.sensitive),
		}
		adopted, found, publication, adoptErr := bundle.beginMeetingReviewEvaluation(
			ctx, attempt, input, request, verified,
		)
		if adoptErr != nil {
			attempt = failedMeetingReviewAttempt(attempt, "published secondary review could not be adopted")
			prepared[task.ID] = preparedMeetingReview{
				Attempt: attempt, Pending: input.clone(), ContextPayload: contextPayload,
			}
			evaluationFailures = errors.Join(evaluationFailures,
				fmt.Errorf("%s: adopt published secondary review: %w", task.ID, adoptErr))
			continue
		}
		if found {
			prepared[task.ID] = preparedMeetingReview{
				Attempt: adopted, Pending: input.clone(), ContextPayload: contextPayload,
			}
			continue
		}
		evaluation, reviewErr := revieweval.Evaluate(ctx, bundle.reviewer, request)
		if cause := context.Cause(ctx); cause != nil {
			return prepared, errors.Join(cause, closeMeetingEvaluationPublication(publication))
		}
		inputErr := verifyMeetingEvaluationInputs(evaluation, contextPayload, verified.ReviewMedia())
		failureReason := ""
		switch {
		case reviewErr != nil:
			failureReason = "secondary review invocation failed"
		case evaluation.Record.Provider != bundle.reviewerID ||
			evaluation.Record.AttemptID != request.AttemptID || evaluation.Record.Suite != request.Suite ||
			evaluation.Record.Case != request.Case || evaluation.Record.Trial != request.Trial:
			failureReason = "secondary review identity differs from its exact request"
		case meetingAssessmentBeyondMedia(evaluation.Record.Assessment, verified.AttemptEndUS):
			failureReason = "secondary review timeline exceeds retained media"
		case inputErr != nil:
			failureReason = inputErr.Error()
		}
		if failureReason != "" {
			closeErr := closeMeetingEvaluationPublication(publication)
			attempt = failedMeetingReviewAttempt(attempt, failureReason)
			prepared[task.ID] = preparedMeetingReview{
				Attempt: attempt, Pending: input.clone(), ContextPayload: contextPayload,
			}
			evaluationFailures = errors.Join(evaluationFailures,
				fmt.Errorf("%s: %s", task.ID, failureReason), closeErr)
			continue
		}
		contextPayload = slices.Clone(evaluation.Context)
		attempt.Context.SHA256 = reviewDigest(contextPayload)
		attempt.Context.SizeBytes = int64(len(contextPayload))
		postReview, verifyErr := reviewmedia.VerifyBundle(
			input.MediaReceipt.Directory, input.MediaReceipt.ManifestSHA256,
		)
		postArtifacts, snapshotErr := snapshotMeetingReviewSubtree(
			input.MediaReceipt.Directory, filepath.ToSlash(filepath.Dir(attempt.MediaManifest.Path)),
		)
		if verifyErr != nil || !reflect.DeepEqual(postReview, verified) || snapshotErr != nil ||
			!reflect.DeepEqual(postArtifacts, input.MediaArtifacts) {
			closeErr := closeMeetingEvaluationPublication(publication)
			attempt = failedMeetingReviewAttempt(attempt, "source media changed during secondary review")
			prepared[task.ID] = preparedMeetingReview{
				Attempt: attempt, Pending: input.clone(), ContextPayload: contextPayload,
			}
			evaluationFailures = errors.Join(evaluationFailures,
				fmt.Errorf("%s: source media changed during secondary review", task.ID), closeErr)
			continue
		}
		evaluationCopy := evaluation
		prepared[task.ID] = preparedMeetingReview{
			Attempt: attempt, Pending: input.clone(), ContextPayload: contextPayload,
			Evaluation: &evaluationCopy, Publication: publication,
		}
	}
	return prepared, evaluationFailures
}

func meetingAttemptRunMatchesResult(specification EvidenceAttempt, result bench.Result) bool {
	if !reflect.DeepEqual(specification.Cell, result.Cell) ||
		!reflect.DeepEqual(specification.ExecutionRequirement, result.Cell.Execution) {
		return false
	}
	initial := result.Provenance
	initial.FinishedAt = ""
	return reflect.DeepEqual(specification.Provenance, initial)
}

func meetingResultSummaryIsFinal(result bench.Result) bool {
	probe := result
	probe.Summary = bench.Summary{}
	probe.Finish()
	want, wantErr := json.Marshal(probe.Summary)
	actual, actualErr := json.Marshal(result.Summary)
	return wantErr == nil && actualErr == nil && bytes.Equal(want, actual)
}

func failedMeetingReviewAttempt(attempt ReviewAttempt, reason string) ReviewAttempt {
	attempt.ReviewStatus = "failed"
	attempt.Reportable = false
	attempt.ReportabilityNote = appendReportabilityNote(attempt.ReportabilityNote, reason)
	return attempt
}

func meetingReviewAttemptID(caseID, resultSHA256 string) string {
	// ResultSHA256 covers the final cell, provenance, exact deterministic row,
	// and summary. The shared request fingerprint independently covers the
	// canonical review context and media, so the attempt identity remains fixed
	// even when review.Prepare normalizes whitespace in that context.
	return fmt.Sprintf("%s/%s/1/%s", SuiteName, caseID,
		strings.TrimPrefix(resultSHA256, "sha256:"))
}

func verifyMeetingEvaluationInputs(
	evaluation revieweval.Evaluation, contextPayload []byte, media []revieweval.Media,
) error {
	if !meetingReviewContextsEqual(evaluation.Context, contextPayload) ||
		evaluation.Record.ContextSHA256 != reviewDigest(evaluation.Context) ||
		len(evaluation.Record.Media) != len(media) || len(evaluation.Media) != len(media) {
		return errors.New("secondary review context or media count differs from its exact input")
	}
	for index, expected := range media {
		item := evaluation.Media[index]
		if evaluation.Record.Media[index] != item.Media || item.SizeBytes != int64(len(item.Bytes)) ||
			item.Kind != expected.Kind || item.Role != expected.Role || item.Path != expected.Path ||
			item.SHA256 != expected.SHA256 || item.MediaType != expected.MediaType ||
			item.Validation != expected.Validation || reviewDigest(item.Bytes) != expected.SHA256 {
			return errors.New("secondary review media identity differs from source media")
		}
	}
	return nil
}

func meetingReviewContextsEqual(left, right []byte) bool {
	leftValue, leftErr := decodeMeetingReviewContext(left)
	rightValue, rightErr := decodeMeetingReviewContext(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftValue, rightValue)
}

func decodeMeetingReviewContext(payload []byte) (meetingReviewContext, error) {
	if strictjson.Validate(payload) != nil {
		return meetingReviewContext{}, errors.New("meeting review context is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value meetingReviewContext
	if err := decoder.Decode(&value); err != nil {
		return meetingReviewContext{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return meetingReviewContext{}, errors.New("meeting review context has trailing JSON")
	}
	return value, nil
}

func (bundle *ReviewBundle) writeReviewEvaluation(
	ctx context.Context, ordinal int, caseID string, evaluation revieweval.Evaluation,
	publication *revieweval.EvaluationBundlePublication,
) (ReviewArtifact, ReviewArtifact, ReviewEvaluationReceipt, []ReviewArtifact, error) {
	_, directory, ok := meetingAttemptArtifactNamespaces(ordinal, caseID)
	if !ok || publication == nil {
		return ReviewArtifact{}, ReviewArtifact{}, ReviewEvaluationReceipt{}, nil,
			errors.New("meeting reviewer evaluation namespace is invalid")
	}
	receipt, err := publication.Write(ctx, evaluation)
	if err != nil {
		return ReviewArtifact{}, ReviewArtifact{}, ReviewEvaluationReceipt{}, nil,
			fmt.Errorf("durably publish sealed meeting reviewer evaluation: %w", err)
	}
	manifest, manifestPayload, err := readPublishedMeetingEvaluationManifest(ctx, receipt)
	if err != nil || manifest.AttemptID != evaluation.Record.AttemptID ||
		manifest.Suite != evaluation.Record.Suite || manifest.Case != evaluation.Record.Case ||
		manifest.Trial != evaluation.Record.Trial || manifest.RecordSHA256 != receipt.RecordSHA256 ||
		manifest.FileSetSHA256 != receipt.FileSetSHA256 {
		return ReviewArtifact{}, ReviewArtifact{}, ReviewEvaluationReceipt{}, nil,
			errors.New("index durably published meeting reviewer evaluation")
	}
	return meetingEvaluationArtifacts(directory, receipt, manifest, manifestPayload)
}

func readPublishedMeetingEvaluationManifest(
	ctx context.Context, receipt revieweval.EvaluationBundleReceipt,
) (revieweval.EvaluationBundleManifest, []byte, error) {
	manifestPayload, _, err := readMeetingReviewFileContext(
		ctx, filepath.Join(receipt.Directory, "manifest.json"), false,
	)
	if err != nil || reviewDigest(manifestPayload) != receipt.ManifestSHA256 ||
		strictjson.Validate(manifestPayload) != nil {
		return revieweval.EvaluationBundleManifest{}, nil,
			errors.New("sealed meeting reviewer manifest differs from its receipt")
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestPayload))
	decoder.DisallowUnknownFields()
	var manifest revieweval.EvaluationBundleManifest
	if err := decoder.Decode(&manifest); err != nil {
		return revieweval.EvaluationBundleManifest{}, nil,
			errors.New("decode sealed meeting reviewer manifest")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return revieweval.EvaluationBundleManifest{}, nil,
			errors.New("sealed meeting reviewer manifest has trailing data")
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil || !bytes.Equal(canonical.Bytes(), manifestPayload) ||
		manifest.Format != revieweval.EvaluationBundleFormat ||
		manifest.FormatVersion != revieweval.EvaluationBundleFormatVersion || !manifest.Complete ||
		manifest.RecordSHA256 != receipt.RecordSHA256 ||
		manifest.FileSetSHA256 != receipt.FileSetSHA256 || len(manifest.Files) == 0 {
		return revieweval.EvaluationBundleManifest{}, nil,
			errors.New("sealed meeting reviewer manifest is noncanonical or invalid")
	}
	return manifest, manifestPayload, nil
}

func meetingEvaluationArtifacts(
	directory string, receipt revieweval.EvaluationBundleReceipt,
	manifest revieweval.EvaluationBundleManifest, manifestPayload []byte,
) (ReviewArtifact, ReviewArtifact, ReviewEvaluationReceipt, []ReviewArtifact, error) {
	manifestArtifact := ReviewArtifact{
		Path:   filepath.ToSlash(filepath.Join(directory, "manifest.json")),
		SHA256: receipt.ManifestSHA256, SizeBytes: int64(len(manifestPayload)), MediaType: "application/json",
	}
	artifacts := make([]ReviewArtifact, 0, len(manifest.Files)+1)
	artifacts = append(artifacts, manifestArtifact)
	var recordArtifact ReviewArtifact
	for _, file := range manifest.Files {
		artifact := ReviewArtifact{
			Path:   filepath.ToSlash(filepath.Join(directory, file.Path)),
			SHA256: file.SHA256, SizeBytes: file.SizeBytes,
			MediaType: evaluationBundleFileMediaType(file),
		}
		artifacts = append(artifacts, artifact)
		if file.Purpose == "record" {
			recordArtifact = artifact
		}
	}
	if recordArtifact.Path == "" {
		return ReviewArtifact{}, ReviewArtifact{}, ReviewEvaluationReceipt{}, nil,
			errors.New("sealed meeting reviewer evaluation has no exact record")
	}
	projection := ReviewEvaluationReceipt{
		Directory: directory, ManifestSHA256: receipt.ManifestSHA256,
		RecordSHA256: receipt.RecordSHA256, FileSetSHA256: receipt.FileSetSHA256,
		ReceiptSHA256: receipt.ReceiptSHA256, EvidenceScope: manifest.EvidenceScope,
		IndependentRemoteAttestation: manifest.IndependentRemoteAttestation,
		AuthenticityCaveat:           manifest.AuthenticityCaveat,
	}
	return recordArtifact, manifestArtifact, projection, artifacts, nil
}

func (bundle *ReviewBundle) beginMeetingReviewEvaluation(
	ctx context.Context, attempt ReviewAttempt, pending pendingMeetingReview,
	request revieweval.Request, media reviewmedia.Manifest,
) (ReviewAttempt, bool, *revieweval.EvaluationBundlePublication, error) {
	receiptPath, err := bundle.externalEvaluationReceiptPath(attempt.Ordinal, attempt.Case)
	if err != nil {
		return ReviewAttempt{}, false, nil, err
	}
	_, relativeDirectory, ok := meetingAttemptArtifactNamespaces(attempt.Ordinal, attempt.Case)
	expectedDirectory := filepath.Join(bundle.directory, filepath.FromSlash(relativeDirectory))
	if !ok {
		return ReviewAttempt{}, false, nil, errors.New("meeting reviewer evaluation namespace is invalid")
	}
	options := revieweval.EvaluationBundleOptions{
		Directory: expectedDirectory, SensitiveValues: slices.Clone(bundle.sensitive),
	}
	publication, preparation, err := revieweval.BeginEvaluationBundlePublication(
		ctx, revieweval.EvaluationBundlePublicationConfig{
			Bundle:              options,
			ReceiptStore:        revieweval.FileEvaluationBundleReceiptStore{Path: receiptPath},
			QuarantineDirectory: bundle.evaluationQuarantineDirectory,
		},
	)
	if err != nil {
		return ReviewAttempt{}, false, nil, err
	}
	if preparation.Recovered == nil {
		return ReviewAttempt{}, false, publication, nil
	}
	closeWith := func(cause error) error {
		return errors.Join(cause, closeMeetingEvaluationPublication(publication))
	}
	opened := *preparation.Recovered
	external := opened.Receipt
	record := opened.Record
	if record.Provider != bundle.reviewerID || record.AttemptID != request.AttemptID ||
		record.Suite != request.Suite || record.Case != request.Case || record.Trial != request.Trial ||
		record.ContextSHA256 != attempt.Context.SHA256 ||
		meetingAssessmentBeyondMedia(record.Assessment, media.AttemptEndUS) ||
		!meetingReviewRecordMediaMatches(record.Media, media.ReviewMedia(), pending.Media) {
		return ReviewAttempt{}, false, nil, closeWith(
			errors.New("published meeting reviewer evaluation differs from sealed source inputs"))
	}
	manifest, manifestPayload, err := readPublishedMeetingEvaluationManifest(ctx, external)
	if err != nil || !reflect.DeepEqual(manifest, opened.Manifest) {
		return ReviewAttempt{}, false, nil, closeWith(
			errors.New("recovered meeting reviewer manifest changed"))
	}
	recordArtifact, manifestArtifact, projection, artifacts, err :=
		meetingEvaluationArtifacts(relativeDirectory, external, manifest, manifestPayload)
	if err != nil {
		return ReviewAttempt{}, false, nil, closeWith(err)
	}
	result := cloneReviewAttempt(attempt)
	response := meetingReviewResponse(record)
	result.SecondaryReview = &recordArtifact
	result.EvaluationManifest = &manifestArtifact
	result.EvaluationReceipt = &projection
	result.ReviewArtifacts = artifacts
	result.Assessment = &response
	result.ReviewStatus = "complete"
	if err := closeMeetingEvaluationPublication(publication); err != nil {
		return ReviewAttempt{}, false, nil, err
	}
	return result, true, nil, nil
}

func closeMeetingEvaluationPublication(
	publication *revieweval.EvaluationBundlePublication,
) error {
	if publication == nil {
		return nil
	}
	if err := publication.Close(); err != nil {
		return errors.New("close meeting evaluation publication lease")
	}
	return nil
}

func evaluationBundleFileMediaType(file revieweval.EvaluationBundleFile) string {
	if file.MediaType != "" {
		return file.MediaType
	}
	switch file.Purpose {
	case "record", "provider_configuration", "schema", "context", "normalized_output":
		return "application/json"
	case "prompt":
		return "text/plain; charset=utf-8"
	case "human_review":
		return "text/markdown; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func (bundle *ReviewBundle) publishPreparedMeetingReview(
	ctx context.Context, root *os.Root, prepared preparedMeetingReview,
) (result ReviewAttempt, resultErr error) {
	_ = root // retained for call-site compatibility while evaluations remain siblings of source
	attempt := cloneReviewAttempt(prepared.Attempt)
	if prepared.Publication != nil {
		defer func() {
			if closeErr := closeMeetingEvaluationPublication(prepared.Publication); closeErr != nil {
				result = failedMeetingReviewAttempt(attempt,
					"meeting evaluation publication lease close failed")
				resultErr = errors.Join(resultErr, closeErr)
			}
		}()
	}
	if len(prepared.ContextPayload) != 0 {
		retained, _, err := readSealedMeetingReviewFile(filepath.Join(
			bundle.directory, filepath.FromSlash(attempt.Context.Path),
		))
		if attempt.Context.Path == "" || reviewDigest(prepared.ContextPayload) != attempt.Context.SHA256 ||
			int64(len(prepared.ContextPayload)) != attempt.Context.SizeBytes || err != nil ||
			!bytes.Equal(retained, prepared.ContextPayload) {
			return failedMeetingReviewAttempt(attempt, "exact sealed source context verification failed"),
				errors.New("verify exact meeting review source context")
		}
	}
	if prepared.Evaluation == nil {
		return attempt, nil
	}
	recordArtifact, evaluationManifest, evaluationReceipt, reviewArtifacts, err :=
		bundle.writeReviewEvaluation(
			ctx, attempt.Ordinal, attempt.Case, *prepared.Evaluation, prepared.Publication,
		)
	if err != nil {
		return failedMeetingReviewAttempt(attempt, err.Error()), err
	}
	if bundle.afterEvaluationPublication != nil {
		if err := bundle.afterEvaluationPublication(attempt.Case); err != nil {
			return failedMeetingReviewAttempt(attempt, "fault after sealed evaluation publication"), err
		}
	}
	verifiedMedia, verifyErr := reviewmedia.VerifyBundle(
		prepared.Pending.MediaReceipt.Directory, prepared.Pending.MediaReceipt.ManifestSHA256,
	)
	mediaArtifacts, snapshotErr := snapshotMeetingReviewSubtree(
		prepared.Pending.MediaReceipt.Directory,
		filepath.ToSlash(filepath.Dir(attempt.MediaManifest.Path)),
	)
	if verifyErr != nil || !reflect.DeepEqual(verifiedMedia, prepared.Pending.MediaReceipt.Manifest) ||
		snapshotErr != nil || !reflect.DeepEqual(mediaArtifacts, prepared.Pending.MediaArtifacts) {
		return failedMeetingReviewAttempt(attempt, "source media changed before review commit"),
			errors.New("verify source media before meeting review commit")
	}
	responseCopy := meetingReviewResponse(prepared.Evaluation.Record)
	attempt.SecondaryReview = &recordArtifact
	attempt.EvaluationManifest = &evaluationManifest
	attempt.EvaluationReceipt = &evaluationReceipt
	attempt.ReviewArtifacts = reviewArtifacts
	attempt.Assessment = &responseCopy
	attempt.ReviewStatus = "complete"
	return attempt, nil
}

func meetingReviewResponse(record revieweval.Record) ReviewResponse {
	assessment := record.Assessment
	return ReviewResponse{
		Reviewer: record.Provider, RequestSHA256: record.RequestFingerprint,
		MediaUsable: assessment.MediaUsable, ObservedOutcome: assessment.ObservedOutcome,
		AgreesWithDeterministic: assessment.AgreesWithDeterministic,
		Confidence:              assessment.Confidence,
		Summary:                 assessment.Summary,
		SignificantProblems:     cloneReviewFindings(assessment.SignificantProblems),
		MinorObservations:       cloneReviewFindings(assessment.MinorObservations),
		Limitations:             slices.Clone(assessment.Limitations),
	}
}

func meetingAssessmentBeyondMedia(assessment revieweval.Assessment, attemptEndUS int64) bool {
	if attemptEndUS <= 0 {
		return true
	}
	for _, findings := range [][]revieweval.Finding{
		assessment.SignificantProblems, assessment.MinorObservations,
	} {
		if findings == nil {
			return true
		}
		for _, finding := range findings {
			if finding.StartMS != nil && meetingMillisecondBeyondUS(*finding.StartMS, attemptEndUS) ||
				finding.EndMS != nil && meetingMillisecondBeyondUS(*finding.EndMS, attemptEndUS) {
				return true
			}
		}
	}
	return assessment.Limitations == nil
}

func meetingMillisecondBeyondUS(milliseconds, attemptEndUS int64) bool {
	return milliseconds < 0 || milliseconds > math.MaxInt64/1000 || milliseconds*1000 > attemptEndUS
}

func cloneReviewFindings(source []revieweval.Finding) []ReviewFinding {
	result := slices.Clone(source)
	for index := range result {
		if source[index].StartMS != nil {
			value := *source[index].StartMS
			result[index].StartMS = &value
		}
		if source[index].EndMS != nil {
			value := *source[index].EndMS
			result[index].EndMS = &value
		}
	}
	return result
}

func (bundle *ReviewBundle) recordPendingAttempt(
	attempt ReviewAttempt, pending pendingMeetingReview,
) {
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	bundle.attempts[attempt.Case] = cloneReviewAttempt(attempt)
	bundle.pending[attempt.Case] = pending.clone()
	bundle.media[attempt.Case] = pending.MediaReceipt.clone()
	delete(bundle.failures, attempt.Case)
}

func snapshotMeetingReviewSubtree(directory, relativeDirectory string) ([]ReviewArtifact, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == filepath.Dir(absolute) {
		return nil, errors.New("snapshot meeting media subtree path is invalid")
	}
	root, rootInfo, err := openMeetingSourceRoot(absolute)
	if err != nil {
		return nil, errors.New("open exact meeting media subtree")
	}
	defer root.Close()
	var artifacts []ReviewArtifact
	identities := make([]os.FileInfo, 0, 16)
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		path = filepath.ToSlash(path)
		info, err := root.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 {
			return errors.New("meeting media tree contains a symlink or writable node")
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("meeting media tree contains a non-regular artifact")
		}
		for _, previous := range identities {
			if os.SameFile(previous, info) {
				return errors.New("meeting media tree contains duplicate hard-linked identities")
			}
		}
		identities = append(identities, info)
		payload, err := readMeetingSourceFile(context.Background(), root, ReviewArtifact{
			Path: path, SizeBytes: info.Size(), MediaType: meetingArtifactMediaType(path),
		}, true)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, ReviewArtifact{
			Path:   filepath.ToSlash(filepath.Join(relativeDirectory, path)),
			SHA256: reviewDigest(payload), SizeBytes: int64(len(payload)),
			MediaType: meetingArtifactMediaType(path),
		})
		return nil
	})
	if err != nil || len(artifacts) == 0 ||
		verifyMeetingSourceRootIdentity(absolute, root, rootInfo) != nil {
		return nil, errors.New("snapshot exact meeting media subtree")
	}
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].Path < artifacts[right].Path })
	return artifacts, nil
}

func reviewArtifactsContain(artifacts []ReviewArtifact, want ReviewArtifact) bool {
	for _, artifact := range artifacts {
		if artifact == want {
			return true
		}
	}
	return false
}

func meetingArtifactMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "application/json"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".wav":
		return "audio/wav"
	case ".mp4":
		return "video/mp4"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
}

func (bundle *ReviewBundle) recordFailure(caseID, reason string) {
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if _, complete := bundle.attempts[caseID]; !complete {
		bundle.failures[caseID] = reason
	}
}

func retainedMeetingReviewMedia(
	manifest reviewmedia.Manifest, artifacts []ReviewArtifact, relativeDirectory string,
) ([]ReviewMedia, error) {
	items := manifest.ReviewMedia()
	result := make([]ReviewMedia, 0, len(items))
	for _, item := range items {
		expectedPath := filepath.ToSlash(filepath.Join(relativeDirectory, item.Path))
		sizeBytes := int64(0)
		for _, artifact := range artifacts {
			if artifact.Path == expectedPath && artifact.SHA256 == item.SHA256 &&
				artifact.MediaType == item.MediaType {
				sizeBytes = artifact.SizeBytes
				break
			}
		}
		if sizeBytes <= 0 || !validReviewDigest(item.SHA256) {
			return nil, errors.New("retained meeting review media is missing")
		}
		result = append(result, ReviewMedia{
			Kind: item.Kind, Role: item.Role, Path: item.Path, SHA256: item.SHA256,
			SizeBytes: sizeBytes, MediaType: item.MediaType,
		})
	}
	if len(result) == 0 || result[0].Kind != "audio" || result[0].MediaType != "audio/wav" {
		return nil, errors.New("meeting review media has no playable WAV")
	}
	return result, nil
}

func alignedMeetingAudioEnd(capture bench.SessionAudioCapture) (float64, error) {
	microseconds, err := alignedMeetingAudioEndUS(capture)
	return float64(microseconds) / 1000, err
}

func alignedMeetingAudioEndUS(capture bench.SessionAudioCapture) (int64, error) {
	rate := capture.SampleRateHz
	if rate == 0 {
		rate = 24_000
	}
	if rate != 24_000 {
		return 0, errors.New("meeting review audio must use 24 kHz PCM")
	}
	frames := uint64(len(capture.RoomPCM16))
	for _, chunk := range capture.Agent {
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < 0 || len(chunk.PCM16) == 0 {
			return 0, errors.New("meeting review audio has an invalid agent chunk")
		}
		startFloat := math.Round(chunk.AtMS * float64(rate) / 1000)
		if startFloat < 0 || startFloat >= float64(math.MaxUint64) {
			return 0, errors.New("meeting review audio exceeds its timeline")
		}
		start := uint64(startFloat)
		if uint64(len(chunk.PCM16)) > math.MaxUint64-start {
			return 0, errors.New("meeting review audio exceeds its timeline")
		}
		frames = max(frames, start+uint64(len(chunk.PCM16)))
	}
	if frames == 0 || frames > math.MaxUint64/1_000_000 {
		return 0, errors.New("meeting review audio has no bounded samples")
	}
	microseconds := (frames*1_000_000 + uint64(rate) - 1) / uint64(rate)
	// 24 kHz sample endpoints are exactly representable in integer microseconds
	// every 125 us. Round upward so the declared end never truncates audio.
	microseconds = ((microseconds + 124) / 125) * 125
	if microseconds == 0 || microseconds >= uint64(math.MaxInt64) {
		return 0, errors.New("meeting review audio has an invalid terminal timestamp")
	}
	return int64(microseconds), nil
}

func meetingAttemptEndUS(audioEndUS, latestVideoUS int64, videoCaptured bool) (int64, error) {
	if audioEndUS <= 0 || audioEndUS%125 != 0 {
		return 0, errors.New("meeting review audio endpoint is not on the 24 kHz integer-microsecond boundary")
	}
	result := audioEndUS
	if videoCaptured {
		if latestVideoUS < 0 || latestVideoUS > math.MaxInt64-1_000 {
			return 0, errors.New("meeting review video endpoint is invalid")
		}
		// The terminal endpoint must be strictly after the last presentation
		// timestamp and must leave an encodable final-frame interval. The shared
		// FFmpeg plug-in requires at least 1ms while 24kHz WAV endpoints must land
		// on a 125us integer-microsecond boundary.
		strictlyAfter := ((latestVideoUS + 1_000 + 124) / 125) * 125
		result = max(result, strictlyAfter)
	}
	const maximumStereoFrames = ((128 << 20) - 44) / 4
	if result <= 0 || result > math.MaxInt64/24_000 ||
		result*24_000%1_000_000 != 0 || result*24_000/1_000_000 > maximumStereoFrames {
		return 0, errors.New("meeting review attempt endpoint exceeds the bounded audio timeline")
	}
	return result, nil
}

func padMeetingAudioCapture(
	capture bench.SessionAudioCapture, attemptEndUS int64,
) (bench.SessionAudioCapture, error) {
	result := cloneMeetingAudioCapture(capture)
	if attemptEndUS <= 0 || attemptEndUS > math.MaxInt64/24_000 ||
		attemptEndUS*24_000%1_000_000 != 0 {
		return bench.SessionAudioCapture{}, errors.New("meeting review padded audio endpoint is invalid")
	}
	frames := attemptEndUS * 24_000 / 1_000_000
	if frames < int64(len(result.RoomPCM16)) || frames > int64(int(^uint(0)>>1)) {
		return bench.SessionAudioCapture{}, errors.New("meeting review padded audio endpoint does not cover captured room audio")
	}
	if int64(len(result.RoomPCM16)) < frames {
		result.RoomPCM16 = append(result.RoomPCM16, make([]int16, int(frames)-len(result.RoomPCM16))...)
	}
	return result, nil
}

func cloneMeetingAudioCapture(source bench.SessionAudioCapture) bench.SessionAudioCapture {
	result := source
	result.RoomPCM16 = slices.Clone(source.RoomPCM16)
	result.Agent = slices.Clone(source.Agent)
	for index := range result.Agent {
		result.Agent[index].PCM16 = slices.Clone(source.Agent[index].PCM16)
	}
	return result
}

func meetingTranscriptExecutionMatchesOutcome(
	transcript bench.Transcript, outcome bench.TaskOutcome,
) bool {
	if transcript.ExecutionError != outcome.ExecutionError {
		return false
	}
	if (transcript.Execution == nil) != (outcome.Execution == nil) {
		return false
	}
	if transcript.Execution == nil {
		return true
	}
	if transcript.Execution.Scope != "" && transcript.Execution.Scope != outcome.ID {
		return false
	}
	return reflect.DeepEqual(transcript.Execution, outcome.Execution)
}

func meetingExecutionStatus(
	requirement bench.ExecutionRequirement, outcome bench.TaskOutcome,
) string {
	if !requirement.Required() {
		return "unattested"
	}
	if err := requirement.Match(outcome.Execution); err != nil {
		return "mismatch"
	}
	if requirement.Kind == bench.ExecutionGraphNative {
		return "graph-native-attested"
	}
	return "legacy-attested"
}

func meetingAttemptReportability(
	origin EvidenceRunOrigin, requirement bench.ExecutionRequirement, outcome bench.TaskOutcome,
	cell bench.Cell, fullyDecodedVideo bool,
) (bool, string) {
	if !outcome.Completed {
		return false, "the deterministic attempt did not complete"
	}
	if err := origin.validate(); err != nil || !origin.Live || origin.Kind != EvidenceOriginProduction {
		return false, "the attempt used a hermetic or invalid run origin, not the production shared Realtime path"
	}
	if !requirement.Required() {
		return false, "no reviewed execution requirement was supplied"
	}
	if err := requirement.Match(outcome.Execution); err != nil {
		return false, "execution evidence did not match the reviewed requirement"
	}
	if strings.Contains(strings.ToLower(cell.Levels[bench.FactorObservers]), "video") &&
		!fullyDecodedVideo {
		return false, "the reviewed audio+video cell retained no independently full-decoded screen video"
	}
	return true, ""
}

func renderMeetingReview(manifest ReviewManifest) string {
	var output strings.Builder
	output.WriteString("# OpenRealtime Meeting Assistant v1 review\n\n")
	fmt.Fprintf(&output, "Complete: %t (%d/%d attempts retained).\n\n",
		manifest.Complete, len(manifest.Attempts), manifest.Expected)
	if manifest.Reportable {
		output.WriteString("Behavioral reportable: yes; the core benchmark reportability gate accepted the exact result and every case traversed the production shared Realtime path with matching reviewed execution evidence.\n\n")
	} else {
		output.WriteString("Behavioral reportable: no. Media and deterministic failures remain reviewable.\n\n")
		for _, reason := range manifest.ReportabilityErrors {
			fmt.Fprintf(&output, "- Reportability: %s\n", meetingMarkdown(reason))
		}
		output.WriteByte('\n')
	}
	fmt.Fprintf(&output, "Exact benchmark result: [%s](%s), `%s`. Core reportable: `%t`. Source revision: `%s`; working tree modified: `%t`.\n\n",
		filepath.Base(manifest.Result.Path), manifest.Result.Path, manifest.Result.SHA256,
		manifest.CoreReportable, meetingMarkdown(manifest.Provenance.Revision), manifest.Provenance.Modified)
	fmt.Fprintf(&output, "Declared secondary reviewer plug-in identity: `%s` / `%s` (`%s`).\n\n",
		meetingMarkdown(manifest.Reviewer.Provider), meetingMarkdown(manifest.Reviewer.Model),
		manifest.Reviewer.Implementation.SHA256)
	fmt.Fprintf(&output, "Reviewer evidence scope: %s Independent remote-service attestation: `%t`.\n\n",
		meetingMarkdown(manifest.ReviewEvidenceScope), manifest.IndependentRemoteAttestation)
	fmt.Fprintf(&output, "Authenticity caveat: %s\n\n",
		meetingMarkdown(manifest.ReviewAuthenticityCaveat))
	output.WriteString("Audio is 24 kHz PCM16 stereo: scripted room/user input is the left channel and agent output is the right channel. Secondary review is advisory; the deterministic result below remains authoritative.\n\n")
	for _, attempt := range manifest.Attempts {
		status := "FAIL"
		if attempt.Deterministic.Passed {
			status = "PASS"
		} else if !attempt.Deterministic.Completed {
			status = "ERROR"
		}
		fmt.Fprintf(&output, "## %02d. %s — %s\n\n", attempt.Ordinal, meetingMarkdown(attempt.Case), status)
		for _, media := range attempt.Media {
			label := media.Kind
			if label != "" {
				label = strings.ToUpper(label[:1]) + label[1:]
			}
			fmt.Fprintf(&output, "- %s: [%s](%s), `%s`, %d bytes\n",
				label, meetingMarkdown(filepath.Base(media.Path)), media.Path,
				media.SHA256, media.SizeBytes)
		}
		fmt.Fprintf(&output, "- Deterministic completed/pass: `%t` / `%t`\n",
			attempt.Deterministic.Completed, attempt.Deterministic.Passed)
		fmt.Fprintf(&output, "- Execution evidence: `%s`\n", attempt.ExecutionStatus)
		fmt.Fprintf(&output, "- Video evidence: `%s`\n", attempt.VideoStatus)
		fmt.Fprintf(&output, "- Run origin: `%s` (live `%t`, transport `%s`, endpoint `%s`)\n",
			attempt.RunOrigin.Kind, attempt.RunOrigin.Live, attempt.RunOrigin.Transport,
			attempt.RunOrigin.EndpointSHA256)
		if attempt.Assessment != nil {
			fmt.Fprintf(&output, "- Secondary observed outcome: `%s` (agreement `%t`, confidence %.3f)\n",
				attempt.Assessment.ObservedOutcome, attempt.Assessment.AgreesWithDeterministic,
				attempt.Assessment.Confidence)
			fmt.Fprintf(&output, "- Secondary summary: %s\n", meetingMarkdown(attempt.Assessment.Summary))
			for _, finding := range attempt.Assessment.SignificantProblems {
				fmt.Fprintf(&output, "- Significant problem `%s`: %s — %s\n",
					meetingMarkdown(finding.Category), meetingMarkdown(finding.Evidence),
					meetingMarkdown(finding.Impact))
			}
		} else {
			fmt.Fprintf(&output, "- Secondary review: `%s`\n", attempt.ReviewStatus)
		}
		fmt.Fprintf(&output, "- Machine context: [%s](%s)\n",
			filepath.Base(attempt.Context.Path), attempt.Context.Path)
		if attempt.SecondaryReview != nil {
			fmt.Fprintf(&output, "- Machine review: [%s](%s)\n",
				filepath.Base(attempt.SecondaryReview.Path), attempt.SecondaryReview.Path)
		}
		output.WriteByte('\n')
	}
	for _, missing := range manifest.Missing {
		fmt.Fprintf(&output, "## %s — MISSING\n\n%s\n\n",
			meetingMarkdown(missing.Case), meetingMarkdown(missing.Reason))
	}
	return output.String()
}

// VerifyReviewBundle verifies a sealed complete bundle against a manifest
// digest retained outside the bundle. It rejects writable nodes, symlinks,
// unexpected files, artifact drift, reviewer-exchange drift, result drift,
// and media that no longer passes the media package's full verifier.
func VerifyReviewBundle(directory, expectedManifestSHA256 string) (ReviewManifest, error) {
	if !validReviewDigest(expectedManifestSHA256) {
		return ReviewManifest{}, errors.New("meeting review manifest digest is invalid")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil || filepath.Clean(directory) != directory {
		return ReviewManifest{}, errors.New("meeting review verification directory is invalid")
	}
	verificationRoot, verificationRootInfo, err := openMeetingSourceRoot(absolute)
	if err != nil {
		return ReviewManifest{}, errors.New("open exact meeting review root")
	}
	defer verificationRoot.Close()
	manifestInfo, err := meetingSourceFileInfo(verificationRoot, "manifest.json", true)
	if err != nil {
		return ReviewManifest{}, errors.New("meeting review manifest is unavailable")
	}
	manifestArtifact := ReviewArtifact{
		Path: "manifest.json", SizeBytes: manifestInfo.Size(), MediaType: "application/json",
	}
	manifestPayload, err := readMeetingSourceFile(
		context.Background(), verificationRoot, manifestArtifact, true,
	)
	if err != nil || reviewDigest(manifestPayload) != expectedManifestSHA256 ||
		strictjson.Validate(manifestPayload) != nil {
		return ReviewManifest{}, errors.New("meeting review manifest identity or JSON is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestPayload))
	decoder.DisallowUnknownFields()
	var manifest ReviewManifest
	if err := decoder.Decode(&manifest); err != nil {
		return ReviewManifest{}, errors.New("decode meeting review manifest")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ReviewManifest{}, errors.New("meeting review manifest has trailing JSON")
	}
	canonical, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ReviewManifest{}, errors.New("canonicalize meeting review manifest")
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(canonical, manifestPayload) || manifest.Format != ReviewBundleFormat ||
		manifest.FormatVersion != ReviewBundleFormatVersion || manifest.Suite != SuiteName ||
		manifest.Expected != ExpectedTasks() || !manifest.Complete ||
		len(manifest.Attempts) != ExpectedTasks() || len(manifest.Missing) != 0 {
		return ReviewManifest{}, errors.New("meeting review manifest is noncanonical or incomplete")
	}
	if manifest.SourceReceipt.Directory != reviewSourceDirectory ||
		manifest.SourceManifest.Path != filepath.ToSlash(filepath.Join(
			reviewSourceDirectory, reviewSourceManifestName,
		)) || manifest.SourceManifest.SHA256 != manifest.SourceReceipt.ManifestSHA256 {
		return ReviewManifest{}, errors.New("meeting review source receipt projection is invalid")
	}
	sourceDirectory := filepath.Join(absolute, reviewSourceDirectory)
	expectedSourceReceipt := manifest.SourceReceipt
	expectedSourceReceipt.Directory = sourceDirectory
	openedSource, err := VerifyMeetingReviewSource(
		context.Background(), sourceDirectory, expectedSourceReceipt,
	)
	if err != nil {
		return ReviewManifest{}, fmt.Errorf("verify independently sealed meeting review source: %w", err)
	}
	sourceManifestPayload, _, err := readSealedMeetingReviewFile(filepath.Join(
		sourceDirectory, reviewSourceManifestName,
	))
	if err != nil || int64(len(sourceManifestPayload)) != manifest.SourceManifest.SizeBytes ||
		reviewDigest(sourceManifestPayload) != manifest.SourceManifest.SHA256 {
		return ReviewManifest{}, errors.New("meeting review source manifest artifact differs from its receipt")
	}

	resultPayload, err := verifyMeetingReviewArtifact(absolute, manifest.Result)
	if err != nil {
		return ReviewManifest{}, errors.New("verify exact meeting benchmark result artifact")
	}
	resultDecoder := json.NewDecoder(bytes.NewReader(resultPayload))
	resultDecoder.DisallowUnknownFields()
	var result bench.Result
	if err := resultDecoder.Decode(&result); err != nil {
		return ReviewManifest{}, errors.New("decode exact meeting benchmark result")
	}
	if err := resultDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ReviewManifest{}, errors.New("exact meeting benchmark result has trailing JSON")
	}
	resultCanonical, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return ReviewManifest{}, errors.New("canonicalize exact meeting benchmark result")
	}
	resultCanonical = append(resultCanonical, '\n')
	if !bytes.Equal(resultCanonical, resultPayload) || !reflect.DeepEqual(result.Cell, manifest.Cell) ||
		!reflect.DeepEqual(result.Provenance, manifest.Provenance) || result.Suite != SuiteName ||
		result.Expected != ExpectedTasks() || len(result.Tasks) != ExpectedTasks() ||
		!meetingResultSummaryIsFinal(result) {
		return ReviewManifest{}, errors.New("exact result, cell, or provenance differs from the review index")
	}
	if !reflect.DeepEqual(openedSource.Result, result) ||
		openedSource.Receipt.ResultSHA256 != manifest.Result.SHA256 ||
		!reflect.DeepEqual(openedSource.Manifest.Cell, manifest.Cell) ||
		!reflect.DeepEqual(openedSource.Manifest.Provenance, manifest.Provenance) {
		return ReviewManifest{}, errors.New("sealed source result differs from the enclosing review index")
	}
	coreErr := result.Reportable()
	if manifest.CoreReportable != (coreErr == nil) ||
		(coreErr != nil && manifest.CoreReportability != coreErr.Error()) ||
		(coreErr == nil && manifest.CoreReportability != "") {
		return ReviewManifest{}, errors.New("core benchmark reportability differs from the review index")
	}
	reviewPayload, err := verifyMeetingReviewArtifact(absolute, manifest.ReviewDocument)
	if err != nil {
		return ReviewManifest{}, errors.New("verify meeting human review document")
	}
	if manifest.Reviewer.Validate() != nil {
		return ReviewManifest{}, errors.New("meeting review provider identity is invalid")
	}

	resultTasks := make(map[string]bench.TaskOutcome, len(result.Tasks))
	for _, outcome := range result.Tasks {
		if _, duplicate := resultTasks[outcome.ID]; duplicate {
			return ReviewManifest{}, errors.New("exact meeting result duplicates a task")
		}
		resultTasks[outcome.ID] = outcome
	}
	expectedFiles := map[string]ReviewArtifact{
		"manifest.json": {
			Path: "manifest.json", SHA256: expectedManifestSHA256,
			SizeBytes: int64(len(manifestPayload)), MediaType: "application/json",
		},
		manifest.SourceManifest.Path: manifest.SourceManifest,
		manifest.Result.Path:         manifest.Result,
		manifest.ReviewDocument.Path: manifest.ReviewDocument,
	}
	allAttemptsReportable := true
	for index, attempt := range manifest.Attempts {
		if index >= len(Suite()) || attempt.Ordinal != index+1 || attempt.Case != Suite()[index].ID ||
			attempt.Trial != 1 || !reflect.DeepEqual(resultTasks[attempt.Case], attempt.Deterministic) ||
			attempt.SecondaryReview == nil || attempt.EvaluationManifest == nil ||
			attempt.EvaluationReceipt == nil || attempt.ReviewStatus != "complete" ||
			len(attempt.ReviewArtifacts) < 12 || attempt.Assessment == nil ||
			!reflect.DeepEqual(attempt.Cell, result.Cell) ||
			!reflect.DeepEqual(attempt.Provenance, result.Provenance) ||
			!reflect.DeepEqual(attempt.ExecutionRequirement, result.Cell.Execution) {
			return ReviewManifest{}, errors.New("meeting review attempt index differs from the exact result")
		}
		if index >= len(openedSource.Manifest.Attempts) {
			return ReviewManifest{}, errors.New("meeting review source omits an indexed attempt")
		}
		expectedMediaDirectory, expectedEvaluationDirectory, namespaceOK :=
			meetingAttemptArtifactNamespaces(attempt.Ordinal, attempt.Case)
		if !namespaceOK || attempt.MediaManifest.Path != filepath.ToSlash(filepath.Join(
			expectedMediaDirectory, "manifest.json",
		)) {
			return ReviewManifest{}, errors.New("meeting media manifest is outside its anchored attempt namespace")
		}
		sourceProjection, projectionErr := reviewSourceAttempt(attempt)
		if projectionErr != nil ||
			!reflect.DeepEqual(sourceProjection, openedSource.Manifest.Attempts[index]) {
			return ReviewManifest{}, errors.New("meeting outer attempt differs from its deterministic source projection")
		}
		hasVideo := false
		for _, media := range attempt.Media {
			if media.Kind == "video" {
				hasVideo = true
			}
		}
		if hasVideo != (attempt.VideoStatus == "retained-playable-video") ||
			(!hasVideo && attempt.VideoStatus != "audio-only-no-video-factory") {
			return ReviewManifest{}, errors.New("meeting review video status differs from indexed media")
		}
		wantExecution := meetingExecutionStatus(attempt.ExecutionRequirement, attempt.Deterministic)
		wantReportable, wantReportabilityNote := meetingAttemptReportability(
			attempt.RunOrigin, attempt.ExecutionRequirement, attempt.Deterministic,
			attempt.Cell, hasVideo,
		)
		if attempt.ExecutionStatus != wantExecution || attempt.Reportable != wantReportable ||
			attempt.ReportabilityNote != wantReportabilityNote {
			return ReviewManifest{}, errors.New("meeting review attempt reportability was not recomputed from exact evidence")
		}
		allAttemptsReportable = allAttemptsReportable && attempt.Reportable
		contextPayload, err := verifyMeetingReviewArtifact(absolute, attempt.Context)
		if err != nil {
			return ReviewManifest{}, errors.New("verify meeting review context")
		}
		contextValue, err := decodeMeetingReviewContext(contextPayload)
		if err != nil || contextValue.Format != ReviewContextFormat || contextValue.Version != ReviewContextVersion ||
			contextValue.Suite != SuiteName || contextValue.Case != attempt.Case || contextValue.Trial != attempt.Trial ||
			contextValue.ResultSHA256 != manifest.Result.SHA256 ||
			!reflect.DeepEqual(contextValue.Task, Suite()[index]) ||
			!reflect.DeepEqual(contextValue.Cell, result.Cell) ||
			!reflect.DeepEqual(contextValue.Provenance, result.Provenance) ||
			!reflect.DeepEqual(contextValue.RunOrigin, attempt.RunOrigin) ||
			!reflect.DeepEqual(contextValue.ExecutionRequirement, attempt.ExecutionRequirement) ||
			!reflect.DeepEqual(contextValue.Outcome, attempt.Deterministic) ||
			!meetingTranscriptExecutionMatchesOutcome(contextValue.Transcript, contextValue.Outcome) {
			return ReviewManifest{}, errors.New("meeting review context differs from exact run evidence")
		}
		mediaDirectory := expectedMediaDirectory
		verifiedMedia, err := reviewmedia.VerifyBundle(
			filepath.Join(absolute, filepath.FromSlash(mediaDirectory)), attempt.MediaManifest.SHA256,
		)
		if err != nil {
			return ReviewManifest{}, errors.New("verify meeting attempt media subtree")
		}
		media, err := retainedMeetingReviewMedia(verifiedMedia, attempt.MediaArtifacts, mediaDirectory)
		if err != nil || !reflect.DeepEqual(
			prefixMeetingReviewMedia(mediaDirectory, media), attempt.Media,
		) {
			return ReviewManifest{}, errors.New("meeting playable media differs from its review index")
		}
		manifestBytes, err := verifyMeetingReviewArtifact(absolute, attempt.MediaManifest)
		if err != nil || reviewDigest(manifestBytes) != attempt.MediaManifest.SHA256 {
			return ReviewManifest{}, errors.New("meeting media manifest artifact differs from its receipt")
		}
		mediaArtifacts, err := snapshotMeetingReviewSubtree(
			filepath.Join(absolute, filepath.FromSlash(mediaDirectory)), mediaDirectory,
		)
		if err != nil || !reflect.DeepEqual(mediaArtifacts, attempt.MediaArtifacts) ||
			!reviewArtifactsContain(mediaArtifacts, attempt.MediaManifest) {
			return ReviewManifest{}, errors.New("meeting media exact tree differs from its review index")
		}
		for _, artifact := range mediaArtifacts {
			if err := addExpectedMeetingReviewArtifact(expectedFiles, artifact); err != nil {
				return ReviewManifest{}, err
			}
		}

		receipt := attempt.EvaluationReceipt
		if receipt.Directory != expectedEvaluationDirectory {
			return ReviewManifest{}, errors.New("meeting reviewer receipt is outside its anchored attempt namespace")
		}
		reviewDirectory := filepath.Join(absolute, filepath.FromSlash(receipt.Directory))
		expectedReceipt := revieweval.EvaluationBundleReceipt{
			Directory: reviewDirectory, ManifestSHA256: receipt.ManifestSHA256,
			RecordSHA256: receipt.RecordSHA256, FileSetSHA256: receipt.FileSetSHA256,
			ReceiptSHA256: receipt.ReceiptSHA256,
		}
		opened, err := revieweval.VerifyEvaluationBundle(
			context.Background(), revieweval.EvaluationBundleOptions{Directory: reviewDirectory}, expectedReceipt,
		)
		if err != nil || opened.Manifest.EvidenceScope != receipt.EvidenceScope ||
			opened.Manifest.IndependentRemoteAttestation != receipt.IndependentRemoteAttestation ||
			opened.Manifest.AuthenticityCaveat != receipt.AuthenticityCaveat ||
			receipt.EvidenceScope != manifest.ReviewEvidenceScope ||
			receipt.IndependentRemoteAttestation != manifest.IndependentRemoteAttestation ||
			receipt.AuthenticityCaveat != manifest.ReviewAuthenticityCaveat {
			return ReviewManifest{}, errors.New("meeting reviewer bundle provenance scope differs from sealed evidence")
		}
		expectedReviewArtifacts, recordArtifact, evaluationManifest, err :=
			expectedMeetingEvaluationArtifacts(
				receipt.Directory, *receipt, opened.Manifest, *attempt.EvaluationManifest,
			)
		if err != nil || !reflect.DeepEqual(expectedReviewArtifacts, attempt.ReviewArtifacts) ||
			*attempt.SecondaryReview != recordArtifact || *attempt.EvaluationManifest != evaluationManifest {
			return ReviewManifest{}, errors.New("meeting reviewer exact artifact tree differs from its portable receipt")
		}
		for _, artifact := range attempt.ReviewArtifacts {
			if _, err := verifyMeetingReviewArtifact(absolute, artifact); err != nil {
				return ReviewManifest{}, errors.New("verify meeting reviewer exchange artifact")
			}
			if err := addExpectedMeetingReviewArtifact(expectedFiles, artifact); err != nil {
				return ReviewManifest{}, err
			}
		}
		record := opened.Record
		if record.Provider != manifest.Reviewer ||
			record.AttemptID != meetingReviewAttemptID(attempt.Case, manifest.Result.SHA256) ||
			record.Suite != SuiteName || record.Case != attempt.Case ||
			record.Trial != attempt.Trial || record.ContextSHA256 != attempt.Context.SHA256 ||
			record.RequestFingerprint != attempt.Assessment.RequestSHA256 ||
			!reflect.DeepEqual(meetingReviewResponse(record), *attempt.Assessment) ||
			meetingAssessmentBeyondMedia(record.Assessment, verifiedMedia.AttemptEndUS) ||
			!meetingReviewRecordMediaMatches(record.Media, verifiedMedia.ReviewMedia(), media) {
			return ReviewManifest{}, errors.New("meeting review record differs from exact context, media, or normalized index")
		}
		if err := addExpectedMeetingReviewArtifact(expectedFiles, attempt.Context); err != nil {
			return ReviewManifest{}, err
		}
	}
	if manifest.Reportable != (manifest.CoreReportable && allAttemptsReportable) {
		return ReviewManifest{}, errors.New("meeting review reportability is inconsistent with bound evidence")
	}
	if !bytes.Equal(reviewPayload, []byte(renderMeetingReview(manifest))) {
		return ReviewManifest{}, errors.New("meeting human review document is not the canonical render")
	}
	if err := verifyMeetingReviewExactTree(verificationRoot, expectedFiles); err != nil {
		return ReviewManifest{}, err
	}
	manifestArtifact.SHA256 = expectedManifestSHA256
	finalPayload, err := readMeetingSourceFile(
		context.Background(), verificationRoot, manifestArtifact, true,
	)
	if err != nil ||
		!bytes.Equal(finalPayload, manifestPayload) || reviewDigest(finalPayload) != expectedManifestSHA256 {
		return ReviewManifest{}, errors.New("meeting review manifest changed during verification")
	}
	if err := verifyMeetingSourceRootIdentity(absolute, verificationRoot, verificationRootInfo); err != nil {
		return ReviewManifest{}, errors.New("meeting review root changed during verification")
	}
	return manifest, nil
}

func addExpectedMeetingReviewArtifact(
	expected map[string]ReviewArtifact, artifact ReviewArtifact,
) error {
	if existing, duplicate := expected[artifact.Path]; duplicate {
		if existing != artifact {
			return errors.New("meeting review artifact path has conflicting identities")
		}
		return nil
	}
	expected[artifact.Path] = artifact
	return nil
}

func expectedMeetingEvaluationArtifacts(
	directory string, receipt ReviewEvaluationReceipt, manifest revieweval.EvaluationBundleManifest,
	manifestArtifact ReviewArtifact,
) ([]ReviewArtifact, ReviewArtifact, ReviewArtifact, error) {
	wantManifest := manifestArtifact
	wantManifest.Path = filepath.ToSlash(filepath.Join(directory, "manifest.json"))
	wantManifest.SHA256 = receipt.ManifestSHA256
	wantManifest.MediaType = "application/json"
	if wantManifest.SizeBytes <= 0 {
		return nil, ReviewArtifact{}, ReviewArtifact{}, errors.New("meeting reviewer manifest size is invalid")
	}
	artifacts := make([]ReviewArtifact, 0, len(manifest.Files)+1)
	artifacts = append(artifacts, wantManifest)
	var record ReviewArtifact
	for _, file := range manifest.Files {
		artifact := ReviewArtifact{
			Path: filepath.ToSlash(filepath.Join(directory, file.Path)), SHA256: file.SHA256,
			SizeBytes: file.SizeBytes, MediaType: evaluationBundleFileMediaType(file),
		}
		artifacts = append(artifacts, artifact)
		if file.Purpose == "record" {
			record = artifact
		}
	}
	if record.Path == "" {
		return nil, ReviewArtifact{}, ReviewArtifact{}, errors.New("meeting reviewer bundle has no record")
	}
	return artifacts, record, wantManifest, nil
}

func meetingReviewRecordMediaMatches(
	actual, expected []revieweval.Media, retained []ReviewMedia,
) bool {
	if len(actual) != len(expected) || len(actual) != len(retained) {
		return false
	}
	for index := range actual {
		left, right := actual[index], expected[index]
		if left.Kind != right.Kind || left.Role != right.Role || left.Path != right.Path ||
			left.SHA256 != right.SHA256 || left.MediaType != right.MediaType ||
			left.Validation != right.Validation || left.SizeBytes != retained[index].SizeBytes ||
			left.SizeBytes <= 0 {
			return false
		}
	}
	return true
}

func verifyMeetingReviewArtifact(directory string, artifact ReviewArtifact) ([]byte, error) {
	if artifact.Path == "" || filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path ||
		strings.Contains(artifact.Path, "..") || artifact.SizeBytes <= 0 || artifact.MediaType == "" ||
		!validReviewDigest(artifact.SHA256) {
		return nil, errors.New("meeting review artifact identity is invalid")
	}
	payload, _, err := readSealedMeetingReviewFile(
		filepath.Join(directory, filepath.FromSlash(artifact.Path)),
	)
	if err != nil || int64(len(payload)) != artifact.SizeBytes || reviewDigest(payload) != artifact.SHA256 {
		return nil, errors.New("meeting review artifact bytes differ from their identity")
	}
	return payload, nil
}

func readSealedMeetingReviewFile(path string) ([]byte, os.FileInfo, error) {
	return readMeetingReviewFileContext(context.Background(), path, true)
}

func readMeetingReviewFileContext(
	ctx context.Context, path string, requireSealed bool,
) (payload []byte, info os.FileInfo, resultErr error) {
	if ctx == nil {
		return nil, nil, errors.New("read meeting review artifact: nil context")
	}
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == filepath.Dir(absolute) {
		return nil, nil, errors.New("meeting review artifact path is invalid")
	}
	parent, name := filepath.Dir(absolute), filepath.Base(absolute)
	root, rootInfo, err := openMeetingSourceRoot(parent)
	if err != nil {
		return nil, nil, errors.New("open meeting review artifact parent")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			payload = nil
			info = nil
			resultErr = errors.Join(resultErr, errors.New("close meeting review artifact parent"))
		}
	}()
	meetingSourceVerifyHook(ctx, "review_read_opened_root")
	if err := context.Cause(ctx); err != nil {
		return nil, nil, err
	}
	before, err := meetingSourceFileInfo(root, name, requireSealed)
	if err != nil {
		return nil, nil, err
	}
	payload, err = readMeetingSourceFile(ctx, root, ReviewArtifact{
		Path: name, SizeBytes: before.Size(), MediaType: meetingArtifactMediaType(name),
	}, requireSealed)
	if err != nil {
		return nil, nil, err
	}
	info, err = meetingSourceFileInfo(root, name, requireSealed)
	if err != nil || !os.SameFile(before, info) {
		return nil, nil, errors.New("meeting review artifact changed after reading")
	}
	if err := verifyMeetingSourceRootIdentity(parent, root, rootInfo); err != nil {
		return nil, nil, err
	}
	return payload, info, nil
}

func verifyMeetingReviewExactTree(root *os.Root, expectedFiles map[string]ReviewArtifact) error {
	if root == nil {
		return errors.New("meeting review exact-tree verifier has no anchored root")
	}
	// Keep all expected parents derived from the content-addressed file index
	// rather than accepting arbitrary empty directories.
	expectedDirectories := map[string]struct{}{".": {}}
	for path := range expectedFiles {
		for parent := filepath.ToSlash(filepath.Dir(path)); ; parent = filepath.ToSlash(filepath.Dir(parent)) {
			expectedDirectories[parent] = struct{}{}
			if parent == "." {
				break
			}
		}
	}
	seenFiles := make(map[string]struct{}, len(expectedFiles))
	identities := make([]os.FileInfo, 0, len(expectedFiles))
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative := filepath.ToSlash(path)
		info, err := root.Lstat(relative)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 {
			return errors.New("meeting review tree contains a symlink or writable node")
		}
		if relative == "." {
			if !entry.IsDir() {
				return errors.New("meeting review root is not a directory")
			}
			return nil
		}
		if entry.IsDir() {
			if _, expected := expectedDirectories[relative]; expected {
				return nil
			}
			return errors.New("meeting review tree contains an unexpected directory")
		}
		artifact, expected := expectedFiles[relative]
		if !expected {
			return errors.New("meeting review tree contains an unexpected file")
		}
		for _, previous := range identities {
			if os.SameFile(previous, info) {
				return errors.New("meeting review tree contains duplicate hard-linked file identities")
			}
		}
		identities = append(identities, info)
		payload, err := readMeetingSourceFile(context.Background(), root, artifact, true)
		if err != nil || int64(len(payload)) != artifact.SizeBytes ||
			reviewDigest(payload) != artifact.SHA256 {
			return errors.New("meeting review tree file differs from its exact artifact identity")
		}
		seenFiles[relative] = struct{}{}
		return nil
	})
	if err != nil || len(seenFiles) != len(expectedFiles) {
		return errors.New("meeting review tree differs from its exact manifest")
	}
	return nil
}

func sealMeetingReviewTree(directory string) error {
	return sealMeetingReviewTreeContext(context.Background(), directory)
}

func sealMeetingReviewTreeContext(ctx context.Context, directory string) (resultErr error) {
	if ctx == nil {
		return errors.New("seal meeting review tree: nil context")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == filepath.Dir(absolute) {
		return errors.New("meeting review tree path is invalid")
	}
	root, rootInfo, err := openMeetingSourceRoot(absolute)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close sealed meeting review tree"))
		}
	}()
	meetingSourceVerifyHook(ctx, "review_seal_opened_root")
	if err := context.Cause(ctx); err != nil {
		return err
	}
	before, err := snapshotMeetingSourceFiles(ctx, root, "", false)
	if err != nil {
		return errors.New("snapshot meeting review tree before sealing")
	}
	expectedFiles := make(map[string]ReviewArtifact, len(before))
	files := make([]string, 0, len(before))
	for _, artifact := range before {
		expectedFiles[artifact.Path] = artifact
		files = append(files, artifact.Path)
	}
	var directories []string
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		info, err := root.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("meeting review tree contains an invalid node")
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("meeting review tree contains a non-regular artifact")
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, path := range files {
		beforeInfo, err := root.Lstat(path)
		if err != nil || beforeInfo.Mode()&os.ModeSymlink != 0 || !beforeInfo.Mode().IsRegular() {
			return errors.New("inspect meeting review artifact before sealing")
		}
		handle, err := root.Open(path)
		if err != nil {
			return errors.New("open meeting review artifact for sealing")
		}
		opened, statErr := handle.Stat()
		linkErr := fileidentity.RequireSingleLink(handle)
		syncBeforeErr := handle.Sync()
		chmodErr := handle.Chmod(0o400)
		syncAfterErr := handle.Sync()
		sealed, sealedErr := handle.Stat()
		closeErr := handle.Close()
		visible, visibleErr := root.Lstat(path)
		if statErr != nil || linkErr != nil || syncBeforeErr != nil || chmodErr != nil ||
			syncAfterErr != nil || sealedErr != nil || closeErr != nil || visibleErr != nil ||
			!os.SameFile(beforeInfo, opened) || !os.SameFile(opened, sealed) ||
			!os.SameFile(sealed, visible) || sealed.Mode().Perm() != 0o400 ||
			visible.Mode().Perm() != 0o400 {
			return errors.New("sync and seal meeting review artifact")
		}
	}
	sort.Slice(directories, func(left, right int) bool {
		return strings.Count(directories[left], "/") > strings.Count(directories[right], "/")
	})
	for _, path := range directories {
		beforeInfo, err := root.Lstat(path)
		if err != nil || !beforeInfo.IsDir() || beforeInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("inspect meeting review directory before sealing")
		}
		handle, err := root.Open(path)
		if err != nil {
			return errors.New("open meeting review directory for sealing")
		}
		opened, statErr := handle.Stat()
		syncBeforeErr := handle.Sync()
		chmodErr := handle.Chmod(0o500)
		syncAfterErr := handle.Sync()
		sealed, sealedErr := handle.Stat()
		closeErr := handle.Close()
		visible, visibleErr := root.Lstat(path)
		if statErr != nil || syncBeforeErr != nil || chmodErr != nil || syncAfterErr != nil ||
			sealedErr != nil || closeErr != nil || visibleErr != nil ||
			!os.SameFile(beforeInfo, opened) || !os.SameFile(opened, sealed) ||
			!os.SameFile(sealed, visible) || sealed.Mode().Perm() != 0o500 ||
			visible.Mode().Perm() != 0o500 {
			return errors.New("sync and seal meeting review directory")
		}
	}
	meetingSourceVerifyHook(ctx, "review_seal_before_final_identity")
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := verifyMeetingSourceRootIdentity(absolute, root, rootInfo); err != nil {
		return err
	}
	if err := verifyMeetingReviewExactTree(root, expectedFiles); err != nil {
		return errors.New("sealed meeting review tree differs from its exact pre-seal snapshot")
	}
	return verifyMeetingSourceRootIdentity(absolute, root, rootInfo)
}

func validReviewDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validateNewMeetingReviewDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) != path {
		return "", errors.New("meeting review directory must be a nonempty clean path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil || absolute == filepath.Dir(absolute) {
		return "", errors.New("meeting review directory is invalid")
	}
	for current := filepath.Dir(absolute); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("meeting review directory has an invalid or symlinked parent")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return absolute, nil
}

func meetingExternalEvidencePaths(
	bundleDirectory, sourceReceiptPath, evaluationReceiptDirectory,
	evaluationQuarantineDirectory string, requireAbsent bool,
) (string, string, string, error) {
	if sourceReceiptPath == "" {
		sourceReceiptPath = bundleDirectory + ".source-receipt.json"
	}
	if evaluationReceiptDirectory == "" {
		evaluationReceiptDirectory = bundleDirectory + ".evaluation-receipts"
	}
	if evaluationQuarantineDirectory == "" {
		evaluationQuarantineDirectory = bundleDirectory + ".evaluation-quarantine"
	}
	resolve := func(path string) (string, error) {
		absolute, err := filepath.Abs(path)
		if err != nil || filepath.Clean(path) != path || absolute == filepath.Dir(absolute) {
			return "", errors.New("meeting external evidence path is invalid")
		}
		for current := filepath.Dir(absolute); ; current = filepath.Dir(current) {
			info, statErr := os.Lstat(current)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", errors.New("meeting external evidence path has an invalid parent")
			}
			if current == filepath.Dir(current) {
				break
			}
		}
		return absolute, nil
	}
	source, err := resolve(sourceReceiptPath)
	if err != nil {
		return "", "", "", err
	}
	evaluations, err := resolve(evaluationReceiptDirectory)
	if err != nil {
		return "", "", "", err
	}
	quarantine, err := resolve(evaluationQuarantineDirectory)
	if err != nil {
		return "", "", "", err
	}
	separator := string(filepath.Separator)
	pathsOverlap := func(left, right string) bool {
		return left == right || strings.HasPrefix(left, right+separator) ||
			strings.HasPrefix(right, left+separator)
	}
	if pathsOverlap(source, bundleDirectory) || pathsOverlap(evaluations, bundleDirectory) ||
		pathsOverlap(quarantine, bundleDirectory) || pathsOverlap(source, evaluations) ||
		pathsOverlap(source, quarantine) || pathsOverlap(evaluations, quarantine) ||
		strings.HasPrefix(source, bundleDirectory+separator) ||
		strings.HasPrefix(evaluations, bundleDirectory+separator) ||
		strings.HasPrefix(quarantine, bundleDirectory+separator) {
		return "", "", "", errors.New("meeting external evidence paths overlap the bundle or each other")
	}
	if requireAbsent {
		for _, path := range []string{source, evaluations, quarantine} {
			if _, statErr := os.Lstat(path); statErr == nil {
				return "", "", "", errors.New("meeting external evidence create-only path already exists")
			} else if !os.IsNotExist(statErr) {
				return "", "", "", errors.New("inspect meeting external evidence create-only path")
			}
		}
	}
	return source, evaluations, quarantine, nil
}

func writeMeetingReviewFile(root *os.Root, path string, payload []byte) error {
	if root == nil || path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path ||
		strings.Contains(path, "..") || len(payload) == 0 {
		return errors.New("meeting review artifact path or payload is invalid")
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = root.Remove(path)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	written = true
	return nil
}

func canonicalReviewSecrets(source []string) ([]string, error) {
	if len(source) > maximumReviewSecrets {
		return nil, errors.New("meeting review declares too many sensitive values")
	}
	seen := make(map[string]struct{}, len(source))
	result := make([]string, 0, len(source))
	for _, value := range source {
		if len(value) < minimumReviewSecretBytes || len(value) > maximumReviewSecretBytes ||
			!utf8.ValidString(value) || strings.TrimSpace(value) != value ||
			strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, errors.New("meeting review sensitive value is short, invalid, or oversized")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	// Redact the longest declaration first so overlapping secrets cannot expose
	// a suffix (for example replacing "abcdefgh" before "abcdefghijkl").
	sort.Slice(result, func(left, right int) bool {
		if len(result[left]) != len(result[right]) {
			return len(result[left]) > len(result[right])
		}
		return result[left] < result[right]
	})
	return result, nil
}

func containsReviewSecret(secrets []string, payload []byte) bool {
	var candidates [][]byte
	for _, value := range secrets {
		raw := []byte(value)
		candidates = append(candidates, raw)
		if bytes.Contains(payload, raw) {
			return true
		}
		if encoded, err := json.Marshal(value); err == nil && len(encoded) > 2 &&
			bytes.Contains(payload, encoded[1:len(encoded)-1]) {
			return true
		} else if err == nil && len(encoded) > 2 {
			candidates = append(candidates, slices.Clone(encoded[1:len(encoded)-1]))
		}
		for _, encoding := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
		} {
			buffer := make([]byte, encoding.EncodedLen(len(raw)))
			encoding.Encode(buffer, raw)
			candidates = append(candidates, buffer)
			if bytes.Contains(payload, buffer) {
				return true
			}
		}
	}
	// A JSON producer can split one declared secret across adjacent keys or
	// string values so no individual encoded literal contains it. Conservatively
	// concatenate decoded value strings in wire order and scan each contiguous
	// run. Object keys are deliberately ignored, and a non-string value or
	// container boundary ends the run, which avoids treating unrelated labels as
	// secret fragments while preserving the exact serialized value order.
	// False positives fail closed and never reveal the declaration.
	if len(candidates) != 0 && strictjson.Validate(payload) == nil {
		runs, err := meetingJSONStringValueRuns(payload)
		if err != nil {
			return true
		}
		for _, run := range runs {
			for _, candidate := range candidates {
				if bytes.Contains(run, candidate) {
					return true
				}
			}
		}
	}
	return false
}

func meetingJSONStringValueRuns(payload []byte) ([][]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	var runs [][]byte
	appendRun := func(run []byte) {
		if len(run) != 0 {
			runs = append(runs, slices.Clone(run))
		}
	}
	var scanValue func(json.Token) error
	var scanContainer func(json.Delim) error
	scanContainer = func(open json.Delim) error {
		var run []byte
		flush := func() {
			appendRun(run)
			run = run[:0]
		}
		for decoder.More() {
			if open == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				if _, ok := key.(string); !ok {
					return errors.New("meeting review JSON object key is not a string")
				}
			}
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			if value, ok := token.(string); ok {
				run = append(run, value...)
				continue
			}
			flush()
			if err := scanValue(token); err != nil {
				return err
			}
		}
		flush()
		closeToken, err := decoder.Token()
		if err != nil {
			return err
		}
		closeDelimiter, ok := closeToken.(json.Delim)
		if !ok || (open == '{' && closeDelimiter != '}') ||
			(open == '[' && closeDelimiter != ']') {
			return errors.New("meeting review JSON container is unbalanced")
		}
		return nil
	}
	scanValue = func(token json.Token) error {
		switch value := token.(type) {
		case string:
			appendRun([]byte(value))
			return nil
		case json.Delim:
			if value != '{' && value != '[' {
				return errors.New("meeting review JSON value begins with a closing delimiter")
			}
			return scanContainer(value)
		default:
			return nil
		}
	}
	if err := scanValue(first); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("meeting review JSON has trailing tokens")
	}
	return runs, nil
}

func redactMeetingReviewContext(value *meetingReviewContext, secrets []string) {
	redact := func(source string) string {
		for _, secret := range secrets {
			source = strings.ReplaceAll(source, secret, "[REDACTED]")
		}
		return source
	}
	value.Outcome.Error = redact(value.Outcome.Error)
	value.Outcome.ExecutionError = redact(value.Outcome.ExecutionError)
	for name, note := range value.Outcome.Notes {
		value.Outcome.Notes[name] = redact(note)
	}
	value.Transcript.Failure = redact(value.Transcript.Failure)
	value.Transcript.ExecutionError = redact(value.Transcript.ExecutionError)
	for index := range value.Transcript.Moments {
		moment := &value.Transcript.Moments[index]
		moment.Text = redact(moment.Text)
		moment.Arguments = redact(moment.Arguments)
	}
}

func cloneReviewResponse(source ReviewResponse) ReviewResponse {
	result := source
	result.SignificantProblems = cloneReviewFindings(source.SignificantProblems)
	result.MinorObservations = cloneReviewFindings(source.MinorObservations)
	result.Limitations = slices.Clone(source.Limitations)
	return result
}

func cloneReviewAttempt(source ReviewAttempt) ReviewAttempt {
	result := source
	result.Deterministic = cloneTaskOutcome(source.Deterministic)
	result.Cell = cloneMeetingCell(source.Cell)
	result.ExecutionRequirement = cloneMeetingExecutionRequirement(source.ExecutionRequirement)
	result.Media = cloneReviewMedia(source.Media)
	result.MediaArtifacts = slices.Clone(source.MediaArtifacts)
	result.ReviewArtifacts = slices.Clone(source.ReviewArtifacts)
	if source.SecondaryReview != nil {
		copy := *source.SecondaryReview
		result.SecondaryReview = &copy
	}
	if source.EvaluationManifest != nil {
		copy := *source.EvaluationManifest
		result.EvaluationManifest = &copy
	}
	if source.EvaluationReceipt != nil {
		copy := *source.EvaluationReceipt
		result.EvaluationReceipt = &copy
	}
	if source.Assessment != nil {
		copy := cloneReviewResponse(*source.Assessment)
		result.Assessment = &copy
	}
	return result
}

func cloneReviewMedia(source []ReviewMedia) []ReviewMedia { return slices.Clone(source) }

func prefixMeetingReviewMedia(directory string, source []ReviewMedia) []ReviewMedia {
	result := cloneReviewMedia(source)
	for index := range result {
		result[index].Path = filepath.ToSlash(filepath.Join(directory, result[index].Path))
	}
	return result
}

func meetingCaseOrdinal(caseID string) (int, bool) {
	for index, task := range Suite() {
		if task.ID == caseID {
			return index + 1, true
		}
	}
	return 0, false
}

func meetingAttemptArtifactNamespaces(ordinal int, caseID string) (string, string, bool) {
	wantOrdinal, ok := meetingCaseOrdinal(caseID)
	if !ok || ordinal != wantOrdinal {
		return "", "", false
	}
	leaf := fmt.Sprintf("%02d-%s-trial-01", ordinal, meetingReviewSlug(caseID))
	return filepath.ToSlash(filepath.Join(reviewSourceDirectory, "media", leaf)),
		filepath.ToSlash(filepath.Join("evaluations", leaf)), true
}

func (bundle *ReviewBundle) externalEvaluationReceiptPath(
	ordinal int, caseID string,
) (string, error) {
	if bundle == nil || bundle.evaluationReceiptDirectory == "" {
		return "", errors.New("meeting reviewer evaluation has no external receipt destination")
	}
	_, evaluationDirectory, ok := meetingAttemptArtifactNamespaces(ordinal, caseID)
	if !ok {
		return "", errors.New("meeting reviewer evaluation receipt namespace is invalid")
	}
	return filepath.Join(bundle.evaluationReceiptDirectory,
		filepath.Base(evaluationDirectory)+".receipt.json"), nil
}

func meetingReviewSlug(value string) string {
	var output strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			output.WriteRune(character)
		}
	}
	return strings.Trim(output.String(), "-")
}

func meetingMarkdown(value string) string {
	value = html.EscapeString(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(value))
	var output strings.Builder
	output.Grow(len(value))
	for _, character := range value {
		switch character {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '#', '+', '-', '.', '!', '|':
			output.WriteByte('\\')
		}
		output.WriteRune(character)
	}
	return output.String()
}

func appendReportabilityNote(existing, next string) string {
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

func reviewDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
