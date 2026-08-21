package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
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
	http := httptest.NewServer(server.Handler())
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
