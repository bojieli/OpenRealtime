package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/conformance"
)

// runConformance verifies what the server claims about the wire.
//
// It is a command rather than only a test because a deployment should be able
// to check its own build: a compatibility claim that only holds in CI is a
// compatibility claim about CI.
func runConformance(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime conformance <protocol|extension|all> [-out path]")
	}
	suite := strings.ToLower(strings.TrimSpace(arguments[0]))
	flags := flag.NewFlagSet("openrealtime conformance "+suite, flag.ContinueOnError)
	var out string
	flags.StringVar(&out, "out", "", "write the report to this path as JSON")
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
		return fmt.Errorf("conformance suite must be protocol, extension, or all, got %q", suite)
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
