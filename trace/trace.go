// Package trace records exact OpenAI Realtime messages with monotonic research timing.
package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

const SchemaVersion = "0.2.0"

type Record struct {
	SchemaVersion   string               `json:"schema_version"`
	TraceID         string               `json:"trace_id"`
	SessionID       string               `json:"session_id"`
	Sequence        uint64               `json:"sequence"`
	MonotonicNS     uint64               `json:"monotonic_ns"`
	Direction       openaiwire.Direction `json:"direction"`
	Profile         openaiwire.Profile   `json:"profile"`
	CausalParentIDs []string             `json:"causal_parent_ids"`
	Message         json.RawMessage      `json:"message"`
}

type InvariantError struct {
	Message string
}

func (err *InvariantError) Error() string { return err.Message }

type State struct {
	sessionID       string
	nextSequence    uint64
	lastMonotonicNS uint64
	hasEvent        bool
	traceIDs        map[string]struct{}
}

func NewState() *State {
	return &State{traceIDs: make(map[string]struct{})}
}

func (state *State) Accept(record Record) error {
	if record.SchemaVersion != SchemaVersion {
		return &InvariantError{fmt.Sprintf("unsupported trace schema %q", record.SchemaVersion)}
	}
	if record.TraceID == "" || record.SessionID == "" {
		return &InvariantError{"trace_id and session_id must not be empty"}
	}
	if state.sessionID == "" {
		state.sessionID = record.SessionID
	} else if record.SessionID != state.sessionID {
		return &InvariantError{"one trace stream must contain exactly one session"}
	}
	if record.Sequence != state.nextSequence {
		return &InvariantError{fmt.Sprintf("expected sequence %d, got %d", state.nextSequence, record.Sequence)}
	}
	if state.hasEvent && record.MonotonicNS < state.lastMonotonicNS {
		return &InvariantError{"monotonic_ns moved backwards"}
	}
	if _, exists := state.traceIDs[record.TraceID]; exists {
		return &InvariantError{fmt.Sprintf("duplicate trace_id %q", record.TraceID)}
	}
	parents := slices.Clone(record.CausalParentIDs)
	slices.Sort(parents)
	if adjacentDuplicate(parents) {
		return &InvariantError{"causal_parent_ids must be unique"}
	}
	for _, parent := range parents {
		if _, exists := state.traceIDs[parent]; !exists {
			return &InvariantError{fmt.Sprintf("causal parent %q must precede its child", parent)}
		}
	}
	if _, err := openaiwire.Decode(record.Message); err != nil {
		return &InvariantError{fmt.Sprintf("invalid embedded OpenAI event: %v", err)}
	}
	state.traceIDs[record.TraceID] = struct{}{}
	state.nextSequence++
	state.lastMonotonicNS = record.MonotonicNS
	state.hasEvent = true
	return nil
}

func adjacentDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

type Writer struct {
	output *bufio.Writer
	state  *State
}

func NewWriter(output io.Writer) *Writer {
	return &Writer{output: bufio.NewWriter(output), state: NewState()}
}

func (writer *Writer) Write(record Record) error {
	if err := writer.state.Accept(record); err != nil {
		return err
	}
	// causal_parent_ids is required and typed as an array by the published
	// schema, and a nil slice marshals to null. A root record names no
	// parents, so without this every trace would open with a record no
	// third-party validator accepts.
	if record.CausalParentIDs == nil {
		record.CausalParentIDs = []string{}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode trace record: %w", err)
	}
	if _, err := writer.output.Write(data); err != nil {
		return fmt.Errorf("write trace record: %w", err)
	}
	if err := writer.output.WriteByte('\n'); err != nil {
		return fmt.Errorf("write trace newline: %w", err)
	}
	return nil
}

func (writer *Writer) Flush() error {
	return writer.output.Flush()
}

func Read(input io.Reader, visit func(Record) error) (uint64, error) {
	state := NewState()
	reader := bufio.NewReader(input)
	var count uint64
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var record Record
			decoder := json.NewDecoder(bytes.NewReader(line))
			if decodeErr := decoder.Decode(&record); decodeErr != nil {
				return count, fmt.Errorf("trace line %d: %w", count+1, decodeErr)
			}
			if stateErr := state.Accept(record); stateErr != nil {
				return count, fmt.Errorf("trace line %d: %w", count+1, stateErr)
			}
			if visit != nil {
				if visitErr := visit(record); visitErr != nil {
					return count, fmt.Errorf("trace line %d: %w", count+1, visitErr)
				}
			}
			count++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, fmt.Errorf("read trace: %w", err)
		}
	}
	if count == 0 {
		return 0, errors.New("trace must contain at least one record")
	}
	return count, nil
}
