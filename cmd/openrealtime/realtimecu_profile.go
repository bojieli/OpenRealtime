package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/bench"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	realtimecubinding "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	realtimeCULocalModelProvider       = "vllm"
	realtimeCULocalModelName           = "qwen-fast"
	realtimeCULocalModelURL            = "http://127.0.0.1:8000/v1"
	realtimeCULocalASRProvider         = "sensevoice"
	realtimeCULocalASRModel            = "iic/SenseVoiceSmall"
	realtimeCULocalASRURL              = "http://127.0.0.1:8002/v1"
	realtimeCULocalModelKeyEnvironment = "OPENREALTIME_LOCAL_API_KEY"
	realtimeCULocalASRKeyEnvironment   = "OPENREALTIME_ASR_API_KEY"

	realtimeCULocalModelReference    = "provider.openrealtime.realtime-cu.model.vllm.qwen-fast.local.v1"
	realtimeCULocalObserverReference = "provider.openrealtime.realtime-cu.observer.sensevoice-keyframe.local.v2"
	realtimeCULocalObserverName      = "openrealtime.realtime-cu.local-audiovisual-observer"

	realtimeCUApplicationArtifactID = "go://github.com/bojieli/OpenRealtime/graphs/realtime-computer-use/application/v1"
	realtimeCUProviderArtifactID    = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/session-provider/v1"
	realtimeCUInspectionTokenTTLMS  = uint64((5 * time.Minute) / time.Millisecond)
	realtimeCUAttachedKeyframeMode  = "attached-keyframe-v1"
)

type realtimeCUProfileOptions struct {
	out                string
	graphOut           string
	valuesOut          string
	resolutionOut      string
	executionOut       string
	name               string
	revision           uint64
	targetName         string
	width              int
	height             int
	tokenEnv           string
	inspectionTTL      uint64
	maxAudioBytes      int
	deployments        realtimeCUDeploymentIdentities
	deploymentVerifier realtimeCUDeploymentVerifier
}

func defaultRealtimeCUProfileOptions() realtimeCUProfileOptions {
	return realtimeCUProfileOptions{
		name: "openrealtime.launch.realtime-cu-local", revision: 1,
		// Chromium's 1280x720 headless outer window exposes a 1280x577 CSS
		// viewport after browser chrome. The profile must bind the actual action
		// coordinate space used by bench/realtimecu, not the outer-window size.
		targetName: "benchmark-browser", width: 1280, height: 577,
		tokenEnv:      "OPENREALTIME_TOKEN",
		inspectionTTL: realtimeCUInspectionTokenTTLMS, maxAudioBytes: 1 << 20,
	}
}

func runRealtimeCUProfileFreeze(arguments []string, output io.Writer) error {
	options := defaultRealtimeCUProfileOptions()
	flags := flag.NewFlagSet("openrealtime profile realtime-cu", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.out, "out", "", "new absolute Realtime-CU launch-profile YAML path")
	flags.StringVar(&options.graphOut, "graph-out", "", "new absolute exact bound Graph IR JSON path")
	flags.StringVar(&options.valuesOut, "values-out", "", "new absolute exact element-values JSON path")
	flags.StringVar(&options.resolutionOut, "resolution-out", "", "new absolute expected live-resolution JSON path")
	flags.StringVar(&options.executionOut, "execution-out", "", "new absolute reviewed execution-requirement JSON path")
	flags.StringVar(&options.name, "name", options.name, "immutable profile name")
	flags.Uint64Var(&options.revision, "revision", options.revision, "positive profile revision")
	flags.StringVar(&options.targetName, "target", options.targetName, "exact browser action-target name")
	flags.IntVar(&options.width, "width", options.width, "exact browser CSS viewport width")
	flags.IntVar(&options.height, "height", options.height, "exact browser CSS viewport height")
	flags.StringVar(&options.tokenEnv, "token-env", options.tokenEnv, "required gateway bearer-token environment name")
	flags.Uint64Var(&options.inspectionTTL, "inspection-token-ttl-ms", options.inspectionTTL, "runtime-inspection token lifetime")
	flags.IntVar(&options.maxAudioBytes, "max-audio-frame-bytes", options.maxAudioBytes, "Realtime audio-frame bound")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("profile realtime-cu accepts flags only")
	}
	verifier, err := newLocalRealtimeCUDeploymentVerifier()
	if err != nil {
		return err
	}
	options.deploymentVerifier = verifier
	options.deployments, err = verifier.Resolve(context.Background())
	if err != nil {
		return fmt.Errorf("resolve live Realtime-CU deployments for profile freeze: %w", err)
	}
	if err := validateRealtimeCUProfileOptions(options); err != nil {
		return err
	}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		return err
	}
	frozen, err := freezeProductionRealtimeCUProfile(context.Background(), options, artifacts.Gateway)
	if err != nil {
		return err
	}
	return writeFrozenRealtimeCUProfile(output, options, frozen)
}

