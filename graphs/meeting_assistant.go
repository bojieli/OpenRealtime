package graphs

import (
	"embed"
	"errors"
	"fmt"
	"slices"
	"time"

	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	meetinggraph "github.com/bojieli/OpenRealtime/meeting/graphnative"
)

//go:embed components/meeting-assistant/agent.ortg
//go:embed components/meeting-assistant/agent.values.yaml
//go:embed components/meeting-assistant/openrealtime.lock
//go:embed components/meeting-assistant/agent.deployment.yaml
//go:embed components/meeting-assistant/agent.secrets.yaml
//go:embed components/meeting-assistant/agent.evidence.yaml
var meetingAssistantArtifacts embed.FS

const meetingAssistantArtifactDirectory = "components/meeting-assistant/"

// MeetingAssistantRegistrationConfig is the exact executable/provider
// inventory installed by one host. The session plugin remains resource-free:
// its provider factories are invoked only after the selected graph mounts.
type MeetingAssistantRegistrationConfig struct {
	ApplicationArtifact inspect.ArtifactIdentity
	ProviderArtifact    inspect.ArtifactIdentity
	Session             meetinggraph.SessionPluginConfig
	Inspection          graphruntime.InspectionConfig
	ShutdownTimeout     time.Duration
	TraceRecording      *graphbinding.TraceRecordingConfig
	Readiness           []graphlaunch.ReadinessCheck
}

// MeetingAssistantRegistration exposes the generic application registration
// and the exact Realtime adapter selection that a strict profile must bind.
type MeetingAssistantRegistration struct {
	Application launchprofile.Registration
	Adapter     graphlaunch.AdapterSelection
}

// MeetingAssistantArtifacts returns independent repository-owned graph
// artifacts and their empty secret catalog. Provider implementations and
// presentation layers are deliberately absent from this bundle.
func MeetingAssistantArtifacts() (graphconfig.Artifacts, *graphsecret.Document, error) {
	read := func(name string) ([]byte, error) {
		payload, err := meetingAssistantArtifacts.ReadFile(meetingAssistantArtifactDirectory + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded Meeting Assistant %s: %w", name, err)
		}
		return slices.Clone(payload), nil
	}
	topology, err := read("agent.ortg")
	if err != nil {
		return graphconfig.Artifacts{}, nil, err
	}
	values, err := read("agent.values.yaml")
	if err != nil {
		return graphconfig.Artifacts{}, nil, err
	}
	lock, err := read("openrealtime.lock")
	if err != nil {
		return graphconfig.Artifacts{}, nil, err
	}
	deployment, err := read("agent.deployment.yaml")
	if err != nil {
		return graphconfig.Artifacts{}, nil, err
	}
	secretPayload, err := read("agent.secrets.yaml")
	if err != nil {
		return graphconfig.Artifacts{}, nil, err
	}
	secrets, err := graphsecret.ParseYAML("meeting-assistant/agent.secrets.yaml", secretPayload)
	if err != nil {
		return graphconfig.Artifacts{}, nil, fmt.Errorf("parse embedded Meeting Assistant secrets: %w", err)
	}
	return graphconfig.Artifacts{
		Topology: graphconfig.Artifact{
			Path: "meeting-assistant/agent.ortg", Encoding: graphconfig.ORTG, Data: topology,
		},
		Values: graphconfig.Artifact{
			Path: "meeting-assistant/agent.values.yaml", Encoding: graphconfig.YAML, Data: values,
		},
		Lock: graphconfig.Artifact{
			Path: "meeting-assistant/openrealtime.lock", Encoding: graphconfig.JSON, Data: lock,
		},
		Deployment: graphconfig.Artifact{
			Path: "meeting-assistant/agent.deployment.yaml", Encoding: graphconfig.YAML, Data: deployment,
		},
	}, &secrets, nil
}

// MeetingAssistantEvidence returns the separate empirical claims manifest
// shipped with the graph. It contains no executable or credential material.
func MeetingAssistantEvidence() (graphevidence.Document, error) {
	payload, err := meetingAssistantArtifacts.ReadFile(
		meetingAssistantArtifactDirectory + "agent.evidence.yaml",
	)
	if err != nil {
		return graphevidence.Document{}, fmt.Errorf(
			"read embedded Meeting Assistant agent.evidence.yaml: %w", err,
		)
	}
	return graphevidence.ParseYAML("meeting-assistant/agent.evidence.yaml", payload)
}

