package livekit

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image/jpeg"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// The fixture is a lossy WebP, which is a VP8 key frame in a RIFF container:
// the one VP8 bitstream this project can produce without a native encoder.
func loadKeyframe(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/keyframe-64x48.webp")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 20 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" ||
		string(data[12:16]) != "VP8 " {
		t.Fatalf("fixture is not a lossy WebP: %q", data[:min(16, len(data))])
	}
	size := int(binary.LittleEndian.Uint32(data[16:20]))
	if 20+size > len(data) {
		t.Fatalf("VP8 chunk of %d bytes runs past the %d-byte file", size, len(data))
	}
	frame := data[20 : 20+size]
	if frame[0]&1 != 0 {
		t.Fatal("fixture is not a key frame")
	}
	return frame
}

type recordedEvents struct {
	events []map[string]any
	fail   error
}

func (recorder *recordedEvents) emit(event any) error {
	if recorder.fail != nil {
		return recorder.fail
	}
	decoded, ok := event.(map[string]any)
	if !ok {
		return errors.New("bridge emitted something other than a protocol event object")
	}
	recorder.events = append(recorder.events, decoded)
	return nil
}

// The events must survive the wire this agent actually writes them to.
func encodeAndDecode(t *testing.T, event map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	return round
}

func decodeFrameEvent(t *testing.T, event map[string]any) (int, int, int) {
	t.Helper()
	round := encodeAndDecode(t, event)
	if round["type"] != "openrealtime.input_video_frame.append" ||
		round["source"] == "" || round["timestamp_ms"] == nil {
		t.Fatalf("frame event is incomplete: %+v", round)
	}
	payload, err := base64.StdEncoding.DecodeString(round["frame"].(string))
	if err != nil {
		t.Fatal(err)
	}
	picture, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("frame is not a JPEG the engine can decode: %v", err)
	}
	return picture.Bounds().Dx(), picture.Bounds().Dy(), len(payload)
}

