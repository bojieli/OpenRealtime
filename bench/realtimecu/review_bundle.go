package realtimecu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	revieweval "github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	ReviewBundleFormat         = "openrealtime.realtime-cu-review"
	ReviewBundleFormatVersion  = 3
	ReviewContextFormat        = "openrealtime.realtime-cu-review-context"
	ReviewContextVersion       = 2
	ReviewSourceReceiptFormat  = "openrealtime.realtime-cu-review-source-receipt"
	ReviewSourceReceiptVersion = 1
	ReviewPhaseSource          = "deterministic-source"
	ReviewPhaseReviewed        = "reviewed-evidence"
	reviewSourceManifest       = "source.manifest.json"
	reviewSourceManifestStage  = ".openrealtime-source-manifest.stage"

	maximumReviewContextBytes     = 4 << 20
	maximumReviewManifestBytes    = 16 << 20
	maximumReviewResultBytes      = 16 << 20
	maximumReviewMarkdownBytes    = 8 << 20
	maximumReviewSecrets          = 256
	maximumReviewSecretBytes      = 4 << 10
	minimumReviewSecretBytes      = 8
	reviewAttemptRetentionTimeout = 2 * time.Minute
	reviewSuiteRetentionTimeout   = 10 * time.Minute
)

// ReviewVideoFactory supplies fresh attempt-scoped encoder and independent
// full-decode attestor plug-ins. Realtime-CU always requires both: its human
// evidence is synchronized video+audio, never an audio-only approximation.
type ReviewVideoFactory interface {
	NewReviewVideo(context.Context, EvidenceAttempt) (reviewmedia.Encoder, reviewmedia.Attestor, error)
}

// ReviewBundleOptions contains only caller-owned plug-ins and storage. No
// encoder, browser, reviewer provider, server, or UI is selected by the suite.
type ReviewBundleOptions struct {
	Directory    string
	VideoFactory ReviewVideoFactory
	// Reviewer is an optional caller-owned provider lease. The bundle never
	// closes it. When configured, every completed attempt is evaluated only
	// after FinishSuite supplies the exact final cell, provenance, and result.
	Reviewer *revieweval.ProviderLease
	// SourceAnchor durably publishes and then independently verifies the
	// portable deterministic-source receipt before Reviewer may observe any
	// media. It is required whenever Reviewer is configured.
	SourceAnchor ReviewSourceReceiptAnchor
	// EvaluationStores supplies caller-owned durable receipt storage per case.
	// Generic review publication reopens the receipt before atomically exposing
	// the final evaluation directory. It is required whenever Reviewer is set.
	EvaluationStores ReviewEvaluationReceiptStoreFactory
	// ReviewConcurrency bounds parallel attempt reviews. Zero uses four;
	// providers are concurrency-safe by review.Provider contract.
	ReviewConcurrency int
	SensitiveValues   []string
}

// ReviewBundleResumeOptions reopens only a previously verified deterministic
// source phase. Evaluation siblings are recovered only through caller-owned
// receipt stores; an unanchored final sibling is never silently adopted.
type ReviewBundleResumeOptions struct {
	Directory     string
	SourceReceipt ReviewSourceReceipt
	// SourceAnchor is required only to recover a crash after the external
	// receipt committed but before the staged source marker was promoted.
	// Ordinary final-source resume still accepts the caller-retained receipt.
	SourceAnchor      ReviewSourceReceiptAnchor
	Reviewer          *revieweval.ProviderLease
	EvaluationStores  ReviewEvaluationReceiptStoreFactory
	ReviewConcurrency int
	SensitiveValues   []string
}

// ReviewArtifact identifies one exact file relative to the suite bundle.
type ReviewArtifact struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

