package sidecar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures a sidecar connection.
type Config struct {
	// Command runs a sidecar as a child process, talking over its stdin and
	// stdout. This is the ordinary case: the engine owns the process, and its
	// lifetime is the session's.
	Command []string
	// Address connects to an already-running sidecar over TCP or a Unix
	// socket, as "tcp:host:port" or "unix:/path". Use it when the model is
	// expensive enough to load once and share.
	Address string
	// Directory is the working directory for a spawned command.
	Directory string
	// Environment is added to a spawned command's environment.
	Environment []string
	// Handshake bounds how long the engine waits for Ready. Model loading can
	// be slow, so this is generous by default.
	Handshake time.Duration
	// Logf receives the sidecar's own log frames and any process stderr.
	Logf func(string, ...any)
	// ProtocolVersion selects the wire contract. Zero keeps frozen v1.
	// VersionInteraction must be selected to send typed interaction acts.
	ProtocolVersion int
}

// Client is one connection to a sidecar.
type Client struct {
	config Config

	process *exec.Cmd
	conn    io.Closer
	writer  *Writer
	reader  *Reader

	ready   Message
	frames  chan Message
	closed  atomic.Bool
	once    sync.Once
	readErr atomic.Pointer[error]
	version int
}

// Dial starts or connects to a sidecar and completes the handshake.
func Dial(ctx context.Context, config Config, hello Message) (*Client, error) {
	if len(config.Command) == 0 && strings.TrimSpace(config.Address) == "" {
		return nil, errors.New("a sidecar needs a command to run or an address to connect to")
	}
	if config.Handshake <= 0 {
		config.Handshake = 120 * time.Second
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	version := config.ProtocolVersion
	if version == 0 {
		version = Version
	}
	if version != Version && version != VersionInteraction && version != VersionMultimodal &&
		version != VersionElementGraph {
		return nil, fmt.Errorf("unsupported sidecar protocol version %d", version)
	}
	client := &Client{config: config, frames: make(chan Message, 256), version: version}
	if err := client.open(ctx); err != nil {
		return nil, err
	}

	hello.Type = TypeHello
	hello.Version = version
	if err := client.writer.Write(hello); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("send hello: %w", err)
	}
	ready, err := client.awaitReady(ctx, hello)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	client.ready = ready
	go client.read(ctx)
	return client, nil
}

func (client *Client) open(ctx context.Context) error {
	if address := strings.TrimSpace(client.config.Address); address != "" {
		network, target, found := strings.Cut(address, ":")
		if !found || network == "" || target == "" {
			return fmt.Errorf("sidecar address must be tcp:host:port or unix:/path, got %q", address)
		}
		connection, err := (&net.Dialer{}).DialContext(ctx, network, target)
		if err != nil {
			return fmt.Errorf("connect to sidecar at %s: %w", address, err)
		}
		client.conn = connection
		client.writer, client.reader = NewWriter(connection), NewReader(connection)
		return nil
	}

	process := exec.Command(client.config.Command[0], client.config.Command[1:]...)
	process.Dir = client.config.Directory
	process.Env = append(os.Environ(), client.config.Environment...)
	input, err := process.StdinPipe()
	if err != nil {
		return err
	}
	output, err := process.StdoutPipe()
	if err != nil {
		return err
	}
	diagnostics, err := process.StderrPipe()
	if err != nil {
		return err
	}
	if err := process.Start(); err != nil {
		return fmt.Errorf("start sidecar %q: %w", client.config.Command[0], err)
	}
	client.process = process
	client.writer, client.reader = NewWriter(input), NewReader(output)
	// A sidecar's stderr is where a Python traceback lands. Forwarding it is
	// the difference between "the model failed" and knowing why.
	go func() {
		scanner := bufio.NewScanner(diagnostics)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			client.config.Logf("sidecar: %s", scanner.Text())
		}
	}()
	return nil
}

