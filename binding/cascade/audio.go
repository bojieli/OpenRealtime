package cascade

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Audio drives the whole input path for one frame.
//
// The acoustic gate is upstream of the observer rather than inside it, and
// deliberately: its decision is not only a perception decision. It answers "is
// the user speaking", which is one of the two facts the duplex state owns and
// which every interaction policy reads. An observer that hid that answer
// inside itself would make the rest of the system ask it a second time.
func (runtime *runtime) Audio(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Kind != perception.FrameAudio {
		return errors.New("audio path requires an audio frame")
	}
	manual := runtime.manualTurns()
	runtime.audioMu.Lock()
	acoustic, err := runtime.acousticFor(frame.SampleRateHz)
	if err != nil {
		runtime.audioMu.Unlock()
		return err
	}
	result, err := acoustic.Push(frame.PCM16LE)
	if err != nil {
		runtime.audioMu.Unlock()
		return err
	}
	now := runtime.scheduler.NowNS()
	started, stopped := result.Started, result.Stopped
	if manual && stopped {
		// The client owns the floor. Silence is not an endpoint here; it is
		// silence, and the turn ends when the client says so. The gate has
		// already closed itself, so it is reopened on the next audible frame
		// and this turn continues.
		stopped = false
		runtime.acoustic.Reopen()
	}
	if started {
		runtime.utteranceID = idFor("item", runtime.sequence.Add(1))
		runtime.speechStartNS = now
		runtime.lastStable, runtime.lastCanonical = "", 0
		runtime.heard = interaction.Revision{}
		runtime.pauseStartNS = 0
	}
	if manual {
		// The client owns the buffer. What it appended is the turn, whether or
		// not the gate thinks any of it was speech - a client that sends two
		// seconds of a quiet page and commits is declaring a turn, and gating
		// its audio away would answer a turn it never got to make.
		admittedByClient := frame.PCM16LE
		if result.Started {
			admittedByClient = result.Audio
		}
		result.Audio = admittedByClient
		if runtime.utteranceID == "" {
			runtime.utteranceID = idFor("item", runtime.sequence.Add(1))
			runtime.speechStartNS = now
			runtime.lastStable, runtime.lastCanonical = "", 0
			runtime.heard = interaction.Revision{}
			runtime.pauseStartNS = 0
			started = true
		}
	}
	utteranceID := runtime.utteranceID
	admitted := result.Audio
	var due bool
	if len(admitted) > 0 {
		runtime.pending = append(runtime.pending, perception.Frame{
			Kind: perception.FrameAudio, Source: frame.Source, CapturedNS: frame.CapturedNS,
			PCM16LE: admitted, SampleRateHz: frame.SampleRateHz,
		})
		cadence := uint64(runtime.audio.Cadence().Nanoseconds())
		due = stopped || runtime.lastObserveNS == 0 || now-runtime.lastObserveNS >= cadence
	}
	var batch []perception.Frame
	var latest interaction.Revision
	if due {
		batch, runtime.pending = runtime.pending, nil
		runtime.lastObserveNS = now
	}
	silenceNS := result.SilenceNS
	runtime.audioMu.Unlock()

	if started {
		// A new turn makes any speculation from the previous one an answer to
		// the wrong sentence. Discarding here rather than at the endpoint is
		// deliberate: the endpoint only *submits* the canonical observation,
		// and the safe point that might adopt a preparation runs afterwards on
		// the event loop - so discarding at the endpoint would throw the work
		// away a moment before the only thing that could use it.
		runtime.discardPreparations()
		runtime.duplex.UserSpeechStarted(now)
		if err := runtime.onUserSpeechStarted(ctx, utteranceID, result.AudioStartMS); err != nil {
			return err
		}
	}
	if len(batch) > 0 && runtime.observing(audioObserverName) {
		// A session that deselected the audio observer still runs the acoustic
		// gate - it answers "is the user speaking", which every interaction
		// policy reads - but it runs no recogniser. That is what factor F3's
		// video-only level asks for, and a level that quietly recognised
		// speech anyway would be the audio+video level under another name.
		observed, err := runtime.observeAudio(ctx, batch, silenceNS)
		if err != nil {
			runtime.fail("asr_provider_error", err)
		}
		latest = observed
	}
	if !started && !stopped {
		// A barge-in policy that waits needs its deadline driven by something.
		// Revisions are too slow and too irregular to be that something: a
		// recogniser's first partial can be half a second away, and a policy
		// whose timeout only fires when words happen to arrive is not a
		// timeout. Frames arrive every 100 ms whatever the recogniser is
		// doing, so the deadline is checked here.
		state := runtime.duplex.Snapshot()
		if state.Overlapping() {
			overlap := now - state.UserSpeechStartedNS
			if err := runtime.considerBargeIn(ctx, interaction.Revision{}, overlap); err != nil {
				return err
			}
		}
		// And the same argument for the opposite case. A policy about time -
		// "ask if I go quiet for fifteen seconds" - is about the moment when
		// nothing arrives, so nothing that arrives can drive it. Frames do.
		if state.Silent() {
			runtime.considerQuiet(ctx, now, state)
		}
	}
	if stopped && runtime.holdsThroughPause(now, latest, silenceNS) {
		stopped = false
	}
	if stopped {
		return runtime.onUserSpeechStopped(ctx, utteranceID, result.AudioEndMS, now)
	}
	return nil
}

