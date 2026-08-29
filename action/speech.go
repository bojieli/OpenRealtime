package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Utterance is one unit of speech on its way to the world.
type Utterance struct {
	// ID is the commitment ID in the ledger.
	ID               string           `json:"id"`
	Text             string           `json:"text"`
	Phase            trajectory.Phase `json:"phase,omitempty"`
	SourceRevision   uint64           `json:"source_revision,omitempty"`
	AssistantItemIDs []string         `json:"assistant_item_ids,omitempty"`
	// Continuer marks a listener backchannel rather than a turn: a short "mm-hm"
	// the agent chose to emit while the user is still speaking. It is audio
	// reaching the user like any other, so it crosses the same commit boundary
	// - but it is not the agent taking the floor, and treating the user's
	// continuing speech over it as a barge-in would have the agent interrupt
	// itself for saying it was listening.
	Continuer bool `json:"continuer,omitempty"`
	// SpokeOver marks speech the agent began on purpose while somebody else
	// held the floor. A continuer is one case of it; an interruption and a
	// speak-through are the others.
	//
	// Barge-in asks whether somebody took the floor from the agent. Nobody did
	// here: the agent walked into speech that was already in progress, so the
	// overlap is the utterance's premise rather than evidence against it.
	// Measured, without this every correction was decided, worded, and then
	// cancelled by the sentence it was correcting after a single
	// hundred-millisecond frame had gone out, so nothing audible ever reached
	// the person it was for.
	//
	// It says which act produced the speech, and is not derived from the
	// duplex state when the audio is queued. The recogniser cuts one
	// continuous sentence into several stretches, and a rule keyed on the
	// stretch in progress fails exactly when the person keeps talking - which
	// is the case this exists for. What bounds talking over somebody is the
	// act's own brevity and scarcity, not barge-in.
	SpokeOver bool `json:"spoke_over,omitempty"`
}

// Frame is one paced block of audio.
type Frame struct {
	PCM16LE      []byte        `json:"-"`
	SampleRateHz uint32        `json:"sample_rate_hz"`
	Duration     time.Duration `json:"duration"`
	Final        bool          `json:"final,omitempty"`
}

// Outcome is how an utterance ended.
type Outcome struct {
	Completed bool   `json:"completed"`
	PlayedMS  uint64 `json:"played_ms"`
	Reason    string `json:"reason,omitempty"`
}

// SpeechSink is how paced audio reaches the world.
//
// Encoding is not this package's business: the sink is handed PCM16 at the
// synthesiser's rate and renders it however its transport requires - Realtime
// wire events, a WebRTC track, a file. Pacing is done in duration, so it is
// the same whatever the encoding turns out to be.
type SpeechSink interface {
	// Begin announces an utterance that is about to be emitted.
	Begin(context.Context, Utterance) error
	// Audio delivers one paced frame. It is called only after the ledger has
	// recorded that the boundary is being crossed.
	Audio(context.Context, Utterance, Frame) error
	// End completes the utterance with its terminal outcome. It is called
	// exactly once for every Begin, including when emission failed.
	End(context.Context, Utterance, Outcome) error
}

// SpeechReservationSink is the optional queue-lifecycle half of a speech
// sink. A renderer whose response lifetime includes asynchronously queued
// speech can reserve that work before the speech goroutine begins synthesis.
//
// Reserve is called before an utterance becomes visible to the queue worker.
// CancelReservation is called only when that accepted utterance is discarded
// before Begin. Once Begin is called, the ordinary Begin/End pair owns the
// reservation's lifetime.
type SpeechReservationSink interface {
	Reserve(Utterance) error
	CancelReservation(Utterance)
}

// SpeechConfig configures the planner.
type SpeechConfig struct {
	Provider v1.StreamingSpeechProvider
	Sink     SpeechSink
	Ledger   *Ledger
	// Playback is notified as audio is handed to the world, so the duplex
	// state can answer "is the agent speaking" from what actually crossed the
	// boundary rather than from what was decided.
	Playback PlaybackReporter
	// FrameDuration is the wire frame size. Zero selects 100 ms, which is what
	// the Realtime clients in the field expect.
	FrameDuration time.Duration
	// QueueDepth bounds how many utterances may wait. A bounded horizon is
	// what keeps a burst of decisions from becoming a backlog of speech that
	// no longer matches the conversation.
	QueueDepth int
	Scheduler  clock.Scheduler
}

