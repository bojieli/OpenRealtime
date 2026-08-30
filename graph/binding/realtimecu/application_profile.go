package realtimecu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	ApplicationReference          = "application.openrealtime.realtime-computer-use.v1"
	ApplicationFormatVersion      = uint64(1)
	maximumApplicationConfigBytes = 1 << 20
)

// ApplicationModelSelection is the serializable, exact model plugin identity
// pinned by a graph-launch profile. Reference is opaque to the graph; the host
// registry exact-matches it before supplying a factory under the graph's fixed
// deployment.computer-use service reference.
type ApplicationModelSelection struct {
	Reference  string                   `json:"reference"`
	Artifact   inspect.ArtifactIdentity `json:"artifact"`
	Descriptor continuation.Descriptor  `json:"descriptor"`
}

// ApplicationObserverSelection pins the audiovisual observer without placing
// a factory, device handle, credential, or provider client in profile data.
type ApplicationObserverSelection struct {
	Reference string                   `json:"reference"`
	Name      string                   `json:"name"`
	Artifact  inspect.ArtifactIdentity `json:"artifact"`
	Sources   []string                 `json:"sources"`
}

// ApplicationConfig is the complete Realtime-CU-owned portion of a strict
// graph-launch profile. All eleven computer-use tools, the deny-unrequested
// confirmer, client dispatcher, ledger, and trajectory store are derived by
// RealtimeComputerUseLaunchConfig and are deliberately not mutable selectors.
type ApplicationConfig struct {
	FormatVersion uint64                       `json:"format_version"`
	Model         ApplicationModelSelection    `json:"model"`
	Observer      ApplicationObserverSelection `json:"observer"`
	Target        computeruse.Target           `json:"target"`
}

