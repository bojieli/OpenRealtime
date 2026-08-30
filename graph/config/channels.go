package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
)

const ChannelsAPIVersion = "openrealtime.ai/channels/v1alpha1"

// ChannelDocument is the exceptional, edge-keyed queue-depth overlay. Loss
// remains topology (`=>`) and ordinary depths remain descriptor defaults.
type ChannelDocument struct {
	APIVersion string             `json:"apiVersion" yaml:"apiVersion"`
	Graph      string             `json:"graph" yaml:"graph"`
	Edges      map[string]Channel `json:"edges,omitempty" yaml:"edges,omitempty"`
}

type Channel struct {
	Depth int `json:"depth" yaml:"depth"`
}

func parseChannels(artifact Artifact, graphID string, limits Limits) (ChannelDocument, string, error) {
	if artifact.Path == "" {
		document := ChannelDocument{APIVersion: ChannelsAPIVersion, Graph: graphID, Edges: map[string]Channel{}}
		digest, err := channelDigest(document)
		return document, digest, err
	}
	var document ChannelDocument
	switch artifact.Encoding {
	case JSON:
		if err := preflightJSON(artifact.Path, artifact.Data, limits); err != nil {
			return ChannelDocument{}, "", err
		}
		if err := strictjson.Validate(artifact.Data); err != nil {
			return ChannelDocument{}, "", fmt.Errorf("parse channels %s: %w", artifact.Path, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(artifact.Data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&document); err != nil {
			return ChannelDocument{}, "", fmt.Errorf("parse channels %s: %w", artifact.Path, err)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return ChannelDocument{}, "", fmt.Errorf("parse channels %s: %w", artifact.Path, err)
		}
	case YAML:
		if err := preflightYAML(artifact.Path, artifact.Data, limits); err != nil {
			return ChannelDocument{}, "", err
		}
		if _, err := strictyaml.Decode(artifact.Path, artifact.Data, &document); err != nil {
			return ChannelDocument{}, "", err
		}
	default:
		return ChannelDocument{}, "", fmt.Errorf("channels artifact %s must use JSON or YAML", artifact.Path)
	}
	normalized, err := normalizeChannels(document, graphID, limits)
	if err != nil {
		return ChannelDocument{}, "", err
	}
	digest, err := channelDigest(normalized)
	return normalized, digest, err
}

func normalizeChannels(document ChannelDocument, graphID string, limits Limits) (ChannelDocument, error) {
	if document.APIVersion != ChannelsAPIVersion {
		return ChannelDocument{}, fmt.Errorf("channels apiVersion must be %q, got %q",
			ChannelsAPIVersion, document.APIVersion)
	}
	if document.Graph != graphID {
		return ChannelDocument{}, fmt.Errorf("channels target graph %q, topology graph is %q", document.Graph, graphID)
	}
	if len(document.Edges) > limits.MaxGraphEdges {
		return ChannelDocument{}, fmt.Errorf("channels artifact contains %d edges; maximum is %d",
			len(document.Edges), limits.MaxGraphEdges)
	}
	result := ChannelDocument{
		APIVersion: ChannelsAPIVersion, Graph: graphID,
		Edges: make(map[string]Channel, len(document.Edges)),
	}
	for edge, value := range document.Edges {
		if edge == "" || edge != strings.TrimSpace(edge) || strings.ContainsAny(edge, "\x00\r\n") ||
			len(edge) > limits.MaxStringBytes {
			return ChannelDocument{}, fmt.Errorf("channels artifact contains a non-canonical edge ID %q", edge)
		}
		if value.Depth <= 0 || value.Depth > 1_048_576 {
			return ChannelDocument{}, fmt.Errorf("channels edge %s depth must be between 1 and 1048576", edge)
		}
		result.Edges[edge] = value
	}
	return result, nil
}

func (document ChannelDocument) depths() map[string]int {
	result := make(map[string]int, len(document.Edges))
	for edge, channel := range document.Edges {
		result[edge] = channel.Depth
	}
	return result
}

func channelDigest(document ChannelDocument) (string, error) {
	ids := make([]string, 0, len(document.Edges))
	for id := range document.Edges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type edge struct {
		ID    string `json:"id"`
		Depth int    `json:"depth"`
	}
	canonical := struct {
		APIVersion string `json:"apiVersion"`
		Graph      string `json:"graph"`
		Edges      []edge `json:"edges"`
	}{APIVersion: document.APIVersion, Graph: document.Graph, Edges: make([]edge, 0, len(ids))}
	for _, id := range ids {
		canonical.Edges = append(canonical.Edges, edge{ID: id, Depth: document.Edges[id].Depth})
	}
	return semanticDigest("channels", canonical)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