// PlaybackReporter receives audio hand-off notifications.
type PlaybackReporter interface {
	AgentAudioHandedOff(utteranceID string, duration time.Duration) error
	AgentAudioStopped(atNS uint64)
}

// ErrSilentProducer means the caller tried to voice content whose producer had
// no voice authority. It is the second cognition boundary, enforced where
// output commits rather than where it is configured.
var ErrSilentProducer = errors.New("producer has no voice authority")

// ErrQueueFull means the bounded speech horizon is exhausted.
var ErrQueueFull = errors.New("speech queue is full")

type queuedUtterance struct {
	utterance Utterance
	epoch     uint64
}

// Speech plans and paces spoken output.
//
// Pacing is the whole point. A synthesiser produces audio faster than it plays,
// and emitting it as fast as it arrives makes "the agent is speaking" a
// fiction: everything would be on the wire in a moment, and nothing the user
// heard could be cancelled. Emitting at the rate the audio plays keeps the
// commit boundary meaningful.
type Speech struct {
	config    SpeechConfig
	scheduler clock.Scheduler
	queue     chan queuedUtterance

	mu           sync.Mutex
	epoch        uint64
	activeID     string
	active       Utterance
	activeCancel context.CancelCauseFunc
	closed       bool
}

// NewSpeech creates a speech planner.
func NewSpeech(config SpeechConfig) (*Speech, error) {
	if config.Provider == nil {
		return nil, errors.New("speech planning requires a streaming speech provider")
	}
	if config.Sink == nil {
		return nil, errors.New("speech planning requires a sink")
	}
	if config.Ledger == nil {
		return nil, errors.New("speech planning requires the irreversibility ledger")
	}
	if config.FrameDuration <= 0 {
		config.FrameDuration = 100 * time.Millisecond
	}
	if config.QueueDepth <= 0 {
		config.QueueDepth = 16
	}
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}
	return &Speech{
		config: config, scheduler: config.Scheduler,
		queue: make(chan queuedUtterance, config.QueueDepth),
	}, nil
}

// Enqueue accepts an utterance for emission.
//
// It refuses silent producers outright. That check lives here, at the point
// where content would cross into speech, because a rule enforced only at
// configuration time is a rule that a new binding can forget.
func (speech *Speech) Enqueue(utterance Utterance, speechAuthority string) error {
	utterance.Text = strings.TrimSpace(utterance.Text)
	if utterance.ID == "" || utterance.Text == "" {
		return errors.New("utterance requires an ID and non-empty text")
	}
	if speechAuthority == "silent" {
		return fmt.Errorf("%w: %s", ErrSilentProducer, utterance.ID)
	}
	if err := speech.config.Ledger.Prepare(Commitment{
		ID: utterance.ID, Kind: KindSpeech, AssistantItemIDs: utterance.AssistantItemIDs,
		SourceRevision: utterance.SourceRevision, Phase: utterance.Phase,
		SpeechAuthority: speechAuthority,
	}); err != nil {
		return err
	}
	if err := speech.config.Ledger.Queue(utterance.ID); err != nil {
		return err
	}
	speech.mu.Lock()
	epoch := speech.epoch
	closed := speech.closed
	speech.mu.Unlock()
	if closed {
		_, _ = speech.config.Ledger.Cancel(utterance.ID, "speech planner closed")
		return errors.New("speech planner is closed")
	}
	reserved := false
	if sink, ok := speech.config.Sink.(SpeechReservationSink); ok {
		if err := sink.Reserve(utterance); err != nil {
			_, _ = speech.config.Ledger.Cancel(utterance.ID, "speech sink refused queue reservation")
			return err
		}
		reserved = true
	}
	select {
	case speech.queue <- queuedUtterance{utterance: utterance, epoch: epoch}:
		return nil
	default:
		if reserved {
			speech.cancelReservation(utterance)
		}
		_, _ = speech.config.Ledger.Cancel(utterance.ID, "speech queue full")
		return ErrQueueFull
	}
}

