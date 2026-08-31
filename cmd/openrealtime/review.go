package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	benchreview "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	scenarioEvaluationIndexFormat        = "openrealtime.scenario-evaluation-index"
	scenarioEvaluationIndexFormatVersion = 2
	scenarioEvaluationReceiptFormat      = "openrealtime.scenario-evaluation-receipt"
	scenarioEvaluationReceiptVersion     = 1
	maximumScenarioEvaluationIndex       = 16 << 20
	maximumScenarioEvaluationReview      = 8 << 20
	maximumScenarioEvaluationWorkers     = 16
)

const reviewUsage = `usage: openrealtime review <scenario|verify-scenario> [flags]

The scenario reviewer consumes an externally anchored graph-native source
bundle. It never starts a Realtime session and never changes deterministic
pass/fail results. verify-scenario reopens the source, every evaluation, and
both levels of external receipt without credentials or provider work.`

type scenarioEvaluationPortableReceipt struct {
	ManifestSHA256 string `json:"manifest_sha256"`
	RecordSHA256   string `json:"record_sha256"`
	FileSetSHA256  string `json:"file_set_sha256"`
	ReceiptSHA256  string `json:"receipt_sha256"`
}

type scenarioEvaluationEntry struct {
	Ordinal               int                               `json:"ordinal"`
	AttemptID             string                            `json:"attempt_id"`
	Case                  string                            `json:"case"`
	Trial                 int                               `json:"trial"`
	DeterministicBehavior string                            `json:"deterministic_behavior"`
	Bundle                string                            `json:"bundle"`
	ReceiptFile           string                            `json:"receipt_file"`
	Receipt               scenarioEvaluationPortableReceipt `json:"receipt"`
	Provider              benchreview.ProviderDescriptor    `json:"provider"`
	Assessment            benchreview.Assessment            `json:"assessment"`
	EvidenceScope         string                            `json:"evidence_scope"`
	IndependentRemote     bool                              `json:"independent_remote_attestation"`
	AuthenticityCaveat    string                            `json:"authenticity_caveat"`
}

type scenarioEvaluationIndex struct {
	Format                 string                    `json:"format"`
	FormatVersion          int                       `json:"format_version"`
	Complete               bool                      `json:"complete"`
	Suite                  string                    `json:"suite"`
	SourceReceiptSHA256    string                    `json:"source_receipt_sha256"`
	SourceManifestSHA256   string                    `json:"source_manifest_sha256"`
	SourceFileSetSHA256    string                    `json:"source_file_set_sha256"`
	ChecklistFingerprint   string                    `json:"checklist_fingerprint"`
	Planned                int                       `json:"planned_attempts"`
	Retained               int                       `json:"retained_attempts"`
	Expected               int                       `json:"expected_evaluations"`
	ReviewCoverageComplete bool                      `json:"review_coverage_complete"`
	SourceComplete         bool                      `json:"source_population_complete"`
	ChecklistReportable    bool                      `json:"deterministic_checklist_reportable"`
	ArchitectureReportable bool                      `json:"architecture_result_reportable"`
	EvaluationSetSHA256    string                    `json:"evaluation_set_sha256"`
	ReviewSHA256           string                    `json:"review_sha256"`
	Entries                []scenarioEvaluationEntry `json:"entries"`
}

type scenarioEvaluationIndexReceipt struct {
	Format              string `json:"format"`
	FormatVersion       int    `json:"format_version"`
	Directory           string `json:"directory"`
	ManifestSHA256      string `json:"manifest_sha256"`
	SourceReceiptSHA256 string `json:"source_receipt_sha256"`
	EvaluationSetSHA256 string `json:"evaluation_set_sha256"`
	ReceiptSHA256       string `json:"receipt_sha256"`
}

type scenarioEvaluationRunOptions struct {
	SourceDirectory string
	SourceReceipt   string
	OutputDirectory string
	OutputReceipt   string
	Provider        string
	Parallel        int
	Timeout         time.Duration
	Progress        func(scenarioEvaluationEntry)
}

func runReview(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(reviewUsage)
	}
	switch arguments[0] {
	case "scenario":
		registry, err := benchreview.NewRegistry([]benchreview.Registration{
			gemini.Registration(gemini.EnvironmentAPIKey),
		})
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(
			context.Background(), os.Interrupt, syscall.SIGTERM,
		)
		defer cancel()
		return runScenarioEvaluationContext(ctx, arguments[1:], output, registry)
	case "verify-scenario":
		return runScenarioEvaluationVerification(arguments[1:], output)
	case "help", "-h", "--help":
		fmt.Fprintln(output, reviewUsage)
		return nil
	default:
		return fmt.Errorf("unknown review command %q\n%s", arguments[0], reviewUsage)
	}
}

func runScenarioEvaluation(
	arguments []string, output io.Writer, registry *benchreview.Registry,
) (returnErr error) {
	return runScenarioEvaluationContext(context.Background(), arguments, output, registry)
}

