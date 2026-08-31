package speech

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// validateAudioFrame is the boundary every synthesized frame crosses before it
// reaches a listener. Six of its seven refusals had no coverage. They are all
// one property: a frame's own identity must agree with the stream it claims to
// belong to. A chunk whose candidate ID names another utterance is audio from
// one turn played into another, and nothing downstream re-checks it.
const frameChunkBound = 4096

func beginFrame() AudioFrame {
	return AudioFrame{
		Kind: AudioBegin, UtteranceID: "utt-1",
		Utterance: action.Utterance{ID: "utt-1", Text: "hello there"},
	}
}

func chunkFrame() AudioFrame {
	return AudioFrame{
		Kind: AudioChunk, UtteranceID: "utt-1",
		Chunk: v1.SpeechChunk{
			ChunkID: "chunk-1", CandidateID: "utt-1",
			SampleRateHz: 24_000, PCM16LE: make([]byte, 320),
		},
	}
}

func endFrame() AudioFrame {
	return AudioFrame{
		Kind: AudioEnd, UtteranceID: "utt-1",
		Terminal: SynthesisOutcome{UtteranceID: "utt-1", Kind: OutcomeSucceeded},
	}
}

func TestAudioFrameIdentityMustAgreeWithItsStream(t *testing.T) {
	t.Parallel()
	for _, frame := range []AudioFrame{beginFrame(), chunkFrame(), endFrame()} {
		if err := validateAudioFrame(frame, frameChunkBound); err != nil {
			t.Fatalf("well-formed %s frame = %v, want accepted", frame.Kind, err)
		}
	}
	for _, kind := range []OutcomeKind{OutcomeCancelled, OutcomeFailed} {
		frame := endFrame()
		frame.Terminal.Kind = kind
		if err := validateAudioFrame(frame, frameChunkBound); err != nil {
			t.Fatalf("terminal outcome %q = %v, want accepted", kind, err)
		}
	}

	for _, test := range []struct {
		name  string
		frame AudioFrame
		want  string
	}{
		{
			name: "frame names no utterance",
			frame: func() AudioFrame {
				frame := beginFrame()
				frame.UtteranceID = "   "
				return frame
			}(),
			want: "requires an utterance ID",
		},
		{
			name: "begin metadata names another utterance",
			frame: func() AudioFrame {
				frame := beginFrame()
				frame.Utterance.ID = "utt-2"
				return frame
			}(),
			want: "begin utterance metadata does not match its stream ID",
		},
		{
			name: "begin carries no utterance text",
			frame: func() AudioFrame {
				frame := beginFrame()
				frame.Utterance.Text = "  "
				return frame
			}(),
			want: "begin requires utterance text",
		},
		{
			// Audio from one turn played into another.
			name: "chunk candidate names another utterance",
			frame: func() AudioFrame {
				frame := chunkFrame()
				frame.Chunk.CandidateID = "utt-2"
				return frame
			}(),
			want: "chunk candidate does not match its stream ID",
		},
		{
			name: "chunk is not internally valid",
			frame: func() AudioFrame {
				frame := chunkFrame()
				frame.Chunk.PCM16LE = make([]byte, 321)
				return frame
			}(),
			want: "audio chunk:",
		},
		{
			name: "chunk exceeds the negotiated byte bound",
			frame: func() AudioFrame {
				frame := chunkFrame()
				frame.Chunk.PCM16LE = make([]byte, frameChunkBound+2)
				return frame
			}(),
			want: "exceeds 4096 bytes",
		},
		{
			name: "terminal names another utterance",
			frame: func() AudioFrame {
				frame := endFrame()
				frame.Terminal.UtteranceID = "utt-2"
				return frame
			}(),
			want: "terminal result does not match its stream ID",
		},
		{
			name: "terminal outcome is not one of the declared kinds",
			frame: func() AudioFrame {
				frame := endFrame()
				frame.Terminal.Kind = OutcomeKind("interrupted")
				return frame
			}(),
			want: "invalid terminal outcome",
		},
		{
			name:  "frame kind is unknown",
			frame: AudioFrame{Kind: AudioFrameKind("resume"), UtteranceID: "utt-1"},
			want:  "unknown audio frame kind",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateAudioFrame(test.frame, frameChunkBound)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("frame error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