type serveRealtimeCURegistration struct {
	Application launchprofile.Registration
	Model       realtimecubinding.ApplicationModelSelection
	Observer    realtimecubinding.ApplicationObserverSelection
	Provider    inspect.ArtifactIdentity
	Runtime     inspect.ArtifactIdentity
}

type realtimeCUDeploymentIdentities struct {
	Model  inspect.ArtifactIdentity `json:"model"`
	ASR    inspect.ArtifactIdentity `json:"asr"`
	Vision inspect.ArtifactIdentity `json:"vision"`
}

func (identities realtimeCUDeploymentIdentities) validate() error {
	for _, deployment := range []struct {
		name     string
		identity inspect.ArtifactIdentity
	}{
		{name: "model", identity: identities.Model},
		{name: "ASR", identity: identities.ASR},
		{name: "vision", identity: identities.Vision},
	} {
		name, identity := deployment.name, deployment.identity
		if err := identity.Validate(); err != nil || identity.Revision == "" || identity.Digest == "" {
			return fmt.Errorf("Realtime-CU %s deployment requires an exact ID, revision, and digest", name)
		}
	}
	return nil
}

type realtimeCULocalConfiguration struct {
	FormatVersion uint64                        `json:"format_version"`
	Executable    inspect.ArtifactIdentity      `json:"executable"`
	Model         realtimeCULocalModelConfig    `json:"model"`
	Observer      realtimeCULocalObserverConfig `json:"observer"`
}

type realtimeCULocalModelConfig struct {
	Provider   string                   `json:"provider"`
	Model      string                   `json:"model"`
	BaseURL    string                   `json:"base_url"`
	Effort     string                   `json:"effort"`
	Reason     string                   `json:"reason"`
	Deployment inspect.ArtifactIdentity `json:"deployment"`
}

type realtimeCULocalObserverConfig struct {
	ASRProvider     string                   `json:"asr_provider"`
	ASRModel        string                   `json:"asr_model"`
	ASRBaseURL      string                   `json:"asr_base_url"`
	VideoMode       string                   `json:"video_mode"`
	AttachKeyframes bool                     `json:"attach_keyframes"`
	ExternalCadence bool                     `json:"external_cadence"`
	ChangeThreshold float64                  `json:"change_threshold"`
	Gate            perception.GateConfig    `json:"gate"`
	ASRDeployment   inspect.ArtifactIdentity `json:"asr_deployment"`
}

