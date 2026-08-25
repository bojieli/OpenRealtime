package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// runScenario drives the end-to-end interaction suite.
//
// It sits beside bench rather than under it because it scores a different
// thing. A benchmark plays somebody else's recording and asks whether the task
// was completed; a scenario scripts a conversation and asks whether the agent
// spoke at the right moments. A system can pass every benchmark here while
// talking over everybody in it.
func runScenario(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	var (
		endpoint = flags.String("url", "ws://127.0.0.1:8765/v1/realtime", "session endpoint")
		tokenEnv = flags.String("token-env", "", "environment variable holding the session credential")
		model    = flags.String("model", "", "model to request; empty selects the server's own")
		speech   = flags.String("speech-url", "http://127.0.0.1:8081/v1/audio/speech", "speech endpoint that gives the participants voices")
		voice    = flags.String("speech-model", "fishaudio/s2-pro", "speech model")
		only     = flags.String("only", "", "run one scenario by name")
		timeout  = flags.Duration("timeout", 3*time.Minute, "bound on one scenario")
		record   = flags.String("record", "", "write the timed record of each scenario to this file")
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("scenario accepts flags only")
	}

	speaker := scenario.SpeechVoice{
		Endpoint: *speech, Model: *voice, Default: "default",
		// The second party gets a different voice. A phone menu that sounds
		// exactly like the caller removes the difficulty the case exists to
		// pose.
		Voices: map[string]string{"other": "alloy"},
	}
	config := bench.SessionConfig{
		Endpoint: *endpoint, Token: os.Getenv(*tokenEnv), Model: *model,
		Timeout: *timeout, Quiet: true,
	}

	var results []scenario.Result
	passed := 0
	for _, item := range scenario.Suite() {
		if *only != "" && item.Name != *only {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		result, err := scenario.Play(ctx, speaker, config, item)
		cancel()
		if err != nil {
			fmt.Fprintf(output, "  ERR  %-28s %v\n", item.Name, err)
			results = append(results, result)
			continue
		}
		results = append(results, result)
		if result.Passed {
			passed++
			fmt.Fprintf(output, "  ok   %-28s %s\n", item.Name, item.Note)
			continue
		}
		fmt.Fprintf(output, "  FAIL %-28s %s\n", item.Name, item.Note)
		for _, failure := range result.Failures {
			fmt.Fprintf(output, "         %s\n", failure)
		}
	}
	fmt.Fprintf(output, "\n  scenarios %d/%d\n", passed, len(results))

	if strings.TrimSpace(*record) != "" {
		encoded, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*record, encoded, 0o644); err != nil {
			return fmt.Errorf("write the record: %w", err)
		}
	}
	return nil
}
