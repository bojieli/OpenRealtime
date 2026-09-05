// Package cognition implements graph-native model execution over the
// repository's provider-neutral continuation API.
//
// A TextModel samples an immutable trajectory snapshot when an explicit
// generation trigger is admitted. It emits prepared data only: this package
// neither appends to the canonical trajectory nor grants speech or tool
// execution authority. Those decisions remain visible graph topology.
package cognition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	// ProviderRegistryService contains deployment-selected continuation
	// providers. Provider selection is a node value, not topology syntax.
	ProviderRegistryService = "cognition.continuation.providers"
	// MediaResolverService optionally resolves media handles referenced by a
	// sampled trajectory for multimodal continuation adapters.
	MediaResolverService = "cognition.media.resolver"

	defaultMaxOutputBytes   = 4 << 20
	defaultMaxEvents        = 65_536
	defaultMaxToolProposals = 1_024
	maximumOutputBytes      = 64 << 20
	maximumEvents           = 1_000_000
	maximumToolProposals    = 4_096
)

var (
	contextType  = element.State(element.Named("trajectory.Snapshot"))
	generateType = element.Trigger(element.Named("cognition.Generate"))
	cancelType   = element.Interrupt(element.Named("flow.RunID"))
	textType     = element.Segmented(
		element.Named("text.PreparedDelta"), element.Named("flow.RunID"),
	)
	resultType             = element.Event(element.Named("cognition.Result"))
	toolProposalType       = element.Stream(element.Named("tool.Proposal"))
	outcomeType            = element.Event(element.Named("cognition.Outcome"))
	providerResolutionType = element.State(element.Named("cognition.ProviderResolution"))
)

// Public type helpers let graph-boundary adapters construct exact envelopes
// without exporting mutable package-level Type values.
func ContextType() element.Type            { return contextType.Clone() }
func GenerateType() element.Type           { return generateType.Clone() }
func CancelType() element.Type             { return cancelType.Clone() }
func PreparedTextType() element.Type       { return textType.Clone() }
func ResultType() element.Type             { return resultType.Clone() }
func ToolProposalType() element.Type       { return toolProposalType.Clone() }
func OutcomeType() element.Type            { return outcomeType.Clone() }
func ProviderResolutionType() element.Type { return providerResolutionType.Clone() }

// TextModelDescriptor makes state sampling, activation, cancellation, and all
// terminal/data paths independently inspectable. MaxConcurrency is one per
// node; parallel fast and deliberative work is expressed by separate nodes.
func TextModelDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "cognition.TextModel",
		Revision:      3,
		Ports: []element.Port{
			{Name: "context", Direction: element.Input, Type: contextType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "trigger", Direction: element.Input, Type: generateType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "cancel", Direction: element.Input, Type: cancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "text", Direction: element.Output, Type: textType,
				Cardinality: element.One, Required: true, DefaultDepth: 64},
			{Name: "result", Direction: element.Output, Type: resultType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "tools", Direction: element.Output, Type: toolProposalType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: outcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "resolved", Direction: element.Output, Type: providerResolutionType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"trigger"}, SampledState: []string{"context"},
			Interrupts:     []string{"cancel"},
			Outcomes:       []string{"text", "result", "tools", "outcome", "resolved"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/cognition/text-model-state/v1",
		ConfigSchema: "schema://openrealtime/cognition/text-model-config/v1",
		Dependencies: []element.Dependency{
			{Name: ProviderRegistryService},
			{Name: graphruntime.ClockServiceName},
			{Name: MediaResolverService, Optional: true},
		},
		Effects: []element.Effect{{Name: "cognition.provider.session", Reversible: true}},
	}
}

// TextModelConfig selects one symbolic deployment registration. Endpoint,
// credential, model, and adapter settings belong to that provider deployment,
// not to graph topology.
type TextModelConfig struct {
	Provider         string `json:"provider"`
	RetainReasoning  bool   `json:"retain_reasoning,omitempty"`
	MaxOutputBytes   int    `json:"max_output_bytes,omitempty"`
	MaxEvents        int    `json:"max_events,omitempty"`
	MaxToolProposals int    `json:"max_tool_proposals,omitempty"`
}