func newServeRealtimeCURegistration(
	ctx context.Context, executable inspect.ArtifactIdentity, deployments realtimeCUDeploymentIdentities,
	verifier realtimeCUDeploymentVerifier,
) (serveRealtimeCURegistration, error) {
	if ctx == nil || nilRealtimeCUDeploymentInterface(verifier) {
		return serveRealtimeCURegistration{}, errors.New("Realtime-CU registration requires a deployment verifier")
	}
	if err := executable.Validate(); err != nil {
		return serveRealtimeCURegistration{}, fmt.Errorf("Realtime-CU profile executable: %w", err)
	}
	if err := deployments.validate(); err != nil {
		return serveRealtimeCURegistration{}, err
	}
	if err := verifier.Verify(ctx, deployments); err != nil {
		return serveRealtimeCURegistration{}, fmt.Errorf("verify Realtime-CU registration deployments: %w", err)
	}
	linked := func(id string) (inspect.ArtifactIdentity, error) {
		artifact := inspect.ArtifactIdentity{ID: id, Digest: executable.Digest}
		if err := artifact.Validate(); err != nil {
			return inspect.ArtifactIdentity{}, err
		}
		return artifact, nil
	}
	applicationArtifact, err := linked(realtimeCUApplicationArtifactID)
	if err != nil {
		return serveRealtimeCURegistration{}, err
	}
	providerArtifact, err := linked(realtimeCUProviderArtifactID)
	if err != nil {
		return serveRealtimeCURegistration{}, err
	}
	runtimeArtifact, err := linked(realtimecubinding.AdapterReference)
	if err != nil {
		return serveRealtimeCURegistration{}, err
	}
	local := realtimeCULocalConfiguration{
		FormatVersion: 1, Executable: executable,
		Model: realtimeCULocalModelConfig{
			Provider: realtimeCULocalModelProvider, Model: realtimeCULocalModelName,
			BaseURL: realtimeCULocalModelURL, Effort: string(continuation.EffortMinimal),
			Reason:     string(providers.ReasonOff),
			Deployment: deployments.Model,
		},
		Observer: realtimeCULocalObserverConfig{
			ASRProvider: realtimeCULocalASRProvider, ASRModel: realtimeCULocalASRModel,
			ASRBaseURL: realtimeCULocalASRURL, VideoMode: realtimeCUAttachedKeyframeMode,
			AttachKeyframes: true, ExternalCadence: true, ChangeThreshold: 0.02,
			Gate: perception.DefaultGateConfig(), ASRDeployment: deployments.ASR,
		},
	}
	observerArtifact, err := realtimeCUConfigurationArtifact(
		"profile://openrealtime/realtime-cu/local-observer-composition", local.Observer, executable,
	)
	if err != nil {
		return serveRealtimeCURegistration{}, err
	}
	vision := true
	temperature := 0.0
	modelRequest := providers.LLMRequest{
		Provider: realtimeCULocalModelProvider, Model: realtimeCULocalModelName,
		BaseURL: realtimeCULocalModelURL, Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent, Reason: providers.ReasonOff,
		Vision: &vision, RetainReasoning: false, Temperature: &temperature,
		RequestTimeout: 30 * time.Second,
	}
	descriptor, err := providers.DescribeLLM(modelRequest)
	if err != nil {
		return serveRealtimeCURegistration{}, fmt.Errorf("describe Realtime-CU local model: %w", err)
	}
	modelSelection := realtimecubinding.ApplicationModelSelection{
		Reference: realtimeCULocalModelReference, Artifact: deployments.Model, Descriptor: descriptor,
	}
	observerSelection := realtimecubinding.ApplicationObserverSelection{
		Reference: realtimeCULocalObserverReference, Name: realtimeCULocalObserverName,
		Artifact: observerArtifact,
		Sources: []string{
			realtimecubinding.SourceCamera,
			realtimecubinding.SourceMicrophone,
			realtimecubinding.SourceScreen,
		},
	}
	registration, err := graphs.RealtimeComputerUseApplicationRegistration(
		realtimecubinding.ApplicationRegistrationConfig{
			ApplicationArtifact: applicationArtifact, ProviderArtifact: providerArtifact,
			RuntimeArtifact: runtimeArtifact,
			Models: []realtimecubinding.ModelFactoryRegistration{{
				ApplicationModelSelection: modelSelection,
				Readiness: func(ctx context.Context) error {
					if err := verifyRealtimeCUModelDeployment(ctx, verifier, deployments); err != nil {
						return fmt.Errorf("verify Realtime-CU model deployment readiness: %w", err)
					}
					return nil
				},
				Factory: func(ctx context.Context, _ legacy.Options) (continuation.Provider, error) {
					if ctx == nil {
						return nil, errors.New("open Realtime-CU local model: nil context")
					}
					if cause := context.Cause(ctx); cause != nil {
						return nil, cause
					}
					request := modelRequest
					request.APIKey = os.Getenv(realtimeCULocalModelKeyEnvironment)
					return providers.NewLLM(request)
				},
			}},
			Observers: []realtimecubinding.ObserverFactoryRegistration{{
				ApplicationObserverSelection: observerSelection,
				Readiness: func(ctx context.Context) error {
					if err := verifyRealtimeCUObserverDeployment(ctx, verifier, deployments); err != nil {
						return fmt.Errorf("verify Realtime-CU observer deployment readiness: %w", err)
					}
					return nil
				},
				ResourceFactory: func(
					ctx context.Context, _ legacy.Options, resources realtimecubinding.ObserverResources,
				) (realtimecubinding.Observer, error) {
					return newRealtimeCULocalObserver(ctx, local.Observer, resources.Retainer)
				},
			}},
		},
	)
	if err != nil {
		return serveRealtimeCURegistration{}, err
	}
	return serveRealtimeCURegistration{
		Application: registration, Model: modelSelection, Observer: observerSelection,
		Provider: providerArtifact, Runtime: runtimeArtifact,
	}, nil
}

func verifyRealtimeCUModelDeployment(
	ctx context.Context, verifier realtimeCUDeploymentVerifier,
	deployments realtimeCUDeploymentIdentities,
) error {
	if scoped, ok := verifier.(realtimeCUScopedDeploymentVerifier); ok {
		return scoped.VerifyModel(ctx, deployments.Model)
	}
	return verifier.Verify(ctx, deployments)
}

