// Command openrealtime runs the server and its verification tools.
//
// One documented command brings up a working /v1/realtime on localhost:
//
//	openrealtime serve
//
// The default binding is cascade: fully local, no third-party account,
// nothing to sign up for. Switching to a hosted model is one flag and one
// credential.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "openrealtime:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return usage()
	}
	switch arguments[0] {
	case "serve":
		return runServe(arguments[1:], stdout)
	case "conformance":
		return runConformance(arguments[1:], stdout)
	case "probe":
		return runProbe(arguments[1:], stdout)
	case "efficiency":
		return runEfficiency(arguments[1:], stdout)
	case "bench":
		return runBench(arguments[1:], stdout)
	case "compare":
		return runCompare(arguments[1:], stdout)
	case "datasets":
		return runDatasets(arguments[1:], stdout)
	case "version":
		fmt.Fprintln(stdout, version())
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usageText)
		return nil
	default:
		fmt.Fprintln(stderr, usageText)
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

const usageText = `usage: openrealtime <command> [flags]

commands:
  serve         run the OpenRealtime server
  probe         drive a running server through one turn and report it
  conformance   verify protocol and component conformance
  efficiency    measure the costs the design claims are small
  bench         run a measurement suite against a running server
  compare       read two saved cells and report the pairing
  datasets      inventory a prepared benchmark dataset
  version       print the version

run "openrealtime <command> -h" for a command's flags`

func usage() error { return errors.New(usageText) }
