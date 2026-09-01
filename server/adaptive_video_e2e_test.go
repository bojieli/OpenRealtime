package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/elements"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	"github.com/bojieli/OpenRealtime/gateway"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	adaptivevideo "github.com/bojieli/OpenRealtime/graph/binding/adaptivevideo"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

func TestAdaptiveVideoGraphRunsThroughCompiledServerProfileAndRealtimeWebSocket(t *testing.T) {
	providerDescriptor := perceptionelements.VisualProviderDescriptor{
		Name: "deterministic-youtube-narrator", Revision: "model-test-1",
		Digest: "sha256:" + strings.Repeat("1", 64),
	}
	var providerAcquisitions atomic.Int64
	narrated := make(chan perception.Frame, 1)
	providerDependency, err := adaptivevideo.NewProviderDependency(
		serverArtifact("go://openrealtime/test/adaptive-video-visual-registry", "build-1", "2"),
		[]adaptivevideo.ProviderRegistration{{
			Reference: "visual.youtube.narrator.v1", Descriptor: providerDescriptor,
			Factory: func() (perceptionelements.VisualProvider, error) {
				providerAcquisitions.Add(1)
				return &deterministicVisualProvider{
					descriptor: providerDescriptor, narrated: narrated,
				}, nil
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, catalog, secrets := compileAdaptiveVideoPlan(t, providerDependency)
	adapterArtifact := serverArtifact(
		"go://openrealtime/graph-adapters/adaptive-video", "build-1", "3",
	)
	adapter, err := adaptivevideo.New(plan, adaptivevideo.Config{
		ProfileName: "openrealtime.graph.adaptive-video", ProfileRevision: 1,
		Reference: "go://openrealtime/graph-adapters/adaptive-video",
		Artifact:  adapterArtifact, Ownership: adaptiveVideoOwnership(),
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := adapter.Profile()
	if err := profile.ValidateGraph(plan.Graph()); err != nil {
		t.Fatal(err)
	}
	if !profile.Capabilities.Video || !profile.Capabilities.Observations ||
		!reflect.DeepEqual(profile.Capabilities.Observers, []string{"visual-youtube"}) {
		t.Fatalf("adaptive video profile capabilities = %+v", profile.Capabilities)
	}
	mutatedProfile := adapter.Profile()
	mutatedProfile.Capabilities.Observers[0] = "mutated"
	mutatedRegistration := adapter.Registration()
	mutatedRegistration.Reference = "go://openrealtime/test/mutated"
	if adapter.Profile().Capabilities.Observers[0] != "visual-youtube" ||
		adapter.Registration().Reference != "go://openrealtime/graph-adapters/adaptive-video" {
		t.Fatal("adaptive video adapter accessors aliased the frozen profile or registration")
	}
	nativeBinding, err := graphbinding.NewNative(graphbinding.NativeConfig{
		Plan: plan, Catalog: catalog, SecretCatalog: secrets,
		MountDependencies: providerDependency.MountDependencyFactory(),
		AdapterProfile:    profile, Adapter: adapter.Registration(),
		Inspection: graphruntime.InspectionConfig{
			MaxFlows: 64, MaxEdgesPerFlow: 64, MaxCorrelationBytes: 512,
		},
		TraceRecording: &graphbinding.TraceRecordingConfig{
			MaxRetainedBytes: 512 << 10, CaptureInterval: time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerAcquisitions.Load() != 0 {
		t.Fatal("resource-free plan/profile/native preflight acquired the visual provider")
	}

	captured := &capturingNativeProvider{
		binding: nativeBinding, sessions: make(chan *graphbinding.NativeRuntime, 1),
	}
	providerArtifact := serverArtifact(
		"go://openrealtime/server-providers/adaptive-video", "build-1", "4",
	)
	gatewayArtifact := serverArtifact(
		"go://openrealtime/server-gateways/realtime", "build-1", "5",
	)
	bundle, err := serverplugin.NewBundle(serverplugin.BundleConfig{
		ProfileName: "openrealtime.server.adaptive-video", ProfileRevision: 1,
		Provider: captured,
		Gateway: gateway.Config{
			Model: "adaptive-video-e2e", ValidateWire: true,
		},
		ProviderArtifact: providerArtifact, GatewayArtifact: gatewayArtifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	realm, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close adaptive video server realm: %v", err)
		}
	})
	if providerAcquisitions.Load() != 0 {
		t.Fatal("server profile compilation or mount acquired the visual provider")
	}
	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)

	healthResponse, err := http.Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer healthResponse.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(healthResponse.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	serverProfile, ok := health["server_profile"].(map[string]any)
	if healthResponse.StatusCode != http.StatusOK || !ok ||
		serverProfile["fingerprint"] != bundle.Plan.Fingerprint ||
		serverProfile["realm"] != string(plugin.ServerRealm) {
		t.Fatalf("adaptive video server health = status %d payload %+v",
			healthResponse.StatusCode, health)
	}

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") +
		"/v1/realtime?model=adaptive-video-e2e"
	connection, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
	})
	created := awaitAdaptiveEvent(t, connection, "session.created")
	if created["type"] != "session.created" {
		t.Fatalf("first adaptive video event = %+v", created)
	}
	var nativeRuntime *graphbinding.NativeRuntime
	select {
	case nativeRuntime = <-captured.sessions:
	case <-time.After(5 * time.Second):
		t.Fatal("server profile did not start a native graph session")
	}
	if providerAcquisitions.Load() != 1 {
		t.Fatalf("visual provider acquisitions after one session = %d, want 1",
			providerAcquisitions.Load())
	}

	writeAdaptiveEvent(t, connection, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				},
				"output": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				},
			},
			"openrealtime": map[string]any{
				"version":   openrealtime.Version,
				"supports":  []string{"video.input", "observations"},
				"observers": []string{"visual-youtube"},
				"debug": map[string]any{
					"enabled": true, "categories": []string{"session"},
				},
			},
		},
	})
	updated := awaitAdaptiveEvent(t, connection, "session.updated")
	updatedSession, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated has no session: %+v", updated)
	}
	extension, ok := updatedSession["openrealtime"].(map[string]any)
	if !ok || !jsonStringSetEqual(extension["enabled"], "video.input", "observations") ||
		!jsonStringSetEqual(extension["observers"], "visual-youtube") {
		t.Fatalf("adaptive video negotiation = %+v", extension)
	}
	debug, ok := extension["debug"].(map[string]any)
	if !ok {
		t.Fatalf("adaptive video negotiation omitted debug projection: %+v", extension)
	}
	encodedAccess, err := json.Marshal(debug["inspection"])
	if err != nil {
		t.Fatal(err)
	}
	var inspectionAccess openrealtime.InspectionAccess
	if err := json.Unmarshal(encodedAccess, &inspectionAccess); err != nil {
		t.Fatal(err)
	}
	if inspectionAccess.SessionID == "" || inspectionAccess.Path == "" ||
		inspectionAccess.Token == "" || inspectionAccess.ExpiresAtMS <= time.Now().UnixMilli() {
		t.Fatalf("adaptive video inspection access = %+v", inspectionAccess)
	}
	writeAdaptiveEvent(t, connection, map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "youtube",
		"state": "active", "width": 64, "height": 48,
	})
	writeAdaptiveEvent(t, connection, map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "youtube",
		"frame": adaptiveVideoJPEG(t), "timestamp_ms": 1000,
	})
	select {
	case frame := <-narrated:
		if frame.Source != "youtube" || frame.CapturedNS != uint64(time.Second) || frame.Index != 1 {
			t.Fatalf("visual provider received frame %+v", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("adaptive video frame did not reach the visual provider; graph live = %+v",
			nativeRuntime.Live())
	}
	observation := awaitAdaptiveEvent(t, connection, openrealtime.EventObservationAdded)
	if observation["text"] != deterministicNarration ||
		observation["observer"] != "visual-youtube" || observation["source"] != "youtube" ||
		observation["authority"] != string(trajectory.AuthorityObserver) {
		t.Fatalf("adaptive video observation = %+v", observation)
	}

	graphLive := awaitAdaptiveGraphLive(t, nativeRuntime)
	if graphLive.Fingerprint != plan.Graph().Fingerprint ||
		graphLive.Configuration == nil || graphLive.Configuration.Digest != plan.Identity().ValuesDigest ||
		graphLive.Adapter == nil || graphLive.Adapter.ProfileFingerprint != profile.Fingerprint ||
		graphLive.Adapter.Runtime != adapterArtifact ||
		graphLive.Adapter.RuntimeEvidence != inspect.EvidenceRegistered {
		t.Fatalf("adaptive video graph live evidence = %+v", graphLive)
	}
	vision := graphLive.Nodes["vision"].Resolution
	if vision == nil || vision.RuntimeEvidence != inspect.EvidenceLive ||
		vision.CapabilitiesEvidence != inspect.EvidenceLive || len(vision.Capabilities) != 1 ||
		vision.Capabilities[0].Name != "vision.narration" ||
		vision.Capabilities[0].Provider.ID !=
			"provider://openrealtime/visual/"+providerDescriptor.Name {
		t.Fatalf("adaptive video visual-provider evidence = %+v", vision)
	}

	if want := management.APIPrefix + "/sessions/" +
		inspectionAccess.SessionID + "/live"; inspectionAccess.Path != want {
		t.Fatalf("negotiated inspection path = %q, want %q", inspectionAccess.Path, want)
	}
	canonicalLive := readAdaptiveInspection[inspect.Live](
		t, httpServer.URL+inspectionAccess.Path,
		management.CapabilityHeader, inspectionAccess.Token,
	)
	if canonicalLive.Fingerprint != graphLive.Fingerprint ||
		canonicalLive.Configuration == nil ||
		canonicalLive.Configuration.Digest != graphLive.Configuration.Digest {
		t.Fatalf("composed management route lost graph identity: canonical=%+v", canonicalLive)
	}

	serverLive := realm.Live()
	if serverLive.Fingerprint != bundle.Plan.Fingerprint || serverLive.Realm != plugin.ServerRealm ||
		serverLive.Entries["sessions"].Runtime != providerArtifact ||
		serverLive.Entries["gateway"].Runtime != gatewayArtifact ||
		serverLive.Entries["http-router"].Runtime != gatewayArtifact ||
		serverLive.Entries["inspection"].Runtime != gatewayArtifact ||
		serverLive.Entries["observability"].Runtime != gatewayArtifact ||
		serverLive.Entries["session-api"].Runtime != gatewayArtifact ||
		len(serverLive.Entries) != 7 ||
		!serverLive.Exports[serverplugin.RealtimeHTTPExport].Available {
		t.Fatalf("adaptive video server realm evidence = %+v", serverLive)
	}

	trace, err := nativeRuntime.RecordedTrace()
	if err != nil {
		t.Fatal(err)
	}
	if trace.Adapter == nil || !reflect.DeepEqual(trace.Adapter, graphLive.Adapter) ||
		trace.Adapter.ProfileFingerprint != profile.Fingerprint ||
		trace.Adapter.BoundaryMapDigest != profile.BoundaryMapDigest ||
		trace.Adapter.ProjectionDigest != profile.ProjectionDigest {
		t.Fatalf("adaptive video recorded adapter evidence = %+v", trace.Adapter)
	}
	canonicalTrace := readAdaptiveInspection[inspect.LiveTrace](
		t,
		httpServer.URL+management.APIPrefix+"/sessions/"+inspectionAccess.SessionID+"/trace",
		management.CapabilityHeader,
		inspectionAccess.Token,
	)
	if !reflect.DeepEqual(canonicalTrace.Adapter, trace.Adapter) ||
		canonicalTrace.Graph.Fingerprint != trace.Graph.Fingerprint {
		t.Fatalf("composed management trace lost exact evidence: %+v", canonicalTrace)
	}
	encodedTrace, err := inspect.MarshalLiveTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	parsedTrace, err := inspect.ParseLiveTrace(encodedTrace)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsedTrace.Adapter, trace.Adapter) {
		t.Fatalf("parsed adaptive video adapter evidence = %+v, want %+v",
			parsedTrace.Adapter, trace.Adapter)
	}
	replayer, err := inspect.NewTraceReplayer(nativeRuntime.Graph(), parsedTrace)
	if err != nil {
		t.Fatal(err)
	}
	firstReplay, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	secondReplay, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstReplay, secondReplay) ||
		!reflect.DeepEqual(firstReplay.Adapter, trace.Adapter) {
		t.Fatalf("adaptive video trace replay is not deterministic or lost adapter evidence: %+v / %+v",
			firstReplay, secondReplay)
	}

	assertNoCompatibilityIdentity(t, struct {
		Graph   inspect.Live         `json:"graph"`
		Trace   inspect.LiveTrace    `json:"trace"`
		Replay  inspect.TraceOverlay `json:"replay"`
		Server  any                  `json:"server"`
		Profile any                  `json:"profile"`
		Status  legacy.Status        `json:"status"`
	}{
		Graph: graphLive, Trace: trace, Replay: firstReplay, Server: serverLive,
		Profile: profile, Status: nativeRuntime.Status(),
	})
}

