// Package codec reads and writes language-neutral element descriptor bundles.
package codec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
	"gopkg.in/yaml.v3"
)

const APIVersion = "openrealtime.ai/elements/v1alpha1"

type Bundle struct {
	APIVersion string               `json:"apiVersion" yaml:"apiVersion"`
	Elements   []element.Descriptor `json:"elements" yaml:"elements"`
}

func New(descriptors ...element.Descriptor) Bundle {
	return Bundle{APIVersion: APIVersion, Elements: descriptors}
}

func ParseJSON(path string, source []byte) (Bundle, error) {
	if err := strictjson.Validate(source); err != nil {
		return Bundle{}, fmt.Errorf("decode element bundle %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode element bundle %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Bundle{}, fmt.Errorf("decode element bundle %s: trailing JSON value", path)
	} else if !errors.Is(err, io.EOF) {
		return Bundle{}, fmt.Errorf("decode element bundle %s trailing data: %w", path, err)
	}
	return canonical(bundle)
}

func ParseYAML(path string, source []byte) (Bundle, error) {
	var bundle Bundle
	if _, err := strictyaml.Decode(path, source, &bundle); err != nil {
		return Bundle{}, err
	}
	return canonical(bundle)
}

func Parse(path string, source []byte) (Bundle, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return ParseYAML(path, source)
	case ".json":
		return ParseJSON(path, source)
	default:
		return Bundle{}, fmt.Errorf("element descriptor bundle %s must use .json, .yaml, or .yml", path)
	}
}

func MarshalJSON(bundle Bundle) ([]byte, error) {
	canonical, err := canonical(bundle)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode element bundle JSON: %w", err)
	}
	return append(payload, '\n'), nil
}

func MarshalYAML(bundle Bundle) ([]byte, error) {
	canonical, err := canonical(bundle)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(canonical); err != nil {
		return nil, fmt.Errorf("encode element bundle YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close element bundle YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

func canonical(bundle Bundle) (Bundle, error) {
	if bundle.APIVersion != APIVersion {
		return Bundle{}, fmt.Errorf("element bundle apiVersion is %q, want %q", bundle.APIVersion, APIVersion)
	}
	if len(bundle.Elements) == 0 {
		return Bundle{}, errors.New("element bundle contains no descriptors")
	}
	result := Bundle{APIVersion: APIVersion, Elements: make([]element.Descriptor, len(bundle.Elements))}
	seen := make(map[string]struct{}, len(bundle.Elements))
	for index, descriptor := range bundle.Elements {
		canonicalDescriptor, err := descriptor.Canonical()
		if err != nil {
			return Bundle{}, fmt.Errorf("element bundle descriptor %d: %w", index, err)
		}
		identity, err := canonicalDescriptor.Identity()
		if err != nil {
			return Bundle{}, err
		}
		key := fmt.Sprintf("%s@%d", identity.Name, identity.Revision)
		if _, duplicate := seen[key]; duplicate {
			return Bundle{}, fmt.Errorf("element bundle repeats %s@%d", identity.Name, identity.Revision)
		}
		seen[key] = struct{}{}
		result.Elements[index] = canonicalDescriptor
	}
	sort.Slice(result.Elements, func(left, right int) bool {
		if result.Elements[left].Name != result.Elements[right].Name {
			return result.Elements[left].Name < result.Elements[right].Name
		}
		return result.Elements[left].Revision < result.Elements[right].Revision
	})
	return result, nil
}
