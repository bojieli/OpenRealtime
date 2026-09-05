package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/dynacu"
	"github.com/bojieli/OpenRealtime/bench/fdb"
	"github.com/bojieli/OpenRealtime/bench/fdbench"
	"github.com/bojieli/OpenRealtime/bench/fdbv3"
	"github.com/bojieli/OpenRealtime/bench/meeting"
	"github.com/bojieli/OpenRealtime/bench/realtimecu"
	review "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	reviewffmpeg "github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
	"github.com/bojieli/OpenRealtime/bench/tauvoice"
)

type meetingFFmpegReviewFactory struct {
	options reviewffmpeg.Options
}

func (factory meetingFFmpegReviewFactory) NewReviewVideo(
	ctx context.Context, _ meeting.EvidenceAttempt,
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

type meetingReviewCLIConfig struct {
	Directory                     string
	Resume                        bool
	Provider                      string
	APIKeyEnvironment             string
	SourceReceiptPath             string
	EvaluationReceiptDirectory    string
	EvaluationQuarantineDirectory string
	FFmpegPath                    string
	FFprobePath                   string
	BubblewrapPath                string
}

type meetingReviewCLIResources struct {
	bundle                        *meeting.ReviewBundle
	resumedResult                 *bench.Result
	lease                         *review.ProviderLease
	reviewer                      review.ProviderDescriptor
	encoder                       reviewmedia.EncoderDescriptor
	attestor                      reviewmedia.AttestorDescriptor
	sourceReceiptPath             string
	evaluationReceiptDirectory    string
	evaluationQuarantineDirectory string
}

// runBench executes one suite against a running server.
//
// It drives the server over the protocol rather than constructing a session
// in-process, and deliberately: a measurement of something other than what
// users get is not a measurement of anything.
func runBench(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime bench <execution|architecture|meeting|realtime-cu|anchor-realtime-cu-review|verify-realtime-cu-review|fdb|fdbv3|fdbench|tau-voice|dynacu|review-candidate|verify-candidate-review> [flags]")
	}
	suite := strings.ToLower(strings.TrimSpace(arguments[0]))
	switch suite {
	case "execution", "attestation":
		return runExecutionRequirement(arguments[1:], output)
	case "architecture", "architecture-pair", "f52":
		return runArchitecturePair(arguments[1:], output)
	case "fdb", "fdb-v1.5":
		return runFDB(arguments[1:], output)
	case "fdbench", "fd-bench":
		return runFDBench(arguments[1:], output)
	case "fdbv3", "fdb-v3":
		return runFDBv3(arguments[1:], output)
	case "tau-voice", "tauvoice", "tau":
		return runTauVoice(arguments[1:], output)
	case "realtime-cu", "realtime-computer-use", "computer-use":
		return runRealtimeCU(arguments[1:], output)
	case "verify-realtime-cu-review":
		return runRealtimeCUReviewVerification(arguments[1:], output)
	case "anchor-realtime-cu-review":
		return runRealtimeCUReviewReceiptPublication(arguments[1:], output)
	case "meeting", "meeting-assistant", "live-meeting":
		return runMeeting(arguments[1:], output)
	case "dynacu":
		return runDynaCU(arguments[1:], output)
	case "review-candidate":
		return runCandidateReviewRecovery(arguments[1:], output)
	case "verify-candidate-review":
		return runCandidateReviewVerification(arguments[1:], output)
	default:
		return fmt.Errorf("unknown suite %q", suite)
	}
}

type meetingCommandDependencies struct {
	run        func(context.Context, meeting.Options) (bench.Result, error)
	openReview func(
		context.Context, meetingReviewCLIConfig, string, func(string) (string, bool),
	) (*meetingReviewCLIResources, error)
}

// runMeeting executes the repository-owned concurrent meeting-assistant suite.
func runMeeting(arguments []string, output io.Writer) error {
	return runMeetingWithDependencies(arguments, output, meetingCommandDependencies{
		run: meeting.Run, openReview: openMeetingReviewCLI,
	})
}

func runMeetingWithDependencies(
	arguments []string, output io.Writer, dependencies meetingCommandDependencies,
) error {
	if dependencies.run == nil || dependencies.openReview == nil {
		return errors.New("meeting command dependencies are incomplete")
	}
	flags := flag.NewFlagSet("openrealtime bench meeting", flag.ContinueOnError)
	var (
		endpoint        string
		transport       string
		tokenEnv        string
		model           string
		out             string
		browser         string
		categories      string
		limit           int
		fps             int
		timeout         time.Duration
		analysisDelay   time.Duration
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		executionPath   string
		inspectionGraph string
		list            bool
		reviewConfig    meetingReviewCLIConfig
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "WebSocket or WebRTC SDP endpoint")
	flags.StringVar(&transport, "transport", bench.TransportWebSocket, "sensor/executor transport: websocket or webrtc")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "openrealtime", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&browser, "browser", "", "Chromium executable; empty discovers it")
	flags.StringVar(&categories, "categories", "", "comma-separated task categories; empty runs all")
	flags.IntVar(&limit, "limit", 0, "stop after this many cases; a limited run is incomplete")
	flags.IntVar(&fps, "fps", 5, "shared-screen capture rate")
	flags.DurationVar(&timeout, "task-timeout", 40*time.Second, "bound one meeting episode")
	flags.DurationVar(&analysisDelay, "analysis-delay", 8*time.Second, "duration of the deliberately outstanding analysis tool")
	flags.StringVar(&cellName, "cell", "meeting-assistant-graph-native-candidate", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides held fixed across a pair")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F9")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.StringVar(&executionPath, "execution", "", benchmarkExecutionFlagHelp)
	flags.StringVar(&inspectionGraph, "inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	flags.StringVar(&reviewConfig.Directory, "review-dir", "", "retain sealed Meeting source media and advisory review evidence")
	flags.BoolVar(&reviewConfig.Resume, "review-resume", false,
		"resume a receipt-anchored Meeting review campaign without rerunning deterministic tasks")
	flags.StringVar(&reviewConfig.Provider, "review-provider", gemini.RegistrationName,
		"offline reviewer plug-in (exactly google.gemini-3.7-flash)")
	flags.StringVar(&reviewConfig.APIKeyEnvironment, "review-key-env", "GEMINI_API_KEY",
		"environment variable holding the Gemini review key")
	flags.StringVar(&reviewConfig.SourceReceiptPath, "review-source-receipt", "",
		"external create-only deterministic source receipt path")
	flags.StringVar(&reviewConfig.EvaluationReceiptDirectory, "review-evaluation-receipts", "",
		"external directory for create-only per-case evaluation receipts")
	flags.StringVar(&reviewConfig.EvaluationQuarantineDirectory, "review-evaluation-quarantine", "",
		"external directory preserving interrupted pre-receipt evaluation stages")
	flags.StringVar(&reviewConfig.FFmpegPath, "review-ffmpeg", "", "explicit FFmpeg binary for review video")
	flags.StringVar(&reviewConfig.FFprobePath, "review-ffprobe", "", "explicit FFprobe binary for full-decode attestation")
	flags.StringVar(&reviewConfig.BubblewrapPath, "review-bwrap", "", "explicit bubblewrap binary for media sandboxing")
	flags.BoolVar(&list, "list", false, "list repository-owned meeting tasks and stop")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("meeting accepts flags only")
	}
	if list {
		for _, task := range meeting.Suite() {
			fmt.Fprintf(output, "%-34s %-20s %s\n", task.ID, task.Category, task.Difficulty)
		}
		return nil
	}
	cell, err := resolveCellFrom(
		meeting.ReferenceCell(), cellName, referenceLevels, varyFactor, varyLevel,
	)
	if err != nil {
		return err
	}
	if err := attachBenchmarkExecution(&cell, executionPath); err != nil {
		return err
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		cell.Execution, inspectionGraph, endpoint, tokenEnv, os.Getenv,
	)
	if err != nil {
		return err
	}
	var selectedCategories []string
	for _, value := range strings.Split(categories, ",") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			selectedCategories = append(selectedCategories, trimmed)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reviewResources, err := dependencies.openReview(ctx, reviewConfig, deploymentToken, os.LookupEnv)
	if err != nil {
		return err
	}
	var evidence meeting.EvidencePlugin
	if reviewResources != nil {
		evidence = reviewResources.bundle
	}
	var result bench.Result
	if reviewResources != nil && reviewResources.resumedResult != nil {
		result = *reviewResources.resumedResult
		resumeContext, cancelResume := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Minute)
		err = reviewResources.bundle.FinishSuite(resumeContext, result)
		cancelResume()
	} else {
		result, err = dependencies.run(ctx, meeting.Options{
			Endpoint: endpoint, Transport: transport, Token: deploymentToken, Model: model,
			Cell: cell, Browser: browser, Categories: selectedCategories, Limit: limit,
			FrameRate: fps, Timeout: timeout, AnalysisDelay: analysisDelay,
			RuntimeAttestor: attestor,
			Evidence:        evidence,
			Progress:        func(line string) { fmt.Fprintln(output, line) },
		})
	}
	if reviewResources != nil {
		retentionContext, cancelRetention := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		retentionErr := reviewResources.retain(retentionContext, output)
		cancelRetention()
		err = errors.Join(err, retentionErr)
		err = errors.Join(err, reviewResources.close())
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "suite      : %s\n", result.Suite)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "tasks      : %d completed, %d infrastructure failures, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	fmt.Fprintf(output, "passed     : %d/%d (%.1f%%)\n",
		result.Summary.Passed, result.Summary.Completed, result.Summary.PassRate*100)
	for _, metric := range []string{
		"followup_action_latency_ms", "visual_cue_to_action_ms", "correction_to_action_ms",
		"cue_to_document_open_ms", "cue_to_screen_share_ms", "action_count",
		"invalid_action_count", "deadline_miss_count", "session_timeout_count",
	} {
		distribution, present := result.Summary.Distributions[metric]
		if !present {
			continue
		}
		fmt.Fprintf(output, "  %-32s n=%-3d p50 %-9s p95 %-9s max %s\n", metric,
			distribution.Count, distribution.Format(distribution.P50),
			distribution.Format(distribution.P95), distribution.Format(distribution.Max))
	}
	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	} else {
		fmt.Fprintln(output, "\nreportable")
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "written to %s\n", out)
	}
	return nil
}

