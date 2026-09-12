package gptlive

import (
	"context"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
)

// Turn boundaries do not exist in the Live protocol, so this file invents them.
//
// The vendor is explicit that transcript fragments "do not define complete
// turns or include a transcript-done event", and that an application must not
// infer silence from a missing event. That is a reasonable contract for a
// captioning UI, which only ever appends. It is not enough for this binding:
// the background reasoner is handed a *turn*, and the trajectory records
// utterances, so something has to decide where one ends.
//
// Two signals are used, and the difference between them matters.
//
// The reliable one is session.delegation.created. When the endpoint delegates,
// it has decided the user's request is complete enough to need backend work -
// that is the endpoint's own judgement about a turn boundary, not this
// package's guess, and it arrives at exactly the moment the reasoner should
// start. Every delegated turn ends on this signal.
//
// The fallback is silence. A conversation contains turns the endpoint answers
// by itself and never delegates; without a second signal those never reach the
// trajectory, and the reasoner would later be asked to continue a conversation
// with holes in it. So a gap after the last fragment also closes a turn.
//
// The gap is a guess and is treated as one. It is long enough that a pause for
// breath does not split a sentence, and it is never the thing that starts
// backend work when the endpoint has spoken - a delegation always preempts it.
// Both are configurable because the right value depends on a deployment's
// speech, and neither is measured here: nothing in this package can tell a
// thinking pause from a finished sentence.
const (
	// defaultInputTurnGap closes a user turn that the endpoint did not
	// delegate. Conversational pauses cluster well under this; fragment
	// delivery is uneven, so a shorter window would cut sentences in half
	// more often than it would start the reasoner sooner.
	defaultInputTurnGap = 900 * time.Millisecond
	// defaultOutputTurnGap closes the assistant's own utterance. Audio deltas
	// re-arm it as well as transcript fragments, so it fires only once the
	// endpoint has genuinely stopped speaking rather than between two
	// sentences of one answer.
	defaultOutputTurnGap = 1200 * time.Millisecond
	// defaultSilenceHangover is how much carrier follows real speech before
	// the stream goes quiet. Two of the endpoint's 100 ms frames: long enough
	// that an ordinary gap between words is carried through as the pause it
	// is, short enough that a stop is audible as a stop within the second a
	// yielding agent is given.
	defaultSilenceHangover = 200 * time.Millisecond
)

// turnState is everything the boundary synthesis remembers. It is guarded by
// turnMu, which also serialises emission, so the synthesised events for one
// boundary reach the mirror together and in order.
type turnState struct {
	userSpeaking bool
	userText     strings.Builder
	userStartMS  int64
	userEndMS    int64
	userTimer    clock.Timer

	assistantSpeaking bool
	assistantText     strings.Builder
	assistantTimer    clock.Timer
	// silentFor is how much carrier has arrived since the last audible frame,
	// which is what bounds how much of it is passed on.
	silentFor time.Duration

	// delegation is the open client delegation the endpoint is waiting on, so
	// a completed answer can be returned against it.
	delegation string
}

// noteUserTranscript accumulates one user fragment and restarts the gap.
func (client *Client) noteUserTranscript(ctx context.Context, delta string, startMS, endMS int64) error {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	if delta == "" {
		return nil
	}
	opening := !client.turn.userSpeaking
	if opening {
		client.turn.userSpeaking = true
		client.turn.userStartMS = startMS
	}
	client.turn.userText.WriteString(delta)
	client.turn.userEndMS = endMS
	// The gap is armed before anything is emitted. Emitting first would
	// publish a boundary the caller can observe while the timer that closes
	// the turn does not yet exist, which is a race a test can lose and a
	// production reader can too.
	client.armUserTimer()
	if opening {
		// The endpoint has no speech-started event either. This is the moment
		// it first reports hearing words, which is what the duplex state and
		// the client's own view of the floor need.
		if err := client.emit(ctx, "input_audio_buffer.speech_started", map[string]any{
			"type": "input_audio_buffer.speech_started", "audio_start_ms": startMS,
		}); err != nil {
			return err
		}
	}
	// The fragment itself, under the Realtime name for a partial transcript.
	// It is what a caption needs and what the binding's transcript policy
	// reads; the committed turn still arrives at the boundary.
	return client.emit(ctx, "conversation.item.input_audio_transcription.delta", map[string]any{
		"type":  "conversation.item.input_audio_transcription.delta",
		"delta": delta, "start_ms": startMS, "end_ms": endMS,
	})
}

// noteAssistantTranscript forwards one assistant fragment and restarts its gap.
func (client *Client) noteAssistantTranscript(ctx context.Context, delta string) error {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	if delta == "" {
		return nil
	}
	client.turn.assistantSpeaking = true
	client.turn.assistantText.WriteString(delta)
	client.armAssistantTimer()
	return client.emit(ctx, "response.output_audio_transcript.delta", map[string]any{
		"type": "response.output_audio_transcript.delta", "delta": delta,
	})
}