func runScenarioEvaluationContext(
	ctx context.Context,
	arguments []string,
	output io.Writer,
	registry *benchreview.Registry,
) (returnErr error) {
	if ctx == nil {
		return errors.New("scenario review: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := flag.NewFlagSet("review scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	options := scenarioEvaluationRunOptions{}
	flags.StringVar(&options.SourceDirectory, "source-dir", "", "sealed graph-native scenario source directory")
	flags.StringVar(&options.SourceReceipt, "source-receipt", "", "external scenario source receipt")
	flags.StringVar(&options.OutputDirectory, "out", "", "new directory for case-by-case secondary evaluation bundles")
	flags.StringVar(&options.OutputReceipt, "out-receipt", "", "external aggregate evaluation receipt (default: <out>.receipt.json)")
	flags.StringVar(&options.Provider, "provider", gemini.RegistrationName, "registered offline review provider")
	flags.IntVar(&options.Parallel, "parallel", 4, "maximum concurrent offline reviews")
	flags.DurationVar(&options.Timeout, "timeout", 12*time.Minute, "bound for each provider review and retention")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("review scenario accepts flags only")
	}
	resolved, err := resolveScenarioEvaluationOptions(options)
	if err != nil {
		return err
	}
	if registry == nil {
		return errors.New("scenario review provider registry is nil")
	}
	sourceReceipt, err := graphnative.ReadSourceReceipt(resolved.SourceReceipt)
	if err != nil {
		return fmt.Errorf("read scenario source receipt: %w", err)
	}
	sourceOptions := graphnative.SourceBundleOptions{Directory: resolved.SourceDirectory}
	population, err := graphnative.BuildSourceReviewPopulation(
		ctx, sourceOptions, sourceReceipt,
	)
	if err != nil {
		return fmt.Errorf("verify scenario source before provider selection: %w", err)
	}
	source, requests, reviewAttempts := population.Bundle, population.Requests, population.Attempts
	if len(requests) == 0 {
		return errors.New(
			"scenario source has no media-complete attempts for exact review; " +
				"the sealed source receipt remains the recovery boundary",
		)
	}
	if len(requests) != len(reviewAttempts) {
		return errors.New("scenario source review request population is incomplete")
	}
	var progressMu sync.Mutex
	completed := 0
	resolved.Progress = func(entry scenarioEvaluationEntry) {
		progressMu.Lock()
		defer progressMu.Unlock()
		completed++
		fmt.Fprintf(
			output, "  reviewed     %d/%d  %s  trial %d\n",
			completed, len(requests), entry.Case, entry.Trial,
		)
	}
	// Provider credentials remain provider-owned. In particular, the Gemini
	// plug-in scans its prepared prompt, context, media, and final encoded body
	// with the active credential before transport. Copying that credential into
	// the generic request guard duplicates the policy and can synthesize a false
	// match by concatenating otherwise unrelated JSON tokens.
	root, rootInfo, err := createScenarioEvaluationRoot(resolved.OutputDirectory)
	if err != nil {
		return err
	}
	rootClosed := false
	defer func() {
		if !rootClosed {
			if closeErr := root.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, errors.New("close scenario evaluation directory"))
			}
		}
	}()
	lease, err := registry.Open(ctx, resolved.Provider)
	if err != nil {
		return err
	}
	leaseClosed := false
	defer func() {
		if !leaseClosed {
			if closeErr := lease.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	entries, err := evaluateScenarioRequests(
		ctx, resolved, lease, requests, reviewAttempts,
	)
	if err != nil {
		return err
	}
	if err := lease.Close(); err != nil {
		return fmt.Errorf("close scenario evaluation provider before publication: %w", err)
	}
	leaseClosed = true
	if _, err := graphnative.VerifySourceBundle(
		ctx, sourceOptions, sourceReceipt,
	); err != nil {
		return errors.New("scenario source changed during secondary review")
	}
	index, manifestPayload, reviewPayload, err := buildScenarioEvaluationIndex(
		source, sourceReceipt, reviewAttempts, entries,
	)
	if err != nil {
		return err
	}
	if err := verifyScenarioEvaluationRootIdentity(resolved.OutputDirectory, root, rootInfo); err != nil {
		return err
	}
	if err := writeScenarioEvaluationFile(root, "REVIEW.md", reviewPayload); err != nil {
		return err
	}
	if err := writeScenarioEvaluationFile(root, "manifest.json", manifestPayload); err != nil {
		return err
	}
	if err := syncScenarioEvaluationRoot(root); err != nil {
		return err
	}
	expectedReceipt, err := buildScenarioEvaluationIndexReceipt(
		resolved.OutputDirectory, sourceReceipt, index, manifestPayload,
	)
	if err != nil {
		return err
	}
	if err := verifyScenarioEvaluationCollection(
		ctx, resolved, sourceReceipt, expectedReceipt,
	); err != nil {
		return err
	}
	if err := writeScenarioEvaluationIndexReceipt(
		ctx, resolved.OutputReceipt, expectedReceipt,
	); err != nil {
		return err
	}
	if err := verifyScenarioEvaluationCollection(
		ctx, resolved, sourceReceipt, expectedReceipt,
	); err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return errors.New("close published scenario evaluation directory")
	}
	rootClosed = true
	fmt.Fprintf(output, "  source       %s\n", sourceReceipt.ReceiptSHA256)
	fmt.Fprintf(output, "  provider     %s\n", resolved.Provider)
	fmt.Fprintf(output, "  evaluations  %d/%d retained attempts (%d planned)\n",
		len(entries), len(source.Manifest.Attempts), source.Checklist.Expected)
	fmt.Fprintf(output, "  review       %s\n", resolved.OutputDirectory)
	fmt.Fprintf(output, "  receipt      %s\n", resolved.OutputReceipt)
	fmt.Fprintf(output, "  index        %s\n", expectedReceipt.ManifestSHA256)
	if !index.ReviewCoverageComplete {
		return fmt.Errorf(
			"scenario advisory review covered %d of %d retained attempts; "+
				"result-only attempts remain explicitly unreviewable without fabricated media",
			index.Expected, index.Retained,
		)
	}
	return nil
}

func runScenarioEvaluationVerification(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("review verify-scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	options := scenarioEvaluationRunOptions{}
	flags.StringVar(&options.SourceDirectory, "source-dir", "", "sealed graph-native scenario source directory")
	flags.StringVar(&options.SourceReceipt, "source-receipt", "", "external scenario source receipt")
	flags.StringVar(&options.OutputDirectory, "evaluation-dir", "", "committed scenario evaluation directory")
	flags.StringVar(&options.OutputReceipt, "evaluation-receipt", "", "external aggregate evaluation receipt")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("review verify-scenario accepts flags only")
	}
	var err error
	if options.SourceDirectory, err = resolveScenarioEvaluationPath(
		"source directory", options.SourceDirectory,
	); err != nil {
		return err
	}
	if options.SourceReceipt, err = resolveScenarioEvaluationPath(
		"source receipt", options.SourceReceipt,
	); err != nil {
		return err
	}
	if options.OutputDirectory, err = resolveScenarioEvaluationPath(
		"evaluation directory", options.OutputDirectory,
	); err != nil {
		return err
	}
	if options.OutputReceipt, err = resolveScenarioEvaluationPath(
		"evaluation receipt", options.OutputReceipt,
	); err != nil {
		return err
	}
	if options.SourceDirectory == options.OutputDirectory ||
		pathBelow(options.OutputDirectory, options.SourceDirectory) ||
		pathBelow(options.OutputReceipt, options.SourceDirectory) ||
		options.OutputReceipt == options.OutputDirectory ||
		pathBelow(options.OutputReceipt, options.OutputDirectory) {
		return errors.New("scenario evaluation verification paths overlap")
	}
	for _, path := range []string{
		options.SourceDirectory, options.SourceReceipt,
		options.OutputDirectory, options.OutputReceipt,
	} {
		if err := validateScenarioEvaluationAncestors(filepath.Dir(path)); err != nil {
			return err
		}
	}
	sourceReceipt, err := graphnative.ReadSourceReceipt(options.SourceReceipt)
	if err != nil {
		return fmt.Errorf("read scenario source receipt: %w", err)
	}
	evaluationReceipt, err := readScenarioEvaluationIndexReceipt(
		context.Background(), options.OutputReceipt,
	)
	if err != nil {
		return err
	}
	if err := verifyScenarioEvaluationCollection(
		context.Background(), options, sourceReceipt, evaluationReceipt,
	); err != nil {
		return err
	}
	root, rootInfo, err := openScenarioEvaluationRoot(options.OutputDirectory)
	if err != nil {
		return err
	}
	manifestPayload, readErr := readScenarioEvaluationFile(
		context.Background(), root, "manifest.json", maximumScenarioEvaluationIndex,
	)
	index, decodeErr := decodeScenarioEvaluationIndex(manifestPayload)
	identityErr := verifyScenarioEvaluationRootIdentity(options.OutputDirectory, root, rootInfo)
	closeErr := root.Close()
	if readErr != nil || decodeErr != nil || identityErr != nil || closeErr != nil ||
		scenarioEvaluationDigest(manifestPayload) != evaluationReceipt.ManifestSHA256 {
		return errors.New("reopen verified scenario evaluation summary")
	}
	fmt.Fprintf(output, "  source       %s\n", sourceReceipt.ReceiptSHA256)
	fmt.Fprintf(output, "  evaluations  %d/%d verified\n", len(index.Entries), index.Expected)
	fmt.Fprintf(output, "  review       %s\n", options.OutputDirectory)
	fmt.Fprintf(output, "  receipt      %s\n", evaluationReceipt.ReceiptSHA256)
	return nil
}

