// Package config compiles the separate graph topology, resolution lock,
// values, channel, deployment, and secret-catalog artifacts into one immutable
// launch plan. Construction is pure: it resolves metadata and validates every
// contract before a runtime factory can acquire resources.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Encoding identifies an artifact frontend. It is provenance, not graph
// semantics: equivalent frontends may compile to the same Graph IR while
// retaining different exact source identities.
type Encoding string

const (
	ORTG Encoding = "ortg"
	JSON Encoding = "json"
	YAML Encoding = "yaml"
)

// Artifact is one caller-owned input. Create snapshots Data before returning;
// no Plan accessor aliases it.
type Artifact struct {
	Path     string
	Encoding Encoding
	Data     []byte
}

// Limits bounds attacker-controlled artifacts and the frozen result. Zero
// fields select production defaults; negative fields are invalid.
type Limits struct {
	MaxTopologyBytes   int
	MaxValuesBytes     int
	MaxLockBytes       int
	MaxChannelsBytes   int
	MaxDeploymentBytes int
	MaxTotalBytes      int
	MaxJSONDepth       int
	MaxJSONTokens      int
	MaxGraphNodes      int
	MaxGraphEdges      int
	MaxLockEntries     int
	MaxStringBytes     int
}

// DefaultLimits returns an independent set of production limits.
func DefaultLimits() Limits {
	return Limits{
		MaxTopologyBytes:   8 << 20,
		MaxValuesBytes:     16 << 20,
		MaxLockBytes:       16 << 20,
		MaxChannelsBytes:   8 << 20,
		MaxDeploymentBytes: 16 << 20,
		MaxTotalBytes:      48 << 20,
		MaxJSONDepth:       128,
		MaxJSONTokens:      1_000_000,
		MaxGraphNodes:      65_536,
		MaxGraphEdges:      262_144,
		MaxLockEntries:     65_536,
		MaxStringBytes:     64 << 10,
	}
}

func (limits Limits) normalized() (Limits, error) {
	defaults := DefaultLimits()
	fields := []struct {
		name   string
		value  *int
		preset int
		max    int
	}{
		{"topology bytes", &limits.MaxTopologyBytes, defaults.MaxTopologyBytes, 64 << 20},
		{"values bytes", &limits.MaxValuesBytes, defaults.MaxValuesBytes, 64 << 20},
		{"lock bytes", &limits.MaxLockBytes, defaults.MaxLockBytes, 64 << 20},
		{"channels bytes", &limits.MaxChannelsBytes, defaults.MaxChannelsBytes, 64 << 20},
		{"deployment bytes", &limits.MaxDeploymentBytes, defaults.MaxDeploymentBytes, 64 << 20},
		{"total artifact bytes", &limits.MaxTotalBytes, defaults.MaxTotalBytes, 256 << 20},
		{"JSON depth", &limits.MaxJSONDepth, defaults.MaxJSONDepth, 1_024},
		{"JSON tokens", &limits.MaxJSONTokens, defaults.MaxJSONTokens, 10_000_000},
		{"graph nodes", &limits.MaxGraphNodes, defaults.MaxGraphNodes, 1_000_000},
		{"graph edges", &limits.MaxGraphEdges, defaults.MaxGraphEdges, 4_000_000},
		{"lock entries", &limits.MaxLockEntries, defaults.MaxLockEntries, 1_000_000},
		{"string bytes", &limits.MaxStringBytes, defaults.MaxStringBytes, 1 << 20},
	}
	for _, field := range fields {
		if *field.value < 0 {
			return Limits{}, fmt.Errorf("config limit %s cannot be negative", field.name)
		}
		if *field.value == 0 {
			*field.value = field.preset
		}
		if *field.value > field.max {
			return Limits{}, fmt.Errorf("config limit %s exceeds safety maximum %d", field.name, field.max)
		}
	}
	return limits, nil
}

