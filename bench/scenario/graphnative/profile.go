package graphnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	// ApplicationReference is the production host key for the scenario-suite
	// decorator. The delegate remains an independently registered application;
	// a strict launch profile selects this reference only when it requires the
	// reviewed scenario contract around that delegate.
	ApplicationReference = "application.openrealtime.scenario-suite.v1"

	// ApplicationConfigurationFormatVersion identifies the scenario suite's
	// plugin-owned portion of a generic graph launch profile.
	ApplicationConfigurationFormatVersion uint64 = 1
	maximumApplicationConfigurationBytes         = 4 << 20
)

// ApplicationPluginConfig describes one scenario-suite application plugin.
//
// The plugin is a decorator over another exact application registration. Its
// own Reference and Artifact identify the scenario contract enforcement code;
// Delegate identifies the application that supplies the graph, providers, and
// adapter. The resulting registration retains Delegate.ProviderArtifact so
// the generic profile resolver can attest the installed SessionProvider bytes.
type ApplicationPluginConfig struct {
	Reference string
	Artifact  inspect.ArtifactIdentity
	Delegate  launchprofile.Registration
}

// LaunchProfileConfig freezes a generic graph/server profile for one exact
// scenario contract. Configuration must be produced by
// FreezeApplicationConfiguration for Application.
//
// Every executable identity is caller supplied and subsequently exact-matched
// by graph/launch/profile and server.NewProfileGraphBundle. The plan and
// adapter identities are derived from the selected application's real
// resource-free launch config; this helper never invents runtime digests.
type LaunchProfileConfig struct {
	Name          string
	Revision      uint64
	Contract      Contract
	Application   launchprofile.Registration
	Configuration json.RawMessage
	Server        launchprofile.Server
}

type delegatedApplication struct {
	Reference        string                   `json:"reference"`
	Artifact         inspect.ArtifactIdentity `json:"artifact"`
	ProviderArtifact inspect.ArtifactIdentity `json:"provider_artifact"`
	Configuration    json.RawMessage          `json:"configuration"`
}

type applicationConfiguration struct {
	FormatVersion       uint64               `json:"format_version"`
	ContractFingerprint string               `json:"contract_fingerprint"`
	Cases               []string             `json:"cases"`
	Delegate            delegatedApplication `json:"delegate"`
}

// NewApplicationPlugin builds the everything-as-a-plugin scenario decorator.
// Construction only validates and snapshots registrations. Its factory strict
// decodes the profile-owned data, reconstructs the exact repository contract,
// invokes the resource-free delegate factory, and installs GuardLaunch before
// the generic launcher can bind an adapter.
func NewApplicationPlugin(config ApplicationPluginConfig) (launchprofile.Registration, error) {
	delegate := config.Delegate
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{delegate}); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("scenario application delegate: %w", err)
	}
	if config.Reference == delegate.Reference {
		return launchprofile.Registration{}, errors.New(
			"scenario application plugin reference must differ from its delegate",
		)
	}
	registration := launchprofile.Registration{
		Reference:        config.Reference,
		Artifact:         config.Artifact,
		ProviderArtifact: delegate.ProviderArtifact,
	}
	registration.Factory = func(
		ctx context.Context, source json.RawMessage,
	) (graphlaunch.Config, error) {
		if ctx == nil {
			return graphlaunch.Config{}, errors.New("prepare scenario application: nil context")
		}
		if err := context.Cause(ctx); err != nil {
			return graphlaunch.Config{}, err
		}
		contract, parsed, err := decodeApplicationConfiguration(source)
		if err != nil {
			return graphlaunch.Config{}, fmt.Errorf("prepare scenario application configuration: %w", err)
		}
		if parsed.Delegate.Reference != delegate.Reference ||
			parsed.Delegate.Artifact != delegate.Artifact ||
			parsed.Delegate.ProviderArtifact != delegate.ProviderArtifact {
			return graphlaunch.Config{}, errors.New(
				"prepare scenario application: delegate plugin identity drifted",
			)
		}
		launch, err := delegate.Factory(ctx, bytes.Clone(parsed.Delegate.Configuration))
		if err != nil {
			return graphlaunch.Config{}, fmt.Errorf(
				"prepare scenario application delegate %q: %w", delegate.Reference, err,
			)
		}
		guarded, err := GuardLaunch(contract, launch)
		if err != nil {
			return graphlaunch.Config{}, fmt.Errorf("prepare scenario application guard: %w", err)
		}
		return guarded, nil
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{registration}); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("scenario application plugin: %w", err)
	}
	return registration, nil
}

