// Package campaign composes a sealed candidate source bundle with one selected
// offline review provider. It is deliberately outside every realtime server,
// benchmark runner, and presentation client: those components depend only on
// the candidate evidence API and never on Gemini or filesystem publication.
package campaign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
)

const maximumConcurrency = 16

type Options struct {
	SourceDirectory     string
	SourceReceiptPath   string
	EvaluationDirectory string
	ReceiptDirectory    string
	QuarantineDirectory string
	Reviewer            *review.ProviderLease
	SensitiveValues     []string
	Concurrency         int
}

type CaseEvaluation struct {
	AttemptID     string                         `json:"attempt_id"`
	Suite         string                         `json:"suite"`
	Case          string                         `json:"case"`
	Trial         int                            `json:"trial"`
	Recovered     bool                           `json:"-"`
	ReceiptPath   string                         `json:"receipt_path"`
	Receipt       review.EvaluationBundleReceipt `json:"receipt"`
	Deterministic bench.TaskOutcome              `json:"deterministic_outcome"`
	Assessment    review.Assessment              `json:"assessment"`
}

type Result struct {
	Source      sourcebundle.Receipt      `json:"source_receipt"`
	Provider    review.ProviderDescriptor `json:"provider"`
	Expected    int                       `json:"expected_evaluations"`
	Evaluations []CaseEvaluation          `json:"evaluations"`
}

type evaluationJob struct {
	index   int
	request review.Request
}

type evaluationResult struct {
	index int
	value CaseEvaluation
	err   error
}

type productionResult struct {
	count int
	err   error
}

// Run performs a bounded-concurrency advisory campaign. Every per-attempt
// evaluation uses the crash-recoverable create-only publication primitive;
// a rerun recovers durable receipts and never invokes the provider twice for
// an already committed attempt.
func Run(ctx context.Context, options Options) (result Result, resultErr error) {
	if ctx == nil {
		return Result{}, errors.New("run candidate review campaign: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	configuration, err := prepareOptions(options)
	if err != nil {
		return Result{}, err
	}
	source, err := sourcebundle.OpenReviewSource(
		ctx, configuration.SourceDirectory, configuration.SourceReceiptPath,
	)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		closeErr := source.Close(context.WithoutCancel(ctx))
		resultErr = errors.Join(resultErr, closeErr)
		if closeErr != nil {
			result = Result{}
		}
	}()
	manifest, err := source.Manifest()
	if err != nil {
		return Result{}, err
	}
	for _, attempt := range manifest.Attempts {
		if !attempt.EvidenceComplete || attempt.Media == nil || attempt.ContextPath == "" {
			return Result{}, errors.New(
				"candidate review campaign requires complete source evidence for every new attempt",
			)
		}
	}
	if err := ensureCampaignDirectories(configuration); err != nil {
		return Result{}, err
	}
	sourceReceipt, err := source.Receipt()
	if err != nil {
		return Result{}, err
	}
	result = Result{
		Source:   sourceReceipt,
		Provider: configuration.Reviewer.Descriptor(), Expected: manifest.AttemptCount,
		Evaluations: make([]CaseEvaluation, manifest.AttemptCount),
	}

	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan evaluationJob, configuration.Concurrency)
	completed := make(chan evaluationResult, configuration.Concurrency)
	var workers sync.WaitGroup
	for range configuration.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				value, jobErr := evaluateOne(runContext, configuration, job.request)
				completed <- evaluationResult{index: job.index, value: value, err: jobErr}
				if jobErr != nil {
					cancel(jobErr)
					return
				}
			}
		}()
	}
	production := make(chan productionResult, 1)
	go func() {
		produced := 0
		defer close(jobs)
		for produced < manifest.AttemptCount {
			request, found, err := source.Next(runContext, configuration.SensitiveValues)
			if err != nil {
				production <- productionResult{count: produced, err: err}
				return
			}
			if !found {
				production <- productionResult{count: produced, err: errors.New(
					"candidate review source ended before its declared population",
				)}
				return
			}
			select {
			case jobs <- evaluationJob{index: produced, request: request}:
				produced++
			case <-runContext.Done():
				production <- productionResult{count: produced, err: context.Cause(runContext)}
				return
			}
		}
		if _, found, err := source.Next(runContext, configuration.SensitiveValues); err != nil {
			production <- productionResult{count: produced, err: err}
			return
		} else if found {
			production <- productionResult{count: produced, err: errors.New(
				"candidate review source exceeds its declared population",
			)}
			return
		}
		production <- productionResult{count: produced}
	}()
	go func() {
		workers.Wait()
		close(completed)
	}()

	var failures []error
	seen := make([]bool, manifest.AttemptCount)
	for item := range completed {
		if item.err != nil {
			failures = append(failures, item.err)
			continue
		}
		if item.index < 0 || item.index >= len(result.Evaluations) || seen[item.index] {
			failures = append(failures, errors.New("candidate review worker returned an invalid result index"))
			continue
		}
		seen[item.index] = true
		result.Evaluations[item.index] = item.value
	}
	produced := <-production
	if produced.err != nil {
		failures = append(failures, produced.err)
		cancel(produced.err)
	}
	if cause := context.Cause(runContext); cause != nil && !errors.Is(cause, context.Canceled) {
		failures = append(failures, cause)
	}
	for index, present := range seen {
		if index < produced.count && !present {
			failures = append(failures, fmt.Errorf("candidate review attempt %d did not publish", index+1))
		}
	}
	if len(failures) != 0 || produced.count != manifest.AttemptCount {
		return result, errors.Join(failures...)
	}
	for index, evaluation := range result.Evaluations {
		entry := manifest.Attempts[index]
		if evaluation.AttemptID != entry.AttemptID || evaluation.Case != entry.Case ||
			evaluation.Trial != entry.Trial || evaluation.Suite != entry.Suite {
			return result, errors.New("candidate review results differ from source order")
		}
		result.Evaluations[index].Deterministic = cloneOutcome(entry.Deterministic)
	}
	return cloneResult(result), nil
}

