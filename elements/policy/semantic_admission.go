package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
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
	semanticAdmissionRuntimeRevision  = "implementation:20"
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
		Name:          "policy.SemanticAdmission", Revision: 10,
		Ports: []element.Port{
			{Name: "context", Direction: element.Input, Type: semanticContextType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "update", Direction: element.Input, Type: semanticUpdateType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "agent_output", Direction: element.Input, Type: coreinteraction.AgentOutputType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "release", Direction: element.Input, Type: speechelements.PlaybackReleaseType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "safe_release", Direction: element.Output, Type: speechelements.PlaybackReceiptType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
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
			{Name: "voice_create", Direction: element.Output, Type: semanticCreateType,
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
			Triggers: []string{"committed", "create", "quiet", "release"}, SampledState: []string{"context", "update", "agent_output"},
			Interrupts:     []string{"cancel"},
			Outcomes:       []string{"voice_committed", "voice_create", "decision", "state", "outcome", "resolved", "safe_release"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/policy/semantic-admission-state/v2",
		ConfigSchema: "schema://openrealtime/policy/semantic-admission-config/v4",
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
	Decider            string `json:"decider"`
	DirectVisualInput  bool   `json:"direct_visual_input,omitempty"`
	StandingExtraction bool   `json:"standing_extraction,omitempty"`
	RecentLines        int    `json:"recent_lines,omitempty"`
	MaxPending         int    `json:"max_pending,omitempty"`
	TerminalMemory     int    `json:"terminal_memory,omitempty"`
	CancelMemory       int    `json:"cancel_memory,omitempty"`
	StandingMemory     int    `json:"standing_memory,omitempty"`
	// Rules is the instruction the policy model reads with the situation. It
	// is one text for every event kind - a provisional transcript, a settled
	// one, a frame, a quiet clock - because the question is the same at each:
	// speak now or not, and while speaking, stop or not. Empty selects
	// interaction.ChoiceInstruction. The option set is never configured; it
	// is derived from whether the agent is speaking.
	Rules string `json:"rules,omitempty"`
}

// SemanticGrant couples the exact canonical observation receipt to the act
// that authorized its branch. SessionInvocation consumes this typed value so
// neither an envelope convention nor a mutated state receipt can silently
// erase whether generation deliberately began over an active speaker.
type SemanticGrant struct {
	Commit         stateelements.ObservationCommitOutcome `json:"commit"`
	Choice         coreinteraction.Choice                 `json:"choice"`
	DecisionItemID string                                 `json:"decision_item_id"`
	// SpokeOver says the choice invoked the voice while the other party was
	// still speaking. The overlap controller protects such a run from being
	// cancelled as an accidental barge-in: it was the decision.
	SpokeOver bool `json:"spoke_over,omitempty"`
}

func (SemanticGrant) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type SemanticDecision struct {
	Operation string                 `json:"operation"`
	Choice    coreinteraction.Choice `json:"choice"`
	SpokeOver bool                   `json:"spoke_over,omitempty"`
	// Event names the transcript event the choice was taken on - "partial",
	// "final", or empty for a frame, a clock, or an explicit request. A trace
	// that cannot tell a decision on a live hypothesis from one on the settled
	// words cannot say whether the policy acted early or late.
	Event            coreinteraction.TranscriptEventKind `json:"event,omitempty"`
	Policy           string                              `json:"policy"`
	EvidenceItemID   string                              `json:"evidence_item_id"`
	StreamID         string                              `json:"stream_id,omitempty"`
	SourceRevision   uint64                              `json:"source_revision,omitempty"`
	ContextVersion   uint64                              `json:"context_version"`
	InvocationDigest string                              `json:"invocation_digest"`
	Provider         string                              `json:"provider"`
	Model            string                              `json:"model"`
	Confidence       float64                             `json:"confidence,omitempty"`
	Measured         bool                                `json:"measured,omitempty"`
	// DecisionStage names what produced the choice: "policy" when the model
	// chose it, "state" when the option set left nothing to decide, "clock"
	// when an unowned quiet tick was answered without asking.
	DecisionStage   string `json:"decision_stage,omitempty"`
	StandingBefore  int    `json:"standing_before,omitempty"`
	StandingAfter   int    `json:"standing_after,omitempty"`
	StandingPinned  int    `json:"standing_pinned,omitempty"`
	StandingRevoked int    `json:"standing_revoked,omitempty"`
	StartedNS       uint64 `json:"started_ns"`
	FinishedNS      uint64 `json:"finished_ns"`
	// Evidence is exactly what the policy model was shown, when one was
	// asked. It is conversation content and therefore a debug payload, but
	// without it a wrong choice cannot be told from a wrong question.
	Evidence string `json:"evidence,omitempty"`
	// Standing reports what the standing-instruction pass concluded on this
	// event, when it ran.
	Standing *StandingReport `json:"standing,omitempty"`
	// Questions are the step questions the policy was asked, in order, with
	// their answers: the choice is composed from them.
	Questions []coreinteraction.AskedQuestion `json:"questions,omitempty"`
}

// StandingReport is what one run of the standing-instruction pass did:
// the words it read, every question it asked and the answer it got, and the
// pinboard it left behind.
type StandingReport struct {
	Utterance string                      `json:"utterance"`
	Pinned    []string                    `json:"pinned,omitempty"`
	Revoked   []string                    `json:"revoked,omitempty"`
	Dropped   []string                    `json:"dropped,omitempty"`
	InForce   []string                    `json:"in_force,omitempty"`
	Calls     []coreinteraction.ModelCall `json:"calls,omitempty"`
	Failure   string                      `json:"failure,omitempty"`
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
	Kind           SemanticAdmissionOutcomeKind        `json:"kind"`
	Operation      string                              `json:"operation"`
	Choice         *coreinteraction.Choice             `json:"choice,omitempty"`
	Event          coreinteraction.TranscriptEventKind `json:"event,omitempty"`
	StreamID       string                              `json:"stream_id,omitempty"`
	SourceRevision uint64                              `json:"source_revision,omitempty"`
	ContextVersion uint64                              `json:"context_version,omitempty"`
	DecisionItemID string                              `json:"decision_item_id,omitempty"`
	Code           string                              `json:"code,omitempty"`
	Message        string                              `json:"message,omitempty"`
	FinishedNS     uint64                              `json:"finished_ns,omitempty"`
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
	Stopped            uint64 `json:"stopped"`
	Suppressed         uint64 `json:"suppressed"`
	Canceled           uint64 `json:"canceled"`
	Refused            uint64 `json:"refused"`
	Failed             uint64 `json:"failed"`
	Ignored            uint64 `json:"ignored"`
	TerminalMemory     int    `json:"terminal_memory"`
	CancellationMemory int    `json:"cancellation_memory"`
	StandingPolicies   int    `json:"standing_policies"`
	StandingMemory     int    `json:"standing_memory"`
	// Holding says the next decision waits for an admitted generation to
	// answer; HoldTimeouts counts holds lifted because it never did.
	Holding      bool   `json:"holding,omitempty"`
	HoldTimeouts uint64 `json:"hold_timeouts,omitempty"`
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
	config.Rules = strings.TrimSpace(config.Rules)
	if len(config.Rules) > maximumSemanticTextBytes {
		return SemanticAdmissionConfig{}, fmt.Errorf("semantic admission rules exceed %d bytes", maximumSemanticTextBytes)
	}
	return config, nil
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
