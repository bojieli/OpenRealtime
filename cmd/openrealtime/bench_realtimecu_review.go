package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/realtimecu"
	review "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	reviewffmpeg "github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
)

type realtimeCUFFmpegReviewFactory struct {
	options reviewffmpeg.Options
}

func (factory realtimeCUFFmpegReviewFactory) NewReviewVideo(
	ctx context.Context, _ realtimecu.EvidenceAttempt,
) (reviewmedia.Encoder, reviewmedia.Attestor, error) {
	encoder, err := reviewffmpeg.NewEncoder(ctx, factory.options)
	if err != nil {
		return nil, nil, err
	}
	attestor, err := reviewffmpeg.NewAttestor(ctx, factory.options)
	if err != nil {
		_ = encoder.Close()
		return nil, nil, err
	}
	return encoder, attestor, nil
}

type realtimeCUReviewCLIConfig struct {
	Directory                     string
	Resume                        bool
	Provider                      string
	APIKeyEnvironment             string
	ReceiptPath                   string
	SourceReceiptPath             string
	EvaluationReceiptDirectory    string
	EvaluationQuarantineDirectory string
	Concurrency                   int
	FFmpegPath                    string
	FFprobePath                   string
	BubblewrapPath                string
	newBundle                     func(realtimecu.ReviewBundleOptions) (*realtimecu.ReviewBundle, error)
}

type realtimeCUReviewCLIResources struct {
	bundle                        *realtimecu.ReviewBundle
	resumedResult                 *bench.Result
	lease                         *review.ProviderLease
	reviewer                      review.ProviderDescriptor
	encoder                       reviewmedia.EncoderDescriptor
	attestor                      reviewmedia.AttestorDescriptor
	receiptPath                   string
	sourceReceiptPath             string
	evaluationReceiptDirectory    string
	evaluationQuarantineDirectory string
	sensitive                     []string
	evidenceComplete              bool
}

func runRealtimeCUReviewVerification(arguments []string, output io.Writer) error {
	options, err := resolveRealtimeCUReviewVerificationOptions(
		"verify-realtime-cu-review", arguments, output,
	)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	verified, err := realtimecu.VerifyReviewBundlePublication(ctx, options)
	if err != nil {
		return err
	}
	return reportRealtimeCUReviewVerification(output, options, verified)
}

func runRealtimeCUReviewReceiptPublication(arguments []string, output io.Writer) error {
	options, err := resolveRealtimeCUReviewVerificationOptions(
		"anchor-realtime-cu-review", arguments, output,
	)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	verified, err := realtimecu.PublishReviewBundleReceipt(ctx, options)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "create-only Realtime-CU final review receipt anchored at %s\n",
		options.ReceiptPath)
	return reportRealtimeCUReviewVerification(output, options, verified)
}

func resolveRealtimeCUReviewVerificationOptions(
	command string, arguments []string, output io.Writer,
) (realtimecu.ReviewBundleVerificationOptions, error) {
	flags := flag.NewFlagSet("openrealtime bench "+command, flag.ContinueOnError)
	var options realtimecu.ReviewBundleVerificationOptions
	flags.StringVar(&options.Directory, "review-dir", "", "sealed Realtime-CU review directory")
	flags.StringVar(&options.ReceiptPath, "review-receipt", "", "external final Realtime-CU review receipt")
	flags.StringVar(&options.SourceReceiptPath, "review-source-receipt", "", "external deterministic-source receipt")
	flags.StringVar(&options.EvaluationReceiptDirectory, "review-evaluation-receipts", "", "external per-case evaluation receipt directory")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return realtimecu.ReviewBundleVerificationOptions{}, err
	}
	if flags.NArg() != 0 || strings.TrimSpace(options.Directory) == "" {
		return realtimecu.ReviewBundleVerificationOptions{},
			fmt.Errorf("%s accepts flags only and requires -review-dir", command)
	}
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return realtimecu.ReviewBundleVerificationOptions{},
			errors.New("resolve Realtime-CU review verification directory")
	}
	options.Directory = directory
	if options.ReceiptPath == "" {
		options.ReceiptPath = directory + ".receipt.json"
	}
	if options.SourceReceiptPath == "" {
		options.SourceReceiptPath = directory + ".source-receipt.json"
	}
	if options.EvaluationReceiptDirectory == "" {
		options.EvaluationReceiptDirectory = directory + ".evaluation-receipts"
	}
	for destination, source := range map[*string]string{
		&options.ReceiptPath:                options.ReceiptPath,
		&options.SourceReceiptPath:          options.SourceReceiptPath,
		&options.EvaluationReceiptDirectory: options.EvaluationReceiptDirectory,
	} {
		resolved, resolveErr := filepath.Abs(source)
		if resolveErr != nil {
			return realtimecu.ReviewBundleVerificationOptions{},
				errors.New("resolve Realtime-CU review verification path")
		}
		*destination = resolved
	}
	return options, nil
}

