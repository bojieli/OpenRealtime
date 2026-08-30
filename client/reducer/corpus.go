package reducer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

type Corpus struct {
	FormatVersion int      `json:"format_version"`
	Limits        Limits   `json:"limits"`
	Vectors       []Vector `json:"vectors"`
}

type Vector struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Steps       []Step      `json:"steps"`
	Expect      Expectation `json:"expect"`
}

type Step struct {
	AtMS      int64     `json:"at_ms"`
	Operation Operation `json:"operation"`
}

type Expectation struct {
	Snapshot Snapshot          `json:"snapshot"`
	Outbound []json.RawMessage `json:"outbound"`
	Errors   []string          `json:"errors"`
}

type Result struct {
	Snapshot Snapshot
	Outbound []json.RawMessage
	Errors   []string
}

// LoadCorpus performs a bounded, duplicate-key-safe, unknown-field-safe parse.
// The same checks are reproduced in the JavaScript and Swift runners.
func LoadCorpus(reader io.Reader) (Corpus, error) {
	if reader == nil {
		return Corpus{}, errors.New("corpus reader is nil")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, MaxCorpusBytes+1))
	if err != nil {
		return Corpus{}, fmt.Errorf("read reducer corpus: %w", err)
	}
	if len(payload) == 0 {
		return Corpus{}, errors.New("reducer corpus is empty")
	}
	if len(payload) > MaxCorpusBytes {
		return Corpus{}, fmt.Errorf("reducer corpus exceeds %d bytes", MaxCorpusBytes)
	}
	if err := strictjson.Validate(payload); err != nil {
		return Corpus{}, fmt.Errorf("validate reducer corpus: %w", err)
	}
	var corpus Corpus
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpus); err != nil {
		return Corpus{}, fmt.Errorf("decode reducer corpus: %w", err)
	}
	if corpus.FormatVersion != FormatVersion {
		return Corpus{}, fmt.Errorf("unsupported reducer corpus format_version %d", corpus.FormatVersion)
	}
	if !reflect.DeepEqual(corpus.Limits, DefaultLimits()) {
		return Corpus{}, fmt.Errorf("reducer corpus limits do not match format %d", FormatVersion)
	}
	if err := validateCorpus(corpus); err != nil {
		return Corpus{}, err
	}
	return corpus, nil
}

func validateCorpus(corpus Corpus) error {
	if len(corpus.Vectors) == 0 {
		return errors.New("reducer corpus has no vectors")
	}
	if len(corpus.Vectors) > corpus.Limits.MaxVectors {
		return fmt.Errorf("reducer corpus has %d vectors, limit is %d", len(corpus.Vectors), corpus.Limits.MaxVectors)
	}
	names := map[string]struct{}{}
	shapeValidator, err := New(corpus.Limits)
	if err != nil {
		return err
	}
	for vectorIndex, vector := range corpus.Vectors {
		if err := boundedString("vector name", vector.Name, corpus.Limits.MaxStringBytes, true); err != nil {
			return fmt.Errorf("vector %d: %w", vectorIndex, err)
		}
		if _, duplicate := names[vector.Name]; duplicate {
			return fmt.Errorf("duplicate reducer vector name %q", vector.Name)
		}
		names[vector.Name] = struct{}{}
		if err := boundedString("vector description", vector.Description, corpus.Limits.MaxStringBytes, true); err != nil {
			return fmt.Errorf("vector %q: %w", vector.Name, err)
		}
		if len(vector.Steps) == 0 {
			return fmt.Errorf("vector %q has no steps", vector.Name)
		}
		if len(vector.Steps) > corpus.Limits.MaxStepsPerVector {
			return fmt.Errorf("vector %q has %d steps, limit is %d", vector.Name, len(vector.Steps), corpus.Limits.MaxStepsPerVector)
		}
		previous := int64(0)
		for stepIndex, step := range vector.Steps {
			if step.AtMS < previous {
				return fmt.Errorf("vector %q step %d moves virtual time backwards", vector.Name, stepIndex)
			}
			if step.AtMS > corpus.Limits.MaxVirtualTimeMS {
				return fmt.Errorf("vector %q step %d exceeds virtual time limit", vector.Name, stepIndex)
			}
			previous = step.AtMS
			if err := shapeValidator.validateOperation(step.Operation); err != nil {
				return fmt.Errorf("vector %q step %d: %w", vector.Name, stepIndex, err)
			}
		}
		if len(vector.Expect.Outbound) > corpus.Limits.MaxOutboundEvents {
			return fmt.Errorf("vector %q expected outbound events exceed limit", vector.Name)
		}
		for outboundIndex, raw := range vector.Expect.Outbound {
			if _, err := strictObject(raw, corpus.Limits.MaxEventBytes, "expected outbound event"); err != nil {
				return fmt.Errorf("vector %q outbound %d: %w", vector.Name, outboundIndex, err)
			}
		}
		if len(vector.Expect.Errors) > corpus.Limits.MaxErrorsPerVector {
			return fmt.Errorf("vector %q expected errors exceed limit", vector.Name)
		}
		for errorIndex, message := range vector.Expect.Errors {
			if err := boundedString("expected error", message, corpus.Limits.MaxStringBytes, true); err != nil {
				return fmt.Errorf("vector %q error %d: %w", vector.Name, errorIndex, err)
			}
		}
		if err := validateExpectedSnapshot(vector.Name, vector.Expect.Snapshot, corpus.Limits); err != nil {
			return err
		}
	}
	return nil
}

