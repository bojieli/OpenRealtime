package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	review "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/campaign"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
)

const candidateReviewDefaultConcurrency = 16

// candidateReviewCLIConfig is the suite-neutral command-line composition of
// the candidate evidence API. Benchmark packages know only candidate.Plugin;
// Gemini and filesystem publication remain an offline CLI concern.
type candidateReviewCLIConfig struct {
	Prefix            string
	Provider          string
	APIKeyEnvironment string
	Concurrency       int
}

func (config *candidateReviewCLIConfig) bind(flags *flag.FlagSet) {
	flags.StringVar(&config.Prefix, "review-prefix", "",
		"fresh artifact prefix for current-run audio, exact Gemini review, and human index")
	flags.StringVar(&config.Provider, "review-provider", gemini.RegistrationName,
		"offline reviewer plug-in (exactly google.gemini-3.7-flash)")
	flags.StringVar(&config.APIKeyEnvironment, "review-key-env", "GEMINI_API_KEY",
		"environment variable holding the Gemini review key")
	flags.IntVar(&config.Concurrency, "review-concurrency", candidateReviewDefaultConcurrency,
		"concurrent offline Gemini reviews (1..16)")
}

type candidateReviewPaths struct {
	Prefix               string
	SourceDirectory      string
	SourceReceipt        string
	EvaluationDirectory  string
	EvaluationReceipts   string
	EvaluationQuarantine string
	AggregateDirectory   string
	AggregateReceipt     string
	AggregateQuarantine  string
}

func (paths candidateReviewPaths) all() []string {
	return []string{
		paths.SourceDirectory, paths.SourceReceipt, paths.EvaluationDirectory,
		paths.EvaluationReceipts, paths.EvaluationQuarantine,
		paths.AggregateDirectory, paths.AggregateReceipt, paths.AggregateQuarantine,
	}
}

func (paths candidateReviewPaths) campaignOptions(
	lease *review.ProviderLease, sensitive []string, concurrency int,
) campaign.Options {
	return campaign.Options{
		SourceDirectory: paths.SourceDirectory, SourceReceiptPath: paths.SourceReceipt,
		EvaluationDirectory: paths.EvaluationDirectory, ReceiptDirectory: paths.EvaluationReceipts,
		QuarantineDirectory: paths.EvaluationQuarantine, Reviewer: lease,
		SensitiveValues: slices.Clone(sensitive), Concurrency: concurrency,
	}
}

func (paths candidateReviewPaths) aggregateOptions(sensitive []string) campaign.AggregateOptions {
	return campaign.AggregateOptions{
		Directory: paths.AggregateDirectory, ReceiptPath: paths.AggregateReceipt,
		SourceReceiptPath: paths.SourceReceipt, QuarantineDirectory: paths.AggregateQuarantine,
		SensitiveValues: slices.Clone(sensitive),
	}
}

type candidateReviewOperations struct {
	preflight func(context.Context, string) error
	run       func(
		context.Context, candidateReviewPaths, string, []string, int,
	) (campaign.AggregateBundle, error)
	verify func(context.Context, candidateReviewPaths) (campaign.AggregateBundle, error)
}

func productionCandidateReviewOperations() candidateReviewOperations {
	return candidateReviewOperations{
		preflight: preflightExactCandidateReviewer,
		run:       runExactCandidateReviewCampaign,
		verify: func(ctx context.Context, paths candidateReviewPaths) (campaign.AggregateBundle, error) {
			return campaign.VerifyAggregate(ctx, paths.aggregateOptions(nil))
		},
	}
}

type candidateReviewCLIResources struct {
	paths       candidateReviewPaths
	bundle      *sourcebundle.Bundle
	origin      candidate.RunOrigin
	apiKey      string
	sensitive   []string
	concurrency int
	operations  candidateReviewOperations
}

func openCandidateReviewCLI(
	ctx context.Context, config candidateReviewCLIConfig, deploymentToken, endpoint string,
	lookupEnv func(string) (string, bool),
) (*candidateReviewCLIResources, error) {
	return openCandidateReviewCLIWithOperations(
		ctx, config, deploymentToken, endpoint, lookupEnv, productionCandidateReviewOperations(),
	)
}

