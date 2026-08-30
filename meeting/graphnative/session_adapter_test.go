package graphnative

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestMeetingSessionAdapterPreservesExternalMediaClock(t *testing.T) {
	audio := newTestOutput("audio", modelelements.AudioInputType())
	video := newTestOutput("video", modelelements.VideoInputType())
	adapter := &meetingSessionAdapter{
		ctx: context.Background(), sessionID: "meeting-clock-test",
		ports:     meetingAdapterPorts{audio: audio, video: video},
		frameRate: 5_000, videoCaptured: make(map[string]uint64),
	}

	audioFrame := perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 101,
		PCM16LE: []byte{0, 0}, SampleRateHz: 24_000,
	}
	if err := adapter.Audio(context.Background(), audioFrame); err != nil {
		t.Fatal(err)
	}
	audioEnvelope := outputEnvelope(t, audio)
	assertMeetingExternalClock(t, audioEnvelope, audioFrame.CapturedNS)
	audioPayload, ok := audioEnvelope.Payload.(acousticelements.InputFrame)
	if !ok || audioPayload.Frame.CapturedNS != audioEnvelope.CaptureNS {
		t.Fatalf("Meeting audio clock payload=%T %+v envelope=%+v",
			audioEnvelope.Payload, audioPayload, audioEnvelope)
	}

	videoFrame := perception.Frame{
		Kind: perception.FrameImage, Source: "screen", CapturedNS: 202,
		Image: []byte{1}, MIMEType: "image/png", Width: 1, Height: 1, Index: 1,
	}
	if err := adapter.Video(context.Background(), videoFrame); err != nil {
		t.Fatal(err)
	}
	videoEnvelope := outputEnvelope(t, video)
	assertMeetingExternalClock(t, videoEnvelope, videoFrame.CapturedNS)
	videoPayload, ok := videoEnvelope.Payload.(modelelements.VideoInputFrame)
	if !ok || videoPayload.Frame.CapturedNS != videoEnvelope.CaptureNS {
		t.Fatalf("Meeting video clock payload=%T %+v envelope=%+v",
			videoEnvelope.Payload, videoPayload, videoEnvelope)
	}
}

func assertMeetingExternalClock(t testing.TB, envelope element.Envelope, want uint64) {
	t.Helper()
	if envelope.CaptureNS != want || envelope.CaptureNS == 0 {
		t.Fatalf("Meeting media capture clock = %d, want %d", envelope.CaptureNS, want)
	}
}