func validateExpectedSnapshot(name string, snapshot Snapshot, limits Limits) error {
	if len(snapshot.Conversation) > limits.MaxConversationItems || len(snapshot.Tools) > limits.MaxToolCalls ||
		len(snapshot.ProtocolLog) > limits.MaxProtocolLogEntries {
		return fmt.Errorf("vector %q expected snapshot exceeds collection limits", name)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("vector %q encode expected snapshot: %w", name, err)
	}
	if len(encoded) > MaxCorpusBytes {
		return fmt.Errorf("vector %q expected snapshot is unbounded", name)
	}
	return nil
}

func RunVector(vector Vector, limits Limits) (Result, error) {
	reducer, err := New(limits)
	if err != nil {
		return Result{}, err
	}
	errorsSeen := []string{}
	for index, step := range vector.Steps {
		if err := reducer.Apply(step.AtMS, step.Operation); err != nil {
			if len(errorsSeen) >= limits.MaxErrorsPerVector {
				return Result{}, fmt.Errorf("vector %q exceeded error limit", vector.Name)
			}
			errorsSeen = append(errorsSeen, fmt.Sprintf("step %d: %s", index, err))
		}
	}
	return Result{
		Snapshot: reducer.Snapshot(), Outbound: reducer.Outbound(), Errors: errorsSeen,
	}, nil
}

func CompareResult(expect Expectation, actual Result) error {
	if !reflect.DeepEqual(expect.Snapshot, actual.Snapshot) {
		expected, _ := json.MarshalIndent(expect.Snapshot, "", "  ")
		observed, _ := json.MarshalIndent(actual.Snapshot, "", "  ")
		return fmt.Errorf("snapshot mismatch\nexpected: %s\nactual:   %s", expected, observed)
	}
	if len(expect.Outbound) != len(actual.Outbound) {
		return fmt.Errorf("outbound count mismatch: expected %d, actual %d", len(expect.Outbound), len(actual.Outbound))
	}
	for index := range expect.Outbound {
		if !jsonEqual(expect.Outbound[index], actual.Outbound[index]) {
			return fmt.Errorf("outbound %d mismatch: expected %s, actual %s", index, expect.Outbound[index], actual.Outbound[index])
		}
	}
	if !reflect.DeepEqual(expect.Errors, actual.Errors) {
		return fmt.Errorf("errors mismatch: expected %q, actual %q", expect.Errors, actual.Errors)
	}
	return nil
}

func jsonEqual(left, right []byte) bool {
	var leftValue any
	var rightValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	rightDecoder.UseNumber()
	if leftDecoder.Decode(&leftValue) != nil || rightDecoder.Decode(&rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

// ValidateCorpusJSON is exposed for non-file adapters and fuzz tests. It is
// intentionally just as strict as LoadCorpus.
func ValidateCorpusJSON(payload []byte) error {
	_, err := LoadCorpus(strings.NewReader(string(payload)))
	return err
}
