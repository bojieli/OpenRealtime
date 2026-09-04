package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	// SemanticDeciderRegistryService is the mount-scoped provider registry used
	// by SemanticAdmission. A graph selects one symbolic reference; endpoint,
	// credential, client, and model construction remain provider-plugin state.
	SemanticDeciderRegistryService = "policy.semantic.deciders"

	semanticAdmissionRuntimeID        = "builtin://openrealtime/elements/policy.SemanticAdmission"
	semanticAdmissionRuntimeRevision  = "implementation:9"
	defaultSemanticRecentLines        = 12
	defaultSemanticPending            = 64
	defaultSemanticTerminalMemory     = 512
	defaultSemanticCancelMemory       = 256
	defaultSemanticStandingMemory     = 64
	maximumSemanticTextBytes          = 1 << 20
	maximumSemanticAgentOutputStreams = 4096
)

var (
	semanticContextType    = stateelements.SnapshotType()
	semanticUpdateType     = SessionInvocationUpdateType()
	semanticCommitType     = stateelements.ObservationCommitOutcomeType()
	semanticCreateType     = ResponseCreateType()
	semanticCancelType     = GenerationCancelType()
	semanticGrantType      = element.Event(element.Named("policy.SemanticGrant"))
	semanticDecisionType   = element.Event(element.Named("policy.SemanticDecision"))
	semanticStateType      = element.State(element.Named("policy.SemanticAdmissionState"))
	semanticOutcomeType    = element.Event(element.Named("policy.SemanticAdmissionOutcome"))
	semanticResolutionType = element.State(element.Named("policy.SemanticDeciderResolution"))
)

func SemanticDecisionType() element.Type          { return semanticDecisionType.Clone() }
func SemanticGrantType() element.Type             { return semanticGrantType.Clone() }
func SemanticAdmissionStateType() element.Type    { return semanticStateType.Clone() }
func SemanticAdmissionOutcomeType() element.Type  { return semanticOutcomeType.Clone() }
func SemanticDeciderResolutionType() element.Type { return semanticResolutionType.Clone() }

// SemanticDeciderDescriptor is the provider-neutral, serializable identity of
// one enumerated interaction-act deployment. ConfigurationDigest binds all
// provider-specific non-secret behavior knobs without exposing their shape to
// the graph element. A credential is deliberately absent.
type SemanticDeciderDescriptor struct {
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	Protocol            string `json:"protocol"`
	Revision            string `json:"revision"`
	ConfigurationDigest string `json:"configuration_digest"`
	Vision              bool   `json:"vision,omitempty"`
	// StandingExtraction declares that the same provider plug-in also
	// implements interaction.Generator. SemanticAdmission may use that
	// separately bounded control-plane capability to extract structured
	// standing policies; extracted text never receives cognition, tool, or
	// speech authority.
	StandingExtraction bool  `json:"standing_extraction,omitempty"`
	DecisionTimeoutMS  int64 `json:"decision_timeout_ms"`
}

func (descriptor SemanticDeciderDescriptor) Validate() error {
	for name, value := range map[string]string{
		"provider": descriptor.Provider, "model": descriptor.Model,
		"protocol": descriptor.Protocol, "revision": descriptor.Revision,
	} {
		if !canonicalSemanticIdentity(value) {
			return fmt.Errorf("semantic decider %s is not canonical", name)
		}
	}
	if !validSemanticDigest(descriptor.ConfigurationDigest) {
		return errors.New("semantic decider configuration digest is not canonical SHA-256")
	}
	if descriptor.DecisionTimeoutMS < 1 || descriptor.DecisionTimeoutMS > 300_000 {
		return errors.New("semantic decider decision_timeout_ms must be between 1 and 300000")
	}
	if err := (liveidentity.Artifact{
		ID:       "interaction-model://" + descriptor.Provider,
		Revision: descriptor.Model + "@" + descriptor.Revision,
		Digest:   descriptor.ConfigurationDigest,
	}).Validate("semantic decider deployment"); err != nil {
		return err
	}
	return nil
}

// SemanticDecider extends the deliberately narrow enumerated Decider contract
// with exact live deployment identity. Decide can return only a supplied
// option. A descriptor may separately declare the optional
// interaction.Generator control-plane capability used for standing-policy
// extraction; neither capability can emit tools or acquire speech authority.
type SemanticDecider interface {
	coreinteraction.Decider
	Descriptor() SemanticDeciderDescriptor
}