// holdsThroughPause asks whether the silence that closed the gate is the end
// of the turn or a pause inside it.
//
// The gate answers "is the user still audible", and that is the wrong question
// to end a turn on. A person searching for a word goes quiet exactly like a
// person who has finished, and half a second is a short hesitation - so an
// acoustic threshold splits one request into several, each answered
// separately, each answer cancelling the last. The floor already owns the
// other question, and a client-owned floor already reopens the gate on this
// exact reasoning; this is the engine-owned case of it.
//
// The revision is passed without Final set on purpose. Final is the gate's
// verdict, and the gate's verdict is what is under review here: the floor is
// being asked what the words and the silence say, not to agree with the
// mechanism that called it.
func (runtime *runtime) holdsThroughPause(nowNS uint64, latest interaction.Revision, silenceNS uint64) bool {
	if !runtime.policies.Floor.EngineOwned() {
		return false
	}
	if latest.Empty() {
		runtime.audioMu.Lock()
		latest = runtime.heard
		runtime.audioMu.Unlock()
	}
	if latest.Empty() {
		return false
	}
	latest.Final = false
	// How long this pause has actually lasted, which is what the hold is
	// bounded by. Each Reopen zeroes the gate's own counter, so asking the
	// gate would restart the clock on every hold and the bound would never
	// arrive - the turn would end when the model stopped saying "continuing",
	// which is exactly the runaway the bound exists to prevent.
	runtime.audioMu.Lock()
	// A pause that the speaker has since talked through is not one pause.
	//
	// The clock deliberately survives a hold, because reopening the gate zeroes
	// the gate's own counter and a bound measured from there would restart on
	// every hold and never arrive. But it must not survive the speaker actually
	// saying something: this is the clock a liveness bound is measured against,
	// and without this it reported twenty seconds of silence across a stretch
	// in which somebody had spoken three sentences - so the bound fired in the
	// middle of a conversation and the agent interrupted a monologue it had
	// been asked not to interrupt.
	if spoken := latest.Text(); spoken != runtime.pauseHeard {
		runtime.pauseHeard = spoken
		runtime.pauseStartNS = 0
	}
	if runtime.pauseStartNS == 0 && nowNS > silenceNS {
		runtime.pauseStartNS = nowNS - silenceNS
	}
	pauseStart := runtime.pauseStartNS
	runtime.audioMu.Unlock()
	if pauseStart != 0 && nowNS > pauseStart {
		silenceNS = nowNS - pauseStart
	}
	latest.SilenceNS = silenceNS
	// The duplex state still says the user is speaking, because the transition
	// is published by the endpoint this call is deciding whether to make. Both
	// corrections are the same one: the question is about the pause that has
	// just started, so the context describes that pause rather than the moment
	// before it.
	state := runtime.duplex.Snapshot()
	state.UserSpeaking = false
	decision := interaction.Context{NowNS: nowNS, Duplex: state, Revision: latest}
	// This is the moment that decides whether a pause ends a turn, and it is
	// the moment a floor that reads the conversation most needs to read it.
	// Without this the interaction model can only add endpoints and never
	// withhold one: every silence-driven ending goes through here, and a floor
	// handed no conversation falls back to the rule it was installed to
	// replace. "Don't interrupt me while I think" cannot work from anywhere
	// else.
	if runtime.policies.Interaction != nil {
		situation := runtime.situation(decision)
		decision.Situation = &situation
	}
	endpoint := runtime.policies.Floor.Endpoint(decision)
	// The pause decision is recorded like any other. It was invisible until it
	// was, and it is the one that decides whether a silence ends a turn - the
	// single most consequential call the floor makes.
	if recorder := runtime.policies.ShadowInteraction; recorder != nil && decision.Situation != nil {
		recorder(interaction.ShadowDecision{
			NowNS: nowNS, Situation: decision.Situation.Render(), Act: string(endpoint.Act),
			Predicates: map[string]string{"where": "pause", "ended": strconv.FormatBool(endpoint.Ended)},
			Agreed:     endpoint.Ended,
		})
	}
	// The one act that produces speech without ending a turn has to be acted on
	// wherever it is decided. It was handled where partials are processed and
	// not here, so a speak-through chosen at a pause was decided and dropped -
	// nine of them in one conversation, and the two that did reach the
	// interjection were the only ones anybody could have heard.
	if endpoint.Act == interaction.ActCallTool {
		runtime.actSilently(decision)
	}
	if endpoint.Act == interaction.ActSpeakThrough {
		runtime.interject(decision)
	}
	if endpoint.Ended {
		runtime.setInterjecting(decision.Revision.ID, endpoint.Act == interaction.ActInterrupt)
		runtime.audioMu.Lock()
		runtime.pauseStartNS = 0
		runtime.audioMu.Unlock()
		return false
	}
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	if runtime.acoustic == nil {
		return false
	}
	// The gate has already closed itself, so it is reopened and this turn
	// continues on the next audible frame.
	runtime.acoustic.Reopen()
	return true
}

