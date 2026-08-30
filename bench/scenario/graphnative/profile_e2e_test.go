package graphnative_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	graphnative "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

const (
	scenarioApplicationReference = "go://openrealtime/test/scenario-application/v1"
	scenarioDelegateReference    = "go://openrealtime/test/scenario-delegate/v1"
	scenarioProtocolModel        = "scenario-graph-profile-e2e"
	scenarioInstructionPrefix    = "openrealtime-scenario-protocol-e2e:"
)

type profiledScenarioFixture struct {
	launch          scenarioLaunchFixture
	contract        graphnative.Contract
	delegate        launchprofile.Registration
	application     launchprofile.Registration
	configuration   json.RawMessage
	profile         launchprofile.Document
	registry        *launchprofile.Registry
	gatewayArtifact inspect.ArtifactIdentity
	delegateCalls   *atomic.Int64
}

func TestScenarioApplicationPluginFreezesExactResourceFreeProfile(t *testing.T) {
	fixture := newProfiledScenarioFixture(t, nil)
	if err := fixture.profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if fixture.profile.Plan.GraphFingerprint == "" ||
		fixture.profile.Plan.PlanFingerprint == "" ||
		fixture.profile.Application.Reference != scenarioApplicationReference ||
		fixture.profile.Server.ProviderArtifact != fixture.application.ProviderArtifact {
		t.Fatalf("scenario launch profile omitted an exact identity: %+v", fixture.profile)
	}
	if fixture.launch.counters.binders.Load() != 1 ||
		fixture.launch.counters.adapterFactories.Load() != 0 ||
		fixture.launch.counters.elementMounts.Load() != 0 ||
		fixture.delegateCalls.Load() != 1 {
		t.Fatalf("profile freeze acquired resources: launch=%+v delegate_calls=%d",
			scenarioCounterSnapshot(fixture.launch.counters), fixture.delegateCalls.Load())
	}

	var configured struct {
		FormatVersion       uint64 `json:"format_version"`
		ContractFingerprint string `json:"contract_fingerprint"`
		Cases               []string
		Delegate            struct {
			Reference        string                   `json:"reference"`
			Artifact         inspect.ArtifactIdentity `json:"artifact"`
			ProviderArtifact inspect.ArtifactIdentity `json:"provider_artifact"`
			Configuration    map[string]any           `json:"configuration"`
		} `json:"delegate"`
	}
	if err := json.Unmarshal(fixture.profile.Application.Configuration, &configured); err != nil {
		t.Fatal(err)
	}
	wantNames := scenarioNames(fixture.contract)
	if configured.FormatVersion != graphnative.ApplicationConfigurationFormatVersion ||
		configured.ContractFingerprint != fixture.contract.Fingerprint ||
		!slices.Equal(configured.Cases, wantNames) || len(configured.Cases) != 11 ||
		configured.Delegate.Reference != fixture.delegate.Reference ||
		configured.Delegate.Artifact != fixture.delegate.Artifact ||
		configured.Delegate.ProviderArtifact != fixture.delegate.ProviderArtifact ||
		configured.Delegate.Configuration["profile"] != "scenario-e2e" {
		t.Fatalf("profile application configuration drifted: %+v", configured)
	}

	payload, err := launchprofile.MarshalYAML(fixture.profile)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := launchprofile.ParseYAML("scenario-profile.yaml", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, fixture.profile) {
		t.Fatal("scenario profile YAML round trip changed the exact document")
	}

	composition, err := serverplugin.NewProfileGraphBundle(
		context.Background(), serverplugin.ProfileGraphBundleConfig{
			Profile: fixture.profile, Applications: fixture.registry,
			GatewayArtifact: fixture.gatewayArtifact,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if composition.GraphPlan.Identity() != fixture.profile.Plan ||
		composition.ServerBundle.Profile.Name != fixture.profile.Server.ProfileName {
		t.Fatal("profile composition changed graph or server identity")
	}
	if fixture.launch.counters.binders.Load() != 2 ||
		fixture.launch.counters.adapterFactories.Load() != 0 ||
		fixture.launch.counters.elementMounts.Load() != 0 ||
		fixture.delegateCalls.Load() != 2 {
		t.Fatalf("profile composition acquired session resources: launch=%+v delegate_calls=%d",
			scenarioCounterSnapshot(fixture.launch.counters), fixture.delegateCalls.Load())
	}
	realm, err := composition.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeScenarioRealm(t, realm)
	if fixture.launch.counters.adapterFactories.Load() != 0 ||
		fixture.launch.counters.elementMounts.Load() != 0 {
		t.Fatal("server mount acquired per-session graph resources")
	}
}

func TestScenarioApplicationPluginRejectsProfileDriftBeforeDelegateOrResources(t *testing.T) {
	fixture := newProfiledScenarioFixture(t, nil)
	baselineDelegateCalls := fixture.delegateCalls.Load()
	baselineBinders := fixture.launch.counters.binders.Load()

	tests := []struct {
		name   string
		mutate func(json.RawMessage) json.RawMessage
		want   string
	}{
		{
			name: "duplicate key",
			mutate: func(source json.RawMessage) json.RawMessage {
				return append([]byte(`{"format_version":1,`), source[1:]...)
			},
			want: "duplicate",
		},
		{
			name: "unknown field",
			mutate: func(source json.RawMessage) json.RawMessage {
				return mutateScenarioJSON(t, source, func(value map[string]any) { value["kind"] = "cascade" })
			},
			want: "unknown field",
		},
		{
			name: "reordered cases",
			mutate: func(source json.RawMessage) json.RawMessage {
				return mutateScenarioJSON(t, source, func(value map[string]any) {
					cases := value["cases"].([]any)
					cases[0], cases[1] = cases[1], cases[0]
				})
			},
			want: "canonical suite order",
		},
		{
			name: "contract fingerprint",
			mutate: func(source json.RawMessage) json.RawMessage {
				return mutateScenarioJSON(t, source, func(value map[string]any) {
					value["contract_fingerprint"] = "sha256:" + strings.Repeat("0", 64)
				})
			},
			want: "contract fingerprint",
		},
		{
			name: "delegate artifact",
			mutate: func(source json.RawMessage) json.RawMessage {
				return mutateScenarioJSON(t, source, func(value map[string]any) {
					delegate := value["delegate"].(map[string]any)
					artifact := delegate["artifact"].(map[string]any)
					artifact["revision"] = "drifted-build"
				})
			},
			want: "delegate plugin identity drifted",
		},
		{
			name: "noncanonical encoding",
			mutate: func(source json.RawMessage) json.RawMessage {
				return append([]byte(" "), source...)
			},
			want: "not canonical JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.application.Factory(
				context.Background(), test.mutate(bytes.Clone(fixture.configuration)),
			)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("scenario application drift error = %v, want %q", err, test.want)
			}
			if fixture.delegateCalls.Load() != baselineDelegateCalls ||
				fixture.launch.counters.binders.Load() != baselineBinders ||
				fixture.launch.counters.adapterFactories.Load() != 0 ||
				fixture.launch.counters.elementMounts.Load() != 0 {
				t.Fatal("rejected scenario profile reached delegate, binder, adapter, or mount")
			}
		})
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("scenario profile canceled")
	cancel(want)
	if _, err := fixture.application.Factory(ctx, fixture.configuration); !errors.Is(err, want) {
		t.Fatalf("canceled scenario application error = %v, want %v", err, want)
	}
	if fixture.delegateCalls.Load() != baselineDelegateCalls ||
		fixture.launch.counters.binders.Load() != baselineBinders {
		t.Fatal("canceled scenario profile reached delegate or binder")
	}

	driftedServer := fixture.profile.Server
	driftedServer.ProviderArtifact = scenarioArtifact("server/other-scenario-provider", "1")
	if _, err := graphnative.FreezeLaunchProfile(context.Background(), graphnative.LaunchProfileConfig{
		Name: fixture.profile.Name, Revision: fixture.profile.Revision,
		Contract: fixture.contract, Application: fixture.application,
		Configuration: fixture.configuration, Server: driftedServer,
	}); err == nil || !strings.Contains(err.Error(), "provider artifact differs") {
		t.Fatalf("provider drift profile error = %v", err)
	}
	if fixture.delegateCalls.Load() != baselineDelegateCalls ||
		fixture.launch.counters.binders.Load() != baselineBinders {
		t.Fatal("provider drift reached application or binder")
	}
}

