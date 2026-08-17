// Package conformance validates the stable component API and the complete
// pinned OpenAI Realtime event registry.
package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

type ProtocolSource struct {
	URL         string `json:"url"`
	Revision    string `json:"revision"`
	SHA256      string `json:"sha256"`
	RetrievedAt string `json:"retrieved_at"`
}

type ProfileCount struct {
	Profile   openaiwire.Profile   `json:"profile"`
	Direction openaiwire.Direction `json:"direction"`
	Count     uint64               `json:"count"`
}

type ProtocolReport struct {
	Suite                   string         `json:"suite"`
	Passed                  bool           `json:"passed"`
	Definitions             uint64         `json:"definitions"`
	UniqueWireTypes         uint64         `json:"unique_wire_types"`
	SchemaDefinitions       uint64         `json:"schema_definitions"`
	SchemaSHA256            string         `json:"schema_sha256"`
	Source                  ProtocolSource `json:"source"`
	Counts                  []ProfileCount `json:"counts"`
	KnownDirectionProbes    uint64         `json:"known_direction_probes"`
	UnknownTypeRejected     bool           `json:"unknown_type_rejected"`
	CrossDirectionRejected  bool           `json:"cross_direction_rejected"`
	CrossProfileRejected    bool           `json:"cross_profile_rejected"`
	MissingRequiredRejected bool           `json:"missing_required_rejected"`
}

func RunProtocol() (ProtocolReport, error) {
	var bundle struct {
		Source ProtocolSource             `json:"x-openrealtime-source"`
		Defs   map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(openaiwire.SchemaBundle, &bundle); err != nil {
		return ProtocolReport{}, fmt.Errorf("decode OpenAI schema bundle: %w", err)
	}
	if len(bundle.Defs) == 0 || bundle.Source.Revision == "" || bundle.Source.SHA256 == "" {
		return ProtocolReport{}, errors.New("OpenAI schema bundle lacks definitions or provenance")
	}
	definitions := openaiwire.Definitions()
	if len(definitions) == 0 {
		return ProtocolReport{}, errors.New("OpenAI event registry is empty")
	}
	registryKeys := make(map[string]struct{}, len(definitions))
	wireTypes := make(map[openaiwire.EventType]struct{})
	counts := make(map[string]uint64)
	for _, definition := range definitions {
		key := string(definition.Profile) + "/" + string(definition.Direction) + "/" + string(definition.Type)
		if _, exists := registryKeys[key]; exists {
			return ProtocolReport{}, fmt.Errorf("duplicate protocol registry key %s", key)
		}
		registryKeys[key] = struct{}{}
		wireTypes[definition.Type] = struct{}{}
		counts[string(definition.Profile)+"/"+string(definition.Direction)]++
		const prefix = "#/$defs/"
		if !strings.HasPrefix(definition.SchemaRef, prefix) {
			return ProtocolReport{}, fmt.Errorf("definition %s has unsafe schema reference", key)
		}
		raw, exists := bundle.Defs[strings.TrimPrefix(definition.SchemaRef, prefix)]
		if !exists {
			return ProtocolReport{}, fmt.Errorf("definition %s references missing schema", key)
		}
		var eventSchema struct {
			Properties struct {
				Type struct {
					Enum []openaiwire.EventType `json:"enum"`
				} `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &eventSchema); err != nil {
			return ProtocolReport{}, fmt.Errorf("decode event schema for %s: %w", key, err)
		}
		if len(eventSchema.Properties.Type.Enum) != 1 || eventSchema.Properties.Type.Enum[0] != definition.Type {
			return ProtocolReport{}, fmt.Errorf("event schema type mismatch for %s", key)
		}
		lookedUp, exists := openaiwire.Lookup(definition.Profile, definition.Direction, definition.Type)
		if !exists || lookedUp != definition {
			return ProtocolReport{}, fmt.Errorf("protocol lookup mismatch for %s", key)
		}
	}
	validator := openaiwire.NewValidator()
	probes := []struct {
		profile   openaiwire.Profile
		direction openaiwire.Direction
		payload   string
	}{
		{openaiwire.ProfileRealtime, openaiwire.DirectionClient, `{"type":"input_audio_buffer.clear"}`},
		{openaiwire.ProfileTranscription, openaiwire.DirectionClient, `{"type":"input_audio_buffer.clear"}`},
		{openaiwire.ProfileTranslation, openaiwire.DirectionClient, `{"type":"session.close"}`},
		{openaiwire.ProfileBeta, openaiwire.DirectionClient, `{"type":"input_audio_buffer.clear"}`},
	}
	for _, probe := range probes {
		message, err := openaiwire.Decode([]byte(probe.payload))
		if err != nil {
			return ProtocolReport{}, err
		}
		if err := validator.Validate(probe.profile, probe.direction, message); err != nil {
			return ProtocolReport{}, fmt.Errorf("known protocol probe %s/%s failed: %w", probe.profile, probe.direction, err)
		}
	}
	unknown, err := openaiwire.Decode([]byte(`{"type":"future.not_yet_pinned"}`))
	if err != nil {
		return ProtocolReport{}, err
	}
	unknownRejected := validator.Validate(openaiwire.ProfileRealtime, openaiwire.DirectionClient, unknown) != nil
	if !unknownRejected {
		return ProtocolReport{}, errors.New("unknown event was falsely accepted as conformant")
	}
	clientOnly, err := openaiwire.Decode([]byte(`{"type":"output_audio_buffer.clear"}`))
	if err != nil {
		return ProtocolReport{}, err
	}
	crossDirectionRejected := validator.Validate(openaiwire.ProfileRealtime, openaiwire.DirectionServer, clientOnly) != nil
	translationOnly, err := openaiwire.Decode([]byte(`{"type":"session.close"}`))
	if err != nil {
		return ProtocolReport{}, err
	}
	crossProfileRejected := validator.Validate(openaiwire.ProfileRealtime, openaiwire.DirectionClient, translationOnly) != nil
	missingAudio, err := openaiwire.Decode([]byte(`{"type":"input_audio_buffer.append"}`))
	if err != nil {
		return ProtocolReport{}, err
	}
	missingRequiredRejected := validator.Validate(openaiwire.ProfileRealtime, openaiwire.DirectionClient, missingAudio) != nil
	if !crossDirectionRejected || !crossProfileRejected || !missingRequiredRejected {
		return ProtocolReport{}, errors.New("protocol validator accepted direction, profile, or required-field fault")
	}
	countRows := make([]ProfileCount, 0, len(counts))
	for key, count := range counts {
		parts := strings.Split(key, "/")
		countRows = append(countRows, ProfileCount{Profile: openaiwire.Profile(parts[0]), Direction: openaiwire.Direction(parts[1]), Count: count})
	}
	sort.Slice(countRows, func(left, right int) bool {
		if countRows[left].Profile != countRows[right].Profile {
			return countRows[left].Profile < countRows[right].Profile
		}
		return countRows[left].Direction < countRows[right].Direction
	})
	digest := sha256.Sum256(openaiwire.SchemaBundle)
	return ProtocolReport{
		Suite: "openai_realtime_protocol", Passed: true, Definitions: uint64(len(definitions)),
		UniqueWireTypes: uint64(len(wireTypes)), SchemaDefinitions: uint64(len(bundle.Defs)),
		SchemaSHA256: hex.EncodeToString(digest[:]), Source: bundle.Source, Counts: countRows,
		KnownDirectionProbes: uint64(len(probes)), UnknownTypeRejected: true,
		CrossDirectionRejected: true, CrossProfileRejected: true, MissingRequiredRejected: true,
	}, nil
}
