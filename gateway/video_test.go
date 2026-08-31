package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func screenFrame(t *testing.T, shade uint8, patch image.Rectangle) string {
	t.Helper()
	canvas := image.NewGray(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			canvas.SetGray(x, y, color.Gray{Y: shade})
		}
	}
	for y := patch.Min.Y; y < patch.Max.Y; y++ {
		for x := patch.Min.X; x < patch.Max.X; x++ {
			canvas.SetGray(x, y, color.Gray{Y: 255 - shade})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func startVideoServer(t *testing.T, narration string) *httptest.Server {
	t.Helper()
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "what is on screen"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: perception.StaticNarrator{Text: narration}, Cadence: 0,
		})},
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	server, err := gateway.New(gateway.Config{Binding: bind, ValidateWire: true})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	http := httptest.NewServer(testGatewayHandler(server))
	t.Cleanup(http.Close)
	return http
}

func TestVideoEntersAsProtocolEventsAndProducesObservations(t *testing.T) {
	narration := "A confirmation dialog is open: Confirm payment of $40."
	server := startVideoServer(t, narration)
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input", "observations"},
	})
	updated := client.await("session.updated", 5*time.Second)
	extension := updated["session"].(map[string]any)["openrealtime"].(map[string]any)
	enabled := extension["enabled"].([]any)
	if len(enabled) != 2 {
		t.Fatalf("expected video and observations enabled, got %v", enabled)
	}
	limits := extension["video"].(map[string]any)
	if limits["format"] != "jpeg" || int(limits["fps_cap"].(float64)) != 3 {
		t.Fatalf("the server must state its video limits: %v", limits)
	}

	// Geometry is declared before frames, because a click coordinate is
	// meaningless without the space the model saw.
	client.send(map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "active", "width": 320, "height": 240,
	})
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame": screenFrame(t, 20, image.Rect(60, 60, 260, 200)), "timestamp_ms": 1000,
	})
	observation := client.await(openrealtime.EventObservationAdded, 5*time.Second)
	if observation["text"] != narration {
		t.Fatalf("unexpected observation %v", observation["text"])
	}
	if observation["observer"] != "video" || observation["source"] != "screen" {
		t.Fatalf("the observation must carry its provenance: %v", observation)
	}
	// Screen content is data forever, and the wire says so.
	if observation["authority"] != string(trajectory.AuthorityObserver) {
		t.Fatalf("expected observer authority on the wire, got %v", observation["authority"])
	}
}

func TestFramesFromAnUndeclaredSourceAreRefused(t *testing.T) {
	server := startVideoServer(t, "a screen")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input"},
	})
	client.await("session.updated", 5*time.Second)
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame": screenFrame(t, 20, image.Rect(0, 0, 0, 0)),
	})
	failure := client.await("error", 5*time.Second)
	message := failure["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "never declared") {
		t.Fatalf("expected an undeclared-source error, got %q", message)
	}
}

func TestAPausedSourceStopsProducingObservations(t *testing.T) {
	server := startVideoServer(t, "a screen")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input", "observations"},
	})
	client.await("session.updated", 5*time.Second)
	client.send(map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "paused", "width": 320, "height": 240,
	})
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame": screenFrame(t, 20, image.Rect(60, 60, 260, 200)),
	})
	// Nothing should arrive. Confirm the session still works by updating it.
	client.configurePCM16(nil)
	next := client.await("session.updated", 5*time.Second)
	if next["type"] != "session.updated" {
		t.Fatal("the session must remain usable")
	}
	for _, message := range client.received {
		if message["type"] == openrealtime.EventObservationAdded {
			t.Fatal("a paused source must produce nothing")
		}
	}
}

func TestVideoIsRefusedWhenTheBindingHasNoVideoObserver(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input"},
	})
	updated := client.await("session.updated", 5*time.Second)
	extension := updated["session"].(map[string]any)["openrealtime"].(map[string]any)
	if len(extension["enabled"].([]any)) != 0 {
		t.Fatal("a binding with no video observer must not enable video")
	}
	if _, present := extension["video"]; present {
		t.Fatal("limits are only stated for a capability that was enabled")
	}
	_ = context.Background()
}

