package binding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/plugin"
)

const SessionAdapterProfileFormatVersion uint64 = 1

var adapterProfileNamePattern = regexp.MustCompile(
	`^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$`,
)

var sessionAdapterContract = adapterContract(
	"openrealtime.graph.session_adapter", 1,
	"stable realtime session operations mapped to exact typed graph boundaries v1",
)

func SessionAdapterContract() plugin.Contract { return sessionAdapterContract }

// AdapterOperation is one direction of the stable gateway/session boundary.
// A profile lists only operations it supports; absence is the exact,
// inspectable representation of ErrUnsupported.
type AdapterOperation string

const (
	AdapterInputUpdate         AdapterOperation = "gateway.input.update"
	AdapterInputAudio          AdapterOperation = "gateway.input.audio"
	AdapterInputVideo          AdapterOperation = "gateway.input.video"
	AdapterInputText           AdapterOperation = "gateway.input.text"
	AdapterInputToolResult     AdapterOperation = "gateway.input.tool_result"
	AdapterInputCommitAudio    AdapterOperation = "gateway.input.commit_audio"
	AdapterInputCreateResponse AdapterOperation = "gateway.input.create_response"
	AdapterInputCancel         AdapterOperation = "gateway.input.cancel"
	AdapterInputTruncate       AdapterOperation = "gateway.input.truncate"

	AdapterOutputTurnBegin   AdapterOperation = "gateway.output.turn_begin"
	AdapterOutputTurnEnd     AdapterOperation = "gateway.output.turn_end"
	AdapterOutputActivity    AdapterOperation = "gateway.output.activity"
	AdapterOutputTranscript  AdapterOperation = "gateway.output.transcript"
	AdapterOutputObservation AdapterOperation = "gateway.output.observation"
	AdapterOutputSpeechBegin AdapterOperation = "gateway.output.speech_begin"
	AdapterOutputSpeechText  AdapterOperation = "gateway.output.speech_text"
	AdapterOutputSpeechAudio AdapterOperation = "gateway.output.speech_audio"
	AdapterOutputSpeechEnd   AdapterOperation = "gateway.output.speech_end"
	AdapterOutputToolCalls   AdapterOperation = "gateway.output.tool_calls"
	AdapterOutputFailed      AdapterOperation = "gateway.output.failed"
	AdapterOutputDebug       AdapterOperation = "gateway.output.debug"
)

var adapterOperations = map[AdapterOperation]ir.BoundaryDirection{
	AdapterInputUpdate: ir.InputBoundary, AdapterInputAudio: ir.InputBoundary,
	AdapterInputVideo: ir.InputBoundary, AdapterInputText: ir.InputBoundary,
	AdapterInputToolResult: ir.InputBoundary, AdapterInputCommitAudio: ir.InputBoundary,
	AdapterInputCreateResponse: ir.InputBoundary, AdapterInputCancel: ir.InputBoundary,
	AdapterInputTruncate:   ir.InputBoundary,
	AdapterOutputTurnBegin: ir.OutputBoundary, AdapterOutputTurnEnd: ir.OutputBoundary,
	AdapterOutputActivity: ir.OutputBoundary, AdapterOutputTranscript: ir.OutputBoundary,
	AdapterOutputObservation: ir.OutputBoundary, AdapterOutputSpeechBegin: ir.OutputBoundary,
	AdapterOutputSpeechText: ir.OutputBoundary, AdapterOutputSpeechAudio: ir.OutputBoundary,
	AdapterOutputSpeechEnd: ir.OutputBoundary, AdapterOutputToolCalls: ir.OutputBoundary,
	AdapterOutputFailed: ir.OutputBoundary, AdapterOutputDebug: ir.OutputBoundary,
}

// AdapterBoundary maps one stable gateway operation to one exact Graph IR
// boundary. Direction and Type are duplicated intentionally: they make the
// adapter contract reviewable without loading topology, then ValidateGraph
// proves that declaration is not stale.
type AdapterBoundary struct {
	Operation AdapterOperation     `json:"operation"`
	Boundary  string               `json:"boundary"`
	Direction ir.BoundaryDirection `json:"direction"`
	Type      element.Type         `json:"type"`
}

// SessionAdapterProfile is the immutable protocol-to-graph projection. Name,
// ownership, and capabilities are derived from this fingerprinted artifact;
// NativeConfig cannot assert independent values that drift from it.
type SessionAdapterProfile struct {
	FormatVersion     uint64              `json:"format_version"`
	Name              string              `json:"name"`
	Revision          uint64              `json:"revision"`
	GraphFingerprint  string              `json:"graph_fingerprint"`
	Contract          plugin.Contract     `json:"contract"`
	Ownership         legacy.Ownership    `json:"ownership"`
	Capabilities      legacy.Capabilities `json:"capabilities"`
	Boundaries        []AdapterBoundary   `json:"boundaries"`
	BoundaryMapDigest string              `json:"boundary_map_digest"`
	ProjectionDigest  string              `json:"projection_digest"`
	Fingerprint       string              `json:"fingerprint"`
}