// onUserSpeechStarted applies the barge-in policy at the moment sound begins.
//
// At this instant there are no words yet, so there is nothing to classify: an
// immediate policy decides here and a policy that waits decides later, as
// revisions arrive. Both are the interaction plane's decision; carrying it out
// is here.
func (runtime *runtime) onUserSpeechStarted(ctx context.Context, utteranceID string, startMS int) error {
	if err := runtime.considerBargeIn(ctx, interaction.Revision{}, 0); err != nil {
		return err
	}
	if runtime.manualTurns() {
		// Voice-activity events describe a detector the client turned off.
		// Reporting them anyway would tell a client that took the floor what
		// the server thinks it is doing.
		return nil
	}
	return runtime.sink.Activity(ctx, binding.ActivityEvent{
		Started: true, ItemID: utteranceID, AudioStartMS: startMS,
	})
}

// considerBargeIn asks whether the overlap in progress should stop the agent.
//
// It is called once when sound starts and again on every revision while the
// overlap continues, which is what makes a waiting policy useful: the first
// call has no words, and by the third there is usually enough to tell an
// interruption from someone saying "mm-hm".
func (runtime *runtime) considerBargeIn(
	ctx context.Context, revision interaction.Revision, overlapNS uint64,
) error {
	state := runtime.duplex.Snapshot()
	if !state.Overlapping() {
		return nil
	}
	decision := interaction.Context{
		NowNS: runtime.scheduler.NowNS(), Duplex: state, Revision: revision,
	}
	if runtime.speech.ActiveSpokeOver() {
		// The agent is talking over speech it chose to talk over, so this
		// overlap is not somebody taking the floor from it - it is the reason
		// the act exists. Cancelling here would have the agent abandon its own
		// backchannel for saying it was listening, and abandon its own
		// correction for the sentence it is correcting.
		//
		// Asked before the classifier, because the answer does not depend on
		// what the overlapping speech turns out to be, and that call costs up
		// to 150ms on every partial for the length of the interjection.
		return nil
	}
	evidence := interaction.OverlapEvidence("")
	if !revision.Empty() {
		// Classification is bounded hard. A barge-in decision that arrives
		// after the user has finished their sentence is not a decision.
		classify, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		evidence = runtime.policies.Overlap.Classify(classify, decision)
		cancel()
	}
	outcome := runtime.policies.BargeIn.Decide(interaction.BargeInInput{
		Context: decision, OverlapNS: overlapNS, Evidence: evidence,
	})
	if !outcome.Cancel {
		return nil
	}
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", outcome.Reason, eventloop.ErrInterrupted))
	cancelled, heard := runtime.speech.Cancel(outcome.Reason)
	if err := runtime.recordCancellations(cancelled, "barge-in", eventloop.PriorityInterrupt); err != nil {
		return err
	}
	if heard {
		// Something was already audible. The repair policy decides what that
		// costs; the duplex state records that playback has stopped.
		runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	}
	return nil
}

