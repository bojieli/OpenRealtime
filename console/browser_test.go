package console_test

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/console"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/bojieli/OpenRealtime/transport/webrtc"
)

// The console is the only client that exercises the whole system at once, and
// most of what it does cannot be checked from Go: capture, playout, the client
// half of the chunk framing, and whether a real browser's WebRTC stack talks to
// the adapter at all.
//
// So this test assembles the real thing - the real gateway, the real cascade
// binding, the real WebRTC adapter, the real console - puts scripted providers
// where the models would be, and drives it with Chromium.
//
// It skips when Chromium or Node is missing rather than failing, because the
// gate has to run offline on a machine with neither.

func requireBrowser(t *testing.T) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the browser test")
	}
	chromium := os.Getenv("CHROMIUM")
	if chromium == "" {
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if found, err := exec.LookPath(candidate); err == nil {
				chromium = found
				break
			}
		}
	}
	if chromium == "" {
		t.Skip("chromium is not installed; skipping the browser test")
	}
	return node, chromium
}

// --- scripted stand-ins for the models --------------------------------------

type staticRecogniser struct{ text string }

func (staticRecogniser) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "static", Version: "1"}
}
func (staticRecogniser) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (recogniser staticRecogniser) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 1, StableText: recogniser.text, Final: true}, nil
}
func (staticRecogniser) Close() error { return nil }

type scriptedProvider struct {
	descriptor continuation.Descriptor
	mu         sync.Mutex
	turns      [][]continuation.Event
	calls      int
}

func (provider *scriptedProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *scriptedProvider) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	var events []continuation.Event
	if len(provider.turns) > 0 {
		events = provider.turns[index%len(provider.turns)]
	}
	provider.mu.Unlock()
	for _, event := range events {
		if event.ToolCall != nil {
			// A fresh identifier per invocation. A cycling script that reused
			// one would be a duplicate call, which the trajectory refuses -
			// correctly, and it would look like a console defect.
			call := *event.ToolCall
			call.CallID = "call_" + strings.Repeat("x", index%3) + itoa(index)
			event.ToolCall = &call
		}
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

type toneSynthesiser struct{}

func (toneSynthesiser) Descriptor() v1.Descriptor { return v1.Descriptor{Name: "tone", Version: "1"} }
func (toneSynthesiser) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, errors.New("the console test uses the streaming path")
}

// Stream emits half a second of tone, so the page has real audio to decode and
// schedule rather than a buffer of zeros that would prove less.
func (toneSynthesiser) Stream(_ context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error) error {
	const samples = 12000
	pcm := make([]byte, samples*2)
	for index := range samples {
		value := int16(6000 * math.Sin(2*math.Pi*440*float64(index)/24000))
		pcm[index*2] = byte(value)
		pcm[index*2+1] = byte(value >> 8)
	}
	return emit(v1.SpeechChunk{
		ChunkID: "chunk", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: pcm, Final: true,
	})
}

// --- the stack --------------------------------------------------------------

func startStack(t *testing.T, root string) (protocolURL, adapterURL string) {
	t.Helper()
	fast := &scriptedProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}, turns: [][]continuation.Event{
		{{Kind: continuation.EventAssistantDelta, Text: "Let me look."}},
		{{Kind: continuation.EventAssistantDelta, Text: "Here is what I found."}},
	}}
	slow := &scriptedProvider{descriptor: continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}, turns: [][]continuation.Event{
		{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			Name: "read_file", Arguments: []byte(`{"path":"notes.txt"}`),
		}}},
		{{Kind: continuation.EventAssistantDelta, Text: "The notes say the deadline moved to Friday."}},
	}}

	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return staticRecogniser{text: "please read the notes file"}, nil
		},
		Fast: fast, Slow: slow, Speech: toneSynthesiser{},
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: perception.StaticNarrator{
				Text: "The screen shows an editor with a file open and a terminal below it.",
			},
			Cadence: 2 * time.Second,
		})},
	})
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	server, err := gateway.New(gateway.Config{Binding: bind, ValidateWire: true})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	// A real listener rather than httptest, because the browser reaches the
	// adapter over ICE and the adapter reaches the gateway as an ordinary
	// client; both need addresses that exist.
	protocol := httptest.NewServer(server.Handler())
	t.Cleanup(protocol.Close)
	protocolURL = "ws" + strings.TrimPrefix(protocol.URL, "http") + "/v1/realtime"

	adapter, err := webrtc.New(webrtc.Config{Endpoint: protocolURL})
	if err != nil {
		t.Fatalf("webrtc adapter: %v", err)
	}
	adapterServer := httptest.NewServer(adapter.Handler())
	t.Cleanup(adapterServer.Close)
	return protocolURL, adapterServer.URL + "/v1/realtime"
}

func startConsoleServer(t *testing.T, protocolURL, adapterURL, root string) string {
	t.Helper()
	host, err := console.NewHost(console.HostConfig{
		Root: root, Enabled: []string{"read_file", "list_directory", "write_file"},
	})
	if err != nil {
		t.Fatalf("tool host: %v", err)
	}
	server, err := console.New(console.Config{
		Endpoint: protocolURL, WebRTCEndpoint: adapterURL, Host: host,
	})
	if err != nil {
		t.Fatalf("console: %v", err)
	}
	// The page must be served from a loopback address for the browser to treat
	// it as a secure context and hand it a microphone, which httptest gives.
	local := httptest.NewServer(server.Handler())
	t.Cleanup(local.Close)
	return local.URL
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

func TestTheConsoleDrivesARealSession(t *testing.T) {
	node, chromium := requireBrowser(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve the root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "notes.txt"),
		[]byte("The deadline moved to Friday.\nOwner: the developer.\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	protocolURL, adapterURL := startStack(t, resolved)
	consoleURL := startConsoleServer(t, protocolURL, adapterURL, resolved)

	driver, err := filepath.Abs(filepath.Join("testdata", "browser.mjs"))
	if err != nil {
		t.Fatalf("locate the driver: %v", err)
	}

	for _, mode := range []string{"websocket", "webrtc"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, node, driver, consoleURL, mode)
			command.Env = append(os.Environ(),
				"CHROMIUM="+chromium,
				"CDP_PORT="+freePort(t))
			output, err := command.CombinedOutput()
			t.Log("\n" + string(output))
			if err != nil {
				t.Fatalf("the browser run failed: %v", err)
			}
		})
	}
}

// A console started with no tool host still has to be a complete client for
// everything except tool use, and the page has to say so rather than offering
// tools that cannot run.
func TestTheConsoleWithoutToolsStillServesAWorkingPage(t *testing.T) {
	t.Parallel()
	fake := newEndpoint(t)
	local := startConsole(t, console.Config{Endpoint: fake.url()})
	response, err := http.Get(local.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected the page, got %d", response.StatusCode)
	}
}