// ReviewMedia is a human-playable artifact from an independently verified
// attempt media bundle. Raw frames and timelines remain in MediaBundle.Path.
type ReviewMedia struct {
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

// NestedBundleReceipt binds a delegated create-only media or model-review
// bundle without copying its complete potentially large manifest here.
type NestedBundleReceipt struct {
	Path           string `json:"path"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

// NestedEvaluationReceipt is the portable, externally anchored receipt for a
// generic provider-neutral review.EvaluationBundle. Directory is represented
// by Path so no machine-local absolute path enters the suite manifest.
type NestedEvaluationReceipt struct {
	Path           string `json:"path"`
	ManifestSHA256 string `json:"manifest_sha256"`
	RecordSHA256   string `json:"record_sha256"`
	FileSetSHA256  string `json:"file_set_sha256"`
	ReceiptSHA256  string `json:"receipt_sha256"`
}

// ReviewAttempt is the case-by-case pass/fail index consumed by REVIEW.md and
// presentation clients. Deterministic is always the benchmark authority.
type ReviewAttempt struct {
	Ordinal                      int                            `json:"ordinal"`
	Case                         string                         `json:"case"`
	Trial                        int                            `json:"trial"`
	Grounding                    Grounding                      `json:"grounding"`
	Observers                    []string                       `json:"observers,omitempty"`
	Deterministic                bench.TaskOutcome              `json:"deterministic"`
	Context                      ReviewArtifact                 `json:"context"`
	MediaBundle                  NestedBundleReceipt            `json:"media_bundle"`
	Media                        []ReviewMedia                  `json:"media"`
	ReviewStatus                 string                         `json:"review_status"`
	EvaluationBundle             *NestedEvaluationReceipt       `json:"evaluation_bundle,omitempty"`
	Reviewer                     *revieweval.ProviderDescriptor `json:"reviewer,omitempty"`
	Assessment                   *revieweval.Assessment         `json:"assessment,omitempty"`
	EvidenceScope                string                         `json:"review_evidence_scope,omitempty"`
	IndependentRemoteAttestation bool                           `json:"independent_remote_attestation"`
	AuthenticityCaveat           string                         `json:"authenticity_caveat,omitempty"`
	RunOrigin                    EvidenceRunOrigin              `json:"run_origin"`
	ExecutionStatus              string                         `json:"execution_status"`
	Reportable                   bool                           `json:"reportable"`
	ReportabilityNote            string                         `json:"reportability_note,omitempty"`
}

type ReviewMissing struct {
	Case   string `json:"case"`
	Reason string `json:"reason"`
}

// ReviewManifest is published last. Complete describes evidence coverage;
// Reportable additionally requires the exact core bench.Result gate, live
// production origins, and a verified secondary review for every attempt.
type ReviewManifest struct {
	Format              string           `json:"format"`
	FormatVersion       int              `json:"format_version"`
	Phase               string           `json:"phase"`
	Suite               string           `json:"suite"`
	Expected            int              `json:"expected"`
	Complete            bool             `json:"complete"`
	Reportable          bool             `json:"reportable"`
	CoreReportable      bool             `json:"core_reportable"`
	CoreReportability   string           `json:"core_reportability,omitempty"`
	Result              ReviewArtifact   `json:"result"`
	SourceManifest      *ReviewArtifact  `json:"source_manifest,omitempty"`
	Cell                bench.Cell       `json:"cell"`
	Provenance          bench.Provenance `json:"provenance"`
	ReportabilityErrors []string         `json:"reportability_errors,omitempty"`
	Missing             []ReviewMissing  `json:"missing,omitempty"`
	Attempts            []ReviewAttempt  `json:"attempts"`
}

// ReviewSourceReceipt externally anchors deterministic result, context, raw
// media, and independently attested playable A/V before any advisory model is
// invoked. It remains valid when model review is unavailable or canceled.
type ReviewSourceReceipt struct {
	Format         string `json:"format"`
	FormatVersion  int    `json:"format_version"`
	Directory      string `json:"directory"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ResultSHA256   string `json:"result_sha256"`
	ReceiptSHA256  string `json:"receipt_sha256"`
}

type ReviewBundleReceipt struct {
	Directory            string `json:"directory"`
	ManifestSHA256       string `json:"manifest_sha256"`
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	SourceReceiptSHA256  string `json:"source_receipt_sha256"`
}

// ReviewBundlePublicationError reports the exceptional case in which final
// verification failed and the published outer manifest could not be removed.
// VerifiedReceipt is populated only by a fresh anchored verification.
type ReviewBundlePublicationError struct {
	ExpectedReceipt ReviewBundleReceipt
	VerifiedReceipt ReviewBundleReceipt
	MarkerMayRemain bool
	cause           error
}

// ReviewSourcePublicationError is the source-phase equivalent: a non-zero
// VerifiedReceipt is populated only by a fresh externally anchored source
// verification after invalidation itself failed.
type ReviewSourcePublicationError struct {
	ExpectedReceipt ReviewSourceReceipt
	VerifiedReceipt ReviewSourceReceipt
	MarkerMayRemain bool
	cause           error
}

func (failure *ReviewSourcePublicationError) Error() string {
	return "deterministic realtime computer-use source publication failed and its commit marker could not be invalidated"
}

func (failure *ReviewSourcePublicationError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func (failure *ReviewBundlePublicationError) Error() string {
	return "realtime computer-use review publication failed and its commit marker could not be invalidated"
}

func (failure *ReviewBundlePublicationError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

type retainedMediaReceipt struct {
	directory      string
	manifestSHA256 string
	manifest       reviewmedia.Manifest
}

type retainedReviewCompletion struct {
	specification     EvidenceAttempt
	completion        EvidenceCompletion
	ordinal           int
	relativeDirectory string
	media             []ReviewMedia
	mediaReceipt      retainedMediaReceipt
}

func (source retainedReviewCompletion) clone() (retainedReviewCompletion, error) {
	result := source
	specification, err := cloneEvidenceAttempt(source.specification)
	if err != nil {
		return retainedReviewCompletion{}, err
	}
	completion, err := cloneEvidenceCompletion(source.completion)
	if err != nil {
		return retainedReviewCompletion{}, err
	}
	result.specification = specification
	result.completion = completion
	result.media = slices.Clone(source.media)
	result.mediaReceipt = source.mediaReceipt.clone()
	return result, nil
}

func (receipt retainedMediaReceipt) clone() retainedMediaReceipt {
	result := receipt
	result.manifest = cloneMediaManifest(receipt.manifest)
	return result
}

// ReviewBundle is a create-only EvidencePlugin. Attempt sub-bundles own the
// full raw media lifecycle; this suite index closes admission and drains every
// active attempt before it publishes its manifest.
type ReviewBundle struct {
	mu                   sync.Mutex
	directory            string
	root                 *os.Root
	videoFactory         ReviewVideoFactory
	reviewer             *revieweval.ProviderLease
	sourceAnchor         ReviewSourceReceiptAnchor
	evaluationStores     ReviewEvaluationReceiptStoreFactory
	reviewConcurrency    int
	sensitive            []string
	completed            map[string]retainedReviewCompletion
	failures             map[string]string
	active               int
	closing              bool
	finishing            bool
	finished             bool
	abandoning           bool
	abandoned            bool
	closed               bool
	drainDone            chan struct{}
	drainOnce            sync.Once
	finishDone           chan struct{}
	finishAttemptDone    chan struct{}
	finishErr            error
	lastFinishErr        error
	finishSource         []byte
	pendingSourceReceipt *ReviewSourceReceipt
	sourceReceipt        *ReviewSourceReceipt
	sourceAnchored       bool
	sourceAttempts       map[string]ReviewAttempt
	sourceMedia          map[string]retainedMediaReceipt
	evaluationReceipts   map[string]revieweval.EvaluationBundleReceipt
	receipt              *ReviewBundleReceipt
	closeDone            chan struct{}
	closeErr             error
}

func NewReviewBundle(options ReviewBundleOptions) (*ReviewBundle, error) {
	if nilInterface(options.VideoFactory) {
		return nil, errors.New("realtime computer-use review bundle requires a video encoder and attestor factory")
	}
	if options.Reviewer != nil && nilInterface(options.SourceAnchor) {
		return nil, errors.New("realtime computer-use advisory review requires an external source receipt anchor")
	}
	if options.Reviewer != nil && nilInterface(options.EvaluationStores) {
		return nil, errors.New("realtime computer-use advisory review requires external evaluation receipt stores")
	}
	if options.ReviewConcurrency < 0 || options.ReviewConcurrency > 16 {
		return nil, errors.New("realtime computer-use review concurrency must be 1..16 or zero")
	}
	if options.ReviewConcurrency == 0 {
		options.ReviewConcurrency = 4
	}
	sensitive, err := canonicalReviewSecrets(options.SensitiveValues)
	if err != nil {
		return nil, err
	}
	directory, err := validateNewReviewDirectory(options.Directory)
	if err != nil {
		return nil, err
	}
	if containsReviewSecret(sensitive, []byte(directory)) {
		return nil, errors.New("realtime computer-use review directory contains a sensitive value")
	}
	parentPath, name := filepath.Dir(directory), filepath.Base(directory)
	parentRoot, parentIdentity, err := openReviewDirectoryRoot(parentPath)
	if err != nil {
		return nil, err
	}
	created := false
	cleanupParent := func(primary error) error {
		if created {
			_ = parentRoot.RemoveAll(name)
		}
		if closeErr := parentRoot.Close(); closeErr != nil {
			primary = errors.Join(primary,
				errors.New("close realtime computer-use review bundle parent"))
		}
		return primary
	}
	if _, err := parentRoot.Lstat(name); err == nil {
		return nil, cleanupParent(errors.New("realtime computer-use review bundle directory already exists"))
	} else if !os.IsNotExist(err) {
		return nil, cleanupParent(errors.New("inspect realtime computer-use review bundle directory"))
	}
	if err := parentRoot.Mkdir(name, 0o700); err != nil {
		return nil, cleanupParent(errors.New("create realtime computer-use review bundle directory exclusively"))
	}
	created = true
	if err := syncReviewRoot(parentRoot); err != nil {
		return nil, cleanupParent(errors.New("sync realtime computer-use review bundle parent"))
	}
	root, err := parentRoot.OpenRoot(name)
	if err != nil {
		return nil, cleanupParent(errors.New("open realtime computer-use review bundle directory"))
	}
	createdInfo, createdErr := parentRoot.Lstat(name)
	rootInfo, rootErr := root.Stat(".")
	if createdErr != nil || rootErr != nil || !os.SameFile(createdInfo, rootInfo) {
		_ = root.Close()
		return nil, cleanupParent(errors.New("realtime computer-use review bundle identity changed during creation"))
	}
	bundle := &ReviewBundle{
		directory: directory, root: root, videoFactory: options.VideoFactory,
		reviewer: options.Reviewer, sourceAnchor: options.SourceAnchor,
		evaluationStores:   options.EvaluationStores,
		reviewConcurrency:  options.ReviewConcurrency,
		sensitive:          slices.Clone(sensitive),
		completed:          make(map[string]retainedReviewCompletion, 16),
		failures:           make(map[string]string, 16),
		sourceAttempts:     make(map[string]ReviewAttempt, 16),
		sourceMedia:        make(map[string]retainedMediaReceipt, 16),
		evaluationReceipts: make(map[string]revieweval.EvaluationBundleReceipt, 16),
		drainDone:          make(chan struct{}), finishDone: make(chan struct{}), closeDone: make(chan struct{}),
	}
	for _, name := range []string{"contexts", "media", "reviews"} {
		if err := root.Mkdir(name, 0o700); err != nil {
			_ = root.Close()
			return nil, cleanupParent(errors.New("create realtime computer-use review bundle layout"))
		}
	}
	if _, err := verifyReviewRootIdentity(parentPath, parentRoot, parentIdentity); err != nil {
		_ = root.Close()
		return nil, cleanupParent(err)
	}
	if closeErr := parentRoot.Close(); closeErr != nil {
		_ = root.Close()
		return nil, errors.New("close realtime computer-use review bundle parent")
	}
	return bundle, nil
}

func ResumeReviewBundle(
	ctx context.Context, options ReviewBundleResumeOptions,
) (*ReviewBundle, error) {
	if ctx == nil {
		return nil, errors.New("resume realtime computer-use review bundle: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if options.ReviewConcurrency < 0 || options.ReviewConcurrency > 16 {
		return nil, errors.New("realtime computer-use review concurrency must be 1..16 or zero")
	}
	if options.ReviewConcurrency == 0 {
		options.ReviewConcurrency = 4
	}
	if options.Reviewer != nil && nilInterface(options.EvaluationStores) {
		return nil, errors.New("resumable realtime computer-use reviewer requires external evaluation receipt stores")
	}
	sensitive, err := canonicalReviewSecrets(options.SensitiveValues)
	if err != nil {
		return nil, err
	}
	directory, err := validateNewReviewDirectory(options.Directory)
	if err != nil {
		return nil, err
	}
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return nil, err
	}
	if containsReviewSecret(sensitive, []byte(directory)) {
		return nil, errors.New("realtime computer-use review directory contains a sensitive value")
	}
	source, err := VerifyReviewSourceReceipt(directory, options.SourceReceipt)
	if err != nil {
		if nilInterface(options.SourceAnchor) {
			return nil, fmt.Errorf("resume deterministic realtime computer-use source: %w", err)
		}
		publication, recoverErr := commitStagedReviewSource(
			ctx, directory, options.SourceReceipt, options.SourceAnchor,
		)
		if recoverErr != nil || !publication.ReceiptDurable || !publication.FinalVerified {
			return nil, fmt.Errorf(
				"recover staged deterministic realtime computer-use source: %w",
				errors.Join(err, recoverErr),
			)
		}
		source, err = VerifyReviewSourceReceipt(directory, options.SourceReceipt)
		if err != nil {
			return nil, fmt.Errorf("verify recovered deterministic realtime computer-use source: %w", err)
		}
	}
	if err := recoverRejectedReviewPublication(directory); err != nil {
		return nil, err
	}
	for _, name := range []string{"manifest.json", "REVIEW.md"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
			return nil, errors.New("realtime computer-use review bundle already has an outer publication")
		} else if !os.IsNotExist(err) {
			return nil, errors.New("inspect resumable realtime computer-use review publication")
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("open resumable realtime computer-use review bundle")
	}
	fail := func(failure error) (*ReviewBundle, error) {
		if closeErr := root.Close(); closeErr != nil {
			failure = errors.Join(failure, errors.New("close rejected resumable review bundle"))
		}
		return nil, failure
	}
	resultPayload, _, err := readReviewFile(root, source.Result.Path, maximumReviewResultBytes)
	if err != nil || !artifactMatches(source.Result, resultPayload) {
		return fail(errors.New("read exact resumable realtime computer-use result"))
	}
	result, err := decodeReviewResult(resultPayload)
	if err != nil {
		return fail(err)
	}
	completed := make(map[string]retainedReviewCompletion, len(source.Attempts))
	sourceAttempts := make(map[string]ReviewAttempt, len(source.Attempts))
	sourceMedia := make(map[string]retainedMediaReceipt, len(source.Attempts))
	for _, indexed := range source.Attempts {
		contextPayload, _, err := readReviewFile(
			root, indexed.Context.Path, maximumReviewContextBytes,
		)
		if err != nil || !artifactMatches(indexed.Context, contextPayload) {
			return fail(errors.New("read resumable realtime computer-use source context"))
		}
		contextValue, err := decodeReviewContext(contextPayload)
		if err != nil {
			return fail(err)
		}
		specification := EvidenceAttempt{
			Suite: SuiteName, Case: indexed.Case, Trial: indexed.Trial,
			Task:      cloneCase(Case{Task: contextValue.Task}).Task,
			Grounding: indexed.Grounding, Origin: contextValue.RunOrigin,
			Observers:            slices.Clone(contextValue.Observers),
			ExecutionRequirement: contextValue.ExecutionRequirement,
		}
		if err := specification.validate(); err != nil {
			return fail(err)
		}
		actions, err := resumedReviewActions(contextValue.Actions)
		if err != nil {
			return fail(err)
		}
		completion := EvidenceCompletion{
			Attempt: specification, Outcome: cloneTaskOutcome(contextValue.Outcome),
			Transcript: contextValue.Transcript, Page: contextValue.Page, Actions: actions,
		}
		if err := validateReviewCompletionEvidence(completion); err != nil {
			return fail(err)
		}
		mediaDirectory := filepath.Join(
			directory, filepath.FromSlash(indexed.MediaBundle.Path),
		)
		mediaManifest, err := reviewmedia.VerifyBundle(
			mediaDirectory, indexed.MediaBundle.ManifestSHA256,
		)
		if err != nil {
			return fail(errors.New("verify resumable realtime computer-use source media"))
		}
		media, err := retainedReviewMedia(
			mediaDirectory, indexed.MediaBundle.Path, specification, mediaManifest,
		)
		if err != nil || !reflect.DeepEqual(media, indexed.Media) {
			return fail(errors.New("resumable realtime computer-use media index drifted"))
		}
		retained := retainedMediaReceipt{
			directory: mediaDirectory, manifestSHA256: indexed.MediaBundle.ManifestSHA256,
			manifest: mediaManifest,
		}
		completed[indexed.Case] = retainedReviewCompletion{
			specification: specification, completion: completion, ordinal: indexed.Ordinal,
			relativeDirectory: indexed.MediaBundle.Path, media: slices.Clone(indexed.Media),
			mediaReceipt: retained,
		}
		sourceAttempts[indexed.Case] = cloneReviewAttempt(indexed)
		sourceMedia[indexed.Case] = retained.clone()
	}
	failures, err := retainedReviewFailures(source, sourceAttempts)
	if err != nil {
		return fail(err)
	}
	sourceReceipt := options.SourceReceipt
	sourceReceipt.Directory = directory
	bundle := &ReviewBundle{
		directory: directory, root: root, reviewer: options.Reviewer,
		sourceAnchor:      options.SourceAnchor,
		evaluationStores:  options.EvaluationStores,
		reviewConcurrency: options.ReviewConcurrency, sensitive: slices.Clone(sensitive),
		completed: completed, failures: failures, closing: true,
		finishSource: slices.Clone(resultPayload), sourceReceipt: &sourceReceipt,
		sourceAnchored: true,
		sourceAttempts: sourceAttempts, sourceMedia: sourceMedia,
		evaluationReceipts: make(map[string]revieweval.EvaluationBundleReceipt, 16),
		drainDone:          make(chan struct{}), finishDone: make(chan struct{}), closeDone: make(chan struct{}),
	}
	bundle.drainOnce.Do(func() { close(bundle.drainDone) })
	// Re-encoding here proves the persisted result is the exact source used by
	// future FinishSuite input comparison, not merely a digest-equivalent view.
	canonical, err := encodeReviewResult(result)
	if err != nil || !bytes.Equal(canonical, resultPayload) {
		return fail(errors.New("resumable realtime computer-use result is noncanonical"))
	}
	if err := makeReviewRootWritableForResume(directory, root); err != nil {
		return fail(err)
	}
	return bundle, nil
}

func makeReviewRootWritableForResume(directory string, root *os.Root) error {
	identity, err := verifyReviewRootIdentity(directory, root, nil)
	if err != nil {
		return err
	}
	if err := chmodReviewEntryHandle(root, ".", identity, 0o700); err != nil {
		return errors.New("make resumable realtime computer-use review root writable")
	}
	anchored, rootErr := root.Stat(".")
	current, pathErr := os.Lstat(directory)
	if rootErr != nil || pathErr != nil || !anchored.IsDir() || !current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, anchored) ||
		!os.SameFile(anchored, current) || anchored.Mode().Perm() != 0o700 {
		_ = chmodReviewEntryHandle(root, ".", identity, 0o500)
		return errors.New("resumable realtime computer-use review root changed while opening")
	}
	return nil
}

func resumedReviewActions(source []realtimeCUReviewAction) ([]ActionRecord, error) {
	result := make([]ActionRecord, len(source))
	for index, action := range source {
		result[index] = ActionRecord{
			CallID: action.CallID, Name: action.Name,
			Arguments: slices.Clone(action.Arguments), Error: action.Error,
		}
		for _, item := range []struct {
			value       string
			destination *time.Time
		}{
			{action.ReceivedAt, &result[index].ReceivedAt},
			{action.CompletedAt, &result[index].CompletedAt},
		} {
			value, destination := item.value, item.destination
			if value == "" {
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || parsed.Format(time.RFC3339Nano) != value {
				return nil, errors.New("resumable realtime computer-use action timestamp is invalid")
			}
			*destination = parsed
		}
	}
	return result, nil
}

func (bundle *ReviewBundle) Directory() string {
	if bundle == nil {
		return ""
	}
	return bundle.directory
}

func (bundle *ReviewBundle) Receipt() (ReviewBundleReceipt, bool) {
	if bundle == nil {
		return ReviewBundleReceipt{}, false
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.receipt == nil {
		return ReviewBundleReceipt{}, false
	}
	return *bundle.receipt, true
}

func (bundle *ReviewBundle) SourceReceipt() (ReviewSourceReceipt, bool) {
	if bundle == nil {
		return ReviewSourceReceipt{}, false
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.sourceReceipt == nil {
		return ReviewSourceReceipt{}, false
	}
	return *bundle.sourceReceipt, true
}

// SourceResult returns an independent decode of the exact canonical result
// sealed by the deterministic source phase. It is primarily a restart seam:
// callers can resume advisory evaluation without rerunning browser actions or
// trusting a separately supplied result file.
func (bundle *ReviewBundle) SourceResult() (bench.Result, error) {
	if bundle == nil {
		return bench.Result{}, errors.New("read realtime computer-use source result: nil bundle")
	}
	bundle.mu.Lock()
	payload := slices.Clone(bundle.finishSource)
	var receipt ReviewSourceReceipt
	sealed := bundle.sourceReceipt != nil
	if sealed {
		receipt = *bundle.sourceReceipt
	}
	directory := bundle.directory
	bundle.mu.Unlock()
	if len(payload) == 0 || !sealed {
		return bench.Result{}, errors.New("realtime computer-use deterministic source is not sealed")
	}
	if _, err := VerifyReviewSourceReceipt(directory, receipt); err != nil {
		return bench.Result{}, errors.New("verify realtime computer-use source before reading result")
	}
	result, err := decodeReviewResult(payload)
	if err != nil {
		return bench.Result{}, err
	}
	canonical, err := encodeReviewResult(result)
	if err != nil || !bytes.Equal(canonical, payload) {
		return bench.Result{}, errors.New("realtime computer-use deterministic source result is noncanonical")
	}
	return result, nil
}

// EvaluationReceipts returns portable receipts already durable in the
// caller-owned store and atomically promoted by advisory publication. It is an
// observability snapshot, not the recovery authority; Resume reloads stores.
func (bundle *ReviewBundle) EvaluationReceipts() map[string]revieweval.EvaluationBundleReceipt {
	if bundle == nil {
		return nil
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	result := make(map[string]revieweval.EvaluationBundleReceipt, len(bundle.evaluationReceipts))
	for caseID, receipt := range bundle.evaluationReceipts {
		result[caseID] = receipt
	}
	return result
}

// AdoptEvaluationReceipt admits one externally retained sibling receipt for a
// later FinishSuite retry. The exact source context and media are reverified
// before the receipt enters bundle state; callers cannot attach an evaluation
// for another result, case, or media tree.
func (bundle *ReviewBundle) AdoptEvaluationReceipt(
	ctx context.Context, caseID string, receipt revieweval.EvaluationBundleReceipt,
) error {
	if bundle == nil || ctx == nil {
		return errors.New("adopt realtime computer-use evaluation receipt: nil bundle or context")
	}
	if _, known := canonicalCase(caseID); !known {
		return errors.New("adopt realtime computer-use evaluation receipt: unknown case")
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.finishing || bundle.finished || bundle.closed || bundle.abandoning ||
		bundle.sourceReceipt == nil || bundle.root == nil {
		return errors.New("realtime computer-use source is unavailable for evaluation adoption")
	}
	sourceReceipt := *bundle.sourceReceipt
	if _, err := VerifyReviewSourceReceipt(bundle.directory, sourceReceipt); err != nil {
		return errors.New("verify deterministic source before evaluation adoption")
	}
	indexed, attemptExists := bundle.sourceAttempts[caseID]
	mediaReceipt, mediaExists := bundle.sourceMedia[caseID]
	if !attemptExists || !mediaExists {
		return errors.New("realtime computer-use source has no matching evaluation case")
	}
	contextPayload, _, err := readReviewFile(
		bundle.root, indexed.Context.Path, maximumReviewContextBytes,
	)
	if err != nil || !artifactMatches(indexed.Context, contextPayload) {
		return errors.New("read deterministic source context for evaluation adoption")
	}
	verifiedMedia, err := reviewmedia.VerifyBundle(
		mediaReceipt.directory, mediaReceipt.manifestSHA256,
	)
	if err != nil || !reflect.DeepEqual(verifiedMedia, mediaReceipt.manifest) {
		return errors.New("verify deterministic source media for evaluation adoption")
	}
	stem := fmt.Sprintf("%02d-%s-trial-01", indexed.Ordinal, reviewSlug(caseID))
	evaluationDirectory := filepath.Join(bundle.directory, "reviews", stem)
	receipt.Directory = evaluationDirectory
	if nilInterface(bundle.evaluationStores) {
		return errors.New("adopted realtime computer-use evaluation has no external receipt store")
	}
	store, err := bundle.evaluationStores.Store(caseID, evaluationDirectory)
	if err != nil {
		return fmt.Errorf("open adopted realtime computer-use evaluation receipt store: %w", err)
	}
	if nilInterface(store) {
		return errors.New("adopted realtime computer-use evaluation receipt store is nil")
	}
	quarantineDirectory, err := bundle.evaluationStores.Quarantine(caseID, evaluationDirectory)
	if err != nil {
		return fmt.Errorf("open adopted realtime computer-use evaluation quarantine: %w", err)
	}
	options := revieweval.EvaluationBundleOptions{
		Directory: evaluationDirectory, SensitiveValues: slices.Clone(bundle.sensitive),
	}
	publication, preparation, err := revieweval.BeginEvaluationBundlePublication(
		ctx, revieweval.EvaluationBundlePublicationConfig{
			Bundle: options, ReceiptStore: store, QuarantineDirectory: quarantineDirectory,
		},
	)
	if err != nil {
		return fmt.Errorf("begin adopted realtime computer-use evaluation publication: %w", err)
	}
	closePublication := func(primary error) error {
		if closeErr := publication.Close(); closeErr != nil {
			return errors.Join(primary,
				errors.New("close adopted realtime computer-use evaluation publication"), closeErr)
		}
		return primary
	}
	if preparation.Recovered == nil {
		return closePublication(
			errors.New("adopted realtime computer-use evaluation lacks its exact durable receipt"),
		)
	}
	opened := *preparation.Recovered
	if !sameEvaluationBundlePortableReceipt(receipt, opened.Receipt) {
		return closePublication(
			errors.New("adopted realtime computer-use evaluation differs from its durable receipt"),
		)
	}
	attemptID := secondaryReviewAttemptID(reviewDigest(bundle.finishSource), caseID)
	if err := validateOpenedSecondaryReview(
		opened, attemptID, indexed, contextPayload,
		verifiedMedia.ReviewMedia(), verifiedMedia.AttemptEndUS,
	); err != nil {
		return closePublication(err)
	}
	if err := closePublication(nil); err != nil {
		return err
	}
	if !sameEvaluationBundlePortableReceipt(receipt, opened.Receipt) {
		return errors.New("adopted realtime computer-use evaluation lacks its exact durable receipt")
	}
	if after, verifyErr := reviewmedia.VerifyBundle(
		mediaReceipt.directory, mediaReceipt.manifestSHA256,
	); verifyErr != nil || !reflect.DeepEqual(after, mediaReceipt.manifest) {
		return errors.New("deterministic source media changed during evaluation adoption")
	}
	if _, err := VerifyReviewSourceReceipt(bundle.directory, sourceReceipt); err != nil {
		return errors.New("deterministic source changed during evaluation adoption")
	}
	if existing, exists := bundle.evaluationReceipts[caseID]; exists &&
		!sameEvaluationBundlePortableReceipt(existing, opened.Receipt) {
		return errors.New("realtime computer-use evaluation receipt differs from the adopted receipt")
	}
	bundle.evaluationReceipts[caseID] = opened.Receipt
	return nil
}

func sameEvaluationBundlePortableReceipt(
	left, right revieweval.EvaluationBundleReceipt,
) bool {
	return left.ManifestSHA256 == right.ManifestSHA256 &&
		left.RecordSHA256 == right.RecordSHA256 &&
		left.FileSetSHA256 == right.FileSetSHA256 &&
		left.ReceiptSHA256 == right.ReceiptSHA256
}

// Close abandons an uncommitted bundle without deleting its diagnostic media,
// or becomes an idempotent no-op after FinishSuite. It closes admission and
// drains attempt-owned recorders before releasing the suite directory handle.
func (bundle *ReviewBundle) Close() error {
	if bundle == nil {
		return nil
	}
	for {
		bundle.mu.Lock()
		if bundle.closed || bundle.abandoning {
			done := bundle.closeDone
			bundle.mu.Unlock()
			<-done
			bundle.mu.Lock()
			err := bundle.closeErr
			bundle.mu.Unlock()
			return err
		}
		if bundle.finishing {
			done := bundle.finishAttemptDone
			bundle.mu.Unlock()
			<-done
			continue
		}
		if bundle.finished {
			bundle.closed = true
			close(bundle.closeDone)
			bundle.mu.Unlock()
			return nil
		}
		bundle.abandoning = true
		bundle.closing = true
		if bundle.active == 0 {
			bundle.drainOnce.Do(func() { close(bundle.drainDone) })
		}
		drainDone := bundle.drainDone
		bundle.mu.Unlock()
		<-drainDone
		break
	}

	bundle.mu.Lock()
	root := bundle.root
	bundle.root = nil
	bundle.mu.Unlock()
	var closeErr error
	if root != nil {
		if err := root.Close(); err != nil {
			_ = root.Close()
			closeErr = errors.New("close abandoned realtime computer-use review bundle")
		}
	}
	bundle.mu.Lock()
	bundle.abandoning = false
	bundle.abandoned = true
	bundle.closed = true
	bundle.finished = true
	bundle.closeErr = closeErr
	message := "realtime computer-use review bundle was closed without a suite commit"
	if bundle.sourceReceipt != nil {
		message = "realtime computer-use advisory review closed after the deterministic source committed"
	}
	bundle.finishErr = errors.Join(errors.New(message), closeErr)
	close(bundle.finishDone)
	close(bundle.closeDone)
	bundle.mu.Unlock()
	return closeErr
}

func (bundle *ReviewBundle) begin(caseID string) error {
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.root == nil || bundle.closing || bundle.finishing || bundle.finished {
		return errors.New("realtime computer-use review bundle is closing or closed")
	}
	if _, exists := bundle.completed[caseID]; exists {
		return errors.New("realtime computer-use review attempt was already completed")
	}
	if _, exists := bundle.failures[caseID]; exists {
		return errors.New("realtime computer-use review attempt was already started")
	}
	bundle.failures[caseID] = "attempt did not reach terminal evidence"
	bundle.active++
	return nil
}

func (bundle *ReviewBundle) release() {
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
		return nil, errors.New("realtime computer-use review bundle is nil")
	}
	if ctx == nil {
		return nil, errors.New("begin realtime computer-use review attempt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if err := specification.validate(); err != nil {
		return nil, err
	}
	specification, err := cloneEvidenceAttempt(specification)
	if err != nil {
		return nil, err
	}
	if err := bundle.begin(specification.Case); err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			bundle.release()
		}
	}()
	ordinal, ok := caseOrdinal(specification.Case)
	if !ok {
		bundle.recordFailure(specification.Case, "case is outside the v1 suite")
		return nil, errors.New("realtime computer-use review case is outside the v1 suite")
	}
	encoder, attestor, err := bundle.videoFactory.NewReviewVideo(ctx, specification)
	if err != nil || nilInterface(encoder) || nilInterface(attestor) {
		if !nilInterface(encoder) {
			_ = encoder.Close()
		}
		if !nilInterface(attestor) {
			_ = attestor.Close()
		}
		bundle.recordFailure(specification.Case, "video evidence plug-in creation failed")
		return nil, errors.New("create realtime computer-use video evidence plug-ins")
	}
	relativeDirectory := filepath.ToSlash(filepath.Join(
		"media", fmt.Sprintf("%02d-%s-trial-01", ordinal, reviewSlug(specification.Case)),
	))
	expectedSources := []string{"screen"}
	if specification.Task.Camera {
		expectedSources = append(expectedSources, "camera")
	}
	recorder, err := reviewmedia.New(ctx, reviewmedia.Config{
		Directory:    filepath.Join(bundle.directory, filepath.FromSlash(relativeDirectory)),
		RequireAudio: true, ExpectedVideoSources: expectedSources,
		Encoder: encoder, Attestor: attestor, SensitiveValues: slices.Clone(bundle.sensitive),
	})
	if err != nil {
		bundle.recordFailure(specification.Case, "media recorder creation failed")
		return nil, errors.New("create realtime computer-use media recorder")
	}
	attempt := &reviewBundleAttempt{
		bundle: bundle, specification: specification, recorder: recorder,
		ordinal: ordinal, relativeDirectory: relativeDirectory,
	}
	owned = true
	return attempt, nil
}

type reviewBundleAttempt struct {
	mu                sync.Mutex
	bundle            *ReviewBundle
	specification     EvidenceAttempt
	recorder          *reviewmedia.Recorder
	ordinal           int
	relativeDirectory string
	audio             bench.SessionAudioCapture
	latestVideoMS     float64
	audioCaptured     bool
	videoCaptured     bool
	terminal          bool
}

func (attempt *reviewBundleAttempt) CaptureAudio(capture bench.SessionAudioCapture) error {
	if attempt == nil || attempt.recorder == nil {
		return errors.New("realtime computer-use review attempt is nil")
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("realtime computer-use review attempt is terminal")
	}
	if attempt.audioCaptured {
		return errors.New("realtime computer-use review audio was already captured")
	}
	if _, err := alignedReviewAudioEnd(capture); err != nil {
		return err
	}
	attempt.audio = cloneReviewAudioCapture(capture)
	attempt.audioCaptured = true
	return nil
}

func (attempt *reviewBundleAttempt) CaptureVideo(capture bench.SessionVideoCapture) error {
	if attempt == nil || attempt.recorder == nil {
		return errors.New("realtime computer-use review attempt is nil")
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("realtime computer-use review attempt is terminal")
	}
	if err := attempt.recorder.CaptureVideo(capture); err != nil {
		return err
	}
	if !attempt.videoCaptured || capture.EpisodeAtMS > attempt.latestVideoMS {
		attempt.latestVideoMS = capture.EpisodeAtMS
	}
	attempt.videoCaptured = true
	return nil
}

func (attempt *reviewBundleAttempt) Complete(
	ctx context.Context, completion EvidenceCompletion,
) error {
	if attempt == nil || attempt.recorder == nil {
		return errors.New("realtime computer-use review attempt is nil")
	}
	if ctx == nil {
		return errors.New("complete realtime computer-use review attempt: nil context")
	}
	attempt.mu.Lock()
	if attempt.terminal {
		attempt.mu.Unlock()
		return errors.New("realtime computer-use review attempt is terminal")
	}
	attempt.terminal = true
	audioCaptured, videoCaptured := attempt.audioCaptured, attempt.videoCaptured
	audio := cloneReviewAudioCapture(attempt.audio)
	latestVideoMS := attempt.latestVideoMS
	attempt.mu.Unlock()
	defer attempt.bundle.release()
	completion, err := cloneEvidenceCompletion(completion)
	if err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "terminal evidence snapshot failed")
		return err
	}
	if !equalEvidenceAttempt(completion.Attempt, attempt.specification) ||
		completion.Outcome.ID != attempt.specification.Case {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "terminal evidence identity drifted")
		return errors.New("realtime computer-use review completion identity drifted")
	}
	if err := validateReviewCompletionEvidence(completion); err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "execution evidence drifted")
		return err
	}
	if !audioCaptured {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "required audio was not captured")
		return errors.New("realtime computer-use review attempt is missing required audio")
	}
	if !videoCaptured {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "required video was not captured")
		return errors.New("realtime computer-use review attempt is missing required video")
	}
	paddedAudio, attemptEndMS, err := padReviewAudioAfterVideo(audio, latestVideoMS)
	if err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "audiovisual timeline is invalid")
		return err
	}
	if err := attempt.recorder.CaptureAudio(paddedAudio); err != nil {
		_ = attempt.recorder.Abort()
		attempt.bundle.recordFailure(attempt.specification.Case, "audio retention failed")
		return errors.New("retain realtime computer-use review audio")
	}
	retentionCtx, cancelRetention := boundedReviewRetentionContext(
		ctx, reviewAttemptRetentionTimeout,
	)
	defer cancelRetention()
	receipt, err := attempt.recorder.Finalize(retentionCtx, attemptEndMS)
	if err != nil {
		attempt.bundle.recordFailure(attempt.specification.Case, "media finalization failed")
		return fmt.Errorf("finalize realtime computer-use review media: %w", err)
	}
	verified, err := reviewmedia.VerifyBundle(attempt.recorder.Directory(), receipt.ManifestSHA256)
	if err != nil || !reflect.DeepEqual(verified, receipt.Manifest) {
		attempt.bundle.recordFailure(attempt.specification.Case, "media verification failed")
		return errors.New("verify finalized realtime computer-use review media")
	}
	manifestPath := filepath.Join(attempt.recorder.Directory(), "manifest.json")
	manifestPayload, err := os.ReadFile(manifestPath)
	if err != nil || len(manifestPayload) == 0 || reviewDigest(manifestPayload) != receipt.ManifestSHA256 {
		attempt.bundle.recordFailure(attempt.specification.Case, "media manifest disappeared after verification")
		return errors.New("verified realtime computer-use media manifest is unavailable")
	}
	media, err := retainedReviewMedia(
		attempt.recorder.Directory(), attempt.relativeDirectory, attempt.specification, verified,
	)
	if err != nil {
		attempt.bundle.recordFailure(attempt.specification.Case, "retained media identity failed")
		return err
	}
	if err := attempt.bundle.recordCompletion(retainedReviewCompletion{
		specification: attempt.specification, completion: completion,
		ordinal: attempt.ordinal, relativeDirectory: attempt.relativeDirectory,
		media: media, mediaReceipt: retainedMediaReceipt{
			directory: attempt.recorder.Directory(), manifestSHA256: receipt.ManifestSHA256,
			manifest: verified,
		},
	}); err != nil {
		attempt.bundle.recordFailure(attempt.specification.Case, "terminal evidence snapshot failed")
		return err
	}
	return nil
}

func validateReviewCompletionEvidence(completion EvidenceCompletion) error {
	if completion.Outcome.ExecutionError != completion.Transcript.ExecutionError ||
		!reflect.DeepEqual(completion.Outcome.Execution, completion.Transcript.Execution) {
		return errors.New("realtime computer-use transcript execution evidence differs from its outcome")
	}
	if completion.Outcome.Execution != nil &&
		completion.Outcome.Execution.Scope != completion.Attempt.Case {
		return errors.New("realtime computer-use execution evidence scope differs from its case")
	}
	return nil
}

func boundedReviewRetentionContext(
	parent context.Context, timeout time.Duration,
) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = reviewAttemptRetentionTimeout
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (attempt *reviewBundleAttempt) Abort() error {
	if attempt == nil || attempt.recorder == nil {
		return nil
	}
	attempt.mu.Lock()
	terminal := attempt.terminal
	attempt.terminal = true
	attempt.mu.Unlock()
	if terminal {
		return nil
	}
	defer attempt.bundle.release()
	attempt.bundle.recordFailure(attempt.specification.Case, "attempt aborted before evidence completion")
	return attempt.recorder.Abort()
}

