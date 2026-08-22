package browser_test

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/examples/browser"
	"github.com/bojieli/OpenRealtime/internal/testserver"
)

// The demo, run rather than grepped.
//
// The test beside this one says a page that had drifted would be worse than no
// demo because it looks like it works, and then checks that by searching the
// served bytes for event names. Greppable and runnable are different
// properties: a syntax error, a renamed element, or a flow that no longer
// completes all survive a search intact. Nothing had ever loaded this page.
//
// It matters more than for most files. This is the one client authors copy,
// and the first thing anybody points at a deployment they are unsure of.
//
// Skips when Chromium or Node is missing, like the other browser suites, so
// the gate still runs offline.
func TestTheDemoHoldsAConversation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the demo browser test")
	}
	chromium := os.Getenv("CHROMIUM")
	if chromium == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if found, lookErr := exec.LookPath(candidate); lookErr == nil {
				chromium = found
				break
			}
		}
	}
	if chromium == "" {
		t.Skip("chromium is not installed; skipping the demo browser test")
	}

	// The demo is served from its own origin and reaches the adapter across
	// one, which is how it is actually deployed and why the adapter has to be
	// told the origin is allowed.
	stack := testserver.Start(t, testserver.Config{
		Transcript:     "what is the weather",
		AllowedOrigins: []string{"*"},
	})
	page := httptest.NewServer(browser.Handler())
	t.Cleanup(page.Close)

	// Finite audio, because the demo has no mute control. Chromium's synthetic
	// tone never stops, so the server's gate hears someone who has not finished
	// talking and correctly never ends the turn - and this page has no way to
	// tell it otherwise. A file that plays once and stops is how a person using
	// the demo actually ends a turn: by stopping.
	speech := writeUtterance(t)

	driver, err := filepath.Abs(filepath.Join("testdata", "demo.mjs"))
	if err != nil {
		t.Fatalf("locate the driver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver,
		page.URL+"/?adapter="+stack.AdapterURL)
	command.Env = append(os.Environ(), "CHROMIUM="+chromium, "CDP_PORT="+freeDemoPort(t),
		"DEMO_FAKE_AUDIO="+speech)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		t.Fatalf("the demo browser run failed: %v", err)
	}
}

func freeDemoPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

// writeUtterance writes a second of tone as a 16-bit PCM WAV, which is what
// Chromium's fake capture device accepts. The recogniser behind this test
// reports a fixed transcript for whatever it is given, so the audio only has
// to be loud enough for the gate to hear it and short enough to stop.
func writeUtterance(t *testing.T) string {
	t.Helper()
	const rate = 48000
	const samples = rate
	pcm := make([]byte, samples*2)
	for index := range samples {
		value := int16(12000 * math.Sin(2*math.Pi*440*float64(index)/rate))
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(value))
	}
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(pcm)))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], rate)
	binary.LittleEndian.PutUint32(header[28:], rate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))

	path := filepath.Join(t.TempDir(), "utterance.wav")
	if err := os.WriteFile(path, append(header, pcm...), 0o644); err != nil {
		t.Fatalf("write the utterance: %v", err)
	}
	return path
}
