package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file sets what the command line does not, and the command line still wins.
// The whole point of the file is that a deployment keeps ninety-odd settings
// in version control and varies one of them for a run.
func TestConfigFileIsOverriddenByTheCommandLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	body := "fast:\n  effort: high\n  model: gemini-3.5-flash\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	effort := flags.String("fast-effort", "minimal", "")
	model := flags.String("fast-model", "qwen-fast", "")
	if err := loadConfig(flags, path); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := flags.Parse([]string{"-fast-effort", "low"}); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if *effort != "low" {
		t.Fatalf("the command line did not win: %q", *effort)
	}
	if *model != "gemini-3.5-flash" {
		t.Fatalf("the file did not apply: %q", *model)
	}
}

// A key that matches no setting is refused. A file is where somebody records a
// decision, and a decision that silently does nothing is worse than one that
// will not load.
func TestAMisspeltSettingIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte("fast:\n  provdier: vllm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("fast-provider", "vllm", "")
	err := loadConfig(flags, path)
	if err == nil {
		t.Fatal("a misspelt setting loaded quietly")
	}
	if !strings.Contains(err.Error(), "fast-provdier") {
		t.Fatalf("the error does not name the key: %v", err)
	}
}

// Nesting is the flag name, not a second vocabulary, so a flag added later is
// configurable without touching this file.
func TestNestingMapsOntoFlagNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	body := "listen: 127.0.0.1:9000\nperception:\n  asr:\n    model: whisper-turbo\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	listen := flags.String("listen", "", "")
	asr := flags.String("perception-asr-model", "", "")
	if err := loadConfig(flags, path); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if *listen != "127.0.0.1:9000" {
		t.Fatalf("a top-level setting did not apply: %q", *listen)
	}
	if *asr != "whisper-turbo" {
		t.Fatalf("a nested setting did not apply: %q", *asr)
	}
}

func TestMultilineTranscriptRulesComeFromNestedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streaming.yaml")
	body := "transcript:\n  policy: event-aware\n  partial-rules: |\n    Treat this as provisional.\n    Prefer listen.\n  final-rules: |\n    Treat this as settled.\n    Answer completed requests.\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	policy := flags.String("transcript-policy", "none", "")
	partial := flags.String("transcript-partial-rules", "", "")
	final := flags.String("transcript-final-rules", "", "")
	if err := loadConfig(flags, path); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if *policy != "event-aware" {
		t.Fatalf("policy = %q", *policy)
	}
	if !strings.Contains(*partial, "provisional") || !strings.Contains(*final, "settled") {
		t.Fatalf("event-specific rules did not survive YAML: partial=%q final=%q", *partial, *final)
	}
}

func TestASRLanguageComesFromNestedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deepgram.yaml")
	if err := os.WriteFile(path, []byte("asr:\n  language: multi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	language := flags.String("asr-language", "", "")
	if err := loadConfig(flags, path); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if *language != "multi" {
		t.Fatalf("ASR language = %q, want multi", *language)
	}
}

func TestTranscriptExtractionTimeoutComesFromNestedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deepgram.yaml")
	if err := os.WriteFile(path, []byte("transcript:\n  extraction-timeout: 1750ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	timeout := flags.Duration("transcript-extraction-timeout", 250*time.Millisecond, "")
	if err := loadConfig(flags, path); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if *timeout != 1750*time.Millisecond {
		t.Fatalf("transcript extraction timeout = %s, want 1.75s", *timeout)
	}
}