func reportRealtimeCUReviewVerification(
	output io.Writer, options realtimecu.ReviewBundleVerificationOptions,
	verified realtimecu.ReviewBundleVerification,
) error {
	fmt.Fprintf(output, "verified Realtime-CU review integrity: %d/%d cases, %d external evaluations\n",
		len(verified.Manifest.Attempts), verified.Manifest.Expected,
		len(verified.EvaluationReceipts))
	fmt.Fprintf(output, "review bundle   : %s\n", options.Directory)
	fmt.Fprintf(output, "review receipt  : %s (%s)\n",
		options.ReceiptPath, verified.Receipt.ManifestSHA256)
	fmt.Fprintf(output, "source receipt  : %s (%s)\n",
		options.SourceReceiptPath, verified.SourceReceipt.ReceiptSHA256)
	if !verified.EvidenceComplete {
		fmt.Fprintln(output, "DIAGNOSTIC POPULATION INCOMPLETE: integrity verified; exact16 reportable source and evaluation evidence is required")
		return nil
	}
	wantReviewer := gemini.Descriptor()
	for _, attempt := range verified.Manifest.Attempts {
		if attempt.Reviewer == nil || *attempt.Reviewer != wantReviewer {
			fmt.Fprintln(output, "POPULATION EVIDENCE INCOMPLETE: verified evidence does not use the exact google.gemini-3.7-flash reviewer for every case")
			return errors.New("verified Realtime-CU review has a nonconforming reviewer identity")
		}
	}
	fmt.Fprintln(output, "population-complete Realtime-CU review evidence verified (exact16 synchronized A/V + external receipts + Gemini 3.7 Flash); behavioral acceptance not evaluated")
	return nil
}

