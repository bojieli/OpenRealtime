package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const benchmarkArtifactDirectory = "artifacts"

type benchmarkArtifactPathOperations struct {
	getwd    func() (string, error)
	now      func() time.Time
	random   io.Reader
	openRoot func(string) (*os.Root, error)
}

func productionBenchmarkArtifactPathOperations() benchmarkArtifactPathOperations {
	return benchmarkArtifactPathOperations{
		getwd: os.Getwd, now: time.Now, random: rand.Reader, openRoot: os.OpenRoot,
	}
}

// automaticBenchmarkArtifactPath reserves a fresh, private campaign directory
// below ./artifacts and returns a still-absent child path for the suite's
// create-only evidence publisher. Reserving the parent first prevents two
// concurrent commands from selecting the same run identity without weakening
// the publishers' rule that their own final directory must not exist yet.
func automaticBenchmarkArtifactPath(suite string) (string, error) {
	return automaticBenchmarkArtifactPathWithOperations(
		suite, productionBenchmarkArtifactPathOperations(),
	)
}

func resolveBenchmarkReviewDestination(
	suite, configured string, resume bool, automatic func(string) (string, error),
) (string, error) {
	if configured != "" && strings.TrimSpace(configured) != configured {
		return "", errors.New("benchmark review destination is noncanonical")
	}
	if configured != "" {
		return configured, nil
	}
	if resume {
		return "", errors.New("benchmark review recovery requires an explicit existing destination")
	}
	if automatic == nil {
		return "", errors.New("automatic benchmark review destination operation is missing")
	}
	return automatic(suite)
}

