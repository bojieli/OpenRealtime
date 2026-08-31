package model

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/perception"
)

// validateVideoInput is the boundary a captured frame crosses before it is
// handed to an external model as vision. Three of its refusals had no coverage,
// including the two that keep the wrong kind of media out: an audio frame
// arriving on a video port, and an image encoding the model was never told to
// expect.
func validVideoInput() VideoInputFrame {
	return VideoInputFrame{
		StreamID: "screen",
		Frame: perception.Frame{
			Kind: perception.FrameImage, Source: "screen",
			Image: []byte{0xff, 0xd8}, MIMEType: "image/jpeg", Width: 8, Height: 8,
		},
		FrameRateMilliHz: 2_000,
	}
}

func TestVideoInputRefusesWrongMediaEncodingAndCadence(t *testing.T) {
	t.Parallel()
	if err := validateVideoInput(validVideoInput()); err != nil {
		t.Fatalf("well-formed video input = %v, want accepted", err)
	}
	for _, mime := range []string{"image/jpeg", "image/png"} {
		input := validVideoInput()
		input.Frame.MIMEType = mime
		if err := validateVideoInput(input); err != nil {
			t.Fatalf("%s video input = %v, want accepted", mime, err)
		}
	}

	for _, test := range []struct {
		name string
		edit func(*VideoInputFrame)
		want string
	}{
		{
			name: "no stream ID",
			edit: func(input *VideoInputFrame) { input.StreamID = "" },
			want: "bounded canonical stream ID",
		},
		{
			name: "stream ID carries surrounding space",
			edit: func(input *VideoInputFrame) { input.StreamID = " screen" },
			want: "bounded canonical stream ID",
		},
		{
			name: "stream ID carries a newline",
			edit: func(input *VideoInputFrame) { input.StreamID = "screen\nother" },
			want: "bounded canonical stream ID",
		},
		{
			name: "frame is not valid on its own terms",
			edit: func(input *VideoInputFrame) { input.Frame.Image = nil },
			want: "requires bytes and a MIME type",
		},
		{
			// An audio frame arriving on a video port.
			name: "frame is audio, not an image",
			edit: func(input *VideoInputFrame) {
				input.Frame = perception.Frame{
					Kind: perception.FrameAudio, Source: "microphone",
					PCM16LE: make([]byte, 320), SampleRateHz: 16_000,
				}
			},
			want: "carries audio media",
		},
		{
			name: "image frame also carries audio fields",
			edit: func(input *VideoInputFrame) { input.Frame.SampleRateHz = 16_000 },
			want: "contains audio fields",
		},
		{
			// An encoding the model was never told to expect.
			name: "image encoding is not one the model was told to expect",
			edit: func(input *VideoInputFrame) { input.Frame.MIMEType = "image/webp" },
			want: "must be image/jpeg or image/png",
		},
		{
			name: "no frame rate",
			edit: func(input *VideoInputFrame) { input.FrameRateMilliHz = 0 },
			want: "bounded positive frame rate",
		},
		{
			name: "negative frame rate",
			edit: func(input *VideoInputFrame) { input.FrameRateMilliHz = -1 },
			want: "bounded positive frame rate",
		},
		{
			name: "frame rate beyond its bound",
			edit: func(input *VideoInputFrame) { input.FrameRateMilliHz = 1_000_001 },
			want: "bounded positive frame rate",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validVideoInput()
			test.edit(&input)
			err := validateVideoInput(input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("video input error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
