// Command reference-v1 demonstrates direct use of the stable provider API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	reference "github.com/bojieli/OpenRealtime/adapters/reference"
	referencev1 "github.com/bojieli/OpenRealtime/adapters/reference/v1"
	stable "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/fixture"
)

func main() {
	fixturePath := flag.String("fixture", "tests/fixtures/m0-tone.wav", "24 kHz PCM16 WAV")
	manifestPath := flag.String("manifest", "tests/fixtures/m1-reference-manifest.json", "symbolic reference manifest")
	flag.Parse()
	if err := run(context.Background(), *fixturePath, *manifestPath); err != nil {
		fmt.Fprintln(os.Stderr, "reference-v1:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, fixturePath, manifestPath string) error {
	manifest, err := reference.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	input, err := fixture.Load(fixturePath, 20, "example-v1")
	if err != nil {
		return err
	}
	perception := referencev1.NewPerception(manifest)
	for _, frame := range input.Frames {
		if _, err := perception.PushFrame(ctx, stable.AudioFrame{
			Index: frame.Index, SampleOffset: frame.SampleOffset,
			SampleRateHz: frame.SampleRateHz, PCM16LE: append([]byte(nil), frame.PCM16LE...),
		}); err != nil {
			return err
		}
	}
	finalRevision, err := perception.Finalize(ctx, input.SampleCount)
	if err != nil {
		return err
	}
	cognition := referencev1.NewCognition(manifest.ResponseText)
	candidate, err := cognition.Respond(ctx, finalRevision)
	if err != nil {
		return err
	}
	speech := referencev1.NewSpeech(100)
	var chunks uint64
	var samples uint64
	err = speech.Stream(ctx, stable.SpeechPlan{CandidateID: candidate.CandidateID, Text: candidate.Text}, func(chunk stable.SpeechChunk) error {
		end, err := chunk.EndSample()
		if err != nil {
			return err
		}
		chunks++
		samples = end
		return nil
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"api_version": stable.Version, "perception": perception.Descriptor(),
		"cognition": cognition.Descriptor(), "speech": speech.Descriptor(),
		"final_revision": finalRevision.RevisionID, "candidate_id": candidate.CandidateID,
		"streamed_chunks": chunks, "output_samples": samples,
	})
}