type SemanticDeciderFactory func() (SemanticDecider, error)

type semanticDeciderEntry struct {
	descriptor SemanticDeciderDescriptor
	factory    SemanticDeciderFactory
}

// SemanticDeciderRegistry is immutable-by-registration deployment state.
// Factories are not invoked during graph discovery, profile freeze, or
// preflight; SemanticAdmission opens only its exact selected factory at Run.
type SemanticDeciderRegistry struct {
	mu      sync.RWMutex
	entries map[string]semanticDeciderEntry
}

func NewSemanticDeciderRegistry() *SemanticDeciderRegistry {
	return &SemanticDeciderRegistry{entries: make(map[string]semanticDeciderEntry)}
}

func (registry *SemanticDeciderRegistry) Register(
	reference string, descriptor SemanticDeciderDescriptor, factory SemanticDeciderFactory,
) error {
	if registry == nil {
		return errors.New("register semantic decider: nil registry")
	}
	if !canonicalSemanticIdentity(reference) {
		return errors.New("register semantic decider: reference is not canonical")
	}
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("register semantic decider %q: %w", reference, err)
	}
	if factory == nil {
		return fmt.Errorf("register semantic decider %q: nil factory", reference)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]semanticDeciderEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("semantic decider %q is already registered", reference)
	}
	registry.entries[reference] = semanticDeciderEntry{descriptor: descriptor, factory: factory}
	return nil
}

func (registry *SemanticDeciderRegistry) resolve(reference string) (semanticDeciderEntry, error) {
	if registry == nil {
		return semanticDeciderEntry{}, errors.New("semantic decider registry is nil")
	}
	registry.mu.RLock()
	entry, found := registry.entries[reference]
	registry.mu.RUnlock()
	if !found {
		return semanticDeciderEntry{}, fmt.Errorf("semantic decider %q is not registered", reference)
	}
	return entry, nil
}

// Describe returns the immutable deployment descriptor registered under
// reference without opening a live policy client. Graph-native interaction
// elements use this during Mount to validate their selected policy while
// preserving the repository rule that provider factories are opened only
// after Run begins.
func (registry *SemanticDeciderRegistry) Describe(
	reference string,
) (SemanticDeciderDescriptor, error) {
	entry, err := registry.resolve(strings.TrimSpace(reference))
	if err != nil {
		return SemanticDeciderDescriptor{}, err
	}
	return entry.descriptor, nil
}

// Open creates one independently owned semantic decider and verifies that its
// live descriptor is exactly the descriptor registered during deployment
// assembly. Callers must close the returned decider when it implements
// io.Closer. Opening a fresh instance is important: two graph policy nodes may
// select the same symbolic deployment, but must not accidentally share mutable
// per-decision caches or lifecycle state.
func (registry *SemanticDeciderRegistry) Open(
	reference string,
) (SemanticDecider, SemanticDeciderDescriptor, error) {
	entry, err := registry.resolve(strings.TrimSpace(reference))
	if err != nil {
		return nil, SemanticDeciderDescriptor{}, err
	}
	decider, err := entry.factory()
	if err != nil {
		return nil, SemanticDeciderDescriptor{}, fmt.Errorf(
			"create semantic decider %q: %w", reference, err,
		)
	}
	if semanticReflectedNil(decider) {
		return nil, SemanticDeciderDescriptor{}, fmt.Errorf(
			"semantic decider %q factory returned nil", reference,
		)
	}
	live := decider.Descriptor()
	if err := live.Validate(); err != nil {
		return nil, SemanticDeciderDescriptor{}, errors.Join(
			fmt.Errorf("semantic decider %q returned invalid descriptor: %w", reference, err),
			closeSemanticDecider(decider),
		)
	}
	if !reflect.DeepEqual(live, entry.descriptor) {
		return nil, SemanticDeciderDescriptor{}, errors.Join(
			fmt.Errorf("semantic decider %q descriptor drifted", reference),
			closeSemanticDecider(decider),
		)
	}
	return decider, entry.descriptor, nil
}