// FreezeApplicationConfiguration creates the canonical plugin-owned data for
// one scenario contract and exact delegate registration. The complete case
// list is always materialized, including for the reviewed full suite, so a
// checked profile never relies on an empty-list convention.
func FreezeApplicationConfiguration(
	contract Contract,
	delegate launchprofile.Registration,
	delegateConfiguration json.RawMessage,
) (json.RawMessage, error) {
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{delegate}); err != nil {
		return nil, fmt.Errorf("scenario application delegate: %w", err)
	}
	configuration, err := canonicalJSONObject(delegateConfiguration)
	if err != nil {
		return nil, fmt.Errorf("scenario application delegate configuration: %w", err)
	}
	cases := make([]string, len(contract.Cases))
	for index, item := range contract.Cases {
		cases[index] = item.Name
	}
	payload, err := json.Marshal(applicationConfiguration{
		FormatVersion:       ApplicationConfigurationFormatVersion,
		ContractFingerprint: contract.Fingerprint,
		Cases:               cases,
		Delegate: delegatedApplication{
			Reference: delegate.Reference, Artifact: delegate.Artifact,
			ProviderArtifact: delegate.ProviderArtifact,
			Configuration:    configuration,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode scenario application configuration: %w", err)
	}
	payload, err = canonicalJSONObject(payload)
	if err != nil {
		return nil, fmt.Errorf("canonicalize scenario application configuration: %w", err)
	}
	if len(payload) > maximumApplicationConfigurationBytes {
		return nil, fmt.Errorf(
			"scenario application configuration has %d bytes; maximum is %d",
			len(payload), maximumApplicationConfigurationBytes,
		)
	}
	return payload, nil
}

// FreezeLaunchProfile derives and freezes the generic launch profile for an
// exact scenario application. Application factory and graph launch preparation
// are metadata-only operations; provider clients, secrets, graph elements,
// session adapters, and listeners stay behind SessionProvider.Start/Mount.
func FreezeLaunchProfile(
	ctx context.Context, config LaunchProfileConfig,
) (launchprofile.Document, error) {
	if ctx == nil {
		return launchprofile.Document{}, errors.New("freeze scenario launch profile: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return launchprofile.Document{}, err
	}
	if err := config.Contract.Validate(); err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario launch profile contract: %w", err)
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{config.Application}); err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario launch profile application: %w", err)
	}
	if config.Application.ProviderArtifact != config.Server.ProviderArtifact {
		return launchprofile.Document{}, errors.New(
			"freeze scenario launch profile: application provider artifact differs from server selection",
		)
	}
	contract, _, err := decodeApplicationConfiguration(config.Configuration)
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario launch profile configuration: %w", err)
	}
	if !reflect.DeepEqual(contract, config.Contract) {
		return launchprofile.Document{}, errors.New(
			"freeze scenario launch profile: application contract differs from selected contract",
		)
	}
	launch, err := config.Application.Factory(ctx, bytes.Clone(config.Configuration))
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario launch profile application: %w", err)
	}
	preview, err := graphlaunch.New(ctx, launch)
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario launch profile preview: %w", err)
	}
	if preview.Plan == nil || preview.Binding == nil {
		return launchprofile.Document{}, errors.New(
			"freeze scenario launch profile preview omitted plan or provider",
		)
	}
	document, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          config.Name,
		Revision:      config.Revision,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: config.Application.Reference, Artifact: config.Application.Artifact,
			},
			Configuration: bytes.Clone(config.Configuration),
		},
		Plan: preview.Plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference: launch.Adapter.Reference, RuntimeArtifact: launch.Adapter.RuntimeArtifact,
			ProfileName: launch.Adapter.ProfileName, ProfileRevision: launch.Adapter.ProfileRevision,
		},
		Server: config.Server,
	})
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario launch profile: %w", err)
	}
	return document, nil
}

