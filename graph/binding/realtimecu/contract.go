// Package realtimecu adapts the stable OpenAI Realtime-compatible session
// surface to the locked graph-native Realtime-CU authority graph.
//
// It contains no model, ASR, vision, browser, or UI implementation. Those are
// exact plugin registrations supplied by a launcher. The adapter only
// translates session operations to typed graph boundaries and renders typed
// graph outputs back through binding.Sink.
package realtimecu

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
)

const (
	AdapterReference = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/session-adapter/v1"
	ProfileName      = "openrealtime.realtime_computer_use"
	ProfileRevision  = uint64(1)
	ModelReference   = "deployment.computer-use"
	// SettlementPolicyReference is the graph-stable semantic-decider
	// registration used by an independently connected intent-disposition
	// producer. The application profile's host inventory reference remains a
	// separate exact identity and is never interpreted by graph elements.
	SettlementPolicyReference = "deployment.computer-use.settlement-policy"
	ToolReference             = "deployment.tools"
	TargetReference           = "deployment.browser"
	LedgerReference           = "deployment.ledger"
	ConfirmReference          = "deployment.confirmation"

	SourceMicrophone = "microphone"
	SourceScreen     = "screen"
	SourceCamera     = "camera"
)

// ModelPlugin is one exact provider registration selected for the graph's
// cognition.TextModel node. Factory is invoked once per mounted session.
type ModelPlugin struct {
	Reference  string
	Artifact   inspect.ArtifactIdentity
	Descriptor continuation.Descriptor
	Factory    func(context.Context, legacy.Options) (continuation.Provider, error)
}

// PolicyPlugin is the exact, independently owned semantic classifier selected
// for post-effect intent disposition. It is intentionally distinct from the
// continuation model even when both factories address the same deployment:
// each SemanticDeciderRegistry.Open call obtains a fresh client whose caller
// owns its lifecycle.
type PolicyPlugin struct {
	Reference  string
	Artifact   inspect.ArtifactIdentity
	Descriptor policyelements.SemanticDeciderDescriptor
	Factory    func(context.Context, legacy.Options) (policyelements.SemanticDecider, error)
}

// Observer converts protocol media frames into typed observations. Audio
// observations must carry user authority; screen/camera observations must
// carry observer authority. Returning no observation is the normal cadence or
// endpoint decision and is not an error.
type Observer interface {
	Audio(context.Context, perception.Frame) ([]perception.Observation, error)
	Video(context.Context, perception.Frame) ([]perception.Observation, error)
	// Consequence is delivered only after a client effect result reaches the
	// graph's canonical ToolResultCommit safe point. Implementations use it to
	// force the next screen observation even when visual change detection would
	// otherwise collapse an unchanged frame.
	Consequence(context.Context, VisualConsequence) error
	Close() error
}

// VisualConsequence asks the observer to sample the bounded action target
// after one graph-authorized effect. It deliberately carries no model text or
// tool output; the canonical trajectory remains the source of that data.
type VisualConsequence struct {
	CallID                string `json:"call_id"`
	Name                  string `json:"name"`
	CanonicalResultItemID string `json:"canonical_result_item_id"`
	TargetSource          string `json:"target_source"`
	StoreVersion          uint64 `json:"store_version"`
}

// ObserverPlugin is one exact audiovisual observer registration. Sources are
// the complete source vocabulary accepted by the provider. Realtime-CU
// requires microphone, screen, and camera even though camera is not an action
// target.
type ObserverPlugin struct {
	Reference       string
	Name            string
	Artifact        inspect.ArtifactIdentity
	Sources         []string
	Factory         func(context.Context, legacy.Options) (Observer, error)
	ResourceFactory func(context.Context, legacy.Options, ObserverResources) (Observer, error)
}

// ObserverResources are session-scoped graph services an observer may use.
// The profile selects only the observer artifact; handles and retained bytes
// never enter serializable configuration. Retainer is shared with the graph's
// cognition media resolver, so an attached keyframe is the exact byte payload
// the selected continuation provider receives.
type ObserverResources struct {
	Retainer perception.Retainer
}

