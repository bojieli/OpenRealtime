// Package sse contains the small, transport-only portion of Server-Sent Event
// parsing shared by HTTP streaming model adapters.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const defaultMaxEventBytes = 16 << 20

// Read calls onData once for each complete SSE event containing at least one
// data field. Multiple data fields are joined with a newline as required by the
// SSE format. Event names, IDs, retry hints, and comments are intentionally
// ignored because model APIs communicate their payload entirely through data.
//
// maxEventBytes bounds a single scanned line and protects adapters from an
// unbounded response. A non-positive value selects a 16 MiB default.
func Read(reader io.Reader, maxEventBytes int, onData func([]byte) error) error {
	if reader == nil {
		return errors.New("SSE reader is required")
	}
	if onData == nil {
		return errors.New("SSE data callback is required")
	}
	if maxEventBytes <= 0 {
		maxEventBytes = defaultMaxEventBytes
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxEventBytes)
	var data bytes.Buffer
	hasData := false
	flush := func() error {
		if !hasData {
			return nil
		}
		payload := bytes.Clone(bytes.TrimSuffix(data.Bytes(), []byte("\n")))
		data.Reset()
		hasData = false
		return onData(payload)
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if bytes.Equal(line, []byte("data")) {
			hasData = true
			data.WriteByte('\n')
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			hasData = true
			data.Write(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	return flush()
}
