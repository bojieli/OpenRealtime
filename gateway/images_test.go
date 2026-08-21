package gateway_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
)

func jpegDataURI(t *testing.T, width, height int) string {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
}

// A screenshot attached to a turn is how every computer-use client shows the
// agent what it is looking at. Dropping it left the agent acting on a screen
// it had never seen.
func TestAnImageAttachedToATurnReachesTheProvider(t *testing.T) {
	fastProvider := fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "I see it."}})
	slowProvider := slow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "A blue gradient."}})
	server := startServer(t, fastProvider, slowProvider, "unused")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configureText()
	client.await("session.updated", 5*time.Second)

	client.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "What is on the screen?"},
				{"type": "input_image", "image_url": jpegDataURI(t, 64, 48)},
			},
		},
	})
	client.await("conversation.item.created", 5*time.Second)
	client.await("response.created", 10*time.Second)
	client.await("response.done", 10*time.Second)

	// The image is referenced by handle from the observation, and resolvable:
	// a provider that can see gets the bytes, one that cannot gets the text.
	request := fastProvider.lastRequest(t)
	found := false
	for _, item := range request.Trajectory.Items {
		if item.Observation == nil {
			continue
		}
		for _, reference := range item.Observation.Media {
			found = true
			if reference.Handle == "" {
				t.Fatal("a retained image must carry a handle")
			}
			if reference.Width != 64 || reference.Height != 48 {
				t.Fatalf("geometry must survive: %dx%d", reference.Width, reference.Height)
			}
			media, err := request.Media(reference.Handle)
			if err != nil {
				t.Fatalf("the handle must resolve for a provider that can see: %v", err)
			}
			if len(media.Bytes) == 0 || media.MIMEType != "image/jpeg" {
				t.Fatalf("unexpected media %q %d bytes", media.MIMEType, len(media.Bytes))
			}
		}
	}
	if !found {
		t.Fatal("the attached image never reached the trajectory")
	}
}

// A message with no text and no image is not a message.
func TestAnEmptyMessageItemIsRefused(t *testing.T) {
	server := startServer(t, fast(), slow(), "unused")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{"type": "message", "role": "user", "content": []map[string]any{}},
	})
	client.await("error", 5*time.Second)
}

// Malformed image data is a client error worth reporting, not an image worth
// guessing at.
func TestMalformedImageDataIsRefused(t *testing.T) {
	server := startServer(t, fast(), slow(), "unused")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	for _, url := range []string{"data:image/jpeg;base64,not base64!!", "data:image/jpeg,plain", ""} {
		client.send(map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]any{{"type": "input_image", "image_url": url}},
			},
		})
		client.await("error", 5*time.Second)
	}
}
