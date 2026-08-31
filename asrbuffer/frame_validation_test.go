package asrbuffer

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// validateFrame is what keeps an ASR input stream continuous. Five of its six
// refusals had no coverage, and continuity is the whole point: a frame that
// skips a sample offset or changes sample rate mid-stream would be transcribed
// as if the missing audio never existed, which reads downstream as the speaker
// having said something they did not.
func newValidationBuffer(t *testing.T) *Buffer {
	t.Helper()
	upstream := &fakeProvider{descriptor: v1.Descriptor{
		Name: "fake", Version: "1",
		Capabilities: v1.Capabilities{v1.CapabilityRevisions: true},
	}}
	buffer, err := New(Config{Provider: upstream, MinimumChunk: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

func TestASRInputFramesMustBeSizedAndContinuous(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		frame v1.AudioFrame
		want  string
	}{
		{
			name:  "no sample rate",
			frame: v1.AudioFrame{PCM16LE: make([]byte, 320)},
			want:  "sample rate must be in",
		},
		{
			name: "sample rate beyond its bound",
			frame: v1.AudioFrame{
				SampleRateHz: maxSampleRateHz + 1, PCM16LE: make([]byte, 320),
			},
			want: "sample rate must be in",
		},
		{
			name:  "no audio",
			frame: v1.AudioFrame{SampleRateHz: 16_000},
			want:  "non-empty even-length PCM16",
		},
		{
			name: "audio is not whole samples",
			frame: v1.AudioFrame{
				SampleRateHz: 16_000, PCM16LE: make([]byte, 321),
			},
			want: "non-empty even-length PCM16",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := newValidationBuffer(t).PushFrame(context.Background(), test.frame)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("push error = %v, want one containing %q", err, test.want)
			}
		})
	}

	t.Run("frame beyond the configured maximum", func(t *testing.T) {
		t.Parallel()
		buffer := newValidationBuffer(t)
		oversized := newFrame(0, 0, uint64(buffer.maxFrame))
		_, err := buffer.PushFrame(context.Background(), oversized)
		if err == nil || !strings.Contains(err.Error(), "maximum is") {
			t.Fatalf("oversized frame = %v, want a maximum-size refusal", err)
		}
	})

	// Continuity is only checked once a stream has started, so each of these
	// pushes one good frame first.
	for _, test := range []struct {
		name string
		next v1.AudioFrame
		want string
	}{
		{
			name: "index skips ahead",
			next: newFrame(2, 800, 800),
			want: "frame index is 2; expected 1",
		},
		{
			name: "sample offset skips ahead",
			next: newFrame(1, 1600, 800),
			want: "starts at sample 1600; expected 800",
		},
		{
			name: "sample rate changes mid-stream",
			next: v1.AudioFrame{
				Index: 1, SampleOffset: 800, SampleRateHz: 24_000, PCM16LE: make([]byte, 1600),
			},
			want: "sample rate changed from 16000 to 24000",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			buffer := newValidationBuffer(t)
			if _, err := buffer.PushFrame(context.Background(), newFrame(0, 0, 800)); err != nil {
				t.Fatalf("first frame: %v", err)
			}
			_, err := buffer.PushFrame(context.Background(), test.next)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("continuity error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
