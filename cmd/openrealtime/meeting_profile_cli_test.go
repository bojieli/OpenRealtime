package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
)

func TestMeetingProfileHasOneDirectLaunchRouteAndNoLegacySelector(t *testing.T) {
	var output bytes.Buffer
	err := runLaunchProfile([]string{"meeting", "-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) ||
		!strings.Contains(output.String(), "openrealtime profile meeting") {
		t.Fatalf("Meeting profile direct route error=%v output=%q", err, output.String())
	}
	for _, legacy := range []string{"meeting-legacy", "cascade-meeting", "omni-meeting", "meeting-migrate"} {
		output.Reset()
		err := runLaunchProfile([]string{legacy}, &output)
		if err == nil || !strings.Contains(err.Error(), "profile kind must be") {
			t.Fatalf("legacy Meeting selector %q error = %v", legacy, err)
		}
	}
}

func meetingProfileCampaignArguments(directory string) []string {
	return []string{
		"-out", filepath.Join(directory, "profile.yaml"),
		"-graph-out", filepath.Join(directory, "graph.json"),
		"-values-out", filepath.Join(directory, "values.json"),
		"-resolution-out", filepath.Join(directory, "resolution.json"),
		"-execution-out", filepath.Join(directory, "execution.json"),
	}
}

func TestMeetingProfileConcurrentPublicationNeverReplacesCampaign(t *testing.T) {
	parent := t.TempDir()
	campaign := filepath.Join(parent, "meeting-profile")
	options := defaultMeetingProfileOptions()
	options.out = filepath.Join(campaign, "profile.yaml")
	artifacts := []meetingProfileArtifactPublication{
		{label: "one", path: filepath.Join(campaign, "one"), payload: []byte("one")},
		{label: "two", path: filepath.Join(campaign, "two"), payload: []byte("two")},
	}
	const publishers = 8
	started := make(chan struct{})
	results := make(chan error, publishers)
	var ready sync.WaitGroup
	ready.Add(publishers)
	for index := 0; index < publishers; index++ {
		go func() {
			ready.Done()
			<-started
			results <- publishMeetingProfileCampaign(options, artifacts, nil)
		}()
	}
	ready.Wait()
	close(started)
	succeeded := 0
	for index := 0; index < publishers; index++ {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent Meeting profile publisher succeeded")
	}
	if err := publishMeetingProfileCampaign(options, artifacts, nil); err != nil {
		t.Fatalf("reopen concurrently published Meeting profile: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(campaign) {
		t.Fatalf("concurrent Meeting publication debris = %+v", entries)
	}
}

func TestMeetingProfileCommandPublishesExactCreateOnlyCampaign(t *testing.T) {
	parent := t.TempDir()
	campaign := filepath.Join(parent, "meeting-profile")
	verifier := meetingProfileVerifier()
	dependencies := meetingProfileCommandDependencies{
		executable: func() (identity inspect.ArtifactIdentity, err error) {
			return meetingProfileExecutable(), nil
		},
		verifier: func() (meetingDeploymentVerifier, error) { return verifier, nil },
	}
	var output bytes.Buffer
	arguments := meetingProfileCampaignArguments(campaign)
	if err := runMeetingProfileFreezeWithDependencies(arguments, &output, dependencies); err != nil {
		t.Fatal(err)
	}
	if verifier.resolve != 1 || verifier.verify < 2 {
		t.Fatalf("Meeting profile deployment proof calls resolve=%d verify=%d",
			verifier.resolve, verifier.verify)
	}
	entries, err := os.ReadDir(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("Meeting profile campaign entries = %d, want 5", len(entries))
	}
	payload, err := os.ReadFile(filepath.Join(campaign, "profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.ParseYAML("profile.yaml", payload)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Application.Selection.Reference == "" ||
		profile.Server.TokenEnvironment != "OPENREALTIME_TOKEN" ||
		profile.Server.Model != meetingLocalModelName {
		t.Fatalf("published Meeting profile = %+v", profile)
	}
	if !strings.Contains(output.String(), "background=google/gemini-3.7-flash") {
		t.Fatalf("Meeting profile command output = %q", output.String())
	}

	// A new verifier may reopen the same exact campaign, but it must not
	// replace or silently amend any retained artifact.
	verifier = meetingProfileVerifier()
	output.Reset()
	if err := runMeetingProfileFreezeWithDependencies(arguments, &output, dependencies); err != nil {
		t.Fatalf("reopen exact Meeting profile campaign: %v", err)
	}
	if err := os.WriteFile(filepath.Join(campaign, "graph.json"), []byte("drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier = meetingProfileVerifier()
	if err := runMeetingProfileFreezeWithDependencies(arguments, &output, dependencies); err == nil ||
		(!strings.Contains(err.Error(), "differs") && !strings.Contains(err.Error(), "invalid identity")) {
		t.Fatalf("mutated Meeting profile campaign error = %v", err)
	}
}

func TestMeetingProfilePublicationFailureDoesNotExposePartialCampaign(t *testing.T) {
	parent := t.TempDir()
	campaign := filepath.Join(parent, "meeting-profile")
	options := defaultMeetingProfileOptions()
	options.out = filepath.Join(campaign, "profile.yaml")
	artifacts := []meetingProfileArtifactPublication{
		{label: "one", path: filepath.Join(campaign, "one"), payload: []byte("one")},
		{label: "two", path: filepath.Join(campaign, "two"), payload: []byte("two")},
	}
	want := errors.New("injected Meeting profile publication failure")
	err := publishMeetingProfileCampaign(options, artifacts, func(completed int) error {
		if completed == 1 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("Meeting profile publication failure = %v", err)
	}
	if _, err := os.Lstat(campaign); !os.IsNotExist(err) {
		t.Fatalf("partial Meeting profile campaign is visible: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Meeting profile publication debris = %+v", entries)
	}
}

func TestMeetingProfileCampaignRejectsSymlinkParentAndExternalHardlink(t *testing.T) {
	realParent := t.TempDir()
	symlinkParent := filepath.Join(t.TempDir(), "profile-parent")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Fatal(err)
	}
	symlinkCampaign := filepath.Join(symlinkParent, "meeting-profile")
	options := defaultMeetingProfileOptions()
	options.out = filepath.Join(symlinkCampaign, "profile.yaml")
	artifacts := []meetingProfileArtifactPublication{{
		label: "profile", path: options.out, payload: []byte("profile"),
	}}
	if err := publishMeetingProfileCampaign(options, artifacts, nil); err == nil ||
		!strings.Contains(err.Error(), "parent is invalid") {
		t.Fatalf("symlinked Meeting profile parent error = %v", err)
	}

	parent := t.TempDir()
	campaign := filepath.Join(parent, "meeting-profile")
	options.out = filepath.Join(campaign, "profile.yaml")
	artifacts[0].path = options.out
	if err := publishMeetingProfileCampaign(options, artifacts, nil); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "outside-hardlink")
	if err := os.Link(options.out, alias); err != nil {
		t.Fatal(err)
	}
	if err := publishMeetingProfileCampaign(options, artifacts, nil); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Fatalf("externally hardlinked Meeting profile error = %v", err)
	}
}
