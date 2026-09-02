package host

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const effectsHubStateFormatVersion = 1

// effectsHubState is private reconciliation state. It deliberately retains
// only completed idempotency outcomes, payload-free audit rows, and cumulative
// counters. Arguments, authority evidence, pending confirmations, nonces, open
// sessions, and in-flight calls have no representation in this schema.
type effectsHubState struct {
	FormatVersion uint64                `json:"format_version"`
	CatalogDigest string                `json:"catalog_digest"`
	Stats         effectsDurableStats   `json:"stats"`
	Audit         []effectAuditState    `json:"audit"`
	Calls         []effectsHubStateCall `json:"calls"`
}

type effectsDurableStats struct {
	Calls      uint64 `json:"calls"`
	Results    uint64 `json:"results"`
	Authorized uint64 `json:"authorized"`
	Executed   uint64 `json:"executed"`
	Replayed   uint64 `json:"replayed"`
	Refused    uint64 `json:"refused"`
}

type effectAuditState struct {
	ScopeID           string               `json:"scope_id"`
	SessionID         string               `json:"session_id"`
	CallID            string               `json:"call_id"`
	Name              string               `json:"name"`
	DeclarationDigest string               `json:"declaration_digest"`
	Target            string               `json:"target"`
	Confirmation      legacyaction.Confirm `json:"confirmation"`
	Authorized        bool                 `json:"authorized"`
	Confirmed         bool                 `json:"confirmed"`
	Crossed           bool                 `json:"crossed"`
	Executed          bool                 `json:"executed"`
	Replayed          bool                 `json:"replayed"`
	ErrorCode         string               `json:"error_code"`
	StartedAt         string               `json:"started_at"`
	FinishedAt        string               `json:"finished_at"`
}

type effectsHubStateCall struct {
	SessionID string            `json:"session_id"`
	CallID    string            `json:"call_id"`
	Identity  string            `json:"identity"`
	Result    effectResultState `json:"result"`
	Record    effectAuditState  `json:"record"`
}

type effectResultState struct {
	Type     string               `json:"type"`
	ID       string               `json:"id"`
	Channel  EffectChannel        `json:"channel,omitempty"`
	Output   string               `json:"output,omitempty"`
	Error    *effectErrorState    `json:"error,omitempty"`
	Artifact *effectArtifactState `json:"artifact,omitempty"`
	Download *effectDownloadState `json:"download,omitempty"`
}

type effectErrorState struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type effectArtifactState struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	Version   uint64 `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

type effectDownloadState struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	Version   uint64 `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

type effectsHubStateWire struct {
	FormatVersion *uint64                    `json:"format_version"`
	CatalogDigest *string                    `json:"catalog_digest"`
	Stats         *effectsDurableStatsWire   `json:"stats"`
	Audit         *[]effectAuditStateWire    `json:"audit"`
	Calls         *[]effectsHubStateCallWire `json:"calls"`
}

type effectsDurableStatsWire struct {
	Calls      *uint64 `json:"calls"`
	Results    *uint64 `json:"results"`
	Authorized *uint64 `json:"authorized"`
	Executed   *uint64 `json:"executed"`
	Replayed   *uint64 `json:"replayed"`
	Refused    *uint64 `json:"refused"`
}

type effectAuditStateWire struct {
	ScopeID           *string               `json:"scope_id"`
	SessionID         *string               `json:"session_id"`
	CallID            *string               `json:"call_id"`
	Name              *string               `json:"name"`
	DeclarationDigest *string               `json:"declaration_digest"`
	Target            *string               `json:"target"`
	Confirmation      *legacyaction.Confirm `json:"confirmation"`
	Authorized        *bool                 `json:"authorized"`
	Confirmed         *bool                 `json:"confirmed"`
	Crossed           *bool                 `json:"crossed"`
	Executed          *bool                 `json:"executed"`
	Replayed          *bool                 `json:"replayed"`
	ErrorCode         *string               `json:"error_code"`
	StartedAt         *string               `json:"started_at"`
	FinishedAt        *string               `json:"finished_at"`
}