func openMeetingReviewCLI(
	ctx context.Context, config meetingReviewCLIConfig, deploymentToken string,
	lookupEnv func(string) (string, bool),
) (*meetingReviewCLIResources, error) {
	if strings.TrimSpace(config.Directory) == "" {
		if config.Resume || config.SourceReceiptPath != "" || config.EvaluationReceiptDirectory != "" ||
			config.EvaluationQuarantineDirectory != "" ||
			config.FFmpegPath != "" || config.FFprobePath != "" || config.BubblewrapPath != "" ||
			(config.Provider != "" && config.Provider != gemini.RegistrationName) {
			return nil, errors.New("meeting review options require -review-dir")
		}
		return nil, nil
	}
	if ctx == nil {
		return nil, errors.New("open meeting review CLI: nil context")
	}
	if config.Provider != gemini.RegistrationName || gemini.ModelID != "gemini-3.7-flash" ||
		gemini.Descriptor().Model != gemini.ModelID {
		return nil, errors.New("meeting review provider must be the exact google.gemini-3.7-flash registration")
	}
	if strings.TrimSpace(config.APIKeyEnvironment) == "" || lookupEnv == nil {
		return nil, errors.New("meeting review key environment is invalid")
	}
	directory, err := filepath.Abs(config.Directory)
	if err != nil {
		return nil, errors.New("resolve meeting review directory")
	}
	if config.SourceReceiptPath == "" {
		config.SourceReceiptPath = directory + ".source-receipt.json"
	}
	if config.EvaluationReceiptDirectory == "" {
		config.EvaluationReceiptDirectory = directory + ".evaluation-receipts"
	}
	if config.EvaluationQuarantineDirectory == "" {
		config.EvaluationQuarantineDirectory = directory + ".evaluation-quarantine"
	}
	sourceReceiptPath, err := filepath.Abs(config.SourceReceiptPath)
	if err != nil {
		return nil, errors.New("resolve meeting review source receipt path")
	}
	evaluationReceiptDirectory, err := filepath.Abs(config.EvaluationReceiptDirectory)
	if err != nil {
		return nil, errors.New("resolve meeting evaluation receipt directory")
	}
	evaluationQuarantineDirectory, err := filepath.Abs(config.EvaluationQuarantineDirectory)
	if err != nil {
		return nil, errors.New("resolve meeting evaluation quarantine directory")
	}
	for _, path := range []string{
		directory, sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory,
	} {
		if filepath.Clean(path) != path || path == filepath.Dir(path) {
			return nil, errors.New("meeting review artifact paths must be clean, absolute, and non-root")
		}
	}
	if sourceReceiptPath == directory || strings.HasPrefix(sourceReceiptPath, directory+string(filepath.Separator)) ||
		evaluationReceiptDirectory == directory ||
		strings.HasPrefix(evaluationReceiptDirectory, directory+string(filepath.Separator)) ||
		evaluationQuarantineDirectory == directory ||
		strings.HasPrefix(evaluationQuarantineDirectory, directory+string(filepath.Separator)) {
		return nil, errors.New("meeting review external receipts must be outside the review bundle")
	}
	separator := string(filepath.Separator)
	pathsOverlap := func(left, right string) bool {
		return left == right || strings.HasPrefix(left, right+separator) ||
			strings.HasPrefix(right, left+separator)
	}
	if pathsOverlap(sourceReceiptPath, evaluationReceiptDirectory) ||
		pathsOverlap(sourceReceiptPath, evaluationQuarantineDirectory) ||
		pathsOverlap(evaluationReceiptDirectory, evaluationQuarantineDirectory) {
		return nil, errors.New("meeting review external receipt and quarantine paths overlap")
	}
	if err := validateMeetingReviewCLIParent(filepath.Dir(directory)); err != nil {
		return nil, err
	}
	if err := validateMeetingReviewCLIParent(filepath.Dir(sourceReceiptPath)); err != nil {
		return nil, err
	}
	if err := validateMeetingReviewCLIParent(filepath.Dir(evaluationReceiptDirectory)); err != nil {
		return nil, err
	}
	if err := validateMeetingReviewCLIParent(filepath.Dir(evaluationQuarantineDirectory)); err != nil {
		return nil, err
	}
	if config.Resume {
		for path, wantDirectory := range map[string]bool{
			directory: true, sourceReceiptPath: false,
			evaluationReceiptDirectory: true, evaluationQuarantineDirectory: true,
		} {
			info, statErr := os.Lstat(path)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDirectory {
				return nil, fmt.Errorf("meeting review resume path is missing or invalid: %s", path)
			}
		}
	} else {
		for _, path := range []string{
			directory, sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory,
		} {
			if _, err := os.Lstat(path); err == nil {
				return nil, fmt.Errorf("meeting review create-only path already exists: %s", path)
			} else if !os.IsNotExist(err) {
				return nil, errors.New("inspect meeting review create-only path")
			}
		}
	}
	apiKey, present := lookupEnv(config.APIKeyEnvironment)
	if !present || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("meeting Gemini review key environment is unset")
	}
	registration := gemini.Registration(func(context.Context) (string, error) { return apiKey, nil })
	registry, err := review.NewRegistry([]review.Registration{registration})
	if err != nil {
		return nil, errors.New("construct meeting review provider registry")
	}
	lease, err := registry.Open(ctx, gemini.RegistrationName)
	if err != nil {
		return nil, errors.New("open exact Gemini 3.7 Flash meeting reviewer")
	}
	cleanupLease := true
	defer func() {
		if cleanupLease {
			_ = lease.Close()
		}
	}()
	sensitive := []string{apiKey}
	if deploymentToken != "" && (len(deploymentToken) < 8 || strings.TrimSpace(deploymentToken) != deploymentToken) {
		return nil, errors.New("meeting review deployment credential is too short or invalid for leakage protection")
	}
	if deploymentToken != "" {
		sensitive = append(sensitive, deploymentToken)
	}
	ffmpegOptions := reviewffmpeg.Options{
		FFmpegPath: config.FFmpegPath, FFprobePath: config.FFprobePath,
		BubblewrapPath: config.BubblewrapPath, SensitiveValues: sensitive,
	}
	var encoderDescriptor reviewmedia.EncoderDescriptor
	var attestorDescriptor reviewmedia.AttestorDescriptor
	if !config.Resume {
		// Resolve and fingerprint the selected encoder/attestor toolchain before a
		// live benchmark starts; attempt-scoped instances are still supplied by the
		// composable factory below.
		encoder, encoderErr := reviewffmpeg.NewEncoder(ctx, ffmpegOptions)
		if encoderErr != nil {
			return nil, errors.New("preflight meeting review FFmpeg encoder")
		}
		attestor, attestorErr := reviewffmpeg.NewAttestor(ctx, ffmpegOptions)
		if attestorErr != nil {
			_ = encoder.Close()
			return nil, errors.New("preflight meeting review FFmpeg full-decode attestor")
		}
		if encoder.Descriptor().Name == "" ||
			attestor.Descriptor().Capability != reviewmedia.FullDecodeAttestationCapability {
			_ = encoder.Close()
			_ = attestor.Close()
			return nil, errors.New("meeting review FFmpeg toolchain lacks full-decode identity")
		}
		encoderDescriptor = encoder.Descriptor()
		attestorDescriptor = attestor.Descriptor()
		if err := errors.Join(encoder.Close(), attestor.Close()); err != nil {
			return nil, errors.New("close meeting review FFmpeg preflight plug-ins")
		}
	}
	bundleOptions := meeting.ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		SourceReceiptPath: sourceReceiptPath, EvaluationReceiptDirectory: evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
		SensitiveValues:               sensitive,
	}
	var bundle *meeting.ReviewBundle
	var resumedResult *bench.Result
	if config.Resume {
		resumed, recovered, resumeErr := meeting.ResumeReviewBundle(ctx, bundleOptions)
		if resumeErr != nil {
			return nil, errors.New("resume meeting review evidence plug-in")
		}
		bundle = resumed
		resumedResult = &recovered
	} else {
		bundleOptions.VideoFactory = meetingFFmpegReviewFactory{options: ffmpegOptions}
		bundle, err = meeting.NewReviewBundle(bundleOptions)
	}
	if err != nil {
		return nil, errors.New("create meeting review evidence plug-in")
	}
	cleanupLease = false
	return &meetingReviewCLIResources{
		bundle: bundle, resumedResult: resumedResult, lease: lease, reviewer: lease.Descriptor(),
		encoder: encoderDescriptor, attestor: attestorDescriptor,
		sourceReceiptPath:             sourceReceiptPath,
		evaluationReceiptDirectory:    evaluationReceiptDirectory,
		evaluationQuarantineDirectory: evaluationQuarantineDirectory,
	}, nil
}

