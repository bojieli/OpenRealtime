package sidecar_test

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	"github.com/bojieli/OpenRealtime/sidecar"
)

// Protocol v4 extends the stream; it must not reinterpret any conformant
// frame from the three frozen legacy protocols.
func TestLegacyV1ThroughV3FramesStillRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		messages []sidecar.Message
	}{
		{
			name: "v1 audio",
			messages: []sidecar.Message{
				{Type: sidecar.TypeHello, Version: sidecar.Version, SampleRate: 24_000},
				{Type: sidecar.TypeReady, Version: sidecar.Version, Model: "legacy-audio", OutputRate: 24_000},
				{Type: sidecar.TypeAudio, Payload: []byte{1, 2, 3, 4}},
			},
		},
		{
			name: "v2 interaction",
			messages: []sidecar.Message{
				{
					Type: sidecar.TypeHello, Version: sidecar.VersionInteraction, SampleRate: 24_000,
					InteractionOwner: "engine", FloorOwner: "model",
				},
				{Type: sidecar.TypeReady, Version: sidecar.VersionInteraction, Model: "legacy-interaction", OutputRate: 24_000},
				{
					Type: sidecar.TypeInteractionAct, Act: "answer", Floor: "take",
					Policy: "legacy-policy", EvidenceRef: "revision:2", DeadlineMS: 1,
					Confidence: 0.75,
				},
			},
		},
		{
			name: "v3 vision",
			messages: []sidecar.Message{
				{
					Type: sidecar.TypeHello, Version: sidecar.VersionMultimodal, SampleRate: 24_000,
					InteractionOwner: "model", FloorOwner: "model",
				},
				{
					Type: sidecar.TypeReady, Version: sidecar.VersionMultimodal, Model: "legacy-multimodal",
					OutputRate: 24_000, Capabilities: []string{string(sidecar.CapabilityVisualInput)},
				},
				{
					Type: sidecar.TypeImage, Payload: []byte{0xff, 0xd8, 0xff}, Source: "camera.front",
					MIMEType: "image/jpeg", Width: 640, Height: 480, TimestampMS: 42,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stream bytes.Buffer
			writer := sidecar.NewWriter(&stream)
			for index, message := range test.messages {
				if err := writer.Write(message); err != nil {
					t.Fatalf("write frame %d (%s): %v", index, message.Type, err)
				}
			}

			reader := sidecar.NewReader(&stream)
			for index, want := range test.messages {
				got, err := reader.Read()
				if err != nil {
					t.Fatalf("read frame %d: %v", index, err)
				}
				want.PayloadBytes = len(want.Payload)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("frame %d changed across v4-capable framing:\n got: %+v\nwant: %+v", index, got, want)
				}
			}
			if _, err := reader.Read(); err != io.EOF {
				t.Fatalf("stream terminator = %v, want EOF", err)
			}
		})
	}
}
