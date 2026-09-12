package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// mirror reads the remote session and does two things with every event:
// forwards it to this session's client, and commits what matters to the
// canonical trajectory so the background reasoner sees the same conversation
// the remote is having.
//
// The second half is the whole binding. Without a shared log there is nothing
// for a second model to continue, which is exactly the limitation a
// single-model server has by construction.
func (runtime *runtime) mirror() {
	for event := range runtime.remote.Events() {
		if err := runtime.mirrorEvent(event.Type, event.Raw); err != nil {
			runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
				Code: "upstream_error", Message: err.Error(),
			})
		}
	}
	if err := runtime.remote.Err(); err != nil {
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "upstream_closed", Message: err.Error(),
		})
	}
}

func (runtime *runtime) mirrorEvent(eventType string, raw []byte) error {
	switch eventType {
	case "input_audio_buffer.speech_started":
		var decoded struct {
			ItemID       string `json:"item_id"`
			AudioStartMS int    `json:"audio_start_ms"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.duplex.UserSpeechStarted(runtime.scheduler.NowNS())
		runtime.noteUserSpeech()
		// The user has taken the floor. Whether the remote should stop is this
		// binding's policy to decide and the remote's to obey.
		runtime.considerBargeIn()
		return runtime.sink.Activity(runtime.ctx, binding.ActivityEvent{
			Started: true, ItemID: decoded.ItemID, AudioStartMS: decoded.AudioStartMS,
		})
	case "input_audio_buffer.speech_stopped":
		var decoded struct {
			ItemID     string `json:"item_id"`
			AudioEndMS int    `json:"audio_end_ms"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.duplex.UserSpeechStopped(runtime.scheduler.NowNS())
		return runtime.sink.Activity(runtime.ctx, binding.ActivityEvent{
			Stopped: true, ItemID: decoded.ItemID, AudioEndMS: decoded.AudioEndMS,
		})
	case "conversation.item.input_audio_transcription.completed":
		var decoded struct {
			ItemID     string `json:"item_id"`
			Transcript string `json:"transcript"`
			EndMS      int64  `json:"end_ms"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		if strings.TrimSpace(decoded.Transcript) == "" {
			return nil
		}
		if err := runtime.sink.Transcript(runtime.ctx, binding.TranscriptEvent{
			ItemID: decoded.ItemID, Text: decoded.Transcript, Final: true,
		}); err != nil {
			return err
		}
		if err := runtime.commitUserSpeech(decoded.Transcript, runtime.occurredAt(decoded.EndMS)); err != nil {
			return err
		}
		runtime.extractStanding(decoded.Transcript)
		return nil
	case "response.output_audio.delta":
		return runtime.forwardAudio(raw)
	case "response.output_audio_transcript.delta":
		var decoded struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		return runtime.forwardText(decoded.Delta)
	case "response.output_text.delta":
		// A text session's output arrives on its own events, and until the
		// modality reached the remote there were never any to handle. Routing
		// them to the same pair as the spoken transcript is what makes a
		// text-only upstream session produce anything at all: forwarding the
		// declaration without this would trade wasted audio for silence.
		var decoded struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		return runtime.forwardText(decoded.Delta)
	case "response.output_text.done":
		var decoded struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		return runtime.commitRemoteAssistant(decoded.Text, 0)
	case "response.output_audio_transcript.done":
		var decoded struct {
			Transcript string `json:"transcript"`
			EndMS      int64  `json:"end_ms"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		return runtime.commitRemoteAssistant(decoded.Transcript, runtime.occurredAt(decoded.EndMS))
	case "conversation.item.input_audio_transcription.failed":
		// The remote could not make out what the user said. It still answers,
		// because it heard the audio natively - but the transcript is the only
		// evidence this side gets, so without one the reasoner never sees the
		// turn at all. The binding's whole contribution silently skips it, and
		// the client renders an agent replying to nothing.
		//
		// Nothing here can recover the words. Saying so is the entire fix: an
		// unreported failure and a turn the reasoner had nothing to add to
		// look identical from outside.
		var decoded struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &decoded)
		message := decoded.Error.Message
		if strings.TrimSpace(message) == "" {
			message = "the remote could not transcribe what the user said"
		}
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code:    "upstream_transcription_failed",
			Message: message + ": the background reasoner did not see this turn",
		})
		return nil
	case "conversation.item.input_audio_transcription.delta":
		// A fragment of what the user is saying. The base protocol carries the
		// committed transcript to a client; the fragment is runtime evidence,
		// which the transcript policy and the debug stream read.
		var decoded struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		_ = json.Unmarshal(raw, &decoded)
		if strings.TrimSpace(decoded.Delta) == "" {
			return nil
		}
		runtime.noteUserSpeech()
		return runtime.sink.Transcript(runtime.ctx, binding.TranscriptEvent{
			ItemID: decoded.ItemID, Text: decoded.Delta, Final: false,
		})
	case "session.created":
		var decoded struct {
			Session struct {
				ID        string `json:"id"`
				ExpiresAt int64  `json:"expires_at"`
			} `json:"session"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.remoteMu.Lock()
		runtime.remote_.SessionID = decoded.Session.ID
		runtime.remote_.ExpiresAt = decoded.Session.ExpiresAt
		// The remote's timeline starts now. Its milliseconds are the honest
		// clock for everything it reports; this is what they are measured
		// from.
		runtime.liveEpochNS = runtime.scheduler.NowNS()
		runtime.remoteMu.Unlock()
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.session.created",
			Attributes: map[string]any{"session_id": decoded.Session.ID, "expires_at": decoded.Session.ExpiresAt},
		})
		return nil
	case "openrealtime.upstream.delegation":
		// The remote asked for help. This is the escalation the rollout already
		// understands - the fast turn handing the work on - and it is the one
		// signal on this endpoint that comes from the model's own judgement
		// rather than from a silence heuristic.
		var decoded struct {
			ID       string `json:"delegation_id"`
			Target   string `json:"target"`
			OffsetMS int64  `json:"offset_ms"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.remoteMu.Lock()
		runtime.remote_.OpenDelegation = decoded.ID
		runtime.escalationPending = true
		runtime.remoteMu.Unlock()
		runtime.debug(binding.DebugEvent{
			Category: "cognition", Name: "upstream.delegation",
			CorrelationID: decoded.ID,
			Attributes:    map[string]any{"target": decoded.Target, "offset_ms": decoded.OffsetMS},
		})
		return runtime.signal(interaction.SignalEscalated)
	case "openrealtime.upstream.usage":
		var decoded struct {
			Seconds float64 `json:"seconds"`
			Ratio   float64 `json:"context_window_ratio"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.remoteMu.Lock()
		runtime.remote_.UsageSeconds = decoded.Seconds
		runtime.remote_.ContextWindowRatio = decoded.Ratio
		runtime.remoteMu.Unlock()
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.usage",
			Attributes: map[string]any{"seconds": decoded.Seconds, "context_window_ratio": decoded.Ratio},
		})
		return nil
	case "openrealtime.upstream.ack":
		var decoded struct {
			Of            string `json:"of"`
			ClientEventID string `json:"client_event_id"`
			StartMS       int64  `json:"start_ms"`
			EndMS         int64  `json:"end_ms"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.ack", CorrelationID: decoded.ClientEventID,
			Attributes: map[string]any{"of": decoded.Of, "start_ms": decoded.StartMS, "end_ms": decoded.EndMS},
		})
		return nil
	case "openrealtime.upstream.reconnected":
		var decoded struct {
			From    string `json:"from"`
			Attempt int    `json:"attempt"`
			Cause   string `json:"cause"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.reconnected", Message: decoded.Cause,
			Attributes: map[string]any{"from": decoded.From, "attempt": decoded.Attempt},
		})
		return nil
	case "openrealtime.upstream.transport":
		var decoded struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.transport", Attributes: map[string]any{"kind": decoded.Kind},
		})
		return nil
	case "openrealtime.upstream.info":
		var decoded struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.info", Message: decoded.Message,
			Attributes: map[string]any{"code": decoded.Code},
		})
		return nil
	case "response.done":
		runtime.remoteMu.Lock()
		runtime.remote_.OpenDelegation = ""
		runtime.remoteMu.Unlock()
		// The interrupted utterance has ended, so the hold is over: whatever
		// the remote says next is an answer to the person who interrupted.
		runtime.resumeAudio()
		return runtime.finishRemoteResponse()
	case "error":
		var decoded struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &decoded)
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "upstream_" + decoded.Error.Code, Message: decoded.Error.Message,
		})
		return nil
	default:
		return nil
	}
}

// commitUserSpeech records what the remote heard as a canonical observation.
// The remote is the perception owner here, so the transcript is the evidence
// rather than something this side re-derives.
func (runtime *runtime) commitUserSpeech(text string, occurredNS uint64) error {
	observation := perception.Observation{
		Text: text, Observer: "remote", Source: "microphone",
		Authority: trajectory.AuthorityUser, Final: true, OccurredNS: occurredNS,
	}
	if err := runtime.sink.Observation(runtime.ctx, observation); err != nil {
		return err
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "remote.transcript", Source: "remote", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
		Content: text, OccurredNS: occurredNS,
	})
	return err
}

// commitRemoteAssistant records what the remote said.
//
// It is committed as an observation with observer authority rather than as an
// assistant item, and the distinction matters: the engine's slow provider did
// not produce this text and must not be able to mistake it for its own prior
// reasoning. What the remote said is evidence about the conversation.
func (runtime *runtime) commitRemoteAssistant(text string, occurredNS uint64) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	observation := perception.Observation{
		Text: "The voice model said: " + text, Observer: "remote-voice", Source: "assistant",
		Authority: trajectory.AuthorityObserver, Final: true, OccurredNS: occurredNS,
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "remote.assistant", Source: "remote-voice", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
		Content: observation.Text, Observation: observation.Meta(), OccurredNS: occurredNS,
	})
	return err
}

// forwardText delivers one transcript delta from the remote, opening the turn
// if the remote produced words before sound.
func (runtime *runtime) forwardText(delta string) error {
	if strings.TrimSpace(delta) == "" {
		return nil
	}
	utterance, err := runtime.currentUtterance("")
	if err != nil {
		return err
	}
	return runtime.sink.SpeechText(runtime.ctx, utterance, delta)
}

// currentUtterance returns the turn in progress, opening one if needed.
func (runtime *runtime) currentUtterance(itemID string) (action.Utterance, error) {
	runtime.stateMu.Lock()
	if runtime.utterance != nil {
		utterance := *runtime.utterance
		runtime.stateMu.Unlock()
		return utterance, nil
	}
	if strings.TrimSpace(itemID) == "" {
		itemID = "upstream_turn"
	}
	utterance := action.Utterance{ID: itemID}
	runtime.utterance = &utterance
	runtime.stateMu.Unlock()
	return utterance, runtime.sink.SpeechBegin(runtime.ctx, utterance)
}

func (runtime *runtime) forwardAudio(raw []byte) error {
	var decoded struct {
		ItemID string `json:"item_id"`
		Delta  string `json:"delta"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	payload, err := base64.StdEncoding.DecodeString(decoded.Delta)
	if err != nil || len(payload) == 0 {
		return err
	}
	if runtime.audioSuppressed() {
		// The user took the floor and the remote was asked to stop. Until it
		// does, what it is still saying does not reach them.
		return nil
	}
	utterance, err := runtime.currentUtterance(decoded.ItemID)
	if err != nil {
		return err
	}
	frame := action.Frame{PCM16LE: payload, SampleRateHz: wireSampleRateHz}
	frame.Duration = pcmDuration(len(payload), frame.SampleRateHz)
	if err := runtime.duplex.AgentAudioHandedOff(utterance.ID, frame.Duration); err != nil {
		return err
	}
	return runtime.sink.SpeechAudio(runtime.ctx, utterance, frame)
}

func (runtime *runtime) finishRemoteResponse() error {
	runtime.stateMu.Lock()
	utterance := runtime.utterance
	runtime.utterance = nil
	restore := runtime.restoreInstruction
	runtime.restoreInstruction = false
	runtime.stateMu.Unlock()
	// A session-instruction handoff borrowed the session instruction to carry
	// one answer. Now that it has been said, give the instruction back, or the
	// remote would keep being told to repeat it.
	if restore {
		settings := runtime.Settings()
		base := remoteInstruction(settings.Instruction, runtime.isLive())
		if err := runtime.remote.Send(runtime.ctx,
			sessionUpdate(base, settings)); err != nil {
			return err
		}
	}
	if utterance == nil {
		return nil
	}
	return runtime.sink.SpeechEnd(runtime.ctx, *utterance, action.Outcome{Completed: true})
}
