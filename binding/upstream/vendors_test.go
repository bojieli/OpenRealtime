package upstream_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Every vendor in the catalogue is driven here through the events its own
// documentation says it sends, against the settings the catalogue resolves for
// it. A vendor entry that names an event the mirror does not read, or a
// hand-off channel the endpoint would refuse, fails here rather than on
// somebody's first live session.

// startVendor brings up the binding against a fake endpoint using the
// catalogue's settings for one vendor.
func startVendor(
	t *testing.T, remote *fakeRemote, provider string, slow continuation.Provider,
) (binding.Runtime, *collectingSink) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("XAI_API_KEY", "test-key")
	t.Setenv("DASHSCOPE_API_KEY", "test-key")
	t.Setenv("AZURE_OPENAI_API_KEY", "test-key")
	settings, err := providers.ResolveUpstream(providers.UpstreamRequest{
		Provider: provider, URL: remote.url(),
	})
	if err != nil {
		t.Fatalf("resolve %s: %v", provider, err)
	}
	bind, err := upstream.New(upstream.Config{
		URL: settings.URL, Token: settings.Token, Model: settings.Model,
		Header: settings.Header, EventAliases: settings.EventAliases,
		Handoff: settings.Handoff, Slow: slow,
	})
	if err != nil {
		t.Fatalf("new upstream for %s: %v", provider, err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test-" + provider,
	})
	if err != nil {
		t.Fatalf("start %s: %v", provider, err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	return runtime, sink
}

// vendorScript is the event vocabulary one endpoint documents.
type vendorScript struct {
	provider string
	// transcription is how this endpoint reports what the user said.
	transcription string
	// audioDelta, transcriptDelta, and transcriptDone are how it reports its
	// own speech.
	audioDelta      string
	transcriptDelta string
	transcriptDone  string
	// handoffEvent is the client event that must carry the answer.
	handoffEvent string
}

func vendorScripts() []vendorScript {
	return []vendorScript{
		{
			provider:        "openai",
			transcription:   "conversation.item.input_audio_transcription.completed",
			audioDelta:      "response.output_audio.delta",
			transcriptDelta: "response.output_audio_transcript.delta",
			transcriptDone:  "response.output_audio_transcript.done",
			handoffEvent:    "conversation.item.create",
		},
		{
			// xAI documents the Realtime protocol with one rename, on the
			// event carrying what the user said.
			provider:        "xai",
			transcription:   "conversation.item.input_audio_transcription.updated",
			audioDelta:      "response.output_audio.delta",
			transcriptDelta: "response.output_audio_transcript.delta",
			transcriptDone:  "response.output_audio_transcript.done",
			handoffEvent:    "conversation.item.create",
		},
		{
			// Qwen implements the protocol as it stood before OpenAI renamed
			// its audio events, and reserves conversation items for tool
			// results - so the answer travels in the session instruction.
			provider:        "qwen-omni",
			transcription:   "conversation.item.input_audio_transcription.completed",
			audioDelta:      "response.audio.delta",
			transcriptDelta: "response.audio_transcript.delta",
			transcriptDone:  "response.audio_transcript.done",
			handoffEvent:    "session.update",
		},
	}
}

func TestEveryVendorsOwnEventsReachTheReasonerAndItsAnswerGetsBack(t *testing.T) {
	for _, script := range vendorScripts() {
		t.Run(script.provider, func(t *testing.T) {
			remote := newFakeRemote(t)
			slow := &scriptedSlow{turns: [][]continuation.Event{{
				{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
			}}}
			_, sink := startVendor(t, remote, script.provider, slow)
			<-remote.ready

			// What the user said, in this vendor's spelling.
			remote.emit(map[string]any{
				"type": script.transcription, "item_id": "item_1",
				"transcript": "what is my balance",
			})
			waitFor(t, func() bool {
				for _, message := range remote.sent() {
					if message["type"] == "response.create" {
						return true
					}
				}
				return false
			}, "the reasoner's answer never reached the endpoint")

			// The answer has to be in the channel this endpoint accepts.
			var carrier string
			for _, message := range remote.sent() {
				if message["type"] != script.handoffEvent {
					continue
				}
				encoded, _ := json.Marshal(message)
				if strings.Contains(string(encoded), "The balance is $40.00.") {
					carrier = string(encoded)
				}
			}
			if carrier == "" {
				t.Fatalf("the answer must be handed over in a %s, sent: %v",
					script.handoffEvent, kinds(remote.sent()))
			}

			// An endpoint that reserves conversation items for tool results
			// must never be sent a message item.
			if script.handoffEvent == "session.update" {
				for _, message := range remote.sent() {
					if message["type"] == "conversation.item.create" {
						t.Fatal("this endpoint accepts conversation items only for tool results")
					}
				}
			}

			sink.mu.Lock()
			transcripts := len(sink.transcripts)
			sink.mu.Unlock()
			if transcripts == 0 {
				t.Fatal("the client must see the transcript the endpoint produced")
			}
		})
	}
}

// The endpoint's own speech has to reach the client whatever it calls the
// events, or the caller hears silence while the model is talking.
func TestEveryVendorsSpeechReachesTheClient(t *testing.T) {
	for _, script := range vendorScripts() {
		t.Run(script.provider, func(t *testing.T) {
			remote := newFakeRemote(t)
			runtime, sink := startVendor(t, remote, script.provider, &scriptedSlow{})
			<-remote.ready

			remote.emit(map[string]any{
				"type": script.audioDelta, "delta": encodedSilence(),
			})
			waitFor(t, func() bool {
				sink.mu.Lock()
				defer sink.mu.Unlock()
				return sink.audioFrames > 0
			}, "the endpoint's audio never reached the client")

			// Its transcript deltas are what the client hears as text.
			remote.emit(map[string]any{
				"type": script.transcriptDelta, "delta": "Let me check that.",
			})
			waitFor(t, func() bool {
				sink.mu.Lock()
				defer sink.mu.Unlock()
				return strings.Contains(strings.Join(sink.spoken, ""), "Let me check")
			}, "the endpoint's own transcript never reached the client")

			// And the finished utterance becomes observer-authority evidence
			// in the trajectory, so the reasoner reads what the voice said
			// without mistaking it for something the user asked.
			remote.emit(map[string]any{
				"type": script.transcriptDone, "transcript": "Let me check that.",
			})
			waitFor(t, func() bool {
				for _, item := range runtime.Trajectory().Items {
					if item.Kind == trajectory.KindObservation &&
						trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
						return true
					}
				}
				return false
			}, "the endpoint's finished speech never reached the trajectory")
		})
	}
}

// A borrowed session instruction has to be given back. Leaving the answer in
// it would have the endpoint repeat a stale answer on every later turn.
func TestASessionInstructionHandoffIsRestoredAfterTheResponse(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	startVendor(t, remote, "qwen-omni", slow)
	<-remote.ready

	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "what is my balance",
	})
	waitFor(t, func() bool { return instructionCarrying(remote.sent(), "$40.00") }, "no handoff instruction")

	remote.emit(map[string]any{"type": "response.done"})
	waitFor(t, func() bool {
		sent := remote.sent()
		for index := len(sent) - 1; index >= 0; index-- {
			if sent[index]["type"] != "session.update" {
				continue
			}
			encoded, _ := json.Marshal(sent[index])
			return !strings.Contains(string(encoded), "$40.00")
		}
		return false
	}, "the session instruction was never restored after the answer was said")
}

func instructionCarrying(sent []map[string]any, needle string) bool {
	for _, message := range sent {
		if message["type"] != "session.update" {
			continue
		}
		encoded, _ := json.Marshal(message)
		if strings.Contains(string(encoded), needle) {
			return true
		}
	}
	return false
}

func kinds(sent []map[string]any) []string {
	var result []string
	for _, message := range sent {
		if name, ok := message["type"].(string); ok {
			result = append(result, name)
		}
	}
	return result
}

// encodedSilence is one 20 ms frame of 24 kHz PCM16 silence, base64 encoded.
func encodedSilence() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 960))
}