func resolveScenarioEvaluationOptions(
	source scenarioEvaluationRunOptions,
) (scenarioEvaluationRunOptions, error) {
	var err error
	if source.SourceDirectory, err = resolveScenarioEvaluationPath(
		"source directory", source.SourceDirectory,
	); err != nil {
		return scenarioEvaluationRunOptions{}, err
	}
	if source.SourceReceipt, err = resolveScenarioEvaluationPath(
		"source receipt", source.SourceReceipt,
	); err != nil {
		return scenarioEvaluationRunOptions{}, err
	}
	if source.OutputDirectory, err = resolveScenarioEvaluationPath(
		"output directory", source.OutputDirectory,
	); err != nil {
		return scenarioEvaluationRunOptions{}, err
	}
	if strings.TrimSpace(source.OutputReceipt) == "" {
		source.OutputReceipt = source.OutputDirectory + ".receipt.json"
	}
	if source.OutputReceipt, err = resolveScenarioEvaluationPath(
		"output receipt", source.OutputReceipt,
	); err != nil {
		return scenarioEvaluationRunOptions{}, err
	}
	if source.Parallel <= 0 || source.Parallel > maximumScenarioEvaluationWorkers {
		return scenarioEvaluationRunOptions{}, fmt.Errorf(
			"scenario evaluation parallelism must be 1..%d", maximumScenarioEvaluationWorkers)
	}
	if source.Timeout <= 0 || source.Timeout > time.Hour {
		return scenarioEvaluationRunOptions{}, errors.New(
			"scenario evaluation timeout must be greater than zero and at most one hour")
	}
	source.Provider = strings.TrimSpace(source.Provider)
	if source.Provider == "" || len(source.Provider) > 256 || strings.ContainsAny(source.Provider, "\x00\r\n") {
		return scenarioEvaluationRunOptions{}, errors.New("scenario evaluation provider is invalid")
	}
	if source.SourceDirectory == source.OutputDirectory ||
		pathBelow(source.OutputDirectory, source.SourceDirectory) ||
		pathBelow(source.OutputReceipt, source.SourceDirectory) {
		return scenarioEvaluationRunOptions{}, errors.New(
			"scenario evaluation output and receipt must be outside the sealed source directory")
	}
	if source.OutputReceipt == source.OutputDirectory ||
		pathBelow(source.OutputReceipt, source.OutputDirectory) {
		return scenarioEvaluationRunOptions{}, errors.New(
			"scenario evaluation aggregate receipt must be outside the output directory")
	}
	if source.SourceReceipt == source.OutputReceipt || source.SourceReceipt == source.OutputDirectory {
		return scenarioEvaluationRunOptions{}, errors.New("scenario evaluation paths must be distinct")
	}
	if _, err := os.Lstat(source.OutputDirectory); err == nil {
		return scenarioEvaluationRunOptions{}, errors.New("scenario evaluation output already exists")
	} else if !os.IsNotExist(err) {
		return scenarioEvaluationRunOptions{}, errors.New("inspect scenario evaluation output")
	}
	if _, err := os.Lstat(source.OutputReceipt); err == nil {
		return scenarioEvaluationRunOptions{}, errors.New("scenario evaluation receipt already exists")
	} else if !os.IsNotExist(err) {
		return scenarioEvaluationRunOptions{}, errors.New("inspect scenario evaluation receipt")
	}
	for _, parent := range []string{filepath.Dir(source.OutputDirectory), filepath.Dir(source.OutputReceipt)} {
		if err := validateScenarioEvaluationAncestors(parent); err != nil {
			return scenarioEvaluationRunOptions{}, err
		}
	}
	return source, nil
}

func resolveScenarioEvaluationPath(label, path string) (string, error) {
	if strings.TrimSpace(path) == "" || !utf8.ValidString(path) || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("scenario evaluation %s is required", label)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve scenario evaluation %s", label)
	}
	absolute = filepath.Clean(absolute)
	if absolute == filepath.Dir(absolute) {
		return "", fmt.Errorf("scenario evaluation %s must not be a filesystem root", label)
	}
	return absolute, nil
}

func pathBelow(path, parent string) bool {
	return path != parent && strings.HasPrefix(path, parent+string(filepath.Separator))
}

func validateScenarioEvaluationAncestors(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("scenario evaluation path %q is not an exact directory", current)
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func createScenarioEvaluationRoot(directory string) (*os.Root, os.FileInfo, error) {
	parentPath, name := filepath.Dir(directory), filepath.Base(directory)
	if err := validateScenarioEvaluationAncestors(parentPath); err != nil || !validScenarioEvaluationName(name) {
		return nil, nil, errors.New("scenario evaluation output path is invalid")
	}
	parentBefore, err := os.Lstat(parentPath)
	if err != nil || parentBefore.Mode()&os.ModeSymlink != 0 || !parentBefore.IsDir() {
		return nil, nil, errors.New("scenario evaluation output parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, nil, errors.New("open scenario evaluation output parent")
	}
	defer parent.Close()
	openedParent, openErr := parent.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentBefore, openedParent) ||
		!os.SameFile(openedParent, visibleParent) {
		return nil, nil, errors.New("scenario evaluation output parent changed while opening")
	}
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, nil, errors.New("create scenario evaluation output exclusively")
	}
	created := true
	defer func() {
		if created {
			_ = parent.Remove(name)
			_ = syncScenarioEvaluationRoot(parent)
		}
	}()
	if err := syncScenarioEvaluationRoot(parent); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, errors.New("open created scenario evaluation output")
	}
	rootInfo, rootErr := root.Stat(".")
	entry, entryErr := parent.Lstat(name)
	visible, visibleErr := os.Lstat(directory)
	parentAfter, parentAfterErr := os.Lstat(parentPath)
	if rootErr != nil || entryErr != nil || visibleErr != nil || parentAfterErr != nil ||
		entry.Mode()&os.ModeSymlink != 0 || !entry.IsDir() ||
		!os.SameFile(parentBefore, parentAfter) || !os.SameFile(openedParent, parentAfter) ||
		!os.SameFile(rootInfo, entry) || !os.SameFile(entry, visible) {
		_ = root.Close()
		return nil, nil, errors.New("created scenario evaluation output changed while opening")
	}
	created = false
	return root, rootInfo, nil
}

