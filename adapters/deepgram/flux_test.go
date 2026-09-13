package deepgram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/coder/websocket"
)

// fakeFlux is a /v2/listen WebSocket that sends one scripted TurnInfo per audio
// frame and answers ForceEndTurn the way the service does: an EndOfTurn with
// trigger manual while a turn is open, a no-active-turn warning otherwise.
type fakeFlux struct {
	// lateAfter holds back every scripted entry from this frame on by
	// lateDelay, the way a service still decoding a burst of audio answers.
	lateAfter int
	lateDelay time.Duration
	server    *httptest.Server
	query     chan url.Values
	accepts   atomic.Int32
	forces    atomic.Int32
	closes    atomic.Int32
	mu        sync.Mutex
	script    []string
	next      int
	active    bool
	latest    string
	turnIndex int
	// crossNext makes the next ForceEndTurn cross a natural EndOfTurn: the
	// service ends the turn itself, then finds no turn to force.
	crossNext bool
}

func newFakeFlux(t *testing.T, script ...string) *fakeFlux {
	t.Helper()
	fake := &fakeFlux{query: make(chan url.Values, 4), script: script}
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case fake.query <- request.URL.Query():
		default:
		}
		if request.Header.Get("Authorization") != "Token key" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer connection.CloseNow()
		fake.accepts.Add(1)
		for {
			kind, payload, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				fake.mu.Lock()
				late := fake.lateDelay > 0 && fake.next >= fake.lateAfter
				delay := fake.lateDelay
				fake.mu.Unlock()
				if entry, ok := fake.take(); ok {
					if late {
						time.AfterFunc(delay, func() {
							_ = connection.Write(context.Background(), websocket.MessageText, []byte(entry))
						})
					} else {
						_ = connection.Write(request.Context(), websocket.MessageText, []byte(entry))
					}
				}
				continue
			}
			var control struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload, &control)
			switch control.Type {
			case "ForceEndTurn":
				fake.forces.Add(1)
				for _, answer := range fake.answerForce() {
					_ = connection.Write(request.Context(), websocket.MessageText, []byte(answer))
				}
			case "CloseStream":
				fake.closes.Add(1)
				_ = connection.Close(websocket.StatusNormalClosure, "closed")
				return
			}
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeFlux) url() string {
	return "ws" + strings.TrimPrefix(fake.server.URL, "http") + "/v2/listen"
}

func (fake *fakeFlux) take() (string, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.next >= len(fake.script) {
		return "", false
	}
	entry := fake.script[fake.next]
	fake.next++
	var turn struct {
		Event      string `json:"event"`
		Transcript string `json:"transcript"`
	}
	if json.Unmarshal([]byte(entry), &turn) == nil {
		switch turn.Event {
		case "StartOfTurn", "Update", "EagerEndOfTurn", "TurnResumed":
			fake.active = fake.active || turn.Event == "StartOfTurn"
			fake.latest = turn.Transcript
		case "EndOfTurn":
			fake.active = false
			fake.turnIndex++
		}
	}
	return entry, true
}

func (fake *fakeFlux) answerForce() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	warning, _ := json.Marshal(map[string]any{"type": "Warning", "code": fluxNoActiveTurn})
	if fake.crossNext && fake.active {
		fake.crossNext, fake.active = false, false
		fake.turnIndex++
		return []string{turnInfo("EndOfTurn", fake.latest, "model"), string(warning)}
	}
	if !fake.active {
		return []string{string(warning)}
	}
	fake.active = false
	fake.turnIndex++
	return []string{turnInfo("EndOfTurn", fake.latest, "manual")}
}

func turnInfo(event, transcript string, trigger ...string) string {
	message := map[string]any{
		"type": "TurnInfo", "event": event, "turn_index": 0, "transcript": transcript,
		"audio_window_start": 0, "audio_window_end": 1, "end_of_turn_confidence": 0.5,
		"words": []map[string]any{{"word": "w", "confidence": 0.9, "start": 0, "end": 0.1}},
	}
	if len(trigger) > 0 {
		message["trigger"] = trigger[0]
	}
	payload, _ := json.Marshal(message)
	return string(payload)
}