func openCandidateReviewCLIWithOperations(
	ctx context.Context, config candidateReviewCLIConfig, deploymentToken, endpoint string,
	lookupEnv func(string) (string, bool), operations candidateReviewOperations,
) (*candidateReviewCLIResources, error) {
	if strings.TrimSpace(config.Prefix) == "" {
		if config.Prefix != "" ||
			(config.Provider != "" && config.Provider != gemini.RegistrationName) ||
			(config.APIKeyEnvironment != "" && config.APIKeyEnvironment != "GEMINI_API_KEY") ||
			(config.Concurrency != 0 && config.Concurrency != candidateReviewDefaultConcurrency) {
			return nil, errors.New("candidate review options require -review-prefix")
		}
		return nil, nil
	}
	if ctx == nil {
		return nil, errors.New("open candidate review CLI: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Provider != gemini.RegistrationName || gemini.ModelID != "gemini-3.7-flash" ||
		gemini.Descriptor().Model != gemini.ModelID {
		return nil, errors.New("candidate review provider must be exact google.gemini-3.7-flash")
	}
	if config.Concurrency < 1 || config.Concurrency > 16 {
		return nil, errors.New("candidate review concurrency must be 1..16")
	}
	if strings.TrimSpace(config.APIKeyEnvironment) == "" || lookupEnv == nil {
		return nil, errors.New("candidate review key environment is invalid")
	}
	apiKey, present := lookupEnv(config.APIKeyEnvironment)
	if !present || !validCandidateReviewSecret(apiKey) {
		return nil, errors.New("candidate review Gemini key environment is unset or invalid")
	}
	sensitive, err := canonicalCandidateReviewSecrets(apiKey, deploymentToken)
	if err != nil {
		return nil, err
	}
	paths, err := resolveCandidateReviewPaths(config.Prefix)
	if err != nil {
		return nil, err
	}
	if candidateReviewPathsContainSensitive(paths, sensitive) {
		return nil, errors.New("candidate review artifact path contains a sensitive value")
	}
	if err := requireFreshCandidateReviewPaths(paths); err != nil {
		return nil, err
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginProduction, bench.TransportWebSocket, endpoint,
	)
	if err != nil {
		return nil, fmt.Errorf("candidate review run origin: %w", err)
	}
	if operations.preflight == nil || operations.run == nil || operations.verify == nil {
		return nil, errors.New("candidate review operations are incomplete")
	}
	if err := operations.preflight(ctx, apiKey); err != nil {
		return nil, err
	}
	bundle, err := sourcebundle.New(sourcebundle.Options{
		Directory: paths.SourceDirectory, ReceiptPath: paths.SourceReceipt,
		SensitiveValues: sensitive,
	})
	if err != nil {
		return nil, fmt.Errorf("create current-run candidate source bundle: %w", err)
	}
	return &candidateReviewCLIResources{
		paths: paths, bundle: bundle, origin: origin, apiKey: apiKey,
		sensitive: sensitive, concurrency: config.Concurrency, operations: operations,
	}, nil
}

func (resources *candidateReviewCLIResources) finish(
	ctx context.Context, output io.Writer,
) (campaign.AggregateBundle, error) {
	if resources == nil || resources.bundle == nil || resources.operations.run == nil {
		return campaign.AggregateBundle{}, errors.New("candidate review resources are incomplete")
	}
	// A successful lifecycle seal makes Close a no-op. If the runner failed
	// before constructing its lifecycle, Close releases the open directory but
	// intentionally preserves it as diagnostic evidence; campaign verification
	// then refuses to present it as a completed review.
	closeErr := resources.bundle.Close()
	aggregate, reviewErr := resources.operations.run(
		ctx, resources.paths, resources.apiKey, resources.sensitive, resources.concurrency,
	)
	if reviewErr != nil {
		return campaign.AggregateBundle{}, errors.Join(closeErr, reviewErr)
	}
	if closeErr != nil {
		return campaign.AggregateBundle{}, closeErr
	}
	if output != nil {
		fmt.Fprintf(output, "candidate review: %d/%d recordings evaluated by %s (advisory)\n",
			aggregate.Manifest.EvaluationCount, aggregate.Manifest.Expected,
			aggregate.Manifest.Provider.Model)
		fmt.Fprintf(output, "review bundle   : %s\n", resources.paths.AggregateDirectory)
		fmt.Fprintf(output, "review receipt  : %s\n", aggregate.Receipt.ReceiptSHA256)
	}
	return aggregate, nil
}

func finishCandidateReviewIfConfigured(
	ctx context.Context, resources *candidateReviewCLIResources, output io.Writer,
) error {
	if resources == nil {
		return nil
	}
	_, err := resources.finish(ctx, output)
	return err
}

func resolveCandidateReviewPaths(prefix string) (candidateReviewPaths, error) {
	if strings.TrimSpace(prefix) == "" || strings.TrimSpace(prefix) != prefix {
		return candidateReviewPaths{}, errors.New("candidate review prefix is empty or noncanonical")
	}
	absolute, err := filepath.Abs(prefix)
	if err != nil {
		return candidateReviewPaths{}, errors.New("resolve candidate review prefix")
	}
	absolute = filepath.Clean(absolute)
	if absolute == filepath.Dir(absolute) {
		return candidateReviewPaths{}, errors.New("candidate review prefix cannot be a filesystem root")
	}
	paths := candidateReviewPaths{
		Prefix:               absolute,
		SourceDirectory:      absolute + ".source",
		SourceReceipt:        absolute + ".source.receipt.json",
		EvaluationDirectory:  absolute + ".evaluations",
		EvaluationReceipts:   absolute + ".evaluation-receipts",
		EvaluationQuarantine: absolute + ".evaluation-quarantine",
		AggregateDirectory:   absolute + ".aggregate",
		AggregateReceipt:     absolute + ".aggregate.receipt.json",
		AggregateQuarantine:  absolute + ".aggregate-quarantine",
	}
	parent := filepath.Dir(absolute)
	for current := parent; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return candidateReviewPaths{}, errors.New("candidate review prefix parent is invalid")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return paths, nil
}

func requireFreshCandidateReviewPaths(paths candidateReviewPaths) error {
	for _, path := range paths.all() {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("candidate review create-only path already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("inspect candidate review create-only path")
		}
	}
	return nil
}

func canonicalCandidateReviewSecrets(values ...string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if !validCandidateReviewSecret(value) {
			return nil, errors.New("candidate review credential is invalid for leakage protection")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result, nil
}

func validCandidateReviewSecret(value string) bool {
	return len(value) >= 8 && len(value) <= 4096 && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func candidateReviewPathsContainSensitive(paths candidateReviewPaths, sensitive []string) bool {
	for _, path := range paths.all() {
		for _, secret := range sensitive {
			if secret == "" {
				continue
			}
			raw := []byte(secret)
			for _, value := range []string{
				secret,
				base64.StdEncoding.EncodeToString(raw),
				base64.RawStdEncoding.EncodeToString(raw),
				base64.URLEncoding.EncodeToString(raw),
				base64.RawURLEncoding.EncodeToString(raw),
			} {
				if strings.Contains(path, value) {
					return true
				}
			}
		}
	}
	return false
}

func openExactCandidateReviewer(ctx context.Context, apiKey string) (*review.ProviderLease, error) {
	if gemini.ModelID != "gemini-3.7-flash" || gemini.RegistrationName != "google.gemini-3.7-flash" ||
		gemini.Descriptor().Model != "gemini-3.7-flash" {
		return nil, errors.New("candidate reviewer is not the pinned Gemini 3.7 Flash registration")
	}
	registry, err := review.NewRegistry([]review.Registration{
		gemini.Registration(func(context.Context) (string, error) { return apiKey, nil }),
	})
	if err != nil {
		return nil, errors.New("construct exact candidate review provider registry")
	}
	lease, err := registry.Open(ctx, gemini.RegistrationName)
	if err != nil {
		return nil, errors.New("open exact Gemini 3.7 Flash candidate reviewer")
	}
	if lease.Descriptor() != gemini.Descriptor() {
		return nil, errors.Join(
			errors.New("exact Gemini 3.7 Flash candidate reviewer descriptor drifted"), lease.Close(),
		)
	}
	return lease, nil
}

func preflightExactCandidateReviewer(ctx context.Context, apiKey string) error {
	lease, err := openExactCandidateReviewer(ctx, apiKey)
	if err != nil {
		return err
	}
	if err := lease.Close(); err != nil {
		return errors.New("close exact Gemini 3.7 Flash candidate reviewer preflight")
	}
	return nil
}

func runExactCandidateReviewCampaign(
	ctx context.Context, paths candidateReviewPaths, apiKey string,
	sensitive []string, concurrency int,
) (aggregate campaign.AggregateBundle, resultErr error) {
	manifest, _, err := sourcebundle.Verify(ctx, paths.SourceDirectory, paths.SourceReceipt)
	if err != nil {
		return campaign.AggregateBundle{}, fmt.Errorf("verify current-run candidate source: %w", err)
	}
	if !manifest.Complete || manifest.AttemptCount == 0 || len(manifest.Attempts) != manifest.AttemptCount {
		return campaign.AggregateBundle{}, errors.New("candidate review source population is incomplete")
	}
	for _, attempt := range manifest.Attempts {
		if !attempt.EvidenceComplete || attempt.Media == nil || attempt.ContextPath == "" {
			return campaign.AggregateBundle{}, errors.New(
				"candidate review source contains an incomplete current-run attempt",
			)
		}
	}
	lease, err := openExactCandidateReviewer(ctx, apiKey)
	if err != nil {
		return campaign.AggregateBundle{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lease.Close())
		if resultErr != nil {
			aggregate = campaign.AggregateBundle{}
		}
	}()
	result, err := campaign.Run(ctx, paths.campaignOptions(lease, sensitive, concurrency))
	if err != nil {
		return campaign.AggregateBundle{}, err
	}
	aggregate, err = campaign.PublishAggregate(ctx, paths.aggregateOptions(sensitive), result)
	if err != nil {
		return campaign.AggregateBundle{}, err
	}
	verified, err := campaign.VerifyAggregate(ctx, paths.aggregateOptions(nil))
	if err != nil {
		return campaign.AggregateBundle{}, err
	}
	if verified.Receipt != aggregate.Receipt {
		return campaign.AggregateBundle{}, errors.New("candidate review aggregate changed during credential-free reopen")
	}
	return verified, nil
}

func runCandidateReviewRecovery(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench review-candidate", flag.ContinueOnError)
	config := candidateReviewCLIConfig{}
	config.bind(flags)
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(config.Prefix) == "" {
		return errors.New("review-candidate accepts flags only and requires -review-prefix")
	}
	if config.Provider != gemini.RegistrationName || config.Concurrency < 1 || config.Concurrency > 16 {
		return errors.New("candidate review recovery requires exact Gemini 3.7 Flash and concurrency 1..16")
	}
	apiKey, present := os.LookupEnv(config.APIKeyEnvironment)
	if !present || !validCandidateReviewSecret(apiKey) {
		return errors.New("candidate review Gemini key environment is unset or invalid")
	}
	paths, err := resolveCandidateReviewPaths(config.Prefix)
	if err != nil {
		return err
	}
	if candidateReviewPathsContainSensitive(paths, []string{apiKey}) {
		return errors.New("candidate review artifact path contains a sensitive value")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	aggregate, err := runExactCandidateReviewCampaign(
		ctx, paths, apiKey, []string{apiKey}, config.Concurrency,
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "candidate review recovered: %d/%d recordings evaluated by %s (advisory)\n",
		aggregate.Manifest.EvaluationCount, aggregate.Manifest.Expected, aggregate.Manifest.Provider.Model)
	fmt.Fprintf(output, "review bundle   : %s\n", paths.AggregateDirectory)
	fmt.Fprintf(output, "review receipt  : %s\n", aggregate.Receipt.ReceiptSHA256)
	return nil
}

func runCandidateReviewVerification(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench verify-candidate-review", flag.ContinueOnError)
	var prefix string
	flags.StringVar(&prefix, "review-prefix", "", "artifact prefix of a completed current-run candidate review")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(prefix) == "" {
		return errors.New("verify-candidate-review accepts flags only and requires -review-prefix")
	}
	paths, err := resolveCandidateReviewPaths(prefix)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	aggregate, err := productionCandidateReviewOperations().verify(ctx, paths)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "verified candidate review: %d/%d recordings, %s (advisory)\n",
		aggregate.Manifest.EvaluationCount, aggregate.Manifest.Expected, aggregate.Manifest.Provider.Model)
	fmt.Fprintf(output, "review bundle   : %s\n", paths.AggregateDirectory)
	fmt.Fprintf(output, "review receipt  : %s\n", aggregate.Receipt.ReceiptSHA256)
	return nil
}
