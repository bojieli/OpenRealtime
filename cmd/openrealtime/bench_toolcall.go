package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/bench/toolcall"
)

// runToolCall is `openrealtime bench toolcall`: ten spoken requests that each
// need one tool call, scored on execution count, arguments and whether the
// result was spoken.
func runToolCall(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench toolcall", flag.ContinueOnError)
	var (
		endpoint, tokenEnv, model, out, cellName string
		limit                                    int
		timeout, delay                           time.Duration
		waitConfigured, player                   bool
	)
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "server endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token")
	flags.StringVar(&model, "model", "", "model to request")
	flags.StringVar(&out, "out", "", "write the result to this path as JSON")
	flags.StringVar(&cellName, "cell", "reference", "name for this cell")
	flags.IntVar(&limit, "limit", 0, "stop after this many tasks; a limited run is incomplete")
	flags.DurationVar(&timeout, "task-timeout", time.Minute, "how long one task may take")
	flags.DurationVar(&delay, "result-delay", 2*time.Second, "how long each tool takes to return")
	flags.BoolVar(&waitConfigured, "wait-configured", true, "start each replay only after session.updated")
	flags.BoolVar(&player, "player", false,
		"play received audio like a device: stop and send conversation.item.truncate when the server cancels a response")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cell, err := resolveCell(cellName, "", "", "")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := toolcall.Run(ctx, toolcall.Options{
		Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model, Cell: cell, Limit: limit,
		Timeout: timeout, ResultDelay: delay, WaitConfigured: waitConfigured, Player: player,
		Progress: func(line string) { fmt.Fprintln(output, line) },
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "tasks  : %d completed, %d failed, of %d expected\n",
		result.Summary.Completed, result.Summary.Failed, result.Expected)
	fmt.Fprintf(output, "passed : %d of %d\n", result.Summary.Passed, result.Summary.Completed)
	for _, task := range result.Tasks {
		reason := task.Notes["failure"]
		if task.Error != "" {
			reason = "error: " + task.Error
		}
		fmt.Fprintf(output, "  %-20s passed=%-5v %s\n", task.ID, task.Passed, reason)
	}
	if reportErr := result.Reportable(); reportErr != nil {
		fmt.Fprintf(output, "\nNOT REPORTABLE: %v\n", reportErr)
	}
	if strings.TrimSpace(out) != "" {
		if err := result.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(output, "written to %s\n", out)
	}
	if result.Summary.Completed == 0 {
		return errors.New("no tool-call task completed")
	}
	return nil
}