func openRealtimeCUReviewCLI(
	ctx context.Context, config realtimeCUReviewCLIConfig, deploymentToken string,
	lookupEnv func(string) (string, bool),
) (*realtimeCUReviewCLIResources, error) {
	if strings.TrimSpace(config.Directory) == "" {
		if config.Resume || config.ReceiptPath != "" || config.SourceReceiptPath != "" || config.EvaluationReceiptDirectory != "" ||
			config.EvaluationQuarantineDirectory != "" || (config.Concurrency != 0 && config.Concurrency != 4) ||
			config.FFmpegPath != "" || config.FFprobePath != "" || config.BubblewrapPath != "" ||
			(config.Provider != "" && config.Provider != gemini.RegistrationName) {
			return nil, errors.New("Realtime-CU review options require -review-dir")
		}
		return nil, nil
	}
	if ctx == nil {
		return nil, errors.New("open Realtime-CU review CLI: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if config.Provider != gemini.RegistrationName || gemini.ModelID != "gemini-3.7-flash" ||
		gemini.Descriptor().Model != gemini.ModelID {
		return nil, errors.New("Realtime-CU review provider must be the exact google.gemini-3.7-flash registration")
	}
	if strings.TrimSpace(config.APIKeyEnvironment) == "" || lookupEnv == nil {
		return nil, errors.New("Realtime-CU review key environment is invalid")
	}
	if config.Concurrency < 0 || config.Concurrency > 16 {
		return nil, errors.New("Realtime-CU review concurrency must be 1..16 or zero")
	}
	directory, err := filepath.Abs(config.Directory)
	if err != nil {
		return nil, errors.New("resolve Realtime-CU review directory")
	}
	if config.SourceReceiptPath == "" {
		config.SourceReceiptPath = directory + ".source-receipt.json"
	}
	if config.ReceiptPath == "" {
		config.ReceiptPath = directory + ".receipt.json"
	}
	if config.EvaluationReceiptDirectory == "" {
		config.EvaluationReceiptDirectory = directory + ".evaluation-receipts"
	}
	if config.EvaluationQuarantineDirectory == "" {
		config.EvaluationQuarantineDirectory = directory + ".evaluation-quarantine"
	}
	sourceReceiptPath, err := filepath.Abs(config.SourceReceiptPath)
	if err != nil {
		return nil, errors.New("resolve Realtime-CU source receipt path")
	}
	receiptPath, err := filepath.Abs(config.ReceiptPath)
	if err != nil {
		return nil, errors.New("resolve Realtime-CU final review receipt path")
	}
	evaluationReceiptDirectory, err := filepath.Abs(config.EvaluationReceiptDirectory)
	if err != nil {
		return nil, errors.New("resolve Realtime-CU evaluation receipt directory")
	}
	evaluationQuarantineDirectory, err := filepath.Abs(config.EvaluationQuarantineDirectory)
	if err != nil {
		return nil, errors.New("resolve Realtime-CU evaluation quarantine directory")
	}
	paths := []string{
		directory, receiptPath, sourceReceiptPath, evaluationReceiptDirectory,
		evaluationQuarantineDirectory,
	}
	for _, path := range paths {
		if filepath.Clean(path) != path || path == filepath.Dir(path) {
			return nil, errors.New("Realtime-CU review artifact paths must be clean, absolute, and non-root")
		}
	}
	separator := string(filepath.Separator)
	overlap := func(left, right string) bool {
		return left == right || strings.HasPrefix(left, right+separator) ||
			strings.HasPrefix(right, left+separator)
	}
	for index, left := range paths {
		for _, right := range paths[index+1:] {
			if overlap(left, right) {
				return nil, errors.New("Realtime-CU review bundle, receipt, and quarantine paths overlap")
			}
		}
	}
	for _, path := range paths {
		if err := validateRealtimeCUReviewCLIParent(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	if config.Resume {
		for path, wantDirectory := range map[string]bool{
			directory: true, sourceReceiptPath: false,
			evaluationReceiptDirectory: true, evaluationQuarantineDirectory: true,
		} {
			info, statErr := os.Lstat(path)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDirectory {
				return nil, fmt.Errorf("Realtime-CU review resume path is missing or invalid: %s", path)
			}
		}
		if _, statErr := os.Lstat(receiptPath); statErr == nil {
			return nil, fmt.Errorf("Realtime-CU review create-only path already exists: %s", receiptPath)
		} else if !os.IsNotExist(statErr) {
			return nil, errors.New("inspect Realtime-CU review create-only path")
		}
	} else {
		for _, path := range paths {
			if _, statErr := os.Lstat(path); statErr == nil {
				return nil, fmt.Errorf("Realtime-CU review create-only path already exists: %s", path)
			} else if !os.IsNotExist(statErr) {
				return nil, errors.New("inspect Realtime-CU review create-only path")
			}
		}
	}

	apiKey, present := lookupEnv(config.APIKeyEnvironment)
	if !present || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("Realtime-CU Gemini review key environment is unset")
	}
	registration := gemini.Registration(func(context.Context) (string, error) { return apiKey, nil })
	registry, err := review.NewRegistry([]review.Registration{registration})
	if err != nil {
		return nil, errors.New("construct Realtime-CU review provider registry")
	}
	lease, err := registry.Open(ctx, gemini.RegistrationName)
	if err != nil {
		return nil, errors.New("open exact Gemini 3.7 Flash Realtime-CU reviewer")
	}
	cleanupLease := true
	defer func() {
		if cleanupLease {
			_ = lease.Close()
		}
	}()
	sensitive := []string{apiKey}
	if deploymentToken != "" && (len(deploymentToken) < 8 || strings.TrimSpace(deploymentToken) != deploymentToken) {
		return nil, errors.New("Realtime-CU deployment credential is too short or invalid for leakage protection")
	}
	if deploymentToken != "" {
		sensitive = append(sensitive, deploymentToken)
	}
	for _, environment := range []string{
		realtimeCULocalModelKeyEnvironment, realtimeCULocalASRKeyEnvironment,
	} {
		if value, found := lookupEnv(environment); found && strings.TrimSpace(value) != "" {
			sensitive = append(sensitive, value)
		}
	}
	sensitive, err = canonicalRealtimeCUReviewSecrets(sensitive)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if realtimeCUReviewPathContainsSecret(path, sensitive) {
			return nil, errors.New("Realtime-CU review artifact path contains a sensitive value")
		}
	}
	ffmpegOptions := reviewffmpeg.Options{
		FFmpegPath: config.FFmpegPath, FFprobePath: config.FFprobePath,
		BubblewrapPath: config.BubblewrapPath, SensitiveValues: sensitive,
	}
	var encoderDescriptor reviewmedia.EncoderDescriptor
	var attestorDescriptor reviewmedia.AttestorDescriptor
	if !config.Resume {
		encoder, encoderErr := reviewffmpeg.NewEncoder(ctx, ffmpegOptions)
		if encoderErr != nil {
			return nil, errors.New("preflight Realtime-CU review FFmpeg encoder")
		}
		attestor, attestorErr := reviewffmpeg.NewAttestor(ctx, ffmpegOptions)
		if attestorErr != nil {
			_ = encoder.Close()
			return nil, errors.New("preflight Realtime-CU review FFmpeg full-decode attestor")
		}
		if encoder.Descriptor().Name == "" ||
			attestor.Descriptor().Capability != reviewmedia.FullDecodeAttestationCapability {
			_ = encoder.Close()
			_ = attestor.Close()
			return nil, errors.New("Realtime-CU review FFmpeg toolchain lacks full-decode identity")
		}
		encoderDescriptor = encoder.Descriptor()
		attestorDescriptor = attestor.Descriptor()
		if err := errors.Join(encoder.Close(), attestor.Close()); err != nil {
			return nil, errors.New("close Realtime-CU review FFmpeg preflight plug-ins")
		}
	}

	anchor := realtimecu.FileReviewSourceReceiptAnchor{Path: sourceReceiptPath}
	stores := realtimecu.FileReviewEvaluationReceiptStoreFactory{
		Directory: evaluationReceiptDirectory, QuarantineRoot: evaluationQuarantineDirectory,
	}
	var externalDirectories *realtimeCUExternalReviewDirectoryTransaction
	if !config.Resume {
		externalDirectories, err = beginRealtimeCUExternalReviewDirectories(
			evaluationReceiptDirectory, evaluationQuarantineDirectory,
		)
		if err != nil {
			return nil, err
		}
	}
	var bundle *realtimecu.ReviewBundle
	var resumedResult *bench.Result
	if config.Resume {
		sourceReceipt, readErr := realtimecu.ReadReviewSourceReceipt(ctx, sourceReceiptPath)
		if readErr != nil {
			return nil, errors.New("read Realtime-CU deterministic source receipt for resume")
		}
		bundle, err = realtimecu.ResumeReviewBundle(ctx, realtimecu.ReviewBundleResumeOptions{
			Directory: directory, SourceReceipt: sourceReceipt, SourceAnchor: anchor,
			Reviewer: lease, EvaluationStores: stores, ReviewConcurrency: config.Concurrency,
			SensitiveValues: sensitive,
		})
		if err == nil {
			recovered, resultErr := bundle.SourceResult()
			if resultErr != nil {
				_ = bundle.Close()
				return nil, errors.New("recover Realtime-CU deterministic result for review resume")
			}
			resumedResult = &recovered
		}
	} else {
		newBundle := realtimecu.NewReviewBundle
		if config.newBundle != nil {
			newBundle = config.newBundle
		}
		bundle, err = newBundle(realtimecu.ReviewBundleOptions{
			Directory: directory, VideoFactory: realtimeCUFFmpegReviewFactory{options: ffmpegOptions},
			Reviewer: lease, SourceAnchor: anchor, EvaluationStores: stores,
			ReviewConcurrency: config.Concurrency, SensitiveValues: sensitive,
		})
	}
	if err != nil {
		var cleanupErr error
		if externalDirectories != nil {
			cleanupErr = externalDirectories.Rollback()
		}
		return nil, errors.Join(
			errors.New("create Realtime-CU review evidence plug-in"), err, cleanupErr,
		)
	}
	if externalDirectories != nil {
		if err := externalDirectories.Commit(); err != nil {
			return nil, errors.Join(
				errors.New("commit Realtime-CU external review directories"), err, bundle.Close(),
			)
		}
	}
	cleanupLease = false
	return &realtimeCUReviewCLIResources{
		bundle: bundle, resumedResult: resumedResult, lease: lease, reviewer: lease.Descriptor(),
		encoder: encoderDescriptor, attestor: attestorDescriptor,
		sensitive:                     slices.Clone(sensitive),
		receiptPath:                   receiptPath,
		sourceReceiptPath:             sourceReceiptPath,
		evaluationReceiptDirectory:    evaluationReceiptDirectory,
		evaluationQuarantineDirectory: evaluationQuarantineDirectory,
	}, nil
}

func canonicalRealtimeCUReviewSecrets(source []string) ([]string, error) {
	result := make([]string, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for _, secret := range source {
		if len(secret) < 8 || strings.TrimSpace(secret) != secret ||
			strings.ContainsAny(secret, "\x00\r\n") {
			return nil, errors.New("Realtime-CU review sensitive values must be canonical and at least eight bytes")
		}
		if _, duplicate := seen[secret]; duplicate {
			continue
		}
		seen[secret] = struct{}{}
		result = append(result, secret)
	}
	sort.Slice(result, func(left, right int) bool {
		if len(result[left]) != len(result[right]) {
			return len(result[left]) > len(result[right])
		}
		return result[left] < result[right]
	})
	return result, nil
}

func realtimeCUReviewPathContainsSecret(path string, secrets []string) bool {
	for _, secret := range secrets {
		encoded := []string{
			secret,
			url.PathEscape(secret), url.QueryEscape(secret),
			base64.StdEncoding.EncodeToString([]byte(secret)),
			base64.RawStdEncoding.EncodeToString([]byte(secret)),
			base64.URLEncoding.EncodeToString([]byte(secret)),
			base64.RawURLEncoding.EncodeToString([]byte(secret)),
			hex.EncodeToString([]byte(secret)),
		}
		for index, candidate := range encoded {
			if candidate == "" {
				continue
			}
			if index == 0 {
				if strings.Contains(path, candidate) {
					return true
				}
				continue
			}
			if strings.Contains(strings.ToLower(path), strings.ToLower(candidate)) {
				return true
			}
		}
	}
	return false
}

type realtimeCUExternalReviewDirectory struct {
	parent         *os.Root
	parentPath     string
	name           string
	parentIdentity os.FileInfo
	entryIdentity  os.FileInfo
}

type realtimeCUExternalReviewDirectoryTransaction struct {
	entries []realtimeCUExternalReviewDirectory
	done    bool
}

func createRealtimeCUExternalReviewDirectories(paths ...string) error {
	transaction, err := beginRealtimeCUExternalReviewDirectories(paths...)
	if err != nil {
		return err
	}
	return transaction.Commit()
}

func beginRealtimeCUExternalReviewDirectories(
	paths ...string,
) (transaction *realtimeCUExternalReviewDirectoryTransaction, resultErr error) {
	transaction = &realtimeCUExternalReviewDirectoryTransaction{
		entries: make([]realtimeCUExternalReviewDirectory, 0, len(paths)),
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, transaction.Rollback())
			transaction = nil
		}
	}()
	for _, path := range paths {
		parentPath, name := filepath.Dir(path), filepath.Base(path)
		before, err := os.Lstat(parentPath)
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			return nil, errors.New("Realtime-CU external review parent is invalid")
		}
		parent, err := os.OpenRoot(parentPath)
		if err != nil {
			return nil, errors.New("open Realtime-CU external review parent")
		}
		opened, openErr := parent.Stat(".")
		visible, visibleErr := os.Lstat(parentPath)
		if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
			!visible.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, visible) {
			_ = parent.Close()
			return nil, errors.New("Realtime-CU external review parent changed while opening")
		}
		if _, err := parent.Lstat(name); err == nil || !os.IsNotExist(err) {
			_ = parent.Close()
			return nil, errors.New("Realtime-CU external review path already exists")
		}
		if err := parent.Mkdir(name, 0o700); err != nil {
			_ = parent.Close()
			return nil, errors.New("create Realtime-CU external review directory exclusively")
		}
		created, createdErr := parent.Lstat(name)
		if createdErr != nil || created.Mode()&os.ModeSymlink != 0 || !created.IsDir() {
			_ = parent.RemoveAll(name)
			_ = parent.Close()
			return nil, errors.New("inspect created Realtime-CU external review directory")
		}
		anchored := realtimeCUExternalReviewDirectory{
			parent: parent, parentPath: parentPath, name: name,
			parentIdentity: opened, entryIdentity: created,
		}
		transaction.entries = append(transaction.entries, anchored)
		parentFile, err := parent.Open(".")
		if err != nil {
			return nil, errors.New("open Realtime-CU external review parent for sync")
		}
		syncErr := parentFile.Sync()
		closeErr := parentFile.Close()
		entry, entryErr := parent.Lstat(name)
		visibleEntry, visibleEntryErr := os.Lstat(path)
		visibleParent, visibleParentErr := os.Lstat(parentPath)
		if syncErr != nil || closeErr != nil || entryErr != nil || visibleEntryErr != nil ||
			visibleParentErr != nil || entry.Mode()&os.ModeSymlink != 0 || !entry.IsDir() ||
			visibleParent.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, visibleParent) ||
			!os.SameFile(entry, visibleEntry) {
			return nil, errors.New("Realtime-CU external review directory changed during creation")
		}
		transaction.entries[len(transaction.entries)-1].entryIdentity = entry
	}
	return transaction, nil
}

