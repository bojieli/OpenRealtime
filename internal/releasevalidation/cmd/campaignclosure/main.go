package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/releasevalidation"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("campaignclosure", flag.ContinueOnError)
	flags.SetOutput(stderr)
	draftPath := flags.String("draft", "", "strict canonical campaign-closure draft")
	outputPath := flags.String("out", "", "create-only campaign-closure destination")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*draftPath) == "" ||
		strings.TrimSpace(*outputPath) == "" {
		fmt.Fprintln(stderr, "campaignclosure requires -draft, -out, and only flags")
		return 2
	}
	draft, err := releasevalidation.LoadCampaignClosureDraft(*draftPath)
	if err != nil {
		fmt.Fprintf(stderr, "campaign closure draft invalid: %v\n", err)
		return 2
	}
	closure, err := releasevalidation.PublishCampaignClosure(
		releasevalidation.CampaignClosurePublication{Closure: draft, Path: *outputPath},
	)
	if err != nil {
		fmt.Fprintf(stderr, "publish campaign closure: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "campaign closure: %s\n", closure.ClosureSHA256)
	return 0
}