func normalizeArtifact(kind string, artifact Artifact, maximum int, required bool, limits Limits) (Artifact, error) {
	originalPath := artifact.Path
	artifact.Path = strings.TrimSpace(artifact.Path)
	if originalPath != artifact.Path {
		return Artifact{}, fmt.Errorf("%s artifact path has surrounding whitespace", kind)
	}
	if artifact.Path == "" {
		if required || len(artifact.Data) != 0 || artifact.Encoding != "" {
			return Artifact{}, fmt.Errorf("%s artifact requires a path", kind)
		}
		return Artifact{}, nil
	}
	if len(artifact.Path) > limits.MaxStringBytes || strings.ContainsAny(artifact.Path, "\x00\r\n") {
		return Artifact{}, fmt.Errorf("%s artifact path is not canonical", kind)
	}
	if len(artifact.Data) == 0 {
		return Artifact{}, fmt.Errorf("%s artifact %s is empty", kind, artifact.Path)
	}
	if len(artifact.Data) > maximum {
		return Artifact{}, fmt.Errorf("%s artifact %s has %d bytes; maximum is %d",
			kind, artifact.Path, len(artifact.Data), maximum)
	}
	encoding, err := resolveEncoding(artifact.Path, artifact.Encoding)
	if err != nil {
		return Artifact{}, fmt.Errorf("%s artifact: %w", kind, err)
	}
	artifact.Encoding = encoding
	artifact.Data = bytes.Clone(artifact.Data)
	return artifact, nil
}

func resolveEncoding(path string, explicit Encoding) (Encoding, error) {
	if explicit != "" {
		switch explicit {
		case ORTG, JSON, YAML:
			return explicit, nil
		default:
			return "", fmt.Errorf("unsupported encoding %q", explicit)
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ortg":
		return ORTG, nil
	case ".json", ".lock":
		return JSON, nil
	case ".yaml", ".yml":
		return YAML, nil
	default:
		return "", fmt.Errorf("cannot infer encoding for %s", path)
	}
}

func artifactDigest(domain string, encoding Encoding, source []byte) string {
	hash := sha256.New()
	hash.Write([]byte("openrealtime.config/" + domain + "/v1\x00"))
	hash.Write([]byte(encoding))
	hash.Write([]byte{0})
	hash.Write(source)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func semanticDigest(domain string, value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode %s identity: %w", domain, err)
	}
	hash := sha256.New()
	hash.Write([]byte("openrealtime.config/" + domain + "/v1\x00"))
	hash.Write(payload)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// preflightJSON bounds nesting and token work before a recursive strict-key
// validator or a typed decoder sees attacker-controlled input.
func preflightJSON(path string, source []byte, limits Limits) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	depth := 0
	tokens := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 {
				return fmt.Errorf("preflight JSON %s: unterminated composite", path)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("preflight JSON %s: %w", path, err)
		}
		tokens++
		if tokens > limits.MaxJSONTokens {
			return fmt.Errorf("preflight JSON %s: token count exceeds %d", path, limits.MaxJSONTokens)
		}
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{', '[':
				depth++
				if depth > limits.MaxJSONDepth {
					return fmt.Errorf("preflight JSON %s: nesting depth exceeds %d", path, limits.MaxJSONDepth)
				}
			case '}', ']':
				depth--
				if depth < 0 {
					return fmt.Errorf("preflight JSON %s: unexpected closing delimiter", path)
				}
			}
		case string:
			if len(typed) > limits.MaxStringBytes {
				return fmt.Errorf("preflight JSON %s: string exceeds %d bytes", path, limits.MaxStringBytes)
			}
		}
	}
}

// preflightYAML builds only the syntax tree, then walks it iteratively. The
// subsequent strict typed decoder is therefore never asked to recursively
// validate a document beyond the configured depth/token/string bounds.
func preflightYAML(path string, source []byte, limits Limits) error {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("preflight YAML %s: %w", path, err)
	}
	if len(document.Content) == 0 {
		return fmt.Errorf("preflight YAML %s: empty document", path)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("preflight YAML %s: multiple documents are not allowed", path)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("preflight YAML %s trailing data: %w", path, err)
	}
	type work struct {
		node  *yaml.Node
		depth int
	}
	stack := []work{{node: &document, depth: 1}}
	tokens := 0
	for len(stack) != 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		tokens++
		if tokens > limits.MaxJSONTokens {
			return fmt.Errorf("preflight YAML %s: node count exceeds %d", path, limits.MaxJSONTokens)
		}
		if current.depth > limits.MaxJSONDepth {
			return fmt.Errorf("preflight YAML %s: nesting depth exceeds %d", path, limits.MaxJSONDepth)
		}
		if current.node.Kind == yaml.ScalarNode && len(current.node.Value) > limits.MaxStringBytes {
			return fmt.Errorf("preflight YAML %s: scalar exceeds %d bytes", path, limits.MaxStringBytes)
		}
		for index := len(current.node.Content) - 1; index >= 0; index-- {
			stack = append(stack, work{node: current.node.Content[index], depth: current.depth + 1})
		}
	}
	return nil
}