func (transaction *realtimeCUExternalReviewDirectoryTransaction) Commit() (resultErr error) {
	if transaction == nil || transaction.done {
		return errors.New("Realtime-CU external review directory transaction is unavailable")
	}
	transaction.done = true
	for index := range transaction.entries {
		entry := &transaction.entries[index]
		if entry.parent != nil {
			if err := entry.parent.Close(); err != nil {
				resultErr = errors.Join(resultErr,
					errors.New("close committed Realtime-CU external review parent"))
			}
			entry.parent = nil
		}
	}
	return resultErr
}

func (transaction *realtimeCUExternalReviewDirectoryTransaction) Rollback() (resultErr error) {
	if transaction == nil || transaction.done {
		return nil
	}
	transaction.done = true
	for index := len(transaction.entries) - 1; index >= 0; index-- {
		entry := &transaction.entries[index]
		if entry.parent == nil {
			continue
		}
		visibleParent, parentErr := os.Lstat(entry.parentPath)
		current, entryErr := entry.parent.Lstat(entry.name)
		if parentErr != nil || entryErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(entry.parentIdentity, visibleParent) || entry.entryIdentity == nil ||
			current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
			!os.SameFile(entry.entryIdentity, current) {
			resultErr = errors.Join(resultErr,
				errors.New("refuse to remove changed Realtime-CU external review directory"))
		} else if err := entry.parent.RemoveAll(entry.name); err != nil {
			resultErr = errors.Join(resultErr,
				errors.New("remove unused Realtime-CU external review directory"))
		} else if err := syncRealtimeCUExternalReviewParent(entry.parent); err != nil {
			resultErr = errors.Join(resultErr,
				errors.New("sync removed Realtime-CU external review directory"))
		}
		if err := entry.parent.Close(); err != nil {
			resultErr = errors.Join(resultErr,
				errors.New("close Realtime-CU external review parent"))
		}
		entry.parent = nil
	}
	return resultErr
}

