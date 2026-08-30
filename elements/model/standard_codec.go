package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/acoustic"
	"github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

var standardConcretePayloadTypes = func() map[string]struct{} {
	result := make(map[string]struct{})
	for _, valueType := range []element.Type{
		AudioInputType(), ContextType(), ToolResultType(), GenerateType(), CommitType(),
		VideoInputType(), InteractionType(), TickType(), CancelType(), TranscriptType(), PreparedTextType(),
		PreparedAudioType(), ResultType(), ToolProposalType(), InteractionActType(), OutcomeType(),
		TruncateType(), ActivityType(),
	} {
		result[valueType.String()] = struct{}{}
	}
	return result
}()

func standardConcretePayload(valueType element.Type) bool {
	_, found := standardConcretePayloadTypes[valueType.String()]
	return found
}

// NewStandardJSONCodec returns the production codec for graph-native model
// ports whose Go representation is established in this repository. Unknown
// extension types remain explicitly opaque and may be registered by the
// deployment before installation.
func NewStandardJSONCodec() *JSONCodec {
	codec := NewJSONCodec()
	registrations := []struct {
		valueType element.Type
		adapter   PayloadAdapter
	}{
		{AudioInputType(), acousticInputAdapter()},
		{VideoInputType(), videoInputAdapter()},
		{ContextType(), jsonAdapter(func() any { return &trajectory.Snapshot{} })},
		{ToolResultType(), jsonAdapter(func() any { return &trajectory.ToolResult{} })},
		{GenerateType(), jsonAdapter(func() any { return &cognition.Generate{} })},
		{CommitType(), jsonAdapter(func() any { return &acoustic.AudioCommit{} })},
		{InteractionType(), jsonAdapter(func() any { return new(interaction.Act) })},
		{TickType(), jsonAdapter(func() any { return &acoustic.TimingTick{} })},
		{CancelType(), jsonAdapter(func() any { return &cognition.Cancel{} })},
		{TruncateType(), jsonAdapter(func() any { return &binding.Truncation{} })},
		{TranscriptType(), jsonAdapter(func() any { return &perception.Observation{} })},
		{PreparedTextType(), jsonAdapter(func() any { return &cognition.PreparedTextDelta{} })},
		{PreparedAudioType(), preparedAudioAdapter()},
		{ResultType(), jsonAdapter(func() any { return &cognition.Result{} })},
		{ToolProposalType(), jsonAdapter(func() any { return &cognition.ToolProposal{} })},
		{InteractionActType(), jsonAdapter(func() any { return new(interaction.Act) })},
		{ActivityType(), jsonAdapter(func() any { return &binding.ActivityEvent{} })},
		{OutcomeType(), jsonAdapter(func() any { return &cognition.Outcome{} })},
	}
	for _, registration := range registrations {
		if err := codec.RegisterAdapter(registration.valueType, registration.adapter); err != nil {
			// These are compile-time-owned type constants and a fresh registry. A
			// failure is therefore a package invariant, not deployment input.
			panic(fmt.Sprintf("register standard external-model payload %s: %v",
				registration.valueType.String(), err))
		}
	}
	return codec
}

// VideoInputFrame is one directly observed image addressed to a live model
// stream. FrameRateMilliHz is explicit protocol evidence; it must not be
// guessed from arrival cadence by the transport adapter.
type VideoInputFrame struct {
	StreamID         string           `json:"stream_id"`
	Frame            perception.Frame `json:"frame"`
	FrameRateMilliHz int              `json:"frame_rate_millihz"`
}

func videoInputAdapter() PayloadAdapter {
	return PayloadAdapter{
		Factory: func() any { return &VideoInputFrame{} },
		ExtractBinary: func(payload any) ([]byte, *sidecar.MediaFrameMetadata, error) {
			input, err := videoInputFrame(payload)
			if err != nil {
				return nil, nil, err
			}
			if err := validateVideoInput(input); err != nil {
				return nil, nil, err
			}
			metadata := &sidecar.MediaFrameMetadata{
				Kind: sidecar.MediaVideo, Encoding: input.Frame.MIMEType,
				Width: input.Frame.Width, Height: input.Frame.Height,
				FrameRateMilliHz: input.FrameRateMilliHz,
			}
			if err := metadata.Validate(); err != nil {
				return nil, nil, err
			}
			return input.Frame.Image, metadata, nil
		},
		AttachBinary: func(value any, binary []byte, metadata *sidecar.MediaFrameMetadata) error {
			input, ok := value.(*VideoInputFrame)
			if !ok || input == nil {
				return fmt.Errorf("video input decoder received %T", value)
			}
			if len(input.Frame.Image) != 0 {
				return errors.New("video input JSON duplicated the binary image body")
			}
			if len(binary) == 0 || metadata == nil {
				return errors.New("video input requires an authenticated binary image frame")
			}
			if err := metadata.Validate(); err != nil {
				return err
			}
			if metadata.Kind != sidecar.MediaVideo || metadata.Encoding != input.Frame.MIMEType ||
				metadata.Width != input.Frame.Width || metadata.Height != input.Frame.Height ||
				metadata.FrameRateMilliHz != input.FrameRateMilliHz {
				return fmt.Errorf("video JSON/binary metadata mismatch: got %+v", *metadata)
			}
			input.Frame.Image = append([]byte(nil), binary...)
			return validateVideoInput(*input)
		},
	}
}

