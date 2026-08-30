package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	sessionInvocationRuntimeID          = "builtin://openrealtime/elements/policy.SessionInvocation"
	sessionInvocationRuntimeRevision    = "implementation:2"
	defaultSessionInvocationTerminalMax = 512
)

var (
	sessionInvocationUpdateType  = element.Event(element.Named("policy.SessionInvocationUpdate"))
	responseCreateType           = element.Trigger(element.Named("policy.ResponseCreate"))
	sessionInvocationStateType   = element.State(element.Named("policy.SessionInvocationState"))
	sessionInvocationOutcomeType = element.Event(
		element.Named("policy.SessionInvocationOutcome"),
	)
)

func SessionInvocationUpdateType() element.Type { return sessionInvocationUpdateType.Clone() }
func ResponseCreateType() element.Type          { return responseCreateType.Clone() }
func SessionInvocationStateType() element.Type  { return sessionInvocationStateType.Clone() }
func SessionInvocationOutcomeType() element.Type {
	return sessionInvocationOutcomeType.Clone()
}

// SessionInvocationDescriptor is the graph-owned session configuration and
// activation boundary. Transport adapters translate authenticated session
// updates into Update events; the element snapshots those settings and is the
// only component allowed to place them in cognition.Generate.
//
// Observation commits carry an exact canonical prefix and therefore produce
// authority-bearing activations. Explicit response creation is also visible
// and carries no observation authority, but its transport-supplied context
// binding prevents the model from sampling a stale trajectory snapshot.
func SessionInvocationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.SessionInvocation",
		Revision:      1,
		Ports: []element.Port{
			{Name: "update", Direction: element.Input, Type: sessionInvocationUpdateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "committed", Direction: element.Input, Type: observationCommitType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "create", Direction: element.Input, Type: responseCreateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: generationCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "trigger", Direction: element.Output, Type: generationTriggerType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "authority", Direction: element.Output, Type: authorityCandidateType,
				Cardinality: element.One, Required: false, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: sessionInvocationStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: sessionInvocationOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:       []string{"update", "committed", "create"},
			Interrupts:     []string{"cancel"},
			Outcomes:       []string{"trigger", "authority", "state", "outcome"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/policy/session-invocation-state/v1",
		ConfigSchema: "schema://openrealtime/policy/session-invocation-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "policy.session-invocation.memory", Reversible: true}},
	}
}

type SessionInvocationConfig struct {
	Role           string `json:"role"`
	TerminalMemory int    `json:"terminal_memory,omitempty"`
	CancelMemory   int    `json:"cancel_memory,omitempty"`
}

type SessionInvocationUpdate struct {
	Revision   uint64                  `json:"revision"`
	Invocation continuation.Invocation `json:"invocation"`
}

type ResponseCreate struct {
	ResponseID string `json:"response_id"`
	// ExpectedContextVersion and ExpectedContextItemID are one inseparable
	// state binding supplied by the authenticated transport adapter. Explicit
	// creation has no observation authority, but it may not sample an older
	// context merely because graph input lanes were scheduled independently.
	ExpectedContextVersion *uint64 `json:"expected_context_version"`
	ExpectedContextItemID  string  `json:"expected_context_item_id"`
}

type SessionInvocationOutcomeKind string

const (
	SessionInvocationUpdated  SessionInvocationOutcomeKind = "updated"
	SessionInvocationEmitted  SessionInvocationOutcomeKind = "emitted"
	SessionInvocationCanceled SessionInvocationOutcomeKind = "canceled"
	SessionInvocationRefused  SessionInvocationOutcomeKind = "refused"
	SessionInvocationIgnored  SessionInvocationOutcomeKind = "ignored"
)

type SessionInvocationOutcome struct {
	Kind               SessionInvocationOutcomeKind `json:"kind"`
	Operation          string                       `json:"operation"`
	GenerationID       string                       `json:"generation_id,omitempty"`
	Role               string                       `json:"role"`
	InvocationRevision uint64                       `json:"invocation_revision,omitempty"`
	InvocationDigest   string                       `json:"invocation_digest,omitempty"`
	StreamID           string                       `json:"stream_id,omitempty"`
	SourceRevision     uint64                       `json:"source_revision,omitempty"`
	ContextVersion     uint64                       `json:"context_version,omitempty"`
	TriggerItemID      string                       `json:"trigger_item_id,omitempty"`
	Code               string                       `json:"code,omitempty"`
	Message            string                       `json:"message,omitempty"`
	FinishedNS         uint64                       `json:"finished_ns,omitempty"`
}

type SessionInvocationState struct {
	Role               string `json:"role"`
	InvocationRevision uint64 `json:"invocation_revision"`
	InvocationDigest   string `json:"invocation_digest,omitempty"`
	ContextVersion     uint64 `json:"context_version"`
	Updated            uint64 `json:"updated"`
	Emitted            uint64 `json:"emitted"`
	Canceled           uint64 `json:"canceled"`
	Refused            uint64 `json:"refused"`
	Ignored            uint64 `json:"ignored"`
	TerminalMemory     int    `json:"terminal_memory"`
	CancellationMemory int    `json:"cancellation_memory"`
}

func decodeSessionInvocationConfig(source json.RawMessage) (SessionInvocationConfig, error) {
	config := SessionInvocationConfig{
		TerminalMemory: defaultSessionInvocationTerminalMax,
		CancelMemory:   defaultGenerationCancelMemory,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return SessionInvocationConfig{}, err
	}
	if err := validatePolicyIdentifier("session invocation role", config.Role, true); err != nil {
		return SessionInvocationConfig{}, err
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return SessionInvocationConfig{}, errors.New("terminal_memory must be between 1 and 1000000")
	}
	if config.CancelMemory < 1 || config.CancelMemory > 1_000_000 {
		return SessionInvocationConfig{}, errors.New("cancel_memory must be between 1 and 1000000")
	}
	return config, nil
}

func validateSessionInvocationUpdate(update SessionInvocationUpdate) (SessionInvocationUpdate, string, error) {
	if update.Revision == 0 {
		return SessionInvocationUpdate{}, "", errors.New("session invocation revision must be positive")
	}
	if err := validateInvocation(update.Invocation); err != nil {
		return SessionInvocationUpdate{}, "", err
	}
	seenCapabilities := make(map[string]struct{}, len(update.Invocation.Capabilities))
	for _, capability := range update.Invocation.Capabilities {
		if _, duplicate := seenCapabilities[capability.Name]; duplicate {
			return SessionInvocationUpdate{}, "", fmt.Errorf("session invocation repeats capability %q", capability.Name)
		}
		seenCapabilities[capability.Name] = struct{}{}
	}
	seenTools := make(map[string]struct{}, len(update.Invocation.Tools))
	for _, tool := range update.Invocation.Tools {
		if _, duplicate := seenTools[tool.Name]; duplicate {
			return SessionInvocationUpdate{}, "", fmt.Errorf("session invocation repeats tool %q", tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
	}
	update.Invocation = cloneInvocation(update.Invocation)
	payload, err := json.Marshal(update.Invocation)
	if err != nil {
		return SessionInvocationUpdate{}, "", fmt.Errorf("fingerprint session invocation: %w", err)
	}
	digest := sha256.Sum256(payload)
	return update, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func sessionInvocationUpdatePayload(payload any) (SessionInvocationUpdate, bool) {
	switch value := payload.(type) {
	case SessionInvocationUpdate:
		value.Invocation = cloneInvocation(value.Invocation)
		return value, true
	case *SessionInvocationUpdate:
		if value != nil {
			copy := *value
			copy.Invocation = cloneInvocation(value.Invocation)
			return copy, true
		}
	}
	return SessionInvocationUpdate{}, false
}

func responseCreatePayload(payload any) (ResponseCreate, bool) {
	switch value := payload.(type) {
	case ResponseCreate:
		value.ExpectedContextVersion = cloneResponseCreateVersion(value.ExpectedContextVersion)
		return value, true
	case *ResponseCreate:
		if value != nil {
			copy := *value
			copy.ExpectedContextVersion = cloneResponseCreateVersion(value.ExpectedContextVersion)
			return copy, true
		}
	}
	return ResponseCreate{}, false
}

func cloneResponseCreateVersion(source *uint64) *uint64 {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func cloneSessionInvocationState(value SessionInvocationState) SessionInvocationState { return value }

func cloneSessionInvocationOutcome(value SessionInvocationOutcome) SessionInvocationOutcome {
	return value
}

func cloneSessionInvocationUpdate(value SessionInvocationUpdate) SessionInvocationUpdate {
	value.Invocation = cloneInvocation(value.Invocation)
	return value
}

func sessionInvocationGenerationID(role, sessionID, operation, causeID string, revision uint64) string {
	hash := sha256.New()
	for _, identity := range []string{role, sessionID, operation, causeID} {
		_, _ = fmt.Fprintf(hash, "%d:", len(identity))
		_, _ = hash.Write([]byte(identity))
	}
	_, _ = fmt.Fprintf(hash, "%d", revision)
	return "generation:sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func responseCreateIdentifier(value ResponseCreate) (string, error) {
	identifier := strings.TrimSpace(value.ResponseID)
	if identifier == "" || identifier != value.ResponseID {
		return "", errors.New("response create requires a canonical response ID")
	}
	if err := validatePolicyIdentifier("response ID", identifier, true); err != nil {
		return "", err
	}
	if value.ExpectedContextVersion == nil {
		return "", errors.New("response create requires an expected context version")
	}
	if err := validatePolicyIdentifier(
		"response create expected context item ID", value.ExpectedContextItemID, true,
	); err != nil {
		return "", err
	}
	return identifier, nil
}

func invocationForCommit(update SessionInvocationUpdate, commit stateelements.ObservationCommitOutcome) continuation.Invocation {
	invocation := cloneInvocation(update.Invocation)
	invocation.SourceRevision = commit.SourceRevision
	return invocation
}

func invocationForManualCreate(update SessionInvocationUpdate) continuation.Invocation {
	invocation := cloneInvocation(update.Invocation)
	invocation.SourceRevision = 0
	return invocation
}

func authorityForSessionCommit(
	generationID string, cause element.Envelope, trigger element.Envelope,
	commit stateelements.ObservationCommitOutcome,
) authority.Candidate {
	return authority.Candidate{
		RunID: generationID, SessionID: cause.SessionID,
		ActivationItemID: trigger.ItemID, ActivationCauseItemID: cause.ItemID,
		ObservationItemID:        commit.TrajectoryItemID,
		ObservationTriggerItemID: commit.TriggerItemID,
		SourceRevision:           commit.SourceRevision, ContextVersion: commit.StoreVersion,
		ContextEnvelopeItemID: commit.Context.StateItemID,
		ContextTailItem:       commit.TrajectoryItemID,
	}
}

func cloneCausalParents(source []string) []string { return slices.Clone(source) }
