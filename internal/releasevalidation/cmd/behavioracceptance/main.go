package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/releasevalidation"
)

type resultFlags []releasevalidation.BehavioralResultInput

func (values *resultFlags) String() string {
	parts := make([]string, 0, len(*values))
	for _, value := range *values {
		parts = append(parts, value.ID+"="+value.Path)
	}
	return strings.Join(parts, ",")
}

func (values *resultFlags) Set(raw string) error {
	id, path, found := strings.Cut(raw, "=")
	if !found || strings.TrimSpace(id) == "" || strings.TrimSpace(path) == "" {
		return errors.New("result must be SUITE_ID=PATH")
	}
	*values = append(*values, releasevalidation.BehavioralResultInput{ID: id, Path: path})
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("behavioracceptance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	targetPath := flags.String("targets", "scripts/behavioral-acceptance-targets.json",
		"checked preregistered behavioral targets")
	candidatePath := flags.String("candidate", "", "frozen final-candidate declaration")
	reportPath := flags.String("report", "", "create-only acceptance report path")
	var results resultFlags
	flags.Var(&results, "result", "final candidate result as SUITE_ID=PATH; repeat for every required suite")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*candidatePath) == "" ||
		strings.TrimSpace(*reportPath) == "" {
		fmt.Fprintln(stderr, "behavioral acceptance requires -candidate, -report, and only flags")
		return 2
	}
	targets, targetDigest, err := releasevalidation.LoadBehavioralTargets(*targetPath)
	if err != nil {
		fmt.Fprintf(stderr, "behavioral targets invalid: %v\n", err)
		return 2
	}
	candidate, candidateDigest, err := releasevalidation.LoadFrozenCandidate(*candidatePath)
	if err != nil {
		fmt.Fprintf(stderr, "frozen candidate invalid: %v\n", err)
		return 2
	}
	report := releasevalidation.EvaluateBehavioralAcceptance(
		targets, targetDigest, candidate, candidateDigest, results,
	)
	payload, err := releasevalidation.MarshalBehavioralAcceptanceReport(report)
	if err != nil {
		fmt.Fprintf(stderr, "encode behavioral acceptance report: %v\n", err)
		return 2
	}
	if err := writeCreateOnly(*reportPath, payload); err != nil {
		fmt.Fprintf(stderr, "write behavioral acceptance report: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "behavioral acceptance: %s\n", report.Outcome)
	if report.Accepted {
		return 0
	}
	return 1
}

func writeCreateOnly(path string, payload []byte) error {
	if strings.TrimSpace(path) == "" || path == "-" {
		return errors.New("report must name a create-only file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(payload)
	closeErr := file.Close()
	if writeErr != nil || written != len(payload) || closeErr != nil {
		return errors.Join(writeErr, closeErr, errors.New("acceptance report was not written completely"))
	}
	return nil
}