func readAdaptiveInspection[T any](
	t *testing.T, endpoint, header, token string,
) T {
	t.Helper()
	var result T
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(header, token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("inspection %s = status %d headers %v", endpoint, response.StatusCode, response.Header)
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func compileAdaptiveVideoPlan(
	t testing.TB, dependency adaptivevideo.ProviderDependency,
) (*graphconfig.Plan, graphassembly.Catalog, *graphsecret.Document) {
	t.Helper()
	fixture := newAdaptiveVideoGraphFixture(t, dependency)
	discovery, err := fixture.catalog.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	options := fixture.options
	options.Discovery = discovery
	options.SecretCatalog = fixture.secrets
	plan, err := graphconfig.Create(context.Background(), fixture.artifacts, options)
	if err != nil {
		t.Fatal(err)
	}
	return plan, fixture.catalog, fixture.secrets
}

type adaptiveVideoGraphFixture struct {
	artifacts graphconfig.Artifacts
	options   graphconfig.Options
	catalog   graphassembly.Catalog
	secrets   *graphsecret.Document
	evidence  graphevidence.Document
}

func newAdaptiveVideoGraphFixture(
	t testing.TB, dependency adaptivevideo.ProviderDependency,
) adaptiveVideoGraphFixture {
	t.Helper()
	descriptors, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.AssemblyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog.Dependencies = append(catalog.Dependencies, dependency.AssemblyDependency())
	schemas, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		t.Fatal(err)
	}
	secretsBody := readAdaptiveVideoArtifact(t, "agent.secrets.yaml")
	secrets, err := graphsecret.ParseYAML("agent.secrets.yaml", secretsBody)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := graphevidence.ParseYAML(
		"agent.evidence.yaml", readAdaptiveVideoArtifact(t, "agent.evidence.yaml"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if secrets.Catalog != "adaptive_video" || evidence.Graph != "adaptive_video" ||
		len(secrets.Secrets) != 0 || len(evidence.Profiles) != 0 {
		t.Fatalf("adaptive video auxiliary artifacts = secrets %+v evidence %+v", secrets, evidence)
	}
	return adaptiveVideoGraphFixture{artifacts: graphconfig.Artifacts{
		Topology: graphconfig.Artifact{
			Path: "agent.ortg", Data: readAdaptiveVideoArtifact(t, "agent.ortg"),
		},
		Values: graphconfig.Artifact{
			Path: "agent.values.yaml", Data: readAdaptiveVideoArtifact(t, "agent.values.yaml"),
		},
		Lock: graphconfig.Artifact{
			Path: "openrealtime.lock", Data: readAdaptiveVideoArtifact(t, "openrealtime.lock"),
		},
		Deployment: graphconfig.Artifact{
			Path: "agent.deployment.yaml", Data: readAdaptiveVideoArtifact(t, "agent.deployment.yaml"),
		},
	}, options: graphconfig.Options{
		Catalog: descriptors, SchemaResolver: schemas, Loader: graphcompiler.FileLoader{},
	}, catalog: catalog, secrets: &secrets, evidence: evidence}
}

func adaptiveVideoLaunchConfig(
	t testing.TB,
	dependency adaptivevideo.ProviderDependency,
	adapterArtifact inspect.ArtifactIdentity,
) graphlaunch.Config {
	t.Helper()
	fixture := newAdaptiveVideoGraphFixture(t, dependency)
	const adapterReference = "go://openrealtime/graph-adapters/adaptive-video"
	const profileName = "openrealtime.graph.adaptive-video"
	const profileRevision = uint64(1)
	adapterPlugin := graphlaunch.AdapterPlugin{
		Reference: adapterReference,
		Artifact:  adapterArtifact,
		Bind: func(_ context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			adapter, err := adaptivevideo.New(plan, adaptivevideo.Config{
				ProfileName: profileName, ProfileRevision: profileRevision,
				Reference: adapterReference, Artifact: adapterArtifact,
				Ownership: adaptiveVideoOwnership(),
			})
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			return graphlaunch.BoundAdapter{
				Profile: adapter.Profile(), Registration: adapter.Registration(),
			}, nil
		},
	}
	providerMetadata := dependency.AssemblyDependency()
	return graphlaunch.Config{
		Artifacts: fixture.artifacts, PlanOptions: fixture.options,
		GraphMetadata: graphcatalog.Metadata{
			Stage: graphcatalog.Experimental, Summary: "Adaptive-video server test graph.",
			Change: "Initial test graph revision.",
		},
		Catalog: graphlaunch.Catalog{
			Assembly: fixture.catalog,
			Adapters: []graphlaunch.AdapterPlugin{adapterPlugin},
			MountDependencies: []graphlaunch.MountDependencyPlugin{{
				Name: providerMetadata.Name, Artifact: providerMetadata.Artifact,
				Factory: dependency.MountDependencyFactory(),
			}},
		},
		SecretCatalog: fixture.secrets,
		Evidence:      fixture.evidence,
		Adapter:       adapterPlugin.Selection(profileName, profileRevision),
		Inspection: graphruntime.InspectionConfig{
			MaxFlows: 64, MaxEdgesPerFlow: 64, MaxCorrelationBytes: 512,
		},
		TraceRecording: &graphbinding.TraceRecordingConfig{
			MaxRetainedBytes: 512 << 10, CaptureInterval: time.Millisecond,
		},
	}
}

func readAdaptiveVideoArtifact(t testing.TB, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile("../graphs/components/adaptive-video/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

const deterministicNarration = "A deterministic YouTube frame shows the graph-native observation path."

type deterministicVisualProvider struct {
	descriptor perceptionelements.VisualProviderDescriptor
	narrated   chan<- perception.Frame
}

func (provider *deterministicVisualProvider) Name() string { return provider.descriptor.Name }
func (provider *deterministicVisualProvider) Descriptor() perceptionelements.VisualProviderDescriptor {
	return provider.descriptor
}
func (provider *deterministicVisualProvider) Narrate(
	_ context.Context, frames []perception.Frame, _ trajectory.Snapshot,
) (string, error) {
	if len(frames) != 1 || frames[0].Kind != perception.FrameImage ||
		frames[0].Source != "youtube" {
		return "", errors.New("deterministic visual provider received the wrong frame contract")
	}
	select {
	case provider.narrated <- frames[0]:
	default:
		return "", errors.New("deterministic visual provider received an excess frame")
	}
	return deterministicNarration, nil
}

type capturingNativeProvider struct {
	binding  *graphbinding.NativeBinding
	sessions chan *graphbinding.NativeRuntime
}

func (provider *capturingNativeProvider) Name() string { return provider.binding.Name() }
func (provider *capturingNativeProvider) Ownership() legacy.Ownership {
	return provider.binding.Ownership()
}
func (provider *capturingNativeProvider) Capabilities() legacy.Capabilities {
	return provider.binding.Capabilities()
}
func (provider *capturingNativeProvider) Start(
	ctx context.Context, options legacy.Options,
) (legacy.Runtime, error) {
	live, err := provider.binding.Start(ctx, options)
	if err != nil {
		return nil, err
	}
	native, ok := live.(*graphbinding.NativeRuntime)
	if !ok {
		return nil, errors.New("adaptive video provider did not start a native runtime")
	}
	select {
	case provider.sessions <- native:
	default:
		return nil, errors.New("adaptive video capture received an excess session")
	}
	return live, nil
}

func adaptiveVideoOwnership() legacy.Ownership {
	return legacy.Ownership{
		Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
		Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
	}
}

func adaptiveVideoJPEG(t testing.TB) string {
	t.Helper()
	canvas := image.NewGray(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			shade := uint8(24)
			if x >= 12 && x < 52 && y >= 10 && y < 38 {
				shade = 224
			}
			canvas.SetGray(x, y, color.Gray{Y: shade})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func writeAdaptiveEvent(t testing.TB, connection *websocket.Conn, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func awaitAdaptiveEvent(
	t testing.TB, connection *websocket.Conn, eventType string,
) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("await adaptive video event %s: %v", eventType, err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == eventType {
			return event
		}
		if event["type"] == "error" {
			t.Fatalf("await adaptive video event %s received error: %+v", eventType, event)
		}
	}
}

func jsonStringSetEqual(value any, expected ...string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(expected) {
		return false
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return false
		}
		seen[text] = struct{}{}
	}
	for _, item := range expected {
		if _, found := seen[item]; !found {
			return false
		}
	}
	return true
}

func awaitAdaptiveGraphLive(
	t testing.TB, runtime *graphbinding.NativeRuntime,
) inspect.Live {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		live := runtime.Live()
		ready := live.State == "running"
		for _, node := range []string{"ingress", "policy", "vision"} {
			resolution := live.Nodes[node].Resolution
			ready = ready && resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive
		}
		if ready {
			return live
		}
		if time.Now().After(deadline) {
			t.Fatalf("adaptive video graph never reported live exact resolution: %+v", live)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertNoCompatibilityIdentity(t testing.TB, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{
		"elements.compatbinding", "compat.binding/", "compat_", "legacy://",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("adaptive video evidence contains compatibility identity %q: %s",
				forbidden, payload)
		}
	}
}
