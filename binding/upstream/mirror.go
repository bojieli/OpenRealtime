package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/eventloop"
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
		return runtime.commitUserSpeech(decoded.Transcript)
	case "response.output_audio.delta":
		return runtime.forwardAudio(raw)
	case "response.output_audio_transcript.done":
		var decoded struct {
			Transcript string `json:"transcript"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		return runtime.commitRemoteAssistant(decoded.Transcript)
	case "response.done":
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
func (runtime *runtime) commitUserSpeech(text string) error {
	observation := perception.Observation{
		Text: text, Observer: "remote", Source: "microphone",
		Authority: trajectory.AuthorityUser, Final: true,
	}
	if err := runtime.sink.Observation(runtime.ctx, observation); err != nil {
		return err
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "remote.transcript", Source: "remote", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
		Content: text,
	})
	return err
}

// commitRemoteAssistant records what the remote said.
//
// It is committed as an observation with observer authority rather than as an
// assistant item, and the distinction matters: the engine's slow provider did
// not produce this text and must not be able to mistake it for its own prior
// reasoning. What the remote said is evidence about the conversation.
func (runtime *runtime) commitRemoteAssistant(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	observation := perception.Observation{
		Text: "The voice model said: " + text, Observer: "remote-voice", Source: "assistant",
		Authority: trajectory.AuthorityObserver, Final: true,
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "remote.assistant", Source: "remote-voice", Channel: "voice",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
		Content: observation.Text, Observation: observation.Meta(),
	})
	return err
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
	runtime.stateMu.Lock()
	if runtime.utterance == nil {
		utterance := action.Utterance{ID: decoded.ItemID}
		runtime.utterance = &utterance
		runtime.stateMu.Unlock()
		if err := runtime.sink.SpeechBegin(runtime.ctx, utterance); err != nil {
			return err
		}
	} else {
		runtime.stateMu.Unlock()
	}
	runtime.stateMu.Lock()
	utterance := *runtime.utterance
	runtime.stateMu.Unlock()
	frame := action.Frame{PCM16LE: payload, SampleRateHz: 24_000}
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
	runtime.stateMu.Unlock()
	if utterance == nil {
		return nil
	}
	return runtime.sink.SpeechEnd(runtime.ctx, *utterance, action.Outcome{Completed: true})
}
