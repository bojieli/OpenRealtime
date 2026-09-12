package upstream_test

// The whole system against the real vendor: GPT-Live in front, a real reasoner
// behind, and this binding between them. It is the one test that proves the
// seam end to end at the level a deployment runs - not the translator alone.
// It costs a few cents of voice time and two model calls, and runs only when
// asked:
//
//	OPENREALTIME_LIVE_E2E=1 OPENAI_API_KEY=... GEMINI_API_KEY=... \
//	  go test ./binding/upstream/ -run Live -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// synthesise turns one sentence into 24 kHz mono PCM16 through the vendor's
// speech API, so the spoken request has no provenance the repository cannot
// state.
func synthesise(t *testing.T, text string) []byte {
	t.Helper()
	for _, model := range []string{"gpt-4o-mini-tts", "tts-1"} {
		body, _ := json.Marshal(map[string]any{
			"model": model, "input": text, "voice": "alloy", "response_format": "pcm",
		})
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"https://api.openai.com/v1/audio/speech", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("synthesise: %v", err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode == http.StatusOK && len(payload) > 0 {
			t.Logf("synthesised %q with %s: %.1fs", text, model, float64(len(payload))/2/24000)
			return payload
		}
	}
	t.Fatal("no speech model accepted the request")
	return nil
}

// TestLiveTheWholeBindingAnswersThroughADelegation is the system claim.
//
// The voice is told it cannot look anything up. The reasoner is told a fact
// the voice was never given. The caller asks for that fact by speaking. If the
// voice says it, then the voice delegated, the binding ran the reasoner on the
// delegation, the reasoner read the mirrored transcript and answered from its
// own instruction, the binding handed the answer back against the delegation,
// and the voice spoke it - the entire seam, on the real endpoint.
func TestLiveTheWholeBindingAnswersThroughADelegation(t *testing.T) {
	if os.Getenv("OPENREALTIME_LIVE_E2E") == "" {
		t.Skip("set OPENREALTIME_LIVE_E2E=1 to run against the real endpoints")
	}
	if os.Getenv("OPENAI_API_KEY") == "" || os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY and GEMINI_API_KEY are both needed")
	}
	settings, err := providers.ResolveUpstream(providers.UpstreamRequest{Provider: "openai-live"})
	if err != nil {
		t.Fatalf("resolve openai-live: %v", err)
	}
	// Built the way serve builds it: silent, because its output is voiced by
	// the remote, and able to execute tools, because it is the only thing
	// here that may.
	slow, err := providers.NewLLM(providers.LLMRequest{
		Provider: "google", Phase: trajectory.PhaseSlow,
		ToolAuthority: continuation.ToolAuthorityExecute, SpeechAuthority: continuation.SpeechAuthoritySilent,
	})
	if err != nil {
		t.Fatalf("build the reasoner: %v", err)
	}
	bind, err := upstream.New(upstream.Config{
		URL: settings.URL, Token: settings.Token, Model: settings.Model, Header: settings.Header,
		Dialect: string(settings.Dialect), Dial: settings.Dial, Handoff: settings.Handoff,
		Slow: slow, SlowMaxTokens: 2048,
		// The fact lives here and nowhere the voice can reach.
		AgentInstruction: "You are the backend of a parcel company. The only order on file is " +
			"number 4217: it shipped yesterday and arrives tomorrow before noon. When asked " +
			"about an order, answer with exactly those facts in one short sentence.",
	})
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "live-system",
		Settings: binding.Settings{
			Instruction: "You are the voice of a parcel company. You cannot look anything up " +
				"yourself. Whenever the caller asks about an order, delegate to the backend and " +
				"tell the caller you are checking; never guess an order's status.",
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })

	utterance := synthesise(t, "Hi there. Could you check the status of my order, number four two one seven?")
	go func() {
		const frame = 24000 / 1000 * 20 * 2
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for offset := 0; offset < len(utterance); offset += frame {
			end := min(offset+frame, len(utterance))
			if err := runtime.Audio(context.Background(), perception.Frame{
				Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24000,
				PCM16LE: utterance[offset:end], CapturedNS: uint64(offset) * 1_000_000 / 48,
			}); err != nil {
				t.Errorf("audio: %v", err)
				return
			}
			<-ticker.C
		}
	}()

	var heardRequest, sawDelegation bool
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		status := runtime.Status()
		if status.Remote != nil && status.Remote.OpenDelegation != "" {
			sawDelegation = true
		}
		sink.mu.Lock()
		for _, transcript := range sink.transcripts {
			// The vendor may render the number as digits or as words, so the
			// request is "a final transcript that mentions the order" rather
			// than an exact string. The delegation is the stronger signal and
			// is asserted below.
			if transcript.Final && strings.Contains(strings.ToLower(transcript.Text), "order") {
				heardRequest = true
			}
		}
		said := strings.ToLower(strings.Join(sink.spoken, ""))
		failures := append([]binding.ErrorEvent(nil), sink.failures...)
		sink.mu.Unlock()
		for _, failure := range failures {
			if strings.HasPrefix(failure.Code, "upstream_") && failure.Code != "upstream_idle" {
				t.Fatalf("the binding reported %s: %s", failure.Code, failure.Message)
			}
		}
		if heardRequest && sawDelegation && strings.Contains(said, "tomorrow") {
			status := runtime.Status()
			t.Logf("the voice said: %q", strings.Join(sink.spoken, ""))
			t.Logf("remote: session %s, %.0fs billed, delegation seen=%v", status.Remote.SessionID,
				status.Remote.UsageSeconds, sawDelegation)
			if status.Remote.SessionID == "" {
				t.Error("the vendor's session id never reached Status")
			}
			var reasoned bool
			for _, item := range runtime.Trajectory().Items {
				if item.Producer.Phase == trajectory.PhaseSlow && strings.Contains(item.Content, "4217") {
					reasoned = true
				}
			}
			if !reasoned {
				t.Error("the answer was spoken but no slow-phase item in the trajectory carries it; " +
					"the voice guessed, which it was told not to")
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	t.Fatalf("no answer within 90s: heard request=%v, delegation seen=%v, voice said %q, failures %v",
		heardRequest, sawDelegation, strings.Join(sink.spoken, ""), sink.failures)
}
