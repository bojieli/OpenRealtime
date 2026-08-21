package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/simulation"
)

// runSimulate holds conversations between two OpenRealtime agents.
//
// Every other suite plays a recording at the system and scores the reply,
// which can only measure the half of the conversation the recording is not
// doing. This connects two sessions ear to mouth and lets them talk, so the
// motivating scenarios can be checked rather than described.
func runSimulate(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime simulate", flag.ContinueOnError)
	var (
		endpoint  string
		tokenEnv  string
		model     string
		scenarios string
		out       string
		budget    time.Duration
		settle    time.Duration
		maxTurns  int
		list      bool
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN",
		"environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&scenarios, "scenarios", "all",
		"comma-separated scenarios, or all: "+strings.Join(simulation.ScenarioNames(), ", "))
	flags.StringVar(&out, "out", "", "write the conversations to this path as JSON")
	flags.DurationVar(&budget, "budget", 3*time.Minute, "wall-clock budget for one conversation")
	flags.DurationVar(&settle, "settle", 12*time.Second,
		"how long both sides must be silent before a conversation counts as finished")
	flags.IntVar(&maxTurns, "max-turns", 0, "cap turns per conversation; zero uses the scenario's own")
	flags.BoolVar(&list, "list", false, "list the scenarios and stop")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	if list {
		for _, name := range simulation.ScenarioNames() {
			scenario, err := simulation.Lookup(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(output, "%-10s %s\n", scenario.Name, scenario.Question)
		}
		return nil
	}

	selected := simulation.ScenarioNames()
	if strings.TrimSpace(scenarios) != "" && !strings.EqualFold(scenarios, "all") {
		selected = nil
		for _, name := range strings.Split(scenarios, ",") {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				selected = append(selected, trimmed)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config := simulation.Config{
		Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model,
		Logf: func(format string, args ...any) { fmt.Fprintf(output, format+"\n", args...) },
	}

	conversations := make([]simulation.Conversation, 0, len(selected))
	failures := 0
	for _, name := range selected {
		scenario, err := simulation.Lookup(name)
		if err != nil {
			return err
		}
		if budget > 0 {
			scenario.Budget = budget
		}
		if settle > 0 {
			scenario.Settle = settle
		}
		if maxTurns > 0 {
			scenario.MaxTurns = maxTurns
		}
		fmt.Fprintf(output, "\n=== %s: %s\n", scenario.Name, scenario.Question)
		conversation, err := simulation.Run(ctx, config, scenario)
		if err != nil {
			fmt.Fprintf(output, "%s did not run: %v\n", scenario.Name, err)
			failures++
			if ctx.Err() != nil {
				break
			}
			continue
		}
		conversations = append(conversations, conversation)
		report(output, conversation)
		if !conversation.Passed() {
			failures++
		}
	}

	if strings.TrimSpace(out) != "" {
		encoded, err := json.MarshalIndent(conversations, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(output, "\nwritten to %s\n", out)
	}

	passed := 0
	for _, conversation := range conversations {
		if conversation.Passed() {
			passed++
		}
	}
	fmt.Fprintf(output, "\n%d of %d scenarios passed\n", passed, len(selected))
	if failures > 0 {
		return fmt.Errorf("%d of %d scenarios did not pass", failures, len(selected))
	}
	return nil
}

// report prints one conversation and its verdicts.
func report(output io.Writer, conversation simulation.Conversation) {
	total := 0.0
	for _, milliseconds := range conversation.SpeechMS {
		total += milliseconds
	}
	fmt.Fprintf(output, "\n%s: %d turns in %.0f s, ended by %s\n",
		conversation.Scenario, len(conversation.Turns),
		conversation.DurationMS/1000, conversation.EndedBy)
	fmt.Fprintf(output, "  speech: ")
	for _, speaker := range []string{conversation.Left, conversation.Right} {
		fmt.Fprintf(output, "%s %.1f s  ", speaker, conversation.SpeechMS[speaker]/1000)
	}
	if total > 0 {
		fmt.Fprintf(output, "overlap %.0f%%", conversation.OverlapMS*2/total*100)
	}
	fmt.Fprintln(output)
	for _, call := range conversation.ToolCalls {
		fmt.Fprintf(output, "  tool: %s(%s) by %s\n", call.Name, string(call.Arguments), call.Caller)
	}
	for _, failure := range conversation.Errors {
		fmt.Fprintf(output, "  error: %s\n", failure)
	}
	for _, result := range conversation.Results {
		mark := "FAIL"
		if result.Passed {
			mark = "pass"
		}
		fmt.Fprintf(output, "  [%s] %s", mark, result.Name)
		if result.Detail != "" {
			fmt.Fprintf(output, " — %s", result.Detail)
		}
		fmt.Fprintln(output)
		if !result.Passed {
			fmt.Fprintf(output, "         why it matters: %s\n", result.Why)
		}
	}
}
