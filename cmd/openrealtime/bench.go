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
	"github.com/bojieli/OpenRealtime/bench/dynacu"
	"github.com/bojieli/OpenRealtime/bench/fdb"
	"github.com/bojieli/OpenRealtime/bench/fdbench"
	"github.com/bojieli/OpenRealtime/bench/fdbv3"
	"github.com/bojieli/OpenRealtime/bench/tauvoice"
)

// runBench executes one suite against a running server.
//
// It drives the server over the protocol rather than constructing a session
// in-process, and deliberately: a measurement of something other than what
// users get is not a measurement of anything.
func runBench(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime bench <fdb|fdbv3|fdbench|tau-voice|dynacu> [flags]")
	}
	suite := strings.ToLower(strings.TrimSpace(arguments[0]))
	switch suite {
	case "fdb", "fdb-v1.5":
		return runFDB(arguments[1:], output)
	case "fdbench", "fd-bench":
		return runFDBench(arguments[1:], output)
	case "fdbv3", "fdb-v3":
		return runFDBv3(arguments[1:], output)
	case "tau-voice", "tauvoice", "tau":
		return runTauVoice(arguments[1:], output)
	case "dynacu", "computer-use":
		return runDynaCU(arguments[1:], output)
	default:
		return fmt.Errorf("unknown suite %q", suite)
	}
}