func evaluateScenarioRequests(
	ctx context.Context,
	options scenarioEvaluationRunOptions,
	lease *benchreview.ProviderLease,
	requests []benchreview.Request,
	attempts []graphnative.SourceAttempt,
) ([]scenarioEvaluationEntry, error) {
	if ctx == nil || lease == nil || len(requests) == 0 || len(requests) != len(attempts) {
		return nil, errors.New("scenario evaluation request population is invalid")
	}
	workerCount := min(options.Parallel, len(requests))
	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan int)
	entries := make([]scenarioEvaluationEntry, len(requests))
	var workers sync.WaitGroup
	var failureOnce sync.Once
	var firstFailure error
	fail := func(err error) {
		failureOnce.Do(func() {
			firstFailure = err
			cancel(err)
		})
	}
	worker := func() {
		defer workers.Done()
		for {
			select {
			case <-runContext.Done():
				return
			case index, open := <-jobs:
				if !open {
					return
				}
				entry, err := evaluateScenarioRequest(
					runContext, options, lease, index, requests[index], attempts[index],
				)
				if err != nil {
					fail(fmt.Errorf("scenario evaluation %s: %w", attempts[index].Record.Key.TaskID, err))
					return
				}
				entries[index] = entry
				if options.Progress != nil {
					options.Progress(entry)
				}
			}
		}
	}
	workers.Add(workerCount)
	for range workerCount {
		go worker()
	}
	populate := true
	for index := range requests {
		select {
		case jobs <- index:
		case <-runContext.Done():
			populate = false
		}
		if !populate {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if firstFailure != nil {
		return nil, firstFailure
	}
	if cause := context.Cause(runContext); cause != nil {
		return nil, cause
	}
	for index := range entries {
		if entries[index].Ordinal != index+1 {
			return nil, errors.New("scenario evaluation worker population is incomplete")
		}
	}
	return entries, nil
}

func evaluateScenarioRequest(
	parent context.Context,
	options scenarioEvaluationRunOptions,
	lease *benchreview.ProviderLease,
	index int,
	request benchreview.Request,
	attempt graphnative.SourceAttempt,
) (scenarioEvaluationEntry, error) {
	if request.AttemptID != attempt.Record.Fingerprint || request.Case != attempt.Record.Key.CaseName ||
		request.Trial != attempt.Record.Key.Trial || request.Suite != graphnative.SuiteName {
		return scenarioEvaluationEntry{}, errors.New("review request differs from the sealed attempt")
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	evaluation, err := benchreview.Evaluate(ctx, lease, request)
	if err != nil {
		return scenarioEvaluationEntry{}, err
	}
	if err := validateScenarioEvaluationTimeline(request, evaluation.Record.Assessment); err != nil {
		return scenarioEvaluationEntry{}, err
	}
	bundleName, receiptName := scenarioEvaluationNames(index, attempt)
	bundleDirectory := filepath.Join(options.OutputDirectory, bundleName)
	receipt, err := benchreview.WriteEvaluationBundle(ctx, benchreview.EvaluationBundleOptions{
		Directory: bundleDirectory, SensitiveValues: request.SensitiveValues,
	}, evaluation)
	if err != nil {
		return scenarioEvaluationEntry{}, err
	}
	receiptPath := filepath.Join(options.OutputDirectory, receiptName)
	if err := benchreview.WriteEvaluationBundleReceipt(ctx, receiptPath, receipt); err != nil {
		return scenarioEvaluationEntry{}, err
	}
	retainedReceipt, err := benchreview.ReadEvaluationBundleReceipt(ctx, receiptPath)
	if err != nil || !sameScenarioEvaluationPortableReceipt(retainedReceipt, receipt) {
		return scenarioEvaluationEntry{}, errors.New("reopen scenario evaluation receipt")
	}
	opened, err := benchreview.VerifyEvaluationBundle(ctx, benchreview.EvaluationBundleOptions{
		Directory: bundleDirectory, SensitiveValues: request.SensitiveValues,
	}, retainedReceipt)
	if err != nil {
		return scenarioEvaluationEntry{}, err
	}
	if err := verifyScenarioEvaluationRecord(ctx, opened.Record, request, attempt); err != nil {
		return scenarioEvaluationEntry{}, err
	}
	return scenarioEvaluationEntry{
		Ordinal: index + 1, AttemptID: request.AttemptID, Case: request.Case, Trial: request.Trial,
		DeterministicBehavior: attempt.Record.Behavior, Bundle: bundleName, ReceiptFile: receiptName,
		Receipt: scenarioEvaluationPortableReceipt{
			ManifestSHA256: retainedReceipt.ManifestSHA256, RecordSHA256: retainedReceipt.RecordSHA256,
			FileSetSHA256: retainedReceipt.FileSetSHA256, ReceiptSHA256: retainedReceipt.ReceiptSHA256,
		},
		Provider: opened.Record.Provider, Assessment: opened.Record.Assessment,
		EvidenceScope:      opened.Manifest.EvidenceScope,
		IndependentRemote:  opened.Manifest.IndependentRemoteAttestation,
		AuthenticityCaveat: opened.Manifest.AuthenticityCaveat,
	}, nil
}

func scenarioEvaluationNames(
	index int, attempt graphnative.SourceAttempt,
) (string, string) {
	prefix := fmt.Sprintf(
		"%04d-case-%02d-trial-%03d", index+1,
		attempt.Record.Key.CaseOrdinal, attempt.Record.Key.Trial,
	)
	return prefix + ".evaluation", prefix + ".receipt.json"
}

func buildScenarioEvaluationIndex(
	source graphnative.SourceBundle,
	sourceReceipt graphnative.SourceReceipt,
	attempts []graphnative.SourceAttempt,
	entries []scenarioEvaluationEntry,
) (scenarioEvaluationIndex, []byte, []byte, error) {
	if len(entries) == 0 || len(entries) != len(attempts) {
		return scenarioEvaluationIndex{}, nil, nil, errors.New(
			"scenario evaluation entries do not cover the sealed source population")
	}
	for index := range entries {
		if err := validateScenarioEvaluationEntry(entries[index], index, attempts[index]); err != nil {
			return scenarioEvaluationIndex{}, nil, nil, err
		}
	}
	evaluationSetPayload, err := marshalScenarioEvaluationCompact(entries, maximumScenarioEvaluationIndex)
	if err != nil {
		return scenarioEvaluationIndex{}, nil, nil, err
	}
	index := scenarioEvaluationIndex{
		Format: scenarioEvaluationIndexFormat, FormatVersion: scenarioEvaluationIndexFormatVersion,
		Complete: true, Suite: graphnative.SuiteName,
		SourceReceiptSHA256:    sourceReceipt.ReceiptSHA256,
		SourceManifestSHA256:   sourceReceipt.ManifestSHA256,
		SourceFileSetSHA256:    sourceReceipt.FileSetSHA256,
		ChecklistFingerprint:   source.Checklist.Fingerprint,
		Planned:                source.Checklist.Expected,
		Retained:               len(source.Manifest.Attempts),
		Expected:               len(attempts),
		ReviewCoverageComplete: len(attempts) == len(source.Manifest.Attempts),
		SourceComplete:         source.Checklist.Complete,
		ChecklistReportable:    source.Checklist.Reportable,
		ArchitectureReportable: source.Manifest.ArchitectureReportable,
		EvaluationSetSHA256:    scenarioEvaluationDigest(evaluationSetPayload),
		Entries:                entries,
	}
	reviewPayload, err := renderScenarioEvaluationReview(index)
	if err != nil {
		return scenarioEvaluationIndex{}, nil, nil, err
	}
	index.ReviewSHA256 = scenarioEvaluationDigest(reviewPayload)
	manifestPayload, err := marshalScenarioEvaluationIndented(index, maximumScenarioEvaluationIndex)
	if err != nil {
		return scenarioEvaluationIndex{}, nil, nil, err
	}
	return index, manifestPayload, reviewPayload, nil
}

func validateScenarioEvaluationEntry(
	entry scenarioEvaluationEntry, index int, attempt graphnative.SourceAttempt,
) error {
	bundle, receipt := scenarioEvaluationNames(index, attempt)
	if entry.Ordinal != index+1 || entry.AttemptID != attempt.Record.Fingerprint ||
		entry.Case != attempt.Record.Key.CaseName || entry.Trial != attempt.Record.Key.Trial ||
		entry.DeterministicBehavior != attempt.Record.Behavior ||
		entry.Bundle != bundle || entry.ReceiptFile != receipt ||
		!validScenarioEvaluationDigest(entry.Receipt.ManifestSHA256) ||
		!validScenarioEvaluationDigest(entry.Receipt.RecordSHA256) ||
		!validScenarioEvaluationDigest(entry.Receipt.FileSetSHA256) ||
		!validScenarioEvaluationDigest(entry.Receipt.ReceiptSHA256) ||
		entry.Provider.Validate() != nil || entry.EvidenceScope == "" ||
		entry.IndependentRemote || entry.AuthenticityCaveat == "" {
		return fmt.Errorf("scenario evaluation entry %d is invalid", index+1)
	}
	return nil
}

func renderScenarioEvaluationReview(index scenarioEvaluationIndex) ([]byte, error) {
	if !index.Complete || len(index.Entries) != index.Expected || index.Expected == 0 {
		return nil, errors.New("cannot render incomplete scenario evaluation review")
	}
	var output strings.Builder
	output.WriteString("# Graph-native scenario secondary review\n\n")
	output.WriteString("Deterministic checklist and scorer results remain authoritative. Model reviews are advisory and retained per attempt for human inspection.\n\n")
	fmt.Fprintf(&output, "- Source receipt: `%s`\n", index.SourceReceiptSHA256)
	fmt.Fprintf(&output, "- Checklist: `%s`\n", index.ChecklistFingerprint)
	fmt.Fprintf(&output, "- Source population: %d/%d retained; complete: %t\n",
		index.Retained, index.Planned, index.SourceComplete)
	fmt.Fprintf(&output, "- Deterministic checklist reportable: %t\n", index.ChecklistReportable)
	fmt.Fprintf(&output, "- Architecture result reportable: %t\n", index.ArchitectureReportable)
	fmt.Fprintf(&output, "- Media-complete review inputs: %d/%d retained attempts; complete: %t\n",
		index.Expected, index.Retained, index.ReviewCoverageComplete)
	fmt.Fprintf(&output, "- Advisory evaluations: %d/%d media-complete attempts\n",
		len(index.Entries), index.Expected)
	lastCase := ""
	for _, entry := range index.Entries {
		if entry.Case != lastCase {
			fmt.Fprintf(&output, "\n## Case — %s\n\n", escapeScenarioEvaluationMarkdown(entry.Case))
			lastCase = entry.Case
		}
		fmt.Fprintf(&output, "### Trial %d\n\n", entry.Trial)
		fmt.Fprintf(&output, "- Deterministic behavior: **%s**\n", entry.DeterministicBehavior)
		fmt.Fprintf(&output, "- Model-observed outcome: **%s**\n", entry.Assessment.ObservedOutcome)
		fmt.Fprintf(&output, "- Agrees with deterministic result: %t\n", entry.Assessment.AgreesWithDeterministic)
		fmt.Fprintf(&output, "- Media usable: %t\n", entry.Assessment.MediaUsable)
		fmt.Fprintf(&output, "- Confidence: %.6g\n", entry.Assessment.Confidence)
		fmt.Fprintf(&output, "- Provider: %s / %s\n", escapeScenarioEvaluationMarkdown(entry.Provider.Provider), escapeScenarioEvaluationMarkdown(entry.Provider.Model))
		fmt.Fprintf(&output, "- Evidence: [evaluation](%s/REVIEW.md) · [receipt](%s)\n\n", entry.Bundle, entry.ReceiptFile)
		output.WriteString(escapeScenarioEvaluationMarkdown(entry.Assessment.Summary))
		output.WriteString("\n\nSignificant problems:\n\n")
		writeScenarioEvaluationFindings(&output, entry.Assessment.SignificantProblems)
		if len(entry.Assessment.Limitations) > 0 {
			output.WriteString("\nLimitations:\n\n")
			for _, limitation := range entry.Assessment.Limitations {
				output.WriteString("- ")
				output.WriteString(escapeScenarioEvaluationMarkdown(limitation))
				output.WriteByte('\n')
			}
		}
	}
	output.WriteString("\n## Provenance boundary\n\n")
	output.WriteString("Each per-attempt bundle proves a provider-verified in-process exchange and local digest retention. It does not independently attest that the retained response originated at a remote service.\n")
	if output.Len() == 0 || output.Len() > maximumScenarioEvaluationReview {
		return nil, errors.New("scenario evaluation human review is empty or oversized")
	}
	return []byte(output.String()), nil
}

func writeScenarioEvaluationFindings(output *strings.Builder, findings []benchreview.Finding) {
	if len(findings) == 0 {
		output.WriteString("- None reported.\n")
		return
	}
	for _, finding := range findings {
		output.WriteString("- **")
		output.WriteString(escapeScenarioEvaluationMarkdown(finding.Category))
		output.WriteString("**: ")
		output.WriteString(escapeScenarioEvaluationMarkdown(finding.Evidence))
		output.WriteString(" — ")
		output.WriteString(escapeScenarioEvaluationMarkdown(finding.Impact))
		if finding.StartMS != nil || finding.EndMS != nil {
			output.WriteString(" (ms ")
			if finding.StartMS != nil {
				fmt.Fprintf(output, "%d", *finding.StartMS)
			} else {
				output.WriteByte('?')
			}
			output.WriteString("–")
			if finding.EndMS != nil {
				fmt.Fprintf(output, "%d", *finding.EndMS)
			} else {
				output.WriteByte('?')
			}
			output.WriteByte(')')
		}
		output.WriteByte('\n')
	}
}

func escapeScenarioEvaluationMarkdown(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_",
		"[", "\\[", "]", "\\]", "<", "\\<", ">", "\\>",
		"\r", " ", "\n", " ",
	)
	return replacer.Replace(value)
}

func validateScenarioEvaluationTimeline(
	request benchreview.Request, assessment benchreview.Assessment,
) error {
	// Evaluate has already admitted and canonicalized this context on the write
	// path; CanonicalContextSHA256 does the same immediately before this helper
	// on the reopen path. Decode only the sealed timing header here instead of
	// rehydrating the large transcript and architecture payload per record.
	var source struct {
		Format          string `json:"format"`
		FormatVersion   int    `json:"format_version"`
		MediaDurationMS int64  `json:"media_duration_ms"`
	}
	if err := json.Unmarshal(request.Context, &source); err != nil {
		return errors.New("scenario evaluation context is invalid")
	}
	if source.Format != graphnative.SourceReviewContextFormat ||
		source.FormatVersion != graphnative.SourceReviewContextFormatVersion ||
		source.MediaDurationMS <= 0 || source.MediaDurationMS > 24*60*60*1000 {
		return errors.New("scenario evaluation context media duration is invalid")
	}
	if request.FindingTimestampMaximumMS != source.MediaDurationMS {
		return errors.New("scenario evaluation timestamp maximum differs from sealed media duration")
	}
	for _, findings := range [][]benchreview.Finding{
		assessment.SignificantProblems, assessment.MinorObservations,
	} {
		for _, finding := range findings {
			for _, timestamp := range []*int64{finding.StartMS, finding.EndMS} {
				if timestamp != nil && (*timestamp < 0 || *timestamp > source.MediaDurationMS) {
					return errors.New(
						"scenario evaluation finding timestamp exceeds sealed media duration",
					)
				}
			}
		}
	}
	return nil
}

func verifyScenarioEvaluationRecord(
	ctx context.Context,
	record benchreview.Record,
	request benchreview.Request,
	attempt graphnative.SourceAttempt,
) error {
	if err := record.Validate(); err != nil {
		return err
	}
	contextSHA256, err := benchreview.CanonicalContextSHA256(
		ctx, request.Context,
	)
	if err != nil {
		return err
	}
	if record.AttemptID != request.AttemptID || record.Suite != request.Suite ||
		record.Case != request.Case || record.Trial != request.Trial ||
		record.FindingTimestampMaximumMS != request.FindingTimestampMaximumMS ||
		request.AttemptID != attempt.Record.Fingerprint || request.Case != attempt.Record.Key.CaseName ||
		request.Trial != attempt.Record.Key.Trial ||
		record.ContextSHA256 != contextSHA256 {
		return errors.New("retained evaluation record differs from its sealed source request")
	}
	if err := validateScenarioEvaluationTimeline(request, record.Assessment); err != nil {
		return err
	}
	if len(record.Media) != len(request.Media) || len(request.Media) != len(attempt.Submitted)+1 {
		return errors.New("retained evaluation media count differs from its sealed source request")
	}
	for index := range request.Media {
		expected := request.Media[index]
		expected.Validation = benchreview.MediaValidationVersion
		if index == 0 {
			expected.SizeBytes = attempt.Audio.SizeBytes
		} else {
			expected.SizeBytes = attempt.Submitted[index-1].File.SizeBytes
		}
		if record.Media[index] != expected {
			return fmt.Errorf("retained evaluation media %d differs from its sealed source request", index+1)
		}
	}
	return nil
}

func sameScenarioEvaluationPortableReceipt(
	left, right benchreview.EvaluationBundleReceipt,
) bool {
	return left.ManifestSHA256 == right.ManifestSHA256 &&
		left.RecordSHA256 == right.RecordSHA256 &&
		left.FileSetSHA256 == right.FileSetSHA256 &&
		left.ReceiptSHA256 == right.ReceiptSHA256
}

func verifyScenarioEvaluationCollection(
	ctx context.Context,
	options scenarioEvaluationRunOptions,
	sourceReceipt graphnative.SourceReceipt,
	expectedReceipt scenarioEvaluationIndexReceipt,
) (resultErr error) {
	if ctx == nil {
		return errors.New("verify scenario evaluation collection: nil context")
	}
	if err := validateScenarioEvaluationIndexReceipt(expectedReceipt); err != nil {
		return err
	}
	if expectedReceipt.Directory != options.OutputDirectory ||
		expectedReceipt.SourceReceiptSHA256 != sourceReceipt.ReceiptSHA256 {
		return errors.New("scenario evaluation receipt differs from the selected source or output")
	}
	sourceOptions := graphnative.SourceBundleOptions{Directory: options.SourceDirectory}
	population, err := graphnative.BuildSourceReviewPopulation(ctx, sourceOptions, sourceReceipt)
	if err != nil {
		return fmt.Errorf("verify scenario source while reopening evaluations: %w", err)
	}
	source, requests, reviewAttempts := population.Bundle, population.Requests, population.Attempts
	root, rootInfo, err := openScenarioEvaluationRoot(options.OutputDirectory)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close verified scenario evaluation output"))
		}
	}()
	manifestPayload, err := readScenarioEvaluationFile(
		ctx, root, "manifest.json", maximumScenarioEvaluationIndex,
	)
	if err != nil || scenarioEvaluationDigest(manifestPayload) != expectedReceipt.ManifestSHA256 {
		return errors.New("scenario evaluation manifest differs from its external receipt")
	}
	index, err := decodeScenarioEvaluationIndex(manifestPayload)
	if err != nil {
		return err
	}
	if index.SourceReceiptSHA256 != sourceReceipt.ReceiptSHA256 ||
		index.SourceManifestSHA256 != sourceReceipt.ManifestSHA256 ||
		index.SourceFileSetSHA256 != sourceReceipt.FileSetSHA256 ||
		index.ChecklistFingerprint != source.Checklist.Fingerprint ||
		index.EvaluationSetSHA256 != expectedReceipt.EvaluationSetSHA256 ||
		index.Planned != source.Checklist.Expected ||
		index.Retained != len(source.Manifest.Attempts) ||
		index.Expected != len(reviewAttempts) ||
		index.ReviewCoverageComplete != (len(reviewAttempts) == len(source.Manifest.Attempts)) ||
		index.SourceComplete != source.Checklist.Complete ||
		index.ChecklistReportable != source.Checklist.Reportable ||
		index.ArchitectureReportable != source.Manifest.ArchitectureReportable ||
		len(index.Entries) != len(requests) ||
		len(index.Entries) != len(reviewAttempts) {
		return errors.New("scenario evaluation index differs from its sealed source")
	}
	reviewPayload, err := readScenarioEvaluationFile(
		ctx, root, "REVIEW.md", maximumScenarioEvaluationReview,
	)
	if err != nil || scenarioEvaluationDigest(reviewPayload) != index.ReviewSHA256 {
		return errors.New("scenario evaluation human review differs from its index")
	}
	reconstructed := make([]scenarioEvaluationEntry, len(index.Entries))
	expectedNames := map[string]bool{"manifest.json": false, "REVIEW.md": false}
	for attemptIndex, manifestEntry := range index.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		attempt := reviewAttempts[attemptIndex]
		if err := validateScenarioEvaluationEntry(manifestEntry, attemptIndex, attempt); err != nil {
			return err
		}
		bundleName, receiptName := scenarioEvaluationNames(attemptIndex, attempt)
		bundleDirectory := filepath.Join(options.OutputDirectory, bundleName)
		receiptPath := filepath.Join(options.OutputDirectory, receiptName)
		retainedReceipt, err := benchreview.ReadEvaluationBundleReceipt(ctx, receiptPath)
		if err != nil || retainedReceipt.Directory != bundleDirectory ||
			manifestEntry.Receipt != (scenarioEvaluationPortableReceipt{
				ManifestSHA256: retainedReceipt.ManifestSHA256,
				RecordSHA256:   retainedReceipt.RecordSHA256,
				FileSetSHA256:  retainedReceipt.FileSetSHA256,
				ReceiptSHA256:  retainedReceipt.ReceiptSHA256,
			}) {
			return fmt.Errorf("scenario evaluation %d receipt differs from the aggregate index", attemptIndex+1)
		}
		opened, err := benchreview.VerifyEvaluationBundle(ctx, benchreview.EvaluationBundleOptions{
			Directory: bundleDirectory,
		}, retainedReceipt)
		if err != nil {
			return fmt.Errorf("verify scenario evaluation %d: %w", attemptIndex+1, err)
		}
		if err := verifyScenarioEvaluationRecord(
			ctx, opened.Record, requests[attemptIndex], attempt,
		); err != nil {
			return err
		}
		reconstructed[attemptIndex] = scenarioEvaluationEntry{
			Ordinal: attemptIndex + 1, AttemptID: opened.Record.AttemptID,
			Case: opened.Record.Case, Trial: opened.Record.Trial,
			DeterministicBehavior: attempt.Record.Behavior,
			Bundle:                bundleName, ReceiptFile: receiptName, Receipt: manifestEntry.Receipt,
			Provider: opened.Record.Provider, Assessment: opened.Record.Assessment,
			EvidenceScope:      opened.Manifest.EvidenceScope,
			IndependentRemote:  opened.Manifest.IndependentRemoteAttestation,
			AuthenticityCaveat: opened.Manifest.AuthenticityCaveat,
		}
		if !reflect.DeepEqual(reconstructed[attemptIndex], manifestEntry) {
			return fmt.Errorf("scenario evaluation %d metadata differs from its verified bundle", attemptIndex+1)
		}
		expectedNames[bundleName] = true
		expectedNames[receiptName] = false
	}
	rebuilt, rebuiltManifest, rebuiltReview, err := buildScenarioEvaluationIndex(
		source, sourceReceipt, reviewAttempts, reconstructed,
	)
	if err != nil || !reflect.DeepEqual(rebuilt, index) ||
		!bytes.Equal(rebuiltManifest, manifestPayload) || !bytes.Equal(rebuiltReview, reviewPayload) {
		return errors.New("scenario evaluation aggregate artifacts are not reproducible from verified bundles")
	}
	entriesPayload, err := marshalScenarioEvaluationCompact(reconstructed, maximumScenarioEvaluationIndex)
	if err != nil || scenarioEvaluationDigest(entriesPayload) != expectedReceipt.EvaluationSetSHA256 {
		return errors.New("scenario evaluation set digest differs from verified bundles")
	}
	if err := verifyScenarioEvaluationRootEntries(root, expectedNames); err != nil {
		return err
	}
	if err := verifyScenarioEvaluationRootIdentity(
		options.OutputDirectory, root, rootInfo,
	); err != nil {
		return err
	}
	if _, err := graphnative.VerifySourceBundle(ctx, sourceOptions, sourceReceipt); err != nil {
		return errors.New("scenario source changed while reopening secondary evaluations")
	}
	return nil
}

