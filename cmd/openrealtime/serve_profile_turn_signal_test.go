package main

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
)

// The served room builds its recogniser through this registration and hands
// the result to the graph. Every optional turn signal the recogniser gives has
// to survive whatever the registration wraps it in: the room once wrapped it in
// a buffer that forwarded neither, and a configured recogniser end of turn
// reached nothing while every unit test, built on an unwrapped fake, passed.
func TestServedRecogniserKeepsItsTurnSignals(t *testing.T) {
	t.Setenv("DEEPGRAM_API_KEY", "served-turn-signal-test")
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	reference := serveProviderReference("asr", "deepgram")
	for _, testCase := range []struct {
		name      string
		config    serveASRConfiguration
		wantEager bool
	}{
		{name: "flux", wantEager: true, config: serveASRConfiguration{
			FormatVersion: 1, Model: "flux-general-en", BaseURL: "wss://api.deepgram.com/v2/listen",
			Language: "en-US", RequestTimeoutMS: 30_000, CadenceMS: 100, EagerEOTThreshold: 0.5,
		}},
		{name: "nova-3", config: serveASRConfiguration{
			FormatVersion: 1, Model: "nova-3", BaseURL: "wss://api.deepgram.com/v1/listen",
			Language: "en-US", RequestTimeoutMS: 30_000, CadenceMS: 100, EndpointingMS: 300,
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw, err := json.Marshal(testCase.config)
			if err != nil {
				t.Fatal(err)
			}
			for _, registration := range inventory.ASR {
				if registration.Reference != reference {
					continue
				}
				provider, err := registration.FactoryConfiguration(context.Background(), legacy.Options{}, raw)
				if err != nil {
					t.Fatal(err)
				}
				defer closeReadinessResource(provider)
				if _, ok := provider.(interface{ SpeechEndpointed() bool }); !ok {
					t.Fatalf("the served %s recogniser %T hides its end of speech", testCase.name, provider)
				}
				// A streaming recogniser that keeps its connection must be
				// offered for reuse, or the room reconnects every utterance.
				if _, ok := provider.(v1.UtteranceReusable); !ok {
					t.Fatalf("the served %s recogniser %T is closed after every utterance", testCase.name, provider)
				}
				_, eager := provider.(interface{ EagerEndOfTurn() bool })
				if testCase.wantEager && !eager {
					t.Fatalf("the served %s recogniser %T hides its eager end of turn", testCase.name, provider)
				}
				return
			}
			t.Fatalf("no served ASR registration %q", reference)
		})
	}
}
