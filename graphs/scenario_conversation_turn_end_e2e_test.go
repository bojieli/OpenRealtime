package graphs_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/perception"
)

// eagerTurnASR is a recogniser that becomes moderately confident the turn is
// over once it has heard two frames - the shape of Flux's EagerEndOfTurn.
type eagerTurnASR struct {
	descriptor v1.Descriptor
	mu         sync.Mutex
	revision   uint64
}

func (provider *eagerTurnASR) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *eagerTurnASR) PushFrame(_ context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.revision++
	words := []string{"Count", "Count the animals", "Count the animals as I go"}
	text := words[min(int(provider.revision), len(words))-1]
	return []v1.PerceptionRevision{{
		RevisionID: provider.revision, SourceSample: frame.SampleOffset, UnstableText: text,
	}}, nil
}

func (provider *eagerTurnASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.revision++
	return v1.PerceptionRevision{RevisionID: provider.revision, StableText: "Count the animals as I go", Final: true}, nil
}

func (provider *eagerTurnASR) EagerEndOfTurn() bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.revision >= 2
}

// The room ends an utterance on the recogniser's eager end of turn without
// any silence reaching the acoustic gate, and without the setting it waits.
// No silent frame is ever sent, so only the recogniser can close the stream.
func TestScenarioConversationRecognizerTurnEndClosesTheUtteranceBeforeSilence(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		endOfTurn string
		wantClose bool
	}{
		{name: "eager end of turn closes it", endOfTurn: "eager", wantClose: true},
		{name: "the gate alone waits for silence", endOfTurn: "", wantClose: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			base := newScenarioProfileFixture(t)
			config := base.pluginConfig()
			config.ASR.EndOfTurn = testCase.endOfTurn
			config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				return &eagerTurnASR{descriptor: config.ASR.Descriptor}, nil
			}
			policy := newScenarioAddressingPolicyControl()
			config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return &scenarioAddressingPolicyDecider{control: policy, descriptor: config.Policy.Descriptor}, nil
			}
			models := newScenarioAddressingModelControl()
			config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
				return &scenarioAddressingModel{control: models, descriptor: config.Model.Descriptor}, nil
			}
			tts := &scenarioAddressingTTSControl{}
			config.TTS.Factory = func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				return &scenarioAddressingTTS{control: tts, descriptor: config.TTS.Descriptor}, nil
			}
			launchConfig, err := graphs.ScenarioConversationLaunchConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			launched, err := graphlaunch.New(context.Background(), launchConfig)
			if err != nil {
				t.Fatal(err)
			}
			sink := newScenarioAddressingSink()
			settings := legacy.Settings{
				Instruction: "Answer requests addressed to this assistant.",
				Voice:       config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate,
			}
			runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
				SessionID: scenarioAddressingSession, Sink: sink, Settings: settings,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if closeErr := runtime.Close(ctx, errors.New("turn end test complete")); closeErr != nil {
					t.Errorf("close scenario runtime: %v", closeErr)
				}
			})
			if err := runtime.Update(context.Background(), settings); err != nil {
				t.Fatal(err)
			}

			streamID := scenarioAddressingStreamID(scenarioAddressingSession, 1)
			var sample uint64
			for index := 0; index < 3; index++ {
				frame := perception.Frame{
					Kind: perception.FrameAudio, Source: scenarioconversation.SourceMicrophone,
					CapturedNS: uint64(index+1) * uint64(100*time.Millisecond), Index: uint64(index + 1),
					PCM16LE: scenarioEndpointPCM(2_400), SampleRateHz: 24_000, SampleOffset: sample,
				}
				sample += 2_400
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := runtime.Audio(ctx, frame)
				cancel()
				if err != nil {
					t.Fatalf("send voiced frame %d: %v", index, err)
				}
			}

			var started, stopped bool
			window := time.NewTimer(5 * time.Second)
			if !testCase.wantClose {
				window.Reset(1500 * time.Millisecond)
			}
			defer window.Stop()
			for !stopped {
				select {
				case event := <-sink.activities:
					if event.ItemID != streamID {
						t.Fatalf("activity on %q, want %q", event.ItemID, streamID)
					}
					started = started || event.Started
					stopped = stopped || event.Stopped
				case <-window.C:
					if testCase.wantClose {
						t.Fatalf("the recogniser's eager end of turn did not close the utterance (started %v)", started)
					}
					if !started {
						t.Fatal("the utterance never started")
					}
					return
				}
			}
			if !testCase.wantClose {
				t.Fatal("an utterance closed with no silence and no recogniser end of turn configured")
			}
			final := time.NewTimer(5 * time.Second)
			defer final.Stop()
		awaitFinal:
			for {
				select {
				case event := <-sink.transcripts:
					if event.Final {
						if event.Text != "Count the animals as I go" {
							t.Fatalf("final transcript = %q", event.Text)
						}
						break awaitFinal
					}
				case <-final.C:
					t.Fatal("the closed utterance produced no final transcript")
				}
			}

			// The person keeps talking. A recogniser's turn end is a guess the
			// speaker is free to prove wrong, so the audio after it is the next
			// utterance on a new stream - never late audio that fails the
			// session, which is what the live room did.
			next := scenarioAddressingStreamID(scenarioAddressingSession, 2)
			for index := 3; index < 6; index++ {
				frame := perception.Frame{
					Kind: perception.FrameAudio, Source: scenarioconversation.SourceMicrophone,
					CapturedNS: uint64(index+1) * uint64(100*time.Millisecond), Index: uint64(index + 1),
					PCM16LE: scenarioEndpointPCM(2_400), SampleRateHz: 24_000, SampleOffset: sample,
				}
				sample += 2_400
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := runtime.Audio(ctx, frame)
				cancel()
				if err != nil {
					t.Fatalf("speech continuing after the turn end was refused: %v", err)
				}
			}
			resumed := time.NewTimer(5 * time.Second)
			defer resumed.Stop()
			for {
				select {
				case event := <-sink.activities:
					if event.Started {
						if event.ItemID != next {
							t.Fatalf("continued speech started on %q, want the next stream %q", event.ItemID, next)
						}
						sink.mu.Lock()
						failures := slices.Clone(sink.failures)
						sink.mu.Unlock()
						if len(failures) > 0 {
							t.Fatalf("the session reported failures: %+v", failures)
						}
						return
					}
				case <-resumed.C:
					t.Fatal("speech after the recogniser's turn end never started a new utterance")
				}
			}
		})
	}
}