func verifyScenarioEvaluationRootEntries(root *os.Root, expected map[string]bool) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil || len(entries) != len(expected) {
		return errors.New("scenario evaluation output entry count differs from its index")
	}
	names := make([]string, 0, len(entries))
	identities := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		wantDirectory, exists := expected[entry.Name()]
		if !exists || !validScenarioEvaluationName(entry.Name()) {
			return errors.New("scenario evaluation output contains an unexpected entry")
		}
		info, err := root.Lstat(entry.Name())
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDirectory ||
			(!wantDirectory && !info.Mode().IsRegular()) {
			return errors.New("scenario evaluation output entry type differs from its index")
		}
		for _, previous := range identities {
			if os.SameFile(previous, info) {
				return errors.New("scenario evaluation output repeats a filesystem identity")
			}
		}
		identities = append(identities, info)
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	wanted := make([]string, 0, len(expected))
	for name := range expected {
		wanted = append(wanted, name)
	}
	sort.Strings(wanted)
	if !reflect.DeepEqual(names, wanted) {
		return errors.New("scenario evaluation output names differ from its index")
	}
	return nil
}

func buildScenarioEvaluationIndexReceipt(
	directory string,
	source graphnative.SourceReceipt,
	index scenarioEvaluationIndex,
	manifestPayload []byte,
) (scenarioEvaluationIndexReceipt, error) {
	if index.SourceReceiptSHA256 != source.ReceiptSHA256 ||
		index.SourceManifestSHA256 != source.ManifestSHA256 ||
		index.SourceFileSetSHA256 != source.FileSetSHA256 {
		return scenarioEvaluationIndexReceipt{}, errors.New(
			"scenario evaluation index source identity changed before receipt creation")
	}
	receipt := scenarioEvaluationIndexReceipt{
		Format:              scenarioEvaluationReceiptFormat,
		FormatVersion:       scenarioEvaluationReceiptVersion,
		Directory:           directory,
		ManifestSHA256:      scenarioEvaluationDigest(manifestPayload),
		SourceReceiptSHA256: source.ReceiptSHA256,
		EvaluationSetSHA256: index.EvaluationSetSHA256,
	}
	digest, err := scenarioEvaluationIndexReceiptDigest(receipt)
	if err != nil {
		return scenarioEvaluationIndexReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest
	return receipt, nil
}

func scenarioEvaluationIndexReceiptDigest(
	receipt scenarioEvaluationIndexReceipt,
) (string, error) {
	payload, err := marshalScenarioEvaluationCompact(struct {
		Format              string `json:"format"`
		FormatVersion       int    `json:"format_version"`
		ManifestSHA256      string `json:"manifest_sha256"`
		SourceReceiptSHA256 string `json:"source_receipt_sha256"`
		EvaluationSetSHA256 string `json:"evaluation_set_sha256"`
	}{
		receipt.Format, receipt.FormatVersion, receipt.ManifestSHA256,
		receipt.SourceReceiptSHA256, receipt.EvaluationSetSHA256,
	}, maximumScenarioEvaluationIndex)
	if err != nil {
		return "", err
	}
	return scenarioEvaluationDigest(payload), nil
}

func validateScenarioEvaluationIndexReceipt(receipt scenarioEvaluationIndexReceipt) error {
	if receipt.Format != scenarioEvaluationReceiptFormat ||
		receipt.FormatVersion != scenarioEvaluationReceiptVersion ||
		!filepath.IsAbs(receipt.Directory) || filepath.Clean(receipt.Directory) != receipt.Directory ||
		receipt.Directory == filepath.Dir(receipt.Directory) ||
		!validScenarioEvaluationDigest(receipt.ManifestSHA256) ||
		!validScenarioEvaluationDigest(receipt.SourceReceiptSHA256) ||
		!validScenarioEvaluationDigest(receipt.EvaluationSetSHA256) ||
		!validScenarioEvaluationDigest(receipt.ReceiptSHA256) {
		return errors.New("scenario evaluation aggregate receipt is invalid")
	}
	digest, err := scenarioEvaluationIndexReceiptDigest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("scenario evaluation aggregate receipt digest is invalid")
	}
	return nil
}

