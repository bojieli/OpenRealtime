// Package evidence defines the separate, non-executable manifest that binds
// empirical benchmark profiles to exact plans, elements, implementations,
// hardware, and load definitions. It never contains credentials or runtime
// service handles. An empty Profiles list truthfully states that a shipped
// graph makes no empirical performance claims.
package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "openrealtime.ai/evidence/v1alpha1"

	maximumArtifactBytes = 16 << 20
	maximumProfiles      = 65_536
	maximumStringBytes   = 64 << 10
)

// Document is one graph-scoped manifest of empirical profile artifacts.
type Document struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Graph      string    `json:"graph" yaml:"graph"`
	Profiles   []Profile `json:"profiles" yaml:"profiles"`
}

// Profile points at one immutable empirical result and records every axis
// needed to decide whether that result applies to a deployment. Metrics stay
// in Artifact; this manifest cannot turn an unmeasured graph into a claim.
type Profile struct {
	Name            string                   `json:"name" yaml:"name"`
	Artifact        inspect.ArtifactIdentity `json:"artifact" yaml:"artifact"`
	PlanFingerprint string                   `json:"plan_fingerprint" yaml:"plan_fingerprint"`
	NodeID          string                   `json:"node_id" yaml:"node_id"`
	Element         element.Identity         `json:"element" yaml:"element"`
	Implementation  inspect.ArtifactIdentity `json:"implementation" yaml:"implementation"`
	Hardware        inspect.ArtifactIdentity `json:"hardware" yaml:"hardware"`
	Load            inspect.ArtifactIdentity `json:"load" yaml:"load"`
}

func ParseJSON(path string, source []byte) (Document, error) {
	if len(source) > maximumArtifactBytes {
		return Document{}, fmt.Errorf("parse evidence %s: artifact exceeds %d bytes", path, maximumArtifactBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return Document{}, fmt.Errorf("parse evidence %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("parse evidence %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Document{}, fmt.Errorf("parse evidence %s: trailing JSON value", path)
	} else if !errors.Is(err, io.EOF) {
		return Document{}, fmt.Errorf("parse evidence %s trailing data: %w", path, err)
	}
	return normalize(document)
}

func ParseYAML(path string, source []byte) (Document, error) {
	if len(source) > maximumArtifactBytes {
		return Document{}, fmt.Errorf("parse evidence %s: artifact exceeds %d bytes", path, maximumArtifactBytes)
	}
	var document Document
	if _, err := strictyaml.Decode(path, source, &document); err != nil {
		return Document{}, err
	}
	return normalize(document)
}

func MarshalJSON(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode evidence JSON: %w", err)
	}
	return append(payload, '\n'), nil
}

func MarshalYAML(document Document) ([]byte, error) {
	normalized, err := normalize(document)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(normalized); err != nil {
		return nil, fmt.Errorf("encode evidence YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close evidence YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

// Fingerprint identifies the canonical evidence manifest. It covers profile
// references and applicability axes, never benchmark payloads or secrets.
func Fingerprint(document Document) (string, error) {
	normalized, err := normalize(document)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("fingerprint evidence: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func normalize(document Document) (Document, error) {
	if document.APIVersion != APIVersion {
		return Document{}, fmt.Errorf("evidence apiVersion must be %q, got %q", APIVersion, document.APIVersion)
	}
	if err := canonicalText("evidence graph", document.Graph); err != nil {
		return Document{}, err
	}
	if len(document.Profiles) > maximumProfiles {
		return Document{}, fmt.Errorf("evidence contains %d profiles; maximum is %d",
			len(document.Profiles), maximumProfiles)
	}
	result := Document{
		APIVersion: APIVersion,
		Graph:      document.Graph,
		Profiles:   make([]Profile, len(document.Profiles)),
	}
	seen := make(map[string]struct{}, len(document.Profiles))
	for index, source := range document.Profiles {
		profile, err := normalizeProfile(source)
		if err != nil {
			return Document{}, fmt.Errorf("evidence profile %d: %w", index, err)
		}
		if _, duplicate := seen[profile.Name]; duplicate {
			return Document{}, fmt.Errorf("evidence repeats profile %q", profile.Name)
		}
		seen[profile.Name] = struct{}{}
		result.Profiles[index] = profile
	}
	sort.Slice(result.Profiles, func(left, right int) bool {
		return result.Profiles[left].Name < result.Profiles[right].Name
	})
	return result, nil
}

func normalizeProfile(profile Profile) (Profile, error) {
	if err := canonicalText("profile name", profile.Name); err != nil {
		return Profile{}, err
	}
	if err := profile.Artifact.Validate(); err != nil {
		return Profile{}, fmt.Errorf("profile %s artifact: %w", profile.Name, err)
	}
	if err := validateDigest("plan fingerprint", profile.PlanFingerprint); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", profile.Name, err)
	}
	if err := canonicalText("profile node ID", profile.NodeID); err != nil {
		return Profile{}, err
	}
	if err := element.ValidateIdentity(profile.Element); err != nil {
		return Profile{}, fmt.Errorf("profile %s element: %w", profile.Name, err)
	}
	for _, axis := range []struct {
		label    string
		artifact inspect.ArtifactIdentity
	}{
		{label: "implementation", artifact: profile.Implementation},
		{label: "hardware", artifact: profile.Hardware},
		{label: "load", artifact: profile.Load},
	} {
		if err := axis.artifact.Validate(); err != nil {
			return Profile{}, fmt.Errorf("profile %s %s: %w", profile.Name, axis.label, err)
		}
	}
	return profile, nil
}

func canonicalText(kind, value string) error {
	if value == "" || value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "\x00\r\n") || len(value) > maximumStringBytes {
		return fmt.Errorf("%s %q is not canonical", kind, value)
	}
	return nil
}

func validateDigest(kind, digest string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 {
		return fmt.Errorf("%s has invalid SHA-256 digest %q", kind, digest)
	}
	hexadecimal := digest[len(prefix):]
	if _, err := hex.DecodeString(hexadecimal); err != nil || hexadecimal != strings.ToLower(hexadecimal) {
		return fmt.Errorf("%s has invalid SHA-256 digest %q", kind, digest)
	}
	return nil
}
