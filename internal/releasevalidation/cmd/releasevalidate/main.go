package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bojieli/OpenRealtime/internal/releasevalidation"
)

type gateFlags []string

func (values *gateFlags) String() string { return strings.Join(*values, ",") }

func (values *gateFlags) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("gate ID is empty")
	}
	*values = append(*values, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("releasevalidate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var selected gateFlags
	matrixPath := flags.String("matrix", "scripts/release-matrix.json", "checked release matrix")
	mode := flags.String("mode", "run", "validate, plan, or run")
	scope := flags.String("scope", "local", "local or all")
	includeOptIn := flags.Bool("include-opt-in", false, "include opt-in local gates such as performance")
	flags.Var(&selected, "gate", "run one exact gate ID; repeat for more")
	artifacts := flags.String("artifacts", "", "new directory for command logs and artifacts")
	reportPath := flags.String("report", "-", "create-only JSON report path, or - for stdout")
	root := flags.String("root", ".", "repository root")
	goBinary := flags.String("go", "", "Go 1.25+ binary; empty uses repository resolution")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "releasevalidate accepts flags only")
		return 2
	}
	matrix, err := releasevalidation.Load(*matrixPath)
	if err != nil {
		fmt.Fprintf(stderr, "release matrix invalid: %v\n", err)
		return 2
	}
	if *mode == "validate" {
		matrixDigest, digestErr := matrix.Digest()
		if digestErr != nil {
			fmt.Fprintf(stderr, "digest release matrix: %v\n", digestErr)
			return 2
		}
		payload, marshalErr := json.MarshalIndent(struct {
			Version           int      `json:"version"`
			MatrixVersion     int      `json:"matrix_version"`
			MatrixSHA256      string   `json:"matrix_sha256"`
			Valid             bool     `json:"valid"`
			RequiredGateCount int      `json:"required_gate_count"`
			RequiredGateIDs   []string `json:"required_gate_ids"`
		}{
			Version: 1, MatrixVersion: matrix.Version, MatrixSHA256: matrixDigest, Valid: true,
			RequiredGateCount: len(releasevalidation.RequiredGateIDs(matrix)),
			RequiredGateIDs:   releasevalidation.RequiredGateIDs(matrix),
		}, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(stderr, "marshal validation report: %v\n", marshalErr)
			return 2
		}
		payload = append(payload, '\n')
		if err := writeCreateOnly(*reportPath, payload, stdout); err != nil {
			fmt.Fprintf(stderr, "write validation report: %v\n", err)
			return 2
		}
		return 0
	}
	if *mode != string(releasevalidation.ModePlan) && *mode != string(releasevalidation.ModeRun) {
		fmt.Fprintf(stderr, "-mode must be validate, plan, or run; got %q\n", *mode)
		return 2
	}
	resolvedRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve repository root: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := releasevalidation.Execute(ctx, matrix, releasevalidation.Options{
		Mode: releasevalidation.Mode(*mode), Scope: releasevalidation.Scope(*scope),
		GateIDs: selected, IncludeOptIn: *includeOptIn, Root: resolvedRoot,
		ArtifactsDir: *artifacts, GoBinary: *goBinary, Progress: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "release validation failed to start: %v\n", err)
		return 2
	}
	payload, err := releasevalidation.MarshalReport(report)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := writeCreateOnly(*reportPath, payload, stdout); err != nil {
		fmt.Fprintf(stderr, "write release report: %v\n", err)
		return 2
	}
	if report.SelectedOutcome == "passed" || (*mode == string(releasevalidation.ModePlan) && report.SelectedOutcome == "ready") {
		return 0
	}
	return 1
}

func writeCreateOnly(path string, payload []byte, stdout io.Writer) error {
	if path == "-" {
		_, err := stdout.Write(payload)
		return err
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(payload)
	return errorsJoin(writeErr, file.Close())
}

func errorsJoin(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
