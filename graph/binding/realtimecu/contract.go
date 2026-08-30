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
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
)

const (
	AdapterReference = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/session-adapter/v1"
	ProfileName      = "openrealtime.realtime_computer_use"
	ProfileRevision  = uint64(1)
	ModelReference   = "deployment.computer-use"
	ToolReference    = "deployment.tools"
	TargetReference  = "deployment.browser"
	LedgerReference  = "deployment.ledger"
	ConfirmReference = "deployment.confirmation"

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
	Reference string
	Name      string
	Artifact  inspect.ArtifactIdentity
	Sources   []string
	Factory   func(context.Context, legacy.Options) (Observer, error)
}

// PluginConfig supplies exact runtime/provider identities and the one bounded
// browser target. RuntimeArtifact identifies the adapter and mount-service
// executable material; model and observer artifacts remain separately bound
// into the dependency profile digest.
type PluginConfig struct {
	RuntimeArtifact inspect.ArtifactIdentity
	Model           ModelPlugin
	Observer        ObserverPlugin
	Target          computeruse.Target
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
	if err := config.Observer.Artifact.Validate(); err != nil {
		return fmt.Errorf("realtime-CU observer artifact: %w", err)
	}
	if !canonical(config.Observer.Reference) || !canonical(config.Observer.Name) ||
		config.Observer.Factory == nil {
		return errors.New("realtime-CU observer plugin requires a canonical reference, name, and factory")
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