// PluginConfig supplies exact runtime/provider identities and the one bounded
// browser target. RuntimeArtifact identifies the adapter and mount-service
// executable material; model and observer artifacts remain separately bound
// into the dependency profile digest.
type PluginConfig struct {
	RuntimeArtifact  inspect.ArtifactIdentity
	Model            ModelPlugin
	SettlementPolicy PolicyPlugin
	Observer         ObserverPlugin
	Target           computeruse.Target
}

func validatePluginConfig(config PluginConfig) error {
	if err := config.RuntimeArtifact.Validate(); err != nil {
		return fmt.Errorf("realtime-CU runtime artifact: %w", err)
	}
	if err := config.Model.Artifact.Validate(); err != nil {
		return fmt.Errorf("realtime-CU model artifact: %w", err)
	}
	if !canonical(config.Model.Reference) {
		return errors.New("realtime-CU model plugin requires a canonical reference")
	}
	if err := continuation.ValidateDescriptor(config.Model.Descriptor); err != nil {
		return fmt.Errorf("realtime-CU model descriptor: %w", err)
	}
	if config.Model.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
		return errors.New("realtime-CU graph model must have proposal-only tool authority")
	}
	if config.Model.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		return errors.New("realtime-CU graph model must be silent; presentation is a separate client plugin")
	}
	if config.Model.Factory == nil {
		return errors.New("realtime-CU model plugin requires a factory")
	}
	if !canonical(config.SettlementPolicy.Reference) || config.SettlementPolicy.Factory == nil {
		return errors.New("realtime-CU settlement policy plugin requires a canonical reference and factory")
	}
	if config.SettlementPolicy.Reference != SettlementPolicyReference {
		return fmt.Errorf(
			"realtime-CU settlement policy reference %q, want exact graph selection %q",
			config.SettlementPolicy.Reference, SettlementPolicyReference,
		)
	}
	if err := config.SettlementPolicy.Artifact.Validate(); err != nil {
		return fmt.Errorf("realtime-CU settlement policy artifact: %w", err)
	}
	if err := config.SettlementPolicy.Descriptor.Validate(); err != nil {
		return fmt.Errorf("realtime-CU settlement policy descriptor: %w", err)
	}
	if err := config.Observer.Artifact.Validate(); err != nil {
		return fmt.Errorf("realtime-CU observer artifact: %w", err)
	}
	if !canonical(config.Observer.Reference) || !canonical(config.Observer.Name) {
		return errors.New("realtime-CU observer plugin requires a canonical reference and name")
	}
	if (config.Observer.Factory == nil) == (config.Observer.ResourceFactory == nil) {
		return errors.New("realtime-CU observer plugin requires exactly one plain or resource-aware factory")
	}
	sources := canonicalStrings(config.Observer.Sources)
	for _, source := range config.Observer.Sources {
		if !canonical(source) {
			return errors.New("realtime-CU observer sources must be canonical")
		}
	}
	if len(sources) != len(config.Observer.Sources) ||
		!slices.Equal(sources, []string{SourceCamera, SourceMicrophone, SourceScreen}) {
		return fmt.Errorf("realtime-CU observer sources = %v, want camera, microphone, and screen", sources)
	}
	if err := config.Target.Validate(); err != nil {
		return fmt.Errorf("realtime-CU browser target: %w", err)
	}
	if !slices.Equal(config.Target.Sources, []string{SourceScreen}) {
		return fmt.Errorf("realtime-CU action target sources = %v, want only screen", config.Target.Sources)
	}
	if !canonical(config.Target.Name) {
		return errors.New("realtime-CU browser target requires a canonical name")
	}
	return nil
}

func validateProvider(plugin ModelPlugin, provider continuation.Provider) error {
	if provider == nil || reflectedNil(provider) {
		return errors.New("realtime-CU model factory returned nil")
	}
	actual := provider.Descriptor()
	if err := continuation.ValidateDescriptor(actual); err != nil {
		return fmt.Errorf("realtime-CU live model descriptor: %w", err)
	}
	if !reflect.DeepEqual(actual, plugin.Descriptor) {
		return fmt.Errorf("realtime-CU live model descriptor drifted from exact registration")
	}
	return nil
}

func reflectedNil(value any) bool {
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

func canonical(value string) bool {
	return value != "" && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func canonicalStrings(source []string) []string {
	result := slices.Clone(source)
	for index := range result {
		result[index] = strings.TrimSpace(result[index])
	}
	slices.Sort(result)
	return slices.Compact(result)
}