type effectsHubStateCallWire struct {
	SessionID *string                `json:"session_id"`
	CallID    *string                `json:"call_id"`
	Identity  *string                `json:"identity"`
	Result    *effectResultStateWire `json:"result"`
	Record    *effectAuditStateWire  `json:"record"`
}

type effectResultStateWire struct {
	Type     *string              `json:"type"`
	ID       *string              `json:"id"`
	Channel  *EffectChannel       `json:"channel,omitempty"`
	Output   *string              `json:"output,omitempty"`
	Error    *effectErrorState    `json:"error,omitempty"`
	Artifact *effectArtifactState `json:"artifact,omitempty"`
	Download *effectDownloadState `json:"download,omitempty"`
}

func incrementEffectCounter(counter *uint64) {
	if *counter < math.MaxUint64 {
		(*counter)++
	}
}

func effectToolMap(tools []compiledEffectTool) map[string]*compiledEffectTool {
	result := make(map[string]*compiledEffectTool, len(tools))
	for index := range tools {
		result[tools[index].declaration.Name] = &tools[index]
	}
	return result
}

func decodeEffectsHubState(
	raw json.RawMessage,
	limits effectLimits,
	tools []compiledEffectTool,
	catalogDigest string,
) (effectsHubState, error) {
	if len(raw) == 0 || len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return effectsHubState{}, fmt.Errorf(
			"effects state must be 1-%d bytes", pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	maxElements := limits.MaxCalls
	if limits.MaxAuditRecords > maxElements {
		maxElements = limits.MaxAuditRecords
	}
	if err := strictjson.ValidateWithLimits(raw, strictjson.Limits{
		MaxInputBytes: pluginruntime.MaximumStateSnapshotBytes,
		MaxDepth:      8, MaxTokens: 64 + 48*limits.MaxCalls + 34*limits.MaxAuditRecords,
		MaxObjectMembers: 20, MaxArrayElements: maxElements,
		MaxKeyBytes: 64, MaxTotalKeyBytes: 2 * pluginruntime.MaximumStateSnapshotBytes,
		MaxWorkBytes: 8 * pluginruntime.MaximumStateSnapshotBytes,
	}); err != nil {
		return effectsHubState{}, fmt.Errorf("effects state: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return effectsHubState{}, errors.New("effects state must be one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire effectsHubStateWire
	if err := decoder.Decode(&wire); err != nil {
		return effectsHubState{}, fmt.Errorf("effects state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return effectsHubState{}, errors.New("effects state has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return effectsHubState{}, fmt.Errorf("effects state trailing data: %w", err)
	}
	if wire.FormatVersion == nil || wire.CatalogDigest == nil || wire.Stats == nil ||
		wire.Audit == nil || wire.Calls == nil ||
		*wire.FormatVersion != effectsHubStateFormatVersion || *wire.CatalogDigest != catalogDigest {
		return effectsHubState{}, errors.New("effects state has missing, unsupported, or incompatible identity fields")
	}
	if len(*wire.Audit) > limits.MaxAuditRecords {
		return effectsHubState{}, errors.New("effects state exceeds max_audit_records")
	}
	if len(*wire.Calls) > limits.MaxCalls {
		return effectsHubState{}, errors.New("effects state exceeds max_calls")
	}
	stats, err := decodeEffectsDurableStats(wire.Stats, len(*wire.Audit), len(*wire.Calls))
	if err != nil {
		return effectsHubState{}, err
	}
	toolByName := effectToolMap(tools)
	state := effectsHubState{
		FormatVersion: effectsHubStateFormatVersion,
		CatalogDigest: catalogDigest,
		Stats:         stats,
		Audit:         make([]effectAuditState, 0, len(*wire.Audit)),
		Calls:         make([]effectsHubStateCall, 0, len(*wire.Calls)),
	}
	for index := range *wire.Audit {
		record, err := decodeEffectAuditState(&(*wire.Audit)[index], toolByName, false)
		if err != nil {
			return effectsHubState{}, fmt.Errorf("effects state audit %d: %w", index, err)
		}
		state.Audit = append(state.Audit, record)
	}
	previousKey := ""
	for index := range *wire.Calls {
		row, err := decodeEffectsHubStateCall(&(*wire.Calls)[index], limits, toolByName)
		if err != nil {
			return effectsHubState{}, fmt.Errorf("effects state call %d: %w", index, err)
		}
		key := row.SessionID + "\x00" + row.CallID
		if index > 0 && key <= previousKey {
			return effectsHubState{}, errors.New("effects state calls are not strictly ordered or repeat an identity")
		}
		previousKey = key
		state.Calls = append(state.Calls, row)
	}
	return state, nil
}

func decodeEffectsDurableStats(
	wire *effectsDurableStatsWire, auditRecords, calls int,
) (effectsDurableStats, error) {
	if wire.Calls == nil || wire.Results == nil || wire.Authorized == nil ||
		wire.Executed == nil || wire.Replayed == nil || wire.Refused == nil {
		return effectsDurableStats{}, errors.New("effects state stats have missing fields")
	}
	stats := effectsDurableStats{
		Calls: *wire.Calls, Results: *wire.Results, Authorized: *wire.Authorized,
		Executed: *wire.Executed, Replayed: *wire.Replayed, Refused: *wire.Refused,
	}
	if stats.Calls < uint64(calls) || stats.Results < uint64(calls) ||
		stats.Authorized < uint64(calls) || stats.Results < uint64(auditRecords) ||
		stats.Authorized > stats.Results || stats.Executed > stats.Authorized ||
		stats.Replayed > stats.Authorized || stats.Refused > stats.Results {
		return effectsDurableStats{}, errors.New("effects state stats are internally inconsistent")
	}
	return stats, nil
}

func decodeEffectAuditState(
	wire *effectAuditStateWire,
	tools map[string]*compiledEffectTool,
	cached bool,
) (effectAuditState, error) {
	if wire.ScopeID == nil || wire.SessionID == nil || wire.CallID == nil || wire.Name == nil ||
		wire.DeclarationDigest == nil || wire.Target == nil || wire.Confirmation == nil ||
		wire.Authorized == nil || wire.Confirmed == nil || wire.Crossed == nil ||
		wire.Executed == nil || wire.Replayed == nil || wire.ErrorCode == nil ||
		wire.StartedAt == nil || wire.FinishedAt == nil {
		return effectAuditState{}, errors.New("audit record has missing fields")
	}
	record := effectAuditState{
		ScopeID: *wire.ScopeID, SessionID: *wire.SessionID, CallID: *wire.CallID,
		Name: *wire.Name, DeclarationDigest: *wire.DeclarationDigest, Target: *wire.Target,
		Confirmation: *wire.Confirmation, Authorized: *wire.Authorized,
		Confirmed: *wire.Confirmed, Crossed: *wire.Crossed, Executed: *wire.Executed,
		Replayed: *wire.Replayed, ErrorCode: *wire.ErrorCode,
		StartedAt: *wire.StartedAt, FinishedAt: *wire.FinishedAt,
	}
	if !validEffectScopeID(record.ScopeID) {
		return effectAuditState{}, errors.New("audit record has an invalid scope identity")
	}
	if record.SessionID != "" && !validEffectID(record.SessionID) {
		return effectAuditState{}, errors.New("audit record has an invalid session identity")
	}
	if record.CallID != "" && !validEffectID(record.CallID) {
		return effectAuditState{}, errors.New("audit record has an invalid call identity")
	}
	if record.Name != "" && !validEffectName(record.Name) {
		return effectAuditState{}, errors.New("audit record has an invalid effect name")
	}
	if err := validateEffectTarget(record.Target); err != nil {
		return effectAuditState{}, fmt.Errorf("audit record target: %w", err)
	}
	if record.Confirmation != "" {
		confirmation, err := legacyaction.ParseConfirm(string(record.Confirmation))
		if err != nil || confirmation != record.Confirmation {
			return effectAuditState{}, errors.New("audit record has an invalid confirmation mode")
		}
	}
	if record.DeclarationDigest == "" {
		if record.Target != "" || record.Confirmation != "" {
			return effectAuditState{}, errors.New("audit record has declaration metadata without an identity")
		}
	} else {
		if !validEffectSHA256(record.DeclarationDigest) {
			return effectAuditState{}, errors.New("audit record has an invalid declaration digest")
		}
		tool := tools[record.Name]
		if tool == nil || record.DeclarationDigest != tool.declaration.Digest ||
			record.Target != tool.declaration.Target || record.Confirmation != tool.declaration.Confirm {
			return effectAuditState{}, errors.New("audit record does not match the immutable effect declaration")
		}
	}
	if record.ErrorCode != "" && !validEffectName(record.ErrorCode) {
		return effectAuditState{}, errors.New("audit record has an invalid error code")
	}
	started, err := parseCanonicalEffectStateTime(record.StartedAt)
	if err != nil {
		return effectAuditState{}, fmt.Errorf("audit record start: %w", err)
	}
	finished, err := parseCanonicalEffectStateTime(record.FinishedAt)
	if err != nil || finished.Before(started) {
		return effectAuditState{}, errors.New("audit record has an invalid finish time")
	}
	if (record.Executed && !record.Crossed) ||
		(record.Crossed && (!record.Authorized || !record.Confirmed)) ||
		(record.Confirmed && !record.Authorized) ||
		(record.Replayed && (!record.Authorized || record.Crossed || record.Executed)) {
		return effectAuditState{}, errors.New("audit record has inconsistent admission flags")
	}
	if cached && (!record.Authorized || record.Replayed || record.SessionID == "" ||
		record.CallID == "" || record.Name == "" || record.DeclarationDigest == "") {
		return effectAuditState{}, errors.New("cached call has an incomplete terminal audit record")
	}
	return record, nil
}

func decodeEffectsHubStateCall(
	wire *effectsHubStateCallWire,
	limits effectLimits,
	tools map[string]*compiledEffectTool,
) (effectsHubStateCall, error) {
	if wire.SessionID == nil || wire.CallID == nil || wire.Identity == nil ||
		wire.Result == nil || wire.Record == nil {
		return effectsHubStateCall{}, errors.New("cached call has missing fields")
	}
	if !validEffectID(*wire.SessionID) || !validEffectID(*wire.CallID) ||
		!validEffectSHA256(*wire.Identity) {
		return effectsHubStateCall{}, errors.New("cached call has an invalid identity")
	}
	record, err := decodeEffectAuditState(wire.Record, tools, true)
	if err != nil {
		return effectsHubStateCall{}, err
	}
	if record.SessionID != *wire.SessionID || record.CallID != *wire.CallID {
		return effectsHubStateCall{}, errors.New("cached call and audit identities differ")
	}
	tool := tools[record.Name]
	if tool == nil {
		return effectsHubStateCall{}, errors.New("cached call names an unavailable effect declaration")
	}
	result, err := decodeEffectResultState(wire.Result, limits, tool, *wire.CallID)
	if err != nil {
		return effectsHubStateCall{}, err
	}
	if (result.Error == nil && record.ErrorCode != "") ||
		(result.Error != nil && result.Error.Code != record.ErrorCode) {
		return effectsHubStateCall{}, errors.New("cached result and audit outcome differ")
	}
	return effectsHubStateCall{
		SessionID: *wire.SessionID, CallID: *wire.CallID, Identity: *wire.Identity,
		Result: result, Record: record,
	}, nil
}

func decodeEffectResultState(
	wire *effectResultStateWire,
	limits effectLimits,
	tool *compiledEffectTool,
	callID string,
) (effectResultState, error) {
	if wire.Type == nil || wire.ID == nil || *wire.Type != "result" || *wire.ID != callID {
		return effectResultState{}, errors.New("cached call has an invalid terminal result identity")
	}
	result := effectResultState{Type: "result", ID: callID}
	if wire.Channel != nil {
		result.Channel = *wire.Channel
	}
	if wire.Output != nil {
		result.Output = *wire.Output
	}
	if !utf8.ValidString(result.Output) || int64(len(result.Output)) > limits.MaxResultBytes {
		return effectResultState{}, errors.New("cached call has an invalid bounded output")
	}
	if wire.Error != nil {
		if !validEffectName(wire.Error.Code) ||
			wire.Error.Message != boundedEffectMessage(wire.Error.Message) ||
			result.Channel != "" || result.Output != "" || wire.Artifact != nil || wire.Download != nil {
			return effectResultState{}, errors.New("cached call has an invalid terminal error")
		}
		copy := *wire.Error
		result.Error = &copy
		return result, nil
	}
	if result.Channel != tool.declaration.Channel {
		return effectResultState{}, errors.New("cached call result has the wrong declared channel")
	}
	if wire.Artifact != nil && wire.Download != nil {
		return effectResultState{}, errors.New("cached call result contains two resource references")
	}
	if wire.Artifact != nil {
		if result.Channel != EffectChannelArtifact {
			return effectResultState{}, errors.New("cached artifact result has the wrong channel")
		}
		artifact, err := validateEffectArtifactState(*wire.Artifact)
		if err != nil {
			return effectResultState{}, err
		}
		result.Artifact = &artifact
	}
	if wire.Download != nil {
		if result.Channel != EffectChannelDownload {
			return effectResultState{}, errors.New("cached download result has the wrong channel")
		}
		download, err := validateEffectDownloadState(*wire.Download)
		if err != nil {
			return effectResultState{}, err
		}
		result.Download = &download
	}
	if (tool.builtin == "artifact" && result.Artifact == nil) ||
		(tool.builtin == "download" && result.Download == nil) {
		return effectResultState{}, errors.New("cached builtin result lacks its retained resource reference")
	}
	return result, nil
}

func validateEffectArtifactState(state effectArtifactState) (effectArtifactState, error) {
	if validateResourceID("artifact", state.ID) != nil || state.Title == "" ||
		state.Path != "/client/v1/artifacts/"+state.ID || !validEffectSHA256(state.Digest) ||
		state.Bytes <= 0 || state.Bytes > maximumArtifactBytes || state.Version == 0 {
		return effectArtifactState{}, errors.New("cached call has invalid artifact metadata")
	}
	if title, err := validateArtifactTitle(state.Title); err != nil || title != state.Title {
		return effectArtifactState{}, errors.New("cached call has invalid artifact title")
	}
	if _, err := parseCanonicalEffectStateTime(state.UpdatedAt); err != nil {
		return effectArtifactState{}, errors.New("cached call has invalid artifact timestamp")
	}
	return state, nil
}

func validateEffectDownloadState(state effectDownloadState) (effectDownloadState, error) {
	if validateResourceID("download", state.ID) != nil || validateDownloadFilename(state.Filename) != nil ||
		state.Path != "/client/v1/downloads/"+state.ID || !validEffectSHA256(state.Digest) ||
		state.Bytes <= 0 || state.Bytes > maximumDownloadBytes || state.Version == 0 {
		return effectDownloadState{}, errors.New("cached call has invalid download metadata")
	}
	if mediaType, err := canonicalMediaType(state.MediaType); err != nil || mediaType != state.MediaType {
		return effectDownloadState{}, errors.New("cached call has invalid download media type")
	}
	if _, err := parseCanonicalEffectStateTime(state.UpdatedAt); err != nil {
		return effectDownloadState{}, errors.New("cached call has invalid download timestamp")
	}
	return state, nil
}

func validEffectScopeID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validEffectSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func parseCanonicalEffectStateTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() || value != parsed.UTC().Format(time.RFC3339Nano) {
		return time.Time{}, errors.New("timestamp is not canonical UTC")
	}
	return parsed, nil
}

func (hub *effectsHub) quiesceState(ctx context.Context) (pluginruntime.StateResumer, error) {
	if ctx == nil {
		return nil, errors.New("quiesce effects state: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("quiesce effects state: %w", err)
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return nil, errors.New("quiesce effects state: provider is closed")
	}
	if hub.quiescing {
		hub.mu.Unlock()
		return nil, errors.New("effects state is already quiescing")
	}
	hub.quiescing = true
	var drained <-chan struct{}
	if hub.stats.InFlight > 0 {
		hub.mutationsDrained = make(chan struct{})
		drained = hub.mutationsDrained
	}
	hub.mu.Unlock()
	if drained != nil {
		select {
		case <-drained:
		case <-ctx.Done():
			hub.abortStateQuiescence()
			return nil, fmt.Errorf("quiesce effects state: %w", ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		hub.abortStateQuiescence()
		return nil, fmt.Errorf("quiesce effects state: %w", err)
	}
	var once sync.Once
	return func(ctx context.Context) error {
		if ctx == nil {
			return errors.New("resume effects state: nil context")
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("resume effects state: %w", err)
		}
		once.Do(hub.abortStateQuiescence)
		return nil
	}, nil
}

func (hub *effectsHub) abortStateQuiescence() {
	hub.mu.Lock()
	if !hub.closed {
		hub.quiescing = false
	}
	hub.mutationsDrained = nil
	hub.mu.Unlock()
}

func (hub *effectsHub) snapshotState(ctx context.Context) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("snapshot effects state: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot effects state: %w", err)
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return nil, errors.New("snapshot effects state: provider is closed")
	}
	if !hub.quiescing || hub.stats.InFlight != 0 {
		hub.mu.Unlock()
		return nil, errors.New("effects state snapshot requires drained mutation admission")
	}
	state := effectsHubState{
		FormatVersion: effectsHubStateFormatVersion,
		CatalogDigest: hub.catalogDigest,
		Stats: effectsDurableStats{
			Calls: hub.stats.Calls, Results: hub.stats.Results, Authorized: hub.stats.Authorized,
			Executed: hub.stats.Executed, Replayed: hub.stats.Replayed, Refused: hub.stats.Refused,
		},
		Audit: make([]effectAuditState, 0, len(hub.audit)),
		Calls: make([]effectsHubStateCall, 0, len(hub.calls)),
	}
	var retainedBytes int64
	for _, record := range hub.audit {
		encodedRecord := effectAuditStateFromRecord(record)
		retainedBytes += effectAuditStateStringBytes(encodedRecord)
		if retainedBytes > pluginruntime.MaximumStateSnapshotBytes {
			hub.mu.Unlock()
			return nil, fmt.Errorf(
				"effects state retained fields exceed the %d-byte migration limit",
				pluginruntime.MaximumStateSnapshotBytes,
			)
		}
		state.Audit = append(state.Audit, encodedRecord)
	}
	for key, call := range hub.calls {
		call.mu.Lock()
		if !call.completed {
			call.mu.Unlock()
			hub.mu.Unlock()
			return nil, errors.New("effects state contains an incomplete call after drain")
		}
		result := effectResultStateFromMessage(call.result)
		record := effectAuditStateFromRecord(call.record)
		identity := call.identity
		call.mu.Unlock()
		if key != record.SessionID+"\x00"+record.CallID {
			hub.mu.Unlock()
			return nil, errors.New("effects state call cache has an inconsistent key")
		}
		if err := hub.validateRetainedResultReferences(result); err != nil {
			hub.mu.Unlock()
			return nil, err
		}
		retainedBytes += int64(len(record.SessionID) + len(record.CallID) + len(identity))
		retainedBytes += effectAuditStateStringBytes(record) + effectResultStateStringBytes(result)
		if retainedBytes > pluginruntime.MaximumStateSnapshotBytes {
			hub.mu.Unlock()
			return nil, fmt.Errorf(
				"effects state retained fields exceed the %d-byte migration limit",
				pluginruntime.MaximumStateSnapshotBytes,
			)
		}
		state.Calls = append(state.Calls, effectsHubStateCall{
			SessionID: record.SessionID, CallID: record.CallID, Identity: identity,
			Result: result, Record: record,
		})
	}
	sort.Slice(state.Calls, func(left, right int) bool {
		leftKey := state.Calls[left].SessionID + "\x00" + state.Calls[left].CallID
		rightKey := state.Calls[right].SessionID + "\x00" + state.Calls[right].CallID
		return leftKey < rightKey
	})
	tools := cloneCompiledEffectToolsFromMap(hub.tools)
	hub.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot effects state: %w", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("snapshot effects state: %w", err)
	}
	if len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return nil, fmt.Errorf(
			"effects state is %d bytes; migration limit is %d",
			len(raw), pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	if _, err := decodeEffectsHubState(raw, hub.limits, tools, hub.catalogDigest); err != nil {
		return nil, fmt.Errorf("snapshot effects state validation: %w", err)
	}
	return raw, nil
}

func effectAuditStateStringBytes(state effectAuditState) int64 {
	return int64(
		len(state.ScopeID) + len(state.SessionID) + len(state.CallID) + len(state.Name) +
			len(state.DeclarationDigest) + len(state.Target) + len(state.Confirmation) +
			len(state.ErrorCode) + len(state.StartedAt) + len(state.FinishedAt),
	)
}

func effectResultStateStringBytes(state effectResultState) int64 {
	result := int64(len(state.Type) + len(state.ID) + len(state.Channel) + len(state.Output))
	if state.Error != nil {
		result += int64(len(state.Error.Code) + len(state.Error.Message))
	}
	if state.Artifact != nil {
		result += int64(
			len(state.Artifact.ID) + len(state.Artifact.Title) + len(state.Artifact.Path) +
				len(state.Artifact.Digest) + len(state.Artifact.UpdatedAt),
		)
	}
	if state.Download != nil {
		result += int64(
			len(state.Download.ID) + len(state.Download.Filename) + len(state.Download.MediaType) +
				len(state.Download.Path) + len(state.Download.Digest) + len(state.Download.UpdatedAt),
		)
	}
	return result
}

func cloneCompiledEffectToolsFromMap(source map[string]*compiledEffectTool) []compiledEffectTool {
	result := make([]compiledEffectTool, 0, len(source))
	for _, tool := range source {
		copy := *tool
		copy.declaration = copy.declaration.clone()
		result = append(result, copy)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].declaration.Name < result[right].declaration.Name
	})
	return result
}

func effectAuditStateFromRecord(record EffectAuditRecord) effectAuditState {
	return effectAuditState{
		ScopeID: record.ScopeID, SessionID: record.SessionID, CallID: record.CallID,
		Name: record.Name, DeclarationDigest: record.DeclarationDigest, Target: record.Target,
		Confirmation: record.Confirmation, Authorized: record.Authorized,
		Confirmed: record.Confirmed, Crossed: record.Crossed, Executed: record.Executed,
		Replayed: record.Replayed, ErrorCode: record.ErrorCode,
		StartedAt:  record.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt: record.FinishedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (state effectAuditState) record() EffectAuditRecord {
	started, _ := time.Parse(time.RFC3339Nano, state.StartedAt)
	finished, _ := time.Parse(time.RFC3339Nano, state.FinishedAt)
	return EffectAuditRecord{
		ScopeID: state.ScopeID, SessionID: state.SessionID, CallID: state.CallID,
		Name: state.Name, DeclarationDigest: state.DeclarationDigest, Target: state.Target,
		Confirmation: state.Confirmation, Authorized: state.Authorized,
		Confirmed: state.Confirmed, Crossed: state.Crossed, Executed: state.Executed,
		Replayed: state.Replayed, ErrorCode: state.ErrorCode,
		StartedAt: started, FinishedAt: finished,
	}
}

func effectResultStateFromMessage(message effectServerMessage) effectResultState {
	result := effectResultState{
		Type: message.Type, ID: message.ID, Channel: message.Channel, Output: message.Output,
	}
	if message.Error != nil {
		result.Error = &effectErrorState{Code: message.Error.Code, Message: message.Error.Message}
	}
	if message.Artifact != nil {
		result.Artifact = &effectArtifactState{
			ID: message.Artifact.ID, Title: message.Artifact.Title, Path: message.Artifact.Path,
			Digest: message.Artifact.Digest, Bytes: message.Artifact.Bytes,
			Version:   message.Artifact.Version,
			UpdatedAt: message.Artifact.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	if message.Download != nil {
		result.Download = &effectDownloadState{
			ID: message.Download.ID, Filename: message.Download.Filename,
			MediaType: message.Download.MediaType, Path: message.Download.Path,
			Digest: message.Download.Digest, Bytes: message.Download.Bytes,
			Version:   message.Download.Version,
			UpdatedAt: message.Download.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	return result
}

func (state effectResultState) message() effectServerMessage {
	message := effectServerMessage{
		Type: state.Type, ID: state.ID, Channel: state.Channel, Output: state.Output,
	}
	if state.Error != nil {
		message.Error = &effectWireError{Code: state.Error.Code, Message: state.Error.Message}
	}
	if state.Artifact != nil {
		updatedAt, _ := time.Parse(time.RFC3339Nano, state.Artifact.UpdatedAt)
		message.Artifact = &Artifact{
			ID: state.Artifact.ID, Title: state.Artifact.Title, Path: state.Artifact.Path,
			Digest: state.Artifact.Digest, Bytes: state.Artifact.Bytes,
			Version: state.Artifact.Version, UpdatedAt: updatedAt,
		}
	}
	if state.Download != nil {
		updatedAt, _ := time.Parse(time.RFC3339Nano, state.Download.UpdatedAt)
		message.Download = &Download{
			ID: state.Download.ID, Filename: state.Download.Filename,
			MediaType: state.Download.MediaType, Path: state.Download.Path,
			Digest: state.Download.Digest, Bytes: state.Download.Bytes,
			Version: state.Download.Version, UpdatedAt: updatedAt,
		}
	}
	return message
}

func (hub *effectsHub) validateRetainedResultReferences(result effectResultState) error {
	message := result.message()
	if message.Artifact != nil {
		stored, found := hub.artifacts.Lookup(message.Artifact.ID)
		if !found || stored != *message.Artifact {
			return errors.New("effects state references an artifact no longer owned by the mounted store")
		}
	}
	if message.Download != nil {
		stored, found := hub.downloads.Lookup(message.Download.ID)
		if !found || stored != *message.Download {
			return errors.New("effects state references a download no longer owned by the mounted store")
		}
	}
	return nil
}

func (hub *effectsHub) restoreState(state effectsHubState) error {
	calls := make(map[string]*effectCallState, len(state.Calls))
	for _, row := range state.Calls {
		if err := hub.validateRetainedResultReferences(row.Result); err != nil {
			return err
		}
		done := make(chan struct{})
		close(done)
		calls[row.SessionID+"\x00"+row.CallID] = &effectCallState{
			identity: row.Identity, done: done, result: row.Result.message(),
			record: row.Record.record(), completed: true,
		}
	}
	audit := make([]EffectAuditRecord, 0, len(state.Audit))
	for _, record := range state.Audit {
		audit = append(audit, record.record())
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed || hub.quiescing || len(hub.sessions) != 0 || len(hub.calls) != 0 ||
		hub.stats != (EffectsStats{}) || len(hub.audit) != 0 || state.CatalogDigest != hub.catalogDigest {
		return errors.New("effects state cannot restore into a nonempty, closed, or incompatible provider")
	}
	hub.calls = calls
	hub.audit = audit
	hub.stats = EffectsStats{
		Calls: state.Stats.Calls, Results: state.Stats.Results,
		Authorized: state.Stats.Authorized, Executed: state.Stats.Executed,
		Replayed: state.Stats.Replayed, Refused: state.Stats.Refused,
	}
	return nil
}

func (candidate effectsCandidate) MigrateState(
	_ context.Context, migration pluginruntime.StateMigration,
) (json.RawMessage, error) {
	if migration.EntryID != candidate.entryID || migration.Schema != presentation.EffectsStateContract ||
		migration.SourceImplementation == "" ||
		migration.SourceImplementation != strings.TrimSpace(migration.SourceImplementation) {
		return nil, errors.New("effects state migration identity is invalid")
	}
	state, err := decodeEffectsHubState(
		migration.Snapshot, candidate.limits, candidate.tools, candidate.catalogDigest,
	)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("migrate effects state: %w", err)
	}
	if len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return nil, fmt.Errorf(
			"migrated effects state is %d bytes; limit is %d",
			len(raw), pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	return raw, nil
}