func FreezeSessionAdapterProfile(source SessionAdapterProfile) (SessionAdapterProfile, error) {
	result := source.Clone()
	result.Fingerprint = ""
	result.Contract = SessionAdapterContract()
	result.Ownership = result.Ownership.Effective()
	result.Capabilities.Observers = canonicalAdapterStrings(result.Capabilities.Observers)
	for index := range result.Boundaries {
		result.Boundaries[index].Type = result.Boundaries[index].Type.Clone()
	}
	sort.Slice(result.Boundaries, func(left, right int) bool {
		return result.Boundaries[left].Operation < result.Boundaries[right].Operation
	})
	if err := result.validateStructure(); err != nil {
		return SessionAdapterProfile{}, err
	}
	boundaryDigest, err := adapterDigest(result.Boundaries)
	if err != nil {
		return SessionAdapterProfile{}, fmt.Errorf("fingerprint session adapter boundary map: %w", err)
	}
	result.BoundaryMapDigest = boundaryDigest
	projectionDigest, err := adapterDigest(struct {
		Ownership    legacy.Ownership    `json:"ownership"`
		Capabilities legacy.Capabilities `json:"capabilities"`
	}{result.Ownership, result.Capabilities})
	if err != nil {
		return SessionAdapterProfile{}, fmt.Errorf("fingerprint session adapter projection: %w", err)
	}
	result.ProjectionDigest = projectionDigest
	result.Fingerprint, err = adapterDigest(result)
	if err != nil {
		return SessionAdapterProfile{}, fmt.Errorf("fingerprint session adapter profile: %w", err)
	}
	return result, nil
}

func (profile SessionAdapterProfile) Validate() error {
	want, err := FreezeSessionAdapterProfile(profile)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(profile, want) {
		return fmt.Errorf("session adapter profile %s is not frozen or its fingerprint is stale", profile.Name)
	}
	return nil
}

func (profile SessionAdapterProfile) ValidateGraph(graph ir.Graph) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if err := graph.Validate(); err != nil {
		return fmt.Errorf("session adapter graph: %w", err)
	}
	if profile.GraphFingerprint != graph.Fingerprint {
		return fmt.Errorf("session adapter profile %s selects graph %s, mounted graph is %s",
			profile.Name, profile.GraphFingerprint, graph.Fingerprint)
	}
	boundaries := make(map[string]ir.Boundary, len(graph.Boundaries))
	for _, boundary := range graph.Boundaries {
		boundaries[boundary.Name] = boundary
	}
	for _, mapping := range profile.Boundaries {
		boundary, found := boundaries[mapping.Boundary]
		if !found {
			return fmt.Errorf("session adapter operation %s names absent graph boundary %q",
				mapping.Operation, mapping.Boundary)
		}
		if boundary.Direction != mapping.Direction || !boundary.Type.Equal(mapping.Type) {
			return fmt.Errorf("session adapter operation %s boundary %q drifted from %s %s to %s %s",
				mapping.Operation, mapping.Boundary, mapping.Direction, mapping.Type.String(),
				boundary.Direction, boundary.Type.String())
		}
	}
	return nil
}

func (profile SessionAdapterProfile) Clone() SessionAdapterProfile {
	result := profile
	result.Capabilities.Observers = slices.Clone(profile.Capabilities.Observers)
	result.Boundaries = slices.Clone(profile.Boundaries)
	for index := range result.Boundaries {
		result.Boundaries[index].Type = profile.Boundaries[index].Type.Clone()
	}
	return result
}

