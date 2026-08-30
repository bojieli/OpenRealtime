package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type options struct {
	baselinePath   string
	benchmarkPath  string
	outputPath     string
	goVersion      string
	goos           string
	goarch         string
	cpu            string
	gomaxprocs     int
	affinity       string
	benchtime      string
	warmups        int
	sampleCount    int
	sourceRevision string
	sourceModified bool
	sourceSHA256   string
	sourceFiles    string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("compare-fdbench6147-performance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var input options
	flags.StringVar(&input.baselinePath, "baseline", "", "checked baseline JSON")
	flags.StringVar(&input.benchmarkPath, "benchmark-output", "", "raw go benchmark output")
	flags.StringVar(&input.outputPath, "output", "", "create-only candidate evidence JSON, or -")
	flags.StringVar(&input.goVersion, "go-version", "", "go toolchain version")
	flags.StringVar(&input.goos, "goos", "", "GOOS")
	flags.StringVar(&input.goarch, "goarch", "", "GOARCH")
	flags.StringVar(&input.cpu, "cpu", "", "CPU model reported by go test")
	flags.IntVar(&input.gomaxprocs, "gomaxprocs", 0, "GOMAXPROCS used by the benchmark")
	flags.StringVar(&input.affinity, "affinity", "", "requested CPU affinity")
	flags.StringVar(&input.benchtime, "benchtime", "1x", "go benchmark benchtime")
	flags.IntVar(&input.warmups, "warmups", 1, "untimed warmup count")
	flags.IntVar(&input.sampleCount, "samples", 12, "required raw sample count")
	flags.StringVar(&input.sourceRevision, "source-revision", "", "source revision")
	flags.BoolVar(&input.sourceModified, "source-modified", false, "source worktree was modified")
	flags.StringVar(&input.sourceSHA256, "source-sha256", "", "benchmark target source digest")
	flags.StringVar(&input.sourceFiles, "source-files", "", "comma-separated source digest inputs")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "baseline", value: input.baselinePath},
		{name: "benchmark-output", value: input.benchmarkPath},
		{name: "output", value: input.outputPath},
		{name: "go-version", value: input.goVersion},
		{name: "goos", value: input.goos},
		{name: "goarch", value: input.goarch},
		{name: "cpu", value: input.cpu},
		{name: "affinity", value: input.affinity},
		{name: "source-revision", value: input.sourceRevision},
		{name: "source-sha256", value: input.sourceSHA256},
		{name: "source-files", value: input.sourceFiles},
	} {
		if strings.TrimSpace(required.value) == "" {
			fmt.Fprintf(stderr, "-%s is required\n", required.name)
			return 2
		}
	}

	baselineBytes, err := os.ReadFile(input.baselinePath)
	if err != nil {
		fmt.Fprintf(stderr, "read baseline: %v\n", err)
		return 2
	}
	baseline, err := decodeEvidence(baselineBytes)
	if err != nil {
		fmt.Fprintf(stderr, "validate baseline: %v\n", err)
		return 2
	}
	if baseline.Comparison != nil {
		fmt.Fprintln(stderr, "validate baseline: checked baseline must not contain a comparison")
		return 2
	}
	raw, err := os.Open(input.benchmarkPath)
	if err != nil {
		fmt.Fprintf(stderr, "read benchmark output: %v\n", err)
		return 2
	}
	samples, parseErr := parseBenchmarkOutput(raw)
	closeErr := raw.Close()
	if parseErr != nil {
		fmt.Fprintf(stderr, "parse benchmark output: %v\n", parseErr)
		return 2
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "close benchmark output: %v\n", closeErr)
		return 2
	}
	if len(samples) != input.sampleCount {
		fmt.Fprintf(stderr, "benchmark produced %d samples, want exactly %d\n",
			len(samples), input.sampleCount)
		return 2
	}
	summary, err := summarize(samples)
	if err != nil {
		fmt.Fprintf(stderr, "summarize benchmark output: %v\n", err)
		return 2
	}
	files := strings.Split(input.sourceFiles, ",")
	for index := range files {
		files[index] = strings.TrimSpace(files[index])
	}
	sort.Strings(files)
	candidate := Evidence{
		Version: evidenceVersion, Benchmark: benchmarkName,
		Protocol: Protocol{
			GoVersion: input.goVersion, GOOS: input.goos, GOARCH: input.goarch,
			CPU: input.cpu, GOMAXPROCS: input.gomaxprocs, Affinity: input.affinity,
			Benchtime: input.benchtime, WarmupCount: input.warmups, SampleCount: input.sampleCount,
		},
		Source: Source{
			Revision: input.sourceRevision, Modified: input.sourceModified,
			SHA256: input.sourceSHA256, Files: files,
		},
		Samples: samples, Summary: summary,
	}
	if err := candidate.validate(); err != nil {
		fmt.Fprintf(stderr, "validate candidate evidence: %v\n", err)
		return 2
	}
	comparison := compareEvidence(baseline, candidate, baselineBytes)
	candidate.Comparison = &comparison
	payload, err := marshalEvidence(candidate)
	if err != nil {
		fmt.Fprintf(stderr, "marshal candidate evidence: %v\n", err)
		return 2
	}
	if err := writeEvidence(input.outputPath, payload, stdout); err != nil {
		fmt.Fprintf(stderr, "write candidate evidence: %v\n", err)
		return 2
	}
	statusWriter := stdout
	if input.outputPath == "-" {
		statusWriter = stderr
	}
	fmt.Fprintf(statusWriter,
		"reportable=%t accepted=%t artifact=%s allocations=%s timing=%s output=%s\n",
		comparison.Reportable, comparison.Accepted, comparison.Artifact.Status,
		comparison.Allocations.Status, comparison.Timing.Status, input.outputPath)
	if !comparison.Accepted {
		return 1
	}
	return 0
}