func fluxListener(t *testing.T, fake *fakeFlux, config FluxConfig) *FluxListener {
	t.Helper()
	config.URL, config.APIKey = fake.url(), "key"
	if config.DrainTimeout == 0 {
		config.DrainTimeout = 2 * time.Second
	}
	listener, err := NewFluxListener(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func pushFlux(t *testing.T, listener *FluxListener, index uint64) []v1.PerceptionRevision {
	t.Helper()
	revisions, err := listener.PushFrame(context.Background(), v1.AudioFrame{
		Index: index, SampleOffset: index * 1_280, SampleRateHz: 16_000, PCM16LE: tone(1_280),
	})
	if err != nil {
		t.Fatalf("push %d: %v", index, err)
	}
	return revisions
}

// awaitFlux folds in events as they arrive until the condition holds, and
// returns the revision the listener would emit at that point. A pushed frame
// is answered asynchronously, so a test that read state straight after the
// push would be measuring the socket, not the listener.
func awaitFlux(t *testing.T, listener *FluxListener, condition func() bool) []v1.PerceptionRevision {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		listener.mu.Lock()
		listener.applyAvailable()
		if condition() {
			revisions := listener.revisionIfChanged()
			listener.mu.Unlock()
			return revisions
		}
		listener.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the Flux event")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFluxDialsV2WithTurnDetectionSettings(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t, turnInfo("Update", ""))
	listener := fluxListener(t, fake, FluxConfig{
		Model: FluxMultilingualModel, EOTThreshold: 0.8, EagerEOTThreshold: 0.4,
		EOTTimeout: 7 * time.Second, Keyterms: []string{"sea bass", "fennel"},
		LanguageHints: []string{"en", "es"},
	})
	pushFlux(t, listener, 0)
	query := <-fake.query
	want := url.Values{
		"model": {"flux-general-multi"}, "encoding": {"linear16"}, "sample_rate": {"16000"},
		"eot_threshold": {"0.8"}, "eager_eot_threshold": {"0.4"}, "eot_timeout_ms": {"7000"},
		"keyterm": {"sea bass", "fennel"}, "language_hint": {"en", "es"},
	}
	if !reflect.DeepEqual(query, want) {
		t.Fatalf("Flux query = %v, want %v", query, want)
	}
	if got := listener.Descriptor(); got.Name != "deepgram-flux/flux-general-multi" || got.Version != "deepgram-flux-persistent-1" {
		t.Fatalf("descriptor = %+v", got)
	}
}

// Unset thresholds are the service's defaults, and are not sent: a client
// that restated them would silently pin today's defaults into every session.
func TestFluxLeavesUnsetThresholdsToTheService(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t)
	listener := fluxListener(t, fake, FluxConfig{})
	pushFlux(t, listener, 0)
	query := <-fake.query
	for _, name := range []string{"eot_threshold", "eager_eot_threshold", "eot_timeout_ms", "language_hint", "keyterm"} {
		if query.Has(name) {
			t.Fatalf("unset %s was sent: %v", name, query)
		}
	}
	if query.Get("model") != DefaultFluxModel {
		t.Fatalf("default model = %q", query.Get("model"))
	}
}

// The eager lifecycle: EagerEndOfTurn opens the speculative window, TurnResumed
// closes it with the words that followed, a second EagerEndOfTurn reopens it,
// and EndOfTurn ends the turn with the eager transcript. Flux ended the turn
// itself, so Finalize sends nothing.
func TestFluxEagerEndOfTurnResumesAndEnds(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t,
		turnInfo("StartOfTurn", "Hi I"),
		turnInfo("EagerEndOfTurn", "Hi I need to cancel my subscription."),
		turnInfo("TurnResumed", "Hi I need to cancel my subscription please"),
		turnInfo("EagerEndOfTurn", "Hi I need to cancel my subscription please."),
		turnInfo("EndOfTurn", "Hi I need to cancel my subscription please.", "model"),
	)
	listener := fluxListener(t, fake, FluxConfig{EagerEOTThreshold: 0.4})

	revisions := pushFlux(t, listener, 0)
	revisions = append(revisions, awaitFlux(t, listener, func() bool { return listener.turnActive })...)
	if len(revisions) != 1 || revisions[0].UnstableText != "Hi I" || revisions[0].StableText != "" {
		t.Fatalf("start of turn revisions = %+v", revisions)
	}

	pushFlux(t, listener, 1)
	awaitFlux(t, listener, func() bool { return listener.eager })
	if !listener.EagerEndOfTurn() || listener.SpeechEndpointed() {
		t.Fatal("EagerEndOfTurn must open the eager window without ending the turn")
	}

	revisions = pushFlux(t, listener, 2)
	revisions = append(revisions, awaitFlux(t, listener, func() bool { return !listener.eager })...)
	if listener.EagerEndOfTurn() {
		t.Fatal("TurnResumed must close the eager window")
	}
	if len(revisions) == 0 || revisions[len(revisions)-1].StableText+revisions[len(revisions)-1].UnstableText != "Hi I need to cancel my subscription please" {
		t.Fatalf("resumed revisions = %+v", revisions)
	}

	pushFlux(t, listener, 3)
	awaitFlux(t, listener, func() bool { return listener.eager })
	pushFlux(t, listener, 4)
	awaitFlux(t, listener, func() bool { return listener.endOfTurn })
	if listener.EagerEndOfTurn() || !listener.SpeechEndpointed() {
		t.Fatal("EndOfTurn must end the turn and close the eager window")
	}

	final, err := listener.Finalize(context.Background(), 5*1_280)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final || final.StableText != "Hi I need to cancel my subscription please." {
		t.Fatalf("final = %+v", final)
	}
	if got := fake.forces.Load(); got != 0 {
		t.Fatalf("ForceEndTurn sent for a turn Flux had ended: %d", got)
	}
	if listener.SpeechEndpointed() {
		t.Fatal("the ended turn leaked into the next utterance")
	}
}

