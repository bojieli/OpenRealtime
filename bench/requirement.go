package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// MarshalExecutionRequirement returns one deterministic, newline-terminated
// execution-requirement artifact. The zero value is reserved for local
// diagnostics and is not a useful standalone artifact: an explicitly supplied
// file must select graph-native execution.
func MarshalExecutionRequirement(requirement ExecutionRequirement) ([]byte, error) {
	if !requirement.Required() {
		return nil, errors.New("encode execution requirement: an artifact must select graph-native execution")
	}
	if err := requirement.Validate(); err != nil {
		return nil, fmt.Errorf("encode execution requirement: %w", err)
	}
	canonical := requirement.canonicalized()
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode execution requirement: %w", err)
	}
	return append(payload, '\n'), nil
}

// ParseExecutionRequirement strictly decodes one standalone execution
// requirement. It rejects unknown or duplicate fields, trailing values, and
// the unattested diagnostic zero value.
func ParseExecutionRequirement(source []byte) (ExecutionRequirement, error) {
	if err := strictjson.Validate(source); err != nil {
		return ExecutionRequirement{}, fmt.Errorf("decode execution requirement: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var requirement ExecutionRequirement
	if err := decoder.Decode(&requirement); err != nil {
		return ExecutionRequirement{}, fmt.Errorf("decode execution requirement: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ExecutionRequirement{}, errors.New("decode execution requirement: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return ExecutionRequirement{}, fmt.Errorf("decode execution requirement trailing data: %w", err)
	}
	if !requirement.Required() {
		return ExecutionRequirement{}, errors.New(
			"decode execution requirement: an artifact must select graph-native execution",
		)
	}
	if err := requirement.Validate(); err != nil {
		return ExecutionRequirement{}, fmt.Errorf("decode execution requirement: %w", err)
	}
	return requirement.canonicalized(), nil
}

// ReadExecutionRequirement reads and verifies one reviewable requirement
// artifact. It contains expected identities only; observed task evidence uses
// ParseExecutionEvidence and remains a separate artifact kind.
func ReadExecutionRequirement(path string) (ExecutionRequirement, error) {
	if strings.TrimSpace(path) == "" {
		return ExecutionRequirement{}, errors.New("an execution requirement needs an input path")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return ExecutionRequirement{}, fmt.Errorf("read execution requirement: %w", err)
	}
	return ParseExecutionRequirement(payload)
}

// WriteExecutionRequirement writes one validated standalone requirement.
func WriteExecutionRequirement(path string, requirement ExecutionRequirement) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("an execution requirement needs an output path")
	}
	payload, err := MarshalExecutionRequirement(requirement)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create execution requirement directory: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write execution requirement: %w", err)
	}
	return nil
}
