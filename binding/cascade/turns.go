package cascade

import (
	"context"
	"errors"

	"github.com/bojieli/OpenRealtime/binding"

	"github.com/bojieli/OpenRealtime/interaction"
)

// Turn detection is the one floor decision a client may take from the engine.
//
// A client that switches server voice-activity detection off is not asking for
// a tuning change; it is saying that it knows where its turns end and will say
// so. Two things follow, and both have to happen or the client gets a session
// that half-listens to it: the acoustic gate stops ending turns on silence,
// and the engine stops creating responses on its own. A server that kept doing
// either would be answering turns the client had not finished declaring.
//
// Nothing else changes. The gate still runs, because it answers "is the user
// speaking" for every interaction policy; observations still commit the moment
// they arrive, because commit is unconditional whoever owns the floor. What
// waits is only the decision to act, and the wake-up it waits for is the
// client's own response.create.

// manualTurns reports whether the client declared it will drive turns.
func (runtime *runtime) manualTurns() bool {
	runtime.settingsMu.RLock()
	defer runtime.settingsMu.RUnlock()
	return runtime.settings.ManualTurns
}

// CommitAudio ends the current turn where the client says it ended.
//
// It reports the item the turn was committed as, so the client can correlate
// the transcript that follows. Committing an empty buffer is refused rather
// than silently accepted: a client that commits nothing is not declaring a
// turn, and manufacturing one would put an empty observation in the log.
func (runtime *runtime) CommitAudio(ctx context.Context) error {
	runtime.inputMu.Lock()
	defer runtime.inputMu.Unlock()
	if !runtime.manualTurns() {
		return errors.New(
			"this session runs server voice-activity detection, which owns input commitment: " +
				"set turn_detection to null in session.update to declare turns yourself")
	}
	runtime.audioMu.Lock()
	utteranceID := runtime.utteranceID
	endMS := 0
	if runtime.acoustic != nil {
		// The gate's own opinion about where the turn ended is discarded here
		// - the client just gave one - but its sample accounting is not, so
		// the committed item still knows how much audio it covers.
		endMS, _ = runtime.acoustic.ForceStop()
	}
	pending := runtime.pending
	runtime.pending = nil
	runtime.audioMu.Unlock()
	if utteranceID == "" {
		return errors.New("the input audio buffer is empty")
	}
	// The acknowledgement comes first, because it names the item every later
	// event about this turn refers to. A client that saw the transcript before
	// it was told which item the transcript belonged to would have to guess.
	if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
		Committed: true, ItemID: utteranceID, AudioEndMS: endMS,
	}); err != nil {
		return err
	}
	if len(pending) > 0 {
		// Whatever has not reached the recogniser yet is part of this turn.
		if _, err := runtime.observeAudio(ctx, pending, 0); err != nil {
			runtime.fail("asr_provider_error", err)
		}
	}
	err := runtime.onUserSpeechStopped(ctx, utteranceID, endMS, runtime.scheduler.NowNS())
	runtime.audioMu.Lock()
	if runtime.utteranceID == utteranceID {
		runtime.utteranceID = ""
	}
	runtime.audioMu.Unlock()
	return err
}

// CreateResponse asks for a response now.
//
// For a session running server VAD this is a nudge: the endpoint already
// created one, and the wake-up finds nothing waiting. For a client that took
// the floor it is the request the deferral policy has been waiting for, and it
// is the only wake-up in the system that does not come from a state
// transition.
func (runtime *runtime) CreateResponse(context.Context) error {
	runtime.gate.RequestResponse()
	return nil
}

// deferralFor selects the policy a session's turn detection implies.
func deferralFor(configured interaction.Deferral, manual bool) interaction.Deferral {
	if manual {
		return interaction.NewClientDriven()
	}
	return configured
}
