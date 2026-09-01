package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/management"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/coder/websocket"
)

func TestLiveInspectionIsAuthenticatedUnguessableAndBoundToOneSession(t *testing.T) {
	const deploymentToken = "deployment-secret"
	server := startInspectionServer(t, time.Minute, deploymentToken, nil)
	first := dialInspection(t, server, deploymentToken)
	second := dialInspection(t, server, deploymentToken)
	first.await("session.created", 5*time.Second)
	second.await("session.created", 5*time.Second)
	firstAccess := negotiateInspection(t, first)
	secondAccess := negotiateInspection(t, second)
	if firstAccess.SessionID == secondAccess.SessionID || firstAccess.Token == secondAccess.Token {
		t.Fatalf("sessions shared inspection authority: %+v %+v", firstAccess, secondAccess)
	}
	encoded := strings.TrimPrefix(firstAccess.Token, "mgmt_")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if encoded == firstAccess.Token || err != nil || len(raw) != 32 ||
		strings.Contains(firstAccess.Path, firstAccess.Token) {
		t.Fatalf("inspection capability is not a header-only 256-bit token: %+v (%v)", firstAccess, err)
	}

	firstCanonical := management.APIPrefix + "/sessions/" + firstAccess.SessionID + "/live"
	secondCanonical := management.APIPrefix + "/sessions/" + secondAccess.SessionID + "/live"
	if firstAccess.Path != firstCanonical || secondAccess.Path != secondCanonical {
		t.Fatalf("negotiated management paths are not canonical: %+v %+v",
			firstAccess, secondAccess)
	}
	status, _, _ := getManagement(t, server, firstAccess.Path, "")
	if status != http.StatusNotFound {
		t.Fatalf("guessable session ID was sufficient authority: status %d", status)
	}
	guessed := "ins_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32))
	status, _, _ = getManagement(t, server, firstAccess.Path, guessed)
	if status != http.StatusNotFound {
		t.Fatalf("guessed capability was distinguishable or accepted: status %d", status)
	}
	status, _, _ = getManagement(t, server, secondAccess.Path, firstAccess.Token)
	if status != http.StatusNotFound {
		t.Fatalf("one session capability inspected another session: status %d", status)
	}
	status, _, _ = getManagement(t, server, firstAccess.Path, firstAccess.Token)
	if status != http.StatusOK {
		t.Fatalf("negotiated capability is unavailable: status %d", status)
	}
	status, _, _ = getManagement(t, server, secondCanonical, firstAccess.Token)
	if status != http.StatusNotFound {
		t.Fatalf("management API allowed cross-session capability use: status %d", status)
	}

	status, payload, headers := getManagement(t, server, firstAccess.Path, firstAccess.Token)
	if status != http.StatusOK {
		t.Fatalf("authorized live inspection: status %d: %s", status, payload)
	}
	if headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("inspection response may be retained or referred: %v", headers)
	}
	var live inspect.Live
	if err := json.Unmarshal(payload, &live); err != nil {
		t.Fatal(err)
	}
	if live.FormatVersion != inspect.LiveFormatVersion || live.GraphID == "" ||
		live.Configuration == nil || live.Configuration.Digest == "" || len(live.Nodes) == 0 {
		t.Fatalf("management response fabricated or dropped exact mount evidence: %+v", live)
	}
	modelPath := management.APIPrefix + "/sessions/" + firstAccess.SessionID + "/model"
	status, payload, _ = getManagement(t, server, modelPath, firstAccess.Token)
	if status != http.StatusOK {
		t.Fatalf("authorized static session model: status %d: %s", status, payload)
	}
	var model inspect.Model
	if err := json.Unmarshal(payload, &model); err != nil {
		t.Fatal(err)
	}
	if err := management.ValidateSessionModel(live, model); err != nil ||
		bytes.Contains(payload, []byte(`"source"`)) {
		t.Fatalf("session model is not the exact source-free live graph: model=%+v err=%v", model, err)
	}
	status, _, _ = getManagement(t, server, modelPath, secondAccess.Token)
	if status != http.StatusNotFound {
		t.Fatalf("another session capability read the static model: status %d", status)
	}

	rotated := negotiateInspection(t, first)
	if rotated.Token == firstAccess.Token {
		t.Fatal("renegotiation retained a plaintext bearer instead of rotating it")
	}
	status, _, _ = getManagement(t, server, firstAccess.Path, firstAccess.Token)
	if status != http.StatusNotFound {
		t.Fatalf("rotated inspection capability remained active: status %d", status)
	}
	status, _, _ = getManagement(t, server, firstCanonical, firstAccess.Token)
	if status != http.StatusNotFound {
		t.Fatalf("canonical API retained the rotated capability: status %d", status)
	}
	status, _, _ = getManagement(t, server, rotated.Path, rotated.Token)
	if status != http.StatusOK {
		t.Fatalf("rotated inspection capability is unavailable: status %d", status)
	}
	status, _, _ = getManagement(t, server, firstCanonical, rotated.Token)
	if status != http.StatusOK {
		t.Fatalf("rotated capability is unavailable on canonical API: status %d", status)
	}

	// An ordinary update reports the durable debug policy, not the bearer that
	// was returned once during its explicit negotiation.
	first.configurePCM16(nil)
	updated := first.await("session.updated", 5*time.Second)
	debug := inspectionDebugResponse(t, updated)
	if access, present := debug["inspection"]; present && access != nil {
		t.Fatalf("ordinary session update replayed inspection authority: %v", access)
	}
	status, _, _ = getManagement(t, server, rotated.Path, rotated.Token)
	if status != http.StatusOK {
		t.Fatalf("ordinary session update revoked live inspection: status %d", status)
	}
}