func syncRealtimeCUExternalReviewParent(parent *os.Root) error {
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func validateRealtimeCUReviewCLIParent(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("Realtime-CU review artifact parent is missing, symlinked, or not a directory")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func (resources *realtimeCUReviewCLIResources) retain(ctx context.Context, output io.Writer) error {
	if resources == nil || resources.bundle == nil {
		return nil
	}
	var resultErr error
	sourceReceipt, present := resources.bundle.SourceReceipt()
	if !present {
		resultErr = errors.Join(resultErr, errors.New("Realtime-CU deterministic source receipt is unavailable"))
	} else {
		external, readErr := realtimecu.ReadReviewSourceReceipt(ctx, resources.sourceReceiptPath)
		if readErr != nil || external != sourceReceipt {
			resultErr = errors.Join(resultErr, errors.New("verify durable external Realtime-CU source receipt"))
		} else if _, verifyErr := realtimecu.VerifyReviewSourceReceipt(sourceReceipt.Directory, external); verifyErr != nil {
			resultErr = errors.Join(resultErr, errors.New("reverify durable Realtime-CU source evidence"))
		} else {
			fmt.Fprintf(output, "Realtime-CU source receipt retained at %s\n", resources.sourceReceiptPath)
		}
	}
	evaluationReceipts := resources.bundle.EvaluationReceipts()
	for caseID, receipt := range evaluationReceipts {
		path := filepath.Join(resources.evaluationReceiptDirectory,
			filepath.Base(receipt.Directory)+".receipt.json")
		external, err := review.ReadEvaluationBundleReceipt(ctx, path)
		if err != nil || external != receipt {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("verify durable Realtime-CU evaluation receipt for %s", caseID))
			continue
		}
		opened, err := review.VerifyEvaluationBundle(ctx, review.EvaluationBundleOptions{
			Directory: external.Directory, SensitiveValues: slices.Clone(resources.sensitive),
		}, external)
		if err != nil || opened.Receipt != external {
			resultErr = errors.Join(resultErr,
				fmt.Errorf("reverify sealed Realtime-CU evaluation bundle for %s", caseID))
			continue
		}
		fmt.Fprintf(output, "Realtime-CU evaluation receipt retained at %s\n", path)
	}
	if resultErr != nil {
		return resultErr
	}
	if receipt, ok := resources.bundle.Receipt(); ok {
		manifest, verifyErr := realtimecu.VerifyReviewBundleReceipt(receipt.Directory, receipt)
		if verifyErr != nil {
			resultErr = errors.Join(resultErr, errors.New("reverify sealed Realtime-CU review bundle"))
		} else {
			verified, publicationErr := realtimecu.PublishReviewBundleReceipt(
				ctx, realtimecu.ReviewBundleVerificationOptions{
					Directory: receipt.Directory, ReceiptPath: resources.receiptPath,
					SourceReceiptPath:          resources.sourceReceiptPath,
					EvaluationReceiptDirectory: resources.evaluationReceiptDirectory,
				},
			)
			if publicationErr != nil {
				resultErr = errors.Join(resultErr,
					fmt.Errorf("durably publish final Realtime-CU review receipt: %w", publicationErr))
			} else {
				if verified.Receipt != receipt {
					return errors.Join(resultErr,
						errors.New("published Realtime-CU final receipt differs from the run receipt"))
				}
				resources.evidenceComplete = verified.EvidenceComplete &&
					manifest.Complete && manifest.Reportable && len(manifest.Attempts) == 16 &&
					len(evaluationReceipts) == 16
				fmt.Fprintf(output, "Realtime-CU review bundle sealed at %s (%s)\n",
					receipt.Directory, receipt.ManifestSHA256)
				fmt.Fprintf(output, "Realtime-CU final review receipt retained at %s\n",
					resources.receiptPath)
			}
		}
	} else {
		fmt.Fprintln(output,
			"Realtime-CU review bundle is diagnostic/incomplete; sealed source receipt remains authoritative")
	}
	if len(evaluationReceipts) != 16 {
		resources.evidenceComplete = false
	}
	return resultErr
}

func (resources *realtimeCUReviewCLIResources) close() error {
	if resources == nil {
		return nil
	}
	var resultErr error
	if resources.bundle != nil {
		resultErr = errors.Join(resultErr, resources.bundle.Close())
		resources.bundle = nil
	}
	if resources.lease != nil {
		resultErr = errors.Join(resultErr, resources.lease.Close())
		resources.lease = nil
	}
	return resultErr
}

func finishRealtimeCUReviewResume(
	ctx context.Context, resources *realtimeCUReviewCLIResources,
) (bench.Result, error) {
	if resources == nil || resources.bundle == nil || resources.resumedResult == nil {
		return bench.Result{}, errors.New("Realtime-CU review resume resources are incomplete")
	}
	result := *resources.resumedResult
	retentionContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Minute)
	err := resources.bundle.FinishSuite(retentionContext, result)
	cancel()
	return result, err
}
