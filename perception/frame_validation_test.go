package perception_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/perception"
)

// Frame.Validate rejects a frame that does not carry what its kind promises.
// All four of its refusals were unexercised, so an image frame with no bytes
// or a zero-sized one could have reached an observer that assumes both.
func TestPerceptionFrameMustCarryWhatItsKindPromises(t *testing.T) {
	t.Parallel()
	audio := perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone",
		PCM16LE: make([]byte, 320), SampleRateHz: 16_000,
	}
	image := perception.Frame{
		Kind: perception.FrameImage, Source: "screen",
		Image: []byte{0xff, 0xd8}, MIMEType: "image/jpeg", Width: 4, Height: 4,
	}
	for _, frame := range []perception.Frame{audio, image} {
		if err := frame.Validate(); err != nil {
			t.Fatalf("well-formed %s frame = %v, want accepted", frame.Kind, err)
		}
	}

	for _, test := range []struct {
		name  string
		frame perception.Frame
		want  string
	}{
		{
			// Screen and camera are both live and semantically different, so a
			// frame that does not name its stream cannot be placed.
			name: "frame names no source",
			frame: func() perception.Frame {
				frame := audio
				frame.Source = "   "
				return frame
			}(),
			want: "requires a source",
		},
		{
			name: "audio frame carries no audio",
			frame: func() perception.Frame {
				frame := audio
				frame.PCM16LE = nil
				return frame
			}(),
			want: "non-empty even-length PCM16 and a sample rate",
		},
		{
			name: "audio frame is not whole samples",
			frame: func() perception.Frame {
				frame := audio
				frame.PCM16LE = make([]byte, 321)
				return frame
			}(),
			want: "non-empty even-length PCM16 and a sample rate",
		},
		{
			name: "audio frame has no sample rate",
			frame: func() perception.Frame {
				frame := audio
				frame.SampleRateHz = 0
				return frame
			}(),
			want: "non-empty even-length PCM16 and a sample rate",
		},
		{
			name: "image frame carries no bytes",
			frame: func() perception.Frame {
				frame := image
				frame.Image = nil
				return frame
			}(),
			want: "requires bytes and a MIME type",
		},
		{
			name: "image frame names no MIME type",
			frame: func() perception.Frame {
				frame := image
				frame.MIMEType = " "
				return frame
			}(),
			want: "requires bytes and a MIME type",
		},
		{
			name: "image frame has no width",
			frame: func() perception.Frame {
				frame := image
				frame.Width = 0
				return frame
			}(),
			want: "requires positive dimensions",
		},
		{
			name: "image frame has a negative height",
			frame: func() perception.Frame {
				frame := image
				frame.Height = -1
				return frame
			}(),
			want: "requires positive dimensions",
		},
		{
			name:  "frame kind is unknown",
			frame: perception.Frame{Kind: perception.FrameKind("video"), Source: "camera"},
			want:  "unknown frame kind",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.frame.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("frame error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
