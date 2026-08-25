package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/evals"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// runEval drives the step-by-step layer.
//
// It sits under `bench` rather than beside it. A suite plays a recording and
// scores the outcome; this freezes one decision and asks for the next action,
// which is what makes it fast enough to run before a change ships and specific
// enough to say which boundary moved.
func runEval(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	flags.SetOutput(output)
	var (
		decision  = flags.String("decision", "hand-off", "which boundary: hand-off, identifier, or result")
		provider  = flags.String("provider", "vllm", "provider serving the model under test")
		model     = flags.String("model", "", "model; empty selects the provider's default")
		url       = flags.String("url", "", "base URL; empty selects the provider's own")
		tokenEnv  = flags.String("token-env", "", "environment variable holding the credential")
		effort    = flags.String("effort", "minimal", "reasoning effort for the model under test")
		repeat    = flags.Int("repeat", 1, "runs per case; more than one exposes an unstable decision")
		threshold = flags.Float64("require", 0, "fail unless this fraction of cases pass")
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("eval accepts flags only")
	}

	var cases []evals.Case
	switch *decision {
	case "hand-off":
		cases = evals.HandOffCases()
	case "identifier":
		cases = evals.IdentifierCases()
	case "result":
		cases = evals.ResultCases()
	default:
		return fmt.Errorf("decision must be hand-off, identifier, or result, got %q", *decision)
	}

	// The model under test is configured exactly as the fast phase is, because
	// a boundary measured under different settings is a different boundary.
	client, err := providers.NewLLM(providers.LLMRequest{
		Provider: *provider, Model: *model, BaseURL: *url,
		APIKey: os.Getenv(*tokenEnv),
		Phase:  trajectory.PhaseFast, Effort: continuation.Effort(*effort),
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
		Reason:          providers.ReasonOff,
	})
	if err != nil {
		return fmt.Errorf("configure the model under test: %w", err)
	}
	label := *provider
	if *model != "" {
		label += "/" + *model
	}

	repeated := make([]evals.Case, 0, len(cases)*max(1, *repeat))
	for run := range max(1, *repeat) {
		for _, item := range cases {
			if run > 0 {
				item.Name = fmt.Sprintf("%s#%d", item.Name, run+1)
			}
			repeated = append(repeated, item)
		}
	}

	var runner evals.Runner = evals.HandOffRunner{Provider: client, Label: label}
	switch *decision {
	case "identifier":
		runner = evals.IdentifierRunner{Provider: client, Label: label}
	case "result":
		runner = evals.ResultRunner{Provider: client, Label: label}
	}
	report := evals.Run(context.Background(), runner, repeated)
	fmt.Fprint(output, report.Format())

	summary := report.Summary()
	if *threshold > 0 && summary.Total > 0 {
		rate := float64(summary.Passed) / float64(summary.Total)
		if rate < *threshold {
			return fmt.Errorf("passed %.0f%% of %d cases, required %.0f%%",
				rate*100, summary.Total, *threshold*100)
		}
	}
	return nil
}
