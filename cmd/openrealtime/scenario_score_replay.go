package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
)

func runScenarioScoreReplay(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("review replay-scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	var directory, receiptPath string
	flags.StringVar(&directory, "source-dir", "", "sealed graph-native scenario source directory")
	flags.StringVar(&receiptPath, "source-receipt", "", "external scenario source receipt")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("review replay-scenario accepts flags only")
	}
	var err error
	if directory, err = resolveScenarioEvaluationPath("source directory", directory); err != nil {
		return err
	}
	if receiptPath, err = resolveScenarioEvaluationPath("source receipt", receiptPath); err != nil {
		return err
	}
	receipt, err := graphnative.ReadSourceReceipt(receiptPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	bundle, err := graphnative.VerifyScoredSourceBundle(ctx, graphnative.SourceBundleOptions{Directory: directory}, receipt)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Replayed %d scenario attempts; outcomes and metrics match source receipt %s.\nReplay does not establish recognizer accuracy, provider authenticity, or final behavioral acceptance.\n", len(bundle.Manifest.Attempts), receipt.ReceiptSHA256)
	return err
}