func TestProfiledGraphNativeWebSocketExercisesExactElevenScenarioContract(t *testing.T) {
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	if contract.Fingerprint != "sha256:b450d1e6147c50b114be3f676ec2cb53cbcc1dcfea2c7b1d52fae71eb222d44f" {
		t.Fatalf("full scenario contract fingerprint = %s", contract.Fingerprint)
	}
	probe := newScenarioProtocolProbe(contract)
	fixture := newProfiledScenarioFixture(t, func(
		_ context.Context,
		_ *graphruntime.Mounted,
		options legacy.Options,
		_ graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		fixtureAdapter := probe.adapter(options.Sink)
		return fixtureAdapter, nil
	})

	composition, err := serverplugin.NewProfileGraphBundle(
		context.Background(), serverplugin.ProfileGraphBundleConfig{
			Profile: fixture.profile, Applications: fixture.registry,
			GatewayArtifact: fixture.gatewayArtifact,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.launch.counters.adapterFactories.Load() != 0 ||
		fixture.launch.counters.elementMounts.Load() != 0 {
		t.Fatal("profile composition acquired session resources")
	}
	realm, err := composition.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeScenarioRealm(t, realm)
	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		"/v1/realtime?model=" + scenarioProtocolModel

	for _, requirement := range contract.Cases {
		t.Run(requirement.Name, func(t *testing.T) {
			client := dialScenarioProtocolClient(t, endpoint)
			defer client.close()
			created := client.await("session.created")
			if created["type"] != "session.created" {
				t.Fatalf("session created event = %+v", created)
			}
			client.send(scenarioSessionUpdate(requirement.Name, requirement))
			client.await("session.updated")

			switch requirement.Name {
			case "a recorded menu":
				client.send(scenarioAudioAppend())
				call := client.await("response.function_call_arguments.done")
				if call["call_id"] != scenarioMenuCallID || call["name"] != "press_key" {
					t.Fatalf("menu function call = %+v", call)
				}
				client.send(scenarioAudioAppend())
				client.await("response.done")
				client.send(map[string]any{
					"type": "conversation.item.create",
					"item": map[string]any{
						"type": "function_call_output", "call_id": scenarioMenuCallID,
						"output": `{"ok":true,"destination":"order-status"}`,
					},
				})
				client.await("conversation.item.created")
				client.send(map[string]any{"type": "response.create"})
				client.await("response.output_audio.delta")
				client.await("response.done")
			case "telling them what it saw":
				client.send(scenarioImageMessage(t))
				client.await("conversation.item.created")
				client.send(map[string]any{"type": "response.create"})
				client.await("response.output_audio.delta")
				// Audio arrives while the visual response is still open. The
				// adapter ends only after this frame, proving concurrent input/output.
				client.send(scenarioAudioAppend())
				client.await("response.done")
			default:
				client.send(scenarioAudioAppend())
				client.await("response.output_audio.delta")
				// Keep accepting input while speech is open, then finish the turn.
				client.send(scenarioAudioAppend())
				client.await("response.done")
			}

			if requirement.Name == "an ordinary question" {
				client.send(scenarioSessionUpdate(requirement.Name+"|failure", requirement))
				client.await("session.updated")
				client.send(scenarioAudioAppend())
				failure := client.await("error")
				detail, _ := failure["error"].(map[string]any)
				if detail["code"] != "scenario_probe_failure" {
					t.Fatalf("scenario failure event = %+v", failure)
				}
			}
			client.assertResponseLifecycle(t)
		})
	}

	for _, requirement := range contract.Cases {
		for _, operation := range requirement.Operations {
			if operation == graphbinding.AdapterOutputFailed {
				continue
			}
			if probe.count(requirement.Name, operation) == 0 {
				t.Errorf("scenario %q did not cross %s", requirement.Name, operation)
			}
		}
	}
	for _, operation := range contract.Operations {
		if probe.total(operation) == 0 {
			t.Errorf("full scenario suite did not exercise %s", operation)
		}
	}
	if probe.sessionCount.Load() != 11 {
		t.Fatalf("scenario adapter sessions = %d, want 11", probe.sessionCount.Load())
	}
	if fixture.launch.counters.elementMounts.Load() != 11 {
		t.Fatalf("scenario graph mounts = %d, want 11", fixture.launch.counters.elementMounts.Load())
	}
	if fixture.launch.counters.adapterFactories.Load() != 11 {
		t.Fatalf("scenario adapter factories = %d, want 11", fixture.launch.counters.adapterFactories.Load())
	}
}

func newProfiledScenarioFixture(
	t testing.TB, adapterFactory graphbinding.AdapterFactory,
) profiledScenarioFixture {
	t.Helper()
	launch := newScenarioLaunchFixture(t)
	if adapterFactory != nil {
		plugin := launch.config.Launch.Catalog.Adapters[0]
		binder := plugin.Bind
		plugin.Bind = func(ctx context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			bound, err := binder(ctx, plan)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			bound.Registration.Factory = func(
				ctx context.Context,
				mounted *graphruntime.Mounted,
				options legacy.Options,
				profile graphbinding.SessionAdapterProfile,
			) (graphbinding.SessionAdapter, error) {
				launch.counters.adapterFactories.Add(1)
				return adapterFactory(ctx, mounted, options, profile)
			}
			return bound, nil
		}
		launch.config.Launch.Catalog.Adapters[0] = plugin
	}
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	providerArtifact := scenarioArtifact("server/scenario-profile-provider", "build-e2e-1")
	delegateCalls := &atomic.Int64{}
	delegate := launchprofile.Registration{
		Reference:        scenarioDelegateReference,
		Artifact:         scenarioArtifact("application/scenario-delegate", "build-e2e-1"),
		ProviderArtifact: providerArtifact,
		Factory: func(ctx context.Context, source json.RawMessage) (graphlaunch.Config, error) {
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			var config struct {
				Profile  string `json:"profile"`
				Revision uint64 `json:"revision"`
			}
			decoder := json.NewDecoder(bytes.NewReader(source))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&config); err != nil {
				return graphlaunch.Config{}, err
			}
			if config.Profile != "scenario-e2e" || config.Revision != 1 {
				return graphlaunch.Config{}, errors.New("wrong scenario delegate configuration")
			}
			delegateCalls.Add(1)
			return launch.config.Launch, nil
		},
	}
	application, err := graphnative.NewApplicationPlugin(graphnative.ApplicationPluginConfig{
		Reference: scenarioApplicationReference,
		Artifact:  scenarioArtifact("application/scenario-contract", "build-e2e-1"),
		Delegate:  delegate,
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := graphnative.FreezeApplicationConfiguration(
		contract, delegate, json.RawMessage(`{"profile":"scenario-e2e","revision":1}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	gatewayArtifact := scenarioArtifact("server/scenario-profile-gateway", "build-e2e-1")
	profile, err := graphnative.FreezeLaunchProfile(context.Background(), graphnative.LaunchProfileConfig{
		Name: "openrealtime.launch.scenario-full-suite-test", Revision: 1,
		Contract: contract, Application: application, Configuration: configuration,
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.scenario-full-suite-test", ProfileRevision: 1,
			ProviderArtifact: providerArtifact, GatewayArtifact: gatewayArtifact,
			Model: scenarioProtocolModel, TranscriptionModel: "scenario-probe-transcription",
			ValidateWire: true, InspectionTokenTTLMS: 60_000,
			MaxAudioFrameBytes: 1 << 20, VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{application})
	if err != nil {
		t.Fatal(err)
	}
	return profiledScenarioFixture{
		launch: launch, contract: contract, delegate: delegate, application: application,
		configuration: configuration, profile: profile, registry: registry,
		gatewayArtifact: gatewayArtifact, delegateCalls: delegateCalls,
	}
}

func mutateScenarioJSON(
	t testing.TB, source json.RawMessage, mutate func(map[string]any),
) json.RawMessage {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	mutate(value)
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func scenarioNames(contract graphnative.Contract) []string {
	result := make([]string, len(contract.Cases))
	for index, item := range contract.Cases {
		result[index] = item.Name
	}
	return result
}

type scenarioProtocolProbe struct {
	allowed      map[string]graphnative.CaseRequirement
	mu           sync.Mutex
	operations   map[string]map[graphbinding.AdapterOperation]int
	sessionCount atomic.Int64
	nextID       atomic.Uint64
}

func newScenarioProtocolProbe(contract graphnative.Contract) *scenarioProtocolProbe {
	allowed := make(map[string]graphnative.CaseRequirement, len(contract.Cases))
	for _, item := range contract.Cases {
		allowed[item.Name] = item
	}
	return &scenarioProtocolProbe{
		allowed: allowed, operations: make(map[string]map[graphbinding.AdapterOperation]int),
	}
}

func (probe *scenarioProtocolProbe) adapter(sink legacy.Sink) *scenarioProtocolAdapter {
	probe.sessionCount.Add(1)
	return &scenarioProtocolAdapter{probe: probe, sink: sink}
}

func (probe *scenarioProtocolProbe) record(name string, operation graphbinding.AdapterOperation) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.operations[name] == nil {
		probe.operations[name] = make(map[graphbinding.AdapterOperation]int)
	}
	probe.operations[name][operation]++
}

func (probe *scenarioProtocolProbe) count(
	name string, operation graphbinding.AdapterOperation,
) int {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.operations[name][operation]
}

func (probe *scenarioProtocolProbe) total(operation graphbinding.AdapterOperation) int {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	total := 0
	for _, operations := range probe.operations {
		total += operations[operation]
	}
	return total
}

const scenarioMenuCallID = "scenario-menu-call-1"

type scenarioProtocolAdapter struct {
	probe *scenarioProtocolProbe
	sink  legacy.Sink

	mu          sync.Mutex
	caseName    string
	failureNext bool
	audioFrames int
	turnOpen    bool
	speechOpen  bool
	responded   bool
	toolResult  bool
	visualInput bool
	utterance   action.Utterance
}

func (adapter *scenarioProtocolAdapter) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (adapter *scenarioProtocolAdapter) Update(
	_ context.Context, settings legacy.Settings,
) error {
	if !strings.HasPrefix(settings.Instruction, scenarioInstructionPrefix) {
		return nil
	}
	selected := strings.TrimPrefix(settings.Instruction, scenarioInstructionPrefix)
	failure := strings.HasSuffix(selected, "|failure")
	selected = strings.TrimSuffix(selected, "|failure")
	if _, ok := adapter.probe.allowed[selected]; !ok {
		return fmt.Errorf("unknown scenario protocol probe %q", selected)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.caseName != "" && adapter.caseName != selected {
		return errors.New("scenario protocol session changed case identity")
	}
	adapter.caseName = selected
	adapter.failureNext = failure
	adapter.probe.record(selected, graphbinding.AdapterInputUpdate)
	return nil
}

func (adapter *scenarioProtocolAdapter) Audio(ctx context.Context, _ perception.Frame) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.caseName == "" {
		return errors.New("scenario protocol audio arrived before exact session configuration")
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterInputAudio)
	if adapter.failureNext {
		adapter.failureNext = false
		adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputFailed)
		adapter.sink.Failed(ctx, legacy.ErrorEvent{
			Code: "scenario_probe_failure", Message: "deterministic scenario failure boundary",
		})
		return nil
	}
	adapter.audioFrames++
	if err := adapter.emitActivity(ctx); err != nil {
		return err
	}
	if adapter.audioFrames == 1 {
		if err := adapter.emitTranscript(ctx); err != nil {
			return err
		}
	}
	switch adapter.caseName {
	case "a recorded menu":
		if adapter.audioFrames == 1 {
			return adapter.openToolTurn(ctx)
		}
		if adapter.audioFrames == 2 && adapter.turnOpen {
			return adapter.closeTurn(ctx)
		}
	case "telling them what it saw":
		if adapter.speechOpen {
			return adapter.closeSpeechTurn(ctx)
		}
	default:
		if adapter.audioFrames == 1 {
			return adapter.openSpeechTurn(ctx, "scenario protocol response")
		}
		if adapter.audioFrames == 2 && adapter.speechOpen {
			return adapter.closeSpeechTurn(ctx)
		}
	}
	return nil
}

func (adapter *scenarioProtocolAdapter) Video(context.Context, perception.Frame) error {
	return legacy.ErrUnsupported
}

func (adapter *scenarioProtocolAdapter) Text(_ context.Context, input legacy.TextInput) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.caseName != "telling them what it saw" || len(input.Images) == 0 {
		return errors.New("scenario visual probe requires an image in the reviewed visual case")
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterInputText)
	if adapter.visualInput || adapter.responded || adapter.speechOpen {
		return errors.New("scenario visual probe received duplicate image input")
	}
	adapter.visualInput = true
	return nil
}

func (adapter *scenarioProtocolAdapter) ToolResult(
	_ context.Context, result trajectory.ToolResult,
) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.caseName != "a recorded menu" || result.CallID != scenarioMenuCallID ||
		result.Name != "press_key" || len(result.Output) == 0 || result.Error != "" {
		return fmt.Errorf("unexpected scenario menu tool result: %+v", result)
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterInputToolResult)
	adapter.toolResult = true
	return nil
}

func (*scenarioProtocolAdapter) CommitAudio(context.Context) error { return legacy.ErrUnsupported }

func (adapter *scenarioProtocolAdapter) CreateResponse(ctx context.Context) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	switch adapter.caseName {
	case "a recorded menu":
		if !adapter.toolResult {
			return errors.New("scenario response.create arrived before the menu tool result")
		}
		adapter.probe.record(adapter.caseName, graphbinding.AdapterInputCreateResponse)
		if err := adapter.openSpeechTurn(ctx, "order status reached"); err != nil {
			return err
		}
		return adapter.closeSpeechTurn(ctx)
	case "telling them what it saw":
		if !adapter.visualInput || adapter.responded || adapter.speechOpen {
			return errors.New("scenario response.create arrived before one durable visual input")
		}
		adapter.probe.record(adapter.caseName, graphbinding.AdapterInputCreateResponse)
		return adapter.openSpeechTurn(ctx, "the build finished")
	default:
		return errors.New("scenario response.create has no reviewed trigger")
	}
}

func (*scenarioProtocolAdapter) Cancel(context.Context, string) error { return nil }
func (*scenarioProtocolAdapter) Truncate(context.Context, legacy.Truncation) error {
	return legacy.ErrUnsupported
}
func (*scenarioProtocolAdapter) Trajectory() trajectory.Snapshot { return trajectory.Snapshot{} }
func (*scenarioProtocolAdapter) Status() legacy.Status           { return legacy.Status{} }
func (*scenarioProtocolAdapter) Close(context.Context, error) error {
	return nil
}

func (adapter *scenarioProtocolAdapter) emitActivity(ctx context.Context) error {
	activity := legacy.ActivityEvent{ItemID: "scenario-user-audio"}
	if adapter.audioFrames == 1 {
		activity.Started = true
	} else {
		activity.Stopped = true
		activity.AudioEndMS = adapter.audioFrames * 20
	}
	if err := adapter.sink.Activity(ctx, activity); err != nil {
		return err
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputActivity)
	return nil
}

func (adapter *scenarioProtocolAdapter) emitTranscript(ctx context.Context) error {
	if err := adapter.sink.Transcript(ctx, legacy.TranscriptEvent{
		ItemID: "scenario-user-audio", Text: adapter.caseName, Final: true, DurationSec: 0.02,
	}); err != nil {
		return err
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputTranscript)
	return nil
}

func (adapter *scenarioProtocolAdapter) openToolTurn(ctx context.Context) error {
	if err := adapter.sink.TurnBegin(ctx); err != nil {
		return err
	}
	adapter.turnOpen = true
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputTurnBegin)
	if err := adapter.sink.ToolCalls(ctx, legacy.ToolCallEvent{
		InvocationID: "scenario-menu-invocation",
		Calls: []trajectory.ToolCall{{
			CallID: scenarioMenuCallID, Name: "press_key", Arguments: json.RawMessage(`{"digit":"2"}`),
		}},
	}); err != nil {
		return err
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputToolCalls)
	return nil
}

func (adapter *scenarioProtocolAdapter) openSpeechTurn(ctx context.Context, text string) error {
	if adapter.turnOpen || adapter.speechOpen {
		return errors.New("scenario protocol adapter already has an open turn")
	}
	if err := adapter.sink.TurnBegin(ctx); err != nil {
		return err
	}
	adapter.turnOpen = true
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputTurnBegin)
	adapter.utterance = action.Utterance{
		ID: fmt.Sprintf("scenario-utterance-%d", adapter.probe.nextID.Add(1)), Text: text,
	}
	if err := adapter.sink.SpeechBegin(ctx, adapter.utterance); err != nil {
		return err
	}
	adapter.speechOpen = true
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputSpeechBegin)
	if err := adapter.sink.SpeechText(ctx, adapter.utterance, text); err != nil {
		return err
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputSpeechText)
	if err := adapter.sink.SpeechAudio(ctx, adapter.utterance, action.Frame{
		PCM16LE: make([]byte, 960), SampleRateHz: 24_000,
		Duration: 20 * time.Millisecond, Final: true,
	}); err != nil {
		return err
	}
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputSpeechAudio)
	return nil
}

func (adapter *scenarioProtocolAdapter) closeSpeechTurn(ctx context.Context) error {
	if !adapter.speechOpen {
		return errors.New("scenario protocol adapter has no open speech")
	}
	if err := adapter.sink.SpeechEnd(ctx, adapter.utterance, action.Outcome{
		Completed: true, PlayedMS: 20,
	}); err != nil {
		return err
	}
	adapter.speechOpen = false
	adapter.responded = true
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputSpeechEnd)
	return adapter.closeTurn(ctx)
}

func (adapter *scenarioProtocolAdapter) closeTurn(ctx context.Context) error {
	if !adapter.turnOpen {
		return errors.New("scenario protocol adapter has no open turn")
	}
	if err := adapter.sink.TurnEnd(ctx, legacy.TurnOutcome{}); err != nil {
		return err
	}
	adapter.turnOpen = false
	adapter.probe.record(adapter.caseName, graphbinding.AdapterOutputTurnEnd)
	return nil
}

type scenarioProtocolClient struct {
	t          testing.TB
	connection *websocket.Conn
	events     []string
}

func dialScenarioProtocolClient(t testing.TB, endpoint string) *scenarioProtocolClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &scenarioProtocolClient{t: t, connection: connection}
}

func (client *scenarioProtocolClient) send(event map[string]any) {
	client.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload, err := json.Marshal(event)
	if err != nil {
		client.t.Fatal(err)
	}
	if err := client.connection.Write(ctx, websocket.MessageText, payload); err != nil {
		client.t.Fatal(err)
	}
}

func (client *scenarioProtocolClient) await(eventType string) map[string]any {
	client.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, payload, err := client.connection.Read(ctx)
		cancel()
		if err != nil {
			client.t.Fatalf("await %s after %v: %v", eventType, client.events, err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			client.t.Fatal(err)
		}
		seen, _ := event["type"].(string)
		client.events = append(client.events, seen)
		if seen == eventType {
			return event
		}
	}
	client.t.Fatalf("event %s was not observed after %v", eventType, client.events)
	return nil
}

func (client *scenarioProtocolClient) assertResponseLifecycle(t testing.TB) {
	t.Helper()
	created := slices.Index(client.events, "response.created")
	done := slices.Index(client.events, "response.done")
	if created < 0 || done < 0 || created >= done {
		t.Fatalf("Realtime response lifecycle = %v", client.events)
	}
}

func (client *scenarioProtocolClient) close() {
	_ = client.connection.Close(websocket.StatusNormalClosure, "scenario protocol case complete")
}

func scenarioSessionUpdate(
	name string, requirement graphnative.CaseRequirement,
) map[string]any {
	session := map[string]any{
		"type":         "realtime",
		"instructions": scenarioInstructionPrefix + name,
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
		},
	}
	if slices.Contains(requirement.Seams, graphnative.SeamFunctionTool) {
		session["tools"] = []map[string]any{{
			"type": "function", "name": "press_key",
			"description": "Send a keypad tone on the open call.",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{
					"digit": map[string]any{"type": "string"},
				},
			},
		}}
	}
	return map[string]any{"type": "session.update", "session": session}
}

func scenarioAudioAppend() map[string]any {
	return map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(make([]byte, 960)),
	}
}

func scenarioImageMessage(t testing.TB) map[string]any {
	t.Helper()
	payload, err := os.ReadFile("../testdata/build-finished.png")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{
				"type":      "input_image",
				"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload),
			}},
		},
	}
}

func closeScenarioRealm(t testing.TB, realm interface{ Close(context.Context) error }) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close scenario server realm: %v", err)
		}
	})
}

var _ graphbinding.SessionAdapter = (*scenarioProtocolAdapter)(nil)