func evaluateOne(
	ctx context.Context, options Options, request review.Request,
) (value CaseEvaluation, resultErr error) {
	if err := ctx.Err(); err != nil {
		return CaseEvaluation{}, err
	}
	name := digestName(request.AttemptID)
	finalDirectory := filepath.Join(options.EvaluationDirectory, name)
	receiptPath := filepath.Join(options.ReceiptDirectory, name+".receipt.json")
	publication, preparation, err := review.BeginEvaluationBundlePublication(
		ctx, review.EvaluationBundlePublicationConfig{
			Bundle: review.EvaluationBundleOptions{
				Directory: finalDirectory, SensitiveValues: slices.Clone(options.SensitiveValues),
			},
			ReceiptStore:        review.FileEvaluationBundleReceiptStore{Path: receiptPath},
			QuarantineDirectory: options.QuarantineDirectory,
		},
	)
	if err != nil {
		return CaseEvaluation{}, fmt.Errorf("prepare candidate review %s: %w", request.Case, err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, publication.Close())
		if resultErr != nil {
			value = CaseEvaluation{}
		}
	}()
	recovered := preparation.Recovered != nil
	var receipt review.EvaluationBundleReceipt
	var assessment review.Assessment
	if recovered {
		prepared, err := review.PrepareContext(ctx, request)
		if err != nil {
			return CaseEvaluation{}, fmt.Errorf("prepare candidate review input %s: %w", request.Case, err)
		}
		opened := *preparation.Recovered
		if err := matchEvaluation(ctx, prepared, options.Reviewer.Descriptor(), opened); err != nil {
			return CaseEvaluation{}, fmt.Errorf("bind candidate review %s: %w", request.Case, err)
		}
		receipt = opened.Receipt
		assessment = opened.Record.Assessment
	} else {
		evaluation, err := review.Evaluate(ctx, options.Reviewer, request)
		if err != nil {
			return CaseEvaluation{}, fmt.Errorf("evaluate candidate review %s: %w", request.Case, err)
		}
		receipt, err = publication.Write(ctx, evaluation)
		if err != nil {
			return CaseEvaluation{}, fmt.Errorf("publish candidate review %s: %w", request.Case, err)
		}
		if err := matchFreshEvaluation(request, options.Reviewer.Descriptor(), evaluation.Record); err != nil {
			return CaseEvaluation{}, fmt.Errorf("bind fresh candidate review %s: %w", request.Case, err)
		}
		assessment = evaluation.Record.Assessment
	}
	return CaseEvaluation{
		AttemptID: request.AttemptID, Suite: request.Suite, Case: request.Case, Trial: request.Trial,
		Recovered: recovered, ReceiptPath: receiptPath, Receipt: receipt,
		Assessment: assessment,
	}, nil
}