type realtimeCUReviewContext struct {
	Format               string                     `json:"format"`
	Version              int                        `json:"version"`
	Suite                string                     `json:"suite"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	ResultSHA256         string                     `json:"result_sha256"`
	Cell                 bench.Cell                 `json:"cell"`
	Provenance           bench.Provenance           `json:"provenance"`
	Task                 Task                       `json:"task"`
	Grounding            Grounding                  `json:"grounding"`
	RunOrigin            EvidenceRunOrigin          `json:"run_origin"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
	Observers            []string                   `json:"observers,omitempty"`
	Outcome              bench.TaskOutcome          `json:"deterministic_outcome"`
	Transcript           bench.Transcript           `json:"transcript"`
	Page                 PageResult                 `json:"page_result"`
	Actions              []realtimeCUReviewAction   `json:"actions"`
}

type realtimeCUReviewAction struct {
	CallID      string          `json:"call_id"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
	ReceivedAt  string          `json:"received_at,omitempty"`
	CompletedAt string          `json:"completed_at,omitempty"`
	Error       string          `json:"error,omitempty"`
}

func retainedReviewActions(source []ActionRecord) []realtimeCUReviewAction {
	result := make([]realtimeCUReviewAction, len(source))
	for index, action := range source {
		result[index] = realtimeCUReviewAction{
			CallID: action.CallID, Name: action.Name, Arguments: slices.Clone(action.Arguments),
			Error: action.Error,
		}
		if !action.ReceivedAt.IsZero() {
			result[index].ReceivedAt = action.ReceivedAt.UTC().Format(time.RFC3339Nano)
		}
		if !action.CompletedAt.IsZero() {
			result[index].CompletedAt = action.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return result
}

func retainedReviewContext(
	result bench.Result, resultPayload []byte, source retainedReviewCompletion, sensitive []string,
) ([]byte, error) {
	transcript, err := cloneTranscript(source.completion.Transcript)
	if err != nil {
		return nil, err
	}
	value := realtimeCUReviewContext{
		Format: ReviewContextFormat, Version: ReviewContextVersion,
		Suite: SuiteName, Case: source.specification.Case, Trial: 1,
		ResultSHA256: reviewDigest(resultPayload), Cell: cloneCell(result.Cell),
		Provenance:           result.Provenance,
		Task:                 cloneCase(Case{Task: source.specification.Task}).Task,
		Grounding:            source.specification.Grounding,
		RunOrigin:            source.specification.Origin,
		Observers:            slices.Clone(source.specification.Observers),
		ExecutionRequirement: source.specification.ExecutionRequirement,
		Outcome:              cloneTaskOutcome(source.completion.Outcome),
		Transcript:           transcript,
		Page:                 source.completion.Page,
		Actions:              retainedReviewActions(source.completion.Actions),
	}
	payload, err := canonicalReviewObject(value, maximumReviewContextBytes)
	if err != nil || len(payload) == 0 || len(payload) >= maximumReviewContextBytes ||
		strictjson.Validate(payload) != nil || containsReviewSecret(sensitive, payload) {
		return nil, errors.New("realtime computer-use review context is invalid, oversized, or sensitive")
	}
	return payload, nil
}

func canonicalReviewObject(value any, maximum int) ([]byte, error) {
	encode := func(value any) ([]byte, error) {
		var output bytes.Buffer
		encoder := json.NewEncoder(&output)
		// The provider-neutral review API defines canonical context JSON with
		// HTML escaping disabled. Keep the retained source byte-identical to
		// that contract even when a deterministic failure contains an HTML
		// response body.
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(value); err != nil {
			return nil, err
		}
		return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
	}
	source, err := encode(value)
	if err != nil || len(source) == 0 || len(source) > maximum || strictjson.Validate(source) != nil {
		return nil, errors.New("encode canonical realtime computer-use review JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("decode canonical realtime computer-use review JSON")
	}
	payload, err := encode(object)
	if err != nil || len(payload) == 0 || len(payload) > maximum || strictjson.Validate(payload) != nil {
		return nil, errors.New("encode canonical realtime computer-use review JSON")
	}
	return payload, nil
}

func materializeReviewAttempts(
	root *os.Root, result bench.Result, resultPayload []byte,
	completed map[string]retainedReviewCompletion, sensitive []string,
) (map[string]ReviewAttempt, map[string]retainedMediaReceipt, error) {
	attempts := make(map[string]ReviewAttempt, len(completed))
	mediaReceipts := make(map[string]retainedMediaReceipt, len(completed))
	for id, source := range completed {
		if !reflect.DeepEqual(source.specification.ExecutionRequirement, result.Cell.Execution) {
			return nil, nil, errors.New("realtime computer-use evidence execution requirement differs from the final cell")
		}
		if !equalEvidenceAttempt(source.completion.Attempt, source.specification) ||
			source.completion.Outcome.ID != id {
			return nil, nil, errors.New("realtime computer-use retained completion identity drifted")
		}
		contextPayload, err := retainedReviewContext(result, resultPayload, source, sensitive)
		if err != nil {
			return nil, nil, err
		}
		stem := fmt.Sprintf("%02d-%s-trial-01", source.ordinal, reviewSlug(id))
		contextPath := filepath.ToSlash(filepath.Join("contexts", stem+".json"))
		contextArtifact, err := writeRetainedReviewArtifact(
			root, contextPath, contextPayload, "application/json", sensitive,
		)
		if err != nil {
			return nil, nil, err
		}
		attempt := ReviewAttempt{
			Ordinal: source.ordinal, Case: id, Trial: 1,
			Grounding:     source.specification.Grounding,
			Observers:     slices.Clone(source.specification.Observers),
			Deterministic: cloneTaskOutcome(source.completion.Outcome), Context: contextArtifact,
			MediaBundle: NestedBundleReceipt{
				Path:           source.relativeDirectory,
				ManifestSHA256: source.mediaReceipt.manifestSHA256,
			},
			Media: slices.Clone(source.media), ReviewStatus: "not_configured",
			RunOrigin: source.specification.Origin,
			ExecutionStatus: executionStatus(
				source.specification.ExecutionRequirement, source.completion.Outcome,
			),
		}
		attempt.Reportable, attempt.ReportabilityNote = attemptReportability(
			source.specification.Origin, source.specification.ExecutionRequirement,
			source.completion.Outcome, attempt.ReviewStatus,
		)
		attempts[id] = attempt
		mediaReceipts[id] = source.mediaReceipt.clone()
	}
	return attempts, mediaReceipts, nil
}

func attachSecondaryReviews(
	ctx, retentionCtx context.Context, root *os.Root, directory string, resultPayload []byte,
	attempts map[string]ReviewAttempt, mediaReceipts map[string]retainedMediaReceipt,
	completed map[string]retainedReviewCompletion, reviewer *revieweval.ProviderLease,
	evaluationStores ReviewEvaluationReceiptStoreFactory,
	concurrency int, sensitive []string,
	evaluationReceipts map[string]revieweval.EvaluationBundleReceipt,
) error {
	// A caller may resume with externally anchored evaluation receipts after the
	// original provider lease has gone away. Those siblings remain independently
	// adoptable: a live lease is needed only for cases that have no receipt yet.
	if reviewer == nil && len(evaluationReceipts) == 0 && nilInterface(evaluationStores) {
		return nil
	}
	if concurrency < 1 || concurrency > 16 {
		return errors.New("realtime computer-use review concurrency is invalid")
	}
	ordered := make([]ReviewAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		ordered = append(ordered, cloneReviewAttempt(attempt))
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].Ordinal < ordered[right].Ordinal
	})
	type reviewJob struct {
		attempt    ReviewAttempt
		media      retainedMediaReceipt
		completion retainedReviewCompletion
	}
	jobs := make([]reviewJob, 0, len(ordered))
	for _, indexed := range ordered {
		receipt, mediaExists := mediaReceipts[indexed.Case]
		source, completionExists := completed[indexed.Case]
		if !mediaExists || !completionExists {
			return errors.New("realtime computer-use reviewer input is missing retained evidence")
		}
		snapshot, err := source.clone()
		if err != nil {
			return errors.New("snapshot realtime computer-use reviewer input")
		}
		jobs = append(jobs, reviewJob{
			attempt: cloneReviewAttempt(indexed), media: receipt.clone(), completion: snapshot,
		})
	}
	type completedReview struct {
		ordinal int
		caseID  string
		attempt ReviewAttempt
		receipt *revieweval.EvaluationBundleReceipt
		err     error
	}
	results := make(chan completedReview, len(jobs))
	semaphore := make(chan struct{}, min(concurrency, max(1, len(jobs))))
	var workers sync.WaitGroup
	for _, job := range jobs {
		job := job
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- completedReview{
					ordinal: job.attempt.Ordinal, caseID: job.attempt.Case, err: context.Cause(ctx),
				}
				return
			}
			defer func() { <-semaphore }()
			localAttempts := map[string]ReviewAttempt{job.attempt.Case: job.attempt}
			localMedia := map[string]retainedMediaReceipt{
				job.attempt.Case: job.media,
			}
			localCompleted := map[string]retainedReviewCompletion{
				job.attempt.Case: job.completion,
			}
			localEvaluations := make(map[string]revieweval.EvaluationBundleReceipt, 1)
			if receipt, exists := evaluationReceipts[job.attempt.Case]; exists {
				localEvaluations[job.attempt.Case] = receipt
			}
			err := attachSecondaryReviewsSerial(
				ctx, retentionCtx, root, directory, resultPayload, localAttempts, localMedia,
				localCompleted, reviewer, evaluationStores, sensitive, localEvaluations,
			)
			if err != nil {
				err = fmt.Errorf(
					"review realtime computer-use case %q: %w", job.attempt.Case, err,
				)
			}
			var receipt *revieweval.EvaluationBundleReceipt
			if retained, exists := localEvaluations[job.attempt.Case]; exists {
				copy := retained
				receipt = &copy
			}
			results <- completedReview{
				ordinal: job.attempt.Ordinal, caseID: job.attempt.Case,
				attempt: localAttempts[job.attempt.Case], receipt: receipt, err: err,
			}
		}()
	}
	workers.Wait()
	close(results)
	completedReviews := make([]completedReview, 0, len(jobs))
	for result := range results {
		completedReviews = append(completedReviews, result)
	}
	sort.Slice(completedReviews, func(left, right int) bool {
		return completedReviews[left].ordinal < completedReviews[right].ordinal
	})
	var firstErr error
	for _, result := range completedReviews {
		if result.receipt != nil {
			evaluationReceipts[result.caseID] = *result.receipt
		}
		if result.err == nil {
			attempts[result.caseID] = result.attempt
		} else if firstErr == nil {
			firstErr = result.err
		}
	}
	return firstErr
}

func attachSecondaryReviewsSerial(
	ctx, retentionCtx context.Context, root *os.Root, directory string, resultPayload []byte,
	attempts map[string]ReviewAttempt, mediaReceipts map[string]retainedMediaReceipt,
	completed map[string]retainedReviewCompletion, reviewer *revieweval.ProviderLease,
	evaluationStores ReviewEvaluationReceiptStoreFactory,
	sensitive []string, evaluationReceipts map[string]revieweval.EvaluationBundleReceipt,
) error {
	ordered := make([]ReviewAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		ordered = append(ordered, cloneReviewAttempt(attempt))
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].Ordinal < ordered[right].Ordinal
	})
	resultSHA := reviewDigest(resultPayload)
	for _, indexed := range ordered {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		source, exists := completed[indexed.Case]
		receipt, mediaExists := mediaReceipts[indexed.Case]
		if !exists || !mediaExists {
			return errors.New("realtime computer-use reviewer input is missing retained evidence")
		}
		verifiedMedia, err := reviewmedia.VerifyBundle(receipt.directory, receipt.manifestSHA256)
		if err != nil || !reflect.DeepEqual(verifiedMedia, receipt.manifest) {
			return errors.New("reverify realtime computer-use media before secondary review")
		}
		contextPayload, _, err := readReviewFile(root, indexed.Context.Path, maximumReviewContextBytes)
		if err != nil || !artifactMatches(indexed.Context, contextPayload) {
			return errors.New("read exact realtime computer-use secondary review context")
		}
		attemptID := secondaryReviewAttemptID(resultSHA, indexed.Case)
		media := verifiedMedia.ReviewMedia()
		stem := fmt.Sprintf("%02d-%s-trial-01", indexed.Ordinal, reviewSlug(indexed.Case))
		relativeDirectory := filepath.ToSlash(filepath.Join("reviews", stem))
		evaluationDirectory := filepath.Join(directory, filepath.FromSlash(relativeDirectory))
		options := revieweval.EvaluationBundleOptions{
			Directory: evaluationDirectory, SensitiveValues: slices.Clone(sensitive),
		}
		if nilInterface(evaluationStores) {
			if _, exists := evaluationReceipts[indexed.Case]; exists || reviewer != nil {
				return errors.New("realtime computer-use evaluation has no external receipt store")
			}
			continue
		}
		store, err := evaluationStores.Store(indexed.Case, evaluationDirectory)
		if err != nil {
			return fmt.Errorf("open realtime computer-use evaluation receipt store: %w", err)
		}
		if nilInterface(store) {
			return errors.New("realtime computer-use evaluation receipt store factory returned nil")
		}
		quarantineDirectory, err := evaluationStores.Quarantine(
			indexed.Case, evaluationDirectory,
		)
		if err != nil {
			return fmt.Errorf("open realtime computer-use evaluation quarantine: %w", err)
		}
		publication, preparation, err := revieweval.BeginEvaluationBundlePublication(
			retentionCtx, revieweval.EvaluationBundlePublicationConfig{
				Bundle: options, ReceiptStore: store,
				QuarantineDirectory: quarantineDirectory,
			},
		)
		if err != nil {
			return fmt.Errorf("begin realtime computer-use evaluation publication: %w", err)
		}
		var opened revieweval.EvaluationBundle
		publicationErr := func() (resultErr error) {
			defer func() {
				if closeErr := publication.Close(); closeErr != nil {
					resultErr = errors.Join(resultErr,
						errors.New("close realtime computer-use evaluation publication"), closeErr)
				}
			}()
			if preparation.Recovered != nil {
				opened = *preparation.Recovered
				if adopted, exists := evaluationReceipts[indexed.Case]; exists {
					adopted.Directory = evaluationDirectory
					if !sameEvaluationBundlePortableReceipt(adopted, opened.Receipt) {
						return errors.New("durable realtime computer-use evaluation receipt differs from retry state")
					}
				}
				return nil
			}
			if _, exists := evaluationReceipts[indexed.Case]; exists {
				return errors.New("realtime computer-use retry receipt is absent from its durable store")
			}
			if reviewer == nil {
				return nil
			}
			evaluation, evaluateErr := revieweval.Evaluate(ctx, reviewer, revieweval.Request{
				AttemptID: attemptID, Suite: SuiteName, Case: indexed.Case, Trial: indexed.Trial,
				RootDirectory: receipt.directory, Context: json.RawMessage(slices.Clone(contextPayload)),
				Media: media, SensitiveValues: slices.Clone(sensitive),
			})
			if evaluateErr != nil {
				return fmt.Errorf("evaluate realtime computer-use retained media: %w", evaluateErr)
			}
			if err := validateSecondaryReviewEvaluation(
				evaluation, attemptID, indexed, contextPayload, media, verifiedMedia.AttemptEndUS,
			); err != nil {
				return err
			}
			// The provider was given immutable snapshots, but the separately retained
			// source is independently verified again before those snapshots are
			// published as provider-reviewed evidence.
			if after, verifyErr := reviewmedia.VerifyBundle(receipt.directory, receipt.manifestSHA256); verifyErr != nil || !reflect.DeepEqual(after, receipt.manifest) {
				return errors.New("realtime computer-use source media changed during secondary review")
			}
			evaluationReceipt, writeErr := publication.Write(retentionCtx, evaluation)
			if writeErr != nil {
				return fmt.Errorf("retain sealed realtime computer-use secondary review: %w", writeErr)
			}
			opened, err = revieweval.VerifyEvaluationBundle(
				retentionCtx, options, evaluationReceipt,
			)
			if err != nil {
				return fmt.Errorf("verify sealed realtime computer-use secondary review: %w", err)
			}
			if !reflect.DeepEqual(opened.Record, evaluation.Record) {
				return errors.New("retained realtime computer-use secondary review record drifted")
			}
			return nil
		}()
		if opened.Receipt.ReceiptSHA256 != "" {
			evaluationReceipts[indexed.Case] = opened.Receipt
		}
		if publicationErr != nil {
			return publicationErr
		}
		if preparation.Recovered == nil && reviewer == nil {
			continue
		}
		if err := validateOpenedSecondaryReview(
			opened, attemptID, indexed, contextPayload, media, verifiedMedia.AttemptEndUS,
		); err != nil {
			return err
		}
		evaluationReceipts[indexed.Case] = opened.Receipt
		openedAfterAnchor, verifyErr := revieweval.VerifyEvaluationBundle(
			retentionCtx, options, opened.Receipt,
		)
		if verifyErr != nil || !reflect.DeepEqual(openedAfterAnchor.Record, opened.Record) {
			return errors.New("reverify sealed realtime computer-use evaluation after staged publication")
		}
		if after, verifyErr := reviewmedia.VerifyBundle(receipt.directory, receipt.manifestSHA256); verifyErr != nil || !reflect.DeepEqual(after, receipt.manifest) {
			return errors.New("realtime computer-use source media changed after secondary review retention")
		}
		attempt := attempts[indexed.Case]
		descriptor := opened.Record.Provider
		assessment := cloneReviewAssessment(opened.Record.Assessment)
		attempt.ReviewStatus = "complete"
		attempt.EvaluationBundle = &NestedEvaluationReceipt{
			Path: relativeDirectory, ManifestSHA256: opened.Receipt.ManifestSHA256,
			RecordSHA256:  opened.Receipt.RecordSHA256,
			FileSetSHA256: opened.Receipt.FileSetSHA256,
			ReceiptSHA256: opened.Receipt.ReceiptSHA256,
		}
		attempt.Reviewer = &descriptor
		attempt.Assessment = &assessment
		attempt.EvidenceScope = opened.Manifest.EvidenceScope
		attempt.IndependentRemoteAttestation = opened.Manifest.IndependentRemoteAttestation
		attempt.AuthenticityCaveat = opened.Manifest.AuthenticityCaveat
		attempt.Reportable, attempt.ReportabilityNote = attemptReportability(
			source.specification.Origin, source.specification.ExecutionRequirement,
			source.completion.Outcome, attempt.ReviewStatus,
		)
		attempts[indexed.Case] = attempt
		evaluationReceipts[indexed.Case] = opened.Receipt
	}
	return nil
}

func secondaryReviewAttemptID(resultSHA, caseID string) string {
	return SuiteName + "/" + strings.TrimPrefix(resultSHA, "sha256:") + "/" + caseID + "/trial-1"
}

func evaluationReceiptAt(
	directory string, source NestedEvaluationReceipt,
) revieweval.EvaluationBundleReceipt {
	return revieweval.EvaluationBundleReceipt{
		Directory: directory, ManifestSHA256: source.ManifestSHA256,
		RecordSHA256: source.RecordSHA256, FileSetSHA256: source.FileSetSHA256,
		ReceiptSHA256: source.ReceiptSHA256,
	}
}

func sameNestedEvaluationReceipt(
	actual revieweval.EvaluationBundleReceipt, expected NestedEvaluationReceipt,
) bool {
	return actual.ManifestSHA256 == expected.ManifestSHA256 &&
		actual.RecordSHA256 == expected.RecordSHA256 &&
		actual.FileSetSHA256 == expected.FileSetSHA256 &&
		actual.ReceiptSHA256 == expected.ReceiptSHA256
}

func validateSecondaryReviewEvaluation(
	evaluation revieweval.Evaluation, attemptID string, attempt ReviewAttempt,
	contextPayload []byte, media []revieweval.Media, attemptEndUS int64,
) error {
	if evaluation.Record.AttemptID != attemptID || evaluation.Record.Suite != SuiteName ||
		evaluation.Record.Case != attempt.Case || evaluation.Record.Trial != attempt.Trial {
		return errors.New("realtime computer-use secondary review attempt identity drifted")
	}
	if evaluation.Record.ContextSHA256 != reviewDigest(contextPayload) ||
		!bytes.Equal(evaluation.Context, contextPayload) {
		return errors.New("realtime computer-use secondary review context drifted")
	}
	if !reviewedMediaMatches(evaluation.Record.Media, media) {
		return errors.New("realtime computer-use secondary review media identities drifted")
	}
	if len(evaluation.Media) != len(evaluation.Record.Media) {
		return errors.New("realtime computer-use secondary review media snapshot is incomplete")
	}
	for index := range evaluation.Record.Media {
		if !reflect.DeepEqual(evaluation.Media[index].Media, evaluation.Record.Media[index]) ||
			int64(len(evaluation.Media[index].Bytes)) != evaluation.Record.Media[index].SizeBytes ||
			reviewDigest(evaluation.Media[index].Bytes) != evaluation.Record.Media[index].SHA256 {
			return errors.New("realtime computer-use secondary review media snapshot drifted")
		}
	}
	return validateReviewAssessmentTimeline(evaluation.Record.Assessment, attemptEndUS)
}

func validateOpenedSecondaryReview(
	bundle revieweval.EvaluationBundle, attemptID string, attempt ReviewAttempt,
	contextPayload []byte, media []revieweval.Media, attemptEndUS int64,
) error {
	if bundle.Manifest.AttemptID != attemptID || bundle.Manifest.Suite != SuiteName ||
		bundle.Manifest.Case != attempt.Case || bundle.Manifest.Trial != attempt.Trial ||
		bundle.Manifest.IndependentRemoteAttestation ||
		strings.TrimSpace(bundle.Manifest.EvidenceScope) == "" ||
		strings.TrimSpace(bundle.Manifest.AuthenticityCaveat) == "" ||
		bundle.Record.ContextSHA256 != reviewDigest(contextPayload) ||
		!reviewedMediaMatches(bundle.Record.Media, media) {
		return errors.New("sealed realtime computer-use secondary review is not bound to its source evidence")
	}
	return validateReviewAssessmentTimeline(bundle.Record.Assessment, attemptEndUS)
}

func reviewedMediaMatches(actual, source []revieweval.Media) bool {
	if len(actual) != len(source) {
		return false
	}
	for index := range actual {
		expected := source[index]
		expected.Validation = revieweval.MediaValidationVersion
		if actual[index].SizeBytes <= 0 ||
			(source[index].SizeBytes > 0 && source[index].SizeBytes != actual[index].SizeBytes) {
			return false
		}
		expected.SizeBytes = actual[index].SizeBytes
		if !reflect.DeepEqual(actual[index], expected) {
			return false
		}
	}
	return true
}

func validateReviewAssessmentTimeline(assessment revieweval.Assessment, attemptEndUS int64) error {
	if attemptEndUS <= 0 {
		return errors.New("realtime computer-use review media has no bounded timeline")
	}
	maximumMS := attemptEndUS / 1_000
	for _, findings := range [][]revieweval.Finding{
		assessment.SignificantProblems, assessment.MinorObservations,
	} {
		for _, finding := range findings {
			for _, timestamp := range []*int64{finding.StartMS, finding.EndMS} {
				if timestamp != nil && (*timestamp < 0 || *timestamp > maximumMS) {
					return errors.New("realtime computer-use secondary review finding exceeds retained media")
				}
			}
		}
	}
	return nil
}

func cloneReviewAssessment(source revieweval.Assessment) revieweval.Assessment {
	result := source
	cloneFindings := func(findings []revieweval.Finding) []revieweval.Finding {
		cloned := slices.Clone(findings)
		for index := range cloned {
			if findings[index].StartMS != nil {
				value := *findings[index].StartMS
				cloned[index].StartMS = &value
			}
			if findings[index].EndMS != nil {
				value := *findings[index].EndMS
				cloned[index].EndMS = &value
			}
		}
		return cloned
	}
	result.SignificantProblems = cloneFindings(source.SignificantProblems)
	result.MinorObservations = cloneFindings(source.MinorObservations)
	result.Limitations = slices.Clone(source.Limitations)
	return result
}

func writeRetainedReviewArtifact(
	root *os.Root, path string, payload []byte, mediaType string, sensitive []string,
) (ReviewArtifact, error) {
	if root == nil || containsReviewSecret(sensitive, payload) {
		return ReviewArtifact{}, errors.New("realtime computer-use review artifact is unavailable or sensitive")
	}
	if err := writeReviewFile(root, path, payload); err != nil {
		return ReviewArtifact{}, errors.New("retain realtime computer-use review artifact")
	}
	return ReviewArtifact{
		Path: path, SHA256: reviewDigest(payload), SizeBytes: int64(len(payload)), MediaType: mediaType,
	}, nil
}

func (bundle *ReviewBundle) recordCompletion(source retainedReviewCompletion) error {
	copy, err := source.clone()
	if err != nil {
		return err
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	bundle.completed[copy.specification.Case] = copy
	delete(bundle.failures, copy.specification.Case)
	return nil
}

func (bundle *ReviewBundle) recordFailure(caseID, reason string) {
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if _, complete := bundle.completed[caseID]; !complete {
		bundle.failures[caseID] = reason
	}
}

func (bundle *ReviewBundle) FinishSuite(ctx context.Context, source bench.Result) error {
	if bundle == nil {
		return errors.New("realtime computer-use review bundle is nil")
	}
	if ctx == nil {
		return errors.New("finish realtime computer-use review bundle: nil context")
	}
	result, err := cloneResult(source)
	if err != nil {
		return err
	}
	resultPayload, err := encodeReviewResult(result)
	if err != nil {
		return err
	}
	retentionCtx, cancelRetention := boundedReviewRetentionContext(
		ctx, reviewSuiteRetentionTimeout,
	)
	defer cancelRetention()
	bundle.mu.Lock()
	if !bundle.closing {
		bundle.closing = true
		if bundle.active == 0 {
			bundle.drainOnce.Do(func() { close(bundle.drainDone) })
		}
	}
	if len(bundle.finishSource) > 0 && !bytes.Equal(bundle.finishSource, resultPayload) {
		bundle.mu.Unlock()
		return errors.New("realtime computer-use review finish result differs from the committed input")
	}
	if len(bundle.finishSource) == 0 {
		bundle.finishSource = slices.Clone(resultPayload)
	}
	if bundle.closed || bundle.abandoning || bundle.abandoned {
		err := bundle.finishErr
		bundle.mu.Unlock()
		if err == nil {
			return errors.New("realtime computer-use review bundle is closed")
		}
		return err
	}
	if bundle.finished {
		err := bundle.finishErr
		bundle.mu.Unlock()
		return err
	}
	if bundle.finishing {
		done := bundle.finishAttemptDone
		bundle.mu.Unlock()
		select {
		case <-done:
			bundle.mu.Lock()
			err := bundle.lastFinishErr
			bundle.mu.Unlock()
			return err
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	bundle.finishing = true
	bundle.finishAttemptDone = make(chan struct{})
	attemptDone := bundle.finishAttemptDone
	drainDone := bundle.drainDone
	bundle.mu.Unlock()
	select {
	case <-drainDone:
	case <-retentionCtx.Done():
		cause := context.Cause(retentionCtx)
		bundle.mu.Lock()
		bundle.lastFinishErr = cause
		bundle.finishing = false
		close(attemptDone)
		bundle.mu.Unlock()
		return cause
	}

	bundle.mu.Lock()
	root := bundle.root
	completed := make(map[string]retainedReviewCompletion, len(bundle.completed))
	var snapshotErr error
	for id, source := range bundle.completed {
		copy, err := source.clone()
		if err != nil {
			snapshotErr = err
			break
		}
		completed[id] = copy
	}
	failures := make(map[string]string, len(bundle.failures))
	for id, failure := range bundle.failures {
		failures[id] = failure
	}
	sourceReceipt := bundle.sourceReceipt
	pendingSourceReceipt := bundle.pendingSourceReceipt
	sourceAttempts := cloneReviewAttemptMap(bundle.sourceAttempts)
	sourceMedia := cloneRetainedMediaReceipts(bundle.sourceMedia)
	evaluationReceipts := cloneEvaluationReceipts(bundle.evaluationReceipts)
	bundle.mu.Unlock()
	finishAttempt := func(resultErr error, terminal bool) error {
		bundle.mu.Lock()
		bundle.lastFinishErr = resultErr
		bundle.finishing = false
		if terminal {
			bundle.finishErr = resultErr
			bundle.finished = true
			close(bundle.finishDone)
		}
		close(attemptDone)
		bundle.mu.Unlock()
		return resultErr
	}
	terminalFailure := func(resultErr error) error {
		bundle.mu.Lock()
		ownedRoot := bundle.root
		bundle.root = nil
		bundle.mu.Unlock()
		if ownedRoot != nil {
			if closeErr := ownedRoot.Close(); closeErr != nil {
				_ = ownedRoot.Close()
				resultErr = errors.Join(
					resultErr, errors.New("close realtime computer-use review bundle directory"),
				)
			}
		}
		root = nil
		return finishAttempt(resultErr, true)
	}
	if root == nil {
		return terminalFailure(errors.New("realtime computer-use review bundle directory is unavailable"))
	}
	if snapshotErr != nil {
		return terminalFailure(snapshotErr)
	}
	resultArtifact := ReviewArtifact{
		Path: "result.json", SHA256: reviewDigest(resultPayload),
		SizeBytes: int64(len(resultPayload)), MediaType: "application/json",
	}
	if sourceReceipt == nil && pendingSourceReceipt == nil {
		if containsReviewSecret(bundle.sensitive, resultPayload) {
			return terminalFailure(errors.New("exact realtime computer-use result contains a sensitive value"))
		}
		if err := writeReviewFile(root, "result.json", resultPayload); err != nil {
			return terminalFailure(errors.New("retain exact realtime computer-use result"))
		}
		sourceAttempts, sourceMedia, err = materializeReviewAttempts(
			root, result, resultPayload, completed, bundle.sensitive,
		)
		if err != nil {
			return terminalFailure(err)
		}
		sourceManifest, buildErr := buildReviewSourceManifest(
			result, resultArtifact, sourceAttempts, sourceMedia, failures,
		)
		if buildErr != nil {
			return terminalFailure(buildErr)
		}
		sourceManifestPayload, sourceMarkdown, encodeErr := encodeReviewPublication(
			sourceManifest, bundle.sensitive,
		)
		if encodeErr != nil {
			return terminalFailure(encodeErr)
		}
		if err := writeReviewFile(root, "SOURCE_REVIEW.md", sourceMarkdown); err != nil {
			return terminalFailure(errors.New("retain deterministic realtime computer-use source review"))
		}
		if err := writeReviewFile(root, reviewSourceManifestStage, sourceManifestPayload); err != nil {
			return terminalFailure(errors.New("stage deterministic realtime computer-use source manifest"))
		}
		candidate, receiptErr := buildReviewSourceReceipt(
			bundle.directory, sourceManifest, reviewDigest(sourceManifestPayload),
		)
		if receiptErr != nil {
			return terminalFailure(receiptErr)
		}
		sourceRootIdentity, identityErr := verifyReviewRootIdentity(bundle.directory, root, nil)
		if identityErr != nil {
			return terminalFailure(identityErr)
		}
		if err := sealReviewSourceArtifactsRootAtMarker(
			bundle.directory, root, sourceRootIdentity, reviewSourceManifestStage,
		); err != nil {
			return terminalFailure(
				errors.New("seal staged deterministic realtime computer-use source evidence"),
			)
		}
		bundle.mu.Lock()
		bundle.pendingSourceReceipt = &candidate
		bundle.sourceAttempts = cloneReviewAttemptMap(sourceAttempts)
		bundle.sourceMedia = cloneRetainedMediaReceipts(sourceMedia)
		bundle.mu.Unlock()
		publication, publicationErr := commitStagedReviewSource(
			retentionCtx, bundle.directory, candidate, bundle.sourceAnchor,
		)
		if publication.ReceiptDurable || publication.FinalVerified {
			sourceReceipt = &candidate
			bundle.mu.Lock()
			bundle.pendingSourceReceipt = nil
			bundle.sourceReceipt = &candidate
			bundle.sourceAnchored = publication.ReceiptDurable
			bundle.mu.Unlock()
		}
		if publicationErr != nil {
			return finishAttempt(publicationErr, false)
		}
		if !publication.FinalVerified || sourceReceipt == nil {
			return terminalFailure(errors.New(
				"deterministic realtime computer-use source did not reach final visibility",
			))
		}
	} else {
		candidate := sourceReceipt
		if candidate == nil {
			candidate = pendingSourceReceipt
		}
		publication, publicationErr := commitStagedReviewSource(
			retentionCtx, bundle.directory, *candidate, bundle.sourceAnchor,
		)
		if publication.ReceiptDurable || publication.FinalVerified {
			sourceReceipt = candidate
			bundle.mu.Lock()
			bundle.pendingSourceReceipt = nil
			bundle.sourceReceipt = candidate
			bundle.sourceAnchored = bundle.sourceAnchored || publication.ReceiptDurable
			bundle.mu.Unlock()
		}
		if publicationErr != nil {
			return finishAttempt(publicationErr, false)
		}
		if !publication.FinalVerified {
			return finishAttempt(errors.New(
				"deterministic realtime computer-use source did not reach final visibility",
			), false)
		}
		verifiedSource, verifyErr := VerifyReviewSourceReceipt(bundle.directory, *sourceReceipt)
		if verifyErr != nil || verifiedSource.Result.SHA256 != resultArtifact.SHA256 ||
			!reflect.DeepEqual(verifiedSource.Cell, result.Cell) ||
			!reflect.DeepEqual(verifiedSource.Provenance, result.Provenance) {
			return terminalFailure(errors.New("reverify deterministic realtime computer-use source evidence"))
		}
	}

	if bundle.reviewer != nil {
		bundle.mu.Lock()
		sourceAnchored := bundle.sourceAnchored
		bundle.mu.Unlock()
		if !sourceAnchored {
			return finishAttempt(
				errors.New("realtime computer-use source has no durable external receipt"), false,
			)
		}
	}

	attempts := cloneReviewAttemptMap(sourceAttempts)
	if err := attachSecondaryReviews(
		ctx, retentionCtx, root, bundle.directory, resultPayload, attempts, sourceMedia,
		completed, bundle.reviewer, bundle.evaluationStores,
		bundle.reviewConcurrency, bundle.sensitive,
		evaluationReceipts,
	); err != nil {
		bundle.mu.Lock()
		bundle.evaluationReceipts = cloneEvaluationReceipts(evaluationReceipts)
		bundle.mu.Unlock()
		return finishAttempt(err, false)
	}
	bundle.mu.Lock()
	bundle.evaluationReceipts = cloneEvaluationReceipts(evaluationReceipts)
	bundle.mu.Unlock()
	manifest, buildErr := buildReviewManifest(result, resultArtifact, attempts, sourceMedia, failures)
	if buildErr != nil {
		return terminalFailure(buildErr)
	}
	sourceManifestPayload, sourceInfo, err := readReviewFile(
		root, "source.manifest.json", maximumReviewManifestBytes,
	)
	if err != nil || reviewDigest(sourceManifestPayload) != sourceReceipt.ManifestSHA256 {
		return terminalFailure(errors.New("deterministic realtime computer-use source receipt changed"))
	}
	sourceArtifact := ReviewArtifact{
		Path: "source.manifest.json", SHA256: sourceReceipt.ManifestSHA256,
		SizeBytes: sourceInfo.Size(), MediaType: "application/json",
	}
	manifest.SourceManifest = &sourceArtifact
	manifestPayload, markdown, encodeErr := encodeReviewPublication(manifest, bundle.sensitive)
	if encodeErr != nil {
		return terminalFailure(encodeErr)
	}
	if err := writeReviewFile(root, "REVIEW.md", markdown); err != nil {
		return terminalFailure(errors.New("retain realtime computer-use human review"))
	}
	if err := writeReviewFile(root, "manifest.json", manifestPayload); err != nil {
		return terminalFailure(errors.New("publish realtime computer-use review manifest"))
	}
	manifestSHA := reviewDigest(manifestPayload)
	sealedRootIdentity, identityErr := verifyReviewRootIdentity(bundle.directory, root, nil)
	if identityErr != nil {
		return terminalFailure(identityErr)
	}
	if err := sealReviewTreeRoot(bundle.directory, root, sealedRootIdentity); err != nil {
		invalidateErr := invalidateReviewManifestRoot(root)
		return terminalFailure(errors.Join(
			errors.New("seal realtime computer-use review bundle"), invalidateErr,
		))
	}
	if err := root.Close(); err != nil {
		bundle.mu.Lock()
		bundle.root = nil
		bundle.mu.Unlock()
		root = nil
		invalidateErr := invalidateReviewManifestExpected(bundle.directory, sealedRootIdentity)
		return finishAttempt(errors.Join(
			errors.New("close sealed realtime computer-use review bundle"), invalidateErr,
		), true)
	}
	root = nil
	bundle.mu.Lock()
	bundle.root = nil
	bundle.mu.Unlock()
	expectedReceipt := ReviewBundleReceipt{
		Directory: bundle.directory, ManifestSHA256: manifestSHA,
		SourceManifestSHA256: sourceReceipt.ManifestSHA256,
		SourceReceiptSHA256:  sourceReceipt.ReceiptSHA256,
	}
	if _, verifyErr := VerifyReviewBundleReceipt(bundle.directory, expectedReceipt); verifyErr != nil {
		if invalidateErr := invalidateReviewManifestExpected(
			bundle.directory, sealedRootIdentity,
		); invalidateErr != nil {
			verifiedReceipt := ReviewBundleReceipt{}
			if _, freshErr := VerifyReviewBundleReceipt(
				bundle.directory, expectedReceipt,
			); freshErr == nil {
				verifiedReceipt = expectedReceipt
				bundle.mu.Lock()
				bundle.receipt = &verifiedReceipt
				bundle.mu.Unlock()
			}
			return finishAttempt(&ReviewBundlePublicationError{
				ExpectedReceipt: expectedReceipt, VerifiedReceipt: verifiedReceipt,
				MarkerMayRemain: true,
				cause: errors.Join(
					errors.New("verify sealed realtime computer-use review bundle"),
					verifyErr,
					errors.New("invalidate rejected realtime computer-use review manifest"),
				),
			}, true)
		}
		return finishAttempt(errors.Join(
			errors.New("verify sealed realtime computer-use review bundle"), verifyErr,
		), true)
	}
	currentRoot, currentErr := os.Lstat(bundle.directory)
	if currentErr != nil || currentRoot.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(currentRoot, sealedRootIdentity) {
		return finishAttempt(
			errors.New("realtime computer-use review root changed after final verification"), true,
		)
	}
	bundle.mu.Lock()
	bundle.receipt = &expectedReceipt
	bundle.mu.Unlock()
	if !manifest.Complete {
		return finishAttempt(fmt.Errorf("realtime computer-use review bundle retained %d of %d cases",
			len(manifest.Attempts), manifest.Expected), true)
	}
	return finishAttempt(nil, true)
}

func buildReviewManifest(
	result bench.Result, resultArtifact ReviewArtifact, attempts map[string]ReviewAttempt,
	mediaReceipts map[string]retainedMediaReceipt, failures map[string]string,
) (ReviewManifest, error) {
	return buildReviewManifestPhase(
		ReviewPhaseReviewed, result, resultArtifact, attempts, mediaReceipts, failures,
	)
}

func buildReviewSourceManifest(
	result bench.Result, resultArtifact ReviewArtifact, attempts map[string]ReviewAttempt,
	mediaReceipts map[string]retainedMediaReceipt, failures map[string]string,
) (ReviewManifest, error) {
	return buildReviewManifestPhase(
		ReviewPhaseSource, result, resultArtifact, attempts, mediaReceipts, failures,
	)
}

func buildReviewManifestPhase(
	phase string, result bench.Result, resultArtifact ReviewArtifact,
	attempts map[string]ReviewAttempt, mediaReceipts map[string]retainedMediaReceipt,
	failures map[string]string,
) (ReviewManifest, error) {
	if phase != ReviewPhaseSource && phase != ReviewPhaseReviewed {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest phase is invalid")
	}
	manifest := ReviewManifest{
		Format: ReviewBundleFormat, FormatVersion: ReviewBundleFormatVersion,
		Phase: phase, Suite: SuiteName, Expected: 16, Result: resultArtifact,
		Cell: cloneCell(result.Cell), Provenance: result.Provenance,
	}
	var reportability []string
	if result.Suite != SuiteName || result.Expected != 16 {
		reportability = append(reportability, "the exact sixteen-case suite identity is invalid")
	}
	derived := result
	derived.Finish()
	derivedSummary := derived.Summary
	retainedSummary := result.Summary
	if len(derivedSummary.Distributions) == 0 {
		derivedSummary.Distributions = nil
	}
	if len(retainedSummary.Distributions) == 0 {
		retainedSummary.Distributions = nil
	}
	if !reflect.DeepEqual(derivedSummary, retainedSummary) {
		return ReviewManifest{}, errors.New("realtime computer-use result summary differs from its rows")
	}
	if err := result.Reportable(); err != nil {
		manifest.CoreReportability = err.Error()
		reportability = append(reportability, "core benchmark result is not reportable: "+err.Error())
	} else {
		manifest.CoreReportable = true
	}
	resultRows := make(map[string]bench.TaskOutcome, len(result.Tasks))
	for _, row := range result.Tasks {
		if _, known := canonicalCase(row.ID); !known {
			return ReviewManifest{}, errors.New("realtime computer-use result contains an unknown case")
		}
		if _, duplicate := resultRows[row.ID]; duplicate {
			return ReviewManifest{}, errors.New("realtime computer-use result repeats a case")
		}
		resultRows[row.ID] = cloneTaskOutcome(row)
	}
	for id := range attempts {
		if _, known := canonicalCase(id); !known {
			return ReviewManifest{}, errors.New("realtime computer-use evidence contains an unknown case")
		}
		if _, present := resultRows[id]; !present {
			return ReviewManifest{}, errors.New("realtime computer-use evidence has no matching benchmark row")
		}
	}
	cases, err := Select(nil, nil)
	if err != nil || len(cases) != 16 {
		return ReviewManifest{}, errors.New("resolve exact realtime computer-use v1 inventory")
	}
	for ordinal, item := range cases {
		id := item.ID()
		row, hasRow := resultRows[id]
		attempt, hasAttempt := attempts[id]
		failure := strings.TrimSpace(failures[id])
		switch {
		case !hasRow:
			manifest.Missing = append(manifest.Missing, ReviewMissing{Case: id, Reason: "benchmark row was not attempted"})
		case !hasAttempt:
			if failure == "" {
				failure = "attempt evidence is missing"
			}
			manifest.Missing = append(manifest.Missing, ReviewMissing{Case: id, Reason: failure})
		default:
			if attempt.Ordinal != ordinal+1 || attempt.Case != id || attempt.Trial != 1 ||
				attempt.Grounding != item.Grounding || !reflect.DeepEqual(attempt.Deterministic, row) {
				return ReviewManifest{}, errors.New("realtime computer-use evidence differs from its exact benchmark row")
			}
			receipt, ok := mediaReceipts[id]
			if !ok || receipt.manifestSHA256 != attempt.MediaBundle.ManifestSHA256 {
				return ReviewManifest{}, errors.New("realtime computer-use media receipt differs from its attempt")
			}
			verified, verifyErr := reviewmedia.VerifyBundle(receipt.directory, receipt.manifestSHA256)
			if verifyErr != nil || !reflect.DeepEqual(verified, receipt.manifest) {
				return ReviewManifest{}, errors.New("reverify realtime computer-use media before suite commit")
			}
			indexedMedia, mediaErr := retainedReviewMedia(
				receipt.directory, attempt.MediaBundle.Path,
				EvidenceAttempt{Task: item.Task, Grounding: item.Grounding}, verified,
			)
			if mediaErr != nil || !reflect.DeepEqual(indexedMedia, attempt.Media) {
				return ReviewManifest{}, errors.New("realtime computer-use playable media index drifted")
			}
			if err := validateIndexedReviewState(attempt, verified.AttemptEndUS); err != nil {
				return ReviewManifest{}, err
			}
			wantReportable, wantNote := attemptReportability(
				attempt.RunOrigin, result.Cell.Execution, attempt.Deterministic, attempt.ReviewStatus,
			)
			if attempt.ExecutionStatus != executionStatus(result.Cell.Execution, attempt.Deterministic) ||
				attempt.Reportable != wantReportable || attempt.ReportabilityNote != wantNote {
				return ReviewManifest{}, errors.New("realtime computer-use attempt reportability differs from final result")
			}
			manifest.Attempts = append(manifest.Attempts, cloneReviewAttempt(attempt))
			if !attempt.RunOrigin.Live || attempt.RunOrigin.Kind != EvidenceOriginProduction {
				reportability = append(reportability, "case "+id+" did not use the production shared Realtime path")
			}
			if attempt.ReviewStatus != "complete" {
				reportability = append(reportability, "case "+id+" has no verified secondary multimodal review")
			}
		}
	}
	if len(manifest.Missing) > 0 {
		reportability = append(reportability, fmt.Sprintf(
			"%d of 16 case evidence records are missing", len(manifest.Missing),
		))
	}
	manifest.Complete = len(manifest.Attempts) == 16 && len(manifest.Missing) == 0 &&
		result.Summary.Complete && len(result.Tasks) == 16
	manifest.ReportabilityErrors = canonicalReportability(reportability)
	manifest.Reportable = manifest.Complete && manifest.CoreReportable &&
		len(manifest.ReportabilityErrors) == 0
	return manifest, nil
}

func validateIndexedReviewState(attempt ReviewAttempt, attemptEndUS int64) error {
	switch attempt.ReviewStatus {
	case "not_configured":
		if attempt.EvaluationBundle != nil || attempt.Reviewer != nil || attempt.Assessment != nil ||
			attempt.EvidenceScope != "" || attempt.IndependentRemoteAttestation ||
			attempt.AuthenticityCaveat != "" {
			return errors.New("unconfigured realtime computer-use review retains reviewer claims")
		}
	case "complete":
		if attempt.EvaluationBundle == nil || attempt.Reviewer == nil || attempt.Assessment == nil ||
			strings.TrimSpace(attempt.EvidenceScope) == "" ||
			strings.TrimSpace(attempt.AuthenticityCaveat) == "" ||
			attempt.IndependentRemoteAttestation {
			return errors.New("complete realtime computer-use review lacks sealed provider evidence or caveat")
		}
		if err := attempt.Reviewer.Validate(); err != nil {
			return errors.New("realtime computer-use reviewer descriptor is invalid")
		}
		if err := validateReviewAssessmentTimeline(*attempt.Assessment, attemptEndUS); err != nil {
			return err
		}
	default:
		return errors.New("realtime computer-use review status is invalid")
	}
	return nil
}

func retainedReviewMedia(
	directory, relativeDirectory string, specification EvidenceAttempt, manifest reviewmedia.Manifest,
) ([]ReviewMedia, error) {
	items := manifest.ReviewMedia()
	want := 2
	if specification.Task.Camera {
		want = 3
	}
	if len(items) != want || manifest.Audio == nil || len(manifest.Video) != want-1 {
		return nil, errors.New("realtime computer-use review media has the wrong audiovisual source set")
	}
	wantSources := []string{"screen"}
	if specification.Task.Camera {
		wantSources = append(wantSources, "camera")
	}
	sort.Strings(wantSources)
	for index, source := range manifest.Video {
		if source.Source != wantSources[index] || source.Playable == nil ||
			len(source.Frames) == 0 || source.TimelinePath == "" || source.AttestationReport == nil {
			return nil, errors.New("realtime computer-use review media lacks raw or attested video evidence")
		}
	}
	result := make([]ReviewMedia, 0, len(items))
	for _, item := range items {
		payload, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(item.Path)))
		if err != nil || len(payload) == 0 || reviewDigest(payload) != item.SHA256 {
			return nil, errors.New("retained realtime computer-use review media is missing")
		}
		result = append(result, ReviewMedia{
			Kind: item.Kind, Role: item.Role,
			Path:   filepath.ToSlash(filepath.Join(relativeDirectory, item.Path)),
			SHA256: item.SHA256, SizeBytes: int64(len(payload)), MediaType: item.MediaType,
		})
	}
	if result[0].Kind != "audio" || result[0].MediaType != "audio/wav" {
		return nil, errors.New("realtime computer-use review media has no playable stereo WAV")
	}
	for _, item := range result[1:] {
		if item.Kind != "video" || item.MediaType != "video/mp4" {
			return nil, errors.New("realtime computer-use review media has no synchronized MP4")
		}
	}
	return result, nil
}

func cloneReviewAudioCapture(source bench.SessionAudioCapture) bench.SessionAudioCapture {
	result := source
	result.RoomPCM16 = slices.Clone(source.RoomPCM16)
	result.Agent = make([]bench.TimedAudioChunk, len(source.Agent))
	for index, chunk := range source.Agent {
		result.Agent[index] = chunk
		result.Agent[index].PCM16 = slices.Clone(chunk.PCM16)
	}
	return result
}

func padReviewAudioAfterVideo(
	source bench.SessionAudioCapture, latestVideoMS float64,
) (bench.SessionAudioCapture, float64, error) {
	result := cloneReviewAudioCapture(source)
	endMS, err := alignedReviewAudioEnd(result)
	if err != nil || math.IsNaN(latestVideoMS) || math.IsInf(latestVideoMS, 0) || latestVideoMS < 0 {
		return bench.SessionAudioCapture{}, 0,
			errors.New("realtime computer-use audiovisual evidence has an invalid timeline")
	}
	scaled := latestVideoMS * 1_000
	rounded := math.Round(scaled)
	if math.IsInf(scaled, 0) || rounded >= float64(math.MaxInt64) ||
		math.Abs(scaled-rounded) > 1e-6 {
		return bench.SessionAudioCapture{}, 0,
			errors.New("realtime computer-use video timestamp is not a canonical microsecond")
	}
	latestVideoUS := int64(rounded)
	// 24 kHz has exactly three samples per 125 microseconds. The retained
	// concat contract also requires a final frame interval of at least one
	// millisecond, so choose the first aligned endpoint at least 1 ms after the
	// latest video PTS. Extend only the room channel with silence; the agent
	// playout timeline remains exact.
	if latestVideoUS > math.MaxInt64-1_000 {
		return bench.SessionAudioCapture{}, 0,
			errors.New("realtime computer-use audiovisual evidence exceeds its bounded timeline")
	}
	minimumEndUS := uint64(latestVideoUS + 1_000)
	currentEndUS := uint64(math.Round(endMS * 1_000))
	if currentEndUS >= minimumEndUS {
		return result, endMS, nil
	}
	slots := (minimumEndUS + 124) / 125
	if slots > math.MaxUint64/3 || slots*3 > uint64(math.MaxInt) {
		return bench.SessionAudioCapture{}, 0,
			errors.New("realtime computer-use audiovisual evidence exceeds its bounded timeline")
	}
	targetFrames := int(slots * 3)
	if len(result.RoomPCM16) < targetFrames {
		result.RoomPCM16 = append(result.RoomPCM16, make([]int16, targetFrames-len(result.RoomPCM16))...)
	}
	endMS, err = alignedReviewAudioEnd(result)
	if err != nil || endMS*1_000 <= float64(latestVideoUS) {
		return bench.SessionAudioCapture{}, 0,
			errors.New("realtime computer-use audio padding did not cover the final video frame")
	}
	return result, endMS, nil
}

func alignedReviewAudioEnd(capture bench.SessionAudioCapture) (float64, error) {
	rate := capture.SampleRateHz
	if rate == 0 {
		rate = 24_000
	}
	if rate != 24_000 {
		return 0, errors.New("realtime computer-use review audio must use 24 kHz PCM")
	}
	frames := uint64(len(capture.RoomPCM16))
	for _, chunk := range capture.Agent {
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < 0 || len(chunk.PCM16) == 0 {
			return 0, errors.New("realtime computer-use review audio has an invalid agent chunk")
		}
		startFloat := math.Round(chunk.AtMS * float64(rate) / 1000)
		if startFloat < 0 || startFloat >= float64(math.MaxUint64) {
			return 0, errors.New("realtime computer-use review audio exceeds its timeline")
		}
		start := uint64(startFloat)
		if uint64(len(chunk.PCM16)) > math.MaxUint64-start {
			return 0, errors.New("realtime computer-use review audio exceeds its timeline")
		}
		frames = max(frames, start+uint64(len(chunk.PCM16)))
	}
	if frames == 0 || frames > math.MaxUint64/1_000_000 {
		return 0, errors.New("realtime computer-use review audio has no bounded samples")
	}
	microseconds := (frames*1_000_000 + uint64(rate) - 1) / uint64(rate)
	microseconds = ((microseconds + 124) / 125) * 125
	if microseconds == 0 || microseconds >= uint64(math.MaxInt64) {
		return 0, errors.New("realtime computer-use review audio has an invalid terminal timestamp")
	}
	return float64(microseconds) / 1000, nil
}

func executionStatus(requirement bench.ExecutionRequirement, outcome bench.TaskOutcome) string {
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

func attemptReportability(
	origin EvidenceRunOrigin, requirement bench.ExecutionRequirement,
	outcome bench.TaskOutcome, reviewStatus string,
) (bool, string) {
	var reasons []string
	if !outcome.Completed {
		reasons = append(reasons, "the deterministic attempt did not complete")
	}
	if err := origin.validate(); err != nil || !origin.Live || origin.Kind != EvidenceOriginProduction {
		reasons = append(reasons, "the attempt did not use the production shared Realtime path")
	}
	if !requirement.Required() {
		reasons = append(reasons, "no reviewed execution requirement was supplied")
	} else if err := requirement.Match(outcome.Execution); err != nil {
		reasons = append(reasons, "execution evidence did not match the reviewed requirement")
	}
	if reviewStatus != "complete" {
		reasons = append(reasons, "verified secondary multimodal review is absent")
	}
	if len(reasons) > 0 {
		return false, strings.Join(reasons, "; ")
	}
	return true, ""
}

func equalEvidenceAttempt(left, right EvidenceAttempt) bool {
	leftPayload, leftErr := json.Marshal(left)
	rightPayload, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func caseOrdinal(id string) (int, bool) {
	cases, err := Select(nil, nil)
	if err != nil {
		return 0, false
	}
	for index, item := range cases {
		if item.ID() == id {
			return index + 1, true
		}
	}
	return 0, false
}

func cloneCell(source bench.Cell) bench.Cell {
	result := source
	if source.Levels != nil {
		result.Levels = make(map[bench.Factor]string, len(source.Levels))
		for factor, level := range source.Levels {
			result.Levels[factor] = level
		}
	}
	result.Varies = slices.Clone(source.Varies)
	payload, err := json.Marshal(source.Execution)
	if err == nil {
		_ = json.Unmarshal(payload, &result.Execution)
	}
	return result
}

func cloneReviewAttempt(source ReviewAttempt) ReviewAttempt {
	result := source
	result.Deterministic = cloneTaskOutcome(source.Deterministic)
	result.Observers = slices.Clone(source.Observers)
	result.Media = slices.Clone(source.Media)
	if source.EvaluationBundle != nil {
		copy := *source.EvaluationBundle
		result.EvaluationBundle = &copy
	}
	if source.Reviewer != nil {
		copy := *source.Reviewer
		result.Reviewer = &copy
	}
	if source.Assessment != nil {
		copy := cloneReviewAssessment(*source.Assessment)
		result.Assessment = &copy
	}
	return result
}

func cloneReviewAttemptMap(source map[string]ReviewAttempt) map[string]ReviewAttempt {
	result := make(map[string]ReviewAttempt, len(source))
	for caseID, attempt := range source {
		result[caseID] = cloneReviewAttempt(attempt)
	}
	return result
}

func sameReviewAttemptSource(reviewed, source ReviewAttempt) bool {
	return reviewed.Ordinal == source.Ordinal && reviewed.Case == source.Case &&
		reviewed.Trial == source.Trial && reviewed.Grounding == source.Grounding &&
		slices.Equal(reviewed.Observers, source.Observers) &&
		reflect.DeepEqual(reviewed.Deterministic, source.Deterministic) &&
		reflect.DeepEqual(reviewed.Context, source.Context) &&
		reflect.DeepEqual(reviewed.MediaBundle, source.MediaBundle) &&
		reflect.DeepEqual(reviewed.Media, source.Media) &&
		reflect.DeepEqual(reviewed.RunOrigin, source.RunOrigin) &&
		reviewed.ExecutionStatus == source.ExecutionStatus
}

func cloneRetainedMediaReceipts(
	source map[string]retainedMediaReceipt,
) map[string]retainedMediaReceipt {
	result := make(map[string]retainedMediaReceipt, len(source))
	for caseID, receipt := range source {
		result[caseID] = receipt.clone()
	}
	return result
}

func cloneEvaluationReceipts(
	source map[string]revieweval.EvaluationBundleReceipt,
) map[string]revieweval.EvaluationBundleReceipt {
	result := make(map[string]revieweval.EvaluationBundleReceipt, len(source))
	for caseID, receipt := range source {
		result[caseID] = receipt
	}
	return result
}

func cloneMediaManifest(source reviewmedia.Manifest) reviewmedia.Manifest {
	result := source
	if source.Audio != nil {
		copy := *source.Audio
		copy.AudioSpec.ChannelLayout = slices.Clone(source.Audio.AudioSpec.ChannelLayout)
		result.Audio = &copy
	}
	result.Video = make([]reviewmedia.VideoSource, len(source.Video))
	for index, video := range source.Video {
		result.Video[index] = video
		result.Video[index].Frames = slices.Clone(video.Frames)
		if video.Playable != nil {
			copy := *video.Playable
			result.Video[index].Playable = &copy
		}
		if video.PlayableSpec != nil {
			copy := *video.PlayableSpec
			result.Video[index].PlayableSpec = &copy
		}
		if video.AttestationReport != nil {
			copy := *video.AttestationReport
			result.Video[index].AttestationReport = &copy
		}
	}
	if source.Encoder != nil {
		copy := *source.Encoder
		result.Encoder = &copy
	}
	if source.Attestor != nil {
		copy := *source.Attestor
		result.Attestor = &copy
	}
	return result
}

func canonicalReportability(source []string) []string {
	seen := make(map[string]struct{}, len(source))
	result := make([]string, 0, len(source))
	for _, value := range source {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil
	}
	sort.Strings(result)
	return result
}

func reviewSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	dash := false
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			output.WriteRune(character)
			dash = false
		} else if output.Len() > 0 && !dash {
			output.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(output.String(), "-")
}

func canonicalReviewSecrets(source []string) ([]string, error) {
	if len(source) > maximumReviewSecrets {
		return nil, fmt.Errorf("realtime computer-use review secrets exceed %d values", maximumReviewSecrets)
	}
	seen := make(map[string]struct{}, len(source))
	result := make([]string, 0, len(source))
	for _, value := range source {
		if len(value) < minimumReviewSecretBytes || len(value) > maximumReviewSecretBytes ||
			!utf8.ValidString(value) || strings.TrimSpace(value) != value {
			return nil, errors.New("realtime computer-use review secret is invalid")
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return nil, errors.New("realtime computer-use review secret is invalid")
			}
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("realtime computer-use review secrets contain a duplicate")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		if len(result[left]) != len(result[right]) {
			return len(result[left]) > len(result[right])
		}
		return result[left] < result[right]
	})
	return result, nil
}

func containsReviewSecret(secrets []string, payload []byte) bool {
	if len(secrets) == 0 {
		return false
	}
	needles := make([][]byte, 0, len(secrets)*6)
	seen := make(map[string]struct{}, len(secrets)*6)
	add := func(value []byte) {
		if len(value) == 0 {
			return
		}
		key := string(value)
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		needles = append(needles, slices.Clone(value))
	}
	for _, secret := range secrets {
		literal := []byte(secret)
		add(literal)
		if quoted, err := json.Marshal(secret); err == nil && len(quoted) >= 2 {
			add(quoted[1 : len(quoted)-1])
		}
		for _, encoded := range []string{
			base64.StdEncoding.EncodeToString(literal), base64.RawStdEncoding.EncodeToString(literal),
			base64.URLEncoding.EncodeToString(literal), base64.RawURLEncoding.EncodeToString(literal),
		} {
			add([]byte(encoded))
		}
	}
	if containsReviewNeedle(needles, payload) {
		return true
	}
	// Retained suite artifacts are strict JSON. Scan the decoded string-token
	// stream as well as the raw bytes so arbitrary Unicode escapes, nested JSON,
	// and a credential split across nearby keys/values cannot evade the guard.
	// Exceeding the bounded recursive work budget is itself unsafe to retain.
	budget := int64(maximumReviewResultBytes+maximumReviewManifestBytes) * 4
	return containsDecodedReviewSecret(payload, needles, 0, &budget)
}

func containsDecodedReviewSecret(
	payload []byte, needles [][]byte, depth int, budget *int64,
) bool {
	const maximumDepth = 4
	if depth > maximumDepth || budget == nil || *budget < int64(len(payload)) {
		return true
	}
	*budget -= int64(len(payload))
	decoder := json.NewDecoder(bytes.NewReader(payload))
	maximumNeedle := 0
	for _, needle := range needles {
		maximumNeedle = max(maximumNeedle, len(needle))
	}
	const maximumSplitStates = 8
	var tails [][]byte
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			return false
		}
		value, ok := token.(string)
		if !ok {
			continue
		}
		decoded := []byte(value)
		if *budget < int64(len(decoded)) {
			return true
		}
		*budget -= int64(len(decoded))
		if containsReviewNeedle(needles, decoded) {
			return true
		}
		candidates := make([][]byte, 0, len(tails)+1)
		candidates = append(candidates, decoded)
		for _, tail := range tails {
			candidate := make([]byte, 0, len(tail)+len(decoded))
			candidate = append(candidate, tail...)
			candidate = append(candidate, decoded...)
			if containsReviewNeedle(needles, candidate) {
				return true
			}
			candidates = append(candidates, candidate)
		}
		if maximumNeedle > 1 {
			for _, candidate := range candidates {
				keep := min(len(candidate), maximumNeedle-1)
				candidate = slices.Clone(candidate[len(candidate)-keep:])
				duplicate := false
				for _, tail := range tails {
					duplicate = duplicate || bytes.Equal(tail, candidate)
				}
				if !duplicate {
					tails = append(tails, candidate)
				}
			}
			if len(tails) > maximumSplitStates {
				tails = slices.Clone(tails[len(tails)-maximumSplitStates:])
			}
		}
		trimmed := bytes.TrimSpace(decoded)
		if depth < maximumDepth && len(trimmed) > 1 &&
			(trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') &&
			json.Valid(trimmed) && containsDecodedReviewSecret(trimmed, needles, depth+1, budget) {
			return true
		}
	}
}

func containsReviewNeedle(needles [][]byte, payload []byte) bool {
	for _, needle := range needles {
		if bytes.Contains(payload, needle) {
			return true
		}
	}
	return false
}

func validateNewReviewDirectory(path string) (string, error) {
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) || filepath.Clean(path) != path ||
		!filepath.IsAbs(path) {
		return "", errors.New("realtime computer-use review directory must be a clean absolute path")
	}
	for _, character := range path {
		if unicode.IsControl(character) {
			return "", errors.New("realtime computer-use review directory contains a control character")
		}
	}
	return path, nil
}

func writeReviewFile(root *os.Root, path string, payload []byte) error {
	if root == nil || len(payload) == 0 || !validReviewRelativePath(path) {
		return errors.New("invalid realtime computer-use review artifact")
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	written := 0
	for written < len(payload) {
		count, writeErr := file.Write(payload[written:])
		if writeErr != nil {
			_ = file.Close()
			return writeErr
		}
		if count <= 0 {
			_ = file.Close()
			return io.ErrShortWrite
		}
		written += count
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		_ = file.Close()
		return err
	}
	directory := filepath.Dir(filepath.FromSlash(path))
	if directory == "" {
		directory = "."
	}
	directoryFile, err := root.Open(directory)
	if err != nil {
		return err
	}
	syncErr := directoryFile.Sync()
	closeErr := directoryFile.Close()
	return errors.Join(syncErr, closeErr)
}

func validReviewRelativePath(path string) bool {
	return path != "" && len(path) <= 4096 && utf8.ValidString(path) &&
		filepath.IsLocal(filepath.FromSlash(path)) && filepath.ToSlash(filepath.Clean(path)) == path &&
		!strings.Contains(path, "//")
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

// VerifyReviewBundle strictly reopens a sealed suite bundle. It delegates each
// raw-media tree to review/media's verifier, rejects extra or aliased suite
// files, reconstructs the manifest from the exact result and contexts, and
// compares the canonical human review byte for byte.
type reviewBundleVerifyOperations struct {
	afterSemanticVerification func() error
	afterSecondTreeCapture    func() error
	closeRoot                 func(*os.Root) error
}

// VerifyReviewSourceBundle authenticates the deterministic source phase while
// deliberately excluding mutable sibling evaluation directories. The source
// receipt therefore remains stable before, during, and after advisory review.
func VerifyReviewSourceBundle(
	directory, expectedManifestSHA256 string,
) (ReviewManifest, error) {
	return verifyReviewSourceBundleWithOperations(
		directory, expectedManifestSHA256, reviewBundleVerifyOperations{},
	)
}

func verifyReviewSourceBundleWithOperations(
	directory, expectedManifestSHA256 string, operations reviewBundleVerifyOperations,
) (verified ReviewManifest, resultErr error) {
	return verifyReviewSourceBundleAtMarkerWithOperations(
		directory, reviewSourceManifest, expectedManifestSHA256, operations,
	)
}

func verifyReviewSourceBundleAtMarkerWithOperations(
	directory, marker, expectedManifestSHA256 string, operations reviewBundleVerifyOperations,
) (verified ReviewManifest, resultErr error) {
	if marker != reviewSourceManifest && marker != reviewSourceManifestStage {
		return ReviewManifest{}, errors.New("deterministic realtime computer-use source marker is invalid")
	}
	if _, err := validateNewReviewDirectory(directory); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return ReviewManifest{}, err
	}
	if err := validateReviewDigest(expectedManifestSHA256); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifySealedReviewSourceArtifactsAtMarker(directory, marker); err != nil {
		return ReviewManifest{}, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return ReviewManifest{}, errors.New("open deterministic realtime computer-use source bundle")
	}
	defer func() {
		closeRoot := root.Close
		if operations.closeRoot != nil {
			closeRoot = func() error { return operations.closeRoot(root) }
		}
		if closeErr := closeRoot(); closeErr != nil {
			_ = root.Close()
			verified = ReviewManifest{}
			resultErr = errors.Join(
				resultErr, errors.New("close verified deterministic realtime computer-use source bundle"),
			)
		}
	}()
	rootIdentity, err := verifyReviewRootIdentity(directory, root, nil)
	if err != nil {
		return ReviewManifest{}, err
	}
	initialTreeSHA, err := captureReviewSourceTreeSHA256AtMarker(directory, marker, rootIdentity)
	if err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewSourceRootEntriesAtMarker(root, marker); err != nil {
		return ReviewManifest{}, err
	}
	manifestPayload, manifestInfo, err := readReviewFile(
		root, marker, maximumReviewManifestBytes,
	)
	if err != nil || reviewDigest(manifestPayload) != expectedManifestSHA256 {
		return ReviewManifest{}, errors.New("deterministic realtime computer-use source manifest is missing or changed")
	}
	manifest, err := decodeReviewManifest(manifestPayload)
	if err != nil {
		return ReviewManifest{}, err
	}
	if manifest.Format != ReviewBundleFormat ||
		manifest.FormatVersion != ReviewBundleFormatVersion ||
		manifest.Phase != ReviewPhaseSource || manifest.SourceManifest != nil ||
		manifest.Suite != SuiteName || manifest.Expected != 16 {
		return ReviewManifest{}, errors.New("deterministic realtime computer-use source identity is invalid")
	}
	if manifest.Result.Path != "result.json" || manifest.Result.MediaType != "application/json" {
		return ReviewManifest{}, errors.New("deterministic realtime computer-use source result identity is invalid")
	}
	resultPayload, resultInfo, err := readReviewFile(
		root, manifest.Result.Path, maximumReviewResultBytes,
	)
	if err != nil || !artifactMatches(manifest.Result, resultPayload) {
		return ReviewManifest{}, errors.New("deterministic realtime computer-use result is missing or changed")
	}
	result, err := decodeReviewResult(resultPayload)
	if err != nil {
		return ReviewManifest{}, err
	}
	if !reflect.DeepEqual(manifest.Cell, result.Cell) ||
		!reflect.DeepEqual(manifest.Provenance, result.Provenance) {
		return ReviewManifest{}, errors.New("deterministic source treatment differs from its result")
	}
	if err := verifyIndependentReviewFiles([]os.FileInfo{manifestInfo, resultInfo}); err != nil {
		return ReviewManifest{}, err
	}

	attempts := make(map[string]ReviewAttempt, len(manifest.Attempts))
	mediaReceipts := make(map[string]retainedMediaReceipt, len(manifest.Attempts))
	contextNames := make([]string, 0, len(manifest.Attempts))
	mediaNames := make([]string, 0, len(manifest.Attempts))
	regularInfos := []os.FileInfo{manifestInfo, resultInfo}
	previousOrdinal := 0
	for _, attempt := range manifest.Attempts {
		item, known := canonicalCase(attempt.Case)
		ordinal, ordinalKnown := caseOrdinal(attempt.Case)
		if !known || !ordinalKnown || attempt.Ordinal != ordinal ||
			attempt.Ordinal <= previousOrdinal || attempt.Trial != 1 ||
			attempt.Grounding != item.Grounding || attempt.ReviewStatus != "not_configured" {
			return ReviewManifest{}, errors.New("deterministic source attempt ordering or identity is invalid")
		}
		previousOrdinal = attempt.Ordinal
		if _, duplicate := attempts[attempt.Case]; duplicate {
			return ReviewManifest{}, errors.New("deterministic source repeats an attempt")
		}
		wantStem := fmt.Sprintf("%02d-%s-trial-01", ordinal, reviewSlug(attempt.Case))
		wantContext := filepath.ToSlash(filepath.Join("contexts", wantStem+".json"))
		wantMedia := filepath.ToSlash(filepath.Join("media", wantStem))
		if attempt.Context.Path != wantContext || attempt.Context.MediaType != "application/json" ||
			attempt.MediaBundle.Path != wantMedia || attempt.MediaBundle.ManifestSHA256 == "" {
			return ReviewManifest{}, errors.New("deterministic source artifact layout is invalid")
		}
		contextPayload, contextInfo, err := readReviewFile(
			root, attempt.Context.Path, maximumReviewContextBytes,
		)
		if err != nil || !artifactMatches(attempt.Context, contextPayload) {
			return ReviewManifest{}, errors.New("deterministic source context is missing or changed")
		}
		contextValue, err := decodeReviewContext(contextPayload)
		if err != nil || contextValue.Format != ReviewContextFormat ||
			contextValue.Version != ReviewContextVersion || contextValue.Suite != SuiteName ||
			contextValue.Case != attempt.Case || contextValue.Trial != 1 ||
			contextValue.ResultSHA256 != manifest.Result.SHA256 ||
			!reflect.DeepEqual(contextValue.Cell, result.Cell) ||
			!reflect.DeepEqual(contextValue.Provenance, result.Provenance) ||
			contextValue.Grounding != attempt.Grounding ||
			!equalCase(item, Case{Task: contextValue.Task, Grounding: contextValue.Grounding}) ||
			!reflect.DeepEqual(contextValue.Outcome, attempt.Deterministic) ||
			!reflect.DeepEqual(contextValue.RunOrigin, attempt.RunOrigin) ||
			!slices.Equal(contextValue.Observers, attempt.Observers) ||
			validateEvidenceObservers(contextValue.Observers) != nil ||
			!reflect.DeepEqual(contextValue.ExecutionRequirement, result.Cell.Execution) {
			return ReviewManifest{}, errors.New("deterministic source context differs from its attempt")
		}
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(attempt.MediaBundle.Path))
		mediaManifest, err := reviewmedia.VerifyBundle(
			mediaDirectory, attempt.MediaBundle.ManifestSHA256,
		)
		if err != nil {
			return ReviewManifest{}, errors.New("verify deterministic source media bundle")
		}
		media, err := retainedReviewMedia(
			mediaDirectory, attempt.MediaBundle.Path,
			EvidenceAttempt{Task: item.Task, Grounding: item.Grounding}, mediaManifest,
		)
		if err != nil || !reflect.DeepEqual(media, attempt.Media) {
			return ReviewManifest{}, errors.New("deterministic source playable media differs from nested evidence")
		}
		if err := validateIndexedReviewState(attempt, mediaManifest.AttemptEndUS); err != nil {
			return ReviewManifest{}, err
		}
		wantReportable, wantNote := attemptReportability(
			attempt.RunOrigin, contextValue.ExecutionRequirement,
			attempt.Deterministic, attempt.ReviewStatus,
		)
		if attempt.Reportable != wantReportable || attempt.ReportabilityNote != wantNote ||
			attempt.ExecutionStatus != executionStatus(
				contextValue.ExecutionRequirement, attempt.Deterministic,
			) {
			return ReviewManifest{}, errors.New("deterministic source reportability differs from its evidence")
		}
		attempts[attempt.Case] = cloneReviewAttempt(attempt)
		mediaReceipts[attempt.Case] = retainedMediaReceipt{
			directory: mediaDirectory, manifestSHA256: attempt.MediaBundle.ManifestSHA256,
			manifest: mediaManifest,
		}
		contextNames = append(contextNames, filepath.Base(attempt.Context.Path))
		mediaNames = append(mediaNames, filepath.Base(attempt.MediaBundle.Path))
		regularInfos = append(regularInfos, contextInfo)
	}
	if err := verifyReviewDirectoryEntries(root, "contexts", contextNames); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewDirectoryEntries(root, "media", mediaNames); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyIndependentReviewFiles(regularInfos); err != nil {
		return ReviewManifest{}, err
	}
	failures, err := retainedReviewFailures(manifest, attempts)
	if err != nil {
		return ReviewManifest{}, err
	}
	rebuilt, err := buildReviewSourceManifest(
		result, manifest.Result, attempts, mediaReceipts, failures,
	)
	if err != nil || !reflect.DeepEqual(rebuilt, manifest) {
		return ReviewManifest{}, errors.New("deterministic source manifest differs from retained evidence")
	}
	markdownPayload, markdownInfo, err := readReviewFile(
		root, "SOURCE_REVIEW.md", maximumReviewMarkdownBytes,
	)
	if err != nil || string(markdownPayload) != renderReview(manifest) {
		return ReviewManifest{}, errors.New("deterministic source review is missing or noncanonical")
	}
	if err := verifyIndependentReviewFiles(append(regularInfos, markdownInfo)); err != nil {
		return ReviewManifest{}, err
	}
	if operations.afterSemanticVerification != nil {
		if err := operations.afterSemanticVerification(); err != nil {
			return ReviewManifest{}, err
		}
	}
	if _, err := verifyReviewRootIdentity(directory, root, rootIdentity); err != nil {
		return ReviewManifest{}, err
	}
	afterVerificationSHA, err := captureReviewSourceTreeSHA256AtMarker(
		directory, marker, rootIdentity,
	)
	if err != nil || afterVerificationSHA != initialTreeSHA {
		return ReviewManifest{}, errors.New("deterministic source tree changed during verification")
	}
	if operations.afterSecondTreeCapture != nil {
		if err := operations.afterSecondTreeCapture(); err != nil {
			return ReviewManifest{}, err
		}
	}
	if err := reverifyReviewSubtrees(directory, manifest); err != nil {
		return ReviewManifest{}, err
	}
	if _, err := verifyReviewRootIdentity(directory, root, rootIdentity); err != nil {
		return ReviewManifest{}, err
	}
	finalTreeSHA, err := captureReviewSourceTreeSHA256AtMarker(
		directory, marker, rootIdentity,
	)
	if err != nil || finalTreeSHA != initialTreeSHA {
		return ReviewManifest{}, errors.New("deterministic source tree changed during final verification")
	}
	return manifest, nil
}

func retainedReviewFailures(
	manifest ReviewManifest, attempts map[string]ReviewAttempt,
) (map[string]string, error) {
	failures := make(map[string]string, len(manifest.Missing))
	seenMissing := make(map[string]struct{}, len(manifest.Missing))
	for _, missing := range manifest.Missing {
		if _, known := canonicalCase(missing.Case); !known ||
			strings.TrimSpace(missing.Reason) == "" {
			return nil, errors.New("realtime computer-use review missing-case record is invalid")
		}
		if _, duplicate := seenMissing[missing.Case]; duplicate {
			return nil, errors.New("realtime computer-use review repeats a missing case")
		}
		if _, present := attempts[missing.Case]; present {
			return nil, errors.New("realtime computer-use case is both present and missing")
		}
		seenMissing[missing.Case] = struct{}{}
		failures[missing.Case] = missing.Reason
	}
	return failures, nil
}

func validateReviewDigest(value string) error {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return errors.New("realtime computer-use review manifest digest is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil {
		return errors.New("realtime computer-use review manifest digest is invalid")
	}
	return nil
}

func VerifyReviewBundle(directory, expectedManifestSHA256 string) (ReviewManifest, error) {
	return verifyReviewBundleWithOperations(
		directory, expectedManifestSHA256, reviewBundleVerifyOperations{},
	)
}

// VerifyReviewBundleReceipt additionally anchors the independently published
// source phase. The final manifest transitively binds every evaluation receipt,
// while this caller-retained field prevents a final receipt from being paired
// with a different deterministic source receipt.
func VerifyReviewBundleReceipt(
	directory string, expected ReviewBundleReceipt,
) (ReviewManifest, error) {
	if err := validateReviewDigest(expected.SourceManifestSHA256); err != nil ||
		validateReviewDigest(expected.SourceReceiptSHA256) != nil {
		return ReviewManifest{}, errors.New("realtime computer-use source receipt digest is invalid")
	}
	manifest, err := VerifyReviewBundle(directory, expected.ManifestSHA256)
	if err != nil {
		return ReviewManifest{}, err
	}
	if manifest.SourceManifest == nil ||
		manifest.SourceManifest.SHA256 != expected.SourceManifestSHA256 {
		return ReviewManifest{}, errors.New("realtime computer-use review receipt names a different source")
	}
	sourceManifest, err := VerifyReviewSourceBundle(directory, expected.SourceManifestSHA256)
	if err != nil {
		return ReviewManifest{}, err
	}
	sourceReceipt, err := buildReviewSourceReceipt(
		directory, sourceManifest, expected.SourceManifestSHA256,
	)
	if err != nil || sourceReceipt.ReceiptSHA256 != expected.SourceReceiptSHA256 {
		return ReviewManifest{}, errors.New("realtime computer-use review receipt has a different source identity")
	}
	return manifest, nil
}

func verifyReviewBundleWithOperations(
	directory, expectedManifestSHA256 string, operations reviewBundleVerifyOperations,
) (verified ReviewManifest, resultErr error) {
	if _, err := validateNewReviewDirectory(directory); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return ReviewManifest{}, err
	}
	if err := validateReviewDigest(expectedManifestSHA256); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifySealedReviewTree(directory); err != nil {
		return ReviewManifest{}, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return ReviewManifest{}, errors.New("open sealed realtime computer-use review bundle")
	}
	defer func() {
		closeRoot := root.Close
		if operations.closeRoot != nil {
			closeRoot = func() error { return operations.closeRoot(root) }
		}
		if closeErr := closeRoot(); closeErr != nil {
			_ = root.Close()
			verified = ReviewManifest{}
			resultErr = errors.Join(
				resultErr, errors.New("close verified realtime computer-use review bundle"),
			)
		}
	}()
	rootIdentity, err := verifyReviewRootIdentity(directory, root, nil)
	if err != nil {
		return ReviewManifest{}, err
	}
	initialTreeSHA, err := captureReviewTreeSHA256(directory, rootIdentity)
	if err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewDirectoryEntries(root, ".", []string{
		"REVIEW.md", "SOURCE_REVIEW.md", "contexts", "manifest.json", "media",
		"result.json", "reviews", "source.manifest.json",
	}); err != nil {
		return ReviewManifest{}, err
	}
	manifestPayload, manifestInfo, err := readReviewFile(root, "manifest.json", maximumReviewManifestBytes)
	if err != nil || reviewDigest(manifestPayload) != expectedManifestSHA256 {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest is missing or changed")
	}
	manifest, err := decodeReviewManifest(manifestPayload)
	if err != nil {
		return ReviewManifest{}, err
	}
	if manifest.Format != ReviewBundleFormat || manifest.FormatVersion != ReviewBundleFormatVersion ||
		manifest.Phase != ReviewPhaseReviewed || manifest.SourceManifest == nil ||
		manifest.SourceManifest.Path != "source.manifest.json" ||
		manifest.SourceManifest.MediaType != "application/json" ||
		manifest.Suite != SuiteName || manifest.Expected != 16 {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest identity is invalid")
	}
	sourcePayload, sourceInfo, err := readReviewFile(
		root, manifest.SourceManifest.Path, maximumReviewManifestBytes,
	)
	if err != nil || !artifactMatches(*manifest.SourceManifest, sourcePayload) {
		return ReviewManifest{}, errors.New("deterministic realtime computer-use source receipt is missing or changed")
	}
	sourceManifest, err := VerifyReviewSourceBundle(directory, manifest.SourceManifest.SHA256)
	if err != nil {
		return ReviewManifest{}, errors.New("verify deterministic realtime computer-use source receipt")
	}
	if manifest.Result.Path != "result.json" || manifest.Result.MediaType != "application/json" {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest result identity is invalid")
	}
	resultPayload, resultInfo, err := readReviewFile(root, manifest.Result.Path, maximumReviewResultBytes)
	if err != nil || !artifactMatches(manifest.Result, resultPayload) {
		return ReviewManifest{}, errors.New("realtime computer-use retained result is missing or changed")
	}
	result, err := decodeReviewResult(resultPayload)
	if err != nil {
		return ReviewManifest{}, err
	}
	if !reflect.DeepEqual(manifest.Cell, result.Cell) ||
		!reflect.DeepEqual(manifest.Provenance, result.Provenance) {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest treatment differs from result")
	}
	if !reflect.DeepEqual(sourceManifest.Result, manifest.Result) ||
		!reflect.DeepEqual(sourceManifest.Cell, manifest.Cell) ||
		!reflect.DeepEqual(sourceManifest.Provenance, manifest.Provenance) ||
		sourceManifest.Expected != manifest.Expected ||
		sourceManifest.Complete != manifest.Complete ||
		sourceManifest.CoreReportable != manifest.CoreReportable ||
		sourceManifest.CoreReportability != manifest.CoreReportability ||
		!reflect.DeepEqual(sourceManifest.Missing, manifest.Missing) ||
		len(sourceManifest.Attempts) != len(manifest.Attempts) {
		return ReviewManifest{}, errors.New("reviewed realtime computer-use index differs from its source receipt")
	}
	if err := verifyIndependentReviewFiles([]os.FileInfo{manifestInfo, sourceInfo, resultInfo}); err != nil {
		return ReviewManifest{}, err
	}

	attempts := make(map[string]ReviewAttempt, len(manifest.Attempts))
	mediaReceipts := make(map[string]retainedMediaReceipt, len(manifest.Attempts))
	contextNames := make([]string, 0, len(manifest.Attempts))
	mediaNames := make([]string, 0, len(manifest.Attempts))
	reviewNames := make([]string, 0, len(manifest.Attempts))
	regularInfos := []os.FileInfo{manifestInfo, sourceInfo, resultInfo}
	sourceAttempts := make(map[string]ReviewAttempt, len(sourceManifest.Attempts))
	for _, attempt := range sourceManifest.Attempts {
		sourceAttempts[attempt.Case] = cloneReviewAttempt(attempt)
	}
	previousOrdinal := 0
	for _, attempt := range manifest.Attempts {
		item, known := canonicalCase(attempt.Case)
		ordinal, ordinalKnown := caseOrdinal(attempt.Case)
		if !known || !ordinalKnown || attempt.Ordinal != ordinal || attempt.Ordinal <= previousOrdinal ||
			attempt.Trial != 1 || attempt.Grounding != item.Grounding {
			return ReviewManifest{}, errors.New("realtime computer-use review attempt ordering or identity is invalid")
		}
		previousOrdinal = attempt.Ordinal
		if _, duplicate := attempts[attempt.Case]; duplicate {
			return ReviewManifest{}, errors.New("realtime computer-use review manifest repeats an attempt")
		}
		sourceAttempt, sourcePresent := sourceAttempts[attempt.Case]
		if !sourcePresent || !sameReviewAttemptSource(attempt, sourceAttempt) {
			return ReviewManifest{}, errors.New("reviewed realtime computer-use attempt differs from its source receipt")
		}
		wantStem := fmt.Sprintf("%02d-%s-trial-01", ordinal, reviewSlug(attempt.Case))
		wantContext := filepath.ToSlash(filepath.Join("contexts", wantStem+".json"))
		wantMedia := filepath.ToSlash(filepath.Join("media", wantStem))
		if attempt.Context.Path != wantContext || attempt.Context.MediaType != "application/json" ||
			attempt.MediaBundle.Path != wantMedia || attempt.MediaBundle.ManifestSHA256 == "" {
			return ReviewManifest{}, errors.New("realtime computer-use review attempt artifact layout is invalid")
		}
		contextPayload, contextInfo, err := readReviewFile(root, attempt.Context.Path, maximumReviewContextBytes)
		if err != nil || !artifactMatches(attempt.Context, contextPayload) {
			return ReviewManifest{}, errors.New("realtime computer-use review context is missing or changed")
		}
		contextValue, err := decodeReviewContext(contextPayload)
		if err != nil || contextValue.Format != ReviewContextFormat ||
			contextValue.Version != ReviewContextVersion || contextValue.Suite != SuiteName ||
			contextValue.Case != attempt.Case || contextValue.Trial != 1 ||
			contextValue.ResultSHA256 != manifest.Result.SHA256 ||
			!reflect.DeepEqual(contextValue.Cell, result.Cell) ||
			!reflect.DeepEqual(contextValue.Provenance, result.Provenance) ||
			contextValue.Grounding != attempt.Grounding ||
			!equalCase(item, Case{Task: contextValue.Task, Grounding: contextValue.Grounding}) ||
			!reflect.DeepEqual(contextValue.Outcome, attempt.Deterministic) ||
			!reflect.DeepEqual(contextValue.RunOrigin, attempt.RunOrigin) ||
			!slices.Equal(contextValue.Observers, attempt.Observers) ||
			validateEvidenceObservers(contextValue.Observers) != nil ||
			!reflect.DeepEqual(contextValue.ExecutionRequirement, result.Cell.Execution) {
			return ReviewManifest{}, errors.New("realtime computer-use review context differs from its attempt")
		}
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(attempt.MediaBundle.Path))
		mediaManifest, err := reviewmedia.VerifyBundle(
			mediaDirectory, attempt.MediaBundle.ManifestSHA256,
		)
		if err != nil {
			return ReviewManifest{}, errors.New("verify nested realtime computer-use media bundle")
		}
		media, err := retainedReviewMedia(mediaDirectory, attempt.MediaBundle.Path,
			EvidenceAttempt{Task: item.Task, Grounding: item.Grounding}, mediaManifest)
		if err != nil || !reflect.DeepEqual(media, attempt.Media) {
			return ReviewManifest{}, errors.New("realtime computer-use playable media index differs from nested evidence")
		}
		if err := validateIndexedReviewState(attempt, mediaManifest.AttemptEndUS); err != nil {
			return ReviewManifest{}, err
		}
		switch attempt.ReviewStatus {
		case "not_configured":
		case "complete":
			wantReview := filepath.ToSlash(filepath.Join("reviews", wantStem))
			if attempt.EvaluationBundle == nil || attempt.EvaluationBundle.Path != wantReview {
				return ReviewManifest{}, errors.New("realtime computer-use secondary review layout is invalid")
			}
			reviewDirectory := filepath.Join(directory, filepath.FromSlash(wantReview))
			expectedReceipt := evaluationReceiptAt(reviewDirectory, *attempt.EvaluationBundle)
			opened, verifyErr := revieweval.VerifyEvaluationBundle(
				context.Background(), revieweval.EvaluationBundleOptions{Directory: reviewDirectory},
				expectedReceipt,
			)
			if verifyErr != nil {
				return ReviewManifest{}, errors.New("verify nested realtime computer-use secondary review")
			}
			attemptID := secondaryReviewAttemptID(contextValue.ResultSHA256, attempt.Case)
			if err := validateOpenedSecondaryReview(
				opened, attemptID, attempt, contextPayload,
				mediaManifest.ReviewMedia(), mediaManifest.AttemptEndUS,
			); err != nil {
				return ReviewManifest{}, err
			}
			if !reflect.DeepEqual(opened.Record.Provider, *attempt.Reviewer) ||
				!reflect.DeepEqual(opened.Record.Assessment, *attempt.Assessment) ||
				attempt.EvidenceScope != opened.Manifest.EvidenceScope ||
				attempt.IndependentRemoteAttestation != opened.Manifest.IndependentRemoteAttestation ||
				attempt.AuthenticityCaveat != opened.Manifest.AuthenticityCaveat ||
				!sameNestedEvaluationReceipt(opened.Receipt, *attempt.EvaluationBundle) {
				return ReviewManifest{}, errors.New("realtime computer-use secondary review index differs from sealed evidence")
			}
			afterReview, verifyErr := reviewmedia.VerifyBundle(
				mediaDirectory, attempt.MediaBundle.ManifestSHA256,
			)
			if verifyErr != nil || !reflect.DeepEqual(afterReview, mediaManifest) {
				return ReviewManifest{}, errors.New("realtime computer-use source media changed during review verification")
			}
			reviewNames = append(reviewNames, filepath.Base(wantReview))
		}
		wantReportable, wantNote := attemptReportability(
			attempt.RunOrigin, contextValue.ExecutionRequirement, attempt.Deterministic,
			attempt.ReviewStatus,
		)
		if attempt.Reportable != wantReportable || attempt.ReportabilityNote != wantNote ||
			attempt.ExecutionStatus != executionStatus(
				contextValue.ExecutionRequirement, attempt.Deterministic,
			) {
			return ReviewManifest{}, errors.New("realtime computer-use attempt reportability differs from evidence")
		}
		attempts[attempt.Case] = cloneReviewAttempt(attempt)
		mediaReceipts[attempt.Case] = retainedMediaReceipt{
			directory: mediaDirectory, manifestSHA256: attempt.MediaBundle.ManifestSHA256,
			manifest: mediaManifest,
		}
		contextNames = append(contextNames, filepath.Base(attempt.Context.Path))
		mediaNames = append(mediaNames, filepath.Base(attempt.MediaBundle.Path))
		regularInfos = append(regularInfos, contextInfo)
	}
	if err := verifyReviewDirectoryEntries(root, "contexts", contextNames); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewDirectoryEntries(root, "media", mediaNames); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyReviewDirectoryEntries(root, "reviews", reviewNames); err != nil {
		return ReviewManifest{}, err
	}
	if err := verifyIndependentReviewFiles(regularInfos); err != nil {
		return ReviewManifest{}, err
	}
	failures := make(map[string]string, len(manifest.Missing))
	seenMissing := make(map[string]struct{}, len(manifest.Missing))
	for _, missing := range manifest.Missing {
		if _, known := canonicalCase(missing.Case); !known || strings.TrimSpace(missing.Reason) == "" {
			return ReviewManifest{}, errors.New("realtime computer-use review missing-case record is invalid")
		}
		if _, duplicate := seenMissing[missing.Case]; duplicate {
			return ReviewManifest{}, errors.New("realtime computer-use review repeats a missing case")
		}
		if _, present := attempts[missing.Case]; present {
			return ReviewManifest{}, errors.New("realtime computer-use case is both present and missing")
		}
		seenMissing[missing.Case] = struct{}{}
		failures[missing.Case] = missing.Reason
	}
	rebuilt, err := buildReviewManifest(result, manifest.Result, attempts, mediaReceipts, failures)
	if manifest.SourceManifest != nil {
		copy := *manifest.SourceManifest
		rebuilt.SourceManifest = &copy
	}
	if err != nil || !reflect.DeepEqual(rebuilt, manifest) {
		return ReviewManifest{}, fmt.Errorf(
			"realtime computer-use review manifest differs from retained evidence (%s)",
			reviewManifestMismatchField(rebuilt, manifest, err),
		)
	}
	markdownPayload, markdownInfo, err := readReviewFile(root, "REVIEW.md", maximumReviewMarkdownBytes)
	if err != nil || string(markdownPayload) != renderReview(manifest) {
		return ReviewManifest{}, errors.New("realtime computer-use human review is missing or noncanonical")
	}
	if err := verifyIndependentReviewFiles(append(regularInfos, markdownInfo)); err != nil {
		return ReviewManifest{}, err
	}
	if operations.afterSemanticVerification != nil {
		if err := operations.afterSemanticVerification(); err != nil {
			return ReviewManifest{}, err
		}
	}
	if _, err := verifyReviewRootIdentity(directory, root, rootIdentity); err != nil {
		return ReviewManifest{}, err
	}
	afterVerificationSHA, err := captureReviewTreeSHA256(directory, rootIdentity)
	if err != nil || afterVerificationSHA != initialTreeSHA {
		return ReviewManifest{}, errors.New("realtime computer-use review tree changed during verification")
	}
	if operations.afterSecondTreeCapture != nil {
		if err := operations.afterSecondTreeCapture(); err != nil {
			return ReviewManifest{}, err
		}
	}
	if err := reverifyReviewSubtrees(directory, manifest); err != nil {
		return ReviewManifest{}, err
	}
	if _, err := verifyReviewRootIdentity(directory, root, rootIdentity); err != nil {
		return ReviewManifest{}, err
	}
	finalTreeSHA, err := captureReviewTreeSHA256(directory, rootIdentity)
	if err != nil || finalTreeSHA != initialTreeSHA {
		return ReviewManifest{}, errors.New("realtime computer-use review tree changed during final verification")
	}
	return manifest, nil
}

func reviewManifestMismatchField(rebuilt, retained ReviewManifest, buildErr error) string {
	if buildErr != nil {
		return "rebuild"
	}
	if rebuilt.Format != retained.Format || rebuilt.FormatVersion != retained.FormatVersion ||
		rebuilt.Phase != retained.Phase || rebuilt.Suite != retained.Suite ||
		rebuilt.Expected != retained.Expected {
		return "identity"
	}
	if rebuilt.Complete != retained.Complete || rebuilt.Reportable != retained.Reportable ||
		rebuilt.CoreReportable != retained.CoreReportable ||
		rebuilt.CoreReportability != retained.CoreReportability {
		return "completion-reportability"
	}
	if !reflect.DeepEqual(rebuilt.Result, retained.Result) {
		return "result"
	}
	if !reflect.DeepEqual(rebuilt.SourceManifest, retained.SourceManifest) {
		return "source-manifest"
	}
	if !reflect.DeepEqual(rebuilt.Cell, retained.Cell) {
		return "cell"
	}
	if !reflect.DeepEqual(rebuilt.Provenance, retained.Provenance) {
		return "provenance"
	}
	if !reflect.DeepEqual(rebuilt.ReportabilityErrors, retained.ReportabilityErrors) {
		return "reportability-errors"
	}
	if !reflect.DeepEqual(rebuilt.Missing, retained.Missing) {
		return "missing"
	}
	if !reflect.DeepEqual(rebuilt.Attempts, retained.Attempts) {
		return "attempts"
	}
	return "unknown"
}

func reverifyReviewSubtrees(directory string, manifest ReviewManifest) error {
	for index := len(manifest.Attempts) - 1; index >= 0; index-- {
		attempt := manifest.Attempts[index]
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(attempt.MediaBundle.Path))
		if _, err := reviewmedia.VerifyBundle(mediaDirectory, attempt.MediaBundle.ManifestSHA256); err != nil {
			return errors.New("final reverify nested realtime computer-use media bundle")
		}
		if attempt.EvaluationBundle != nil {
			reviewDirectory := filepath.Join(
				directory, filepath.FromSlash(attempt.EvaluationBundle.Path),
			)
			if _, err := revieweval.VerifyEvaluationBundle(
				context.Background(), revieweval.EvaluationBundleOptions{Directory: reviewDirectory},
				evaluationReceiptAt(reviewDirectory, *attempt.EvaluationBundle),
			); err != nil {
				return errors.New("final reverify nested realtime computer-use secondary review")
			}
		}
	}
	return nil
}

func captureReviewTreeSHA256(directory string, expectedRoot os.FileInfo) (string, error) {
	return captureReviewSelectedTreeSHA256(directory, false, "", expectedRoot)
}

func captureReviewSourceTreeSHA256(directory string, expectedRoot os.FileInfo) (string, error) {
	return captureReviewSourceTreeSHA256AtMarker(
		directory, reviewSourceManifest, expectedRoot,
	)
}

func captureReviewSourceTreeSHA256AtMarker(
	directory, marker string, expectedRoot os.FileInfo,
) (string, error) {
	if marker != reviewSourceManifest && marker != reviewSourceManifestStage {
		return "", errors.New("realtime computer-use source snapshot marker is invalid")
	}
	return captureReviewSelectedTreeSHA256(directory, true, marker, expectedRoot)
}

func captureReviewSelectedTreeSHA256(
	directory string, sourceOnly bool, sourceMarker string, expectedRoot os.FileInfo,
) (string, error) {
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", errors.New("open realtime computer-use review tree snapshot")
	}
	if _, err := verifyReviewRootIdentity(directory, root, expectedRoot); err != nil {
		_ = root.Close()
		return "", err
	}
	paths := make([]string, 0, 1024)
	walk := func(walkRoot string) error {
		return filepath.WalkDir(walkRoot, func(
			path string, entry os.DirEntry, walkErr error,
		) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(directory, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if relative == "." {
				return nil
			}
			if len(paths) >= 1_700_000 || !validReviewRelativePath(relative) {
				return errors.New("realtime computer-use review tree snapshot is invalid or too large")
			}
			paths = append(paths, relative)
			return nil
		})
	}
	if sourceOnly {
		paths = append(paths, "SOURCE_REVIEW.md", "result.json", sourceMarker)
		for _, name := range []string{"contexts", "media"} {
			if err := walk(filepath.Join(directory, name)); err != nil {
				_ = root.Close()
				return "", errors.New("enumerate deterministic realtime computer-use source snapshot")
			}
		}
	} else if err := walk(directory); err != nil {
		_ = root.Close()
		return "", errors.New("enumerate realtime computer-use review tree snapshot")
	}
	sort.Strings(paths)
	hash := sha256.New()
	copyBuffer := make([]byte, 64<<10)
	for _, path := range paths {
		info, err := root.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 ||
			(!info.IsDir() && !info.Mode().IsRegular()) ||
			(info.Mode().IsRegular() && reviewFileHasMultipleLinks(info)) {
			_ = root.Close()
			return "", errors.New("realtime computer-use review tree snapshot contains an invalid entry")
		}
		identity := struct {
			Path string `json:"path"`
			Mode uint32 `json:"mode"`
			Size int64  `json:"size"`
		}{Path: path, Mode: uint32(info.Mode()), Size: info.Size()}
		identityPayload, err := json.Marshal(identity)
		if err != nil {
			_ = root.Close()
			return "", errors.New("encode realtime computer-use review tree identity")
		}
		_, _ = hash.Write(identityPayload)
		_, _ = hash.Write([]byte{'\n'})
		if !info.Mode().IsRegular() {
			continue
		}
		file, err := root.Open(path)
		if err != nil {
			_ = root.Close()
			return "", errors.New("open realtime computer-use review tree artifact")
		}
		copied, copyErr := io.CopyBuffer(hash, io.LimitReader(file, info.Size()), copyBuffer)
		if copyErr != nil || copied != info.Size() {
			_ = file.Close()
			_ = root.Close()
			return "", errors.New("hash realtime computer-use review tree artifact")
		}
		after, statErr := file.Stat()
		closeErr := file.Close()
		if closeErr != nil {
			_ = file.Close()
		}
		if statErr != nil || closeErr != nil || !os.SameFile(info, after) || after.Size() != info.Size() {
			_ = root.Close()
			return "", errors.New("realtime computer-use review tree artifact changed while hashing")
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	if err := root.Close(); err != nil {
		_ = root.Close()
		return "", errors.New("close realtime computer-use review tree snapshot")
	}
	current, err := os.Lstat(directory)
	if err != nil || current.Mode()&os.ModeSymlink != 0 ||
		expectedRoot == nil || !os.SameFile(current, expectedRoot) {
		return "", errors.New("realtime computer-use review root identity changed during snapshot")
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyReviewRootIdentity(
	directory string, root *os.Root, expected os.FileInfo,
) (os.FileInfo, error) {
	if root == nil {
		return nil, errors.New("realtime computer-use review root is unavailable")
	}
	if err := verifyReviewNoSymlinkAncestors(directory); err != nil {
		return nil, err
	}
	anchored, rootErr := root.Stat(".")
	current, pathErr := os.Lstat(directory)
	if rootErr != nil || pathErr != nil || !anchored.IsDir() || !current.IsDir() ||
		current.Mode()&os.ModeSymlink != 0 || !os.SameFile(anchored, current) ||
		(expected != nil && !os.SameFile(anchored, expected)) {
		return nil, errors.New("realtime computer-use review root identity changed")
	}
	return anchored, nil
}

func decodeReviewManifest(payload []byte) (ReviewManifest, error) {
	var manifest ReviewManifest
	if len(payload) == 0 || len(payload) > maximumReviewManifestBytes || strictjson.Validate(payload) != nil {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ReviewManifest{}, errors.New("decode realtime computer-use review manifest")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ReviewManifest{}, err
	}
	canonical, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || !bytes.Equal(append(canonical, '\n'), payload) {
		return ReviewManifest{}, errors.New("realtime computer-use review manifest is noncanonical")
	}
	return manifest, nil
}

func decodeReviewResult(payload []byte) (bench.Result, error) {
	var result bench.Result
	if len(payload) == 0 || len(payload) > maximumReviewResultBytes || strictjson.Validate(payload) != nil {
		return bench.Result{}, errors.New("realtime computer-use retained result is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return bench.Result{}, errors.New("decode realtime computer-use retained result")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return bench.Result{}, err
	}
	canonical, err := json.MarshalIndent(result, "", "  ")
	if err != nil || !bytes.Equal(append(canonical, '\n'), payload) {
		return bench.Result{}, errors.New("realtime computer-use retained result is noncanonical")
	}
	return result, nil
}

func encodeReviewResult(result bench.Result) ([]byte, error) {
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) >= maximumReviewResultBytes ||
		strictjson.Validate(payload) != nil {
		return nil, errors.New("encode exact realtime computer-use benchmark result")
	}
	return append(payload, '\n'), nil
}

func decodeReviewContext(payload []byte) (realtimeCUReviewContext, error) {
	var value realtimeCUReviewContext
	if len(payload) == 0 || len(payload) > maximumReviewContextBytes || strictjson.Validate(payload) != nil {
		return realtimeCUReviewContext{}, errors.New("realtime computer-use review context is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return realtimeCUReviewContext{}, errors.New("decode realtime computer-use review context")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return realtimeCUReviewContext{}, err
	}
	canonical, err := canonicalReviewObject(value, maximumReviewContextBytes)
	if err != nil || !bytes.Equal(canonical, payload) {
		return realtimeCUReviewContext{}, errors.New("realtime computer-use review context is noncanonical")
	}
	return value, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("realtime computer-use review JSON has trailing data")
}

func readReviewFile(root *os.Root, path string, maximum int64) ([]byte, os.FileInfo, error) {
	if root == nil || maximum <= 0 || !validReviewRelativePath(path) {
		return nil, nil, errors.New("invalid realtime computer-use review file path")
	}
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximum {
		return nil, nil, errors.New("realtime computer-use review file is missing, non-regular, or oversized")
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, nil, err
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if closeErr != nil {
		_ = file.Close()
	}
	if readErr != nil || statErr != nil || closeErr != nil ||
		int64(len(payload)) != info.Size() || int64(len(payload)) > maximum {
		return nil, nil, errors.New("read bounded realtime computer-use review file")
	}
	if !os.SameFile(info, after) {
		return nil, nil, errors.New("realtime computer-use review file changed while reading")
	}
	return payload, info, nil
}

func artifactMatches(artifact ReviewArtifact, payload []byte) bool {
	return artifact.SizeBytes == int64(len(payload)) && artifact.SizeBytes > 0 &&
		artifact.SHA256 == reviewDigest(payload)
}

func verifyReviewDirectoryEntries(root *os.Root, path string, expected []string) error {
	if root == nil || (path != "." && !validReviewRelativePath(path)) {
		return errors.New("invalid realtime computer-use review directory")
	}
	directory, err := root.Open(path)
	if err != nil {
		return errors.New("open realtime computer-use review directory")
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("read realtime computer-use review directory")
	}
	actual := make([]string, len(entries))
	for index, entry := range entries {
		actual[index] = entry.Name()
	}
	sort.Strings(actual)
	want := slices.Clone(expected)
	sort.Strings(want)
	if !slices.Equal(actual, want) {
		return errors.New("realtime computer-use review directory has missing or extra entries")
	}
	return nil
}

func verifyIndependentReviewFiles(infos []os.FileInfo) error {
	for left := range infos {
		if infos[left] == nil || !infos[left].Mode().IsRegular() {
			return errors.New("realtime computer-use review artifact identity is invalid")
		}
		for right := 0; right < left; right++ {
			if os.SameFile(infos[left], infos[right]) {
				return errors.New("realtime computer-use review artifacts alias one physical file")
			}
		}
	}
	return nil
}

type reviewSealEntry struct {
	path string
	info os.FileInfo
}

func sealReviewTree(directory string) (resultErr error) {
	root, identity, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use review tree after sealing"))
		}
	}()
	return sealReviewTreeRoot(directory, root, identity)
}

func sealReviewTreeRoot(directory string, root *os.Root, expectedRoot os.FileInfo) error {
	if _, err := verifyReviewRootIdentity(directory, root, expectedRoot); err != nil {
		return err
	}
	entries, err := collectReviewSealEntries(root, []string{"."})
	if err != nil {
		return err
	}
	if err := applyReviewSeal(root, entries, true); err != nil {
		return err
	}
	if err := syncReviewRoot(root); err != nil {
		return errors.New("sync sealed realtime computer-use review tree")
	}
	_, err = verifyReviewRootIdentity(directory, root, expectedRoot)
	return err
}

func sealReviewSourceArtifacts(directory string) (resultErr error) {
	root, identity, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close deterministic realtime computer-use source after sealing"))
		}
	}()
	return sealReviewSourceArtifactsRoot(directory, root, identity)
}

func sealReviewSourceArtifactsRoot(
	directory string, root *os.Root, expectedRoot os.FileInfo,
) error {
	return sealReviewSourceArtifactsRootAtMarker(
		directory, root, expectedRoot, reviewSourceManifest,
	)
}

func sealReviewSourceArtifactsRootAtMarker(
	directory string, root *os.Root, expectedRoot os.FileInfo, marker string,
) error {
	if marker != reviewSourceManifest && marker != reviewSourceManifestStage {
		return errors.New("deterministic realtime computer-use source marker is invalid")
	}
	if _, err := verifyReviewRootIdentity(directory, root, expectedRoot); err != nil {
		return err
	}
	entries, err := collectReviewSealEntries(root, []string{"contexts", "media"})
	if err != nil {
		return err
	}
	for _, name := range []string{"SOURCE_REVIEW.md", "result.json", marker} {
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			reviewFileHasMultipleLinks(info) {
			return errors.New("deterministic realtime computer-use source marker is invalid or aliased")
		}
		entries = append(entries, reviewSealEntry{path: name, info: info})
	}
	if err := applyReviewSeal(root, entries, false); err != nil {
		return err
	}
	if err := syncReviewRoot(root); err != nil {
		return errors.New("sync deterministic realtime computer-use source")
	}
	_, err = verifyReviewRootIdentity(directory, root, expectedRoot)
	return err
}

func collectReviewSealEntries(root *os.Root, roots []string) ([]reviewSealEntry, error) {
	if root == nil {
		return nil, errors.New("realtime computer-use review root is unavailable for sealing")
	}
	const maximumEntries = 1_700_000
	entries := make([]reviewSealEntry, 0, 1024)
	var walk func(string) error
	walk = func(path string) error {
		if path != "." && !validReviewRelativePath(path) {
			return errors.New("realtime computer-use review seal path is invalid")
		}
		info, err := root.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 ||
			(!info.IsDir() && !info.Mode().IsRegular()) ||
			(info.Mode().IsRegular() && reviewFileHasMultipleLinks(info)) {
			return errors.New("realtime computer-use review tree contains an invalid or aliased entry")
		}
		if len(entries) >= maximumEntries {
			return errors.New("realtime computer-use review tree has too many entries")
		}
		entries = append(entries, reviewSealEntry{path: path, info: info})
		if !info.IsDir() {
			return nil
		}
		directory, err := root.Open(path)
		if err != nil {
			return errors.New("open realtime computer-use review directory for sealing")
		}
		children, readErr := directory.ReadDir(-1)
		closeErr := directory.Close()
		if readErr != nil || closeErr != nil {
			return errors.New("enumerate realtime computer-use review directory for sealing")
		}
		sort.Slice(children, func(left, right int) bool {
			return children[left].Name() < children[right].Name()
		})
		for _, child := range children {
			childPath := child.Name()
			if path != "." {
				childPath = filepath.ToSlash(filepath.Join(path, child.Name()))
			}
			if err := walk(childPath); err != nil {
				return err
			}
		}
		return nil
	}
	for _, path := range roots {
		if err := walk(path); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func applyReviewSeal(root *os.Root, entries []reviewSealEntry, includeRoot bool) error {
	files := make([]reviewSealEntry, 0, len(entries))
	directories := make([]reviewSealEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.info.IsDir() {
			directories = append(directories, entry)
		} else {
			files = append(files, entry)
		}
	}
	seal := func(entry reviewSealEntry, mode os.FileMode) error {
		if entry.path == "." && !includeRoot {
			return nil
		}
		visible, err := root.Lstat(entry.path)
		if err != nil || !os.SameFile(visible, entry.info) ||
			visible.Mode()&os.ModeSymlink != 0 ||
			(visible.Mode().IsRegular() && reviewFileHasMultipleLinks(visible)) {
			return errors.New("realtime computer-use review entry changed before sealing")
		}
		file, err := root.Open(entry.path)
		if err != nil {
			return errors.New("open realtime computer-use review entry for sealing")
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(visible, opened) {
			_ = file.Close()
			return errors.New("realtime computer-use review entry changed while opening for sealing")
		}
		if err := file.Chmod(mode); err != nil {
			_ = file.Close()
			return errors.New("seal realtime computer-use review entry")
		}
		after, statErr := file.Stat()
		closeErr := file.Close()
		if closeErr != nil {
			_ = file.Close()
		}
		visibleAfter, visibleErr := root.Lstat(entry.path)
		if statErr != nil || closeErr != nil || visibleErr != nil ||
			!os.SameFile(opened, after) || !os.SameFile(after, visibleAfter) ||
			visibleAfter.Mode()&os.ModeSymlink != 0 || after.Mode().Perm()&0o222 != 0 ||
			(after.Mode().IsRegular() && reviewFileHasMultipleLinks(after)) {
			return errors.New("realtime computer-use review entry changed while sealing")
		}
		return nil
	}
	for _, entry := range files {
		if err := seal(entry, 0o400); err != nil {
			return err
		}
	}
	sort.Slice(directories, func(left, right int) bool {
		leftDepth := strings.Count(directories[left].path, "/")
		rightDepth := strings.Count(directories[right].path, "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return directories[left].path > directories[right].path
	})
	for _, entry := range directories {
		if err := seal(entry, 0o500); err != nil {
			return err
		}
	}
	return nil
}

func verifySealedReviewSourceArtifacts(directory string) error {
	return verifySealedReviewSourceArtifactsAtMarker(directory, reviewSourceManifest)
}

func verifySealedReviewSourceArtifactsAtMarker(directory, marker string) error {
	if marker != reviewSourceManifest && marker != reviewSourceManifestStage {
		return errors.New("deterministic realtime computer-use source marker is invalid")
	}
	rootInfo, err := os.Lstat(directory)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("deterministic realtime computer-use source root is invalid")
	}
	for _, name := range []string{"SOURCE_REVIEW.md", "result.json", marker} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0o222 != 0 || reviewFileHasMultipleLinks(info) {
			return errors.New("deterministic realtime computer-use source marker is unsealed or aliased")
		}
	}
	for _, name := range []string{"contexts", "media"} {
		if err := filepath.WalkDir(filepath.Join(directory, name), func(
			path string, entry os.DirEntry, walkErr error,
		) error {
			if walkErr != nil {
				return errors.New("walk deterministic realtime computer-use source")
			}
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 ||
				(info.Mode().IsRegular() && reviewFileHasMultipleLinks(info)) ||
				(!info.IsDir() && !info.Mode().IsRegular()) {
				return errors.New("deterministic realtime computer-use source is unsealed or invalid")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	reviews, err := os.Lstat(filepath.Join(directory, "reviews"))
	if err != nil || !reviews.IsDir() || reviews.Mode()&os.ModeSymlink != 0 {
		return errors.New("deterministic realtime computer-use review sibling directory is invalid")
	}
	return nil
}

func verifyReviewSourceRootEntries(root *os.Root) error {
	return verifyReviewSourceRootEntriesAtMarker(root, reviewSourceManifest)
}

func verifyReviewSourceRootEntriesAtMarker(root *os.Root, marker string) error {
	if root == nil {
		return errors.New("deterministic realtime computer-use source root is unavailable")
	}
	if marker != reviewSourceManifest && marker != reviewSourceManifestStage {
		return errors.New("deterministic realtime computer-use source marker is invalid")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open deterministic realtime computer-use source root")
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("read deterministic realtime computer-use source root")
	}
	required := map[string]bool{
		"SOURCE_REVIEW.md": false, "contexts": false, "media": false,
		"result.json": false, "reviews": false, marker: false,
	}
	optional := map[string]struct{}{"REVIEW.md": {}, "manifest.json": {}}
	for _, entry := range entries {
		if _, ok := required[entry.Name()]; ok {
			required[entry.Name()] = true
			continue
		}
		if _, ok := optional[entry.Name()]; ok {
			continue
		}
		return errors.New("deterministic realtime computer-use source root has an unexpected entry")
	}
	for _, present := range required {
		if !present {
			return errors.New("deterministic realtime computer-use source root is incomplete")
		}
	}
	return nil
}

func verifySealedReviewTree(directory string) error {
	const maximumEntries = 1_700_000
	count := 0
	return filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("walk sealed realtime computer-use review bundle")
		}
		count++
		if count > maximumEntries {
			return errors.New("realtime computer-use review bundle has too many entries")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 ||
			(info.Mode().IsRegular() && reviewFileHasMultipleLinks(info)) {
			return errors.New("realtime computer-use review bundle is unsealed or contains a symlink")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("realtime computer-use review bundle contains a non-regular entry")
		}
		return nil
	})
}

func reviewFileHasMultipleLinks(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Sys() == nil {
		return false
	}
	value := reflect.ValueOf(info.Sys())
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	links := value.FieldByName("Nlink")
	if !links.IsValid() {
		return false
	}
	switch links.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return links.Uint() > 1
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return links.Int() > 1
	default:
		return false
	}
}

func openReviewDirectoryRoot(path string) (*os.Root, os.FileInfo, error) {
	if _, err := validateNewReviewDirectory(path); err != nil {
		return nil, nil, err
	}
	if err := verifyReviewNoSymlinkAncestors(path); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("realtime computer-use review parent is invalid")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, errors.New("open realtime computer-use review parent")
	}
	anchored, err := root.Stat(".")
	if err != nil || !os.SameFile(info, anchored) {
		_ = root.Close()
		return nil, nil, errors.New("realtime computer-use review parent identity changed")
	}
	return root, anchored, nil
}

func verifyReviewNoSymlinkAncestors(path string) error {
	if _, err := validateNewReviewDirectory(path); err != nil {
		return err
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("realtime computer-use review path has a symlinked or invalid ancestor")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func syncReviewRoot(root *os.Root) error {
	if root == nil {
		return errors.New("realtime computer-use review root is unavailable for sync")
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func syncFilesystemDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if closeErr != nil {
		_ = file.Close()
	}
	return errors.Join(syncErr, closeErr)
}

func invalidateReviewManifest(directory string) error {
	return invalidateReviewManifestExpected(directory, nil)
}

func invalidateReviewManifestExpected(directory string, expectedRoot os.FileInfo) (resultErr error) {
	root, identity, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close realtime computer-use review root after invalidation"))
		}
	}()
	if expectedRoot != nil && !os.SameFile(identity, expectedRoot) {
		return errors.New("refuse to invalidate a different realtime computer-use review root")
	}
	return invalidateReviewManifestRoot(root)
}

func invalidateReviewManifestRoot(root *os.Root) error {
	return invalidateReviewPublicationRoot(root, true)
}

func recoverRejectedReviewPublication(directory string) (resultErr error) {
	if _, err := os.Lstat(filepath.Join(directory, "manifest.json")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return errors.New("inspect rejected realtime computer-use review manifest")
	}
	if _, err := os.Lstat(filepath.Join(directory, "REVIEW.md")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.New("inspect rejected realtime computer-use human review")
	}
	root, identity, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close rejected realtime computer-use review publication"))
		}
		current, statErr := os.Lstat(directory)
		if statErr != nil || current.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(current, identity) {
			resultErr = errors.Join(resultErr,
				errors.New("rejected realtime computer-use review root changed"))
		}
	}()
	return invalidateReviewPublicationRoot(root, false)
}

func invalidateReviewPublicationRoot(root *os.Root, requireManifest bool) (resultErr error) {
	if root == nil {
		return errors.New("review manifest root is unavailable")
	}
	if !requireManifest {
		if _, err := root.Lstat("manifest.json"); err == nil {
			return errors.New("refuse to recover a review publication with a manifest")
		} else if !os.IsNotExist(err) {
			return errors.New("inspect rejected review manifest through anchored root")
		}
	}
	names := []string{"REVIEW.md"}
	if requireManifest {
		names = append([]string{"manifest.json"}, names...)
	}
	infos := make(map[string]os.FileInfo, len(names))
	for _, name := range names {
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			reviewFileHasMultipleLinks(info) {
			return errors.New("review publication is not a regular owned file set")
		}
		infos[name] = info
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !rootInfo.IsDir() {
		return errors.New("review manifest root identity is invalid")
	}
	if err := chmodReviewEntryHandle(root, ".", rootInfo, 0o700); err != nil {
		return err
	}
	defer func() {
		if restoreErr := chmodReviewEntryHandle(root, ".", rootInfo, 0o500); restoreErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("reseal rejected realtime computer-use review root"))
		}
	}()
	for _, name := range names {
		info := infos[name]
		if err := chmodReviewEntryHandle(root, name, info, 0o600); err != nil {
			return err
		}
		visible, err := root.Lstat(name)
		if err != nil || !os.SameFile(info, visible) || visible.Mode()&os.ModeSymlink != 0 {
			return errors.New("review publication changed before invalidation")
		}
	}
	for _, name := range names {
		if err := root.Remove(name); err != nil {
			return err
		}
	}
	return syncReviewRoot(root)
}

func invalidateReviewSourceManifest(directory string) (resultErr error) {
	root, _, err := openReviewDirectoryRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close deterministic realtime computer-use source after invalidation"))
		}
	}()
	return invalidateReviewSourceManifestRoot(root)
}

func invalidateReviewSourceManifestRoot(root *os.Root) error {
	if root == nil {
		return errors.New("deterministic source root is unavailable")
	}
	info, err := root.Lstat("source.manifest.json")
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		reviewFileHasMultipleLinks(info) {
		return errors.New("deterministic source manifest is not a regular owned file")
	}
	if err := chmodReviewEntryHandle(root, "source.manifest.json", info, 0o600); err != nil {
		return err
	}
	visible, err := root.Lstat("source.manifest.json")
	if err != nil || !os.SameFile(info, visible) || visible.Mode()&os.ModeSymlink != 0 {
		return errors.New("deterministic source manifest changed before invalidation")
	}
	if err := root.Remove("source.manifest.json"); err != nil {
		return err
	}
	return syncReviewRoot(root)
}

func chmodReviewEntryHandle(
	root *os.Root, path string, expected os.FileInfo, mode os.FileMode,
) error {
	if root == nil || expected == nil {
		return errors.New("realtime computer-use review chmod identity is unavailable")
	}
	visible, err := root.Lstat(path)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !os.SameFile(visible, expected) ||
		(visible.Mode().IsRegular() && reviewFileHasMultipleLinks(visible)) {
		return errors.New("realtime computer-use review chmod entry changed")
	}
	file, err := root.Open(path)
	if err != nil {
		return errors.New("open realtime computer-use review entry for chmod")
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(visible, opened) {
		_ = file.Close()
		return errors.New("realtime computer-use review chmod entry changed while opening")
	}
	chmodErr := file.Chmod(mode)
	after, afterErr := file.Stat()
	closeErr := file.Close()
	if closeErr != nil {
		_ = file.Close()
	}
	visibleAfter, visibleErr := root.Lstat(path)
	if chmodErr != nil || afterErr != nil || closeErr != nil || visibleErr != nil ||
		!os.SameFile(opened, after) || !os.SameFile(after, visibleAfter) ||
		visibleAfter.Mode()&os.ModeSymlink != 0 || after.Mode().Perm() != mode.Perm() ||
		(after.Mode().IsRegular() && reviewFileHasMultipleLinks(after)) {
		return errors.New("realtime computer-use review chmod entry changed")
	}
	return nil
}

func encodeReviewPublication(
	manifest ReviewManifest, sensitive []string,
) ([]byte, []byte, error) {
	markdown := []byte(renderReview(manifest))
	if len(markdown) == 0 || len(markdown) > maximumReviewMarkdownBytes ||
		containsReviewSecret(sensitive, markdown) {
		return nil, nil,
			errors.New("realtime computer-use human review is invalid, oversized, or sensitive")
	}
	manifestPayload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || len(manifestPayload) == 0 || len(manifestPayload) >= maximumReviewManifestBytes ||
		strictjson.Validate(manifestPayload) != nil {
		return nil, nil, errors.New("encode realtime computer-use review manifest")
	}
	manifestPayload = append(manifestPayload, '\n')
	if containsReviewSecret(sensitive, manifestPayload) {
		return nil, nil, errors.New("realtime computer-use review manifest contains a sensitive value")
	}
	return manifestPayload, markdown, nil
}

func renderReview(manifest ReviewManifest) string {
	var output strings.Builder
	output.WriteString("# OpenRealtime Realtime-CU v1 case-by-case review\n\n")
	fmt.Fprintf(&output, "- Evidence phase: `%s`\n", markdownEscape(manifest.Phase))
	fmt.Fprintf(&output, "- Evidence coverage: %d/%d cases\n", len(manifest.Attempts), manifest.Expected)
	fmt.Fprintf(&output, "- Complete: %t\n", manifest.Complete)
	fmt.Fprintf(&output, "- Core benchmark reportable: %t\n", manifest.CoreReportable)
	fmt.Fprintf(&output, "- Review bundle reportable: %t\n", manifest.Reportable)
	fmt.Fprintf(&output, "- Source revision: `%s`\n", markdownEscape(manifest.Provenance.Revision))
	fmt.Fprintf(&output, "- Working tree modified during run: %t\n", manifest.Provenance.Modified)
	if manifest.SourceManifest != nil {
		fmt.Fprintf(&output, "- [Deterministic source manifest](%s) (`%s`)\n",
			manifest.SourceManifest.Path, manifest.SourceManifest.SHA256)
	}
	if manifest.CoreReportability != "" {
		fmt.Fprintf(&output, "- Core refusal: %s\n", markdownEscape(manifest.CoreReportability))
	}
	if len(manifest.ReportabilityErrors) > 0 {
		output.WriteString("\n## Publication refusals\n\n")
		for _, reason := range manifest.ReportabilityErrors {
			fmt.Fprintf(&output, "- %s\n", markdownEscape(reason))
		}
	}
	if len(manifest.Missing) > 0 {
		output.WriteString("\n## Missing cases\n\n")
		for _, missing := range manifest.Missing {
			fmt.Fprintf(&output, "- `%s`: %s\n", markdownEscape(missing.Case), markdownEscape(missing.Reason))
		}
	}
	for _, attempt := range manifest.Attempts {
		status := "FAIL"
		if !attempt.Deterministic.Completed {
			status = "INCOMPLETE"
		} else if attempt.Deterministic.Passed {
			status = "PASS"
		}
		fmt.Fprintf(&output, "\n## %02d. %s — %s\n\n", attempt.Ordinal,
			markdownEscape(attempt.Case), status)
		fmt.Fprintf(&output, "- Grounding: `%s`\n", markdownEscape(string(attempt.Grounding)))
		fmt.Fprintf(&output, "- Deterministic outcome: **%s**\n", status)
		if attempt.Deterministic.Error != "" {
			fmt.Fprintf(&output, "- Infrastructure error: %s\n", markdownEscape(attempt.Deterministic.Error))
		}
		if reason := attempt.Deterministic.Notes["page_result"]; reason != "" {
			fmt.Fprintf(&output, "- Browser evaluator: %s\n", markdownEscape(reason))
		}
		fmt.Fprintf(&output, "- Execution evidence: `%s`\n", markdownEscape(attempt.ExecutionStatus))
		fmt.Fprintf(&output, "- Secondary review: `%s`\n", markdownEscape(attempt.ReviewStatus))
		if attempt.ReviewStatus == "complete" && attempt.Reviewer != nil &&
			attempt.Assessment != nil && attempt.EvaluationBundle != nil {
			fmt.Fprintf(&output, "- Reviewer plug-in: `%s` / `%s`\n",
				markdownEscape(attempt.Reviewer.Provider), markdownEscape(attempt.Reviewer.Model))
			fmt.Fprintf(&output, "- Provider-reviewed observation: `%s` (confidence %.3f)\n",
				markdownEscape(attempt.Assessment.ObservedOutcome), attempt.Assessment.Confidence)
			fmt.Fprintf(&output, "- Provider-reviewed summary: %s\n",
				markdownEscape(attempt.Assessment.Summary))
			fmt.Fprintf(&output, "- Reviewer evidence scope: %s\n", markdownEscape(attempt.EvidenceScope))
			fmt.Fprintf(&output, "- Authenticity caveat: %s\n", markdownEscape(attempt.AuthenticityCaveat))
			fmt.Fprintf(&output, "- [Sealed provider exchange and assessment](%s/manifest.json)\n",
				attempt.EvaluationBundle.Path)
			writeReviewFindings(&output, "Significant problems", attempt.Assessment.SignificantProblems)
			writeReviewFindings(&output, "Minor observations", attempt.Assessment.MinorObservations)
			fmt.Fprintf(&output, "- Limitations: %d\n", len(attempt.Assessment.Limitations))
			if len(attempt.Assessment.Limitations) == 0 {
				output.WriteString("  - None.\n")
			} else {
				for _, limitation := range attempt.Assessment.Limitations {
					fmt.Fprintf(&output, "  - %s\n", markdownEscape(limitation))
				}
			}
		}
		fmt.Fprintf(&output, "- [Exact deterministic context](%s)\n", attempt.Context.Path)
		fmt.Fprintf(&output, "- [Raw frames, timelines, encoder/attestor provenance, and media manifest](%s/manifest.json)\n",
			attempt.MediaBundle.Path)
		output.WriteString("- Human playback media:\n")
		for _, media := range attempt.Media {
			fmt.Fprintf(&output, "  - [%s — %s](%s) (`%s`)\n",
				markdownEscape(media.Kind), markdownEscape(media.Role), media.Path, media.SHA256)
		}
		fmt.Fprintf(&output, "- Attempt reportable: %t\n", attempt.Reportable)
		if attempt.ReportabilityNote != "" {
			fmt.Fprintf(&output, "- Attempt refusal: %s\n", markdownEscape(attempt.ReportabilityNote))
		}
	}
	output.WriteString("\n## Interpretation\n\n")
	output.WriteString("The deterministic browser scorer is the pass/fail authority. Retained media and secondary multimodal assessments support human diagnosis; they do not replace the benchmark result. Each MP4 is independently full-decoded against the raw frame timeline and contains the time-aligned room/user and agent audio tracks. A sealed secondary-review bundle proves a provider plug-in verified its in-process exchange and the retained local digests; it does not independently attest that a remote service originated that exchange.\n")
	return output.String()
}

func writeReviewFindings(
	output *strings.Builder, label string, findings []revieweval.Finding,
) {
	fmt.Fprintf(output, "- %s: %d\n", label, len(findings))
	if len(findings) == 0 {
		output.WriteString("  - None.\n")
		return
	}
	for _, finding := range findings {
		fmt.Fprintf(output, "  - `%s`: %s — %s%s\n",
			markdownEscape(finding.Category), markdownEscape(finding.Evidence),
			markdownEscape(finding.Impact), reviewFindingTime(finding))
	}
}

func reviewFindingTime(finding revieweval.Finding) string {
	switch {
	case finding.StartMS != nil && finding.EndMS != nil:
		return fmt.Sprintf(" (at %d–%d ms)", *finding.StartMS, *finding.EndMS)
	case finding.StartMS != nil:
		return fmt.Sprintf(" (at %d ms)", *finding.StartMS)
	case finding.EndMS != nil:
		return fmt.Sprintf(" (through %d ms)", *finding.EndMS)
	default:
		return ""
	}
}

var reviewMarkdownReplacer = strings.NewReplacer(
	"&", "&amp;", "\\", "\\\\", "`", "\\`", "*", "\\*",
	"_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;",
	"#", "\\#", "!", "\\!", "|", "\\|", "\n", " ", "\r", " ",
)

func markdownEscape(value string) string { return reviewMarkdownReplacer.Replace(value) }