// noteAssistantAudio takes one output frame and reports whether to forward it.
//
// An audible frame is speech: it opens or extends the utterance and restarts
// the gap. This is also what keeps a sentence from being cut off at its last
// transcript fragment - audio outlasts the transcript, because the samples that
// speak the end of a sentence are still arriving after the words are reported.
//
// A silent frame is the carrier. It never extends anything, or the gap would be
// restarted ten times a second for the life of the session and no utterance
// would ever close. It is still forwarded while an utterance is open, where it
// is the pause between two words rather than the gap between two turns.
func (client *Client) noteAssistantAudio(audible bool, duration time.Duration) bool {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	if audible {
		client.turn.assistantSpeaking = true
		client.turn.silentFor = 0
		client.armAssistantTimer()
		return true
	}
	if !client.turn.assistantSpeaking {
		return false
	}
	// Carrier inside an utterance is the pause between two words and belongs
	// to the speech around it - but only for as long as a pause lasts. Past
	// that the agent has stopped, and passing on more would report speech
	// that is not happening.
	client.turn.silentFor += duration
	return client.turn.silentFor <= client.config.SilenceHangover
}

// noteDelegation records the endpoint's request for backend work and ends the
// user's turn on it, which is what sets the reasoner going.
func (client *Client) noteDelegation(ctx context.Context, id, target string) error {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	if target == "client" && id != "" {
		client.turn.delegation = id
	}
	return client.flushUser(ctx)
}

// openDelegation reports the delegation a completed answer should be returned
// against, or empty when the endpoint asked for nothing.
func (client *Client) openDelegation() string {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	return client.turn.delegation
}

// armUserTimer restarts the user's gap. Callers hold turnMu.
func (client *Client) armUserTimer() {
	if client.turn.userTimer != nil {
		client.turn.userTimer.Stop()
	}
	client.turn.userTimer = client.config.Scheduler.AfterFunc(client.config.InputTurnGap, func() {
		client.turnMu.Lock()
		defer client.turnMu.Unlock()
		// The context is deliberately not the caller's: a gap expiring is this
		// package's own event, and emit already stops at a closed connection.
		_ = client.flushUser(context.Background())
	})
}

// armAssistantTimer restarts the assistant's gap. Callers hold turnMu.
func (client *Client) armAssistantTimer() {
	if client.turn.assistantTimer != nil {
		client.turn.assistantTimer.Stop()
	}
	client.turn.assistantTimer = client.config.Scheduler.AfterFunc(client.config.OutputTurnGap, func() {
		client.turnMu.Lock()
		defer client.turnMu.Unlock()
		_ = client.flushAssistant(context.Background())
	})
}

// flushUser closes the user's turn if one is open. Callers hold turnMu.
//
// It is idempotent because two things close a turn - a delegation and a gap -
// and both can fire for the same speech. Committing a turn twice would hand the
// reasoner the same question again and put a duplicate utterance in the
// trajectory, so the accumulator being empty is what makes the second one a
// no-op rather than a race to be avoided.
func (client *Client) flushUser(ctx context.Context) error {
	if client.turn.userTimer != nil {
		client.turn.userTimer.Stop()
		client.turn.userTimer = nil
	}
	if !client.turn.userSpeaking {
		return nil
	}
	heard := strings.TrimSpace(client.turn.userText.String())
	endMS := client.turn.userEndMS
	client.turn.userText.Reset()
	client.turn.userSpeaking = false
	if err := client.emit(ctx, "input_audio_buffer.speech_stopped", map[string]any{
		"type": "input_audio_buffer.speech_stopped", "audio_end_ms": endMS,
	}); err != nil {
		return err
	}
	if heard == "" {
		return nil
	}
	return client.emit(ctx, "conversation.item.input_audio_transcription.completed",
		map[string]any{
			"type":       "conversation.item.input_audio_transcription.completed",
			"transcript": heard,
		})
}

// flushAssistant closes the assistant's utterance. Callers hold turnMu.
//
// The response.done this ends with is synthetic in a way worth naming: it means
// "the endpoint stopped speaking", not "a response completed", because Live has
// no such thing. It is what the mirror needs to commit the utterance, and it is
// the honest translation of the only fact available.
func (client *Client) flushAssistant(ctx context.Context) error {
	if client.turn.assistantTimer != nil {
		client.turn.assistantTimer.Stop()
		client.turn.assistantTimer = nil
	}
	if !client.turn.assistantSpeaking {
		return nil
	}
	said := strings.TrimSpace(client.turn.assistantText.String())
	client.turn.assistantText.Reset()
	client.turn.assistantSpeaking = false
	client.turn.silentFor = 0
	// The answer that was waiting has now been spoken, so the delegation it
	// belonged to is finished. Keeping the ID would attach the next answer to a
	// request the endpoint has already closed.
	client.turn.delegation = ""
	if said != "" {
		if err := client.emit(ctx, "response.output_audio_transcript.done", map[string]any{
			"type": "response.output_audio_transcript.done", "transcript": said,
		}); err != nil {
			return err
		}
	}
	return client.emit(ctx, "response.done", map[string]any{"type": "response.done"})
}

// finishTurns closes whatever was still open when the session ended, so a
// conversation that stops mid-utterance still commits what was heard and said.
func (client *Client) finishTurns(ctx context.Context) {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	_ = client.flushUser(ctx)
	_ = client.flushAssistant(ctx)
}

// stopTurnTimers cancels both gaps so a closed connection fires nothing.
func (client *Client) stopTurnTimers() {
	client.turnMu.Lock()
	defer client.turnMu.Unlock()
	if client.turn.userTimer != nil {
		client.turn.userTimer.Stop()
		client.turn.userTimer = nil
	}
	if client.turn.assistantTimer != nil {
		client.turn.assistantTimer.Stop()
		client.turn.assistantTimer = nil
	}
}