// Run drains the queue until the context ends. It is one goroutine per
// session: speech is inherently serial, because a second voice talking over
// the first is not a feature.
func (speech *Speech) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			speech.drain("session ended")
			return
		case queued := <-speech.queue:
			speech.mu.Lock()
			stale := queued.epoch != speech.epoch
			speech.mu.Unlock()
			if stale {
				speech.cancelReservation(queued.utterance)
				_, _ = speech.config.Ledger.Cancel(queued.utterance.ID, "superseded before emission")
				continue
			}
			speech.emit(ctx, queued.utterance)
		}
	}
}

// Cancel stops the utterance in flight and drops everything queued behind it.
//
// It returns the commitments that never crossed the boundary, so a caller can
// record their cancellation in the trajectory, and separately reports whether
// the active utterance had already been partly heard - which is the input to
// the repair policy rather than a decision this package makes.
func (speech *Speech) Cancel(reason string) (cancelled []Commitment, heard bool) {
	speech.mu.Lock()
	speech.epoch++
	cancel := speech.activeCancel
	activeID := speech.activeID
	speech.mu.Unlock()

	if cancel != nil {
		cancel(fmt.Errorf("speech cancelled: %s", reason))
	}
	if activeID != "" {
		crossed, stopped := speech.stop(activeID, reason)
		heard = crossed
		if stopped != nil {
			cancelled = append(cancelled, *stopped)
		}
	}
	for {
		select {
		case queued := <-speech.queue:
			speech.cancelReservation(queued.utterance)
			if _, stopped := speech.stop(queued.utterance.ID, reason); stopped != nil {
				cancelled = append(cancelled, *stopped)
			}
		default:
			return cancelled, heard
		}
	}
}

// stop cancels one commitment and reports what this call actually did.
//
// The distinction it draws is between "cancelled by me" and "already
// terminal", which the ledger deliberately does not treat as an error -
// cancelling something twice is a normal race between the speech planner
// finishing and a supersession noticing. But a caller that recorded a
// cancellation for it anyway would submit a second cancelled transition for an
// assistant item already cancelled, and the log refuses that: the visibility
// lifecycle is append-only and cancelled does not follow cancelled.
//
// It returns the commitment only when this call moved it, so nothing downstream
// records a transition that already happened.
func (speech *Speech) stop(id, reason string) (crossed bool, cancelled *Commitment) {
	before, exists := speech.config.Ledger.Lookup(id)
	if !exists || before.State.Terminal() {
		return before.State.Crossed(), nil
	}
	crossed, err := speech.config.Ledger.Cancel(id, reason)
	if err != nil || crossed {
		return crossed, nil
	}
	commitment, exists := speech.config.Ledger.Lookup(id)
	if !exists {
		return false, nil
	}
	return false, &commitment
}