// SemanticAdmissionDescriptor is a policy gate between canonical observation
// commit and model activation. It emits the original typed commit/create only
// on the voice or silent branch selected by an enumerated semantic act. The
// decision and outcome are separately inspectable; suppression is never
// represented as a missing or successful generation.
func SemanticAdmissionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.SemanticAdmission", Revision: 7,
		Ports: []element.Port{
			{Name: "context", Direction: element.Input, Type: semanticContextType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "update", Direction: element.Input, Type: semanticUpdateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "agent_output", Direction: element.Input, Type: coreinteraction.AgentOutputType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "committed", Direction: element.Input, Type: semanticCommitType,
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "create", Direction: element.Input, Type: semanticCreateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "quiet", Direction: element.Input, Type: semanticCreateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: semanticCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "voice_committed", Direction: element.Output, Type: semanticGrantType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "silent_committed", Direction: element.Output, Type: semanticGrantType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "voice_create", Direction: element.Output, Type: semanticCreateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "silent_create", Direction: element.Output, Type: semanticCreateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "decision", Direction: element.Output, Type: semanticDecisionType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "state", Direction: element.Output, Type: semanticStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: semanticOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "resolved", Direction: element.Output, Type: semanticResolutionType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"committed", "create", "quiet"}, SampledState: []string{"context", "update", "agent_output"},
			Interrupts:     []string{"cancel"},
			Outcomes:       []string{"voice_committed", "silent_committed", "voice_create", "silent_create", "decision", "state", "outcome", "resolved"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/policy/semantic-admission-state/v2",
		ConfigSchema: "schema://openrealtime/policy/semantic-admission-config/v3",
		Dependencies: []element.Dependency{
			{Name: SemanticDeciderRegistryService},
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: cognitionelements.MediaResolverService, Optional: true},
		},
		Effects: []element.Effect{{Name: "policy.semantic-decision", Reversible: true}},
	}
}

type SemanticAdmissionConfig struct {
	Decider                     string                         `json:"decider"`
	DirectVisualInput           bool                           `json:"direct_visual_input,omitempty"`
	StandingExtraction          bool                           `json:"standing_extraction,omitempty"`
	VerifyVoiceActivation       bool                           `json:"verify_voice_activation,omitempty"`
	VerifySilentAction          bool                           `json:"verify_silent_action,omitempty"`
	MinimumActivationConfidence float64                        `json:"minimum_activation_confidence,omitempty"`
	RecentLines                 int                            `json:"recent_lines,omitempty"`
	MaxPending                  int                            `json:"max_pending,omitempty"`
	TerminalMemory              int                            `json:"terminal_memory,omitempty"`
	CancelMemory                int                            `json:"cancel_memory,omitempty"`
	StandingMemory              int                            `json:"standing_memory,omitempty"`
	TranscriptEvents            *SemanticTranscriptEventConfig `json:"transcript_events,omitempty"`
}

// SemanticTranscriptEventConfig is the values-plane hard boundary around the
// interaction model for live and terminal ASR revisions. Instructions can be
// replaced independently, while Acts remain an enumerated executable set.
type SemanticTranscriptEventConfig struct {
	Partial SemanticTranscriptEventRules `json:"partial"`
	Final   SemanticTranscriptEventRules `json:"final"`
}

type SemanticTranscriptEventRules struct {
	Instruction string                `json:"instruction"`
	Acts        []coreinteraction.Act `json:"acts"`
	TimeoutMS   int64                 `json:"timeout_ms"`
}

// ValidateSemanticTranscriptEventConfig validates the complete values-plane
// selection without creating or retaining a policy provider.
func ValidateSemanticTranscriptEventConfig(config SemanticTranscriptEventConfig) error {
	_, err := semanticTranscriptOptions(config)
	return err
}

// SemanticGrant couples the exact canonical observation receipt to the act
// that authorized its branch. SessionInvocation consumes this typed value so
// neither an envelope convention nor a mutated state receipt can silently
// erase whether generation deliberately began over an active speaker.
type SemanticGrant struct {
	Commit         stateelements.ObservationCommitOutcome `json:"commit"`
	Act            coreinteraction.Act                    `json:"act"`
	DecisionItemID string                                 `json:"decision_item_id"`
}