func (runtime *runtime) onUserSpeechStopped(ctx context.Context, utteranceID string, endMS int, now uint64) error {
	runtime.duplex.UserSpeechStopped(now)
	var observations []perception.Observation
	var flushErr error
	durationMS := uint64(0)
	if runtime.observing(audioObserverName) {
		observations, flushErr = runtime.audio.Flush(ctx)
		durationMS = runtime.audio.DurationMS()
		runtime.audio.Reset()
	}
	runtime.policies.Trigger.Reset()
	runtime.policies.Preparation.Reset()
	runtime.audioMu.Lock()
	runtime.pending, runtime.lastObserveNS = nil, 0
	runtime.audioMu.Unlock()

	if !runtime.manualTurns() {
		if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
			Stopped: true, ItemID: utteranceID, AudioEndMS: endMS,
		}); err != nil {
			return err
		}
	}
	if flushErr != nil {
		runtime.fail("asr_provider_error", flushErr)
		return nil
	}
	for _, observation := range observations {
		if err := runtime.sink.Transcript(ctx, binding.TranscriptEvent{
			ItemID: utteranceID, Text: observation.Text, Final: true,
			DurationSec: float64(durationMS) / 1000,
		}); err != nil {
			return err
		}
		if err := runtime.commitObservation(ctx, observation); err != nil {
			return err
		}
	}
	return nil
}

// observeAudio advances the recogniser and applies the trigger, preparation,
// and observation policies to what it produced.
func (runtime *runtime) observeAudio(
	ctx context.Context, frames []perception.Frame, silenceNS uint64,
) (interaction.Revision, error) {
	var latest interaction.Revision
	observations, err := runtime.audio.Observe(ctx, frames)
	if err != nil {
		return latest, err
	}
	for _, observation := range observations {
		revision := interaction.Revision{
			ID: observation.Revision, StableText: observation.StableText,
			UnstableText: strings.TrimPrefix(observation.Text, observation.StableText),
			Final:        observation.Final, ObservedNS: runtime.scheduler.NowNS(), SilenceNS: silenceNS,
		}
		latest = revision
		runtime.audioMu.Lock()
		runtime.heard = revision
		runtime.audioMu.Unlock()
		decision := interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(), Revision: revision,
		}
		// A waiting barge-in policy decides here rather than at onset, because
		// this is the first point at which there are words to classify.
		if decision.Duplex.Overlapping() {
			overlap := decision.NowNS - decision.Duplex.UserSpeechStartedNS
			if err := runtime.considerBargeIn(ctx, revision, overlap); err != nil {
				return latest, err
			}
		}
		// A policy set at the start of a long utterance has to govern the rest
		// of it, and the floor holds that utterance open while they keep
		// talking - so waiting for a commit means waiting for the very thing
		// the policy was meant to shape.
		runtime.noticeStandingInPartial(observation.StableText)
		// The conversation is attached here, before the predicates run, so
		// that whoever reads it sees the instant they are about to act on
		// rather than the one they leave behind. It is assembled once: the
		// floor may decide from it and the shadow may be scored against it,
		// and two assemblies of "now" taken a few milliseconds apart are two
		// different moments.
		if runtime.policies.Interaction != nil {
			state := runtime.situation(decision)
			decision.Situation = &state
		}
		shadow := runtime.beginShadow(decision)
		// A continuer is decided about here because here is where the words
		// are: a policy that only saw the acoustic envelope could not tell a
		// finished thought from a pause for breath.
		runtime.backchannel(ctx, decision)
		// The floor policy may end the turn before silence confirms it. It is
		// asked before the trigger, because a projected endpoint makes the
		// rest of this revision's processing part of the next turn.
		projected, err := runtime.projectEndpoint(ctx, decision)
		if err != nil {
			return latest, err
		}
		if projected {
			runtime.endShadow(shadow, decision, true, false)
			return latest, nil
		}
		// Preparation is consulted on every revision. It decides whether work
		// starts before the endpoint; it never decides what gets committed.
		runtime.prepare(ctx, decision)
		opportunity := runtime.policies.Trigger.Next(decision)
		runtime.endShadow(shadow, decision, false, opportunity.Open)
		if err := runtime.sink.Transcript(ctx, binding.TranscriptEvent{
			ItemID: runtime.currentUtterance(), Text: observation.Text,
		}); err != nil {
			return latest, err
		}
		if !opportunity.Open {
			continue
		}
		if runtime.config.ObservationPolicy != ObservationStablePartial {
			continue
		}
		stable := strings.TrimSpace(observation.StableText)
		if stable == "" || observation.StableText == runtime.stableText() {
			continue
		}
		partial := observation
		partial.Text = observation.StableText
		partial.Provisional = true
		if err := runtime.commitObservation(ctx, partial); err != nil {
			return latest, err
		}
		runtime.setStableText(observation.StableText)
	}
	return latest, nil
}

