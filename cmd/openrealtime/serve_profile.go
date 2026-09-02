package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/bysentence"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	graphnative "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/gateway"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/internal/runtimeartifact"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/policymodel"
	"github.com/bojieli/OpenRealtime/providers"
	serverprofile "github.com/bojieli/OpenRealtime/server"
)

const (
	maximumServeLaunchProfileBytes = 8 << 20

	profileHostProcessArtifactID              = "go://openrealtime/openrealtime-process"
	profileHostScenarioSuiteArtifactID        = "go://github.com/bojieli/OpenRealtime/bench/scenario/graphnative/application/v1"
	profileHostScenarioConversationArtifactID = "go://github.com/bojieli/OpenRealtime/graphs/scenario-conversation/application/v1"
	profileHostScenarioProviderArtifactID     = "go://github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation/session-provider/v2"
	profileHostScenarioDependencyArtifactID   = "go://github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation/session-dependencies/v1"
)

// serveProfileArtifacts are host/build identities, never configuration
// fingerprints. Every identity is derived from the exact running executable;
// distinct IDs preserve which plugin role those same linked bytes serve.
type serveProfileArtifacts struct {
	Gateway              inspect.ArtifactIdentity
	ScenarioSuite        inspect.ArtifactIdentity
	ScenarioConversation inspect.ArtifactIdentity
	ScenarioProvider     inspect.ArtifactIdentity
	ScenarioRuntime      inspect.ArtifactIdentity
	ScenarioDependencies inspect.ArtifactIdentity
}

func executableServeProfileArtifacts() (serveProfileArtifacts, error) {
	process, err := runtimeartifact.Executable(profileHostProcessArtifactID)
	if err != nil {
		return serveProfileArtifacts{}, fmt.Errorf("identify launch-profile host executable: %w", err)
	}
	linked := func(id string) (inspect.ArtifactIdentity, error) {
		artifact := inspect.ArtifactIdentity{ID: id, Digest: process.Digest}
		if err := artifact.Validate(); err != nil {
			return inspect.ArtifactIdentity{}, fmt.Errorf("launch-profile host artifact %q: %w", id, err)
		}
		return artifact, nil
	}
	artifacts := serveProfileArtifacts{Gateway: process}
	fields := []struct {
		id     string
		target *inspect.ArtifactIdentity
	}{
		{profileHostScenarioSuiteArtifactID, &artifacts.ScenarioSuite},
		{profileHostScenarioConversationArtifactID, &artifacts.ScenarioConversation},
		{profileHostScenarioProviderArtifactID, &artifacts.ScenarioProvider},
		{scenarioconversation.AdapterReference, &artifacts.ScenarioRuntime},
		{profileHostScenarioDependencyArtifactID, &artifacts.ScenarioDependencies},
	}
	for _, field := range fields {
		artifact, err := linked(field.id)
		if err != nil {
			return serveProfileArtifacts{}, err
		}
		*field.target = artifact
	}
	return artifacts, nil
}

type serveScenarioProviders struct {
	ASR        []scenarioconversation.ASRFactoryRegistration
	Policies   []scenarioconversation.PolicyFactoryRegistration
	Models     []scenarioconversation.ModelFactoryRegistration
	TTS        []scenarioconversation.TTSFactoryRegistration
	Recogniser *asrbuffer.Accumulator
}