func TestKeyframeBridgeDeclaresTheSourceThenSendsJPEGFrames(t *testing.T) {
	frame := loadKeyframe(t)
	recorder := &recordedEvents{}
	bridge := newKeyframeBridge("livekit:presenter", defaultVideoLimits(), recorder.emit)
	clock := time.Unix(1_700_000_000, 0)
	bridge.now = func() time.Time { return clock }

	sent, err := bridge.HandleFrame(frame)
	if err != nil || !sent {
		t.Fatalf("first key frame: sent=%v err=%v", sent, err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("expected a source declaration and a frame, got %+v", recorder.events)
	}
	source := encodeAndDecode(t, recorder.events[0])
	if source["type"] != "openrealtime.input_video_source.update" ||
		source["source"] != "livekit:presenter" || source["state"] != "active" ||
		source["width"] != float64(64) || source["height"] != float64(48) {
		t.Fatalf("source declaration = %+v", source)
	}
	if width, height, _ := decodeFrameEvent(t, recorder.events[1]); width != 64 || height != 48 {
		t.Fatalf("frame is %dx%d, want 64x48", width, height)
	}

	// Inside the negotiated cadence the next key frame is dropped, silently.
	clock = clock.Add(100 * time.Millisecond)
	if sent, err := bridge.HandleFrame(frame); err != nil || sent {
		t.Fatalf("a frame inside the cap was sent: sent=%v err=%v", sent, err)
	}
	// Past it, only a frame follows: the source was already declared.
	clock = clock.Add(400 * time.Millisecond)
	if sent, err := bridge.HandleFrame(frame); err != nil || !sent {
		t.Fatalf("a frame past the cap was not sent: sent=%v err=%v", sent, err)
	}
	if len(recorder.events) != 3 {
		t.Fatalf("expected exactly one more frame event, got %d events", len(recorder.events))
	}

	bridge.Close()
	closed := encodeAndDecode(t, recorder.events[len(recorder.events)-1])
	if closed["state"] != "closed" || closed["source"] != "livekit:presenter" {
		t.Fatalf("closing the bridge must close the declared source, got %+v", closed)
	}
}

func TestKeyframeBridgeSkipsInterFramesAndGarbage(t *testing.T) {
	recorder := &recordedEvents{}
	bridge := newKeyframeBridge("livekit:presenter", defaultVideoLimits(), recorder.emit)
	interFrame := append([]byte(nil), loadKeyframe(t)...)
	interFrame[0] |= 1
	if sent, err := bridge.HandleFrame(interFrame); sent || !errors.Is(err, errNotKeyframe) {
		t.Fatalf("an inter frame must be skipped as such: sent=%v err=%v", sent, err)
	}
	if sent, err := bridge.HandleFrame([]byte{0, 1, 2}); sent || err == nil {
		t.Fatalf("garbage must be an error, not a frame: sent=%v err=%v", sent, err)
	}
	if sent, err := bridge.HandleFrame(nil); sent || err == nil {
		t.Fatalf("an empty frame must be an error: sent=%v err=%v", sent, err)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("nothing should have been emitted, got %+v", recorder.events)
	}
	bridge.Close()
	if len(recorder.events) != 0 {
		t.Fatal("closing a bridge that never declared a source must not emit a closure")
	}
}

func TestKeyframeBridgeObeysTheNegotiatedDimensionAndByteLimit(t *testing.T) {
	frame := loadKeyframe(t)
	limits := defaultVideoLimits()
	limits.MaxDimension = 32
	scaled := &recordedEvents{}
	if _, err := newKeyframeBridge("s", limits, scaled.emit).HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	source := encodeAndDecode(t, scaled.events[0])
	width, height, _ := decodeFrameEvent(t, scaled.events[1])
	if width != 32 || height != 24 || source["width"] != float64(32) || source["height"] != float64(24) {
		t.Fatalf("frame %dx%d declared %v x %v, want 32x24 both",
			width, height, source["width"], source["height"])
	}

	full := &recordedEvents{}
	if _, err := newKeyframeBridge("s", defaultVideoLimits(), full.emit).HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	_, _, fullBytes := decodeFrameEvent(t, full.events[1])
	squeezedLimits := defaultVideoLimits()
	squeezedLimits.MaxFrameBytes = fullBytes - 1
	squeezed := &recordedEvents{}
	if _, err := newKeyframeBridge("s", squeezedLimits, squeezed.emit).HandleFrame(frame); err != nil {
		t.Fatalf("a limit one byte under the best quality must be met by a lower quality: %v", err)
	}
	if _, _, size := decodeFrameEvent(t, squeezed.events[1]); size >= fullBytes {
		t.Fatalf("frame did not shrink: %d >= %d", size, fullBytes)
	}

	squeezedLimits.MaxFrameBytes = 16
	refused := &recordedEvents{}
	sent, err := newKeyframeBridge("s", squeezedLimits, refused.emit).HandleFrame(frame)
	if sent || err == nil || len(refused.events) != 0 {
		t.Fatalf("a frame that cannot fit must be dropped with an error, not sent: sent=%v err=%v events=%d",
			sent, err, len(refused.events))
	}
}

func TestKeyframeBridgeReportsAnEndpointThatRefusedTheEvent(t *testing.T) {
	recorder := &recordedEvents{fail: errors.New("endpoint closed")}
	bridge := newKeyframeBridge("s", defaultVideoLimits(), recorder.emit)
	if sent, err := bridge.HandleFrame(loadKeyframe(t)); sent || err == nil {
		t.Fatalf("an emit failure must surface: sent=%v err=%v", sent, err)
	}
	if bridge.declared {
		t.Fatal("a declaration the endpoint refused must not be remembered as made")
	}
}

// The same bytes arrive from a room publisher as RTP packets that the VP8
// depacketizer and sample builder reassemble first; this is the path pumpVideo
// takes before it reaches the bridge.
func TestVP8PacketsReassembleIntoAFrameTheBridgeAccepts(t *testing.T) {
	frame := loadKeyframe(t)
	payloader := &codecs.VP8Payloader{}
	payloads := payloader.Payload(120, frame)
	if len(payloads) < 3 {
		t.Fatalf("expected the fixture to span several packets at a 120-byte MTU, got %d", len(payloads))
	}
	builder := samplebuilder.New(64, &codecs.VP8Packet{}, 90_000)
	sequence := uint16(1000)
	for index, payload := range payloads {
		builder.Push(&rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: sequence, Timestamp: 3000,
				SSRC: 42, Marker: index == len(payloads)-1,
			},
			Payload: payload,
		})
		sequence++
	}
	// The sample builder releases a frame once it sees the start of the next.
	next := payloader.Payload(120, frame)
	builder.Push(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: sequence, Timestamp: 6000, SSRC: 42},
		Payload: next[0],
	})
	sample := builder.Pop()
	if sample == nil {
		t.Fatal("the sample builder did not release the reassembled frame")
	}
	if !bytes.Equal(sample.Data, frame) {
		t.Fatalf("reassembled frame differs from the sent one: %d vs %d bytes", len(sample.Data), len(frame))
	}
	recorder := &recordedEvents{}
	bridge := newKeyframeBridge("livekit:presenter", defaultVideoLimits(), recorder.emit)
	if sent, err := bridge.HandleFrame(sample.Data); err != nil || !sent {
		t.Fatalf("reassembled frame was not bridged: sent=%v err=%v", sent, err)
	}
	if width, height, _ := decodeFrameEvent(t, recorder.events[1]); width != 64 || height != 48 {
		t.Fatalf("frame is %dx%d, want 64x48", width, height)
	}
}