func validateMeetingReviewCLIParent(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("meeting review artifact parent is missing, symlinked, or not a directory")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func (resources *meetingReviewCLIResources) retain(ctx context.Context, output io.Writer) error {
	if resources == nil || resources.bundle == nil {
		return nil
	}
	var resultErr error
	sourceReceipt, err := resources.bundle.SourceReceipt()
	if err == nil {
		external, readErr := meeting.ReadReviewSourceReceipt(ctx, resources.sourceReceiptPath)
		if readErr != nil || external.ManifestSHA256 != sourceReceipt.ManifestSHA256 ||
			external.FileSetSHA256 != sourceReceipt.FileSetSHA256 ||
			external.ResultSHA256 != sourceReceipt.ResultSHA256 ||
			external.ReceiptSHA256 != sourceReceipt.ReceiptSHA256 {
			resultErr = errors.Join(resultErr, errors.New("verify durable external meeting source receipt"))
		} else {
			fmt.Fprintf(output, "meeting source receipt retained at %s\n", resources.sourceReceiptPath)
		}
	} else {
		resultErr = errors.Join(resultErr, err)
	}
	for _, receipt := range resources.bundle.EvaluationReceipts() {
		name := filepath.Base(receipt.Directory) + ".receipt.json"
		path := filepath.Join(resources.evaluationReceiptDirectory, name)
		external, err := review.ReadEvaluationBundleReceipt(ctx, path)
		if err != nil || external != receipt {
			resultErr = errors.Join(resultErr, errors.New("verify durable external meeting evaluation receipt"))
			continue
		}
		fmt.Fprintf(output, "meeting evaluation receipt retained at %s\n", path)
	}
	if receipt, err := resources.bundle.Receipt(); err == nil {
		fmt.Fprintf(output, "meeting review bundle sealed at %s (%s)\n",
			receipt.Directory, receipt.ManifestSHA256)
	} else {
		fmt.Fprintln(output, "meeting review bundle is diagnostic/incomplete; sealed source receipt remains authoritative")
	}
	return resultErr
}

func (resources *meetingReviewCLIResources) close() error {
	if resources == nil {
		return nil
	}
	var err error
	if resources.bundle != nil {
		err = errors.Join(err, resources.bundle.Close())
		resources.bundle = nil
	}
	if resources.lease != nil {
		err = errors.Join(err, resources.lease.Close())
		resources.lease = nil
	}
	return err
}

// runArchitecturePair classifies two measured F52 cells. A comparison can be
// useful and reportable while still being a system comparison; only a paired
// P/T or T/N run with identical non-treatment identities is architecture-only
// evidence.
func runArchitecturePair(arguments []string, output io.Writer) error {
	if len(arguments) > 0 {
		switch strings.ToLower(strings.TrimSpace(arguments[0])) {
		case "inspect":
			return runArchitectureInspect(arguments[1:], output)
		case "cell":
			return runArchitectureCell(arguments[1:], output)
		case "manifest":
			return runArchitectureManifest(arguments[1:], output)
		}
	}
	flags := flag.NewFlagSet("openrealtime bench architecture", flag.ContinueOnError)
	baselinePath := flags.String("baseline", "", "baseline architecture result JSON")
	variantPath := flags.String("variant", "", "variant architecture result JSON")
	out := flags.String("out", "", "write the comparison to this path as JSON")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*baselinePath) == "" || strings.TrimSpace(*variantPath) == "" {
		return errors.New("architecture comparison requires -baseline and -variant result paths")
	}
	baseline, err := archbench.ReadResult(*baselinePath)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}
	variant, err := archbench.ReadResult(*variantPath)
	if err != nil {
		return fmt.Errorf("read variant: %w", err)
	}
	comparison := archbench.Pair(baseline, variant)
	fmt.Fprintf(output, "baseline   : %s (%s)\n", baseline.Cell.Name, baseline.Cell.Architecture.Level)
	fmt.Fprintf(output, "variant    : %s (%s)\n", variant.Cell.Name, variant.Cell.Architecture.Level)
	fmt.Fprintf(output, "pass rate  : %.1f%% -> %.1f%% (%+.1f points)\n",
		comparison.Measurement.BaselineRate*100, comparison.Measurement.VariantRate*100,
		comparison.Measurement.Difference*100)
	if comparison.Reportable {
		fmt.Fprintf(output, "claim       : %s comparison\n", comparison.Claim)
		if len(comparison.Differences) > 0 {
			fmt.Fprintf(output, "confounders : %s\n", strings.Join(comparison.Differences, ", "))
		}
	} else {
		fmt.Fprintf(output, "NOT REPORTABLE: %s\n", comparison.Refusal)
	}
	if strings.TrimSpace(*out) != "" {
		payload, err := json.MarshalIndent(comparison, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(payload, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

const architectureInspectionInstruction = "Inspect the negotiated runtime architecture. Do not generate a response."

func architectureInspectionSessionConfig(
	endpoint, token, model string, timeout time.Duration,
) bench.SessionConfig {
	return bench.SessionConfig{
		Endpoint: endpoint, Token: token, Model: model,
		Instructions: architectureInspectionInstruction,
		Timeout:      timeout, TrailingSilence: time.Millisecond,
		CaptureRuntimeEvidence: true, Quiet: true,
	}
}

// runArchitectureInspect opens an ordinary protocol session and prints the
// handshake-resolved binding status that a manifest must match. It is a
// read-only authoring aid; inspection is not a benchmark result.
func runArchitectureInspect(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench architecture inspect", flag.ContinueOnError)
	endpoint := flags.String("endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	tokenEnv := flags.String("token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	model := flags.String("model", "", "model to request; empty selects the server's own")
	out := flags.String("out", "", "write live status JSON to this path")
	catalogPath := flags.String("catalog", "", "external architecture catalog; empty uses the repository-owned catalog")
	timeout := flags.Duration("timeout", 10*time.Second, "bound the inspection session")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("architecture inspect accepts flags only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	transcript, err := bench.PlaySamples(ctx, architectureInspectionSessionConfig(
		*endpoint, os.Getenv(*tokenEnv), *model, *timeout,
	), nil)
	if err != nil {
		return err
	}
	if transcript.Runtime == nil {
		return errors.New("the server did not emit negotiated runtime evidence")
	}
	catalog, err := loadArchitectureCatalog(*catalogPath)
	if err != nil {
		return err
	}
	if transcript.Runtime.Architecture.Empty() {
		return errors.New("the server was not launched through an immutable architecture definition")
	}
	definition, err := catalog.LookupIdentity(transcript.Runtime.Architecture)
	if err != nil {
		return err
	}
	if err := definition.ValidateStatus(*transcript.Runtime); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(transcript.Runtime, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(output, string(payload))
	if strings.TrimSpace(*out) != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(payload, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// runArchitectureCell authors one experiment cell from three independent
// authorities: a project definition, a post-handshake status, and immutable
// deployment pins. This replaces copy-pasted JSON and launch-script labels.
func runArchitectureCell(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench architecture cell", flag.ContinueOnError)
	name := flags.String("name", "", "cell name")
	definitionRef := flags.String("definition", "", "exact architecture id@revision")
	catalogPath := flags.String("catalog", "", "external architecture catalog; empty uses the repository-owned catalog")
	statusPath := flags.String("status", "", "post-handshake status from architecture inspect")
	pinsPath := flags.String("pins", "", "immutable deployment pins JSON")
	executionPath := flags.String("execution", "", "reviewed execution-requirement JSON; empty authors an unattested compatibility cell")
	out := flags.String("out", "", "write the authored cell JSON")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*name) == "" ||
		strings.TrimSpace(*definitionRef) == "" || strings.TrimSpace(*statusPath) == "" ||
		strings.TrimSpace(*pinsPath) == "" || strings.TrimSpace(*out) == "" {
		return errors.New("architecture cell requires -name, -definition, -status, -pins, and -out")
	}
	catalog, err := loadArchitectureCatalog(*catalogPath)
	if err != nil {
		return err
	}
	definition, err := catalog.Resolve(*definitionRef)
	if err != nil {
		return err
	}
	status, err := archbench.ReadStatus(*statusPath)
	if err != nil {
		return err
	}
	pins, err := archbench.ReadPins(*pinsPath)
	if err != nil {
		return err
	}
	cell, err := archbench.BuildCell(*name, definition, status, pins)
	if err != nil {
		return err
	}
	if err := attachArchitectureExecution(&cell, *executionPath); err != nil {
		return err
	}
	if err := archbench.WriteCell(*out, cell); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s  F52=%s  definition=%s\n", *out, cell.Architecture.Level, definition.Ref())
	return nil
}

// attachArchitectureExecution keeps the executable graph contract separate
// from architecture structure, deployment pins, and observed task evidence.
// An omitted path preserves historical authoring behavior; an explicit path
// must contain a non-zero, strictly validated requirement artifact.
func attachArchitectureExecution(cell *archbench.Cell, path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if cell == nil {
		return errors.New("attach architecture execution: nil cell")
	}
	requirement, err := bench.ReadExecutionRequirement(path)
	if err != nil {
		return err
	}
	cell.Execution = requirement
	return nil
}

type architectureCellPaths []string

func (paths *architectureCellPaths) String() string { return strings.Join(*paths, ",") }
func (paths *architectureCellPaths) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("architecture cell path cannot be empty")
	}
	*paths = append(*paths, value)
	return nil
}

// runArchitectureManifest assembles reviewed cells into the exact experiment
// consumed by scenario. Cells remain separately inspectable and reusable.
func runArchitectureManifest(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench architecture manifest", flag.ContinueOnError)
	name := flags.String("name", "", "experiment name")
	suite := flags.String("suite", "scenario", "benchmark suite")
	fixture := flags.String("fixture-revision", "", "immutable suite fixture revision")
	out := flags.String("out", "", "write the experiment manifest JSON")
	var cellPaths architectureCellPaths
	flags.Var(&cellPaths, "cell", "authored cell JSON; repeat for every desired cell")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*name) == "" ||
		strings.TrimSpace(*fixture) == "" || strings.TrimSpace(*out) == "" || len(cellPaths) == 0 {
		return errors.New("architecture manifest requires -name, -fixture-revision, -out, and one or more -cell")
	}
	manifest := archbench.Manifest{
		Version: archbench.ManifestVersion, Name: *name, Suite: *suite,
		FixtureRevision: *fixture,
	}
	for _, path := range cellPaths {
		cell, err := archbench.ReadCell(path)
		if err != nil {
			return fmt.Errorf("read cell %s: %w", path, err)
		}
		manifest.Cells = append(manifest.Cells, cell)
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := archbench.WriteManifest(*out, manifest); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s  %d cells  manifest=%s\n", *out, len(manifest.Cells), manifest.ID())
	return nil
}

// runRealtimeCU executes the repository-owned audiovisual computer-use suite.
func runRealtimeCU(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench realtime-cu", flag.ContinueOnError)
	var (
		endpoint        string
		tokenEnv        string
		model           string
		out             string
		browser         string
		observers       string
		groundings      string
		categories      string
		limit           int
		fps             int
		timeout         time.Duration
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		executionPath   string
		inspectionGraph string
		list            bool
		reviewConfig    realtimeCUReviewCLIConfig
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "openrealtime", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&browser, "browser", "", "Chromium executable; empty discovers it")
	flags.StringVar(&observers, "observers", realtimeCULocalObserverName,
		"comma-separated exact OpenRealtime perception plug-in names")
	flags.StringVar(&groundings, "grounding", "pixel,set_of_mark", "comma-separated grounding conditions")
	flags.StringVar(&categories, "categories", "", "comma-separated task categories; empty runs all")
	flags.IntVar(&limit, "limit", 0, "stop after this many cases; a limited run is incomplete")
	flags.IntVar(&fps, "fps", 3, "screen and camera capture rate")
	flags.DurationVar(&timeout, "task-timeout", 45*time.Second, "bound one browser task")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides held fixed across a pair")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.StringVar(&executionPath, "execution", "", benchmarkExecutionFlagHelp)
	flags.StringVar(&inspectionGraph, "inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	flags.StringVar(&reviewConfig.Directory, "review-dir", "", "retain sealed Realtime-CU source media and advisory review evidence")
	flags.BoolVar(&reviewConfig.Resume, "review-resume", false,
		"resume a receipt-anchored Realtime-CU review campaign without rerunning browser actions")
	flags.StringVar(&reviewConfig.Provider, "review-provider", gemini.RegistrationName,
		"offline reviewer plug-in (exactly google.gemini-3.7-flash)")
	flags.StringVar(&reviewConfig.APIKeyEnvironment, "review-key-env", "GEMINI_API_KEY",
		"environment variable holding the Gemini review key")
	flags.StringVar(&reviewConfig.ReceiptPath, "review-receipt", "",
		"external create-only final review receipt path")
	flags.StringVar(&reviewConfig.SourceReceiptPath, "review-source-receipt", "",
		"external create-only deterministic source receipt path")
	flags.StringVar(&reviewConfig.EvaluationReceiptDirectory, "review-evaluation-receipts", "",
		"external directory for create-only per-case evaluation receipts")
	flags.StringVar(&reviewConfig.EvaluationQuarantineDirectory, "review-evaluation-quarantine", "",
		"external directory preserving interrupted pre-receipt evaluation stages")
	flags.IntVar(&reviewConfig.Concurrency, "review-concurrency", 4,
		"parallel advisory reviews; maximum 16")
	flags.StringVar(&reviewConfig.FFmpegPath, "review-ffmpeg", "", "explicit FFmpeg binary for review video")
	flags.StringVar(&reviewConfig.FFprobePath, "review-ffprobe", "", "explicit FFprobe binary for full-decode attestation")
	flags.StringVar(&reviewConfig.BubblewrapPath, "review-bwrap", "", "explicit bubblewrap binary for media sandboxing")
	flags.BoolVar(&list, "list", false, "list repository-owned tasks and stop")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("realtime-cu accepts flags only")
	}
	if list {
		for _, task := range realtimecu.Suite() {
			fmt.Fprintf(output, "%-30s %-18s %-6s %s\n", task.ID, task.Category, task.Difficulty, axes(task.Axes))
		}
		return nil
	}
	if fps != 3 {
		return errors.New("the production Realtime-CU benchmark profile requires exactly 3 fps")
	}
	cell, err := resolveRealtimeCUCell(cellName, referenceLevels, varyFactor, varyLevel)
	if err != nil {
		return err
	}
	if err := attachBenchmarkExecution(&cell, executionPath); err != nil {
		return err
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		cell.Execution, inspectionGraph, endpoint, tokenEnv, os.Getenv,
	)
	if err != nil {
		return err
	}
	var selectedGroundings []realtimecu.Grounding
	for _, value := range strings.Split(groundings, ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		grounding, err := realtimecu.ParseGrounding(value)
		if err != nil {
			return err
		}
		selectedGroundings = append(selectedGroundings, grounding)
	}
	var selectedCategories []string
	for _, value := range strings.Split(categories, ",") {
		if strings.TrimSpace(value) != "" {
			selectedCategories = append(selectedCategories, strings.TrimSpace(value))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reviewResources, err := openRealtimeCUReviewCLI(
		ctx, reviewConfig, deploymentToken, os.LookupEnv,
	)
	if err != nil {
		return err
	}
	var result bench.Result
	if reviewResources != nil && reviewResources.resumedResult != nil {
		result, err = finishRealtimeCUReviewResume(ctx, reviewResources)
	} else {
		var evidence realtimecu.EvidencePlugin
		if reviewResources != nil {
			evidence = reviewResources.bundle
		}
		result, err = realtimecu.Run(ctx, realtimecu.Options{
			Endpoint: endpoint, Token: deploymentToken, Model: model,
			Cell: cell, Browser: browser, Observers: splitList(observers), Groundings: selectedGroundings,
			Categories: selectedCategories, Limit: limit, FrameRate: fps, Timeout: timeout,
			RuntimeAttestor: attestor, Evidence: evidence,
			Progress: func(line string) { fmt.Fprintln(output, line) },
		})
	}
	if reviewResources != nil {
		retentionContext, cancelRetention := context.WithTimeout(
			context.WithoutCancel(ctx), 2*time.Minute,
		)
		retentionErr := reviewResources.retain(retentionContext, output)
		cancelRetention()
		err = errors.Join(err, retentionErr, reviewResources.close())
	}
	if strings.TrimSpace(out) != "" && result.Suite != "" {
		if writeErr := result.Write(out); writeErr != nil {
			err = errors.Join(err, writeErr)
		} else {
			fmt.Fprintf(output, "written to %s\n", out)
		}
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "suite      : %s\n", result.Suite)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "tasks      : %d completed, %d infrastructure failures, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	for _, grounding := range []realtimecu.Grounding{realtimecu.GroundingPixel, realtimecu.GroundingSetOfMark} {
		reading, present := realtimecu.ByGrounding(result)[grounding]
		if !present {
			continue
		}
		fmt.Fprintf(output, "  %-12s correct %d/%d (%.1f%%), correct, timely, and settled %d/%d (%.1f%%)\n",
			grounding, reading.Correct, reading.Cases, reading.CorrectRate*100,
			reading.Accepted, reading.Cases, reading.AcceptedRate*100)
	}
	for _, metric := range []string{
		"cue_to_action_latency_ms", "frame_to_observation_latency_ms",
		"cue_to_observation_latency_ms", "action_execution_ms",
		"invalid_action_count", "grounding_error_count", "premature_action_count",
		"post_success_action_count", "settlement_evidence_missing_count", "session_timeout_count",
		"session_failure_count", "outstanding_response_count", "outstanding_tool_count",
	} {
		distribution, present := result.Summary.Distributions[metric]
		if !present {
			continue
		}
		fmt.Fprintf(output, "  %-32s n=%-3d p50 %-9s p95 %-9s max %s\n", metric,
			distribution.Count, distribution.Format(distribution.P50),
			distribution.Format(distribution.P95), distribution.Format(distribution.Max))
	}
	var releaseErr error
	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	} else if reviewResources == nil {
		releaseErr = errors.New(
			"deterministic Realtime-CU result lacks the required synchronized A/V and Gemini 3.7 Flash evidence",
		)
		fmt.Fprintf(output, "\ndeterministic result reportable; NOT EVIDENCE-COMPLETE: %v\n", releaseErr)
	} else if !reviewResources.evidenceComplete {
		releaseErr = errors.New(
			"Realtime-CU result lacks a fully verified exact16 source/evaluation receipt set",
		)
		fmt.Fprintf(output, "\ndeterministic result reportable; NOT EVIDENCE-COMPLETE: %v\n", releaseErr)
	} else {
		fmt.Fprintln(output, "\nevidence-complete reportable (deterministic score + exact16 synchronized A/V + Gemini 3.7 Flash)")
	}
	return releaseErr
}

func resolveRealtimeCUCell(name, referenceLevels, factor, level string) (bench.Cell, error) {
	return resolveCellFrom(realtimecu.ReferenceCell(), name, referenceLevels, factor, level)
}

// resolveCellFrom builds a paired cell while allowing explicit levels that are
// held fixed on both sides. A diagnostic often starts from a non-global
// baseline (for example SenseVoice instead of Qwen ASR) and varies one other
// factor; without recording that fixed level, the artifact describes machinery
// that was never run.
func resolveCellFrom(reference bench.Cell, name, referenceLevels, factor, level string) (bench.Cell, error) {
	for _, assignment := range strings.Split(referenceLevels, ",") {
		assignment = strings.TrimSpace(assignment)
		if assignment == "" {
			continue
		}
		factorName, value, found := strings.Cut(assignment, "=")
		fixedFactor := bench.Factor(strings.ToUpper(strings.TrimSpace(factorName)))
		value = strings.TrimSpace(value)
		if !found || fixedFactor == "" || value == "" {
			return bench.Cell{}, fmt.Errorf("invalid reference level %q; want factor=level", assignment)
		}
		if _, known := reference.Levels[fixedFactor]; !known {
			return bench.Cell{}, fmt.Errorf("unknown reference factor %q", fixedFactor)
		}
		reference.Levels[fixedFactor] = value
	}
	if strings.TrimSpace(factor) == "" {
		if strings.TrimSpace(name) != "" && name != "reference" {
			reference.Name = name
		}
		return reference, nil
	}
	cell, err := bench.VaryFrom(reference, bench.Factor(strings.ToUpper(factor)), level)
	if err != nil {
		return bench.Cell{}, err
	}
	if strings.TrimSpace(name) != "" && name != "reference" {
		cell.Name = name
	}
	return cell, nil
}

func axes(values []realtimecu.Axis) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = string(value)
	}
	return strings.Join(parts, ",")
}

func runFDB(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench fdb", flag.ContinueOnError)
	var reviewConfig candidateReviewCLIConfig
	var (
		root            string
		endpoint        string
		tokenEnv        string
		model           string
		out             string
		categories      string
		limit           int
		repeat          int
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		executionPath   string
		inspectionGraph string
		timeout         time.Duration
	)
	flags.StringVar(&root, "dataset", ".runtime/full-duplex-bench-v1.5/dataset", "FDB v1.5 dataset root")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&categories, "categories", "", "comma-separated categories; empty runs all four")
	flags.IntVar(&limit, "limit", 0, "stop after this many recordings; a limited run is never a complete cell")
	flags.IntVar(&repeat, "repeat", 0,
		"attempts per recording; one attempt cannot tell a defect from noise, and the report says "+
			"how many recordings agreed with themselves")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides describing what this deployment actually runs")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies from the reference, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.StringVar(&executionPath, "execution", "", benchmarkExecutionFlagHelp)
	flags.StringVar(&inspectionGraph, "inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	flags.DurationVar(&timeout, "task-timeout", 3*time.Minute, "how long one recording may take")
	reviewConfig.bind(flags)
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	cell, err := resolveCell(cellName, referenceLevels, varyFactor, varyLevel)
	if err != nil {
		return err
	}
	if err := attachBenchmarkExecution(&cell, executionPath); err != nil {
		return err
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		cell.Execution, inspectionGraph, endpoint, tokenEnv, os.Getenv,
	)
	if err != nil {
		return err
	}
	var wanted []fdb.Category
	for _, name := range strings.Split(categories, ",") {
		if strings.TrimSpace(name) == "" {
			continue
		}
		wanted = append(wanted, fdb.Category(strings.TrimSpace(name)))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reviewResources, err := openCandidateReviewCLI(
		ctx, reviewConfig, deploymentToken, endpoint, os.LookupEnv,
	)
	if err != nil {
		return err
	}
	runOptions := fdb.Options{
		Root: root, Endpoint: endpoint, Token: deploymentToken, Model: model,
		Cell: cell, Categories: wanted, Limit: limit, Repeat: repeat, Timeout: timeout,
		RuntimeAttestor: attestor,
		Progress:        func(line string) { fmt.Fprintln(output, line) },
	}
	if reviewResources != nil {
		runOptions.Evidence, runOptions.EvidenceOrigin = reviewResources.bundle, reviewResources.origin
	}
	result, runErr := fdb.Run(ctx, runOptions)
	reviewErr := finishCandidateReviewIfConfigured(ctx, reviewResources, output)
	if runErr != nil || reviewErr != nil {
		return errors.Join(runErr, reviewErr)
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "provenance : %s\n", result.Provenance)
	fmt.Fprintf(output, "tasks      : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	writeFDBScores(output, result)
	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	} else {
		fmt.Fprintln(output, "\nreportable")
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "written to %s\n", out)
	}
	return nil
}

func writeFDBScores(output io.Writer, result bench.Result) {
	breakdown := fdb.Breakdown(result)
	passed, applicable, notApplicable := 0, 0, 0
	for _, summary := range breakdown {
		passed += summary.Passed
		applicable += summary.Applicable
		notApplicable += summary.NotApplicable
	}
	rate := func(passed, applicable int) string {
		if applicable == 0 {
			return "unavailable (no applicable recordings)"
		}
		if !result.Summary.Complete {
			return "unavailable (incomplete campaign)"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(passed)/float64(applicable))
	}
	fmt.Fprintf(output, "score      : %d/%d applicable, %s (%d not applicable)\n",
		passed, applicable, rate(passed, applicable), notApplicable)
	if stability := fdb.Measure(result); stability.Trials > 1 {
		fmt.Fprintf(output,
			"stability  : %d recordings x %d attempts; %d always passed, %d always failed, "+
				"%d always inapplicable, %d disagreed with themselves\n",
			stability.Recordings, stability.Trials, stability.AlwaysPassed, stability.AlwaysFailed,
			stability.AlwaysNotApplicable, len(stability.Mixed))
		for _, id := range stability.Mixed {
			fmt.Fprintf(output, "  unsettled  %s\n", id)
		}
	}
	for _, category := range fdb.Categories() {
		summary, present := breakdown[category]
		if !present {
			continue
		}
		want := "hold"
		if category.ShouldYield() {
			want = "yield"
		}
		fmt.Fprintf(output, "  %-18s %s  %d/%d applicable  %s  (%d not applicable)\n",
			category, want, summary.Passed, summary.Applicable,
			rate(summary.Passed, summary.Applicable), summary.NotApplicable)
	}
}

// resolveCell builds the cell this run measures.
//
// referenceLevels records the factors this deployment actually holds at
// something other than the canonical reference. Without it a run on local
// recognisers and models still declared the reference levels -- qwen3-asr, a
// hosted slow model -- and the artifact asserted a configuration it had not
// used. A single -vary cannot express that: a local stack differs from the
// reference in several factors at once, and each has to be nameable.
func resolveCell(name, referenceLevels, factor, level string) (bench.Cell, error) {
	return resolveCellFrom(bench.Reference(), name, referenceLevels, factor, level)
}

// compareResults reads two saved cells and prints the pairing, refusing when
// the comparison would not mean anything.
func runCompare(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime compare", flag.ContinueOnError)
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: openrealtime compare <baseline.json> <variant.json>")
	}
	baseline, err := readResult(flags.Arg(0))
	if err != nil {
		return err
	}
	variant, err := readResult(flags.Arg(1))
	if err != nil {
		return err
	}
	comparison := bench.Pair(baseline, variant)
	encoded, err := json.MarshalIndent(comparison, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(output, string(encoded))
	if !comparison.Reportable {
		return errors.New("this comparison is not reportable")
	}
	return nil
}

func readResult(path string) (bench.Result, error) {
	payload, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return bench.Result{}, err
	}
	var result bench.Result
	if err := json.Unmarshal(payload, &result); err != nil {
		return bench.Result{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return result, nil
}

func runFDBench(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench fdbench", flag.ContinueOnError)
	var reviewConfig candidateReviewCLIConfig
	var (
		root            string
		endpoint        string
		tokenEnv        string
		model           string
		out             string
		conditions      string
		list            bool
		limit           int
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		executionPath   string
		inspectionGraph string
		budget          time.Duration
		timeout         time.Duration
	)
	flags.StringVar(&root, "dataset", ".runtime/fd-bench/dataset", "FD-Bench dataset root")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&conditions, "conditions", "", "comma-separated dataset conditions; required")
	flags.BoolVar(&list, "list", false, "list the conditions this dataset contains and stop")
	flags.IntVar(&limit, "limit", 0, "stop after this many conversations")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides describing what this deployment actually runs")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F4")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.StringVar(&executionPath, "execution", "", benchmarkExecutionFlagHelp)
	flags.StringVar(&inspectionGraph, "inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	flags.DurationVar(&budget, "latency-budget", 2*time.Second, "how long a reply may take before it counts as late")
	flags.DurationVar(&timeout, "task-timeout", 5*time.Minute, "how long one conversation may take")
	reviewConfig.bind(flags)
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if list {
		available, err := fdbench.Conditions(root)
		if err != nil {
			return err
		}
		for _, condition := range available {
			count, _ := fdbench.Count(root, []string{condition})
			fmt.Fprintf(output, "%-50s %d conversations\n", condition, count)
		}
		return nil
	}
	var selected []string
	for _, name := range strings.Split(conditions, ",") {
		if strings.TrimSpace(name) != "" {
			selected = append(selected, strings.TrimSpace(name))
		}
	}
	if len(selected) == 0 {
		return errors.New("-conditions is required: the dataset's partitions measure different things and do not merge")
	}
	cell, err := resolveCell(cellName, referenceLevels, varyFactor, varyLevel)
	if err != nil {
		return err
	}
	if err := attachBenchmarkExecution(&cell, executionPath); err != nil {
		return err
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		cell.Execution, inspectionGraph, endpoint, tokenEnv, os.Getenv,
	)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reviewResources, err := openCandidateReviewCLI(
		ctx, reviewConfig, deploymentToken, endpoint, os.LookupEnv,
	)
	if err != nil {
		return err
	}
	runOptions := fdbench.Options{
		Root: root, Conditions: selected, Endpoint: endpoint, Token: deploymentToken,
		Model: model, Cell: cell, Limit: limit, LatencyBudget: budget, Timeout: timeout,
		RuntimeAttestor: attestor,
		Progress:        func(line string) { fmt.Fprintln(output, line) },
	}
	if reviewResources != nil {
		runOptions.Evidence, runOptions.EvidenceOrigin = reviewResources.bundle, reviewResources.origin
	}
	result, runErr := fdbench.Run(ctx, runOptions)
	reviewErr := finishCandidateReviewIfConfigured(ctx, reviewResources, output)
	if runErr != nil || reviewErr != nil {
		return errors.Join(runErr, reviewErr)
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "provenance : %s\n", result.Provenance)
	fmt.Fprintf(output, "tasks      : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	fmt.Fprintf(output, "clean      : %d of %d conversations answered every turn without interrupting\n",
		result.Summary.Passed, result.Summary.Completed)
	for _, name := range []string{
		"response_latency_ms", "response_latency_p95_ms",
		"missed_turns", "premature_turns", "overrun_turns", "overlap_ms",
	} {
		distribution, present := result.Summary.Distributions[name]
		if !present {
			continue
		}
		fmt.Fprintf(output, "  %-24s p50 %-8s p95 %-8s max %s\n", name,
			distribution.Format(distribution.P50),
			distribution.Format(distribution.P95),
			distribution.Format(distribution.Max))
	}
	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	} else {
		fmt.Fprintln(output, "\nreportable")
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "written to %s\n", out)
	}
	return nil
}