// DecodeApplicationConfig rejects duplicate/unknown fields and validates all
// exact public identities. It performs no environment lookup, secret
// resolution, factory call, graph mount, or other resource acquisition.
func DecodeApplicationConfig(source json.RawMessage) (ApplicationConfig, error) {
	if len(source) == 0 || len(source) > maximumApplicationConfigBytes {
		return ApplicationConfig{}, fmt.Errorf("Realtime-CU application configuration has %d bytes; expected 1..%d",
			len(source), maximumApplicationConfigBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return ApplicationConfig{}, fmt.Errorf("Realtime-CU application configuration: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var config ApplicationConfig
	if err := decoder.Decode(&config); err != nil {
		return ApplicationConfig{}, fmt.Errorf("decode Realtime-CU application configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ApplicationConfig{}, errors.New("Realtime-CU application configuration has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return ApplicationConfig{}, fmt.Errorf("decode trailing Realtime-CU application configuration: %w", err)
	}
	config.Observer.Sources = slices.Clone(config.Observer.Sources)
	if err := config.Validate(); err != nil {
		return ApplicationConfig{}, err
	}
	return config, nil
}

// Validate proves the serializable plugin selections without consulting an
// installed factory registry.
func (config ApplicationConfig) Validate() error {
	if config.FormatVersion != ApplicationFormatVersion {
		return fmt.Errorf("Realtime-CU application configuration uses format %d, want %d",
			config.FormatVersion, ApplicationFormatVersion)
	}
	if err := validateApplicationModel(config.Model); err != nil {
		return err
	}
	if err := validateApplicationObserver(config.Observer); err != nil {
		return err
	}
	if err := config.Target.Validate(); err != nil {
		return fmt.Errorf("Realtime-CU application target: %w", err)
	}
	if !canonical(config.Target.Name) || !slices.Equal(config.Target.Sources, []string{SourceScreen}) {
		return errors.New("Realtime-CU application target must be canonical and own only screen")
	}
	return nil
}

func validateApplicationModel(model ApplicationModelSelection) error {
	if !canonical(model.Reference) {
		return errors.New("Realtime-CU application model reference is not canonical")
	}
	if err := model.Artifact.Validate(); err != nil {
		return fmt.Errorf("Realtime-CU application model artifact: %w", err)
	}
	if err := continuation.ValidateDescriptor(model.Descriptor); err != nil {
		return fmt.Errorf("Realtime-CU application model descriptor: %w", err)
	}
	if model.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose ||
		model.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		return errors.New("Realtime-CU application model must be proposal-only and silent")
	}
	return nil
}

func validateApplicationObserver(observer ApplicationObserverSelection) error {
	if !canonical(observer.Reference) || !canonical(observer.Name) {
		return errors.New("Realtime-CU application observer reference and name must be canonical")
	}
	if err := observer.Artifact.Validate(); err != nil {
		return fmt.Errorf("Realtime-CU application observer artifact: %w", err)
	}
	sources := canonicalStrings(observer.Sources)
	if len(sources) != len(observer.Sources) ||
		!slices.Equal(sources, []string{SourceCamera, SourceMicrophone, SourceScreen}) {
		return errors.New("Realtime-CU application observer sources must be exactly camera, microphone, and screen")
	}
	for _, source := range observer.Sources {
		if !canonical(source) {
			return errors.New("Realtime-CU application observer source is not canonical")
		}
	}
	return nil
}

// ModelFactoryRegistration is one host-installed model factory. Its exact
// metadata must match ApplicationModelSelection before Factory is retained in
// a launch config; Factory itself remains unopened until session start.
type ModelFactoryRegistration struct {
	ApplicationModelSelection
	Factory   func(context.Context, legacy.Options) (continuation.Provider, error)
	Readiness func(context.Context) error
}

// ObserverFactoryRegistration is one host-installed audiovisual observer.
type ObserverFactoryRegistration struct {
	ApplicationObserverSelection
	Factory         func(context.Context, legacy.Options) (Observer, error)
	ResourceFactory func(context.Context, legacy.Options, ObserverResources) (Observer, error)
	Readiness       func(context.Context) error
}

// ApplicationRegistrationConfig installs the Realtime-CU application plugin
// into the generic strict profile registry. Every artifact is supplied by the
// host/build; this constructor never synthesizes an executable digest.
type ApplicationRegistrationConfig struct {
	ApplicationArtifact inspect.ArtifactIdentity
	ProviderArtifact    inspect.ArtifactIdentity
	RuntimeArtifact     inspect.ArtifactIdentity
	Models              []ModelFactoryRegistration
	Observers           []ObserverFactoryRegistration
}

// NewApplicationRegistration snapshots a broad host factory inventory and
// returns the one generic launch-profile registration for Realtime-CU.
func NewApplicationRegistration(
	source ApplicationRegistrationConfig,
	constructor func(PluginConfig) (graphlaunch.Config, error),
) (launchprofile.Registration, error) {
	if constructor == nil {
		return launchprofile.Registration{}, errors.New("Realtime-CU application registration requires a launch constructor")
	}
	if err := source.ApplicationArtifact.Validate(); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("Realtime-CU application artifact: %w", err)
	}
	if err := source.ProviderArtifact.Validate(); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("Realtime-CU provider artifact: %w", err)
	}
	if err := source.RuntimeArtifact.Validate(); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("Realtime-CU runtime artifact: %w", err)
	}
	if len(source.Models) == 0 || len(source.Observers) == 0 {
		return launchprofile.Registration{}, errors.New("Realtime-CU application registration requires model and observer factories")
	}
	models := make(map[string]ModelFactoryRegistration, len(source.Models))
	for index, registration := range source.Models {
		if err := validateApplicationModel(registration.ApplicationModelSelection); err != nil {
			return launchprofile.Registration{}, fmt.Errorf("Realtime-CU model registration %d: %w", index, err)
		}
		if registration.Factory == nil {
			return launchprofile.Registration{}, fmt.Errorf("Realtime-CU model registration %d has a nil factory", index)
		}
		if _, duplicate := models[registration.Reference]; duplicate {
			return launchprofile.Registration{}, fmt.Errorf("Realtime-CU model reference %q is registered more than once", registration.Reference)
		}
		models[registration.Reference] = registration
	}
	observers := make(map[string]ObserverFactoryRegistration, len(source.Observers))
	for index, registration := range source.Observers {
		if err := validateApplicationObserver(registration.ApplicationObserverSelection); err != nil {
			return launchprofile.Registration{}, fmt.Errorf("Realtime-CU observer registration %d: %w", index, err)
		}
		if (registration.Factory == nil) == (registration.ResourceFactory == nil) {
			return launchprofile.Registration{}, fmt.Errorf(
				"Realtime-CU observer registration %d requires exactly one plain or resource-aware factory", index,
			)
		}
		if _, duplicate := observers[registration.Reference]; duplicate {
			return launchprofile.Registration{}, fmt.Errorf("Realtime-CU observer reference %q is registered more than once", registration.Reference)
		}
		copy := registration
		copy.Sources = slices.Clone(registration.Sources)
		observers[registration.Reference] = copy
	}
	runtimeArtifact := source.RuntimeArtifact
	registration := launchprofile.Registration{
		Reference: ApplicationReference, Artifact: source.ApplicationArtifact,
		ProviderArtifact: source.ProviderArtifact,
		Factory: func(ctx context.Context, payload json.RawMessage) (graphlaunch.Config, error) {
			if ctx == nil {
				return graphlaunch.Config{}, errors.New("resolve Realtime-CU application: nil context")
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			config, err := DecodeApplicationConfig(payload)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			model, found := models[config.Model.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf("Realtime-CU model registry is missing %q", config.Model.Reference)
			}
			if model.Artifact != config.Model.Artifact ||
				model.Descriptor != config.Model.Descriptor {
				return graphlaunch.Config{}, fmt.Errorf("Realtime-CU model %q artifact or descriptor drifted", config.Model.Reference)
			}
			observer, found := observers[config.Observer.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf("Realtime-CU observer registry is missing %q", config.Observer.Reference)
			}
			if observer.Artifact != config.Observer.Artifact || observer.Name != config.Observer.Name ||
				!slices.Equal(observer.Sources, config.Observer.Sources) {
				return graphlaunch.Config{}, fmt.Errorf("Realtime-CU observer %q identity or source contract drifted", config.Observer.Reference)
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			modelFactory := model.Factory
			if model.Readiness != nil {
				selectedFactory, readiness := modelFactory, model.Readiness
				modelFactory = func(ctx context.Context, options legacy.Options) (continuation.Provider, error) {
					if err := readiness(ctx); err != nil {
						return nil, fmt.Errorf("Realtime-CU model %q is not ready: %w", model.Reference, err)
					}
					return selectedFactory(ctx, options)
				}
			}
			observerFactory := observer.Factory
			observerResourceFactory := observer.ResourceFactory
			if observer.Readiness != nil {
				readiness := observer.Readiness
				if observerFactory != nil {
					selectedFactory := observerFactory
					observerFactory = func(ctx context.Context, options legacy.Options) (Observer, error) {
						if err := readiness(ctx); err != nil {
							return nil, fmt.Errorf("Realtime-CU observer %q is not ready: %w", observer.Reference, err)
						}
						return selectedFactory(ctx, options)
					}
				} else {
					selectedFactory := observerResourceFactory
					observerResourceFactory = func(
						ctx context.Context, options legacy.Options, resources ObserverResources,
					) (Observer, error) {
						if err := readiness(ctx); err != nil {
							return nil, fmt.Errorf("Realtime-CU observer %q is not ready: %w", observer.Reference, err)
						}
						return selectedFactory(ctx, options, resources)
					}
				}
			}
			resolved, constructorErr := constructor(PluginConfig{
				RuntimeArtifact: runtimeArtifact,
				Model: ModelPlugin{
					Reference: model.Reference, Artifact: model.Artifact,
					Descriptor: model.Descriptor, Factory: modelFactory,
				},
				Observer: ObserverPlugin{
					Reference: observer.Reference, Name: observer.Name,
					Artifact: observer.Artifact, Sources: slices.Clone(observer.Sources),
					Factory: observerFactory, ResourceFactory: observerResourceFactory,
				},
				Target: config.Target,
			})
			if cause := context.Cause(ctx); cause != nil {
				return graphlaunch.Config{}, errors.Join(cause, constructorErr)
			}
			if constructorErr != nil {
				return graphlaunch.Config{}, constructorErr
			}
			if model.Readiness != nil {
				resolved.Readiness = append(resolved.Readiness, graphlaunch.ReadinessCheck{
					Name: "realtime-cu-model:" + model.Reference, Check: model.Readiness,
				})
			}
			if observer.Readiness != nil {
				resolved.Readiness = append(resolved.Readiness, graphlaunch.ReadinessCheck{
					Name: "realtime-cu-observer:" + observer.Reference, Check: observer.Readiness,
				})
			}
			return resolved, nil
		},
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{registration}); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("validate Realtime-CU application registration: %w", err)
	}
	return registration, nil
}