func matchFreshEvaluation(
	request review.Request, descriptor review.ProviderDescriptor, record review.Record,
) error {
	if record.AttemptID != request.AttemptID || record.Suite != request.Suite ||
		record.Case != request.Case || record.Trial != request.Trial || record.Provider != descriptor ||
		len(record.Media) != len(request.Media) {
		return errors.New("fresh evaluation identity differs from its source request")
	}
	for index := range record.Media {
		want := request.Media[index]
		got := record.Media[index]
		if got.Kind != want.Kind || got.Role != want.Role || got.Path != want.Path ||
			got.SHA256 != want.SHA256 || got.MediaType != want.MediaType ||
			got.SizeBytes <= 0 || got.Validation == "" {
			return errors.New("fresh evaluation media differs from its source request")
		}
	}
	return nil
}

func matchEvaluation(
	ctx context.Context, prepared review.PreparedRequest, descriptor review.ProviderDescriptor,
	evaluation review.EvaluationBundle,
) error {
	record := evaluation.Record
	if record.AttemptID != prepared.AttemptID || record.Suite != prepared.Suite ||
		record.Case != prepared.Case || record.Trial != prepared.Trial ||
		record.Provider != descriptor || record.RequestFingerprint != prepared.RequestFingerprint ||
		len(record.Media) != len(prepared.Media) {
		return errors.New("evaluation identity differs from its source request")
	}
	for index := range record.Media {
		if !reflect.DeepEqual(record.Media[index], prepared.Media[index].Media) {
			return errors.New("evaluation media differs from its source request")
		}
	}
	contextSHA, err := review.CanonicalContextSHA256(ctx, prepared.Context)
	if err != nil || contextSHA != record.ContextSHA256 {
		return errors.New("evaluation context differs from its source request")
	}
	return nil
}

func prepareOptions(options Options) (Options, error) {
	if options.Reviewer == nil || options.Reviewer.Descriptor().Provider == "" {
		return Options{}, errors.New("candidate review campaign requires an open provider lease")
	}
	if options.Concurrency == 0 {
		options.Concurrency = 1
	}
	if options.Concurrency < 1 || options.Concurrency > maximumConcurrency {
		return Options{}, fmt.Errorf("candidate review concurrency must be 1..%d", maximumConcurrency)
	}
	if err := validateSensitiveValues(options.SensitiveValues); err != nil {
		return Options{}, err
	}
	for _, item := range []struct{ label, path string }{
		{"source directory", options.SourceDirectory},
		{"source receipt", options.SourceReceiptPath},
		{"evaluation directory", options.EvaluationDirectory},
		{"receipt directory", options.ReceiptDirectory},
		{"quarantine directory", options.QuarantineDirectory},
	} {
		if err := validateAbsolutePath(item.label, item.path); err != nil {
			return Options{}, err
		}
		if containsSensitive(item.path, options.SensitiveValues) {
			return Options{}, errors.New("candidate review public path contains a declared sensitive value")
		}
	}
	paths := []string{
		options.SourceDirectory, options.SourceReceiptPath, options.EvaluationDirectory,
		options.ReceiptDirectory, options.QuarantineDirectory,
	}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if pathsOverlap(paths[left], paths[right]) {
				return Options{}, errors.New("candidate review source, evaluation, receipt, and quarantine paths must be disjoint")
			}
		}
	}
	options.SensitiveValues = slices.Clone(options.SensitiveValues)
	return options, nil
}