func decodeApplicationConfiguration(
	source json.RawMessage,
) (Contract, applicationConfiguration, error) {
	canonical, err := canonicalJSONObject(source)
	if err != nil {
		return Contract{}, applicationConfiguration{}, err
	}
	if !bytes.Equal(canonical, source) {
		return Contract{}, applicationConfiguration{}, errors.New("configuration is not canonical JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var parsed applicationConfiguration
	if err := decoder.Decode(&parsed); err != nil {
		return Contract{}, applicationConfiguration{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Contract{}, applicationConfiguration{}, err
	}
	if parsed.FormatVersion != ApplicationConfigurationFormatVersion {
		return Contract{}, applicationConfiguration{}, fmt.Errorf(
			"configuration uses format %d, want %d",
			parsed.FormatVersion, ApplicationConfigurationFormatVersion,
		)
	}
	contract, err := BuildContract(parsed.Cases...)
	if err != nil {
		return Contract{}, applicationConfiguration{}, fmt.Errorf("configuration cases: %w", err)
	}
	names := make([]string, len(contract.Cases))
	for index, item := range contract.Cases {
		names[index] = item.Name
	}
	if !slices.Equal(parsed.Cases, names) {
		return Contract{}, applicationConfiguration{}, errors.New(
			"configuration cases are not in canonical suite order",
		)
	}
	if parsed.ContractFingerprint != contract.Fingerprint {
		return Contract{}, applicationConfiguration{}, fmt.Errorf(
			"configuration contract fingerprint is %q, want %q",
			parsed.ContractFingerprint, contract.Fingerprint,
		)
	}
	delegateSentinel := launchprofile.Registration{
		Reference: parsed.Delegate.Reference, Artifact: parsed.Delegate.Artifact,
		ProviderArtifact: parsed.Delegate.ProviderArtifact,
		Factory: func(context.Context, json.RawMessage) (graphlaunch.Config, error) {
			return graphlaunch.Config{}, errors.New("scenario delegate validation sentinel must not be called")
		},
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{delegateSentinel}); err != nil {
		return Contract{}, applicationConfiguration{}, fmt.Errorf("configuration delegate: %w", err)
	}
	delegateConfiguration, err := canonicalJSONObject(parsed.Delegate.Configuration)
	if err != nil {
		return Contract{}, applicationConfiguration{}, fmt.Errorf("configuration delegate data: %w", err)
	}
	if !bytes.Equal(delegateConfiguration, parsed.Delegate.Configuration) {
		return Contract{}, applicationConfiguration{}, errors.New(
			"configuration delegate data is not canonical JSON",
		)
	}
	parsed.Cases = slices.Clone(parsed.Cases)
	parsed.Delegate.Configuration = bytes.Clone(parsed.Delegate.Configuration)
	return contract, parsed, nil
}

func canonicalJSONObject(source []byte) ([]byte, error) {
	if len(source) == 0 {
		return nil, errors.New("JSON object is required")
	}
	if len(source) > maximumApplicationConfigurationBytes {
		return nil, fmt.Errorf("JSON object has %d bytes; maximum is %d",
			len(source), maximumApplicationConfigurationBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON object: %w", err)
	}
	if value == nil {
		return nil, errors.New("JSON value must be an object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON object: %w", err)
	}
	if len(payload) > maximumApplicationConfigurationBytes {
		return nil, fmt.Errorf("canonical JSON object has %d bytes; maximum is %d",
			len(payload), maximumApplicationConfigurationBytes)
	}
	return payload, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