func runFDBv3(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench fdbv3", flag.ContinueOnError)
	var reviewConfig candidateReviewCLIConfig
	var (
		root            string
		endpoint        string
		tokenEnv        string
		model           string
		out             string
		limit           int
		taskIDs         string
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		executionPath   string
		inspectionGraph string
		timeout         time.Duration
	)
	flags.StringVar(&root, "dataset", ".runtime/full-duplex-bench-v3/dataset/fdb_v3_data_released", "FDB v3 dataset root")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.IntVar(&limit, "limit", 0, "stop after this many recordings")
	flags.StringVar(&taskIDs, "tasks", "", "comma-separated exact task IDs for a focused diagnostic")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides describing what this deployment actually runs")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.StringVar(&executionPath, "execution", "", benchmarkExecutionFlagHelp)
	flags.StringVar(&inspectionGraph, "inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	flags.DurationVar(&timeout, "task-timeout", 3*time.Minute, "how long one recording may take")
	reviewConfig.bind(flags)
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cell, err := resolveCell(cellName, referenceLevels, varyFactor, varyLevel)
	if err != nil {
		return err
	}
	if err := attachBenchmarkExecution(&cell, executionPath); err != nil {
		return err
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		cell.Execution, inspectionGraph, endpoint, tokenEnv, os.Getenv,
	)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reviewResources, err := openCandidateReviewCLI(
		ctx, reviewConfig, deploymentToken, endpoint, os.LookupEnv,
	)
	if err != nil {
		return err
	}
	runOptions := fdbv3.Options{
		Root: root, Endpoint: endpoint, Token: deploymentToken, Model: model,
		Cell: cell, Limit: limit, Timeout: timeout,
		RuntimeAttestor: attestor,
		Progress:        func(line string) { fmt.Fprintln(output, line) },
	}
	if strings.TrimSpace(taskIDs) != "" {
		for _, taskID := range strings.Split(taskIDs, ",") {
			taskID = strings.TrimSpace(taskID)
			if taskID == "" {
				return errors.New("FDB v3 focused task list contains an empty identity")
			}
			runOptions.TaskIDs = append(runOptions.TaskIDs, taskID)
		}
	}
	if reviewResources != nil {
		runOptions.Evidence, runOptions.EvidenceOrigin = reviewResources.bundle, reviewResources.origin
	}
	result, runErr := fdbv3.Run(ctx, runOptions)
	reviewErr := finishCandidateReviewIfConfigured(ctx, reviewResources, output)
	if runErr != nil || reviewErr != nil {
		return errors.Join(runErr, reviewErr)
	}
	breakdown := fdbv3.Summarise(result)

	fmt.Fprintln(output)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "provenance : %s\n", result.Provenance)
	fmt.Fprintf(output, "tasks      : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	fmt.Fprintf(output, "  right tool and arguments : %d\n", breakdown.CalledWithRightArguments)
	fmt.Fprintf(output, "  right tools, wrong args  : %d\n", breakdown.SpellingFailures)
	fmt.Fprintf(output, "  wrong tool selection     : %d\n", breakdown.WrongTool)
	fmt.Fprintf(output, "  missing expected calls   : %d\n", breakdown.MissingCalls)
	fmt.Fprintf(output, "  no tool calls            : %d\n", breakdown.NoCall)
	fmt.Fprintf(output, "  extra tool calls         : %d\n", breakdown.ExtraCalls)
	fmt.Fprintf(output, "  failed simulator calls   : %d\n", breakdown.FailedCalls)
	fmt.Fprintf(output, "  invalid score evidence   : %d\n", breakdown.InvalidScore)
	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	} else {
		fmt.Fprintln(output, "\nreportable")
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "written to %s\n", out)
	}
	return nil
}

