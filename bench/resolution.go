package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// ExpectedResolutionFormatVersion is the standalone authoring schema for an
// expected live graph resolution. It is intentionally independent of Graph IR
// and result schema versions.
const ExpectedResolutionFormatVersion uint64 = 1

type expectedResolutionDocument struct {
	FormatVersion uint64              `json:"format_version"`
	Elements      []ElementResolution `json:"elements"`
	Paths         []SelectedPath      `json:"required_paths,omitempty"`
}

// ValidateExpectedResolution verifies the deployment identities and path
// declarations that can be checked without the target Graph IR. RequireGraph
// performs the graph-relative node, implementation, and edge checks later.
func ValidateExpectedResolution(resolution LiveResolution) error {
	if len(resolution.Elements) == 0 {
		return errors.New("expected resolution has no elements")
	}
	elements := make(map[string]struct{}, len(resolution.Elements))
	for _, item := range resolution.Elements {
		if !canonical(item.Node) {
			return fmt.Errorf("expected resolution has invalid node %q", item.Node)
		}
		if _, duplicate := elements[item.Node]; duplicate {
			return fmt.Errorf("expected resolution repeats node %q", item.Node)
		}
		elements[item.Node] = struct{}{}
		if err := element.ValidateIdentity(item.Element); err != nil {
			return fmt.Errorf("expected resolution node %s element: %w", item.Node, err)
		}
		if !canonical(item.Implementation) {
			return fmt.Errorf("expected resolution node %s requires an implementation identity", item.Node)
		}
		if livePlaceholder(item.Implementation) {
			return fmt.Errorf("expected resolution node %s implementation %q is a live placeholder",
				item.Node, item.Implementation)
		}
		if err := item.Runtime.validateExact("expected resolution node " + item.Node + " runtime"); err != nil {
			return err
		}
		capabilities := make(map[string]struct{}, len(item.Capabilities))
		for _, capability := range item.Capabilities {
			if !canonical(capability.Name) ||
				(capability.Contract != "" && !canonical(capability.Contract)) {
				return fmt.Errorf("expected resolution node %s has an invalid capability identity", item.Node)
			}
			key := capability.Name + "\x00" + capability.Contract + "\x00" + capability.Provider.ID
			if _, duplicate := capabilities[key]; duplicate {
				return fmt.Errorf("expected resolution node %s repeats capability %q", item.Node, capability.Name)
			}
			capabilities[key] = struct{}{}
			if err := capability.Provider.validateExact(
				"expected resolution node " + item.Node + " capability " + capability.Name + " provider",
			); err != nil {
				return err
			}
			if capability.Adapter != nil {
				if err := capability.Adapter.validateExact(
					"expected resolution node " + item.Node + " capability " + capability.Name + " adapter",
				); err != nil {
					return err
				}
			}
		}
	}

	paths := make(map[string]struct{}, len(resolution.Paths))
	for _, path := range resolution.Paths {
		if !canonical(path.Name) {
			return fmt.Errorf("expected resolution has invalid required path %q", path.Name)
		}
		if _, duplicate := paths[path.Name]; duplicate {
			return fmt.Errorf("expected resolution repeats required path %q", path.Name)
		}
		paths[path.Name] = struct{}{}
		if len(path.Edges) == 0 {
			return fmt.Errorf("expected resolution required path %q has no edges", path.Name)
		}
		edges := make(map[string]struct{}, len(path.Edges))
		for _, edge := range path.Edges {
			if !canonical(edge) {
				return fmt.Errorf("expected resolution required path %q has invalid edge %q", path.Name, edge)
			}
			if _, duplicate := edges[edge]; duplicate {
				return fmt.Errorf("expected resolution required path %q repeats edge %q", path.Name, edge)
			}
			edges[edge] = struct{}{}
		}
	}
	return nil
}

// MarshalExpectedResolution returns deterministic, newline-terminated JSON
// for a reviewed expected-resolution artifact.
func MarshalExpectedResolution(resolution LiveResolution) ([]byte, error) {
	if err := ValidateExpectedResolution(resolution); err != nil {
		return nil, fmt.Errorf("encode expected resolution: %w", err)
	}
	canonical := canonicalResolution(resolution)
	payload, err := json.MarshalIndent(expectedResolutionDocument{
		FormatVersion: ExpectedResolutionFormatVersion,
		Elements:      canonical.Elements,
		Paths:         canonical.Paths,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode expected resolution: %w", err)
	}
	return append(payload, '\n'), nil
}

// ParseExpectedResolution strictly decodes a reviewed expected deployment
// resolution. Unknown and duplicate fields or trailing JSON are rejected.
func ParseExpectedResolution(source []byte) (LiveResolution, error) {
	if err := strictjson.Validate(source); err != nil {
		return LiveResolution{}, fmt.Errorf("decode expected resolution: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document expectedResolutionDocument
	if err := decoder.Decode(&document); err != nil {
		return LiveResolution{}, fmt.Errorf("decode expected resolution: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return LiveResolution{}, errors.New("decode expected resolution: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return LiveResolution{}, fmt.Errorf("decode expected resolution trailing data: %w", err)
	}
	if document.FormatVersion != ExpectedResolutionFormatVersion {
		return LiveResolution{}, fmt.Errorf("decode expected resolution: format must be %d, got %d",
			ExpectedResolutionFormatVersion, document.FormatVersion)
	}
	resolution := LiveResolution{Elements: document.Elements, Paths: document.Paths}
	if err := ValidateExpectedResolution(resolution); err != nil {
		return LiveResolution{}, fmt.Errorf("decode expected resolution: %w", err)
	}
	return canonicalResolution(resolution), nil
}

func ReadExpectedResolution(path string) (LiveResolution, error) {
	if strings.TrimSpace(path) == "" {
		return LiveResolution{}, errors.New("an expected resolution needs an input path")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return LiveResolution{}, fmt.Errorf("read expected resolution: %w", err)
	}
	return ParseExpectedResolution(payload)
}

func WriteExpectedResolution(path string, resolution LiveResolution) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("an expected resolution needs an output path")
	}
	payload, err := MarshalExpectedResolution(resolution)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create expected resolution directory: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write expected resolution: %w", err)
	}
	return nil
}

func canonicalResolution(resolution LiveResolution) LiveResolution {
	result := LiveResolution{
		Elements: make([]ElementResolution, len(resolution.Elements)),
		Paths:    make([]SelectedPath, len(resolution.Paths)),
	}
	for index, item := range resolution.Elements {
		result.Elements[index] = cloneElementResolution(item)
		sort.Slice(result.Elements[index].Capabilities, func(left, right int) bool {
			a, b := result.Elements[index].Capabilities[left], result.Elements[index].Capabilities[right]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			if a.Contract != b.Contract {
				return a.Contract < b.Contract
			}
			return a.Provider.ID < b.Provider.ID
		})
	}
	sort.Slice(result.Elements, func(left, right int) bool {
		return result.Elements[left].Node < result.Elements[right].Node
	})
	for index, path := range resolution.Paths {
		result.Paths[index] = SelectedPath{Name: path.Name, Edges: slices.Clone(path.Edges)}
	}
	sort.Slice(result.Paths, func(left, right int) bool {
		return result.Paths[left].Name < result.Paths[right].Name
	})
	return result
}
