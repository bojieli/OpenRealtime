package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
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
		repeat   = flags.Int("repeat", 1, "runs per scenario; latency from one run is noise, so a latency claim needs several")
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
	runs := max(1, *repeat)
	for _, item := range scenario.Suite() {
		if *only != "" && item.Name != *only {
			continue
		}
		var attempts []scenario.Result
		for run := 0; run < runs; run++ {
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			result, err := scenario.Play(ctx, speaker, config, item)
			cancel()
			if err != nil {
				fmt.Fprintf(output, "  ERR  %-28s %v\n", item.Name, err)
			}
			attempts = append(attempts, result)
			results = append(results, result)
			if result.Passed {
				passed++
			}
		}
		reportScenario(output, item, attempts)
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

// report renders one scenario's attempts.
//
// Failures are listed from the first attempt that had them rather than from
// every attempt, because the same failure repeated five times is one fact.
// Latency is pooled across all of them, because one sample of a latency is not
// a measurement of anything.
func reportScenario(output io.Writer, item scenario.Scenario, attempts []scenario.Result) {
	passed := 0
	var failures []string
	for _, attempt := range attempts {
		if attempt.Passed {
			passed++
			continue
		}
		if failures == nil {
			failures = attempt.Failures
		}
	}
	mark := "FAIL"
	if passed == len(attempts) {
		mark = "ok  "
	} else if passed > 0 {
		mark = "part"
	}
	if len(attempts) > 1 {
		fmt.Fprintf(output, "  %s %-28s %d/%d  %s\n", mark, item.Name, passed, len(attempts), item.Note)
	} else {
		fmt.Fprintf(output, "  %s %-28s %s\n", mark, item.Name, item.Note)
	}
	for _, failure := range failures {
		fmt.Fprintf(output, "         %s\n", failure)
	}
	reportLatency(output, attempts)
}

// reportLatency prints the waits a person in the room would have sat through,
// pooled across every attempt.
//
// Only the ones the agent actually answered: a trigger it ignored is a
// correctness result and belongs in the checks, and averaging it in as a very
// large latency would make one silence look like a slow reply. The count is
// printed so a number drawn from two samples cannot be mistaken for one drawn
// from fifty.
func reportLatency(output io.Writer, attempts []scenario.Result) {
	var answered []float64
	for _, attempt := range attempts {
		for _, entry := range attempt.Latencies {
			if entry.Heard && entry.MS >= 0 {
				answered = append(answered, entry.MS)
			}
		}
	}
	if len(answered) == 0 {
		return
	}
	sort.Float64s(answered)
	pick := func(fraction float64) float64 {
		index := int(fraction * float64(len(answered)-1))
		return answered[index]
	}
	fmt.Fprintf(output, "         heard after %.0fms p50, %.0fms p90, %.0fms worst, over %d triggers\n",
		pick(0.5), pick(0.9), answered[len(answered)-1], len(answered))
}
