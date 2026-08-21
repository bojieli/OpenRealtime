package cascade

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	runtime.audioMu.Lock()
	if runtime.acoustic == nil {
		runtime.audioMu.Unlock()
		return errors.New("acoustic gate is not configured")
	}
	result, err := runtime.acoustic.Push(frame.PCM16LE)
	if err != nil {
		runtime.audioMu.Unlock()
		return err
	}
	now := runtime.scheduler.NowNS()
	started, stopped := result.Started, result.Stopped
	if started {
		runtime.utteranceID = idFor("item", runtime.sequence.Add(1))
		runtime.speechStartNS = now
		runtime.lastStable, runtime.lastCanonical = "", 0
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
	if due {
		batch, runtime.pending = runtime.pending, nil
		runtime.lastObserveNS = now
	}
	silenceNS := result.SilenceNS
	runtime.audioMu.Unlock()

	if started {
		runtime.duplex.UserSpeechStarted(now)
		if err := runtime.onUserSpeechStarted(ctx, utteranceID, result.AudioStartMS); err != nil {
			return err
		}
	}
	if len(batch) > 0 {
		if err := runtime.observeAudio(ctx, batch, silenceNS); err != nil {
			runtime.fail("asr_provider_error", err)
		}
	}
	if stopped {
		return runtime.onUserSpeechStopped(ctx, utteranceID, result.AudioEndMS, now)
	}
	return nil
}

// onUserSpeechStarted applies the barge-in policy. The decision is the
// interaction plane's; carrying it out - cancelling speech, recording what was
// heard, interrupting cognition - is here.
func (runtime *runtime) onUserSpeechStarted(ctx context.Context, utteranceID string, startMS int) error {
	state := runtime.duplex.Snapshot()
	decision := runtime.policies.BargeIn.Decide(interaction.BargeInInput{
		Context: interaction.Context{NowNS: runtime.scheduler.NowNS(), Duplex: state},
	})
	if decision.Cancel {
		runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", decision.Reason, eventloop.ErrInterrupted))
		cancelled, heard := runtime.speech.Cancel(decision.Reason)
		if err := runtime.recordCancellations(cancelled, "barge-in", eventloop.PriorityInterrupt); err != nil {
			return err
		}
		if heard {
			// Something was already audible. The repair policy decides what
			// that costs; the duplex state records that playback has stopped.
			runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
		}
	}
	return runtime.sink.Activity(ctx, binding.ActivityEvent{
		Started: true, ItemID: utteranceID, AudioStartMS: startMS,
	})
}

func (runtime *runtime) onUserSpeechStopped(ctx context.Context, utteranceID string, endMS int, now uint64) error {
	runtime.duplex.UserSpeechStopped(now)
	observations, flushErr := runtime.audio.Flush(ctx)
	durationMS := runtime.audio.DurationMS()
	runtime.audio.Reset()
	runtime.policies.Trigger.Reset()
	runtime.policies.Preparation.Reset()
	runtime.audioMu.Lock()
	runtime.pending, runtime.lastObserveNS = nil, 0
	runtime.audioMu.Unlock()

	if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
		Stopped: true, ItemID: utteranceID, AudioEndMS: endMS,
	}); err != nil {
		return err
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
func (runtime *runtime) observeAudio(ctx context.Context, frames []perception.Frame, silenceNS uint64) error {
	observations, err := runtime.audio.Observe(ctx, frames)
	if err != nil {
		return err
	}
	for _, observation := range observations {
		revision := interaction.Revision{
			ID: observation.Revision, StableText: observation.StableText,
			UnstableText: strings.TrimPrefix(observation.Text, observation.StableText),
			Final:        observation.Final, ObservedNS: runtime.scheduler.NowNS(), SilenceNS: silenceNS,
		}
		decision := interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(), Revision: revision,
		}
		// Preparation is consulted on every revision. It decides whether work
		// starts before the endpoint; it never decides what gets committed.
		runtime.policies.Preparation.Prepare(decision)
		opportunity := runtime.policies.Trigger.Next(decision)
		if err := runtime.sink.Transcript(ctx, binding.TranscriptEvent{
			ItemID: runtime.currentUtterance(), Text: observation.Text,
		}); err != nil {
			return err
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
			return err
		}
		runtime.setStableText(observation.StableText)
	}
	return nil
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
		// the older prefix is stale, so it is interrupted and any speech it
		// produced that has not been heard is cancelled.
		runtime.coordinator.Interrupt(errors.New("superseded by a newer canonical observation"))
		cancelled := runtime.speech.CancelMatching("superseded by a newer observation", func(utterance actionUtterance) bool {
			return utterance.SourceRevision < revision
		})
		if err := runtime.recordCancellations(cancelled, "asr-revision", eventloop.PriorityRoutine); err != nil {
			return err
		}
	}
	if err := runtime.sink.Observation(ctx, observation); err != nil {
		return err
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