func (client *Client) awaitReady(ctx context.Context, hello Message) (Message, error) {
	type outcome struct {
		message Message
		err     error
	}
	results := make(chan outcome, 1)
	go func() {
		for {
			message, err := client.reader.Read()
			if err != nil {
				results <- outcome{err: err}
				return
			}
			switch message.Type {
			case TypeReady:
				results <- outcome{message: message}
				return
			case TypeLog:
				client.config.Logf("sidecar: %s", message.Text)
			case TypeError:
				results <- outcome{err: fmt.Errorf("sidecar refused the session: %s", message.Message())}
				return
			}
		}
	}()
	select {
	case result := <-results:
		if result.err != nil {
			return Message{}, fmt.Errorf("sidecar handshake: %w", result.err)
		}
		if result.message.Version != client.version {
			return Message{}, fmt.Errorf(
				"sidecar speaks protocol version %d, this engine speaks %d",
				result.message.Version, client.version)
		}
		if client.version == VersionElementGraph {
			if err := ValidateElementReady(hello, result.message); err != nil {
				return Message{}, fmt.Errorf("sidecar element readiness: %w", err)
			}
		}
		return result.message, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case <-time.After(client.config.Handshake):
		return Message{}, fmt.Errorf("sidecar did not become ready within %s", client.config.Handshake)
	}
}

// Ready returns what the sidecar declared at the handshake.
func (client *Client) Ready() Message { return client.ready }

// Frames yields sidecar messages until the connection ends.
func (client *Client) Frames() <-chan Message { return client.frames }

// Err reports why reading stopped.
func (client *Client) Err() error {
	if pointer := client.readErr.Load(); pointer != nil {
		return *pointer
	}
	return nil
}

// Send writes one frame to the sidecar.
func (client *Client) Send(message Message) error {
	if client.closed.Load() {
		return errors.New("sidecar connection is closed")
	}
	if message.Type == TypeInteractionAct {
		if client.version < VersionInteraction {
			return errors.New("typed interaction acts require sidecar protocol v2")
		}
		if !client.ready.Has(CapabilityInteractionActs) {
			return errors.New("sidecar did not declare the interaction-acts capability")
		}
		if message.DeadlineMS <= time.Now().UnixMilli() {
			return errors.New("typed interaction plan expired before it could be sent")
		}
	}
	if message.Type == TypeElementFrame && client.version != VersionElementGraph {
		return errors.New("generic element frames require sidecar protocol v4")
	}
	return client.writer.Write(message)
}

// Audio sends one input frame.
func (client *Client) Audio(payload []byte) error {
	return client.Send(Message{Type: TypeAudio, Payload: payload})
}

// Image sends one direct visual frame. The capability and protocol checks are
// made here so a caller cannot mistake a silently discarded frame for vision.
func (client *Client) Image(payload []byte, source, mimeType string, width, height int, timestampMS int64) error {
	if client.version < VersionMultimodal {
		return errors.New("direct visual input requires sidecar protocol v3")
	}
	if !client.ready.Has(CapabilityVisualInput) {
		return errors.New("sidecar did not declare the visual-input capability")
	}
	return client.Send(Message{
		Type: TypeImage, Payload: payload, Source: source, MIMEType: mimeType,
		Width: width, Height: height, TimestampMS: timestampMS,
	})
}

// Close ends the session and reaps the process.
func (client *Client) Close() error {
	var err error
	client.once.Do(func() {
		client.closed.Store(true)
		if client.writer != nil {
			_ = client.writer.Write(Message{Type: TypeBye})
		}
		if client.conn != nil {
			err = client.conn.Close()
		}
		if client.process != nil {
			// A sidecar that has been told goodbye should exit; one that does
			// not is killed rather than left holding a GPU.
			done := make(chan struct{})
			go func() {
				_ = client.process.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = client.process.Process.Kill()
				<-done
			}
		}
	})
	return err
}

func (client *Client) read(ctx context.Context) {
	defer close(client.frames)
	for {
		message, err := client.reader.Read()
		if err != nil {
			if !client.closed.Load() && !errors.Is(err, io.EOF) {
				client.readErr.Store(&err)
			}
			return
		}
		if message.Type == TypeLog {
			client.config.Logf("sidecar: %s", message.Text)
			continue
		}
		if message.Type == TypeBye {
			return
		}
		select {
		case client.frames <- message:
		case <-ctx.Done():
			return
		}
	}
}
