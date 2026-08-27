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
	"github.com/bojieli/OpenRealtime/bench/realtimecu"
	"github.com/bojieli/OpenRealtime/bench/tauvoice"
)

// runBench executes one suite against a running server.
//
// It drives the server over the protocol rather than constructing a session
// in-process, and deliberately: a measurement of something other than what
// users get is not a measurement of anything.
func runBench(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime bench <architecture|realtime-cu|fdb|fdbv3|fdbench|tau-voice|dynacu> [flags]")
	}
	suite := strings.ToLower(strings.TrimSpace(arguments[0]))
	switch suite {
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
	case "dynacu":
		return runDynaCU(arguments[1:], output)
	default:
		return fmt.Errorf("unknown suite %q", suite)
	}
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
	transcript, err := bench.PlaySamples(ctx, bench.SessionConfig{
		Endpoint: *endpoint, Token: os.Getenv(*tokenEnv), Model: *model,
		Timeout: *timeout, TrailingSilence: time.Millisecond,
		CaptureRuntimeEvidence: true, Quiet: true,
	}, nil)
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
	if err := archbench.WriteCell(*out, cell); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s  F52=%s  definition=%s\n", *out, cell.Architecture.Level, definition.Ref())
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
		groundings      string
		categories      string
		limit           int
		fps             int
		timeout         time.Duration
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
		list            bool
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "openrealtime", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&browser, "browser", "", "Chromium executable; empty discovers it")
	flags.StringVar(&groundings, "grounding", "pixel,set_of_mark", "comma-separated grounding conditions")
	flags.StringVar(&categories, "categories", "", "comma-separated task categories; empty runs all")
	flags.IntVar(&limit, "limit", 0, "stop after this many cases; a limited run is incomplete")
	flags.IntVar(&fps, "fps", 3, "screen and camera capture rate")
	flags.DurationVar(&timeout, "task-timeout", 45*time.Second, "bound one browser task")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "", "comma-separated factor=level overrides held fixed across a pair")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.BoolVar(&list, "list", false, "list repository-owned tasks and stop")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if list {
		for _, task := range realtimecu.Suite() {
			fmt.Fprintf(output, "%-30s %-18s %-6s %s\n", task.ID, task.Category, task.Difficulty, axes(task.Axes))
		}
		return nil
	}
	cell, err := resolveRealtimeCUCell(cellName, referenceLevels, varyFactor, varyLevel)
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
	result, err := realtimecu.Run(ctx, realtimecu.Options{
		Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model,
		Cell: cell, Browser: browser, Groundings: selectedGroundings,
		Categories: selectedCategories, Limit: limit, FrameRate: fps, Timeout: timeout,
		Progress: func(line string) { fmt.Fprintln(output, line) },
	})
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
		fmt.Fprintf(output, "  %-12s correct %d/%d (%.1f%%), correct within deadline %d/%d (%.1f%%)\n",
			grounding, reading.Correct, reading.Cases, reading.CorrectRate*100,
			reading.Timely, reading.Cases, reading.TimelyRate*100)
	}
	for _, metric := range []string{
		"cue_to_action_latency_ms", "frame_to_observation_latency_ms",
		"cue_to_observation_latency_ms", "action_execution_ms",
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

// runDynaCU runs DynaCU-Bench against a running server.
//
// The benchmark stays in the AOI repository and this points it at an endpoint.
// Nothing about what a task is, or whether it passed, is decided here.
func runDynaCU(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench dynacu", flag.ContinueOnError)
	var (
		endpoint      string
		tokenEnv      string
		model         string
		aoiDir        string
		out           string
		category      string
		difficulty    string
		taskIDs       string
		limit         int
		maxSteps      int
		stepInterval  time.Duration
		withoutImages bool
		withoutList   bool
		resume        bool
		python        string
		timeout       time.Duration
		verify        bool
		cellName      string
		vary          string
		level         string
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
	flags.StringVar(&vary, "vary", "", "factor this cell varies from the reference")
	flags.StringVar(&level, "level", "", "level of the varied factor")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	cell, err := resolveCell(cellName, vary, level)
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
	flags := flag.NewFlagSet("openrealtime bench tau-voice", flag.ContinueOnError)
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
		limit           int
		cadence         float64
		cellName        string
		referenceLevels string
		varyFactor      string
		varyLevel       string
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
	flags.IntVar(&limit, "limit", 0, "stop after this many tasks per domain")
	flags.Float64Var(&cadence, "cadence", 0.2, "trigger cadence in seconds, matching factor F4")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.StringVar(&referenceLevels, "reference-levels", "",
		"comma-separated factor=level overrides held fixed across a pair")
	flags.StringVar(&varyFactor, "vary", "", "factor this cell varies, such as F2")
	flags.StringVar(&varyLevel, "level", "", "the level it varies to")
	flags.DurationVar(&timeout, "task-timeout", 10*time.Minute, "how long one simulation may take")
	flags.BoolVar(&verifyOnly, "verify", false, "check the environment and exit without running")
	flags.BoolVar(&metrics, "interaction-metrics", true, "also compute tau2's turn-taking metrics")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cell, err := resolveCellFrom(bench.Reference(), cellName, referenceLevels, varyFactor, varyLevel)
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
		UserModelURL: userURL, UserModelThinking: thinking, HallucinationRetries: halluRetry,
		Cadence: cadence, Timeout: timeout, Cell: cell, Python: python, TokenEnv: tokenEnv,
		AgentVoice: agentVoice, AgentTranscriptionModel: agentASR,
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