func TestVideoNegotiationIsReadFromSessionEventsAndDefaultsToOff(t *testing.T) {
	enabled, _ := json.Marshal(map[string]any{
		"type": "session.updated",
		"session": map[string]any{
			"openrealtime": map[string]any{
				"version": 1, "enabled": []string{"video.input"},
				"video": map[string]any{
					"format": "jpeg", "fps_cap": 2, "max_dimension": 640, "max_frame_bytes": 1 << 20,
				},
			},
		},
	})
	negotiation, ok := readVideoNegotiation(enabled)
	if !ok || !negotiation.enabled || negotiation.limits.FPSCap != 2 ||
		negotiation.limits.MaxDimension != 640 || negotiation.limits.MaxFrameBytes != 1<<20 {
		t.Fatalf("negotiated video was not read: ok=%v %+v", ok, negotiation)
	}

	// A server that does not implement the extension never echoes the key.
	base, _ := json.Marshal(map[string]any{
		"type": "session.created", "session": map[string]any{"model": "x"},
	})
	negotiation, ok = readVideoNegotiation(base)
	if !ok || negotiation.enabled || negotiation.limits != defaultVideoLimits() {
		t.Fatalf("a base-protocol session must read as video off with shipped limits: ok=%v %+v", ok, negotiation)
	}

	voiceOnly, _ := json.Marshal(map[string]any{
		"type": "session.updated",
		"session": map[string]any{
			"openrealtime": map[string]any{"version": 1, "enabled": []string{"observations"}},
		},
	})
	if negotiation, ok := readVideoNegotiation(voiceOnly); !ok || negotiation.enabled {
		t.Fatalf("a session that negotiated other features only must read as video off: %+v", negotiation)
	}

	if _, ok := readVideoNegotiation([]byte(`{"type":"response.done"}`)); ok {
		t.Fatal("an unrelated event must not be read as a negotiation")
	}
	if _, ok := readVideoNegotiation([]byte(`{not json`)); ok {
		t.Fatal("malformed JSON must not be read as a negotiation")
	}
}

// Video is opt-in, and the wire says so: without it the session.update is
// byte-for-byte what every existing deployment already sends.
func TestVideoIsOptInOnTheWire(t *testing.T) {
	for _, video := range []bool{false, true} {
		agent, err := New(Config{
			URL: "ws://livekit.invalid", APIKey: "k", APISecret: "s",
			Room: "r", Endpoint: "ws://endpoint.invalid/v1/realtime", Video: video,
		})
		if err != nil {
			t.Fatal(err)
		}
		if agent.config.Video != video {
			t.Fatalf("Video config = %v, want %v", agent.config.Video, video)
		}
		if _, known := agent.currentVideo(); known {
			t.Fatal("a fresh agent must not claim to know the video answer")
		}
	}
}