func (SemanticGrant) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type SemanticDecision struct {
	Operation            string              `json:"operation"`
	Act                  coreinteraction.Act `json:"act"`
	Policy               string              `json:"policy"`
	EvidenceItemID       string              `json:"evidence_item_id"`
	StreamID             string              `json:"stream_id,omitempty"`
	SourceRevision       uint64              `json:"source_revision,omitempty"`
	ContextVersion       uint64              `json:"context_version"`
	InvocationDigest     string              `json:"invocation_digest"`
	Provider             string              `json:"provider"`
	Model                string              `json:"model"`
	Confidence           float64             `json:"confidence,omitempty"`
	Measured             bool                `json:"measured,omitempty"`
	DecisionStage        string              `json:"decision_stage,omitempty"`
	Activation           string              `json:"activation,omitempty"`
	ActivationConfidence float64             `json:"activation_confidence,omitempty"`
	ActivationMeasured   bool                `json:"activation_measured,omitempty"`
	StandingCoverage     string              `json:"standing_coverage,omitempty"`
	CoverageConfidence   float64             `json:"coverage_confidence,omitempty"`
	CoverageMeasured     bool                `json:"coverage_measured,omitempty"`
	StandingBefore       int                 `json:"standing_before,omitempty"`
	StandingAfter        int                 `json:"standing_after,omitempty"`
	StandingPinned       int                 `json:"standing_pinned,omitempty"`
	StandingRevoked      int                 `json:"standing_revoked,omitempty"`
	StartedNS            uint64              `json:"started_ns"`
	FinishedNS           uint64              `json:"finished_ns"`
}

func (SemanticDecision) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type SemanticAdmissionOutcomeKind string

const (
	SemanticAdmissionAdmitted   SemanticAdmissionOutcomeKind = "admitted"
	SemanticAdmissionSuppressed SemanticAdmissionOutcomeKind = "suppressed"
	SemanticAdmissionCanceled   SemanticAdmissionOutcomeKind = "canceled"
	SemanticAdmissionRefused    SemanticAdmissionOutcomeKind = "refused"
	SemanticAdmissionFailed     SemanticAdmissionOutcomeKind = "failed"
	SemanticAdmissionIgnored    SemanticAdmissionOutcomeKind = "ignored"
)

type SemanticAdmissionOutcome struct {
	Kind           SemanticAdmissionOutcomeKind `json:"kind"`
	Operation      string                       `json:"operation"`
	Act            coreinteraction.Act          `json:"act,omitempty"`
	StreamID       string                       `json:"stream_id,omitempty"`
	SourceRevision uint64                       `json:"source_revision,omitempty"`
	ContextVersion uint64                       `json:"context_version,omitempty"`
	DecisionItemID string                       `json:"decision_item_id,omitempty"`
	Code           string                       `json:"code,omitempty"`
	Message        string                       `json:"message,omitempty"`
	FinishedNS     uint64                       `json:"finished_ns,omitempty"`
}

