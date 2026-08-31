package acoustic

import (
	"strings"
	"testing"

	coreperception "github.com/bojieli/OpenRealtime/perception"
)

// validateFrame is the gate every captured audio frame passes before acoustic
// admission looks at it. Its bounds are the ones that keep one microphone's
// stream from becoming another's, and keep an oversized or unexpected-rate
// frame out of the energy gate, which is sized for the configured rate.
func admissionFrameRunner(source string) *admissionRunner {
	return &admissionRunner{config: resolvedAdmissionConfig{
		source: source, maxFrameBytes: 4096, maxSampleRateHz: 48_000,
	}}
}

func validAudioFrame() coreperception.Frame {
	return coreperception.Frame{
		Kind: coreperception.FrameAudio, Source: "microphone",
		PCM16LE: make([]byte, 320), SampleRateHz: 16_000,
	}
}

func TestAcousticAdmissionBoundsEveryFrameItAccepts(t *testing.T) {
	t.Parallel()
	if err := admissionFrameRunner("").validateFrame(validAudioFrame()); err != nil {
		t.Fatalf("well-formed frame = %v, want accepted", err)
	}
	if err := admissionFrameRunner("microphone").validateFrame(validAudioFrame()); err != nil {
		t.Fatalf("frame from the configured source = %v, want accepted", err)
	}

	for _, test := range []struct {
		name   string
		source string
		frame  coreperception.Frame
		want   string
	}{
		{
			name: "frame is not valid on its own terms",
			frame: func() coreperception.Frame {
				frame := validAudioFrame()
				frame.PCM16LE = make([]byte, 321)
				return frame
			}(),
			want: "even-length PCM16",
		},
		{
			// A still image reaching acoustic admission would be measured for
			// energy as though it were sound.
			name: "frame is an image, not audio",
			frame: coreperception.Frame{
				Kind: coreperception.FrameImage, Source: "screen",
				Image: []byte{0xff, 0xd8}, MIMEType: "image/jpeg", Width: 4, Height: 4,
			},
			want: "requires an audio frame",
		},
		{
			name: "audio source is not a canonical identifier",
			frame: func() coreperception.Frame {
				frame := validAudioFrame()
				frame.Source = " microphone"
				return frame
			}(),
			want: "audio source",
		},
		{
			name: "frame is larger than the configured bound",
			frame: func() coreperception.Frame {
				frame := validAudioFrame()
				frame.PCM16LE = make([]byte, 4098)
				return frame
			}(),
			want: "maximum is 4096",
		},
		{
			name: "sample rate is above the configured maximum",
			frame: func() coreperception.Frame {
				frame := validAudioFrame()
				frame.SampleRateHz = 48_001
				return frame
			}(),
			want: "exceeds maximum 48000",
		},
		{
			// The energy gate is built for one declared source. Audio from
			// another microphone is not this stream.
			name:   "frame comes from a source this element was not configured for",
			source: "microphone",
			frame: func() coreperception.Frame {
				frame := validAudioFrame()
				frame.Source = "headset"
				return frame
			}(),
			want: "does not match configured source",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := admissionFrameRunner(test.source).validateFrame(test.frame)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("frame error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