func runFDB(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench fdb", flag.ContinueOnError)
	var (
		root       string
		endpoint   string
		tokenEnv   string
		model      string
		out        string
		categories string
		limit      int
		cellName   string
		varyFactor string
		varyLevel  string
		timeout    time.Duration
	)
	flags.StringVar(&root, "dataset", ".runtime/full-duplex-bench-v1.5/dataset", "FDB v1.5 dataset root")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&categories, "categories", "", "comma-separated categories; empty runs all four")
	flags.IntVar(&limit, "limit", 0, "stop after this many recordings; a limited run is never a complete cell")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies from the reference, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.DurationVar(&timeout, "task-timeout", 3*time.Minute, "how long one recording may take")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	cell, err := resolveCell(cellName, varyFactor, varyLevel)
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
	result, err := fdb.Run(ctx, fdb.Options{
		Root: root, Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model,
		Cell: cell, Categories: wanted, Limit: limit, Timeout: timeout,
		Progress: func(line string) { fmt.Fprintln(output, line) },
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "provenance : %s\n", result.Provenance)
	fmt.Fprintf(output, "tasks      : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	for _, category := range fdb.Categories() {
		summary, present := fdb.Breakdown(result)[category]
		if !present {
			continue
		}
		want := "hold"
		if category.ShouldYield() {
			want = "yield"
		}
		fmt.Fprintf(output, "  %-18s %s  %d/%d applicable  %.0f%%  (%d not applicable)\n",
			category, want, summary.Passed, summary.Applicable, summary.Rate*100, summary.NotApplicable)
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

// resolveCell builds the cell this run measures.
func resolveCell(name, factor, level string) (bench.Cell, error) {
	if strings.TrimSpace(factor) == "" {
		cell := bench.Reference()
		if strings.TrimSpace(name) != "" {
			cell.Name = name
		}
		return cell, nil
	}
	cell, err := bench.Vary(bench.Factor(strings.ToUpper(factor)), level)
	if err != nil {
		return bench.Cell{}, err
	}
	if strings.TrimSpace(name) != "" && name != "reference" {
		cell.Name = name
	}
	return cell, nil
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
	var (
		root       string
		endpoint   string
		tokenEnv   string
		model      string
		out        string
		conditions string
		list       bool
		limit      int
		cellName   string
		varyFactor string
		varyLevel  string
		budget     time.Duration
		timeout    time.Duration
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
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F4")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.DurationVar(&budget, "latency-budget", 2*time.Second, "how long a reply may take before it counts as late")
	flags.DurationVar(&timeout, "task-timeout", 5*time.Minute, "how long one conversation may take")
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
	cell, err := resolveCell(cellName, varyFactor, varyLevel)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := fdbench.Run(ctx, fdbench.Options{
		Root: root, Conditions: selected, Endpoint: endpoint, Token: os.Getenv(tokenEnv),
		Model: model, Cell: cell, Limit: limit, LatencyBudget: budget, Timeout: timeout,
		Progress: func(line string) { fmt.Fprintln(output, line) },
	})
	if err != nil {
		return err
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
		fmt.Fprintf(output, "  %-24s p50 %.0f  p95 %.0f  max %.0f\n",
			name, distribution.P50, distribution.P95, distribution.Max)
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
	var (
		root       string
		endpoint   string
		tokenEnv   string
		model      string
		out        string
		limit      int
		cellName   string
		varyFactor string
		varyLevel  string
		timeout    time.Duration
	)
	flags.StringVar(&root, "dataset", ".runtime/full-duplex-bench-v3/dataset/fdb_v3_data_released", "FDB v3 dataset root")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.IntVar(&limit, "limit", 0, "stop after this many recordings")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.DurationVar(&timeout, "task-timeout", 3*time.Minute, "how long one recording may take")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cell, err := resolveCell(cellName, varyFactor, varyLevel)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := fdbv3.Run(ctx, fdbv3.Options{
		Root: root, Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model,
		Cell: cell, Limit: limit, Timeout: timeout,
		Progress: func(line string) { fmt.Fprintln(output, line) },
	})
	if err != nil {
		return err
	}
	breakdown := fdbv3.Summarise(result)

	fmt.Fprintln(output)
	fmt.Fprintf(output, "cell       : %s\n", result.Cell.Describe())
	fmt.Fprintf(output, "provenance : %s\n", result.Provenance)
	fmt.Fprintf(output, "tasks      : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	fmt.Fprintf(output, "  right tool and arguments : %d\n", breakdown.CalledWithRightArguments)
	fmt.Fprintf(output, "  right tool, wrong value  : %d  (identifier reassembly)\n", breakdown.SpellingFailures)
	fmt.Fprintf(output, "  no matching call         : %d\n", breakdown.NoCall)
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

// runDynaCU is the computer-use functional release gate.
//
// It answers one question - does video observation and action grounding work
// end to end, over the protocol, with nothing faked - and it answers it in
// seconds without a dataset, a GPU, or a network.
func runDynaCU(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench dynacu", flag.ContinueOnError)
	var (
		endpoint    string
		tokenEnv    string
		model       string
		out         string
		instruction string
		interval    time.Duration
		timeout     time.Duration
		verbose     bool
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&instruction, "instruction", "", "what the user asks for; empty uses the default")
	flags.DurationVar(&interval, "frame-interval", 350*time.Millisecond, "how often a frame is sent")
	flags.DurationVar(&timeout, "timeout", 90*time.Second, "how long the attempt may take")
	flags.BoolVar(&verbose, "verbose", true, "print what the agent observed and did")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	progress := func(string) {}
	if verbose {
		progress = func(line string) { fmt.Fprintln(output, line) }
	}
	outcome, runErr := dynacu.Run(ctx, dynacu.Options{
		Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model,
		Instruction: instruction, FrameInterval: interval, Timeout: timeout,
		Progress: progress,
	})
	result := dynacu.AsResult(bench.Reference(), outcome, runErr)

	fmt.Fprintln(output)
	fmt.Fprintf(output, "negotiated video and computer use : %v\n", outcome.Negotiated)
	fmt.Fprintf(output, "observed the screen               : %v\n", outcome.Observed)
	if outcome.ObservedText != "" {
		fmt.Fprintf(output, "  %q\n", outcome.ObservedText)
	}
	fmt.Fprintf(output, "acted                             : %v\n", outcome.Acted)
	fmt.Fprintf(output, "grounded the action               : %v (%s)\n", outcome.Grounded, outcome.FinalState)
	if outcome.MissDistance > 0 {
		fmt.Fprintf(output, "  closest click was %.0f px from the target\n", outcome.MissDistance)
	}
	if outcome.Failure != "" {
		fmt.Fprintf(output, "failure                           : %s\n", outcome.Failure)
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}
	if !outcome.Passed() {
		return errors.New("the computer-use gate did not pass")
	}
	fmt.Fprintln(output, "\ngate passed")
	return nil
}

// runTauVoice runs the tau-Voice suite against a running endpoint.
//
// The environment is tau2-bench's, pinned and prepared by
// scripts/prepare-tau-voice.sh. This command points it at OpenRealtime, waits
// - a full cell is hours, not minutes - and turns what comes back into the
// same report every other suite produces.
func runTauVoice(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench tau-voice", flag.ContinueOnError)
	var (
		tau2Dir    string
		endpoint   string
		model      string
		domain     string
		condition  string
		userModel  string
		python     string
		tokenEnv   string
		synthesis  string
		ttsURL     string
		ttsModel   string
		ttsVoice   string
		runPrefix  string
		out        string
		trials     int
		limit      int
		cadence    float64
		cellName   string
		varyFactor string
		varyLevel  string
		timeout    time.Duration
		verifyOnly bool
		metrics    bool
	)
	flags.StringVar(&tau2Dir, "tau2", ".runtime/tau2-bench", "prepared tau2-bench checkout")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&domain, "domain", "", "restrict to one domain (airline, retail, telecom)")
	flags.StringVar(&condition, "condition", "control", "speech condition: control, regular, or an ablation")
	flags.StringVar(&userModel, "user-model", "gpt-4.1", "model behind the simulated caller")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN",
		"environment variable holding the bearer token the agent presents")
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
	flags.IntVar(&limit, "limit", 0, "stop after this many tasks per domain")
	flags.Float64Var(&cadence, "cadence", 0.2, "trigger cadence in seconds, matching factor F4")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.DurationVar(&timeout, "task-timeout", 10*time.Minute, "how long one simulation may take")
	flags.BoolVar(&verifyOnly, "verify", false, "check the environment and exit without running")
	flags.BoolVar(&metrics, "interaction-metrics", true, "also compute tau2's turn-taking metrics")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cell, err := resolveCell(cellName, varyFactor, varyLevel)
	if err != nil {
		return err
	}
	speech, err := tauvoice.ParseCondition(condition)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config := tauvoice.Config{
		Tau2Dir: tau2Dir, Endpoint: endpoint, Model: model, Domain: domain,
		Condition: speech, Trials: trials, Limit: limit, UserModel: userModel,
		Cadence: cadence, Timeout: timeout, Cell: cell, Python: python, TokenEnv: tokenEnv,
		SynthesisProvider: synthesis, SynthesisEndpoint: ttsURL,
		SynthesisModel: ttsModel, SynthesisVoice: ttsVoice,
		RunPrefix: runPrefix,
		Logf:      func(format string, args ...any) { fmt.Fprintf(output, format+"\n", args...) },
	}
	if verifyOnly {
		if err := config.Verify(ctx); err != nil {
			return err
		}
		fmt.Fprintf(output, "tau2-bench at %s is prepared and pinned to %s\n",
			tau2Dir, tauvoice.PinnedRevision)
		return nil
	}

	result, err := tauvoice.Run(ctx, config)
	if err != nil {
		return err
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