// Generate opens one model run. Invocation is the established provider-neutral
// request contract. ExpectedContextVersion is optional but lets a policy bind
// a trigger to one exact sampled prefix instead of accepting a newer state.
type Generate struct {
	Invocation continuation.Invocation `json:"invocation"`
	// SpokeOver records that policy deliberately authorized this run while the
	// user was still speaking. It is carried through prepared text into speech
	// provenance; it does not itself grant playback authority.
	SpokeOver bool `json:"spoke_over,omitempty"`
	// ExpectedContextVersion distinguishes an unconstrained trigger (nil)
	// from one explicitly bound to the empty trajectory (pointer to zero).
	ExpectedContextVersion *uint64 `json:"expected_context_version,omitempty"`
	// ExpectedContextItemID optionally binds the version to the exact State
	// envelope that released the policy trigger. This prevents two independently
	// wired state sources from satisfying the same numeric version with different
	// prefixes. A generation policy should set both fields from one sampled edge.
	ExpectedContextItemID string `json:"expected_context_item_id,omitempty"`
	// CommittedContext is the compact, commit-bound identity of an immutable
	// trajectory prefix. When present, both legacy expected-context fields are
	// required to agree exactly with it. This lets a model reconstruct that
	// prefix from a later append-only State value without accepting an unrelated
	// or replayed activation.
	CommittedContext *stateelements.CommittedContext `json:"committed_context,omitempty"`
}

// InspectionCause records the explicit activation policy that opened the
// model run; result and stream payloads below identify the run itself.
func (Generate) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