func (profile SessionAdapterProfile) validateStructure() error {
	if profile.FormatVersion != SessionAdapterProfileFormatVersion {
		return fmt.Errorf("session adapter profile %q uses format %d, want %d",
			profile.Name, profile.FormatVersion, SessionAdapterProfileFormatVersion)
	}
	if !adapterProfileNamePattern.MatchString(profile.Name) {
		return fmt.Errorf("session adapter profile has invalid name %q", profile.Name)
	}
	if profile.Revision == 0 {
		return fmt.Errorf("session adapter profile %s revision must be positive", profile.Name)
	}
	if !validAdapterDigest(profile.GraphFingerprint) {
		return fmt.Errorf("session adapter profile %s requires an exact graph fingerprint", profile.Name)
	}
	if profile.Contract != SessionAdapterContract() {
		return fmt.Errorf("session adapter profile %s has contract %+v, want %+v",
			profile.Name, profile.Contract, SessionAdapterContract())
	}
	if err := profile.Ownership.Validate(); err != nil {
		return fmt.Errorf("session adapter profile %s ownership: %w", profile.Name, err)
	}
	if profile.Capabilities.MaxOutputTokens < 0 {
		return fmt.Errorf("session adapter profile %s has negative output-token limit", profile.Name)
	}
	for index, observer := range profile.Capabilities.Observers {
		if observer == "" || observer != strings.TrimSpace(observer) ||
			strings.ContainsAny(observer, "\x00\r\n") {
			return fmt.Errorf("session adapter profile %s observer %d is not canonical",
				profile.Name, index)
		}
	}
	seenOperations := make(map[AdapterOperation]struct{}, len(profile.Boundaries))
	seenBoundaries := make(map[string]struct{}, len(profile.Boundaries))
	for index, mapping := range profile.Boundaries {
		wantDirection, known := adapterOperations[mapping.Operation]
		if !known {
			return fmt.Errorf("session adapter profile %s boundary %d has unknown operation %q",
				profile.Name, index, mapping.Operation)
		}
		if mapping.Direction != wantDirection {
			return fmt.Errorf("session adapter operation %s has direction %s, want %s",
				mapping.Operation, mapping.Direction, wantDirection)
		}
		if mapping.Boundary == "" || mapping.Boundary != strings.TrimSpace(mapping.Boundary) ||
			strings.ContainsAny(mapping.Boundary, "\x00\r\n") {
			return fmt.Errorf("session adapter operation %s has invalid boundary %q",
				mapping.Operation, mapping.Boundary)
		}
		if err := mapping.Type.ValidateConcretePort(); err != nil {
			return fmt.Errorf("session adapter operation %s type: %w", mapping.Operation, err)
		}
		if _, duplicate := seenOperations[mapping.Operation]; duplicate {
			return fmt.Errorf("session adapter operation %s is repeated", mapping.Operation)
		}
		if _, duplicate := seenBoundaries[mapping.Boundary]; duplicate {
			return fmt.Errorf("session adapter graph boundary %q is mapped more than once", mapping.Boundary)
		}
		seenOperations[mapping.Operation] = struct{}{}
		seenBoundaries[mapping.Boundary] = struct{}{}
	}
	for _, requirement := range []struct {
		required  bool
		operation AdapterOperation
		claim     string
	}{
		{profile.Capabilities.Video, AdapterInputVideo, "video"},
		{profile.Capabilities.Observations, AdapterOutputObservation, "observations"},
		{profile.Capabilities.ManualTurns, AdapterInputCommitAudio, "manual turns"},
		{profile.Capabilities.ManualTurns, AdapterInputCreateResponse, "manual turns"},
		{profile.Capabilities.Voice.Selectable, AdapterInputUpdate, "selectable voice"},
	} {
		if requirement.required {
			if _, found := seenOperations[requirement.operation]; !found {
				return fmt.Errorf("session adapter profile %s claims %s without operation %s",
					profile.Name, requirement.claim, requirement.operation)
			}
		}
	}
	return nil
}

// AdapterRegistration selects the exact executable that realizes a frozen
// adapter profile. Factory is process-private; Reference and Artifact are the
// complete public deployment evidence.
type AdapterRegistration struct {
	Reference string
	Artifact  inspect.ArtifactIdentity
	Factory   AdapterFactory
}

// Validate verifies the complete resource-free identity and executable
// registration. It does not invoke Factory. Catalogs use this method to reject
// malformed adapter contributions before selecting a graph-specific adapter.
func (registration AdapterRegistration) Validate() error {
	return registration.validate()
}

func (registration AdapterRegistration) validate() error {
	if registration.Reference == "" || registration.Reference != strings.TrimSpace(registration.Reference) ||
		strings.ContainsAny(registration.Reference, "\x00\r\n") ||
		mutableAdapterSelector(registration.Reference) {
		return errors.New("session adapter registration requires a canonical reference")
	}
	if err := registration.Artifact.Validate(); err != nil {
		return fmt.Errorf("session adapter registration artifact: %w", err)
	}
	if registration.Factory == nil {
		return errors.New("session adapter registration requires a factory")
	}
	return nil
}

func mutableAdapterSelector(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, placeholder := range []string{"latest", "current", "unknown", "unresolved"} {
		if value == placeholder || strings.HasSuffix(value, ":"+placeholder) ||
			strings.HasSuffix(value, "@"+placeholder) || strings.HasSuffix(value, "/"+placeholder) {
			return true
		}
	}
	return strings.Contains(value, "${") || strings.Contains(value, "{{")
}

func (profile SessionAdapterProfile) resolution(
	registration AdapterRegistration,
) inspect.SessionAdapterResolution {
	return inspect.SessionAdapterResolution{
		ContractName: profile.Contract.Name, ContractRevision: profile.Contract.Revision,
		ContractDigest: profile.Contract.Digest, ProfileFingerprint: profile.Fingerprint,
		Implementation: registration.Reference, Runtime: registration.Artifact,
		RuntimeEvidence:   inspect.EvidenceRegistered,
		BoundaryMapDigest: profile.BoundaryMapDigest, ProjectionDigest: profile.ProjectionDigest,
	}
}

func canonicalAdapterStrings(source []string) []string {
	result := slices.Clone(source)
	for index := range result {
		result[index] = strings.TrimSpace(result[index])
	}
	sort.Strings(result)
	return slices.Compact(result)
}

func adapterContract(name string, revision uint64, schema string) plugin.Contract {
	digest := sha256.Sum256([]byte("openrealtime/session-adapter-contract/v1\x00" + name + "\x00" + schema))
	return plugin.Contract{Name: name, Revision: revision, Digest: "sha256:" + hex.EncodeToString(digest[:])}
}

func adapterDigest(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validAdapterDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