func TestLiveInspectionExpiresAndIsRevokedWithSessionLifecycle(t *testing.T) {
	server := startInspectionServer(t, 500*time.Millisecond, "", nil)
	client := dialInspection(t, server, "")
	client.await("session.created", 5*time.Second)
	first := negotiateInspection(t, client)
	waitUntil := time.UnixMilli(first.ExpiresAtMS).Add(20 * time.Millisecond)
	if delay := time.Until(waitUntil); delay > 0 {
		time.Sleep(delay)
	}
	status, _, _ := getManagement(t, server, first.Path, first.Token)
	if status != http.StatusNotFound {
		t.Fatalf("expired inspection token remained active: status %d", status)
	}

	second := negotiateInspection(t, client)
	if second.Token == first.Token {
		t.Fatal("expired token was reissued")
	}
	canonical := management.APIPrefix + "/sessions/" + second.SessionID + "/live"
	status, _, _ = getManagement(t, server, second.Path, second.Token)
	if status != http.StatusOK {
		t.Fatalf("renewed inspection token is unavailable: status %d", status)
	}
	status, _, _ = getManagement(t, server, canonical, second.Token)
	if status != http.StatusOK {
		t.Fatalf("renewed capability is unavailable canonically: status %d", status)
	}
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version,
		"debug":   map[string]any{"enabled": false},
	})
	client.await("session.updated", 5*time.Second)
	status, _, _ = getManagement(t, server, second.Path, second.Token)
	if status != http.StatusNotFound {
		t.Fatalf("disabling debug did not revoke inspection token: status %d", status)
	}
	status, _, _ = getManagement(t, server, canonical, second.Token)
	if status != http.StatusNotFound {
		t.Fatalf("disabling debug did not revoke canonical capability: status %d", status)
	}
	third := negotiateInspection(t, client)
	if err := client.connection.Close(websocket.StatusNormalClosure, "test complete"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _, _ = getManagement(t, server,
			management.APIPrefix+"/sessions/"+third.SessionID+"/live", third.Token)
		if status == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session close did not revoke inspection token: status %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveInspectionIsAbsentWhenRuntimeEvidenceIsIncomplete(t *testing.T) {
	server := startInspectionServer(t, time.Minute, "", func(graph ir.Graph) inspect.Live {
		return inspect.Live{
			FormatVersion: inspect.LiveFormatVersion,
			GraphID:       graph.ID, GraphRevision: graph.Revision, Fingerprint: graph.Fingerprint,
			State: "running",
			// A status label or Graph IR fingerprint is not a configuration or a
			// node resolution. The gateway must not fill either from launch flags.
		}
	})
	client := dialInspection(t, server, "")
	client.await("session.created", 5*time.Second)
	client.configurePCM16(inspectionDebugRequest())
	updated := client.await("session.updated", 5*time.Second)
	debug := inspectionDebugResponse(t, updated)
	if access, present := debug["inspection"]; present && access != nil {
		t.Fatalf("incomplete runtime evidence received a management capability: %v", access)
	}
}

func TestLiveInspectionRedactsPayloadDerivedIdentifiersButPreservesConfiguration(t *testing.T) {
	const secret = "PRIVATE-CUSTOMER-PAYLOAD"
	configuration := inspect.ArtifactIdentity{
		ID: "values://redaction-agent", Revision: "openrealtime.ai/config/v1alpha1",
		Digest: inspectionDigest('d'),
	}
	var source inspect.Live
	server := startInspectionServer(t, time.Minute, "", func(graph ir.Graph) inspect.Live {
		nodes := make(map[string]inspect.NodeLive, len(graph.Nodes))
		for _, node := range graph.Nodes {
			nodes[node.ID] = inspect.NodeLive{
				State: "failed", LastTriggerID: secret, LastOutcome: secret, Error: secret,
				Resolution: &inspect.NodeResolution{
					Element: node.Element, Implementation: node.Implementation,
					Runtime: inspect.ArtifactIdentity{
						ID: "runtime://compat/" + node.ID, Revision: "test-build-1",
					},
					RuntimeEvidence: inspect.EvidenceDeclared,
				},
			}
		}
		edges := make(map[string]inspect.EdgeLive, len(graph.Edges)+len(graph.Boundaries))
		for _, edge := range graph.Edges {
			edges[edge.ID] = inspect.EdgeLive{}
		}
		for _, boundary := range graph.Boundaries {
			edges[ir.BoundaryQueuePrefix+boundary.Name] = inspect.EdgeLive{}
		}
		boundary := edges[ir.BoundaryQueuePrefix+"text"]
		boundary.LastItemID = secret
		edges[ir.BoundaryQueuePrefix+"text"] = boundary
		source = inspect.Live{
			FormatVersion: inspect.LiveFormatVersion,
			GraphID:       graph.ID, GraphRevision: graph.Revision, Fingerprint: graph.Fingerprint,
			Configuration: &configuration, Sequence: 9, State: "closed", Error: secret,
			Nodes: nodes, Edges: edges,
			Flows: map[string]inspect.FlowLive{secret: {
				Correlation: secret, Edges: []string{"edge-safe"},
			}},
		}
		return source
	})
	client := dialInspection(t, server, "")
	client.await("session.created", 5*time.Second)
	access := negotiateInspection(t, client)
	status, payload, _ := getManagement(t, server, access.Path, access.Token)
	if status != http.StatusOK {
		t.Fatalf("read redacted inspection: status %d: %s", status, payload)
	}
	if bytes.Contains(payload, []byte(secret)) {
		t.Fatalf("inspection response leaked payload-derived data: %s", payload)
	}
	var live inspect.Live
	if err := json.Unmarshal(payload, &live); err != nil {
		t.Fatal(err)
	}
	if live.Configuration == nil || *live.Configuration != configuration {
		t.Fatalf("redaction changed exact configuration identity: %+v", live.Configuration)
	}
	if live.Error != "redacted" || live.Nodes[graphNodeID(live)].Error != "redacted" ||
		live.Edges["boundary:text"].LastItemID != "" {
		t.Fatalf("free-form management fields were not safely redacted: %+v", live)
	}
	if len(live.Flows) != 1 || live.Flows["flow_000001"].Correlation != "flow_000001" {
		t.Fatalf("payload-derived correlation was not replaced consistently: %+v", live.Flows)
	}
	if source.Error != secret || source.Nodes[graphNodeID(source)].Error != secret ||
		source.Edges["boundary:text"].LastItemID != secret || source.Flows[secret].Correlation != secret {
		t.Fatalf("redaction mutated the runtime-owned live snapshot: %+v", source)
	}
}

func TestCanonicalManagementTraceAndDeltasRequireExplicitRecording(t *testing.T) {
	artifact := inspect.ArtifactIdentity{
		ID: "go://openrealtime/test/gateway-compat-binding", Revision: "test-build-1",
		Digest: inspectionDigest('f'),
	}
	server := startInspectionServerWithGraphConfig(t, time.Minute, "", nil, graphbinding.Config{
		ImplementationArtifact: &artifact,
		TraceRecording: &graphbinding.TraceRecordingConfig{
			MaxRetainedBytes: 64 << 10,
			CaptureInterval:  time.Millisecond,
		},
	})
	client := dialInspection(t, server, "")
	client.await("session.created", 5*time.Second)
	access := negotiateInspection(t, client)
	const privateText = "PRIVATE-MANAGEMENT-TRACE-PAYLOAD"
	client.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": privateText}},
		},
	})
	client.await("conversation.item.created", 5*time.Second)
	base := management.APIPrefix + "/sessions/" + access.SessionID

	status, payload, _ := getManagement(t, server, base+"/trace", access.Token)
	if status != http.StatusOK {
		t.Fatalf("canonical recorded trace status = %d: %s", status, payload)
	}
	if bytes.Contains(payload, []byte(privateText)) {
		t.Fatalf("canonical trace retained conversation payload: %s", payload)
	}
	var trace inspect.LiveTrace
	if err := json.Unmarshal(payload, &trace); err != nil {
		t.Fatal(err)
	}
	if err := trace.Validate(); err != nil {
		t.Fatalf("canonical trace is not exact replay evidence: %v", err)
	}
	if len(trace.Snapshots) == 0 || trace.Configuration.Digest == "" {
		t.Fatalf("canonical trace lacks baseline/configuration evidence: %+v", trace)
	}

	status, payload, _ = getManagement(t, server, base+"/deltas?after=0&limit=32", access.Token)
	if status != http.StatusOK {
		t.Fatalf("canonical deltas status = %d: %s", status, payload)
	}
	var page management.DeltaPage
	if err := json.Unmarshal(payload, &page); err != nil {
		t.Fatal(err)
	}
	if err := management.ValidateDeltaPage(access.SessionID, 0, 32, page); err != nil {
		t.Fatalf("canonical delta page is invalid: %v", err)
	}
	if page.Baseline == nil {
		t.Fatalf("initial delta page has no exact baseline: %+v", page)
	}

	disabledServer := startInspectionServer(t, time.Minute, "", nil)
	disabledClient := dialInspection(t, disabledServer, "")
	disabledClient.await("session.created", 5*time.Second)
	disabledAccess := negotiateInspection(t, disabledClient)
	disabledBase := management.APIPrefix + "/sessions/" + disabledAccess.SessionID
	for _, path := range []string{disabledBase + "/trace", disabledBase + "/deltas?after=0&limit=32"} {
		status, payload, _ = getManagement(t, disabledServer, path, disabledAccess.Token)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("recording-disabled endpoint %s fabricated evidence: %d %s", path, status, payload)
		}
	}
}

