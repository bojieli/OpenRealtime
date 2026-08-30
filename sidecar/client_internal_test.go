package sidecar

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSendEnforcesMultimodalVersionAndVisualCapability(t *testing.T) {
	image := Message{
		Type: TypeImage, Payload: []byte{0xff, 0xd8, 0xff}, Source: "camera.front",
		MIMEType: "image/jpeg", Width: 640, Height: 480,
	}
	tools := Message{Type: TypeToolsUpdate}

	for _, version := range []int{Version, VersionInteraction} {
		t.Run(versionName(version), func(t *testing.T) {
			var output bytes.Buffer
			client := &Client{
				version: version,
				ready: Message{Capabilities: []string{
					string(CapabilityVisualInput), string(CapabilityTools),
				}},
				writer: NewWriter(&output),
			}
			if err := client.Send(image); err == nil || !strings.Contains(err.Error(), "protocol v3") {
				t.Fatalf("direct image error = %v", err)
			}
			if err := client.Send(tools); err == nil || !strings.Contains(err.Error(), "protocol v3") {
				t.Fatalf("tool update error = %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("rejected v%d frames wrote %d bytes", version, output.Len())
			}
		})
	}

	var output bytes.Buffer
	client := &Client{version: VersionMultimodal, writer: NewWriter(&output)}
	if err := client.Send(image); err == nil || !strings.Contains(err.Error(), "visual-input capability") {
		t.Fatalf("undeclared visual capability error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("image without negotiated capability wrote %d bytes", output.Len())
	}
	client.ready.Capabilities = []string{string(CapabilityVisualInput)}
	if err := client.Send(image); err != nil {
		t.Fatalf("conformant v3 image: %v", err)
	}
	if err := client.Send(tools); err == nil || !strings.Contains(err.Error(), "tools capability") {
		t.Fatalf("undeclared tools capability error = %v", err)
	}
	client.ready.Capabilities = append(client.ready.Capabilities, string(CapabilityTools))
	if err := client.Send(tools); err != nil {
		t.Fatalf("conformant v3 tool update: %v", err)
	}
	reader := NewReader(&output)
	for _, want := range []MessageType{TypeImage, TypeToolsUpdate} {
		message, err := reader.Read()
		if err != nil {
			t.Fatalf("read %s: %v", want, err)
		}
		if message.Type != want {
			t.Fatalf("frame type = %s, want %s", message.Type, want)
		}
	}
}

func TestPrepareConfigRequiresExactlyOneTransportAndClonesSlices(t *testing.T) {
	for name, config := range map[string]Config{
		"missing": {},
		"ambiguous": {
			Command: []string{"sidecar"}, Address: "tcp:127.0.0.1:1",
		},
		"empty executable": {Command: []string{"  "}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareConfig(config); err == nil {
				t.Fatalf("invalid transport config was accepted: %+v", config)
			}
		})
	}
	command := []string{"original-sidecar", "--serve"}
	environment := []string{"TOKEN=original"}
	prepared, err := prepareConfig(Config{Command: command, Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	command[0] = "mutated-sidecar"
	environment[0] = "TOKEN=mutated"
	if prepared.Command[0] != "original-sidecar" || prepared.Environment[0] != "TOKEN=original" {
		t.Fatalf("prepared config aliases caller slices: %+v", prepared)
	}
	address, err := prepareConfig(Config{Address: "  tcp:127.0.0.1:1  "})
	if err != nil || address.Address != "tcp:127.0.0.1:1" {
		t.Fatalf("canonical address = %q, error = %v", address.Address, err)
	}
}

func TestDialRejectsAmbiguousTransportBeforeConnecting(t *testing.T) {
	_, err := Dial(t.Context(), Config{
		Command: []string{"must-not-run"}, Address: "tcp:127.0.0.1:1",
	}, Message{SampleRate: 24_000})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("ambiguous Dial error = %v", err)
	}
}

func TestDialRejectsNilContextAndUndocumentedNetworksBeforeConnecting(t *testing.T) {
	if _, err := Dial(nil, Config{Address: "tcp:127.0.0.1:1"}, Message{SampleRate: 24_000}); err == nil ||
		!strings.Contains(err.Error(), "nil context") {
		t.Fatalf("nil-context Dial error = %v", err)
	}
	if _, err := Dial(t.Context(), Config{Address: "udp:127.0.0.1:1"}, Message{SampleRate: 24_000}); err == nil ||
		!strings.Contains(err.Error(), "tcp or unix") {
		t.Fatalf("unsupported-network Dial error = %v", err)
	}
}

type blockingWriteCloser struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (transport *blockingWriteCloser) Write(payload []byte) (int, error) {
	transport.startOnce.Do(func() { close(transport.started) })
	<-transport.closed
	return 0, io.ErrClosedPipe
}

func (transport *blockingWriteCloser) Close() error {
	transport.closeOnce.Do(func() { close(transport.closed) })
	return nil
}

func TestClientCloseInterruptsABlockedGoodbyeWrite(t *testing.T) {
	transport := newBlockingWriteCloser()
	timeout := make(chan time.Time, 1)
	client := &Client{
		writer: NewWriter(transport), transport: transport,
		shutdownAfter: func(time.Duration) <-chan time.Time { return timeout },
	}
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("goodbye write did not block")
	}
	timeout <- time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked after its shutdown bound")
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("Close did not close the transport to unblock the writer")
	}
}