func videoInputFrame(payload any) (VideoInputFrame, error) {
	switch typed := payload.(type) {
	case VideoInputFrame:
		return typed, nil
	case *VideoInputFrame:
		if typed != nil {
			return *typed, nil
		}
	}
	return VideoInputFrame{}, fmt.Errorf("video input payload has type %T, want model.VideoInputFrame", payload)
}

func validateVideoInput(input VideoInputFrame) error {
	if err := validateMediaStreamID(input.StreamID); err != nil {
		return fmt.Errorf("video input: %w", err)
	}
	if err := input.Frame.Validate(); err != nil {
		return err
	}
	if input.Frame.Kind != perception.FrameImage {
		return fmt.Errorf("video input carries %s media", input.Frame.Kind)
	}
	if len(input.Frame.PCM16LE) != 0 || input.Frame.SampleRateHz != 0 || input.Frame.SampleOffset != 0 {
		return errors.New("video input image contains audio fields")
	}
	if input.Frame.MIMEType != "image/jpeg" && input.Frame.MIMEType != "image/png" {
		return fmt.Errorf("video input MIME type must be image/jpeg or image/png, got %q", input.Frame.MIMEType)
	}
	if input.FrameRateMilliHz <= 0 || input.FrameRateMilliHz > 1_000_000 {
		return errors.New("video input requires a bounded positive frame rate")
	}
	return nil
}

func validateMediaStreamID(streamID string) error {
	if streamID == "" || streamID != strings.TrimSpace(streamID) ||
		len(streamID) > sidecar.MaxElementIdentifierBytes || strings.ContainsAny(streamID, "\x00\r\n") {
		return errors.New("media input requires a bounded canonical stream ID")
	}
	return nil
}

func jsonAdapter(factory PayloadFactory) PayloadAdapter {
	return PayloadAdapter{Factory: factory}
}

func acousticInputAdapter() PayloadAdapter {
	return PayloadAdapter{
		Factory: func() any { return &acoustic.InputFrame{} },
		ExtractBinary: func(payload any) ([]byte, *sidecar.MediaFrameMetadata, error) {
			frame, err := acousticInputFrame(payload)
			if err != nil {
				return nil, nil, err
			}
			if err := validateMediaStreamID(frame.StreamID); err != nil {
				return nil, nil, fmt.Errorf("audio input: %w", err)
			}
			if err := frame.Frame.Validate(); err != nil {
				return nil, nil, err
			}
			if frame.Frame.Kind != perception.FrameAudio {
				return nil, nil, fmt.Errorf("audio input carries %s media", frame.Frame.Kind)
			}
			if len(frame.Frame.Image) != 0 || frame.Frame.MIMEType != "" ||
				frame.Frame.Width != 0 || frame.Frame.Height != 0 {
				return nil, nil, errors.New("audio input frame contains image fields")
			}
			metadata, err := pcm16Metadata(frame.Frame.PCM16LE, frame.Frame.SampleRateHz)
			if err != nil {
				return nil, nil, err
			}
			return frame.Frame.PCM16LE, metadata, nil
		},
		AttachBinary: func(value any, binary []byte, metadata *sidecar.MediaFrameMetadata) error {
			frame, ok := value.(*acoustic.InputFrame)
			if !ok || frame == nil {
				return fmt.Errorf("audio input decoder received %T", value)
			}
			if frame.Frame.Kind != perception.FrameAudio {
				return fmt.Errorf("audio input JSON declares %s media", frame.Frame.Kind)
			}
			if len(frame.Frame.PCM16LE) != 0 {
				return errors.New("audio input JSON duplicated the binary PCM body")
			}
			if err := validatePCM16Metadata(binary, frame.Frame.SampleRateHz, metadata); err != nil {
				return err
			}
			frame.Frame.PCM16LE = append([]byte(nil), binary...)
			if err := validateMediaStreamID(frame.StreamID); err != nil {
				return fmt.Errorf("audio input: %w", err)
			}
			if err := frame.Frame.Validate(); err != nil {
				return err
			}
			if frame.Frame.MIMEType != "" || frame.Frame.Width != 0 || frame.Frame.Height != 0 {
				return errors.New("audio input JSON contains image fields")
			}
			return nil
		},
	}
}

