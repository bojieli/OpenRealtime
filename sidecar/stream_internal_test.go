package sidecar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestWriterAndReaderShareTheExactHeaderBound(t *testing.T) {
	probe, err := json.Marshal(Message{Type: TypeLog, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	fixedBytes := len(probe) - 1
	acceptedTextBytes := maxHeaderBytes - 1 - fixedBytes
	accepted := Message{Type: TypeLog, Text: strings.Repeat("x", acceptedTextBytes)}
	var stream bytes.Buffer
	if err := NewWriter(&stream).Write(accepted); err != nil {
		t.Fatalf("write header at the shared bound: %v", err)
	}
	if stream.Len() != maxHeaderBytes {
		t.Fatalf("bounded wire header is %d bytes, want %d", stream.Len(), maxHeaderBytes)
	}
	decoded, err := NewReader(&stream).Read()
	if err != nil {
		t.Fatalf("read header emitted at the shared bound: %v", err)
	}
	if decoded.Text != accepted.Text {
		t.Fatal("bounded header changed across framing")
	}

	rejected := Message{Type: TypeLog, Text: strings.Repeat("x", acceptedTextBytes+1)}
	if err := NewWriter(&bytes.Buffer{}).Write(rejected); err == nil ||
		!strings.Contains(err.Error(), "header exceeds") {
		t.Fatalf("header whose newline exceeds the bound error = %v", err)
	}
}

func TestReaderRejectsOversizedDeclaredPayloadBeforeReadingABody(t *testing.T) {
	header := fmt.Sprintf(`{"type":"audio","payload_bytes":%d}`+"\n", maxPayloadBytes+1)
	if _, err := NewReader(strings.NewReader(header)).Read(); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Fatalf("oversized declared payload error = %v", err)
	}
}

func TestWriterRejectsPayloadItsReaderCouldNotAccept(t *testing.T) {
	var output bytes.Buffer
	err := NewWriter(&output).Write(Message{
		Type: TypeAudio, Payload: make([]byte, MaxElementBinaryBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("oversized writer payload error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("writer emitted %d bytes before rejecting oversized payload", output.Len())
	}
}