func (SemanticAdmissionOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type SemanticAdmissionState struct {
	InvocationRevision uint64 `json:"invocation_revision"`
	InvocationDigest   string `json:"invocation_digest,omitempty"`
	ContextVersion     uint64 `json:"context_version"`
	Pending            int    `json:"pending"`
	Active             bool   `json:"active"`
	AdmittedVoice      uint64 `json:"admitted_voice"`
	AdmittedSilent     uint64 `json:"admitted_silent"`
	Suppressed         uint64 `json:"suppressed"`
	Canceled           uint64 `json:"canceled"`
	Refused            uint64 `json:"refused"`
	Failed             uint64 `json:"failed"`
	Ignored            uint64 `json:"ignored"`
	TerminalMemory     int    `json:"terminal_memory"`
	CancellationMemory int    `json:"cancellation_memory"`
	StandingPolicies   int    `json:"standing_policies"`
	StandingMemory     int    `json:"standing_memory"`
}

func (SemanticAdmissionState) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var (
	_ element.InspectionCauseProvider = SemanticDecision{}
	_ element.InspectionCauseProvider = SemanticAdmissionOutcome{}
	_ element.InspectionCauseProvider = SemanticAdmissionState{}
)

type SemanticDeciderResolution struct {
	Reference        string                    `json:"reference"`
	Descriptor       SemanticDeciderDescriptor `json:"descriptor"`
	DescriptorDigest string                    `json:"descriptor_digest"`
	RegistryRevision uint64                    `json:"registry_revision"`
}

func decodeSemanticAdmissionConfig(source json.RawMessage) (SemanticAdmissionConfig, error) {
	config := SemanticAdmissionConfig{
		RecentLines: defaultSemanticRecentLines, MaxPending: defaultSemanticPending,
		TerminalMemory: defaultSemanticTerminalMemory, CancelMemory: defaultSemanticCancelMemory,
		StandingMemory: defaultSemanticStandingMemory,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return SemanticAdmissionConfig{}, err
	}
	if !canonicalSemanticIdentity(config.Decider) {
		return SemanticAdmissionConfig{}, errors.New("semantic admission decider is not canonical")
	}
	if config.RecentLines < 1 || config.RecentLines > 4096 {
		return SemanticAdmissionConfig{}, errors.New("semantic admission recent_lines must be between 1 and 4096")
	}
	if config.MaxPending < 1 || config.MaxPending > 1_000_000 {
		return SemanticAdmissionConfig{}, errors.New("semantic admission max_pending must be between 1 and 1000000")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return SemanticAdmissionConfig{}, errors.New("semantic admission terminal_memory must be between 1 and 1000000")
	}
	if config.CancelMemory < 1 || config.CancelMemory > 1_000_000 {
		return SemanticAdmissionConfig{}, errors.New("semantic admission cancel_memory must be between 1 and 1000000")
	}
	if config.StandingMemory < 1 || config.StandingMemory > 4096 {
		return SemanticAdmissionConfig{}, errors.New("semantic admission standing_memory must be between 1 and 4096")
	}
	if config.TranscriptEvents != nil {
		if _, err := semanticTranscriptOptions(*config.TranscriptEvents); err != nil {
			return SemanticAdmissionConfig{}, err
		}
	}
	if math.IsNaN(config.MinimumActivationConfidence) || math.IsInf(config.MinimumActivationConfidence, 0) ||
		config.MinimumActivationConfidence < 0 || config.MinimumActivationConfidence > 1 {
		return SemanticAdmissionConfig{}, errors.New("semantic admission minimum_activation_confidence must be between 0 and 1")
	}
	return config, nil
}

func semanticTranscriptOptions(
	config SemanticTranscriptEventConfig,
) (coreinteraction.TranscriptEventOptions, error) {
	for name, rules := range map[string]SemanticTranscriptEventRules{
		"partial": config.Partial, "final": config.Final,
	} {
		if rules.TimeoutMS < 1 || rules.TimeoutMS > 300_000 {
			return coreinteraction.TranscriptEventOptions{}, fmt.Errorf(
				"semantic admission %s transcript timeout_ms must be between 1 and 300000", name,
			)
		}
	}
	options := coreinteraction.TranscriptEventOptions{
		Partial: coreinteraction.TranscriptEventRules{
			Instruction: config.Partial.Instruction,
			Acts:        slices.Clone(config.Partial.Acts),
			Timeout:     time.Duration(config.Partial.TimeoutMS) * time.Millisecond,
		},
		Final: coreinteraction.TranscriptEventRules{
			Instruction: config.Final.Instruction,
			Acts:        slices.Clone(config.Final.Acts),
			Timeout:     time.Duration(config.Final.TimeoutMS) * time.Millisecond,
		},
	}
	if err := coreinteraction.ValidateTranscriptEventOptions(options); err != nil {
		return coreinteraction.TranscriptEventOptions{}, fmt.Errorf(
			"semantic admission transcript events: %w", err,
		)
	}
	return options, nil
}

func semanticDescriptorDigest(descriptor SemanticDeciderDescriptor) (string, error) {
	if err := descriptor.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func semanticInvocationDigest(update SessionInvocationUpdate) (string, error) {
	payload, err := json.Marshal(update.Invocation)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneSemanticUpdate(update SessionInvocationUpdate) SessionInvocationUpdate {
	return cloneSessionInvocationUpdate(update)
}

func canonicalSemanticIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validSemanticDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func semanticReflectedNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func registerSemanticAdmissionDescriptor(catalog *resolve.Catalog) error {
	return catalog.Register(SemanticAdmissionDescriptor())
}
