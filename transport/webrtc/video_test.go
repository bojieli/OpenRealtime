package webrtc

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

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// The fixture is a lossy WebP, which is a VP8 key frame in a RIFF container:
// the one VP8 bitstream this project can produce without a native encoder.
func loadKeyframe(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/keyframe-64x48.webp")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 20 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" || string(data[12:16]) != "VP8 " {
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
	events []any
	fail   error
}

func (recorder *recordedEvents) emit(event any) error {
	if recorder.fail != nil {
		return recorder.fail
	}
	recorder.events = append(recorder.events, event)
	return nil
}

func decodeFrameEvent(t *testing.T, event any) (int, int, int) {
	t.Helper()
	frame, ok := event.(openrealtime.VideoFrameAppend)
	if !ok {
		t.Fatalf("event is %T, want a frame append", event)
	}
	if frame.Type != openrealtime.EventVideoFrameAppend || frame.Source == "" || frame.TimestampMS == 0 {
		t.Fatalf("frame event is incomplete: %+v", frame)
	}
	payload, err := base64.StdEncoding.DecodeString(frame.Frame)
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
	bridge := newKeyframeBridge("webrtc:screen", openrealtime.DefaultLimits(), recorder.emit)
	clock := time.Unix(1_700_000_000, 0)
	bridge.now = func() time.Time { return clock }

	sent, err := bridge.HandleFrame(frame)
	if err != nil || !sent {
		t.Fatalf("first key frame: sent=%v err=%v", sent, err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("expected a source declaration and a frame, got %d events: %+v", len(recorder.events), recorder.events)
	}
	source, ok := recorder.events[0].(openrealtime.VideoSourceUpdate)
	if !ok || source.Type != openrealtime.EventVideoSourceUpdate || source.Source != "webrtc:screen" ||
		source.State != openrealtime.SourceActive || source.Width != 64 || source.Height != 48 {
		t.Fatalf("source declaration = %+v", recorder.events[0])
	}
	if err := source.Validate(); err != nil {
		t.Fatal(err)
	}
	if width, height, _ := decodeFrameEvent(t, recorder.events[1]); width != 64 || height != 48 {
		t.Fatalf("frame is %dx%d, want 64x48", width, height)
	}
	if _, err := recorder.events[1].(openrealtime.VideoFrameAppend).Decode(openrealtime.DefaultLimits()); err != nil {
		t.Fatalf("the server would refuse the frame: %v", err)
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
	decodeFrameEvent(t, recorder.events[2])

	bridge.Close()
	closed, ok := recorder.events[len(recorder.events)-1].(openrealtime.VideoSourceUpdate)
	if !ok || closed.State != openrealtime.SourceClosed {
		t.Fatalf("closing the bridge must close the declared source, got %+v", recorder.events[len(recorder.events)-1])
	}
}

func TestKeyframeBridgeSkipsInterFramesAndGarbage(t *testing.T) {
	recorder := &recordedEvents{}
	bridge := newKeyframeBridge("webrtc:screen", openrealtime.DefaultLimits(), recorder.emit)
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

func TestKeyframeBridgeScalesToTheNegotiatedDimension(t *testing.T) {
	recorder := &recordedEvents{}
	limits := openrealtime.DefaultLimits()
	limits.MaxDimension = 32
	bridge := newKeyframeBridge("webrtc:screen", limits, recorder.emit)
	if _, err := bridge.HandleFrame(loadKeyframe(t)); err != nil {
		t.Fatal(err)
	}
	source := recorder.events[0].(openrealtime.VideoSourceUpdate)
	width, height, _ := decodeFrameEvent(t, recorder.events[1])
	if width != 32 || height != 24 || source.Width != 32 || source.Height != 24 {
		t.Fatalf("frame %dx%d declared %dx%d, want 32x24 both", width, height, source.Width, source.Height)
	}
}

func TestKeyframeBridgeLowersQualityBeforeExceedingTheByteLimit(t *testing.T) {
	frame := loadKeyframe(t)
	full := &recordedEvents{}
	if _, err := newKeyframeBridge("s", openrealtime.DefaultLimits(), full.emit).HandleFrame(frame); err != nil {
		t.Fatal(err)
	}
	_, _, fullBytes := decodeFrameEvent(t, full.events[1])

	limits := openrealtime.DefaultLimits()
	limits.MaxFrameBytes = fullBytes - 1
	squeezed := &recordedEvents{}
	if _, err := newKeyframeBridge("s", limits, squeezed.emit).HandleFrame(frame); err != nil {
		t.Fatalf("a limit one byte under the best quality must be met by a lower quality: %v", err)
	}
	if _, _, size := decodeFrameEvent(t, squeezed.events[1]); size >= fullBytes {
		t.Fatalf("frame did not shrink: %d >= %d", size, fullBytes)
	}

	limits.MaxFrameBytes = 16
	refused := &recordedEvents{}
	sent, err := newKeyframeBridge("s", limits, refused.emit).HandleFrame(frame)
	if sent || err == nil || len(refused.events) != 0 {
		t.Fatalf("a frame that cannot fit must be dropped with an error, not sent: sent=%v err=%v events=%d",
			sent, err, len(refused.events))
	}
}

func TestKeyframeBridgeReportsAnEndpointThatRefusedTheEvent(t *testing.T) {
	recorder := &recordedEvents{fail: errors.New("endpoint closed")}
	bridge := newKeyframeBridge("s", openrealtime.DefaultLimits(), recorder.emit)
	if sent, err := bridge.HandleFrame(loadKeyframe(t)); sent || err == nil {
		t.Fatalf("an emit failure must surface: sent=%v err=%v", sent, err)
	}
	if bridge.declared {
		t.Fatal("a declaration the endpoint refused must not be remembered as made")
	}
}

// The same bytes arrive from a real sender as RTP packets that the VP8
// depacketizer and sample builder have to reassemble first; this is the path
// pumpVideo takes before it reaches the bridge.
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
	// The sample builder releases a frame once it sees the start of the next one.
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
	bridge := newKeyframeBridge("webrtc:screen", openrealtime.DefaultLimits(), recorder.emit)
	if sent, err := bridge.HandleFrame(sample.Data); err != nil || !sent {
		t.Fatalf("reassembled frame was not bridged: sent=%v err=%v", sent, err)
	}
	if width, height, _ := decodeFrameEvent(t, recorder.events[1]); width != 64 || height != 48 {
		t.Fatalf("frame is %dx%d, want 64x48", width, height)
	}
}

func TestVideoNegotiationIsReadFromSessionEventsAndDefaultsToOff(t *testing.T) {
	enabled := map[string]any{
		"type": "session.updated",
		"session": map[string]any{
			"openrealtime": map[string]any{
				"version": 1, "enabled": []string{"video.input"},
				"video": map[string]any{"format": "jpeg", "fps_cap": 2, "max_dimension": 640, "max_frame_bytes": 1 << 20},
			},
		},
	}
	raw, _ := json.Marshal(enabled)
	negotiation, ok := readVideoNegotiation(raw)
	if !ok || !negotiation.enabled || negotiation.limits.FPSCap != 2 || negotiation.limits.MaxDimension != 640 ||
		negotiation.limits.MaxFrameBytes != 1<<20 {
		t.Fatalf("negotiated video was not read: ok=%v %+v", ok, negotiation)
	}

	base, _ := json.Marshal(map[string]any{"type": "session.created", "session": map[string]any{"model": "x"}})
	negotiation, ok = readVideoNegotiation(base)
	if !ok || negotiation.enabled || negotiation.limits != openrealtime.DefaultLimits() {
		t.Fatalf("a base-protocol session must read as video off with shipped limits: ok=%v %+v", ok, negotiation)
	}

	voiceOnly, _ := json.Marshal(map[string]any{
		"type":    "session.updated",
		"session": map[string]any{"openrealtime": map[string]any{"version": 1, "enabled": []string{"observations"}}},
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