func validateSensitiveValues(values []string) error {
	if len(values) > 256 {
		return errors.New("candidate review sensitive-value count is oversized")
	}
	totalSensitive := 0
	seenSensitive := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) < 8 || len(value) > 4096 || strings.TrimSpace(value) != value ||
			!utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return errors.New("candidate review sensitive value is short, oversized, or noncanonical")
		}
		if _, duplicate := seenSensitive[value]; duplicate {
			continue
		}
		if totalSensitive > 64<<10-len(value) {
			return errors.New("candidate review sensitive-value bytes are oversized")
		}
		totalSensitive += len(value)
		seenSensitive[value] = struct{}{}
	}
	return nil
}

func ensureCampaignDirectories(options Options) error {
	for _, directory := range []string{
		options.EvaluationDirectory, options.ReceiptDirectory, options.QuarantineDirectory,
	} {
		if err := ensureDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func validateAbsolutePath(label, path string) error {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path || path == filepath.Dir(path) {
		return errors.New("candidate review " + label + " must be a clean absolute non-root path")
	}
	return nil
}

func ensureDirectory(path string) error {
	parent := filepath.Dir(path)
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("candidate review directory parent is invalid")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	before, err := os.Lstat(parent)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return errors.New("candidate review directory parent is invalid")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return errors.New("open candidate review directory parent")
	}
	defer root.Close()
	opened, openErr := root.Stat(".")
	visible, visibleErr := os.Lstat(parent)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, visible) {
		return errors.New("candidate review directory parent changed while opening")
	}
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		if err := root.Mkdir(name, 0o700); err != nil {
			return errors.New("create candidate review directory exclusively")
		}
		parentDirectory, syncErr := root.Open(".")
		if syncErr != nil {
			return errors.New("open candidate review directory parent for sync")
		}
		syncErr = parentDirectory.Sync()
		closeErr := parentDirectory.Close()
		if syncErr != nil || closeErr != nil {
			return errors.New("sync candidate review directory parent")
		}
		info, err = root.Lstat(name)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("candidate review directory is invalid or overly permissive")
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return errors.New("open candidate review directory")
	}
	defer directory.Close()
	anchored, anchoredErr := directory.Stat(".")
	visible, visibleErr = os.Lstat(path)
	if anchoredErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(info, anchored) || !os.SameFile(anchored, visible) {
		return errors.New("candidate review directory changed while anchoring")
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	separator := string(filepath.Separator)
	return left == right || strings.HasPrefix(left, right+separator) ||
		strings.HasPrefix(right, left+separator)
}

func containsSensitive(value string, sensitive []string) bool {
	payload := []byte(value)
	for _, secret := range sensitive {
		if secret == "" {
			continue
		}
		for _, candidate := range [][]byte{
			[]byte(secret), []byte(base64.StdEncoding.EncodeToString([]byte(secret))),
			[]byte(base64.RawStdEncoding.EncodeToString([]byte(secret))),
			[]byte(base64.URLEncoding.EncodeToString([]byte(secret))),
			[]byte(base64.RawURLEncoding.EncodeToString([]byte(secret))),
		} {
			if bytes.Contains(payload, candidate) {
				return true
			}
		}
	}
	return false
}

func digestName(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cloneResult(source Result) Result {
	result := source
	result.Evaluations = make([]CaseEvaluation, len(source.Evaluations))
	for index, evaluation := range source.Evaluations {
		result.Evaluations[index] = evaluation
		result.Evaluations[index].Deterministic = cloneOutcome(evaluation.Deterministic)
		result.Evaluations[index].Assessment.SignificantProblems = slices.Clone(
			evaluation.Assessment.SignificantProblems,
		)
		result.Evaluations[index].Assessment.MinorObservations = slices.Clone(
			evaluation.Assessment.MinorObservations,
		)
		result.Evaluations[index].Assessment.Limitations = slices.Clone(
			evaluation.Assessment.Limitations,
		)
	}
	return result
}

func cloneOutcome(source bench.TaskOutcome) bench.TaskOutcome {
	result := source
	result.Metrics = make(map[string]float64, len(source.Metrics))
	for name, value := range source.Metrics {
		result.Metrics[name] = value
	}
	result.Notes = make(map[string]string, len(source.Notes))
	for name, value := range source.Notes {
		result.Notes[name] = value
	}
	if source.Execution != nil {
		copy := source.Execution.Clone()
		result.Execution = &copy
	}
	return result
}