// CancelMatching stops the utterance in flight and every queued utterance that
// matches a predicate, leaving the rest alone. Supersession uses it: work
// derived from a prefix that newer evidence replaced is stale, but unrelated
// queued speech is not.
//
// It returns two sets and the difference between them is the whole point.
// Cancelled never reached the world and can simply be forgotten. Heard did
// reach it, wholly or in part, and cannot be taken back - a repair is damage
// limitation rather than reversal - so it is handed back for the repair policy
// to decide about rather than being classified here.
//
// It does not bump the epoch, unlike Cancel: an epoch bump invalidates every
// queued utterance, and the point of a predicate is that some of them survive.
func (speech *Speech) CancelMatching(reason string, match func(Utterance) bool) (cancelled, heard []Commitment) {
	if match == nil {
		cancelled, crossed := speech.Cancel(reason)
		if crossed {
			if commitment, exists := speech.config.Ledger.Lookup(speech.activeCommitment()); exists {
				heard = append(heard, commitment)
			}
		}
		return cancelled, heard
	}

	speech.mu.Lock()
	activeID, active, cancel := speech.activeID, speech.active, speech.activeCancel
	speech.mu.Unlock()
	if activeID != "" && match(active) {
		if cancel != nil {
			cancel(fmt.Errorf("speech cancelled: %s", reason))
		}
		crossed, stopped := speech.stop(activeID, reason)
		switch {
		case stopped != nil:
			cancelled = append(cancelled, *stopped)
		case crossed:
			if commitment, exists := speech.config.Ledger.Lookup(activeID); exists {
				heard = append(heard, commitment)
			}
		}
	}

	var keep []queuedUtterance
	for {
		select {
		case queued := <-speech.queue:
			if !match(queued.utterance) {
				keep = append(keep, queued)
				continue
			}
			speech.cancelReservation(queued.utterance)
			if _, stopped := speech.stop(queued.utterance.ID, reason); stopped != nil {
				cancelled = append(cancelled, *stopped)
			}
		default:
			for _, queued := range keep {
				select {
				case speech.queue <- queued:
				default:
				}
			}
			return cancelled, heard
		}
	}
}

func (speech *Speech) activeCommitment() string {
	speech.mu.Lock()
	defer speech.mu.Unlock()
	return speech.activeID
}

// ActiveSpokeOver reports whether the audio reaching the user right now was
// begun on purpose over somebody who already had the floor.
//
// Barge-in needs it, for every act that produces overlap deliberately: a
// backchannel, where cancelling would have the agent interrupt itself for
// saying it was listening, and an interruption or speak-through, where
// cancelling would have it abandon what it cut in to say the moment the
// sentence it cut into carried on.
func (speech *Speech) ActiveSpokeOver() bool {
	speech.mu.Lock()
	defer speech.mu.Unlock()
	return speech.activeID != "" && speech.active.SpokeOver
}

// ActiveOrdinary reports whether an ordinary response has claimed the speech
// planner but has not necessarily emitted audio yet.
//
// Duplex state deliberately begins at the first audio frame, because that is
// what the user can hear. Turn-taking needs one additional fact at the other
// side of that boundary: a synthesiser can be working on a response while the
// agent is still acoustically silent. If the user resumes during that window,
// treating the response as nonexistent lets it begin after they have already
// taken the floor. This method exposes only the pending fact; the barge-in
// policy still decides whether to cancel it.
func (speech *Speech) ActiveOrdinary() bool {
	speech.mu.Lock()
	defer speech.mu.Unlock()
	return speech.activeID != "" && !speech.active.SpokeOver
}

// Close stops accepting work and cancels what is queued.
func (speech *Speech) Close(reason string) []Commitment {
	speech.mu.Lock()
	speech.closed = true
	speech.mu.Unlock()
	cancelled, _ := speech.Cancel(reason)
	return cancelled
}

func (speech *Speech) drain(reason string) {
	for {
		select {
		case queued := <-speech.queue:
			speech.cancelReservation(queued.utterance)
			_, _ = speech.config.Ledger.Cancel(queued.utterance.ID, reason)
		default:
			return
		}
	}
}

func (speech *Speech) cancelReservation(utterance Utterance) {
	if sink, ok := speech.config.Sink.(SpeechReservationSink); ok {
		sink.CancelReservation(utterance)
	}
}

func (speech *Speech) emit(parent context.Context, utterance Utterance) {
	ctx, cancel := context.WithCancelCause(parent)
	speech.mu.Lock()
	speech.activeCancel = cancel
	speech.activeID = utterance.ID
	speech.active = utterance
	speech.mu.Unlock()
	defer func() {
		speech.mu.Lock()
		speech.activeCancel = nil
		speech.activeID = ""
		speech.active = Utterance{}
		speech.mu.Unlock()
		cancel(nil)
	}()

	outcome, err := speech.stream(ctx, utterance)
	if err != nil && outcome.Reason == "" {
		outcome.Reason = err.Error()
	}
	if outcome.PlayedMS > 0 {
		_ = speech.config.Ledger.Complete(utterance.ID, outcome.PlayedMS)
	} else if !outcome.Completed {
		_, _ = speech.config.Ledger.Cancel(utterance.ID, outcome.Reason)
	} else {
		_ = speech.config.Ledger.Complete(utterance.ID, 0)
	}
	_ = speech.config.Sink.End(ctx, utterance, outcome)
	if !outcome.Completed && speech.config.Playback != nil {
		speech.config.Playback.AgentAudioStopped(speech.scheduler.NowNS())
	}
}

