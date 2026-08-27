package cascade

import (
	"context"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/interaction"
)

// The two policies here decide about a turn that is still happening, and both
// of them were dead weight until they were wired to something: a projection
// that cannot close a turn is an opinion, and a backchannel decision nothing
// emits is a model call with no output.

// projectEndpoint asks the floor policy whether the turn is about to end, and
// closes it when the answer is yes.
//
// Only a *projected* endpoint is acted on here. An ordinary one - silence past
// the threshold - is already the acoustic gate's, and honouring both would end
// every turn twice. That split is also the honest one for measurement: a
// projected endpoint that turns out to be wrong cut the user off, and a late
// one only cost latency, so the two failures must not be averaged together.
func (runtime *runtime) projectEndpoint(ctx context.Context, decision interaction.Context) (bool, error) {
	if !runtime.policies.Floor.EngineOwned() {
		return false, nil
	}
	endpoint := runtime.policies.Floor.Endpoint(decision)
	if endpoint.Act == interaction.ActCallTool {
		runtime.actSilently(decision)
	}
	if endpoint.Act == interaction.ActSpeakThrough {
		// The one act that produces speech without ending a turn. It is handled
		// here rather than by the caller because the caller only ever learns
		// whether the turn ended, and this is the case where it did not and
		// something still has to happen.
		runtime.interject(decision)
	}
	if !endpoint.Ended || !endpoint.Projected {
		return false, nil
	}
	// A turn taken from somebody still speaking is not a turn they offered,
	// and what belongs in it is different. The voice is told which it got.
	runtime.setInterjecting(decision.Revision.ID, endpoint.Act == interaction.ActInterrupt)
	runtime.audioMu.Lock()
	if runtime.acoustic == nil {
		runtime.audioMu.Unlock()
		runtime.finishInterjectingEndpoint()
		return false, nil
	}
	endMS, stopped := runtime.acoustic.ForceStop()
	utteranceID := runtime.utteranceID
	runtime.audioMu.Unlock()
	if !stopped {
		runtime.finishInterjectingEndpoint()
		return false, nil
	}
	return true, runtime.onUserSpeechStopped(ctx, utteranceID, endMS, runtime.scheduler.NowNS())
}

// backchannel decides whether to say "mm-hm" while the user is still talking.
//
// The decision runs off the audio path. A policy model takes tens of
// milliseconds, audio frames arrive every hundred, and a decision that made
// the recogniser wait would trade the thing that makes a system feel alive for
// the thing that makes it feel slow. One decision is in flight at a time -
// asking again about a turn we are already thinking about would spend tokens
// to get the same answer.
func (runtime *runtime) backchannel(ctx context.Context, decision interaction.Context) {
	if runtime.policies.Backchannel.Name() == "off" {
		return
	}
	if !runtime.continuing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer runtime.continuing.Store(false)
		outcome, err := runtime.policies.Backchannel.Decide(ctx, decision)
		if err != nil || outcome.Choice == interaction.BackchannelNone || outcome.Token == "" {
			// A policy model that failed is a policy that is off for this
			// decision, not a session that breaks. Silence is always safe.
			return
		}
		// The state may have moved while the model was thinking. Interjecting
		// into a turn that has since ended is worse than not interjecting.
		state := runtime.duplex.Snapshot()
		if !state.UserSpeaking || state.AgentSpeaking {
			return
		}
		utterance := action.Utterance{
			ID: idFor("continuer", runtime.sequence.Add(1)), Text: outcome.Token,
			SourceRevision: decision.Revision.ID, Continuer: true,
			SpokeOver: true,
		}
		// A continuer carries no assistant item. It is not part of the answer
		// and it is not something the model said - it is the runtime showing
		// the user it is still there, and putting it in the trajectory would
		// teach the next continuation that the agent had spoken.
		if err := runtime.speech.Enqueue(utterance, "voice"); err != nil {
			runtime.fail("backchannel_error", err)
		}
	}()
}

// continuing guards the single in-flight backchannel decision.
type inFlight = atomic.Bool