// Cancel is an addressed control request. RunID may be omitted when the
// envelope's RunID or cancellation scope supplies the address.
type Cancel struct {
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// TextBoundary frames a prepared text stream. An end is emitted after every
// opened stream, including failed or canceled runs, so stream-aware arbiters
// can always release the run.
type TextBoundary string

const (
	TextBegin TextBoundary = "begin"
	TextChunk TextBoundary = "delta"
	TextEnd   TextBoundary = "end"
)

// PreparedTextDelta is prepared model output. The distinct port payload type
// prevents a generic committed-text sink from accepting it without an
// explicit policy/commit element.
type PreparedTextDelta struct {
	Boundary    TextBoundary `json:"boundary"`
	Text        string       `json:"text,omitempty"`
	Index       uint64       `json:"index"`
	Interrupted bool         `json:"interrupted,omitempty"`
	SpokeOver   bool         `json:"spoke_over,omitempty"`
}

func (PreparedTextDelta) InspectionCause() element.InspectionCauseKind {
	return element.CauseModelRun
}

// ToolProposal is structured model output without execution authority. Even a
// legacy provider descriptor that says "execute" produces only this type;
// authorization and dispatch require explicit downstream elements.
type ToolProposal struct {
	Call              trajectory.ToolCall        `json:"call"`
	Declared          bool                       `json:"declared"`
	ProviderAuthority continuation.ToolAuthority `json:"provider_authority"`
}

func (ToolProposal) InspectionCause() element.InspectionCauseKind {
	return element.CauseModelRun
}

// PreparedOutputKind identifies one ordered, uncommitted model-output segment.
type PreparedOutputKind string

const (
	PreparedReasoning PreparedOutputKind = "reasoning"
	PreparedAssistant PreparedOutputKind = "assistant"
	PreparedTool      PreparedOutputKind = "tool_proposal"
)

// PreparedOutput retains provider order for a later, explicit trajectory
// commit adapter. Adjacent text deltas of the same kind are coalesced.
type PreparedOutput struct {
	Kind     PreparedOutputKind `json:"kind"`
	Text     string             `json:"text,omitempty"`
	Proposal *ToolProposal      `json:"proposal,omitempty"`
}

// Result is the complete prepared artifact for one accepted run. It includes
// enough provenance for a separate compare-and-append adapter and deliberately
// contains no claim that anything was committed, spoken, or executed.
type Result struct {
	RunID             string                  `json:"run_id"`
	ProviderReference string                  `json:"provider_reference"`
	Descriptor        continuation.Descriptor `json:"descriptor"`
	ContextVersion    uint64                  `json:"context_version"`
	ContextTailID     string                  `json:"context_tail_id,omitempty"`
	// ContextPrefix binds the immutable input independently of later appends.
	// It is provenance for retaining speech history, never fresh action authority.
	ContextPrefix trajectory.PrefixIdentity `json:"context_prefix,omitzero"`
	Invocation    continuation.Invocation   `json:"invocation"`
	Outputs       []PreparedOutput          `json:"outputs,omitempty"`
	AssistantText string                    `json:"assistant_text,omitempty"`
	ReasoningText string                    `json:"reasoning_text,omitempty"`
	// ReasoningRetained attests that every textual reasoning delta emitted by
	// the provider is present in Outputs and ReasoningText. False is the
	// fail-closed default: an opaque native provider state can then contain
	// model-authored text that no downstream content boundary was able to
	// inspect.
	ReasoningRetained bool                    `json:"reasoning_retained,omitempty"`
	ToolProposals     []ToolProposal          `json:"tool_proposals,omitempty"`
	Completion        continuation.Completion `json:"completion"`
	Interrupted       bool                    `json:"interrupted,omitempty"`
}

func (Result) InspectionCause() element.InspectionCauseKind {
	return element.CauseModelRun
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeIgnored   OutcomeKind = "ignored"
)

// Outcome is the closed terminal control result for generation and interrupt
// requests. Latency is empirical runtime evidence, not part of port typing.
type Outcome struct {
	Kind              OutcomeKind `json:"kind"`
	Operation         string      `json:"operation"`
	RunID             string      `json:"run_id,omitempty"`
	ProviderReference string      `json:"provider_reference,omitempty"`
	ContextVersion    uint64      `json:"context_version,omitempty"`
	Code              string      `json:"code,omitempty"`
	Message           string      `json:"message,omitempty"`
	StartedNS         uint64      `json:"started_ns,omitempty"`
	FinishedNS        uint64      `json:"finished_ns,omitempty"`
	DurationNS        uint64      `json:"duration_ns,omitempty"`
}

func (Outcome) InspectionCause() element.InspectionCauseKind {
	return element.CauseModelRun
}

var (
	_ element.InspectionCauseProvider = Generate{}
	_ element.InspectionCauseProvider = PreparedTextDelta{}
	_ element.InspectionCauseProvider = ToolProposal{}
	_ element.InspectionCauseProvider = Result{}
	_ element.InspectionCauseProvider = Outcome{}
)

// ProviderResolution is live startup evidence. DescriptorDigest covers the
// exact descriptor returned by the created provider instance; RegistryRevision
// identifies the deployment service revision from which it was resolved.
type ProviderResolution struct {
	Reference        string                  `json:"reference"`
	Descriptor       continuation.Descriptor `json:"descriptor"`
	DescriptorDigest string                  `json:"descriptor_digest"`
	RegistryRevision uint64                  `json:"registry_revision"`
}

type ProviderFactory func() (continuation.Provider, error)

type providerEntry struct {
	descriptor continuation.Descriptor
	factory    ProviderFactory
}

// ProviderRegistry is immutable-by-registration deployment state. Multiple
// TextModel nodes may independently select distinct symbolic providers.
type ProviderRegistry struct {
	mu      sync.RWMutex
	entries map[string]providerEntry
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{entries: make(map[string]providerEntry)}
}

func (registry *ProviderRegistry) Register(
	reference string, descriptor continuation.Descriptor, factory ProviderFactory,
) error {
	if registry == nil {
		return errors.New("register cognition provider: nil registry")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register cognition provider: empty reference")
	}
	if err := continuation.ValidateDescriptor(descriptor); err != nil {
		return fmt.Errorf("register cognition provider %q: %w", reference, err)
	}
	if factory == nil {
		return fmt.Errorf("register cognition provider %q: nil factory", reference)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]providerEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("cognition provider %q is already registered", reference)
	}
	registry.entries[reference] = providerEntry{descriptor: descriptor, factory: factory}
	return nil
}