func verifyRealtimeCUObserverDeployment(
	ctx context.Context, verifier realtimeCUDeploymentVerifier,
	deployments realtimeCUDeploymentIdentities,
) error {
	if scoped, ok := verifier.(realtimeCUScopedDeploymentVerifier); ok {
		return scoped.VerifyObserver(ctx, deployments.ASR, deployments.Vision)
	}
	return verifier.Verify(ctx, deployments)
}

func realtimeCUConfigurationArtifact(
	id string, config any, executable inspect.ArtifactIdentity,
) (inspect.ArtifactIdentity, error) {
	payload, err := json.Marshal(struct {
		Executable inspect.ArtifactIdentity `json:"executable"`
		Config     any                      `json:"config"`
	}{Executable: executable, Config: config})
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	digest := sha256.Sum256(payload)
	artifact := inspect.ArtifactIdentity{
		ID: id, Revision: "v1", Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
	if err := artifact.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	return artifact, nil
}

func newRealtimeCULocalObserver(
	ctx context.Context, config realtimeCULocalObserverConfig, retainer perception.Retainer,
) (realtimecubinding.Observer, error) {
	if ctx == nil {
		return nil, errors.New("open Realtime-CU local observer: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if config.VideoMode != realtimeCUAttachedKeyframeMode || !config.AttachKeyframes ||
		!config.ExternalCadence || config.ChangeThreshold <= 0 || config.ChangeThreshold > 1 {
		return nil, errors.New("open Realtime-CU local observer: invalid attached-keyframe contract")
	}
	if retainer == nil {
		return nil, errors.New("open Realtime-CU local observer: attached keyframes require the session media retainer")
	}
	asrFactory, err := providers.NewASRFactory(providers.ASRRequest{
		Provider: config.ASRProvider, Model: config.ASRModel, BaseURL: config.ASRBaseURL,
		APIKey: os.Getenv(realtimeCULocalASRKeyEnvironment), PartialInterval: 200 * time.Millisecond,
		RequestTimeout: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("create Realtime-CU ASR plug-in: %w", err)
	}
	audio, err := newEndpointingAudioObserver(endpointingAudioObserverConfig{
		Name: realtimeCULocalObserverName, Source: realtimecubinding.SourceMicrophone,
		Gate: config.Gate, Provider: asrFactory, Cadence: 200 * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	video, err := perception.NewVideoObserver(perception.VideoConfig{
		Name:            realtimeCULocalObserverName,
		Sources:         []string{realtimecubinding.SourceCamera, realtimecubinding.SourceScreen},
		ExternalCadence: config.ExternalCadence, ChangeThreshold: config.ChangeThreshold,
		Narrator: realtimeCUAttachedKeyframeNarrator{}, Retainer: retainer,
		AttachKeyframes: config.AttachKeyframes,
	})
	if err != nil {
		_ = audio.Close()
		return nil, err
	}
	observer, err := realtimecubinding.NewPerceptionObserver(
		realtimecubinding.PerceptionObserverConfig{
			Name: realtimeCULocalObserverName, Audio: audio, Video: video,
		},
	)
	if err != nil {
		_ = audio.Close()
		video.Reset()
		return nil, err
	}
	return observer, nil
}

type frozenRealtimeCUProfile struct {
	Profile     launchprofile.Document
	Plan        *graphconfig.Plan
	Values      graphvalues.Document
	Resolution  bench.LiveResolution
	Execution   bench.ExecutionRequirement
	Deployments realtimeCUDeploymentIdentities
}

func freezeProductionRealtimeCUProfile(
	ctx context.Context, options realtimeCUProfileOptions, executable inspect.ArtifactIdentity,
) (frozenRealtimeCUProfile, error) {
	if ctx == nil {
		return frozenRealtimeCUProfile{}, errors.New("freeze production Realtime-CU profile: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return frozenRealtimeCUProfile{}, cause
	}
	if nilRealtimeCUDeploymentInterface(options.deploymentVerifier) {
		return frozenRealtimeCUProfile{}, errors.New("freeze production Realtime-CU profile without a deployment verifier")
	}
	if err := options.deploymentVerifier.Verify(ctx, options.deployments); err != nil {
		return frozenRealtimeCUProfile{}, fmt.Errorf("verify Realtime-CU deployments for profile freeze: %w", err)
	}
	selected, err := newServeRealtimeCURegistration(
		ctx, executable, options.deployments, options.deploymentVerifier,
	)
	if err != nil {
		return frozenRealtimeCUProfile{}, err
	}
	target := computeruse.Target{
		Name: options.targetName, Sources: []string{realtimecubinding.SourceScreen},
		Width: options.width, Height: options.height,
	}
	application := realtimecubinding.ApplicationConfig{
		FormatVersion: realtimecubinding.ApplicationFormatVersion,
		Model:         selected.Model, Observer: selected.Observer, Target: target,
	}
	payload, err := json.Marshal(application)
	if err != nil {
		return frozenRealtimeCUProfile{}, err
	}
	launchConfig, err := selected.Application.Factory(ctx, payload)
	if err != nil {
		return frozenRealtimeCUProfile{}, err
	}
	prepared, err := graphlaunch.New(ctx, launchConfig)
	if err != nil {
		return frozenRealtimeCUProfile{}, err
	}
	plan := prepared.Plan
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          options.name, Revision: options.revision,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: selected.Application.Reference, Artifact: selected.Application.Artifact,
			},
			Configuration: payload,
		},
		Plan: plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference: realtimecubinding.AdapterReference, RuntimeArtifact: selected.Runtime,
			ProfileName: realtimecubinding.ProfileName, ProfileRevision: realtimecubinding.ProfileRevision,
		},
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.realtime-cu-local", ProfileRevision: 1,
			ProviderArtifact: selected.Provider, GatewayArtifact: executable,
			TokenEnvironment: options.tokenEnv, Model: realtimeCULocalModelName,
			TranscriptionModel: realtimeCULocalASRModel, ValidateWire: true,
			InspectionTokenTTLMS: options.inspectionTTL,
			MaxAudioFrameBytes:   options.maxAudioBytes,
			VideoLimits:          openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		return frozenRealtimeCUProfile{}, err
	}
	values := graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: plan.Graph().ID, Nodes: plan.Values(),
	}
	bound, err := graphvalues.Bind(plan.Graph(), values)
	if err != nil || bound.Graph.Fingerprint != plan.Graph().Fingerprint {
		return frozenRealtimeCUProfile{}, errors.New("freeze Realtime-CU values did not reproduce the exact bound graph")
	}
	resolution, err := probeRealtimeCUExpectedResolution(ctx, prepared.Binding, plan)
	if err != nil {
		return frozenRealtimeCUProfile{}, err
	}
	requirement, err := bench.RequireGraph(plan.Graph(), bench.ArtifactIdentity{
		ID: "values://" + plan.Graph().ID, Revision: graphvalues.APIVersion,
		Digest: bound.Fingerprint,
	}, resolution)
	if err != nil {
		return frozenRealtimeCUProfile{}, fmt.Errorf("author Realtime-CU execution requirement: %w", err)
	}
	return frozenRealtimeCUProfile{
		Profile: profile, Plan: plan, Values: values,
		Resolution: resolution, Execution: requirement, Deployments: options.deployments,
	}, nil
}

type realtimeCUProfileProbeRuntime interface {
	legacy.Runtime
	Live() inspect.Live
	Done() <-chan struct{}
}

// probeRealtimeCUExpectedResolution mounts the exact selected application long
// enough for every factory/provider boundary to publish its live immutable
// runtime and capability identities. It submits no user input and therefore
// cannot invent task-specific selected paths.
func probeRealtimeCUExpectedResolution(
	ctx context.Context,
	binding legacy.Binding,
	plan *graphconfig.Plan,
) (resolution bench.LiveResolution, resultErr error) {
	if ctx == nil {
		return bench.LiveResolution{}, errors.New("probe Realtime-CU expected resolution: nil context")
	}
	if binding == nil || plan == nil {
		return bench.LiveResolution{}, errors.New(
			"probe Realtime-CU expected resolution: binding and plan are required",
		)
	}
	probeContext, cancelProbe := context.WithTimeout(ctx, time.Minute)
	defer cancelProbe()
	runtimeValue, err := binding.Start(probeContext, legacy.Options{
		Sink:      realtimeCUProfileProbeSink{},
		SessionID: "realtime-cu-profile-resolution-probe",
	})
	if err != nil {
		return bench.LiveResolution{}, fmt.Errorf("probe Realtime-CU expected resolution: start: %w", err)
	}
	runtime, ok := runtimeValue.(realtimeCUProfileProbeRuntime)
	if !ok {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		return bench.LiveResolution{}, errors.Join(
			errors.New("probe Realtime-CU expected resolution: runtime has no graph inspection surface"),
			runtimeValue.Close(closeContext, errors.New("profile resolution probe refused runtime")),
		)
	}
	defer func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if closeErr := runtime.Close(
			closeContext, errors.New("profile resolution probe complete"),
		); closeErr != nil {
			// A resolution is not publishable unless the resource-free probe also
			// reached its bounded terminal lifecycle. Do not hand a caller a
			// tempting non-zero value alongside that failure.
			resolution = bench.LiveResolution{}
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()

	configuration := bench.ArtifactIdentity{
		ID:       "values://" + plan.Graph().ID,
		Revision: graphvalues.APIVersion,
		Digest:   plan.Identity().ValuesDigest,
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := runtime.Live()
		if realtimeCUProfileProbeIsLive(plan, snapshot) {
			resolution, err = bench.AuthorExpectedResolutionFromInspection(
				plan.Graph(), configuration, snapshot,
			)
			if err != nil {
				return bench.LiveResolution{}, fmt.Errorf(
					"probe Realtime-CU expected resolution: freeze: %w", err,
				)
			}
			return resolution, nil
		}
		select {
		case <-probeContext.Done():
			return bench.LiveResolution{}, fmt.Errorf(
				"probe Realtime-CU expected resolution: readiness: %w", context.Cause(probeContext),
			)
		case <-runtime.Done():
			return bench.LiveResolution{}, errors.New(
				"probe Realtime-CU expected resolution: runtime stopped before every live identity was reported",
			)
		case <-ticker.C:
		}
	}
}

func realtimeCUProfileProbeIsLive(plan *graphconfig.Plan, snapshot inspect.Live) bool {
	if plan == nil || snapshot.State != "running" || snapshot.Deployment == nil ||
		len(snapshot.Nodes) != len(plan.Graph().Nodes) {
		return false
	}
	for _, node := range plan.Graph().Nodes {
		live, found := snapshot.Nodes[node.ID]
		if !found || live.State != "running" || live.Resolution == nil ||
			live.Resolution.RuntimeEvidence != inspect.EvidenceLive ||
			live.Resolution.CapabilitiesEvidence != inspect.EvidenceLive {
			return false
		}
	}
	return true
}

type realtimeCUProfileProbeSink struct{}

func (realtimeCUProfileProbeSink) TurnBegin(context.Context) error { return nil }
func (realtimeCUProfileProbeSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	return nil
}
func (realtimeCUProfileProbeSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (realtimeCUProfileProbeSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (realtimeCUProfileProbeSink) Observation(context.Context, perception.Observation) error {
	return nil
}
func (realtimeCUProfileProbeSink) SpeechBegin(context.Context, action.Utterance) error { return nil }
func (realtimeCUProfileProbeSink) SpeechText(context.Context, action.Utterance, string) error {
	return nil
}
func (realtimeCUProfileProbeSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (realtimeCUProfileProbeSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (realtimeCUProfileProbeSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (realtimeCUProfileProbeSink) Failed(context.Context, legacy.ErrorEvent)             {}

func writeFrozenRealtimeCUProfile(
	output io.Writer, options realtimeCUProfileOptions, frozen frozenRealtimeCUProfile,
) error {
	profilePayload, err := launchprofile.MarshalYAML(frozen.Profile)
	if err != nil {
		return err
	}
	graphPayload, err := frozen.Plan.Graph().Marshal()
	if err != nil {
		return err
	}
	valuesPayload, err := json.MarshalIndent(frozen.Values, "", "  ")
	if err != nil {
		return err
	}
	valuesPayload = append(valuesPayload, '\n')
	resolutionPayload, err := bench.MarshalExpectedResolution(frozen.Resolution)
	if err != nil {
		return err
	}
	executionPayload, err := bench.MarshalExecutionRequirement(frozen.Execution)
	if err != nil {
		return err
	}
	artifacts := []struct {
		label   string
		path    string
		payload []byte
	}{
		{"bound Graph IR", options.graphOut, graphPayload},
		{"element values", options.valuesOut, valuesPayload},
		{"expected live resolution", options.resolutionOut, resolutionPayload},
		{"reviewed execution requirement", options.executionOut, executionPayload},
		{"launch profile", options.out, profilePayload},
	}
	if err := publishRealtimeCUProfileCampaign(options, artifacts, nil); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s\n", options.out)
	fmt.Fprintf(output, "graph       %s\n", options.graphOut)
	fmt.Fprintf(output, "values      %s\n", options.valuesOut)
	fmt.Fprintf(output, "resolution  %s\n", options.resolutionOut)
	fmt.Fprintf(output, "execution   %s\n", options.executionOut)
	fmt.Fprintf(output, "profile     %s@%d\n", frozen.Profile.Name, frozen.Profile.Revision)
	fmt.Fprintf(output, "fingerprint %s\n", frozen.Profile.Fingerprint)
	fmt.Fprintf(output, "plan        %s\n", frozen.Profile.Plan.PlanFingerprint)
	fmt.Fprintf(output, "providers   model=%s/%s asr=%s/%s vision=%s/%s\n",
		realtimeCULocalModelProvider, realtimeCULocalModelName,
		realtimeCULocalASRProvider, realtimeCULocalASRModel,
		realtimeCULocalModelProvider, realtimeCULocalModelName)
	fmt.Fprintf(output, "deployments model=%s@%s asr=%s@%s vision=%s@%s\n",
		frozen.Deployments.Model.ID, frozen.Deployments.Model.Revision,
		frozen.Deployments.ASR.ID, frozen.Deployments.ASR.Revision,
		frozen.Deployments.Vision.ID, frozen.Deployments.Vision.Revision)
	return nil
}

type realtimeCUProfilePublicationHook func(completedFiles int) error

func publishRealtimeCUProfileCampaign(
	options realtimeCUProfileOptions,
	artifacts []struct {
		label   string
		path    string
		payload []byte
	},
	hook realtimeCUProfilePublicationHook,
) (resultErr error) {
	campaignDirectory := filepath.Dir(options.out)
	parentPath, campaignName := filepath.Dir(campaignDirectory), filepath.Base(campaignDirectory)
	if err := validateRealtimeCUProfileParent(parentPath); err != nil {
		return errors.New("Realtime-CU profile campaign parent is invalid")
	}
	parentBefore, err := os.Lstat(parentPath)
	if err != nil || parentBefore.Mode()&os.ModeSymlink != 0 || !parentBefore.IsDir() {
		return errors.New("Realtime-CU profile campaign parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return errors.New("open Realtime-CU profile campaign parent")
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close Realtime-CU profile campaign parent"))
		}
	}()
	openedParent, openErr := parent.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(parentBefore, openedParent) || !os.SameFile(openedParent, visibleParent) {
		return errors.New("Realtime-CU profile campaign parent changed while opening")
	}
	if existing, err := parent.Lstat(campaignName); err == nil {
		if !existing.IsDir() || existing.Mode()&os.ModeSymlink != 0 {
			return errors.New("Realtime-CU profile campaign path is not a directory")
		}
		if err := verifyRealtimeCUProfileCampaign(parent, campaignName, artifacts); err != nil {
			return err
		}
		return syncRealtimeCUProfileRoot(parent)
	} else if !os.IsNotExist(err) {
		return errors.New("inspect Realtime-CU profile campaign path")
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return errors.New("create Realtime-CU profile campaign stage identity")
	}
	stageName := "." + campaignName + ".quarantine-" + hex.EncodeToString(entropy[:])
	if err := parent.Mkdir(stageName, 0o700); err != nil {
		return errors.New("create Realtime-CU profile campaign stage")
	}
	published := false
	defer func() {
		if !published {
			if err := parent.RemoveAll(stageName); err != nil {
				resultErr = errors.Join(resultErr, errors.New("remove failed Realtime-CU profile campaign stage"))
			}
		}
	}()
	stage, err := parent.OpenRoot(stageName)
	if err != nil {
		return errors.New("open Realtime-CU profile campaign stage")
	}
	stageClosed := false
	defer func() {
		if !stageClosed {
			if closeErr := stage.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, errors.New("close Realtime-CU profile campaign stage"))
			}
		}
	}()
	for index, artifact := range artifacts {
		name := filepath.Base(artifact.path)
		if filepath.Dir(artifact.path) != campaignDirectory || name == "." || name == "" {
			return errors.New("Realtime-CU profile artifact escaped its campaign directory")
		}
		file, err := stage.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("write Realtime-CU %s: create artifact", artifact.label)
		}
		_, writeErr := file.Write(artifact.payload)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("write Realtime-CU %s: retain artifact", artifact.label)
		}
		if hook != nil {
			if err := hook(index + 1); err != nil {
				return err
			}
		}
	}
	if err := syncRealtimeCUProfileRoot(stage); err != nil {
		return errors.New("sync Realtime-CU profile campaign stage")
	}
	if err := stage.Close(); err != nil {
		return errors.New("close Realtime-CU profile campaign stage before publication")
	}
	stageClosed = true
	if err := renameRealtimeCUProfileNoReplace(
		parent, openedParent, stageName, campaignName,
	); err != nil {
		return errors.New("publish Realtime-CU profile campaign atomically")
	}
	published = true
	if err := syncRealtimeCUProfileRoot(parent); err != nil {
		return errors.New("sync published Realtime-CU profile campaign parent")
	}
	entry, entryErr := parent.Lstat(campaignName)
	visible, visibleErr := os.Lstat(campaignDirectory)
	parentAfter, parentAfterErr := os.Lstat(parentPath)
	if entryErr != nil || visibleErr != nil || parentAfterErr != nil || !entry.IsDir() ||
		entry.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry, visible) ||
		!os.SameFile(openedParent, parentAfter) {
		return errors.New("published Realtime-CU profile campaign identity changed")
	}
	return verifyRealtimeCUProfileCampaign(parent, campaignName, artifacts)
}

func syncRealtimeCUProfileRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	return errors.Join(syncErr, file.Close())
}

func verifyRealtimeCUProfileCampaign(
	parent *os.Root, campaignName string,
	artifacts []struct {
		label   string
		path    string
		payload []byte
	},
) (resultErr error) {
	entryBefore, err := parent.Lstat(campaignName)
	if err != nil || !entryBefore.IsDir() || entryBefore.Mode()&os.ModeSymlink != 0 {
		return errors.New("inspect existing Realtime-CU profile campaign identity")
	}
	root, err := parent.OpenRoot(campaignName)
	if err != nil {
		return errors.New("open existing Realtime-CU profile campaign")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close existing Realtime-CU profile campaign"))
		}
	}()
	openedRoot, openErr := root.Stat(".")
	if openErr != nil || !os.SameFile(entryBefore, openedRoot) {
		return errors.New("existing Realtime-CU profile campaign changed while opening")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("list existing Realtime-CU profile campaign")
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil || len(entries) != len(artifacts) {
		return errors.New("existing Realtime-CU profile campaign has the wrong artifact set")
	}
	wanted := make(map[string][]byte, len(artifacts))
	for _, artifact := range artifacts {
		wanted[filepath.Base(artifact.path)] = artifact.payload
	}
	for _, entry := range entries {
		expected, ok := wanted[entry.Name()]
		if !ok || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("existing Realtime-CU profile campaign has an unexpected artifact")
		}
		before, err := root.Lstat(entry.Name())
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
			before.Size() != int64(len(expected)) {
			return errors.New("existing Realtime-CU profile artifact has an invalid identity")
		}
		file, err := root.Open(entry.Name())
		if err != nil {
			return errors.New("open existing Realtime-CU profile artifact")
		}
		opened, statErr := file.Stat()
		payload, readErr := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
		linkErr := fileidentity.RequireSingleLink(file)
		closeErr := file.Close()
		after, afterErr := root.Lstat(entry.Name())
		if statErr != nil || readErr != nil || linkErr != nil || closeErr != nil || afterErr != nil ||
			!os.SameFile(before, opened) || !os.SameFile(opened, after) ||
			after.Size() != int64(len(expected)) || !bytes.Equal(payload, expected) {
			return errors.New("existing Realtime-CU profile artifact differs from the requested campaign")
		}
	}
	entryAfter, entryErr := parent.Lstat(campaignName)
	rootAfter, rootErr := root.Stat(".")
	if entryErr != nil || rootErr != nil || !os.SameFile(entryBefore, entryAfter) ||
		!os.SameFile(openedRoot, rootAfter) || !os.SameFile(entryAfter, rootAfter) {
		return errors.New("existing Realtime-CU profile campaign changed during verification")
	}
	return nil
}

func validateRealtimeCUProfileParent(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("Realtime-CU profile parent is missing, symlinked, or not a directory")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func validateRealtimeCUProfileOptions(options realtimeCUProfileOptions) error {
	paths := []string{
		options.out, options.graphOut, options.valuesOut, options.resolutionOut, options.executionOut,
	}
	seen := make(map[string]struct{}, len(paths))
	campaignDirectory := ""
	for _, path := range paths {
		if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("profile realtime-cu requires all five output paths")
		}
		if _, duplicate := seen[path]; duplicate {
			return errors.New("profile realtime-cu output paths must be distinct")
		}
		seen[path] = struct{}{}
		if campaignDirectory == "" {
			campaignDirectory = filepath.Dir(path)
		} else if filepath.Dir(path) != campaignDirectory {
			return errors.New("profile realtime-cu outputs must share one create-only campaign directory")
		}
	}
	if campaignDirectory == filepath.Dir(campaignDirectory) {
		return errors.New("profile realtime-cu campaign directory cannot be a filesystem root")
	}
	if strings.TrimSpace(options.name) == "" || options.revision == 0 ||
		strings.TrimSpace(options.targetName) == "" || options.width <= 0 || options.height <= 0 ||
		strings.TrimSpace(options.tokenEnv) == "" || strings.TrimSpace(options.tokenEnv) != options.tokenEnv ||
		options.inspectionTTL == 0 || options.maxAudioBytes <= 0 {
		return errors.New("profile realtime-cu has invalid identity, target, or server bounds")
	}
	if nilRealtimeCUDeploymentInterface(options.deploymentVerifier) {
		return errors.New("profile realtime-cu requires a live deployment verifier")
	}
	return options.deployments.validate()
}
