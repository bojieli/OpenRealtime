package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped profiles are documents people copy, and a key that matches no
// flag is refused rather than ignored - which is the right behaviour and turns
// every new setting into a way to break a file nobody re-reads. So they are
// loaded here.
//
// -help is what makes this cheap: loadConfig runs before the flag set is
// parsed, so a profile with a bad key fails before flag.ErrHelp comes back,
// and nothing opens a port, a model, or a credential.
func TestTheShippedProfilesStillLoad(t *testing.T) {
	for _, profile := range []string{
		"openrealtime.example.yaml", "openrealtime.deepgram.yaml",
	} {
		t.Run(profile, func(t *testing.T) {
			path := filepath.Join("..", "..", profile)
			err := runServe([]string{"-config", path, "-help"}, io.Discard)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("loading %s: %v", profile, err)
			}
		})
	}
}

func TestDeepgramProfilePinsSubTurnFloorAndAddresseeDistinctions(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "openrealtime.deepgram.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	profile := string(payload)
	partialStart := strings.Index(profile, "  partial-rules: |")
	partialEnd := strings.Index(profile, "  final-acts:")
	finalStart := strings.Index(profile, "  final-rules: |")
	finalEnd := strings.Index(profile, "\npolicy:")
	if partialStart < 0 || partialEnd <= partialStart ||
		finalStart <= partialEnd || finalEnd <= finalStart {
		t.Fatal("Deepgram transcript policy blocks are missing or out of order")
	}
	partial := strings.Join(strings.Fields(profile[partialStart:partialEnd]), " ")
	final := strings.Join(strings.Fields(profile[finalStart:finalEnd]), " ")

	for _, contract := range []struct {
		name  string
		block string
		terms []string
	}{
		{
			name:  "active output floor taking",
			block: partial,
			terms: []string{
				"floor-taking or topic-shift opener directed to the agent",
				"Choose stop-speaking",
				"do not wait for the sentence to finish",
			},
		},
		{
			name:  "active output other addressee",
			block: partial,
			terms: []string{
				"vocative naming or role-addressing somebody else",
				"Choose keep-speaking",
				"overrides a tentative speaker-is-user label",
			},
		},
		{
			name:  "active output acknowledgement",
			block: partial,
			terms: []string{
				"pure one- or two-token listener acknowledgement",
				"is keep-speaking",
			},
		},
		{
			name:  "final other addressee",
			block: final,
			terms: []string{
				"explicit vocative naming or role-addressing somebody else",
				"stronger addressee evidence",
				"side speech remains listen",
			},
		},
		{
			name:  "final topic change",
			block: final,
			terms: []string{
				"completed question or request addressed to the agent is answer",
				"explicit topic-change request",
				"talk about something else",
			},
		},
	} {
		t.Run(contract.name, func(t *testing.T) {
			for _, term := range contract.terms {
				if !strings.Contains(contract.block, term) {
					t.Fatalf("shipped Deepgram policy lost %q", term)
				}
			}
		})
	}
}