type signalingWriteCloser struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (transport *signalingWriteCloser) Write(payload []byte) (int, error) {
	transport.once.Do(func() { close(transport.started) })
	return transport.WriteCloser.Write(payload)
}

func TestClientCloseUnblocksANetworkPeerThatDoesNotReadGoodbye(t *testing.T) {
	engine, peer := net.Pipe()
	defer peer.Close()
	transport := &signalingWriteCloser{WriteCloser: engine, started: make(chan struct{})}
	timeout := make(chan time.Time, 1)
	client := &Client{
		writer: NewWriter(transport), transport: transport,
		shutdownAfter: func(time.Duration) <-chan time.Time { return timeout },
	}
	done := make(chan error, 1)
	go func() { done <- client.Close() }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("network goodbye write did not start")
	}
	timeout <- time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("network Close remained blocked by an unread goodbye")
	}
	buffer := make([]byte, 1)
	if _, err := peer.Read(buffer); err == nil {
		t.Fatal("peer remained open after bounded Close")
	}
}

type payloadSignalingWriteCloser struct {
	io.WriteCloser
	calls          atomic.Int32
	payloadStarted chan struct{}
}

func (transport *payloadSignalingWriteCloser) Write(payload []byte) (int, error) {
	if transport.calls.Add(1) == 2 {
		close(transport.payloadStarted)
	}
	return transport.WriteCloser.Write(payload)
}

type stagedShutdown struct {
	mu       sync.Mutex
	next     int
	channels []chan time.Time
	called   chan int
}

func newStagedShutdown(stages int) *stagedShutdown {
	result := &stagedShutdown{channels: make([]chan time.Time, stages), called: make(chan int, stages)}
	for index := range result.channels {
		result.channels[index] = make(chan time.Time, 1)
	}
	return result
}

func (shutdown *stagedShutdown) after(time.Duration) <-chan time.Time {
	shutdown.mu.Lock()
	index := shutdown.next
	shutdown.next++
	shutdown.mu.Unlock()
	if index >= len(shutdown.channels) {
		return make(chan time.Time)
	}
	shutdown.called <- index
	return shutdown.channels[index]
}

func awaitShutdownStage(t *testing.T, shutdown *stagedShutdown) int {
	t.Helper()
	select {
	case stage := <-shutdown.called:
		return stage
	case <-time.After(time.Second):
		t.Fatal("Client.Close did not enter its next bounded shutdown stage")
		return -1
	}
}