// newServeScenarioProviders publishes the whole linked provider catalogue.
// A profile selects a unique provider plugin and supplies every non-secret
// behavior knob as that plugin's exact nested configuration. No descriptor or
// live provider is synthesized from serve flags.
func newServeScenarioProviders(artifacts serveProfileArtifacts) (serveScenarioProviders, error) {
	recogniserMetrics := asrbuffer.NewAccumulator()
	result := serveScenarioProviders{Recogniser: recogniserMetrics}
	const policyProvider = "vllm"
	policyArtifact, err := serveProviderArtifact(artifacts.Gateway, "policy", policyProvider)
	if err != nil {
		return serveScenarioProviders{}, err
	}
	result.Policies = append(result.Policies, scenarioconversation.PolicyFactoryRegistration{
		ApplicationPolicySelection: scenarioconversation.ApplicationPolicySelection{
			Reference: serveProviderReference("policy", policyProvider), Artifact: policyArtifact,
		},
		DescribeConfiguration: func(raw json.RawMessage) (policyelements.SemanticDeciderDescriptor, error) {
			_, descriptor, err := decodeServePolicyConfiguration(policyProvider, raw)
			return descriptor, err
		},
		FactoryConfiguration: func(ctx context.Context, _ legacy.Options, raw json.RawMessage) (policyelements.SemanticDecider, error) {
			if err := profileProviderContext(ctx); err != nil {
				return nil, err
			}
			config, descriptor, err := decodeServePolicyConfiguration(policyProvider, raw)
			if err != nil {
				return nil, err
			}
			credential, err := profileProviderCredential(config.TokenEnvironment)
			if err != nil {
				return nil, err
			}
			client, err := policymodel.New(policymodel.Config{
				BaseURL: config.BaseURL, Model: config.Model, APIKey: credential,
				Timeout:      time.Duration(config.RequestTimeoutMS) * time.Millisecond,
				GuidedChoice: *config.GuidedChoice,
				Reasoning:    openaicompat.ReasoningControl(config.Reasoning),
			})
			if err != nil {
				return nil, err
			}
			return &serveSemanticDecider{Client: client, descriptor: descriptor}, nil
		},
		ReadinessConfiguration: func(ctx context.Context, raw json.RawMessage) error {
			if err := profileProviderContext(ctx); err != nil {
				return err
			}
			config, _, err := decodeServePolicyConfiguration(policyProvider, raw)
			if err != nil {
				return err
			}
			_, err = profileProviderCredential(config.TokenEnvironment)
			return err
		},
	})
	for _, entry := range providers.ASRs() {
		name := entry.Name
		artifact, err := serveProviderArtifact(artifacts.Gateway, "asr", name)
		if err != nil {
			return serveScenarioProviders{}, err
		}
		result.ASR = append(result.ASR, scenarioconversation.ASRFactoryRegistration{
			ApplicationASRSelection: scenarioconversation.ApplicationASRSelection{
				Reference: serveProviderReference("asr", name), Artifact: artifact,
			},
			DescribeConfiguration: func(raw json.RawMessage) (v1.Descriptor, error) {
				_, request, err := decodeServeASRConfiguration(name, raw)
				if err != nil {
					return v1.Descriptor{}, err
				}
				return providers.DescribeASR(request)
			},
			FactoryConfiguration: func(ctx context.Context, _ legacy.Options, raw json.RawMessage) (v1.PerceptionProvider, error) {
				if err := profileProviderContext(ctx); err != nil {
					return nil, err
				}
				config, request, err := decodeServeASRConfiguration(name, raw)
				if err != nil {
					return nil, err
				}
				factory, err := providers.NewASRFactory(request)
				if err != nil {
					return nil, err
				}
				provider, err := factory()
				if err != nil {
					return nil, err
				}
				return recogniserMetrics.New(asrbuffer.Config{
					Provider: provider, MinimumChunk: time.Duration(config.CadenceMS) * time.Millisecond,
				})
			},
			ReadinessConfiguration: func(ctx context.Context, raw json.RawMessage) error {
				if err := profileProviderContext(ctx); err != nil {
					return err
				}
				_, request, err := decodeServeASRConfiguration(name, raw)
				if err != nil {
					return err
				}
				factory, err := providers.NewASRFactory(request)
				if err != nil {
					return err
				}
				provider, err := factory()
				if err != nil {
					return err
				}
				return closeReadinessResource(provider)
			},
		})
	}
	for _, entry := range providers.LLMs() {
		name := entry.Name
		artifact, err := serveProviderArtifact(artifacts.Gateway, "model", name)
		if err != nil {
			return serveScenarioProviders{}, err
		}
		result.Models = append(result.Models, scenarioconversation.ModelFactoryRegistration{
			ApplicationModelSelection: scenarioconversation.ApplicationModelSelection{
				Reference: serveProviderReference("model", name), Artifact: artifact,
			},
			DescribeConfiguration: func(raw json.RawMessage) (continuation.Descriptor, error) {
				_, request, err := decodeServeModelConfiguration(name, raw)
				if err != nil {
					return continuation.Descriptor{}, err
				}
				return providers.DescribeLLM(request)
			},
			FactoryConfiguration: func(ctx context.Context, _ legacy.Options, raw json.RawMessage) (continuation.Provider, error) {
				if err := profileProviderContext(ctx); err != nil {
					return nil, err
				}
				_, request, err := decodeServeModelConfiguration(name, raw)
				if err != nil {
					return nil, err
				}
				return providers.NewLLM(request)
			},
			ReadinessConfiguration: func(ctx context.Context, raw json.RawMessage) error {
				if err := profileProviderContext(ctx); err != nil {
					return err
				}
				_, request, err := decodeServeModelConfiguration(name, raw)
				if err != nil {
					return err
				}
				provider, err := providers.NewLLM(request)
				if err != nil {
					return err
				}
				return closeReadinessResource(provider)
			},
		})
	}
	for _, entry := range providers.TTSs() {
		name := entry.Name
		artifact, err := serveProviderArtifact(artifacts.Gateway, "tts", name)
		if err != nil {
			return serveScenarioProviders{}, err
		}
		result.TTS = append(result.TTS, scenarioconversation.TTSFactoryRegistration{
			ApplicationTTSSelection: scenarioconversation.ApplicationTTSSelection{
				Reference: serveProviderReference("tts", name), Artifact: artifact,
			},
			DescribeConfiguration: func(raw json.RawMessage) (v1.Descriptor, string, error) {
				config, request, err := decodeServeTTSConfiguration(name, raw)
				if err != nil {
					return v1.Descriptor{}, "", err
				}
				descriptor, err := providers.DescribeTTS(request)
				return descriptor, serveTTSVoice(name, config.Voice), err
			},
			FactoryConfiguration: func(ctx context.Context, _ legacy.Options, raw json.RawMessage) (v1.SpeechProvider, error) {
				if err := profileProviderContext(ctx); err != nil {
					return nil, err
				}
				config, request, err := decodeServeTTSConfiguration(name, raw)
				if err != nil {
					return nil, err
				}
				speech, err := providers.NewTTS(request)
				if err != nil {
					return nil, err
				}
				if *config.SentenceWrapping {
					return bysentence.Provider{Inner: speech, Minimum: config.SentenceMinimumRunes}, nil
				}
				return speech, nil
			},
			ReadinessConfiguration: func(ctx context.Context, raw json.RawMessage) error {
				if err := profileProviderContext(ctx); err != nil {
					return err
				}
				_, request, err := decodeServeTTSConfiguration(name, raw)
				if err != nil {
					return err
				}
				provider, err := providers.NewTTS(request)
				if err != nil {
					return err
				}
				return closeReadinessResource(provider)
			},
		})
	}
	return result, nil
}