func (speech *Speech) stream(ctx context.Context, utterance Utterance) (Outcome, error) {
	if err := speech.config.Sink.Begin(ctx, utterance); err != nil {
		return Outcome{Reason: "sink refused the utterance"}, err
	}

	var (
		sampleRate uint32
		buffer     []byte
		playedMS   uint64
		nextSend   time.Time
	)
	frameBytes := 0
	emitFrame := func(payload []byte, final bool) error {
		if len(payload) == 0 {
			return nil
		}
		duration := pcmDuration(len(payload), sampleRate)
		if err := speech.wait(ctx, nextSend); err != nil {
			return err
		}
		// The ledger records the crossing before the bytes move, so a
		// concurrent cancellation can never classify a frame that is already
		// on its way as safely droppable.
		if err := speech.config.Ledger.Emit(utterance.ID); err != nil {
			return err
		}
		if speech.config.Playback != nil {
			if err := speech.config.Playback.AgentAudioHandedOff(utterance.ID, duration); err != nil {
				return err
			}
		}
		if err := speech.config.Sink.Audio(ctx, utterance, Frame{
			PCM16LE: payload, SampleRateHz: sampleRate, Duration: duration, Final: final,
		}); err != nil {
			return err
		}
		playedMS += uint64(duration.Milliseconds())
		now := time.Now()
		if nextSend.Before(now) {
			nextSend = now
		}
		nextSend = nextSend.Add(duration)
		return nil
	}

	streamErr := speech.config.Provider.Stream(ctx, v1.SpeechPlan{
		CandidateID: utterance.ID, Text: utterance.Text,
	}, func(chunk v1.SpeechChunk) error {
		if sampleRate == 0 {
			sampleRate = chunk.SampleRateHz
			if sampleRate == 0 {
				return errors.New("speech provider reported no sample rate")
			}
			frameBytes = int(uint64(sampleRate) * uint64(speech.config.FrameDuration/time.Millisecond) / 1000 * 2)
			if frameBytes <= 0 {
				return errors.New("speech frame size is not positive")
			}
		}
		if chunk.SampleRateHz != sampleRate {
			return errors.New("speech provider changed sample rate within one utterance")
		}
		buffer = append(buffer, chunk.PCM16LE...)
		for len(buffer) >= frameBytes {
			if err := emitFrame(buffer[:frameBytes], false); err != nil {
				return err
			}
			buffer = buffer[frameBytes:]
		}
		return nil
	})
	if streamErr == nil && context.Cause(ctx) != nil {
		streamErr = context.Cause(ctx)
	}
	if streamErr == nil && sampleRate == 0 {
		streamErr = errors.New("speech provider returned no audio")
	}
	if streamErr == nil && len(buffer) > 0 {
		streamErr = emitFrame(buffer, true)
	}
	if streamErr != nil {
		return Outcome{PlayedMS: playedMS, Reason: streamErr.Error()}, streamErr
	}
	return Outcome{Completed: true, PlayedMS: playedMS}, nil
}

func (speech *Speech) wait(ctx context.Context, deadline time.Time) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}
	if deadline.IsZero() {
		return nil
	}
	delay := time.Until(deadline)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func pcmDuration(bytes int, sampleRate uint32) time.Duration {
	if sampleRate == 0 || bytes <= 0 {
		return 0
	}
	samples := int64(bytes / 2)
	return time.Duration(samples) * time.Second / time.Duration(sampleRate)
}
