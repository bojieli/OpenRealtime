package tauvoice

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCampaignSchedulingDefaultsAreFrozen(t *testing.T) {
	if DefaultSeed != 300 || DefaultMaxConcurrency != 3 || DefaultWorkers != 0 {
		t.Fatalf("campaign constants drifted: seed=%d concurrency=%d workers=%d",
			DefaultSeed, DefaultMaxConcurrency, DefaultWorkers)
	}
	config := Config{}
	config.applyDefaults()
	if config.Seed != DefaultSeed {
		t.Fatalf("campaign seed = %d, want frozen upstream seed 300", config.Seed)
	}
	if config.MaxConcurrency != DefaultMaxConcurrency {
		t.Fatalf("max concurrency = %d, want frozen upstream scheduler load 3", config.MaxConcurrency)
	}
	if config.Workers != DefaultWorkers {
		t.Fatalf("workers = %d, want the in-process scheduler", config.Workers)
	}
}

func TestRelativeInterpreterIsAnchoredBeforeTauCheckoutChdir(t *testing.T) {
	relative := filepath.Join("relative-tools", "python")
	want, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Python: relative}
	config.applyDefaults()
	if config.Python != want {
		t.Fatalf("relative interpreter = %q, want %q", config.Python, want)
	}

	config = Config{Python: "python3"}
	config.applyDefaults()
	if config.Python != "python3" {
		t.Fatalf("PATH interpreter = %q, want python3", config.Python)
	}
}

// tau2's CLI takes the trajectory directory positionally. Passing it as
// --input-paths made argparse reject the invocation, and because the measures
// are computed after the simulations have already run, the failure never
// looked like a broken run - it looked like a run that simply had no
// turn-taking numbers in it.
func TestInteractionMetricsPassesThePathPositionally(t *testing.T) {
	args := interactionMetricsArgs("/runs/cell", "/runs/cell/interaction-metrics.json")
	for _, argument := range args {
		if strings.HasPrefix(argument, "--input") {
			t.Fatalf("input_paths is positional in tau2's CLI, not a flag: %v", args)
		}
	}
	position := slices.Index(args, "interaction-metrics")
	if position < 0 || position+1 >= len(args) || args[position+1] != "/runs/cell" {
		t.Fatalf("the trajectory directory must follow the subcommand: %v", args)
	}
}

// LiteLLM routes on a provider prefix. api_base alone is not enough, so a
// caller pointed at a local endpoint was rejected as "LLM Provider NOT
// provided" - which made the flag that exists to run a cell fully locally
// unable to do it.
func TestALocalCallerNamesItsProvider(t *testing.T) {
	local := Config{UserModel: "qwen-fast", UserModelURL: "http://127.0.0.1:8000/v1"}
	if got := local.userModelName(); got != "openai/qwen-fast" {
		t.Fatalf("a local caller must name a provider LiteLLM knows, got %q", got)
	}
	hosted := Config{UserModel: "gpt-4.1"}
	if got := hosted.userModelName(); got != "gpt-4.1" {
		t.Fatalf("a hosted caller is untouched, got %q", got)
	}
	explicit := Config{UserModel: "hosted_vllm/qwen-fast", UserModelURL: "http://127.0.0.1:8000/v1"}
	if got := explicit.userModelName(); got != "hosted_vllm/qwen-fast" {
		t.Fatalf("an explicit provider is a real choice and must survive, got %q", got)
	}
}