// runDynaCU runs DynaCU-Bench against a running server.
//
// The benchmark stays in the AOI repository and this points it at an endpoint.
// Nothing about what a task is, or whether it passed, is decided here.
func runDynaCU(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench dynacu", flag.ContinueOnError)
	var (
		endpoint        string
		tokenEnv        string
		model           string
		aoiDir          string
		out             string
		category        string
		difficulty      string
		taskIDs         string
		limit           int
		maxSteps        int
		stepInterval    time.Duration
		withoutImages   bool
		withoutList     bool
		resume          bool
		python          string
		timeout         time.Duration
		verify          bool
		cellName        string
		referenceLevels string
		vary            string
		level           string
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "openrealtime", "model to request")
	flags.StringVar(&aoiDir, "aoi-dir", defaultAOIDir(), "prepared AOI checkout; see scripts/prepare-dynacu.sh")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&category, "category", "", "restrict to one category, e.g. A_podcast or S_static")
	flags.StringVar(&difficulty, "difficulty", "", "restrict to easy, medium, or hard")
	flags.StringVar(&taskIDs, "tasks", "", "comma-separated task IDs, for reproducing one row")
	flags.IntVar(&limit, "limit", 0, "cap the task count; any cap makes the cell incomplete")
	flags.IntVar(&maxSteps, "max-steps", 15, "steps one task's agent loop may take")
	flags.DurationVar(&stepInterval, "step-interval", 2*time.Second, "how long the agent observes between actions")
	flags.BoolVar(&withoutImages, "no-images", false, "withhold screenshots, leaving audio and the element list")
	flags.BoolVar(&withoutList, "no-page-elements", false, "withhold the interactive-element list")
	flags.BoolVar(&resume, "resume", false, "continue an interrupted run rather than starting again")
	flags.StringVar(&python, "python", "", "interpreter with the AOI dependencies; empty prefers the checkout's own")
	flags.DurationVar(&timeout, "timeout", 6*time.Hour, "bound on the whole run")
	flags.BoolVar(&verify, "verify", false, "check the environment and exit without running")
	flags.StringVar(&cellName, "cell", "reference", "name of the measured cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides describing what this deployment actually runs")
	flags.StringVar(&vary, "vary", "", "factor this cell varies from the reference")
	flags.StringVar(&level, "level", "", "level of the varied factor")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	cell, err := resolveCell(cellName, referenceLevels, vary, level)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config := dynacu.Config{
		AOIDir: aoiDir, Endpoint: endpoint, Model: model, TokenEnv: tokenEnv,
		Category: category, Difficulty: difficulty, Limit: limit,
		MaxSteps: maxSteps, StepInterval: stepInterval,
		WithoutImages: withoutImages, WithoutPageElements: withoutList,
		Resume: resume, Python: python, Timeout: timeout, Cell: cell,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(output, format+"\n", args...)
		},
	}
	if trimmed := strings.TrimSpace(taskIDs); trimmed != "" {
		config.TaskIDs = strings.Split(trimmed, ",")
	}

	if verify {
		// Refusing in seconds beats refusing after six hours of browser
		// automation, which is the whole reason this exists as its own flag.
		if err := config.Verify(ctx); err != nil {
			return err
		}
		fmt.Fprintf(output, "dynacu: ready at revision %s\n", dynacu.PinnedRevision)
		return nil
	}

	result, runErr := dynacu.Run(ctx, config)
	if runErr != nil && len(result.Tasks) == 0 {
		return runErr
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "suite      : %s (%d tasks declared)\n", result.Suite, result.Expected)
	fmt.Fprintf(output, "completed  : %d\n", result.Summary.Completed)
	fmt.Fprintf(output, "invalid    : %d\n", result.Summary.Failed)
	fmt.Fprintf(output, "passed     : %d\n", result.Summary.Passed)
	if result.Summary.Complete {
		fmt.Fprintf(output, "pass rate  : %.3f\n", result.Summary.PassRate)
	} else {
		fmt.Fprintf(output, "incomplete : %s\n", result.Summary.Incompleteness)
	}
	fmt.Fprintln(output, "\nby category:")
	breakdown := dynacu.Breakdown(result)
	for _, category := range dynacu.Categories {
		summary, present := breakdown[category]
		if !present {
			continue
		}
		fmt.Fprintf(output, "  %-12s %2d/%2d passed  (%d invalid)\n",
			category, summary.Passed, summary.Completed, summary.Invalid)
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "\nwritten to %s\n", out)
	}
	return runErr
}