func acousticInputFrame(payload any) (acoustic.InputFrame, error) {
	switch typed := payload.(type) {
	case acoustic.InputFrame:
		return typed, nil
	case *acoustic.InputFrame:
		if typed != nil {
			return *typed, nil
		}
	}
	return acoustic.InputFrame{}, fmt.Errorf("audio input payload has type %T, want acoustic.InputFrame", payload)
}

func preparedAudioAdapter() PayloadAdapter {
	return PayloadAdapter{
		Factory: func() any { return &speech.AudioFrame{} },
		MarshalJSON: func(payload any) (json.RawMessage, error) {
			frame, err := preparedAudioFrame(payload)
			if err != nil {
				return nil, err
			}
			// PCM is authenticated in the binary lane. A null JSON field keeps the
			// established SpeechChunk schema while preventing a second body.
			frame.Chunk.PCM16LE = nil
			return json.Marshal(frame)
		},
		ExtractBinary: func(payload any) ([]byte, *sidecar.MediaFrameMetadata, error) {
			frame, err := preparedAudioFrame(payload)
			if err != nil {
				return nil, nil, err
			}
			switch frame.Kind {
			case speech.AudioBegin, speech.AudioEnd:
				if len(frame.Chunk.PCM16LE) != 0 {
					return nil, nil, errors.New("bodyless prepared-audio boundary contains PCM")
				}
				return nil, nil, nil
			case speech.AudioChunk:
				if err := frame.Chunk.Validate(); err != nil {
					return nil, nil, err
				}
				metadata, err := pcm16Metadata(frame.Chunk.PCM16LE, frame.Chunk.SampleRateHz)
				if err != nil {
					return nil, nil, err
				}
				return frame.Chunk.PCM16LE, metadata, nil
			default:
				return nil, nil, fmt.Errorf("unknown prepared-audio boundary %q", frame.Kind)
			}
		},
		AttachBinary: func(value any, binary []byte, metadata *sidecar.MediaFrameMetadata) error {
			frame, ok := value.(*speech.AudioFrame)
			if !ok || frame == nil {
				return fmt.Errorf("prepared-audio decoder received %T", value)
			}
			if len(frame.Chunk.PCM16LE) != 0 {
				return errors.New("prepared-audio JSON duplicated the binary PCM body")
			}
			switch frame.Kind {
			case speech.AudioBegin, speech.AudioEnd:
				if len(binary) != 0 || metadata != nil {
					return errors.New("bodyless prepared-audio boundary carries media")
				}
				return nil
			case speech.AudioChunk:
				if err := validatePCM16Metadata(binary, frame.Chunk.SampleRateHz, metadata); err != nil {
					return err
				}
				frame.Chunk.PCM16LE = append([]byte(nil), binary...)
				return frame.Chunk.Validate()
			default:
				return fmt.Errorf("unknown prepared-audio boundary %q", frame.Kind)
			}
		},
	}
}

func preparedAudioFrame(payload any) (speech.AudioFrame, error) {
	switch typed := payload.(type) {
	case speech.AudioFrame:
		return typed, nil
	case *speech.AudioFrame:
		if typed != nil {
			return *typed, nil
		}
	}
	return speech.AudioFrame{}, fmt.Errorf("prepared-audio payload has type %T, want speech.AudioFrame", payload)
}

func pcm16Metadata(pcm []byte, sampleRate uint32) (*sidecar.MediaFrameMetadata, error) {
	if len(pcm) == 0 || len(pcm)%2 != 0 || sampleRate == 0 {
		return nil, errors.New("PCM16 media requires non-empty whole samples and a sample rate")
	}
	samples := uint64(len(pcm) / 2)
	durationMS := int((samples*1000 + uint64(sampleRate) - 1) / uint64(sampleRate))
	metadata := &sidecar.MediaFrameMetadata{
		Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
		SampleRateHz: int(sampleRate), Channels: 1, FrameDurationMS: durationMS,
	}
	if err := metadata.Validate(); err != nil {
		return nil, err
	}
	return metadata, nil
}

func validatePCM16Metadata(
	pcm []byte, sampleRate uint32, metadata *sidecar.MediaFrameMetadata,
) error {
	want, err := pcm16Metadata(pcm, sampleRate)
	if err != nil {
		return err
	}
	if metadata == nil {
		return errors.New("PCM16 media requires authenticated frame metadata")
	}
	if *metadata != *want {
		return fmt.Errorf("PCM16 JSON/binary metadata mismatch: got %+v, want %+v", *metadata, *want)
	}
	return nil
}
