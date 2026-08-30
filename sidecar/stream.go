package sidecar

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// maxHeaderBytes bounds one JSON header. A header larger than this is a
// malformed stream rather than a large message: payloads are what carry bulk.
const maxHeaderBytes = 1 << 20

// maxPayloadBytes bounds one binary payload. Keep the stream allocator and
// the strongest protocol-v4 format offer under one limit authority.
const maxPayloadBytes = MaxElementBinaryBytes

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
	if writer == nil || writer.output == nil {
		return errors.New("write sidecar frame: nil writer")
	}
	message.PayloadBytes = len(message.Payload)
	if message.PayloadBytes > maxPayloadBytes {
		return fmt.Errorf("sidecar payload exceeds the %d-byte frame limit", maxPayloadBytes)
	}
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
	// readHeader counts the mandatory newline as part of its bound. Refuse a
	// header that this package's own reader would reject after Write appends it.
	if len(header)+1 > maxHeaderBytes {
		return errors.New("sidecar header exceeds the frame limit")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	header = append(header, '\n')
	if written, err := writer.output.Write(header); err != nil {
		return err
	} else if written != len(header) {
		return io.ErrShortWrite
	}
	if message.PayloadBytes == 0 {
		return nil
	}
	written, err := writer.output.Write(message.Payload)
	if err != nil {
		return err
	}
	if written != len(message.Payload) {
		return io.ErrShortWrite
	}
	return nil
}

// Reader parses frames from a stream.
type Reader struct {
	input *bufio.Reader
}

// NewReader wraps a stream.
func NewReader(input io.Reader) *Reader {
	if input == nil {
		return &Reader{}
	}
	return &Reader{input: bufio.NewReaderSize(input, 64<<10)}
}

// Read returns the next frame, or io.EOF at the end of the stream.
func (reader *Reader) Read() (Message, error) {
	if reader == nil || reader.input == nil {
		return Message{}, errors.New("read sidecar frame: nil reader")
	}
	header, err := reader.readHeader()
	if err != nil {
		return Message{}, err
	}
	if err := strictjson.Validate(header); err != nil {
		return Message{}, fmt.Errorf("decode sidecar header: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	var message Message
	if err := decoder.Decode(&message); err != nil {
		return Message{}, fmt.Errorf("decode sidecar header: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Message{}, errors.New("decode sidecar header: trailing JSON value")
		}
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

func (reader *Reader) readHeader() ([]byte, error) {
	fragment, err := reader.input.ReadSlice('\n')
	if len(fragment) > maxHeaderBytes {
		return nil, errors.New("sidecar header exceeds the frame limit")
	}
	switch {
	case err == nil:
		// The caller decodes the header before the next buffered read, so the
		// ordinary one-buffer frame can be borrowed without a 64 KiB allocation
		// on every media packet.
		return fragment, nil
	case errors.Is(err, io.EOF) && len(fragment) == 0:
		return nil, io.EOF
	case errors.Is(err, io.EOF):
		return nil, errors.New("sidecar header is not newline-terminated")
	case !errors.Is(err, bufio.ErrBufferFull):
		return nil, err
	}
	header := append(make([]byte, 0, min(maxHeaderBytes, len(fragment)*2)), fragment...)
	for {
		fragment, err = reader.input.ReadSlice('\n')
		if len(header)+len(fragment) > maxHeaderBytes {
			return nil, errors.New("sidecar header exceeds the frame limit")
		}
		header = append(header, fragment...)
		switch {
		case err == nil:
			return header, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(header) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, errors.New("sidecar header is not newline-terminated")
		default:
			return nil, err
		}
	}
}