type inspectionSnapshotBinding struct {
	binding.Binding
	snapshot inspect.Live
}

func (binding inspectionSnapshotBinding) Start(
	ctx context.Context, options binding.Options,
) (binding.Runtime, error) {
	runtime, err := binding.Binding.Start(ctx, options)
	if err != nil {
		return nil, err
	}
	return &inspectionSnapshotRuntime{Runtime: runtime, snapshot: binding.snapshot}, nil
}

type inspectionSnapshotRuntime struct {
	binding.Runtime
	snapshot inspect.Live
}

// Return the snapshot exactly as supplied so the management boundary itself,
// rather than this test double, must preserve runtime-owned maps while redacting.
func (runtime *inspectionSnapshotRuntime) Live() inspect.Live { return runtime.snapshot }

func (runtime *inspectionSnapshotRuntime) Graph() ir.Graph {
	graph, _ := runtime.Runtime.(interface{ Graph() ir.Graph })
	if graph == nil {
		return ir.Graph{}
	}
	return graph.Graph()
}

func startInspectionServer(
	t *testing.T, ttl time.Duration, token string, snapshot func(ir.Graph) inspect.Live,
) *httptest.Server {
	t.Helper()
	return startInspectionServerWithGraphConfig(t, ttl, token, snapshot, graphbinding.Config{})
}

