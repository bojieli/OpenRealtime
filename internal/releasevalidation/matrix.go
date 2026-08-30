// Package releasevalidation loads and validates the repository's fail-closed
// release matrix. It deliberately knows nothing about OpenRealtime runtime
// internals: a release gate is an executable contract around the public build,
// test, client, and benchmark entry points.
package releasevalidation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const MatrixVersion = 1

type Availability string

const (
	AvailabilityLocal       Availability = "local"
	AvailabilityProvisioned Availability = "provisioned"
)

type Selection string

const (
	SelectionDefault Selection = "default"
	SelectionOptIn   Selection = "opt_in"
)

type SkipPolicy string

const (
	SkipForbid SkipPolicy = "forbid"
	SkipRecord SkipPolicy = "record"
)

// Matrix is the checked, versioned release definition.
type Matrix struct {
	Version int    `json:"version"`
	Gates   []Gate `json:"gates"`
}

// Gate is one independently runnable release claim. Required means that the
// complete matrix cannot be green without a passing result for this gate.
// Selection controls only the convenient local default; it never changes the
// completeness calculation.
type Gate struct {
	ID            string            `json:"id"`
	Description   string            `json:"description"`
	Availability  Availability      `json:"availability"`
	Selection     Selection         `json:"selection"`
	Required      bool              `json:"required"`
	WorkingDir    string            `json:"working_directory"`
	Command       []string          `json:"command"`
	Environment   map[string]string `json:"environment,omitempty"`
	Timeout       string            `json:"timeout"`
	SkipPolicy    SkipPolicy        `json:"skip_policy"`
	Prerequisites []Prerequisite    `json:"prerequisites,omitempty"`
	Assertions    []Assertion       `json:"assertions,omitempty"`
	Covers        []string          `json:"covers,omitempty"`
}

type Prerequisite struct {
	Kind         string   `json:"kind"`
	Value        string   `json:"value,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"`
	Description  string   `json:"description"`
}

type Assertion struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

var (
	gateIDPattern   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	envNamePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	placeholderExpr = regexp.MustCompile(`\{(?:root|go|gofmt|artifacts|env:[A-Z][A-Z0-9_]*)\}`)
)

// Load reads a strict matrix. Unknown fields and trailing JSON are errors so
// a misspelled prerequisite or policy cannot silently weaken a gate.
func Load(path string) (Matrix, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return Matrix{}, fmt.Errorf("read release matrix: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var matrix Matrix
	if err := decoder.Decode(&matrix); err != nil {
		return Matrix{}, fmt.Errorf("decode release matrix: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Matrix{}, err
	}
	if err := matrix.Validate(); err != nil {
		return Matrix{}, err
	}
	return matrix, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("release matrix contains trailing JSON")
		}
		return fmt.Errorf("decode trailing release matrix content: %w", err)
	}
	return nil
}

func (matrix Matrix) Validate() error {
	if matrix.Version != MatrixVersion {
		return fmt.Errorf("release matrix version must be %d", MatrixVersion)
	}
	if len(matrix.Gates) == 0 {
		return errors.New("release matrix has no gates")
	}
	seenIDs := make(map[string]struct{}, len(matrix.Gates))
	seenCoverage := make(map[string]string)
	previousID := ""
	for index, gate := range matrix.Gates {
		if err := gate.validate(); err != nil {
			return fmt.Errorf("gate %d: %w", index, err)
		}
		if _, exists := seenIDs[gate.ID]; exists {
			return fmt.Errorf("duplicate gate ID %q", gate.ID)
		}
		seenIDs[gate.ID] = struct{}{}
		if previousID != "" && previousID >= gate.ID {
			return errors.New("release gates must be sorted by ID")
		}
		previousID = gate.ID
		for _, coverage := range gate.Covers {
			if owner, exists := seenCoverage[coverage]; exists {
				return fmt.Errorf("coverage %q is claimed by both %q and %q", coverage, owner, gate.ID)
			}
			seenCoverage[coverage] = gate.ID
		}
	}
	return nil
}