// When the engine's gate ends the utterance first, the open turn is ended with
// ForceEndTurn, and the next utterance reuses the connection from a clean
// transcript.
func TestFluxFinalizeForcesAnOpenTurnAndReusesTheStream(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t,
		turnInfo("StartOfTurn", "hello"),
		turnInfo("Update", "hello there"),
		turnInfo("StartOfTurn", "and again"),
	)
	listener := fluxListener(t, fake, FluxConfig{})
	pushFlux(t, listener, 0)
	pushFlux(t, listener, 1)
	awaitFlux(t, listener, func() bool { return listener.current == "hello there" })
	first, err := listener.Finalize(context.Background(), 2*1_280)
	if err != nil {
		t.Fatal(err)
	}
	if first.StableText != "hello there" || fake.forces.Load() != 1 {
		t.Fatalf("forced final = %+v, forces = %d", first, fake.forces.Load())
	}

	revisions := pushFlux(t, listener, 0)
	revisions = append(revisions, awaitFlux(t, listener, func() bool { return listener.current == "and again" })...)
	var deltas strings.Builder
	for _, revision := range revisions {
		deltas.WriteString(revision.Delta)
	}
	if deltas.String() != "and again" {
		t.Fatalf("second utterance deltas = %q", deltas.String())
	}
	if got := fake.accepts.Load(); got != 1 {
		t.Fatalf("connections dialled = %d, want 1", got)
	}
}