func startInspectionServerWithGraphConfig(
	t *testing.T,
	ttl time.Duration,
	token string,
	snapshot func(ir.Graph) inspect.Live,
	graphConfig graphbinding.Config,
) *httptest.Server {
	t.Helper()
	legacy, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hello"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{}, Voice: "test-voice", FastMaxTokens: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphbinding.NewWithConfig(legacy, graphConfig)
	if err != nil {
		t.Fatal(err)
	}
	var served binding.Binding = mounted
	if snapshot != nil {
		served = inspectionSnapshotBinding{Binding: mounted, snapshot: snapshot(mounted.Graph())}
	}
	server, err := gateway.New(gateway.Config{
		Binding: served, Model: "openrealtime-test", ValidateWire: true,
		Token: token, InspectionTokenTTL: ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(testGatewayHandler(server))
	t.Cleanup(func() {
		httpServer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close gateway management realm: %v", err)
		}
	})
	return httpServer
}

func dialInspection(t *testing.T, server *httptest.Server, token string) *client {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=openrealtime-test"
	options := &websocket.DialOptions{}
	if token != "" {
		options.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	connection, _, err := websocket.Dial(context.Background(), url, options)
	if err != nil {
		t.Fatalf("dial inspection session: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	return &client{connection: connection, t: t, validator: protocol.NewValidator()}
}

func inspectionDebugRequest() map[string]any {
	return map[string]any{
		"version": openrealtime.Version,
		"debug":   map[string]any{"enabled": true, "categories": []string{"session"}},
	}
}

func negotiateInspection(t *testing.T, client *client) openrealtime.InspectionAccess {
	t.Helper()
	client.configurePCM16(inspectionDebugRequest())
	updated := client.await("session.updated", 5*time.Second)
	debug := inspectionDebugResponse(t, updated)
	raw, present := debug["inspection"]
	if !present || raw == nil {
		t.Fatalf("session did not receive live inspection access: %v", debug)
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var access openrealtime.InspectionAccess
	if err := json.Unmarshal(payload, &access); err != nil {
		t.Fatal(err)
	}
	if access.SessionID == "" || access.Path == "" || access.Token == "" || access.ExpiresAtMS == 0 {
		t.Fatalf("incomplete live inspection access: %+v", access)
	}
	return access
}

func inspectionDebugResponse(t *testing.T, updated map[string]any) map[string]any {
	t.Helper()
	session, ok := updated["session"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated has no session: %v", updated)
	}
	extension, ok := session["openrealtime"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated has no OpenRealtime response: %v", session)
	}
	debug, ok := extension["debug"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated has no debug response: %v", extension)
	}
	return debug
}

func getManagement(
	t *testing.T, server *httptest.Server, path, capability string,
) (int, []byte, http.Header) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capability != "" {
		request.Header.Set(management.CapabilityHeader, capability)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, payload, response.Header.Clone()
}

func inspectionDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}

func graphNodeID(live inspect.Live) string {
	for id := range live.Nodes {
		return id
	}
	return ""
}
