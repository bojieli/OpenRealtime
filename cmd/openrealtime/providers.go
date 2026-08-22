package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bojieli/OpenRealtime/providers"
)

func runProviders(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime providers", flag.ContinueOnError)
	var (
		role    string
		probe   string
		baseURL string
		timeout time.Duration
	)
	flags.StringVar(&role, "role", "all", "which catalogue to print: llm, asr, tts, or all")
	flags.StringVar(&probe, "probe", "",
		"ask a language-model provider what it serves, instead of printing the catalogue")
	flags.StringVar(&baseURL, "url", "", "endpoint to probe; empty uses the provider's own")
	flags.DurationVar(&timeout, "timeout", 30*time.Second, "probe timeout")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("providers accepts flags only")
	}
	if strings.TrimSpace(probe) != "" {
		return probeProvider(probe, baseURL, timeout, output)
	}
	return printCatalogue(role, output)
}

// probeProvider asks the provider itself, which is the answer that cannot be
// stale.
func probeProvider(name, baseURL string, timeout time.Duration, output io.Writer) error {
	result, err := providers.Probe(context.Background(), providers.LLMRequest{
		Provider: name, BaseURL: baseURL, RequestTimeout: timeout,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "%s serves %d models at %s\n\n", result.Provider, len(result.Models), result.Endpoint)
	for _, model := range result.Models {
		fmt.Fprintf(output, "  %s\n", model)
	}
	return nil
}

func printCatalogue(role string, output io.Writer) error {
	wanted := strings.ToLower(strings.TrimSpace(role))
	switch wanted {
	case "", "all", "llm", "asr", "tts":
	default:
		return fmt.Errorf("role must be llm, asr, tts, or all, got %q", role)
	}
	fmt.Fprintf(output,
		"Provider catalogue, default models reviewed %s.\n"+
			"Endpoints and credentials are durable; a default model is a hint that goes stale.\n"+
			"Ask the provider instead with: openrealtime providers -probe NAME\n",
		providers.Reviewed)

	if wanted == "" || wanted == "all" || wanted == "llm" {
		fmt.Fprintln(output, "\nlanguage models  (-fast-provider, -slow-provider)")
		table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "  NAME\tDIALECT\tFAST MODEL\tSLOW MODEL\tREASONING\tCREDENTIAL")
		for _, entry := range providers.LLMs() {
			reasoning := string(entry.Reasoning)
			switch entry.Dialect {
			case providers.DialectAnthropicMessages:
				reasoning = "thinking"
			case providers.DialectGemini:
				reasoning = "thinkingLevel"
			}
			if reasoning == "" {
				reasoning = "none"
			}
			fmt.Fprintf(table, "  %s\t%s\t%s\t%s\t%s\t%s\n",
				entry.Name, entry.Dialect, dash(entry.FastModel), dash(entry.SlowModel),
				reasoning, credentialState(entry.Common))
		}
		_ = table.Flush()
		printNotes(output, providersCommon(providers.LLMs(), func(entry providers.LLM) providers.Common {
			return entry.Common
		}))
	}

	if wanted == "" || wanted == "all" || wanted == "asr" {
		fmt.Fprintln(output, "\nrecognisers  (-asr-provider)")
		table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "  NAME\tSTREAMING\tMODEL\tCREDENTIAL")
		for _, entry := range providers.ASRs() {
			streaming := "batch"
			if entry.Streaming {
				streaming = "streaming"
			}
			fmt.Fprintf(table, "  %s\t%s\t%s\t%s\n",
				entry.Name, streaming, dash(entry.Model), credentialState(entry.Common))
		}
		_ = table.Flush()
		printNotes(output, providersCommon(providers.ASRs(), func(entry providers.ASR) providers.Common {
			return entry.Common
		}))
	}

	if wanted == "" || wanted == "all" || wanted == "tts" {
		fmt.Fprintln(output, "\nsynthesisers  (-tts-provider)")
		table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "  NAME\tMODEL\tVOICE\tCREDENTIAL")
		for _, entry := range providers.TTSs() {
			voice := dash(entry.Voice)
			if entry.VoiceRequired {
				voice = "required"
			}
			fmt.Fprintf(table, "  %s\t%s\t%s\t%s\n",
				entry.Name, dash(entry.Model), voice, credentialState(entry.Common))
		}
		_ = table.Flush()
		printNotes(output, providersCommon(providers.TTSs(), func(entry providers.TTS) providers.Common {
			return entry.Common
		}))
	}
	return nil
}

// credentialState reports whether a usable credential is present, which is the
// question an operator is actually asking when they run this.
func credentialState(common providers.Common) string {
	name := common.CredentialEnv()
	for _, candidate := range common.KeyEnv {
		if strings.TrimSpace(os.Getenv(candidate)) != "" {
			return candidate + " set"
		}
	}
	if common.Local {
		return "not needed"
	}
	if name == "" {
		return "-"
	}
	return name
}

func printNotes(output io.Writer, entries []providers.Common) {
	for _, entry := range entries {
		if entry.Notes == "" {
			continue
		}
		fmt.Fprintf(output, "    %s: %s\n", entry.Name, entry.Notes)
	}
}

func providersCommon[T any](entries []T, project func(T) providers.Common) []providers.Common {
	result := make([]providers.Common, 0, len(entries))
	for _, entry := range entries {
		result = append(result, project(entry))
	}
	return result
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