func (registry *ProviderRegistry) resolve(reference string) (providerEntry, error) {
	if registry == nil {
		return providerEntry{}, errors.New("cognition provider registry is nil")
	}
	reference = strings.TrimSpace(reference)
	registry.mu.RLock()
	entry, found := registry.entries[reference]
	registry.mu.RUnlock()
	if !found {
		return providerEntry{}, fmt.Errorf("cognition provider %q is not registered", reference)
	}
	return entry, nil
}

func decodeTextModelConfig(source json.RawMessage) (TextModelConfig, error) {
	var config TextModelConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return TextModelConfig{}, err
	}
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" {
		return TextModelConfig{}, errors.New("text model config requires a provider reference")
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultMaxOutputBytes
	}
	if config.MaxEvents == 0 {
		config.MaxEvents = defaultMaxEvents
	}
	if config.MaxToolProposals == 0 {
		config.MaxToolProposals = defaultMaxToolProposals
	}
	if config.MaxOutputBytes < 1 || config.MaxOutputBytes > maximumOutputBytes {
		return TextModelConfig{}, fmt.Errorf("max_output_bytes must be between 1 and %d", maximumOutputBytes)
	}
	if config.MaxEvents < 1 || config.MaxEvents > maximumEvents {
		return TextModelConfig{}, fmt.Errorf("max_events must be between 1 and %d", maximumEvents)
	}
	if config.MaxToolProposals < 1 || config.MaxToolProposals > maximumToolProposals {
		return TextModelConfig{}, fmt.Errorf("max_tool_proposals must be between 1 and %d", maximumToolProposals)
	}
	return config, nil
}

func descriptorDigest(descriptor continuation.Descriptor) (string, error) {
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return "", fmt.Errorf("encode continuation descriptor: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func createProvider(reference string, entry providerEntry) (continuation.Provider, error) {
	provider, err := entry.factory()
	if err != nil {
		return nil, fmt.Errorf("create cognition provider %q: %w", reference, err)
	}
	if provider == nil || reflectedNil(provider) {
		return nil, fmt.Errorf("cognition provider %q factory returned nil", reference)
	}
	actual := provider.Descriptor()
	if err := continuation.ValidateDescriptor(actual); err != nil {
		failure := fmt.Errorf("cognition provider %q returned an invalid descriptor: %w", reference, err)
		return nil, errors.Join(failure, providerCloseFailure(reference, provider))
	}
	if !reflect.DeepEqual(actual, entry.descriptor) {
		failure := fmt.Errorf("cognition provider %q descriptor drifted: registered %+v, live %+v",
			reference, entry.descriptor, actual)
		return nil, errors.Join(failure, providerCloseFailure(reference, provider))
	}
	return provider, nil
}

func providerCloseFailure(reference string, provider continuation.Provider) error {
	if err := closeProvider(provider); err != nil {
		return fmt.Errorf("close cognition provider %q after failed startup: %w", reference, err)
	}
	return nil
}

func closeProvider(provider continuation.Provider) error {
	if provider == nil || reflectedNil(provider) {
		return nil
	}
	if closer, ok := provider.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func reflectedNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// providerHandle lets lifecycle disposal own a provider created during Run.
// take-before-close guarantees exactly one Close call.
type providerHandle struct {
	mu       sync.Mutex
	provider continuation.Provider
}

func (handle *providerHandle) set(provider continuation.Provider) error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.provider != nil {
		return errors.New("cognition provider handle is already initialized")
	}
	handle.provider = provider
	return nil
}

func (handle *providerHandle) close(context.Context) error {
	handle.mu.Lock()
	provider := handle.provider
	handle.provider = nil
	handle.mu.Unlock()
	return closeProvider(provider)
}

func Descriptors() []element.Descriptor { return []element.Descriptor{TextModelDescriptor()} }

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register cognition descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register cognition factories: nil registry")
	}
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	return registry.RegisterFactory(registrations[0])
}

func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	return factoryprofile.Registrations(factoryprofile.Entry{
		Factory: textModelFactory{}, Artifact: inspect.ArtifactIdentity{
			ID: textModelRuntimeID, Revision: cognitionImplementationRevision,
		},
	})
}