func (runtime *runtime) currentUtterance() string {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	return runtime.utteranceID
}

func (runtime *runtime) stableText() string {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	return runtime.lastStable
}

func (runtime *runtime) setStableText(text string) {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	runtime.lastStable = text
}

// commitObservation puts one observation into the canonical trajectory.
//
// Commit is unconditional: whatever the conversational state, an observation
// that perception produced enters the log the moment it arrives. Whether a
// continuation then runs is the deferral policy's decision, made by the gate.
func (runtime *runtime) commitObservation(ctx context.Context, observation perception.Observation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	revision := runtime.nextRevision()
	supersedes := uint64(0)
	runtime.audioMu.Lock()
	previous := runtime.lastCanonical
	if observation.Authority == trajectory.AuthorityUser && previous != 0 {
		supersedes = previous
	}
	if observation.Authority == trajectory.AuthorityUser {
		runtime.lastCanonical = revision
	}
	runtime.audioMu.Unlock()

	if supersedes != 0 {
		// A promoted revision replaces an earlier partial. Work derived from
		// the older prefix is stale, so it is interrupted, speech it produced
		// that nobody heard is cancelled, and speech somebody did hear is
		// recorded as owing a repair. Those are the only two outcomes there
		// are: audio that reached the user cannot be taken back, so the honest
		// move is to owe a correction rather than to pretend it was cancelled.
		runtime.coordinator.Interrupt(fmt.Errorf(
			"superseded by a newer canonical observation: %w", eventloop.ErrInterrupted))
		cancelled, _ := runtime.speech.CancelMatching("superseded by a newer observation", func(utterance actionUtterance) bool {
			return utterance.SourceRevision < revision
		})
		if err := runtime.recordCancellations(cancelled, "asr-revision", eventloop.PriorityRoutine); err != nil {
			return err
		}
		runtime.owe(revision)
	}
	if err := runtime.sink.Observation(ctx, observation); err != nil {
		return err
	}
	if observation.Authority == trajectory.AuthorityUser && observation.Final && !observation.Described {
		// Not a description of a picture. The pass asks what the person just
		// asked for or took back, and a narrator's account of a terminal
		// window is neither: measured, one screen description in five revoked
		// the standing policy the conversation was running on.
		runtime.noticeStanding(observation.Text)
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: observationEventType(observation), Source: observation.Observer, Channel: observationChannel(observation),
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		OccurredNS: observation.OccurredNS, SourceRevision: revision, SupersedesRevision: supersedes,
		Producer: observation.Producer(), Content: observation.Text, Observation: observation.Meta(),
		CorrelationID: runtime.currentUtterance(),
	})
	return err
}

// owe records that later evidence invalidated content the user already heard.
//
// It stops at the ledger. The trajectory item that makes the obligation
// visible to the model is raised at the next safe point instead, because the
// log will only accept a required repair once the assistant content it targets
// is recorded as played and the observation that invalidated it is committed -
// and neither of those has happened yet at the instant the supersession is
// noticed.
func (runtime *runtime) owe(byRevision uint64) {
	// The ledger is asked rather than the speech planner, because "was it
	// heard" is a question about the commit boundary and not about what is
	// currently playing. An utterance that finished a moment ago is exactly as
	// unrecoverable as one still in flight, and a cancellation path that only
	// saw the second would miss the common case.
	for _, commitment := range runtime.ledger.Crossed(func(commitment action.Commitment) bool {
		return commitment.Kind == action.KindSpeech &&
			commitment.SourceRevision != 0 && commitment.SourceRevision < byRevision
	}) {
		runtime.ledger.Invalidate(commitment.ID, byRevision)
	}
}

