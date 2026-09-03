package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/evals"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/policymodel"
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
		decision = flags.String("decision", "hand-off",
			"which boundary: hand-off, identifier, result, interaction, standing-instruction, backchannel, or overlap")
		provider  = flags.String("provider", "vllm", "provider serving the model under test")
		model     = flags.String("model", "", "model; empty selects the provider's default")
		url       = flags.String("url", "", "base URL; empty selects the provider's own")
		tokenEnv  = flags.String("token-env", "", "environment variable holding the credential")
		effort    = flags.String("effort", "minimal", "reasoning effort for the model under test")
		repeat    = flags.Int("repeat", 1, "runs per case; more than one exposes an unstable decision")
		threshold = flags.Float64("require", 0, "fail unless this fraction of cases pass")
		reason    = flags.String("reason", "off", "reasoning: off, on, or default")
		via       = flags.String("via", "policy", "how the interaction decision is taken: policy (what ships) or continuation")
		guided    = flags.Bool("guided", true, "constrain decoding to the enumerated acts, as the runtime does")
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("eval accepts flags only")
	}

	if *decision == "timeline" {
		return runTimelines(*provider, *model, *url, os.Getenv(*tokenEnv), *effort, *reason, output)
	}

	var cases []evals.Case
	switch *decision {
	case "hand-off":
		cases = evals.HandOffCases()
	case "identifier":
		cases = evals.IdentifierCases()
	case "result":
		cases = evals.ResultCases()
	case "interaction":
		cases = evals.InteractionCases()
	case "standing-instruction":
		cases = evals.StandingInstructionCases()
	case "backchannel":
		cases = evals.BackchannelCases()
	case "overlap":
		cases = evals.OverlapCases()
	default:
		return fmt.Errorf("decision must be hand-off, identifier, result, interaction, "+
			"standing-instruction, backchannel, or overlap, got %q", *decision)
	}

	// The model under test is configured exactly as the fast phase is, because
	// a boundary measured under different settings is a different boundary.
	client, err := providers.NewLLM(providers.LLMRequest{
		Provider: *provider, Model: *model, BaseURL: *url,
		APIKey: os.Getenv(*tokenEnv),
		Phase:  trajectory.PhaseFast, Effort: continuation.Effort(*effort),
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
		Reason:          providers.Reason(*reason),
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
	case "interaction":
		runner = evals.InteractionRunner{Provider: client, Label: label}
		if *via == "policy" {
			policyRunner, err := interactionPolicyRunner(*url, *model, os.Getenv(*tokenEnv), *guided, label)
			if err != nil {
				return err
			}
			runner = policyRunner
		}
	case "standing-instruction":
		runner = evals.StandingInstructionRunner{Provider: client, Label: label}
	case "backchannel":
		backchannelRunner, err := backchannelPolicyRunner(*url, *model, os.Getenv(*tokenEnv), *guided, label)
		if err != nil {
			return err
		}
		runner = backchannelRunner
	case "overlap":
		overlapRunner, err := overlapPolicyRunner(*url, *model, os.Getenv(*tokenEnv), *guided, label)
		if err != nil {
			return err
		}
		runner = overlapRunner
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

// runTimelines replays the scripted stretches of conversation. They are a
// separate mode rather than another decision because they score something
// different: not which act, but when.
func runTimelines(provider, model, url, key, effort, reason string, output io.Writer) error {
	client, err := providers.NewLLM(providers.LLMRequest{
		Provider: provider, Model: model, BaseURL: url, APIKey: key,
		Phase: trajectory.PhaseFast, Effort: continuation.Effort(effort),
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
		Reason:          providers.Reason(reason),
	})
	if err != nil {
		return fmt.Errorf("configure the model under test: %w", err)
	}
	label := provider
	if model != "" {
		label += "/" + model
	}
	runner := evals.InteractionRunner{Provider: client, Label: label}
	var outcomes []evals.TimelineOutcome
	for _, item := range evals.TimelineCases() {
		outcomes = append(outcomes, evals.RunTimeline(context.Background(), runner, item))
	}
	fmt.Fprint(output, evals.FormatTimelines(outcomes))
	return nil
}

// interactionPolicyRunner builds the runner that measures the shipping path:
// decoding constrained to the enumerated acts, an output budget derived from
// the longest exact label, and reasoning turned off at the endpoint.
// overlapPolicyRunner serves the overlap classification the way the runtime
// does, through the policy endpoint.
func overlapPolicyRunner(url, model, key string, guided bool, label string) (evals.Runner, error) {
	client, err := policymodel.New(policymodel.Config{
		BaseURL: url, Model: model, APIKey: key,
		GuidedChoice: guided, Reasoning: openaicompat.ReasoningControlTemplateKwargs,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the policy endpoint: %w", err)
	}
	policy, err := interaction.NewModelOverlapClassifier(client)
	if err != nil {
		return nil, err
	}
	return &evals.OverlapRunner{Policy: policy, Label: label + " (policy)"}, nil
}

// backchannelPolicyRunner serves the continuer decision the way the runtime
// does, through the policy endpoint rather than a continuation. The arithmetic
// gates are left at their shipped values; the cases reach the model because the
// situation they are given is past those gates.
func backchannelPolicyRunner(url, model, key string, guided bool, label string) (evals.Runner, error) {
	client, err := policymodel.New(policymodel.Config{
		BaseURL: url, Model: model, APIKey: key,
		GuidedChoice: guided, Reasoning: openaicompat.ReasoningControlTemplateKwargs,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the policy endpoint: %w", err)
	}
	policy, err := interaction.NewModelBackchannel(client, interaction.BackchannelOptions{})
	if err != nil {
		return nil, err
	}
	return &evals.BackchannelRunner{Policy: policy, Label: label + " (policy)"}, nil
}

func interactionPolicyRunner(url, model, key string, guided bool, label string) (evals.Runner, error) {
	client, err := policymodel.New(policymodel.Config{
		BaseURL: url, Model: model, APIKey: key,
		GuidedChoice: guided, Reasoning: openaicompat.ReasoningControlTemplateKwargs,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the policy endpoint: %w", err)
	}
	decider, err := interaction.NewInteractionModel(client)
	if err != nil {
		return nil, err
	}
	// Building the cases is what records their structure.
	evals.InteractionCases()
	return evals.PolicyInteractionRunner{
		Model: decider, Label: label + " (policy)", Situations: evals.InteractionSituations(),
	}, nil
}