func writeScenarioEvaluationIndexReceipt(
	ctx context.Context, path string, receipt scenarioEvaluationIndexReceipt,
) (resultErr error) {
	if ctx == nil {
		return errors.New("write scenario evaluation receipt: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateScenarioEvaluationIndexReceipt(receipt); err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == filepath.Dir(path) ||
		path == receipt.Directory || pathBelow(path, receipt.Directory) {
		return errors.New("scenario evaluation aggregate receipt path is invalid")
	}
	if err := validateScenarioEvaluationAncestors(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("scenario evaluation aggregate receipt already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("inspect scenario evaluation aggregate receipt")
	}
	payload, err := marshalScenarioEvaluationIndented(receipt, maximumScenarioEvaluationIndex)
	if err != nil {
		return err
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	root, rootInfo, err := openScenarioEvaluationRoot(parentPath)
	if err != nil {
		return err
	}
	written := false
	defer func() {
		if !written {
			_ = root.Remove(name)
		}
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close scenario evaluation receipt parent"))
		}
	}()
	if err := verifyScenarioEvaluationRootIdentity(parentPath, root, rootInfo); err != nil {
		return err
	}
	if err := writeScenarioEvaluationFile(root, name, payload); err != nil {
		return errors.New("publish scenario evaluation aggregate receipt")
	}
	retained, err := readScenarioEvaluationFile(ctx, root, name, maximumScenarioEvaluationIndex)
	if err != nil || !bytes.Equal(retained, payload) {
		return errors.New("reopen scenario evaluation aggregate receipt")
	}
	decoded, err := decodeScenarioEvaluationIndexReceipt(retained)
	if err != nil || !reflect.DeepEqual(decoded, receipt) {
		return errors.New("verify scenario evaluation aggregate receipt")
	}
	if err := syncScenarioEvaluationRoot(root); err != nil {
		return err
	}
	if err := verifyScenarioEvaluationRootIdentity(parentPath, root, rootInfo); err != nil {
		return err
	}
	written = true
	return nil
}

func decodeScenarioEvaluationIndexReceipt(
	payload []byte,
) (scenarioEvaluationIndexReceipt, error) {
	if len(payload) == 0 || len(payload) > maximumScenarioEvaluationIndex || strictjson.Validate(payload) != nil {
		return scenarioEvaluationIndexReceipt{}, errors.New("scenario evaluation receipt is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var receipt scenarioEvaluationIndexReceipt
	if err := decoder.Decode(&receipt); err != nil || requireScenarioEvaluationEOF(decoder) != nil {
		return scenarioEvaluationIndexReceipt{}, errors.New("decode scenario evaluation aggregate receipt")
	}
	canonical, err := marshalScenarioEvaluationIndented(receipt, maximumScenarioEvaluationIndex)
	if err != nil || !bytes.Equal(canonical, payload) {
		return scenarioEvaluationIndexReceipt{}, errors.New("scenario evaluation aggregate receipt is noncanonical")
	}
	if err := validateScenarioEvaluationIndexReceipt(receipt); err != nil {
		return scenarioEvaluationIndexReceipt{}, err
	}
	return receipt, nil
}

func readScenarioEvaluationIndexReceipt(
	ctx context.Context, path string,
) (scenarioEvaluationIndexReceipt, error) {
	if ctx == nil {
		return scenarioEvaluationIndexReceipt{}, errors.New(
			"read scenario evaluation receipt: nil context")
	}
	if err := ctx.Err(); err != nil {
		return scenarioEvaluationIndexReceipt{}, err
	}
	resolved, err := resolveScenarioEvaluationPath("receipt", path)
	if err != nil || resolved != path {
		return scenarioEvaluationIndexReceipt{}, errors.New(
			"scenario evaluation receipt path must be clean and absolute")
	}
	if err := validateScenarioEvaluationAncestors(filepath.Dir(path)); err != nil {
		return scenarioEvaluationIndexReceipt{}, err
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	root, rootInfo, err := openScenarioEvaluationRoot(parentPath)
	if err != nil {
		return scenarioEvaluationIndexReceipt{}, err
	}
	payload, readErr := readScenarioEvaluationFile(
		ctx, root, name, maximumScenarioEvaluationIndex,
	)
	identityErr := verifyScenarioEvaluationRootIdentity(parentPath, root, rootInfo)
	closeErr := root.Close()
	if readErr != nil {
		return scenarioEvaluationIndexReceipt{}, readErr
	}
	if identityErr != nil || closeErr != nil {
		return scenarioEvaluationIndexReceipt{}, errors.New(
			"scenario evaluation receipt parent changed while reading")
	}
	return decodeScenarioEvaluationIndexReceipt(payload)
}

func decodeScenarioEvaluationIndex(payload []byte) (scenarioEvaluationIndex, error) {
	if len(payload) == 0 || len(payload) > maximumScenarioEvaluationIndex || strictjson.Validate(payload) != nil {
		return scenarioEvaluationIndex{}, errors.New("scenario evaluation index is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var index scenarioEvaluationIndex
	if err := decoder.Decode(&index); err != nil || requireScenarioEvaluationEOF(decoder) != nil {
		return scenarioEvaluationIndex{}, errors.New("decode scenario evaluation index")
	}
	canonical, err := marshalScenarioEvaluationIndented(index, maximumScenarioEvaluationIndex)
	if err != nil || !bytes.Equal(canonical, payload) {
		return scenarioEvaluationIndex{}, errors.New("scenario evaluation index is noncanonical")
	}
	if index.Format != scenarioEvaluationIndexFormat ||
		index.FormatVersion != scenarioEvaluationIndexFormatVersion || !index.Complete ||
		index.Suite != graphnative.SuiteName || index.Expected <= 0 ||
		index.Planned < index.Retained || index.Retained < index.Expected ||
		index.ReviewCoverageComplete != (index.Expected == index.Retained) ||
		index.SourceComplete != (index.Retained == index.Planned) ||
		((index.ChecklistReportable || index.ArchitectureReportable) && !index.SourceComplete) ||
		len(index.Entries) != index.Expected ||
		!validScenarioEvaluationDigest(index.SourceReceiptSHA256) ||
		!validScenarioEvaluationDigest(index.SourceManifestSHA256) ||
		!validScenarioEvaluationDigest(index.SourceFileSetSHA256) ||
		!validScenarioEvaluationDigest(index.ChecklistFingerprint) ||
		!validScenarioEvaluationDigest(index.EvaluationSetSHA256) ||
		!validScenarioEvaluationDigest(index.ReviewSHA256) {
		return scenarioEvaluationIndex{}, errors.New("scenario evaluation index identity is invalid")
	}
	entriesPayload, err := marshalScenarioEvaluationCompact(index.Entries, maximumScenarioEvaluationIndex)
	if err != nil || scenarioEvaluationDigest(entriesPayload) != index.EvaluationSetSHA256 {
		return scenarioEvaluationIndex{}, errors.New("scenario evaluation index entry-set digest is invalid")
	}
	return index, nil
}

func writeScenarioEvaluationFile(root *os.Root, name string, payload []byte) error {
	if root == nil || !validScenarioEvaluationName(name) || len(payload) == 0 ||
		len(payload) > maximumScenarioEvaluationIndex {
		return errors.New("scenario evaluation file request is invalid")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = root.Remove(name)
		}
	}()
	created, err := file.Stat()
	if err != nil || !created.Mode().IsRegular() || created.Mode().Perm()&0o077 != 0 {
		return errors.New("created scenario evaluation artifact is not a private regular file")
	}
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

func readScenarioEvaluationFile(
	ctx context.Context, root *os.Root, name string, maximum int,
) ([]byte, error) {
	if ctx == nil || root == nil || !validScenarioEvaluationName(name) || maximum <= 0 {
		return nil, errors.New("scenario evaluation read request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > int64(maximum) || before.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("scenario evaluation artifact is not an exact private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("open scenario evaluation artifact")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("scenario evaluation artifact changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, before.Size()+1))
	if err != nil || int64(len(payload)) != before.Size() {
		return nil, errors.New("read exact scenario evaluation artifact")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(before, after) || !os.SameFile(opened, after) || after.Size() != before.Size() {
		return nil, errors.New("scenario evaluation artifact changed while reading")
	}
	return payload, nil
}

func openScenarioEvaluationRoot(directory string) (*os.Root, os.FileInfo, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory ||
		directory == filepath.Dir(directory) {
		return nil, nil, errors.New("scenario evaluation directory is invalid")
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return nil, nil, errors.New("scenario evaluation directory is not an exact directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, errors.New("open scenario evaluation directory")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(visible, opened) {
		_ = root.Close()
		return nil, nil, errors.New("scenario evaluation directory changed while opening")
	}
	return root, opened, nil
}

func verifyScenarioEvaluationRootIdentity(
	directory string, root *os.Root, expected os.FileInfo,
) error {
	if root == nil || expected == nil {
		return errors.New("scenario evaluation root identity is missing")
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		!os.SameFile(visible, expected) {
		return errors.New("scenario evaluation directory identity changed")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, expected) {
		return errors.New("scenario evaluation root identity changed")
	}
	return nil
}

func syncScenarioEvaluationRoot(root *os.Root) error {
	if root == nil {
		return errors.New("scenario evaluation directory is nil")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open scenario evaluation directory for sync")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync scenario evaluation directory")
	}
	return nil
}

func validScenarioEvaluationName(name string) bool {
	return name != "" && len(name) <= 4096 && utf8.ValidString(name) &&
		filepath.Base(name) == name && filepath.Clean(name) == name &&
		name != "." && name != ".." && !strings.ContainsAny(name, "\x00\r\n")
}

func marshalScenarioEvaluationIndented(value any, maximum int) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(payload) == 0 || len(payload)+1 > maximum {
		return nil, errors.New("encode scenario evaluation artifact")
	}
	return append(payload, '\n'), nil
}

func marshalScenarioEvaluationCompact(value any, maximum int) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil || len(payload) == 0 || len(payload) > maximum {
		return nil, errors.New("encode scenario evaluation identity")
	}
	return payload, nil
}

func scenarioEvaluationDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validScenarioEvaluationDigest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

func requireScenarioEvaluationEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("scenario evaluation JSON has trailing data")
	}
	return nil
}