func automaticBenchmarkArtifactPathWithOperations(
	suite string, operations benchmarkArtifactPathOperations,
) (destination string, resultErr error) {
	if err := validateBenchmarkArtifactSuite(suite); err != nil {
		return "", err
	}
	if operations.getwd == nil || operations.now == nil || operations.random == nil ||
		operations.openRoot == nil {
		return "", errors.New("automatic benchmark artifact path operations are incomplete")
	}
	cwd, err := operations.getwd()
	if err != nil {
		return "", errors.New("resolve benchmark artifact working directory")
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil || filepath.Clean(cwd) != cwd || cwd == filepath.Dir(cwd) {
		return "", errors.New("benchmark artifact working directory is invalid")
	}
	if err := validateBenchmarkArtifactAncestors(cwd); err != nil {
		return "", err
	}
	cwdBefore, err := os.Lstat(cwd)
	if err != nil || cwdBefore.Mode()&os.ModeSymlink != 0 || !cwdBefore.IsDir() {
		return "", errors.New("benchmark artifact working directory is invalid")
	}
	cwdRoot, err := operations.openRoot(cwd)
	if err != nil {
		return "", errors.New("open benchmark artifact working directory")
	}
	defer func() {
		if closeErr := cwdRoot.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close benchmark artifact working directory"))
		}
	}()
	cwdOpened, openErr := cwdRoot.Stat(".")
	cwdVisible, visibleErr := os.Lstat(cwd)
	if openErr != nil || visibleErr != nil || cwdVisible.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(cwdBefore, cwdOpened) || !os.SameFile(cwdOpened, cwdVisible) {
		return "", errors.New("benchmark artifact working directory changed while opening")
	}

	artifactInfo, err := cwdRoot.Lstat(benchmarkArtifactDirectory)
	if errors.Is(err, os.ErrNotExist) {
		if err := cwdRoot.Mkdir(benchmarkArtifactDirectory, 0o700); err != nil {
			return "", errors.New("create automatic benchmark artifact directory")
		}
		if err := syncBenchmarkArtifactRoot(cwdRoot); err != nil {
			return "", errors.New("sync automatic benchmark artifact directory parent")
		}
		artifactInfo, err = cwdRoot.Lstat(benchmarkArtifactDirectory)
	}
	if err != nil || artifactInfo.Mode()&os.ModeSymlink != 0 || !artifactInfo.IsDir() {
		return "", errors.New("automatic benchmark artifact directory is invalid")
	}
	artifactRoot, err := cwdRoot.OpenRoot(benchmarkArtifactDirectory)
	if err != nil {
		return "", errors.New("open automatic benchmark artifact directory")
	}
	defer func() {
		if closeErr := artifactRoot.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close automatic benchmark artifact directory"))
		}
	}()
	artifactOpened, openErr := artifactRoot.Stat(".")
	artifactVisible, visibleErr := cwdRoot.Lstat(benchmarkArtifactDirectory)
	if openErr != nil || visibleErr != nil || artifactVisible.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(artifactInfo, artifactOpened) || !os.SameFile(artifactOpened, artifactVisible) {
		return "", errors.New("automatic benchmark artifact directory changed while opening")
	}

	timestamp := operations.now().UTC().Format("20060102T150405.000000000Z")
	var campaignName string
	for attempt := 0; attempt < 8; attempt++ {
		var entropy [16]byte
		if _, err := io.ReadFull(operations.random, entropy[:]); err != nil {
			return "", errors.New("generate automatic benchmark artifact identity")
		}
		campaignName = fmt.Sprintf("%s-%s-%s", suite, timestamp, hex.EncodeToString(entropy[:]))
		err = artifactRoot.Mkdir(campaignName, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", errors.New("reserve automatic benchmark artifact campaign")
		}
		campaignName = ""
	}
	if campaignName == "" {
		return "", errors.New("automatic benchmark artifact identity repeatedly collided")
	}
	if err := syncBenchmarkArtifactRoot(artifactRoot); err != nil {
		return "", errors.New("sync automatic benchmark artifact campaign parent")
	}
	campaignInfo, err := artifactRoot.Lstat(campaignName)
	if err != nil || campaignInfo.Mode()&os.ModeSymlink != 0 || !campaignInfo.IsDir() {
		return "", errors.New("automatic benchmark artifact campaign is invalid")
	}
	campaignRoot, err := artifactRoot.OpenRoot(campaignName)
	if err != nil {
		return "", errors.New("open automatic benchmark artifact campaign")
	}
	campaignOpened, openErr := campaignRoot.Stat(".")
	syncErr := syncBenchmarkArtifactRoot(campaignRoot)
	closeErr := campaignRoot.Close()
	campaignVisible, visibleErr := artifactRoot.Lstat(campaignName)
	if openErr != nil || syncErr != nil || closeErr != nil || visibleErr != nil ||
		campaignVisible.Mode()&os.ModeSymlink != 0 || !os.SameFile(campaignInfo, campaignOpened) ||
		!os.SameFile(campaignOpened, campaignVisible) {
		return "", errors.New("automatic benchmark artifact campaign changed while opening")
	}
	return filepath.Join(cwd, benchmarkArtifactDirectory, campaignName, "review"), nil
}

func validateBenchmarkArtifactSuite(suite string) error {
	if suite == "" || len(suite) > 64 || strings.TrimSpace(suite) != suite ||
		suite[0] == '-' || suite[len(suite)-1] == '-' {
		return errors.New("automatic benchmark artifact suite name is invalid")
	}
	for _, character := range suite {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return errors.New("automatic benchmark artifact suite name is invalid")
	}
	return nil
}

func validateBenchmarkArtifactAncestors(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("benchmark artifact working-directory ancestry is invalid")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func syncBenchmarkArtifactRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func rejectReviewFlagsForNonAttempt(flags *flag.FlagSet, label string) error {
	var found string
	flags.Visit(func(item *flag.Flag) {
		if found == "" && strings.HasPrefix(item.Name, "review-") {
			found = item.Name
		}
	})
	if found != "" {
		return fmt.Errorf("%s does not execute benchmark attempts and cannot use -%s", label, found)
	}
	return nil
}
