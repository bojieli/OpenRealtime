package sidecar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/sidecar"
)

func TestStreamingTextPreservesWhitespaceTokens(t *testing.T) {
	var wire bytes.Buffer
	writer := sidecar.NewWriter(&wire)
	parts := []string{"Hello", " ", "world", "\n", "\t", "again"}
	for _, part := range parts {
		if err := writer.Write(sidecar.Message{Type: sidecar.TypeTextDelta, Text: part}); err != nil {
			t.Fatalf("write token %q: %v", part, err)
		}
	}
	reader := sidecar.NewReader(&wire)
	var text strings.Builder
	for range parts {
		message, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(message.Text)
	}
	if got, want := text.String(), strings.Join(parts, ""); got != want {
		t.Fatalf("streamed text = %q, want %q", got, want)
	}
	for _, message := range []sidecar.Message{
		{Type: sidecar.TypeTextDelta},
		{Type: sidecar.TypeTranscript, Text: " \n"},
		{Type: sidecar.TypeText, Text: " \n"},
	} {
		if err := message.Validate(); err == nil {
			t.Fatalf("accepted empty content: %#v", message)
		}
	}
}

func TestFramingRoundTripsHeadersAndPayloads(t *testing.T) {
	var buffer bytes.Buffer
	writer := sidecar.NewWriter(&buffer)
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	messages := []sidecar.Message{
		{Type: sidecar.TypeHello, Version: sidecar.Version, SampleRate: 24000, Instructions: "be brief"},
		{Type: sidecar.TypeAudio, Payload: payload},
		{Type: sidecar.TypeTextDelta, Text: "hello"},
		{Type: sidecar.TypeTurnDone},
	}
	for _, message := range messages {
		if err := writer.Write(message); err != nil {
			t.Fatalf("write %s: %v", message.Type, err)
		}
	}
	reader := sidecar.NewReader(&buffer)
	for index, want := range messages {
		got, err := reader.Read()
		if err != nil {
			t.Fatalf("read %d: %v", index, err)
		}
		if got.Type != want.Type {
			t.Fatalf("frame %d: got %s, want %s", index, got.Type, want.Type)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d payload mismatch", index)
		}
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

// A payload that does not follow its declared length would desynchronise the
// stream permanently, so it is caught at the frame rather than three frames
// later.
func TestTruncatedPayloadIsRejected(t *testing.T) {
	var buffer bytes.Buffer
	header, _ := json.Marshal(sidecar.Message{Type: sidecar.TypeAudio, PayloadBytes: 8})
	buffer.Write(append(header, '\n'))
	buffer.Write([]byte{1, 2})
	if _, err := sidecar.NewReader(&buffer).Read(); err == nil {
		t.Fatal("a truncated payload must be an error")
	}
}

func TestNilStreamsFailWithoutPanicking(t *testing.T) {
	if err := sidecar.NewWriter(nil).Write(sidecar.Message{Type: sidecar.TypeBye}); err == nil ||
		!strings.Contains(err.Error(), "nil writer") {
		t.Fatalf("nil writer error = %v", err)
	}
	if _, err := sidecar.NewReader(nil).Read(); err == nil ||
		!strings.Contains(err.Error(), "nil reader") {
		t.Fatalf("nil reader error = %v", err)
	}
}

func TestFramingRejectsUnterminatedHeadersAndShortWrites(t *testing.T) {
	if _, err := sidecar.NewReader(strings.NewReader(`{"type":"bye"}`)).Read(); err == nil ||
		!strings.Contains(err.Error(), "newline-terminated") {
		t.Fatalf("unterminated header error = %v", err)
	}
	if err := sidecar.NewWriter(shortWriter{}).Write(sidecar.Message{Type: sidecar.TypeBye}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short header write error = %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	return len(payload) - 1, nil
}

func TestMalformedFramesAreRejectedWithAUsefulReason(t *testing.T) {
	cases := map[string]sidecar.Message{
		"audio with no payload":  {Type: sidecar.TypeAudio},
		"odd-length PCM":         {Type: sidecar.TypeAudio, Payload: []byte{1, 2, 3}},
		"hello with no rate":     {Type: sidecar.TypeHello, Version: 1},
		"ready with no model":    {Type: sidecar.TypeReady, Version: 1, OutputRate: 24000},
		"tool call with no name": {Type: sidecar.TypeToolCall, CallID: "c1"},
		"unknown type":           {Type: "invent"},
		"empty text delta":       {Type: sidecar.TypeTextDelta},
	}
	for name, message := range cases {
		if err := message.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	valid := sidecar.Message{
		Type: sidecar.TypeReady, Version: 1, OutputRate: 24000, Model: "test",
		Capabilities: []string{string(sidecar.CapabilityTranscript)},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid ready frame was rejected: %v", err)
	}
	if !valid.Has(sidecar.CapabilityTranscript) || valid.Has(sidecar.CapabilityTools) {
		t.Fatal("capability reporting is wrong")
	}
}

func TestToolResultRequiresExactlyOneOutcome(t *testing.T) {
	both := sidecar.Message{
		Type: sidecar.TypeToolResult, CallID: "c1",
		Output: json.RawMessage(`{"a":1}`), Error: "failed",
	}
	if err := both.Validate(); err == nil {
		t.Fatal("a result cannot both succeed and fail")
	}
	neither := sidecar.Message{Type: sidecar.TypeToolResult, CallID: "c1"}
	if err := neither.Validate(); err == nil {
		t.Fatal("a result must have an outcome")
	}
}

func TestTypedInteractionActsCarryTheirFloorMeaning(t *testing.T) {
	floors := map[string]string{
		"listen": "unchanged", "speak-through": "preserve", "answer": "take",
		"interrupt": "take", "act-silently": "unchanged",
		"keep-speaking": "unchanged", "stop-speaking": "yield",
	}
	for act, floor := range floors {
		message := sidecar.Message{
			Type: sidecar.TypeInteractionAct, Act: act, Floor: floor,
			Policy: "test", EvidenceRef: "revision:1", Confidence: .8,
			DeadlineMS: time.Now().Add(time.Second).UnixMilli(),
		}
		if err := message.Validate(); err != nil {
			t.Fatalf("%s: %v", act, err)
		}
		message.Floor = "take"
		if floor != "take" && message.Validate() == nil {
			t.Fatalf("%s accepted the wrong floor semantics", act)
		}
	}
}

func TestProtocolV2HelloSelectsOwnersSeparatelyFromCapabilities(t *testing.T) {
	hello := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionInteraction, SampleRate: 24_000,
		InteractionOwner: "engine", FloorOwner: "model",
	}
	if err := hello.Validate(); err != nil {
		t.Fatalf("independent ownership selection was rejected: %v", err)
	}
	hello.InteractionOwner = "duplex"
	if err := hello.Validate(); err == nil {
		t.Fatal("a model species must not be accepted where an owner is required")
	}
}

func TestClientRefusesAnExpiredTypedActBeforeWritingIt(t *testing.T) {
	binary := writeEchoSidecar(t)
	client, err := sidecar.Dial(context.Background(), sidecar.Config{
		Command: []string{binary}, ProtocolVersion: sidecar.VersionInteraction,
		Environment: []string{"ECHO_SIDECAR_VERSION=2"},
	}, sidecar.Message{
		SampleRate: 24_000, InteractionOwner: "engine", FloorOwner: "engine",
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	err = client.Send(sidecar.Message{
		Type: sidecar.TypeInteractionAct, Act: "answer", Floor: "take",
		Policy: "test", EvidenceRef: "revision:1",
		DeadlineMS: time.Now().Add(-time.Millisecond).UnixMilli(),
	})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired plan error = %v", err)
	}
}

// writeEchoSidecar builds a minimal conformant sidecar as a Go program, so the
// suite is verified against something that actually speaks the protocol
// without needing Python in the test path.
func writeEchoSidecar(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "main.go")
	if err := os.WriteFile(source, []byte(echoSidecarSource), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"),
		[]byte("module echosidecar\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	binary := filepath.Join(directory, "echosidecar")
	build := exec.Command(goBinary(t), "build", "-o", binary, ".")
	build.Dir = directory
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sidecar: %v\n%s", err, output)
	}
	return binary
}

func goBinary(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/usr/local/go/bin/go", "go"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no Go toolchain available to build the reference sidecar")
	return ""
}

func TestConformanceSuitePassesAgainstAConformantSidecar(t *testing.T) {
	binary := writeEchoSidecar(t)
	report := sidecar.RunConformance(context.Background(), sidecar.ConformanceOptions{
		Config:        sidecar.Config{Command: []string{binary}},
		SpeechSeconds: 0.2, TurnTimeout: 10 * time.Second,
	})
	if !report.Passed {
		t.Fatalf("a conformant sidecar must pass: %v", report.Failures)
	}
	if report.Model != "echo" || report.OutputRate != 24000 {
		t.Fatalf("the report must carry what the sidecar declared: %+v", report)
	}
	required := 0
	for _, check := range report.Checks {
		if check.Required {
			required++
		}
	}
	if required < 10 {
		t.Fatalf("the contract should be more than %d required checks", required)
	}
}

func TestConformanceFailsOnAVersionMismatch(t *testing.T) {
	binary := writeEchoSidecar(t)
	report := sidecar.RunConformance(context.Background(), sidecar.ConformanceOptions{
		Config: sidecar.Config{
			Command:     []string{binary},
			Environment: []string{"ECHO_SIDECAR_VERSION=99"},
		},
		SpeechSeconds: 0.1, TurnTimeout: 5 * time.Second,
	})
	if report.Passed {
		t.Fatal("a sidecar speaking another version must not pass")
	}
	if !strings.Contains(strings.Join(report.Failures, " "), "version") {
		t.Fatalf("the failure must say what is wrong: %v", report.Failures)
	}
}

func TestConformanceFailsWhenNoTurnIsProduced(t *testing.T) {
	binary := writeEchoSidecar(t)
	report := sidecar.RunConformance(context.Background(), sidecar.ConformanceOptions{
		Config: sidecar.Config{
			Command:     []string{binary},
			Environment: []string{"ECHO_SIDECAR_SILENT=1"},
		},
		SpeechSeconds: 0.1, TurnTimeout: 2 * time.Second,
	})
	if report.Passed {
		t.Fatal("a sidecar that never answers must not pass")
	}
}

func TestDialRequiresSomethingToConnectTo(t *testing.T) {
	if _, err := sidecar.Dial(context.Background(), sidecar.Config{}, sidecar.Message{
		SampleRate: 24000,
	}); err == nil {
		t.Fatal("a sidecar with no command and no address cannot be reached")
	}
}

const echoSidecarSource = `package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strconv"
)

type message struct {
	Type         string   ` + "`json:\"type\"`" + `
	PayloadBytes int      ` + "`json:\"payload_bytes,omitempty\"`" + `
	Version      int      ` + "`json:\"version,omitempty\"`" + `
	Model        string   ` + "`json:\"model,omitempty\"`" + `
	SampleRate   int      ` + "`json:\"sample_rate,omitempty\"`" + `
	OutputRate   int      ` + "`json:\"output_rate,omitempty\"`" + `
	Capabilities []string ` + "`json:\"capabilities,omitempty\"`" + `
	Text         string   ` + "`json:\"text,omitempty\"`" + `
	Final        bool     ` + "`json:\"final,omitempty\"`" + `
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()
	version := 1
	if value := os.Getenv("ECHO_SIDECAR_VERSION"); value != "" {
		version, _ = strconv.Atoi(value)
	}
	silent := os.Getenv("ECHO_SIDECAR_SILENT") != ""

	send := func(header message, payload []byte) {
		header.PayloadBytes = len(payload)
		encoded, _ := json.Marshal(header)
		writer.Write(append(encoded, '\n'))
		if len(payload) > 0 {
			writer.Write(payload)
		}
		writer.Flush()
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var incoming message
		if json.Unmarshal(line, &incoming) != nil {
			continue
		}
		if incoming.PayloadBytes > 0 {
			if _, err := io.ReadFull(reader, make([]byte, incoming.PayloadBytes)); err != nil {
				return
			}
		}
		switch incoming.Type {
		case "hello":
			send(message{
				Type: "ready", Version: version, Model: "echo", OutputRate: 24000,
				Capabilities: []string{"transcript", "text_injection", "interaction_acts"},
			}, nil)
		case "respond":
			if silent {
				continue
			}
			send(message{Type: "transcript", Text: "hello", Final: true}, nil)
			send(message{Type: "text_delta", Text: "Hello."}, nil)
			send(message{Type: "text_done", Text: "Hello."}, nil)
			send(message{Type: "output_audio"}, make([]byte, 4800))
			send(message{Type: "turn_done"}, nil)
		case "bye":
			return
		}
	}
}
`

// A client that took the floor declares where its turn ended, and that
// declaration reaches a model that owns its own floor. It carries nothing, and
// a sidecar that does not implement it ignores it rather than failing the
// session, because the engine has already ended the turn by the time it
// arrives.
func TestCommitIsCarriedAndCarriesNothing(t *testing.T) {
	message := sidecar.Message{Type: sidecar.TypeCommit}
	if err := message.Validate(); err != nil {
		t.Fatalf("a commit needs no fields: %v", err)
	}
	var buffer bytes.Buffer
	if err := sidecar.NewWriter(&buffer).Write(message); err != nil {
		t.Fatalf("write: %v", err)
	}
	decoded, err := sidecar.NewReader(&buffer).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if decoded.Type != sidecar.TypeCommit {
		t.Fatalf("unexpected type %q", decoded.Type)
	}
}