// noiseFrame encodes a JPEG of at least minimumBytes.
//
// Noise rather than a pattern because the point is size: a compressible image
// would need dimensions far past the declared maximum to reach a useful byte
// count, and the two limits are independent.
func noiseFrame(t *testing.T, minimumBytes int) (string, int, int) {
	t.Helper()
	random := rand.New(rand.NewSource(1))
	for edge := 128; edge <= 1280; edge *= 2 {
		canvas := image.NewGray(image.Rect(0, 0, edge, edge))
		for index := range canvas.Pix {
			canvas.Pix[index] = uint8(random.Intn(256))
		}
		var buffer bytes.Buffer
		if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 98}); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if buffer.Len() >= minimumBytes {
			return base64.StdEncoding.EncodeToString(buffer.Bytes()), edge, edge
		}
	}
	t.Fatalf("could not reach %d bytes within the declared dimension limit", minimumBytes)
	return "", 0, 0
}

func startBoundedVideoServer(t *testing.T, maxFrameBytes int) *httptest.Server {
	t.Helper()
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "what is on screen"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: perception.StaticNarrator{Text: "a screen"}, Cadence: 0,
		})},
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	server, err := gateway.New(gateway.Config{
		Binding: bind, ValidateWire: true,
		// Small enough that the audio bound alone could never carry a frame,
		// which is the condition the read limit used to be derived from.
		MaxAudioFrameBytes: 4096,
		VideoLimits:        openrealtime.Limits{MaxFrameBytes: maxFrameBytes},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	http := httptest.NewServer(testGatewayHandler(server))
	t.Cleanup(http.Close)
	return http
}

// A frame within the advertised limit has to arrive whole. The read limit sits
// below the protocol, so getting it wrong does not refuse the frame - it drops
// the connection, and the client is left without even an error to act on.
func TestAFrameWithinTheAdvertisedLimitIsCarriedWhole(t *testing.T) {
	const maxFrameBytes = 512 << 10
	server := startBoundedVideoServer(t, maxFrameBytes)
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input", "observations"},
	})
	updated := client.await("session.updated", 5*time.Second)
	limits := updated["session"].(map[string]any)["openrealtime"].(map[string]any)["video"].(map[string]any)
	if int(limits["max_frame_bytes"].(float64)) != maxFrameBytes {
		t.Fatalf("the server must advertise the limit it enforces: %v", limits)
	}

	frame, width, height := noiseFrame(t, 8*(1<<10)+1)
	if base64.StdEncoding.DecodedLen(len(frame)) > maxFrameBytes {
		t.Fatalf("the test frame must be legal: %d bytes", base64.StdEncoding.DecodedLen(len(frame)))
	}
	client.send(map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "active", "width": width, "height": height,
	})
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame": frame, "timestamp_ms": 1000,
	})
	observation := client.await(openrealtime.EventObservationAdded, 5*time.Second)
	if observation["source"] != "screen" {
		t.Fatalf("unexpected observation %v", observation)
	}
}

// An oversized frame is a client that forgot to downscale, and the useful
// answer is the protocol's own error on a session that stays up.
func TestAnOversizedFrameIsRefusedWithoutClosingTheSession(t *testing.T) {
	const maxFrameBytes = 32 << 10
	server := startBoundedVideoServer(t, maxFrameBytes)
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input", "observations"},
	})
	client.await("session.updated", 5*time.Second)
	client.send(map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "active", "width": 320, "height": 240,
	})

	// The size check runs before the image is ever decoded, so the payload
	// only has to be the wrong size rather than a real picture.
	oversized := make([]byte, maxFrameBytes+1024)
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame": base64.StdEncoding.EncodeToString(oversized),
	})
	failure := client.await("error", 5*time.Second)
	message := failure["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "exceeds") {
		t.Fatalf("expected a frame-size error, got %q", message)
	}

	// The session is still usable, which is the whole difference between an
	// error and a read limit.
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "screen",
		"frame": screenFrame(t, 20, image.Rect(60, 60, 260, 200)),
	})
	client.await(openrealtime.EventObservationAdded, 5*time.Second)
}