func observationEventType(observation perception.Observation) string {
	if observation.Final {
		return observation.Observer + ".endpoint"
	}
	return observation.Observer + ".partial"
}

func observationChannel(observation perception.Observation) string {
	if observation.Authority == trajectory.AuthorityUser {
		return "voice"
	}
	return "observation"
}

// Text commits something the client typed.
//
// It travels the same path speech does - a canonical observation through the
// event loop - so a typed turn and a spoken one are the same thing to
// everything downstream. Only the observer differs, and it says so.
func (runtime *runtime) Text(ctx context.Context, input binding.TextInput) error {
	authority := trajectory.AuthorityUser
	if input.Role == "system" {
		// A system message from a client is not the user talking. It is
		// context, and it must not be able to act like a request.
		authority = trajectory.AuthorityObserver
	}
	text := strings.TrimSpace(input.Text)
	media, err := runtime.retain(input.Images)
	if err != nil {
		return err
	}
	if text == "" && len(media) > 0 {
		// An observation carries text by construction, because text is what
		// survives after the images are pruned. A picture with nothing said
		// about it still gets a line saying it arrived, so the trajectory has
		// something to hang the handle on.
		text = "The user attached an image."
		if runtime.config.Narrator != nil {
			// And that line is all anything without eyes ever got. The fast
			// provider is handed the image; the interaction model, which
			// decides whether this is a moment to speak at, was handed the
			// sentence above and nothing else - so on the visual case it was
			// choosing between silence and speech about a screen it had been
			// told nothing about. Describing it is what the video observer
			// already does with every frame, and for the same reason.
			runtime.describeAttachment(input.Images, media, authority)
			return nil
		}
	}
	observation := perception.Observation{
		Text: text, Observer: "client", Source: "text",
		Authority: authority, Media: media, Final: true,
	}
	return runtime.commitObservation(ctx, observation)
}

// describeAttachment narrates an attached picture and commits it once there is
// something to say about it.
//
// Off the caller's goroutine, because that goroutine is the session's reader:
// blocking it for the length of a vision call would hold up the audio arriving
// behind it. The observation lands a beat later than the client's own
// item.created, which is the right order anyway - a picture is an observation
// and not a turn, and nothing is waiting on it to speak.
func (runtime *runtime) describeAttachment(
	images []binding.Image, media []trajectory.MediaRef, authority trajectory.Authority,
) {
	frames := make([]perception.Frame, 0, len(images))
	for index, image := range images {
		frames = append(frames, perception.Frame{
			Kind: perception.FrameImage, Source: "message", Index: uint64(index),
			CapturedNS: runtime.scheduler.NowNS(), Image: image.Bytes,
			MIMEType: image.MIMEType, Width: image.Width, Height: image.Height,
		})
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		text := "The user attached an image."
		described, err := runtime.config.Narrator.Narrate(
			runtime.ctx, frames, runtime.store.Snapshot())
		if described = strings.TrimSpace(described); err == nil && described != "" {
			// Named, because the description is the narrator talking about
			// what the user put in front of it, and a line that reads as the
			// user's own words would have the voice answer it as if it were.
			text = "The user attached an image showing: " + described
		}
		observation := perception.Observation{
			Text: text, Observer: "client", Source: "text",
			Authority: authority, Media: media, Final: true, Described: true,
		}
		if err := runtime.commitObservation(runtime.ctx, observation); err != nil &&
			runtime.ctx.Err() == nil {
			runtime.fail("observation_error", err)
		}
	}()
}

// retain stores pictures a client attached and returns handles to them.
//
// The trajectory references media rather than inlining it, because Snapshot is
// copied for every continuation request and an inlined screenshot would be
// copied with it. An adapter that can see resolves the handle; one that cannot
// reads the text and never pays for the bytes.
func (runtime *runtime) retain(images []binding.Image) ([]trajectory.MediaRef, error) {
	if len(images) == 0 {
		return nil, nil
	}
	refs := make([]trajectory.MediaRef, 0, len(images))
	for _, image := range images {
		reference, err := runtime.media.Retain(trajectory.MediaRef{
			MIMEType: image.MIMEType, Source: "message",
			Width: image.Width, Height: image.Height,
			CapturedNS: runtime.scheduler.NowNS(),
		}, image.Bytes)
		if err != nil {
			return nil, fmt.Errorf("retain attached image: %w", err)
		}
		refs = append(refs, reference)
	}
	return refs, nil
}