func TestClientCloseUnblocksAndReapsAProcessWithABlockedPipeWrite(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, "-test.run=^TestClientBlockedProcessHelper$")
	process.Env = append(os.Environ(), "OPENREALTIME_BLOCKED_PROCESS_HELPER=1")
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	transport := &payloadSignalingWriteCloser{
		WriteCloser: input, payloadStarted: make(chan struct{}),
	}
	shutdown := newStagedShutdown(3)
	client := &Client{
		version: Version, writer: NewWriter(transport), transport: transport, process: process,
		shutdownAfter: shutdown.after,
	}
	sendDone := make(chan error, 1)
	go func() { sendDone <- client.Send(Message{Type: TypeAudio, Payload: make([]byte, 4<<20)}) }()
	select {
	case <-transport.payloadStarted:
	case <-time.After(time.Second):
		_ = process.Process.Kill()
		_ = process.Wait()
		t.Fatal("process-pipe payload write did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	if stage := awaitShutdownStage(t, shutdown); stage != 0 {
		t.Fatalf("first shutdown stage = %d", stage)
	}
	shutdown.channels[0] <- time.Now()
	if err := <-sendDone; err == nil {
		t.Fatal("closing the process pipe did not interrupt the in-flight send")
	}
	if stage := awaitShutdownStage(t, shutdown); stage != 1 {
		t.Fatalf("second shutdown stage = %d", stage)
	}
	shutdown.channels[1] <- time.Now()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not kill and reap the blocked child process")
	}
	if process.ProcessState == nil {
		t.Fatalf("child process was not reaped: %+v", process.ProcessState)
	}
}

func TestClientBlockedProcessHelper(t *testing.T) {
	if os.Getenv("OPENREALTIME_BLOCKED_PROCESS_HELPER") != "1" {
		return
	}
	select {}
}

type recordingWriteCloser struct {
	mu     sync.Mutex
	stream bytes.Buffer
	closed bool
}

func (transport *recordingWriteCloser) Write(payload []byte) (int, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return 0, io.ErrClosedPipe
	}
	return transport.stream.Write(payload)
}

func (transport *recordingWriteCloser) Close() error {
	transport.mu.Lock()
	transport.closed = true
	transport.mu.Unlock()
	return nil
}

func (transport *recordingWriteCloser) bytes() []byte {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return bytes.Clone(transport.stream.Bytes())
}

func TestClientSendCannotWriteAfterGoodbyeDuringCloseRace(t *testing.T) {
	transport := &recordingWriteCloser{}
	timeout := make(chan time.Time)
	client := &Client{
		version: Version, writer: NewWriter(transport), transport: transport,
		shutdownAfter: func(time.Duration) <-chan time.Time { return timeout },
	}

	// Hold the ordering lock until Close has made the session unavailable. A
	// sender queued in this interval must recheck closed state under the lock.
	client.writeMu.Lock()
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	deadline := time.After(time.Second)
	for !client.closed.Load() {
		select {
		case <-deadline:
			client.writeMu.Unlock()
			t.Fatal("Close did not mark the client closed")
		default:
			runtime.Gosched()
		}
	}
	sendDone := make(chan error, 1)
	go func() { sendDone <- client.Send(Message{Type: TypeRespond}) }()
	client.writeMu.Unlock()
	if err := <-sendDone; err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("sender queued behind Close error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}

	reader := NewReader(bytes.NewReader(transport.bytes()))
	message, err := reader.Read()
	if err != nil || message.Type != TypeBye {
		t.Fatalf("ordered close frame = %+v, error = %v", message, err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("frame emitted after goodbye: %v", err)
	}
}

func TestConcurrentClientCloseEmitsOneGoodbye(t *testing.T) {
	transport := &recordingWriteCloser{}
	client := &Client{version: Version, writer: NewWriter(transport), transport: transport}
	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results <- client.Close()
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	reader := NewReader(bytes.NewReader(transport.bytes()))
	message, err := reader.Read()
	if err != nil || message.Type != TypeBye {
		t.Fatalf("concurrent close frame = %+v, error = %v", message, err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("concurrent Close emitted more than one goodbye: %v", err)
	}
}

func versionName(version int) string {
	switch version {
	case Version:
		return "v1"
	case VersionInteraction:
		return "v2"
	default:
		return "unknown"
	}
}