// A pause Flux hears as a turn end but the engine's gate does not end the
// utterance: the ended turn becomes the utterance's settled text and the next
// turn its unstable tail.
func TestFluxTurnsInsideOneUtteranceAccumulate(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t,
		turnInfo("StartOfTurn", "Count the animals."),
		turnInfo("EndOfTurn", "Count the animals.", "model"),
		turnInfo("StartOfTurn", "A cat"),
	)
	listener := fluxListener(t, fake, FluxConfig{})
	pushFlux(t, listener, 0)
	pushFlux(t, listener, 1)
	awaitFlux(t, listener, func() bool { return listener.endOfTurn })
	revisions := pushFlux(t, listener, 2)
	revisions = append(revisions, awaitFlux(t, listener, func() bool { return listener.current == "A cat" })...)
	if len(revisions) != 1 || revisions[0].StableText != "Count the animals." ||
		revisions[0].UnstableText != " A cat" {
		t.Fatalf("accumulated revisions = %+v", revisions)
	}
	if listener.SpeechEndpointed() {
		t.Fatal("a new StartOfTurn must clear the previous turn's end")
	}
	final, err := listener.Finalize(context.Background(), 3*1_280)
	if err != nil {
		t.Fatal(err)
	}
	if final.StableText != "Count the animals. A cat" {
		t.Fatalf("final = %+v", final)
	}
}

// A natural EndOfTurn and the gate's ForceEndTurn can cross on the wire. The
// force is then answered by a warning, and that warning must be consumed by
// this utterance: left behind, it would end the next utterance's drain before
// its own turn had ended.
func TestFluxCrossedForceIsAnsweredWithinItsUtterance(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t,
		turnInfo("StartOfTurn", "one"),
		turnInfo("StartOfTurn", "two"),
		turnInfo("Update", "two three"),
	)
	listener := fluxListener(t, fake, FluxConfig{})
	pushFlux(t, listener, 0)
	awaitFlux(t, listener, func() bool { return listener.turnActive })
	fake.mu.Lock()
	fake.crossNext = true
	fake.mu.Unlock()
	started := time.Now()
	first, err := listener.Finalize(context.Background(), 1_280)
	if err != nil {
		t.Fatal(err)
	}
	// The warning is the force's answer. A listener that did not count it
	// would wait out the drain timeout and drop the stream to recover.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("finalize waited %v for an answer it had already received", elapsed)
	}
	if first.StableText != "one" {
		t.Fatalf("crossed final = %+v", first)
	}
	listener.mu.Lock()
	outstanding, open := listener.forcesOutstanding, listener.stream != nil
	listener.mu.Unlock()
	if outstanding != 0 || !open {
		t.Fatalf("after finalize: forces outstanding = %d, stream open = %v", outstanding, open)
	}
	pushFlux(t, listener, 0)
	pushFlux(t, listener, 1)
	awaitFlux(t, listener, func() bool { return listener.current == "two three" })
	second, err := listener.Finalize(context.Background(), 2*1_280)
	if err != nil {
		t.Fatal(err)
	}
	if second.StableText != "two three" {
		t.Fatalf("second utterance final = %+v", second)
	}
	if got := fake.accepts.Load(); got != 1 {
		t.Fatalf("connections dialled = %d, want 1", got)
	}
}

func TestFluxEndUtteranceDiscardsTheOpenTurn(t *testing.T) {
	t.Parallel()
	fake := newFakeFlux(t, turnInfo("StartOfTurn", "never mind"), turnInfo("StartOfTurn", "fresh"))
	listener := fluxListener(t, fake, FluxConfig{})
	pushFlux(t, listener, 0)
	awaitFlux(t, listener, func() bool { return listener.turnActive })
	if err := listener.EndUtterance(); err != nil {
		t.Fatal(err)
	}
	if fake.forces.Load() != 1 {
		t.Fatalf("an abandoned open turn was not ended at the service: forces = %d", fake.forces.Load())
	}
	pushFlux(t, listener, 0)
	awaitFlux(t, listener, func() bool { return listener.current == "fresh" })
	final, err := listener.Finalize(context.Background(), 1_280)
	if err != nil {
		t.Fatal(err)
	}
	if final.StableText != "fresh" {
		t.Fatalf("abandoned words opened the next utterance: %+v", final)
	}
}

