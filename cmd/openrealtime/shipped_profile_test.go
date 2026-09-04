package main

import (
	"errors"
	"flag"
	"io"
	"path/filepath"
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