// defaultAOIDir is where scripts/prepare-dynacu.sh puts the checkout.
func defaultAOIDir() string {
	return filepath.Join(".runtime", "dynacu-bench", "aoi")
}

// runTauVoice runs the tau-Voice suite against a running endpoint.
//
// The environment is tau2-bench's, pinned and prepared by
// scripts/prepare-tau-voice.sh. This command points it at OpenRealtime, waits
// - a full cell is hours, not minutes - and turns what comes back into the
// same report every other suite produces.
func runTauVoice(arguments []string, output io.Writer) error {
	if len(arguments) > 0 && strings.EqualFold(strings.TrimSpace(arguments[0]), "inventory") {
		return runTauVoiceInventory(arguments[1:], output)
	}
	flags := flag.NewFlagSet("openrealtime bench tau-voice", flag.ContinueOnError)
	var reviewConfig candidateReviewCLIConfig
	var (
		tau2Dir         string
		endpoint        string
		model           string
		domain          string
		condition       string
		userModel       string
		python          string
		tokenEnv        string
		agentVoice      string
		agentASR        string
		synthesis       string
		ttsURL          string
		ttsModel        string
		ttsVoice        string
		userURL         string
		thinking        bool
		halluRetry      int
		runPrefix       string
		out             string
		trials          int
		seed            int
		maxConcurrency  int
		workers         int
		limit           int
		cadence         float64
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		executionPath   string
		inspectionGraph string
		timeout         time.Duration
		verifyOnly      bool
		metrics         bool
	)
	flags.StringVar(&tau2Dir, "tau2", ".runtime/tau2-bench", "prepared tau2-bench checkout")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&domain, "domain", "", "restrict to one domain (airline, retail, telecom)")
	flags.StringVar(&condition, "condition", "control", "speech condition: control, regular, or an ablation")
	flags.StringVar(&userModel, "user-model", "gpt-4.1", "model behind the simulated caller")
	flags.StringVar(&userURL, "user-model-url", "",
		"OpenAI-compatible endpoint for the caller's model; set it to run fully locally")
	flags.BoolVar(&thinking, "user-model-thinking", false,
		"leave a reasoning caller's thinking mode on")
	flags.IntVar(&halluRetry, "hallucination-retries", 0,
		"tau2 re-rolls when it judges the caller to have hallucinated; the check calls a model")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN",
		"environment variable holding the bearer token the agent presents")
	flags.StringVar(&agentVoice, "agent-voice", "default",
		"fixed voice identity used by the OpenRealtime endpoint")
	flags.StringVar(&agentASR, "agent-transcription-model", "Qwen/Qwen3-ASR-0.6B",
		"transcription model identity used by the OpenRealtime endpoint")
	flags.StringVar(&synthesis, "synthesis", "fish_audio",
		"voice for the simulated caller: fish_audio (local) or elevenlabs")
	flags.StringVar(&ttsURL, "synthesis-url", "", "local speech endpoint for the caller's voice")
	flags.StringVar(&ttsModel, "synthesis-model", "", "speech model for the caller's voice")
	flags.StringVar(&ttsVoice, "synthesis-voice", "", "voice identity for the simulated caller")
	flags.StringVar(&python, "python", "",
		"interpreter with tau2 installed; empty prefers the checkout's own .venv")
	flags.StringVar(&runPrefix, "run-prefix", "openrealtime", "names the tau2 runs, and resumes one that exists")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.IntVar(&trials, "trials", 1, "repeat each task this many times")
	flags.IntVar(&seed, "seed", tauvoice.DefaultSeed, "fixed upstream campaign seed")
	flags.IntVar(&maxConcurrency, "max-concurrency", tauvoice.DefaultMaxConcurrency,
		"maximum in-flight simulations per worker")
	flags.IntVar(&workers, "workers", tauvoice.DefaultWorkers,
		"upstream worker processes; zero uses the pinned in-process scheduler")
	flags.IntVar(&limit, "limit", 0, "stop after this many tasks per domain")
	flags.Float64Var(&cadence, "cadence", 0.2, "trigger cadence in seconds, matching factor F4")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "",
		"comma-separated factor=level overrides held fixed across a pair")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.StringVar(&executionPath, "execution", "", benchmarkExecutionFlagHelp)
	flags.StringVar(&inspectionGraph, "inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	flags.DurationVar(&timeout, "task-timeout", 10*time.Minute, "how long one simulation may take")
	flags.BoolVar(&verifyOnly, "verify", false, "check the environment and exit without running")
	flags.BoolVar(&metrics, "interaction-metrics", true, "also compute tau2's turn-taking metrics")
	reviewConfig.bind(flags)
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("tau-voice accepts flags only; use `tau-voice inventory` to export the pinned task universe")
	}
	cell, err := resolveCellFrom(bench.Reference(), cellName, referenceLevels, varyFactor, varyLevel)
	if err != nil {
		return err
	}
	if err := attachBenchmarkExecution(&cell, executionPath); err != nil {
		return err
	}
	speech, err := tauvoice.ParseCondition(condition)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var executionEvidence *bench.ExecutionEvidence
	if cell.Execution.Required() {
		executionEvidence, err = captureExternalExecutionEvidence(
			ctx, cell.Execution, inspectionGraph, endpoint, tokenEnv, model, os.Getenv,
		)
		if err != nil {
			return err
		}
	}

	config := tauvoice.Config{
		Tau2Dir: tau2Dir, Endpoint: endpoint, Model: model, Domain: domain,
		Condition: speech, Trials: trials, Seed: seed,
		MaxConcurrency: maxConcurrency, Workers: workers, Limit: limit, UserModel: userModel,
		UserModelURL: userURL, UserModelThinking: thinking, HallucinationRetries: halluRetry,
		Cadence: cadence, Timeout: timeout, Cell: cell, Python: python, TokenEnv: tokenEnv,
		ExecutionEvidence: executionEvidence,
		AgentVoice:        agentVoice, AgentTranscriptionModel: agentASR,
		SynthesisProvider: synthesis, SynthesisEndpoint: ttsURL,
		SynthesisModel: ttsModel, SynthesisVoice: ttsVoice,
		RunPrefix: runPrefix,
		Logf:      func(format string, args ...any) { fmt.Fprintf(output, format+"\n", args...) },
	}
	if verifyOnly {
		if strings.TrimSpace(reviewConfig.Prefix) != "" || reviewConfig.Resume {
			return errors.New("tau-voice -verify does not execute candidate attempts and cannot use candidate review options")
		}
		if err := config.Verify(ctx); err != nil {
			return err
		}
		fmt.Fprintf(output, "tau2-bench at %s is prepared and pinned to %s\n",
			tau2Dir, tauvoice.PinnedRevision)
		return nil
	}

	reviewResources, err := openCandidateReviewCLI(
		ctx, reviewConfig, os.Getenv(tokenEnv), endpoint, os.LookupEnv,
	)
	if err != nil {
		return err
	}
	if reviewResources != nil {
		config.Evidence, config.EvidenceOrigin = reviewResources.bundle, reviewResources.origin
	}
	result, runErr := tauvoice.Run(ctx, config)
	reviewErr := finishCandidateReviewIfConfigured(ctx, reviewResources, output)
	if runErr != nil || reviewErr != nil {
		return errors.Join(runErr, reviewErr)
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "condition  : %s\n", speech)
	fmt.Fprintf(output, "provenance : %s\n", result.Provenance)
	fmt.Fprintf(output, "tasks      : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	fmt.Fprintf(output, "pass^1     : %.1f%% (%d passed)\n",
		result.Summary.PassRate*100, result.Summary.Passed)

	if metrics {
		// Turn-taking is the half of tau-Voice that task success cannot see: a
		// system can pass every task while talking over the caller throughout.
		domains := tauvoice.Domains
		if strings.TrimSpace(domain) != "" {
			domains = []string{domain}
		}
		for _, name := range domains {
			runName := fmt.Sprintf("%s-%s-%s", runPrefix, name, speech)
			computed, err := tauvoice.InteractionMetrics(ctx, config, runName)
			if err != nil {
				fmt.Fprintf(output, "interaction metrics for %s unavailable: %v\n", name, err)
				continue
			}
			fmt.Fprintf(output, "\ninteraction metrics (%s):\n", name)
			names := make([]string, 0, len(computed))
			for metric := range computed {
				names = append(names, metric)
			}
			sort.Strings(names)
			for _, metric := range names {
				fmt.Fprintf(output, "  %-44s %.3f\n", metric, computed[metric])
			}
		}
	}

	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	} else {
		fmt.Fprintln(output, "\nreportable")
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "written to %s\n", out)
	}
	return nil
}

// runTauVoiceInventory exports the exact upstream base-split task universe
// consumed by the direct candidate run. It is intentionally a subcommand of
// the benchmark harness: the server and presentation layers do not own, cache,
// or reinterpret upstream task identities.
func runTauVoiceInventory(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench tau-voice inventory", flag.ContinueOnError)
	var tau2Dir, python, destination string
	flags.StringVar(&tau2Dir, "tau2", ".runtime/tau2-bench", "prepared tau2-bench checkout")
	flags.StringVar(&python, "python", "", "interpreter with tau2 installed; empty prefers the checkout's own .venv")
	flags.StringVar(&destination, "out", "", "write canonical inventory JSON to this path; empty writes stdout")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("tau-voice inventory accepts flags only")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	inventory, err := tauvoice.LoadTaskInventory(ctx, tau2Dir, python)
	if err != nil {
		return err
	}
	payload, err := tauvoice.MarshalTaskInventory(inventory)
	if err != nil {
		return err
	}
	return writeOrPrint(destination, payload, output)
}