func TestFluxReportsServiceErrors(t *testing.T) {
	t.Parallel()
	payload, _ := json.Marshal(map[string]any{
		"type": "Error", "sequence_id": 1, "code": "INVALID_QUERY", "description": "bad eot_threshold",
	})
	fake := newFakeFlux(t, string(payload))
	listener := fluxListener(t, fake, FluxConfig{})
	pushFlux(t, listener, 0)
	listener.mu.Lock()
	current := listener.stream
	listener.mu.Unlock()
	select {
	case err := <-current.readErr:
		if !strings.Contains(err.Error(), "INVALID_QUERY: bad eot_threshold") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the service error was not surfaced")
	}
}

func TestFluxRefusesSettingsItCannotHonour(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		config FluxConfig
		want   string
	}{
		{"nova model", FluxConfig{Model: "nova-3"}, "not a Flux model"},
		{"v1 endpoint", FluxConfig{URL: "wss://api.deepgram.com/v1/listen"}, "/v2/listen"},
		{"eot low", FluxConfig{EOTThreshold: 0.4}, "0.5 to 1.0"},
		{"eager high", FluxConfig{EagerEOTThreshold: 0.95}, "0.3 to 0.9"},
		{"eager above default eot", FluxConfig{EagerEOTThreshold: 0.8}, "exceeds the eot_threshold in force, 0.7"},
		{"eager above eot", FluxConfig{EOTThreshold: 0.6, EagerEOTThreshold: 0.65}, "exceeds"},
		{"timeout short", FluxConfig{EOTTimeout: 100 * time.Millisecond}, "500ms to 60s"},
		{"timeout fraction", FluxConfig{EOTTimeout: 1500*time.Millisecond + time.Microsecond}, "whole milliseconds"},
		{"hints on english", FluxConfig{LanguageHints: []string{"en"}}, "English only"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := testCase.config
			config.APIKey = "key"
			_, err := NewFluxListener(config)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}

	listener, err := NewFluxListener(FluxConfig{APIKey: "key", URL: "ws://127.0.0.1:1/v2/listen"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = listener.PushFrame(context.Background(), v1.AudioFrame{SampleRateHz: 22_050, PCM16LE: tone(160)})
	if err == nil || !strings.Contains(err.Error(), "22050") {
		t.Fatalf("an unsupported sample rate was dialled: %v", err)
	}
}

func turnInfoAt(event, transcript string, windowEnd float64) string {
	message := map[string]any{
		"type": "TurnInfo", "event": event, "turn_index": 0, "transcript": transcript,
		"audio_window_start": 0, "audio_window_end": windowEnd, "end_of_turn_confidence": 0.1,
		"words": []map[string]any{{"word": "w", "confidence": 0.9, "start": 0, "end": 0.1}},
	}
	payload, _ := json.Marshal(message)
	return string(payload)
}

// The first utterance of a session waits for the connection and then sends a
// burst. Finalize arrives before Flux has said anything about that audio; it
// must wait for the words rather than end a turn that has not begun.
func TestFluxFinalizeWaitsForTheAudioItSentToBeTranscribed(t *testing.T) {
	t.Parallel()
	// Twelve 80 ms frames: almost a second of speech, sent in one burst.
	fake := newFakeFlux(t,
		turnInfoAt("Update", "", 0.08),
		turnInfoAt("StartOfTurn", "What is the capital of France?", 0.96),
	)
	fake.lateAfter, fake.lateDelay = 1, 400*time.Millisecond
	listener := fluxListener(t, fake, FluxConfig{})
	for index := uint64(0); index < 12; index++ {
		pushFlux(t, listener, index)
	}
	final, err := listener.Finalize(context.Background(), 12*1_280)
	if err != nil {
		t.Fatal(err)
	}
	if final.StableText != "What is the capital of France?" {
		t.Fatalf("finalize did not wait for the burst to be transcribed: %+v", final)
	}
	if fake.forces.Load() != 1 {
		t.Fatalf("ForceEndTurn messages = %d, want 1 once the turn had begun", fake.forces.Load())
	}
}
