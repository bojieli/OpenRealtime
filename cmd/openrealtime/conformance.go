package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/conformance"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// runConformance verifies what the server claims about the wire.
//
// It is a command rather than only a test because a deployment should be able
// to check its own build: a compatibility claim that only holds in CI is a
// compatibility claim about CI.
func runConformance(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime conformance <protocol|extension|sidecar|all> [-out path]")
	}
	suite := strings.ToLower(strings.TrimSpace(arguments[0]))
	flags := flag.NewFlagSet("openrealtime conformance "+suite, flag.ContinueOnError)
	var (
		out              string
		sidecarAddress   string
		sidecarSeconds   float64
		sidecarTurnLimit time.Duration
		sidecarProtocol  int
	)
	flags.StringVar(&out, "out", "", "write the report to this path as JSON")
	flags.StringVar(&sidecarAddress, "address", "", "connect to a running sidecar as tcp:host:port or unix:/path")
	flags.Float64Var(&sidecarSeconds, "speech-seconds", 1.5, "how much audio to send before asking for a turn")
	flags.DurationVar(&sidecarTurnLimit, "turn-timeout", 120*time.Second, "how long one turn may take")
	flags.IntVar(&sidecarProtocol, "protocol-version", 0, "sidecar protocol version; 0 selects legacy v1")
	flags.SetOutput(output)
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}

	reports := map[string]any{}
	switch suite {
	case "protocol":
		report, err := conformance.RunProtocol()
		if err != nil {
			return err
		}
		reports["protocol"] = report
		if !report.Passed {
			return errors.New("protocol conformance failed")
		}
	case "extension":
		report := conformance.RunExtension()
		reports["extension"] = report
		if !report.Passed {
			return errors.New("extension conformance failed")
		}
	case "sidecar":
		// Everything after the flags is the command that runs the sidecar, so
		// a sidecar can be verified exactly as the engine will run it.
		report := sidecar.RunConformance(context.Background(), sidecar.ConformanceOptions{
			Config: sidecar.Config{
				Command: flags.Args(), Address: sidecarAddress,
				ProtocolVersion: sidecarProtocol,
				Logf: func(format string, values ...any) {
					fmt.Fprintf(os.Stderr, format+"\n", values...)
				},
			},
			SpeechSeconds: sidecarSeconds, TurnTimeout: sidecarTurnLimit,
		})
		reports["sidecar"] = report
		if !report.Passed {
			encoded, _ := json.MarshalIndent(reports, "", "  ")
			fmt.Fprintln(output, string(encoded))
			return errors.New("sidecar conformance failed")
		}
	case "all":
		protocolReport, err := conformance.RunProtocol()
		if err != nil {
			return err
		}
		extensionReport := conformance.RunExtension()
		reports["protocol"] = protocolReport
		reports["extension"] = extensionReport
		if !protocolReport.Passed || !extensionReport.Passed {
			return errors.New("conformance failed")
		}
	default:
		return fmt.Errorf("conformance suite must be protocol, extension, sidecar, or all, got %q", suite)
	}

	encoded, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}
