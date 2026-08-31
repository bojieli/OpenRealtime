package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticBenchmarkArtifactPathReservesFreshPrivateCampaign(t *testing.T) {
	working := t.TempDir()
	instant := time.Date(2026, time.August, 31, 12, 34, 56, 789, time.UTC)
	operations := benchmarkArtifactPathOperations{
		getwd: func() (string, error) { return working, nil },
		now:   func() time.Time { return instant },
		random: bytes.NewReader(append(
			make([]byte, 16), append(make([]byte, 15), byte(1))...,
		)),
		openRoot: os.OpenRoot,
	}
	first, err := automaticBenchmarkArtifactPathWithOperations("meeting", operations)
	if err != nil {
		t.Fatal(err)
	}
	second, err := automaticBenchmarkArtifactPathWithOperations("meeting", operations)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || filepath.Base(first) != "review" || filepath.Base(second) != "review" {
		t.Fatalf("automatic paths = %q and %q", first, second)
	}
	wantPrefix := filepath.Join(working, benchmarkArtifactDirectory, "meeting-20260831T123456.000000789Z-")
	for _, path := range []string{first, second} {
		if !strings.HasPrefix(path, wantPrefix) {
			t.Fatalf("automatic path = %q, want prefix %q", path, wantPrefix)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("publisher destination already exists: %v", err)
		}
		campaign := filepath.Dir(path)
		info, err := os.Lstat(campaign)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("campaign identity/mode = %v, %v", info, err)
		}
	}
}

func TestAutomaticBenchmarkArtifactPathRejectsUnsafeNamesAndRoots(t *testing.T) {
	base := t.TempDir()
	validOperations := func(path string) benchmarkArtifactPathOperations {
		return benchmarkArtifactPathOperations{
			getwd: func() (string, error) { return path, nil },
			now:   time.Now, random: bytes.NewReader(make([]byte, 16)), openRoot: os.OpenRoot,
		}
	}
	for _, suite := range []string{"", "Meeting", "meeting/review", "-meeting", "meeting-", " meeting"} {
		if _, err := automaticBenchmarkArtifactPathWithOperations(suite, validOperations(base)); err == nil {
			t.Fatalf("unsafe suite %q was accepted", suite)
		}
	}
	if _, err := automaticBenchmarkArtifactPathWithOperations("meeting", validOperations(string(filepath.Separator))); err == nil {
		t.Fatal("filesystem-root working directory was accepted")
	}
	outside := t.TempDir()
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := automaticBenchmarkArtifactPathWithOperations("meeting", validOperations(linked)); err == nil ||
		!strings.Contains(err.Error(), "ancestry") {
		t.Fatalf("symlink working directory error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(base, benchmarkArtifactDirectory)); err != nil {
		t.Fatal(err)
	}
	if _, err := automaticBenchmarkArtifactPathWithOperations("meeting", validOperations(base)); err == nil ||
		!strings.Contains(err.Error(), "artifact directory is invalid") {
		t.Fatalf("symlink artifact directory error = %v", err)
	}
}

func TestAutomaticBenchmarkArtifactPathFailsClosedOnDependenciesAndEntropy(t *testing.T) {
	working := t.TempDir()
	operations := benchmarkArtifactPathOperations{
		getwd: func() (string, error) { return working, nil }, now: time.Now,
		random: io.LimitReader(bytes.NewReader(nil), 0), openRoot: os.OpenRoot,
	}
	if _, err := automaticBenchmarkArtifactPathWithOperations("scenario", operations); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("entropy failure error = %v", err)
	}
	operations.random = bytes.NewReader(make([]byte, 16))
	operations.getwd = func() (string, error) { return "", errors.New("fixture") }
	if _, err := automaticBenchmarkArtifactPathWithOperations("scenario", operations); err == nil ||
		!strings.Contains(err.Error(), "working directory") {
		t.Fatalf("getwd failure error = %v", err)
	}
	operations.getwd = func() (string, error) { return working, nil }
	operations.openRoot = nil
	if _, err := automaticBenchmarkArtifactPathWithOperations("scenario", operations); err == nil ||
		!strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete operations error = %v", err)
	}
}

func TestRejectReviewFlagsForNonAttempt(t *testing.T) {
	flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
	var directory, provider string
	flags.StringVar(&directory, "review-dir", "", "")
	flags.StringVar(&provider, "review-provider", "default", "")
	if err := flags.Parse([]string{"-review-provider", "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := rejectReviewFlagsForNonAttempt(flags, "fixture -list"); err == nil ||
		!strings.Contains(err.Error(), "-review-provider") {
		t.Fatalf("review-flag refusal = %v", err)
	}
	clean := flag.NewFlagSet("clean", flag.ContinueOnError)
	clean.String("review-dir", "", "")
	if err := rejectReviewFlagsForNonAttempt(clean, "clean"); err != nil {
		t.Fatalf("default review flag was treated as explicit: %v", err)
	}
}

func TestResolveBenchmarkReviewDestinationHasNoDisabledState(t *testing.T) {
	called := 0
	automatic := func(suite string) (string, error) {
		called++
		return "/fixture/" + suite + "/review", nil
	}
	destination, err := resolveBenchmarkReviewDestination("meeting", "", false, automatic)
	if err != nil || destination != "/fixture/meeting/review" || called != 1 {
		t.Fatalf("automatic destination=%q calls=%d error=%v", destination, called, err)
	}
	destination, err = resolveBenchmarkReviewDestination("meeting", "/explicit/review", false, automatic)
	if err != nil || destination != "/explicit/review" || called != 1 {
		t.Fatalf("explicit destination=%q calls=%d error=%v", destination, called, err)
	}
	for _, test := range []struct {
		name       string
		configured string
		resume     bool
		match      string
	}{
		{name: "whitespace", configured: " /review", match: "noncanonical"},
		{name: "recovery", resume: true, match: "explicit existing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveBenchmarkReviewDestination(
				"meeting", test.configured, test.resume, automatic,
			); err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("destination refusal = %v", err)
			}
		})
	}
}