// MeetingAssistantApplicationRegistration installs one direct graph-native
// application behind the common launch-profile/server API. It neither opens a
// provider nor introduces an application-specific listener or wire route.
func MeetingAssistantApplicationRegistration(
	config MeetingAssistantRegistrationConfig,
) (MeetingAssistantRegistration, error) {
	if err := config.ApplicationArtifact.Validate(); err != nil {
		return MeetingAssistantRegistration{}, fmt.Errorf("Meeting Assistant application artifact: %w", err)
	}
	if err := config.ProviderArtifact.Validate(); err != nil {
		return MeetingAssistantRegistration{}, fmt.Errorf("Meeting Assistant provider artifact: %w", err)
	}
	plugin, err := meetinggraph.NewSessionPlugin(config.Session)
	if err != nil {
		return MeetingAssistantRegistration{}, err
	}
	artifacts, secrets, err := MeetingAssistantArtifacts()
	if err != nil {
		return MeetingAssistantRegistration{}, err
	}
	evidence, err := MeetingAssistantEvidence()
	if err != nil {
		return MeetingAssistantRegistration{}, err
	}
	adapterConfig := plugin.AdapterConfig()
	_, adapter, err := meetinggraph.AdapterPlugin(adapterConfig)
	if err != nil {
		return MeetingAssistantRegistration{}, err
	}
	dependencies := plugin.AssemblyDependencies()
	if len(dependencies) == 0 {
		return MeetingAssistantRegistration{}, errors.New("Meeting Assistant session plugin declared no dependencies")
	}
	catalog := graphlaunch.Catalog{
		Assembly:          graphassembly.Catalog{Dependencies: dependencies},
		MountDependencies: plugin.MountDependencies(),
	}
	registration, err := meetinggraph.NewApplicationRegistration(meetinggraph.ApplicationHostConfig{
		ApplicationArtifact: config.ApplicationArtifact, ProviderArtifact: config.ProviderArtifact,
		Artifacts: artifacts, Plugins: catalog, SecretCatalog: secrets, Evidence: evidence,
		GraphMetadata: graphcatalog.Metadata{
			Stage:   graphcatalog.Candidate,
			Summary: "Graph-native multimodal Meeting Assistant application.",
			Change:  "Initial exact production graph and configuration revision.",
			Tags:    []string{"audio", "computer-use", "meeting", "multimodal"},
		},
		Adapters:   []meetinggraph.AdapterPluginConfig{adapterConfig},
		Inspection: config.Inspection, ShutdownTimeout: config.ShutdownTimeout,
		TraceRecording: config.TraceRecording,
		Readiness:      slices.Clone(config.Readiness),
	})
	if err != nil {
		return MeetingAssistantRegistration{}, err
	}
	return MeetingAssistantRegistration{Application: registration, Adapter: adapter}, nil
}

// MeetingAssistantApplicationConfig returns the strict, serializable
// application document for this repository-owned bundle. Selecting the shared
// trajectory store is explicit because it is the proof that the Realtime
// adapter and graph inspection expose the exact same canonical trajectory.
func MeetingAssistantApplicationConfig(
	registration MeetingAssistantRegistration, revision uint64,
) (meetinggraph.ApplicationConfig, error) {
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{registration.Application}); err != nil {
		return meetinggraph.ApplicationConfig{}, err
	}
	if registration.Application.Reference != meetinggraph.ApplicationReference {
		return meetinggraph.ApplicationConfig{}, errors.New("Meeting Assistant registration reference drifted")
	}
	if registration.Adapter.Reference == "" || registration.Adapter.ProfileName == "" ||
		registration.Adapter.ProfileRevision == 0 {
		return meetinggraph.ApplicationConfig{}, errors.New("Meeting Assistant adapter selection is incomplete")
	}
	if err := registration.Adapter.RuntimeArtifact.Validate(); err != nil {
		return meetinggraph.ApplicationConfig{}, fmt.Errorf("Meeting Assistant adapter artifact: %w", err)
	}
	if revision == 0 {
		return meetinggraph.ApplicationConfig{}, errors.New("Meeting Assistant plan revision must be positive")
	}
	return meetinggraph.ApplicationConfig{
		FormatVersion: meetinggraph.ApplicationFormatVersion,
		Artifacts: meetinggraph.ApplicationArtifactPaths{
			Topology:   "meeting-assistant/agent.ortg",
			Values:     "meeting-assistant/agent.values.yaml",
			Lock:       "meeting-assistant/openrealtime.lock",
			Deployment: "meeting-assistant/agent.deployment.yaml",
		},
		Plan: meetinggraph.ApplicationPlanSelection{
			Revision:             revision,
			OptionalDependencies: []string{stateelements.TrajectoryStoreService},
		},
		Adapter: launchprofile.AdapterSelection{
			Reference:       registration.Adapter.Reference,
			RuntimeArtifact: registration.Adapter.RuntimeArtifact,
			ProfileName:     registration.Adapter.ProfileName,
			ProfileRevision: registration.Adapter.ProfileRevision,
		},
	}, nil
}