// Digest binds a report to the exact semantic matrix. encoding/json sorts map
// keys, so equivalent checked JSON has one structural identity independent of
// whitespace.
func (matrix Matrix) Digest() (string, error) {
	if err := matrix.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(matrix)
	if err != nil {
		return "", fmt.Errorf("marshal release matrix identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest[:]), nil
}

func (gate Gate) validate() error {
	if !gateIDPattern.MatchString(gate.ID) {
		return fmt.Errorf("invalid gate ID %q", gate.ID)
	}
	if strings.TrimSpace(gate.Description) == "" {
		return fmt.Errorf("gate %q has no description", gate.ID)
	}
	if gate.Availability != AvailabilityLocal && gate.Availability != AvailabilityProvisioned {
		return fmt.Errorf("gate %q has invalid availability %q", gate.ID, gate.Availability)
	}
	if gate.Selection != SelectionDefault && gate.Selection != SelectionOptIn {
		return fmt.Errorf("gate %q has invalid selection %q", gate.ID, gate.Selection)
	}
	if gate.Availability == AvailabilityProvisioned && gate.Selection != SelectionOptIn {
		return fmt.Errorf("provisioned gate %q must be opt-in", gate.ID)
	}
	if !gate.Required {
		return fmt.Errorf("gate %q is not required; diagnostics do not belong in the release matrix", gate.ID)
	}
	if gate.WorkingDir == "" {
		return fmt.Errorf("gate %q has no working directory", gate.ID)
	}
	cleanDirectory := filepath.Clean(gate.WorkingDir)
	if filepath.IsAbs(gate.WorkingDir) || cleanDirectory == ".." || strings.HasPrefix(cleanDirectory, ".."+string(filepath.Separator)) {
		return fmt.Errorf("gate %q working directory must stay inside the repository", gate.ID)
	}
	if len(gate.Command) == 0 {
		return fmt.Errorf("gate %q has no command", gate.ID)
	}
	for _, argument := range gate.Command {
		if argument == "" {
			return fmt.Errorf("gate %q contains an empty command argument", gate.ID)
		}
		if err := validatePlaceholders(argument); err != nil {
			return fmt.Errorf("gate %q command: %w", gate.ID, err)
		}
	}
	for name, value := range gate.Environment {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("gate %q has invalid environment name %q", gate.ID, name)
		}
		if err := validatePlaceholders(value); err != nil {
			return fmt.Errorf("gate %q environment %s: %w", gate.ID, name, err)
		}
	}
	duration, err := time.ParseDuration(gate.Timeout)
	if err != nil || duration <= 0 {
		return fmt.Errorf("gate %q has invalid positive timeout %q", gate.ID, gate.Timeout)
	}
	if gate.SkipPolicy != SkipForbid && gate.SkipPolicy != SkipRecord {
		return fmt.Errorf("gate %q has invalid skip policy %q", gate.ID, gate.SkipPolicy)
	}
	for index, prerequisite := range gate.Prerequisites {
		if err := prerequisite.validate(); err != nil {
			return fmt.Errorf("gate %q prerequisite %d: %w", gate.ID, index, err)
		}
	}
	declaredEnvironment := make(map[string]bool)
	for _, prerequisite := range gate.Prerequisites {
		if strings.HasPrefix(prerequisite.Kind, "env") {
			declaredEnvironment[prerequisite.Value] = true
		}
	}
	for _, template := range append(slices.Clone(gate.Command), mapValues(gate.Environment)...) {
		for _, token := range placeholderExpr.FindAllString(template, -1) {
			if !strings.HasPrefix(token, "{env:") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(token, "{env:"), "}")
			if !declaredEnvironment[name] {
				return fmt.Errorf("gate %q uses %s without an explicit prerequisite", gate.ID, token)
			}
		}
	}
	for index, assertion := range gate.Assertions {
		if err := assertion.validate(); err != nil {
			return fmt.Errorf("gate %q assertion %d: %w", gate.ID, index, err)
		}
	}
	if !slices.IsSorted(gate.Covers) {
		return fmt.Errorf("gate %q coverage must be sorted", gate.ID)
	}
	for index, coverage := range gate.Covers {
		if strings.TrimSpace(coverage) == "" {
			return fmt.Errorf("gate %q has empty coverage", gate.ID)
		}
		if index > 0 && gate.Covers[index-1] == coverage {
			return fmt.Errorf("gate %q has duplicate coverage %q", gate.ID, coverage)
		}
	}
	return nil
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func validatePlaceholders(value string) error {
	withoutKnown := placeholderExpr.ReplaceAllString(value, "")
	if strings.Contains(withoutKnown, "{") || strings.Contains(withoutKnown, "}") {
		return fmt.Errorf("unknown or malformed placeholder in %q", value)
	}
	return nil
}

func (prerequisite Prerequisite) validate() error {
	if strings.TrimSpace(prerequisite.Description) == "" {
		return errors.New("prerequisite has no description")
	}
	allowed := map[string]bool{
		"command": true, "any_command": true, "any_env": true, "browser": true, "docker_image": true,
		"env": true, "env_url": true, "env_file": true,
		"env_directory": true, "env_executable": true,
		"file": true, "directory": true, "executable": true, "os": true,
		"python_module": true,
	}
	if !allowed[prerequisite.Kind] {
		return fmt.Errorf("unknown prerequisite kind %q", prerequisite.Kind)
	}
	if prerequisite.Kind == "any_command" || prerequisite.Kind == "any_env" {
		if prerequisite.Value != "" || len(prerequisite.Alternatives) == 0 {
			return fmt.Errorf("%s requires alternatives and no value", prerequisite.Kind)
		}
		for _, value := range prerequisite.Alternatives {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s contains an empty alternative", prerequisite.Kind)
			}
			if prerequisite.Kind == "any_env" && !envNamePattern.MatchString(value) {
				return fmt.Errorf("any_env contains invalid environment name %q", value)
			}
		}
		return nil
	}
	if prerequisite.Kind == "browser" {
		if prerequisite.Value != "" || len(prerequisite.Alternatives) != 0 {
			return errors.New("browser takes no value or alternatives")
		}
		return nil
	}
	if strings.TrimSpace(prerequisite.Value) == "" || len(prerequisite.Alternatives) != 0 {
		return fmt.Errorf("%s requires one value and no alternatives", prerequisite.Kind)
	}
	if strings.HasPrefix(prerequisite.Kind, "env") && !envNamePattern.MatchString(prerequisite.Value) {
		return fmt.Errorf("invalid environment name %q", prerequisite.Value)
	}
	if !strings.HasPrefix(prerequisite.Kind, "env") {
		return validatePlaceholders(prerequisite.Value)
	}
	return nil
}

func (assertion Assertion) validate() error {
	switch assertion.Kind {
	case "stdout_regex", "stderr_regex":
		if _, err := regexp.Compile(assertion.Value); err != nil {
			return fmt.Errorf("invalid %s: %w", assertion.Kind, err)
		}
	case "file_nonempty":
		if err := validatePlaceholders(assertion.Value); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown assertion kind %q", assertion.Kind)
	}
	return nil
}
