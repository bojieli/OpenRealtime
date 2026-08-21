package sidecar

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// maxHeaderBytes bounds one JSON header. A header larger than this is a
// malformed stream rather than a large message: payloads are what carry bulk.
const maxHeaderBytes = 1 << 20

// maxPayloadBytes bounds one binary payload.
const maxPayloadBytes = 16 << 20

// Writer serialises frames onto a stream.
type Writer struct {
	mu     sync.Mutex
	output io.Writer
}

// NewWriter wraps a stream.
func NewWriter(output io.Writer) *Writer { return &Writer{output: output} }

// Write validates and emits one frame. It is safe for concurrent use: audio
// and control messages come from different goroutines, and a frame interleaved
// with another frame's payload is an unrecoverable stream.
func (writer *Writer) Write(message Message) error {
	message.PayloadBytes = len(message.Payload)
	if err := message.Validate(); err != nil {
		return err
	}
	if message.PayloadBytes > maxPayloadBytes {
		return fmt.Errorf("payload of %d bytes exceeds the frame limit", message.PayloadBytes)
	}
	header, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(header) > maxHeaderBytes {
		return errors.New("sidecar header exceeds the frame limit")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if _, err := writer.output.Write(append(header, '\n')); err != nil {
		return err
	}
	if message.PayloadBytes == 0 {
		return nil
	}
	_, err = writer.output.Write(message.Payload)
	return err
}

// Reader parses frames from a stream.
type Reader struct {
	input *bufio.Reader
}

// NewReader wraps a stream.
func NewReader(input io.Reader) *Reader {
	return &Reader{input: bufio.NewReaderSize(input, 64<<10)}
}

// Read returns the next frame, or io.EOF at the end of the stream.
func (reader *Reader) Read() (Message, error) {
	header, err := reader.input.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(header) == 0 {
			return Message{}, io.EOF
		}
		if !errors.Is(err, io.EOF) {
			return Message{}, err
		}
	}
	if len(header) > maxHeaderBytes {
		return Message{}, errors.New("sidecar header exceeds the frame limit")
	}
	var message Message
	if err := json.Unmarshal(header, &message); err != nil {
		return Message{}, fmt.Errorf("decode sidecar header: %w", err)
	}
	if message.PayloadBytes < 0 || message.PayloadBytes > maxPayloadBytes {
		return Message{}, fmt.Errorf("payload length %d is out of range", message.PayloadBytes)
	}
	if message.PayloadBytes > 0 {
		message.Payload = make([]byte, message.PayloadBytes)
		if _, err := io.ReadFull(reader.input, message.Payload); err != nil {
			return Message{}, fmt.Errorf("read sidecar payload: %w", err)
		}
	}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}