func serveProviderReference(role, name string) string {
	return "provider.openrealtime." + role + "." + name + ".v1"
}

func serveProviderArtifact(
	process inspect.ArtifactIdentity, role, name string,
) (inspect.ArtifactIdentity, error) {
	artifact := inspect.ArtifactIdentity{
		ID:     "go://github.com/bojieli/OpenRealtime/providers/" + role + "/" + name + "/v1",
		Digest: process.Digest,
	}
	if err := artifact.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("identify %s provider %q: %w", role, name, err)
	}
	return artifact, nil
}

func profileProviderContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("open launch-profile provider: nil context")
	}
	return context.Cause(ctx)
}

type serveSemanticDecider struct {
	*policymodel.Client
	descriptor policyelements.SemanticDeciderDescriptor
}

func (decider *serveSemanticDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	if decider == nil {
		return policyelements.SemanticDeciderDescriptor{}
	}
	return decider.descriptor
}

func profileProviderCredential(environment string) (string, error) {
	if environment == "" {
		return "", nil
	}
	credential := strings.TrimSpace(os.Getenv(environment))
	if credential == "" {
		return "", fmt.Errorf("semantic policy credential environment %s is not set", environment)
	}
	return credential, nil
}

func closeReadinessResource(resource any) error {
	if closer, ok := resource.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func serveTTSVoice(provider, requested string) string {
	entry, err := providers.LookupTTS(provider)
	if err == nil && strings.EqualFold(requested, "default") && strings.TrimSpace(entry.Voice) != "" {
		return strings.TrimSpace(entry.Voice)
	}
	if strings.TrimSpace(requested) == "" || strings.EqualFold(requested, "default") {
		return "model-selected"
	}
	return requested
}

type serveProfileHost struct {
	Artifacts     serveProfileArtifacts
	Providers     serveScenarioProviders
	Delegate      launchprofile.Registration
	ScenarioSuite launchprofile.Registration
	Meeting       *serveMeetingRegistration
	RealtimeCU    *serveRealtimeCURegistration
	Applications  *launchprofile.Registry
}

func newServeProfileHost(
	artifacts serveProfileArtifacts, providerInventory serveScenarioProviders,
) (serveProfileHost, error) {
	delegate, err := graphs.ScenarioConversationApplicationRegistration(
		scenarioconversation.ApplicationRegistrationConfig{
			ApplicationArtifact: artifacts.ScenarioConversation,
			ProviderArtifact:    artifacts.ScenarioProvider,
			RuntimeArtifact:     artifacts.ScenarioRuntime,
			DependencyArtifact:  artifacts.ScenarioDependencies,
			ASR:                 providerInventory.ASR, Policies: providerInventory.Policies,
			Models: providerInventory.Models,
			TTS:    providerInventory.TTS,
		},
	)
	if err != nil {
		return serveProfileHost{}, fmt.Errorf("register scenario-conversation application: %w", err)
	}
	suite, err := graphnative.NewApplicationPlugin(graphnative.ApplicationPluginConfig{
		Reference: graphnative.ApplicationReference,
		Artifact:  artifacts.ScenarioSuite,
		Delegate:  delegate,
	})
	if err != nil {
		return serveProfileHost{}, fmt.Errorf("register scenario-suite application: %w", err)
	}
	applicationsToRegister := []launchprofile.Registration{delegate, suite}
	meeting, err := newProductionServeMeetingRegistration(
		context.Background(), artifacts.Gateway,
	)
	if err != nil {
		return serveProfileHost{}, fmt.Errorf("register Meeting application: %w", err)
	}
	if meeting != nil {
		applicationsToRegister = append(applicationsToRegister, meeting.Application)
	}
	var realtimeCU *serveRealtimeCURegistration
	if strings.TrimSpace(os.Getenv(realtimeCULocalDeploymentEnvironment)) != "" {
		if os.Getenv(realtimeCULocalDeploymentEnvironment) != "1" {
			return serveProfileHost{}, errors.New("Realtime-CU local deployment opt-in must be exactly 1")
		}
		verifier, verifierErr := newLocalRealtimeCUDeploymentVerifier()
		if verifierErr != nil {
			return serveProfileHost{}, fmt.Errorf("construct Realtime-CU deployment verifier: %w", verifierErr)
		}
		deployments, resolveErr := verifier.Resolve(context.Background())
		if resolveErr != nil {
			return serveProfileHost{}, fmt.Errorf("resolve Realtime-CU live deployments: %w", resolveErr)
		}
		registration, registrationErr := newServeRealtimeCURegistration(
			context.Background(), artifacts.Gateway, deployments, verifier,
		)
		if registrationErr != nil {
			return serveProfileHost{}, fmt.Errorf("register Realtime-CU application: %w", registrationErr)
		}
		realtimeCU = &registration
		applicationsToRegister = append(applicationsToRegister, registration.Application)
	}
	applications, err := launchprofile.NewRegistry(applicationsToRegister)
	if err != nil {
		return serveProfileHost{}, fmt.Errorf("register production graph applications: %w", err)
	}
	return serveProfileHost{
		Artifacts: artifacts, Providers: providerInventory,
		Delegate: delegate, ScenarioSuite: suite, Meeting: meeting, RealtimeCU: realtimeCU,
		Applications: applications,
	}, nil
}

type profiledServeComposition struct {
	Profile            launchprofile.Document
	Graph              *serverprofile.GraphBundle
	Host               serveProfileHost
	GatewayToken       string
	OperatorAuthorizer management.Authorizer
	Readiness          *serveProfileReadiness
}

type serveProfileReadiness struct{ ready atomic.Bool }

func (state *serveProfileReadiness) Ready() bool {
	return state != nil && state.ready.Load()
}

func (state *serveProfileReadiness) CheckOnce(
	ctx context.Context, checks []graphlaunch.ReadinessCheck,
) error {
	if state == nil {
		return errors.New("nil launch-profile readiness state")
	}
	if ctx == nil {
		return errors.New("nil launch-profile readiness context")
	}
	state.ready.Store(false)
	for _, check := range checks {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if err := check.Check(ctx); err != nil {
			return fmt.Errorf("selected plugin %s: %w", check.Name, err)
		}
	}
	state.ready.Store(true)
	return nil
}

func (state *serveProfileReadiness) Run(
	ctx context.Context, checks []graphlaunch.ReadinessCheck, logger *slog.Logger,
) {
	for {
		if err := state.CheckOnce(ctx, checks); err == nil {
			return
		} else if logger != nil && context.Cause(ctx) == nil {
			logger.Warn("launch-profile providers are not ready", "error", err)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func newProfiledServeComposition(
	ctx context.Context,
	profile launchprofile.Document,
	host serveProfileHost,
	logger *slog.Logger,
) (profiledServeComposition, error) {
	if ctx == nil {
		return profiledServeComposition{}, errors.New("compose launch-profile server: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return profiledServeComposition{}, err
	}
	if host.Applications == nil {
		return profiledServeComposition{}, errors.New("compose launch-profile server: nil application registry")
	}
	readiness := &serveProfileReadiness{}
	tokenSnapshot := ""
	graph, err := serverprofile.NewProfileGraphBundle(ctx, serverprofile.ProfileGraphBundleConfig{
		Profile: profile, Applications: host.Applications,
		GatewayArtifact: host.Artifacts.Gateway,
		Gateway:         gatewayObservabilityConfig(logger, host.Providers.Recogniser, readiness.Ready),
		ResolveToken: func(resolveCtx context.Context, name string) (string, error) {
			value, resolveErr := resolveProfileTokenEnvironment(resolveCtx, name)
			if resolveErr == nil {
				tokenSnapshot = value
			}
			return value, resolveErr
		},
	})
	if err != nil {
		return profiledServeComposition{}, err
	}
	var operatorAuthorizer management.Authorizer
	if environment := profile.Server.OperatorCapabilityEnvironment; environment != "" {
		operatorToken, resolveErr := resolveProfileTokenEnvironment(ctx, environment)
		if resolveErr != nil {
			return profiledServeComposition{}, fmt.Errorf(
				"resolve launch-profile operator capability environment %s: %w",
				environment, resolveErr,
			)
		}
		if tokenSnapshot != "" &&
			subtle.ConstantTimeCompare([]byte(operatorToken), []byte(tokenSnapshot)) == 1 {
			return profiledServeComposition{},
				errors.New("launch-profile operator and gateway capabilities must be distinct")
		}
		grants, grantErr := graph.OperatorGrants()
		if grantErr != nil {
			return profiledServeComposition{}, grantErr
		}
		sealed, sealErr := management.NewDeploymentAuthorizer(operatorToken, grants)
		if sealErr != nil {
			return profiledServeComposition{}, sealErr
		}
		operatorAuthorizer = sealed
	}
	return profiledServeComposition{
		Profile: profile.Clone(), Graph: graph, Host: host,
		GatewayToken: tokenSnapshot, OperatorAuthorizer: operatorAuthorizer,
		Readiness: readiness,
	}, nil
}

func newProductionProfiledServeComposition(
	ctx context.Context, options serveOptions, logger *slog.Logger,
) (profiledServeComposition, error) {
	if ctx == nil {
		return profiledServeComposition{}, errors.New("compose production launch-profile server: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return profiledServeComposition{}, err
	}
	if err := validateServeProfileFlags(options); err != nil {
		return profiledServeComposition{}, err
	}
	profile, err := readServeLaunchProfile(ctx, options.launchProfile)
	if err != nil {
		return profiledServeComposition{}, err
	}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		return profiledServeComposition{}, err
	}
	providerInventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		return profiledServeComposition{}, err
	}
	host, err := newServeProfileHost(artifacts, providerInventory)
	if err != nil {
		return profiledServeComposition{}, err
	}
	return newProfiledServeComposition(ctx, profile, host, logger)
}

// validateServeProfileFlags rejects flags that the strict application/server
// profile supersedes. Only listener lifecycle and logging remain host-private;
// provider and interaction behavior must be selected by exact plugin config.
func validateServeProfileFlags(options serveOptions) error {
	var unsupported []string
	for name := range options.explicit {
		if serveProfileFlagAllowed(name) {
			continue
		}
		unsupported = append(unsupported, "-"+name)
	}
	if len(unsupported) == 0 {
		return nil
	}
	sort.Strings(unsupported)
	return fmt.Errorf(
		"-launch-profile supersedes flags %s; remove them or encode the selection in the strict profile",
		strings.Join(unsupported, ", "),
	)
}

func serveProfileFlagAllowed(name string) bool {
	switch name {
	case "config", "launch-profile", "listen", "shutdown-timeout", "log-format", "log-level",
		"webrtc-listen", "webrtc-stun", "webrtc-allow-origin":
		return true
	default:
		return false
	}
}

func gatewayObservabilityConfig(
	logger *slog.Logger, recogniser *asrbuffer.Accumulator, warm func() bool,
) gateway.Config {
	return gateway.Config{
		Logger: logger, Recogniser: recogniserReport(recogniser), Warm: warm,
	}
}

func resolveProfileTokenEnvironment(ctx context.Context, name string) (string, error) {
	if ctx == nil {
		return "", errors.New("resolve launch-profile token: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	value, found := os.LookupEnv(name)
	if !found {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return value, nil
}

func readServeLaunchProfile(ctx context.Context, path string) (launchprofile.Document, error) {
	if ctx == nil {
		return launchprofile.Document{}, errors.New("read graph launch profile: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return launchprofile.Document{}, err
	}
	if strings.TrimSpace(path) == "" {
		return launchprofile.Document{}, errors.New("a launch-profile path is required")
	}
	if strings.ContainsAny(path, "\x00\r\n") {
		return launchprofile.Document{}, errors.New("launch-profile path is not canonical")
	}
	payload, err := secureReadServeProfileFile(
		ctx, path, maximumServeLaunchProfileBytes, nil,
	)
	if err != nil {
		return launchprofile.Document{}, err
	}
	profile, err := launchprofile.ParseYAML(path, payload)
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("parse graph launch profile: %w", err)
	}
	return profile, nil
}
