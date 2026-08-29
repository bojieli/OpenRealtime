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
	encoded := strings.TrimPrefix(firstAccess.Token, "ins_")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 || strings.Contains(firstAccess.Path, firstAccess.Token) {
		t.Fatalf("inspection capability is not a header-only 256-bit token: %+v (%v)", firstAccess, err)
	}

	status, _, _ := getInspection(t, server, firstAccess.Path, firstAccess.Token, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("session capability bypassed deployment authentication: status %d", status)
	}
	status, _, _ = getInspection(t, server, firstAccess.Path, "", deploymentToken)
	if status != http.StatusNotFound {
		t.Fatalf("guessable session ID was sufficient authority: status %d", status)
	}
	guessed := "ins_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32))
	status, _, _ = getInspection(t, server, firstAccess.Path, guessed, deploymentToken)
	if status != http.StatusNotFound {
		t.Fatalf("guessed capability was distinguishable or accepted: status %d", status)
	}
	status, _, _ = getInspection(t, server, secondAccess.Path, firstAccess.Token, deploymentToken)
	if status != http.StatusNotFound {
		t.Fatalf("one session capability inspected another session: status %d", status)
	}

	status, payload, headers := getInspection(
		t, server, firstAccess.Path, firstAccess.Token, deploymentToken,
	)
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

	rotated := negotiateInspection(t, first)
	if rotated.Token == firstAccess.Token {
		t.Fatal("renegotiation retained a plaintext bearer instead of rotating it")
	}
	status, _, _ = getInspection(t, server, firstAccess.Path, firstAccess.Token, deploymentToken)
	if status != http.StatusNotFound {
		t.Fatalf("rotated inspection capability remained active: status %d", status)
	}
	status, _, _ = getInspection(t, server, rotated.Path, rotated.Token, deploymentToken)
	if status != http.StatusOK {
		t.Fatalf("rotated inspection capability is unavailable: status %d", status)
	}

	// An ordinary update reports the durable debug policy, not the bearer that
	// was returned once during its explicit negotiation.
	first.configurePCM16(nil)
	updated := first.await("session.updated", 5*time.Second)
	debug := inspectionDebugResponse(t, updated)
	if access, present := debug["inspection"]; present && access != nil {
		t.Fatalf("ordinary session update replayed inspection authority: %v", access)
	}
	status, _, _ = getInspection(t, server, rotated.Path, rotated.Token, deploymentToken)
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
	status, _, _ := getInspection(t, server, first.Path, first.Token, "")
	if status != http.StatusNotFound {
		t.Fatalf("expired inspection token remained active: status %d", status)
	}

	second := negotiateInspection(t, client)
	if second.Token == first.Token {
		t.Fatal("expired token was reissued")
	}
	status, _, _ = getInspection(t, server, second.Path, second.Token, "")
	if status != http.StatusOK {
		t.Fatalf("renewed inspection token is unavailable: status %d", status)
	}
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version,
		"debug":   map[string]any{"enabled": false},
	})
	client.await("session.updated", 5*time.Second)
	status, _, _ = getInspection(t, server, second.Path, second.Token, "")
	if status != http.StatusNotFound {
		t.Fatalf("disabling debug did not revoke inspection token: status %d", status)
	}
	third := negotiateInspection(t, client)
	if err := client.connection.Close(websocket.StatusNormalClosure, "test complete"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, _, _ = getInspection(t, server, third.Path, third.Token, "")
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
		node := graph.Nodes[0]
		source = inspect.Live{
			FormatVersion: inspect.LiveFormatVersion,
			GraphID:       graph.ID, GraphRevision: graph.Revision, Fingerprint: graph.Fingerprint,
			Configuration: &configuration, Sequence: 9, State: "closed", Error: secret,
			Nodes: map[string]inspect.NodeLive{node.ID: {
				State: "failed", LastTriggerID: secret, LastOutcome: secret, Error: secret,
				Resolution: &inspect.NodeResolution{
					Element: node.Element, Implementation: node.Implementation,
					Runtime: inspect.ArtifactIdentity{
						ID: "runtime://compat", Digest: node.Element.Digest,
					},
					RuntimeEvidence: inspect.EvidenceDeclared,
				},
			}},
			Edges: map[string]inspect.EdgeLive{"boundary:text": {LastItemID: secret}},
			Flows: map[string]inspect.FlowLive{secret: {
				Correlation: secret, Edges: []string{"edge-safe"},
			}},
		}
		return source
	})
	client := dialInspection(t, server, "")
	client.await("session.created", 5*time.Second)
	access := negotiateInspection(t, client)
	status, payload, _ := getInspection(t, server, access.Path, access.Token, "")
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

func startInspectionServer(
	t *testing.T, ttl time.Duration, token string, snapshot func(ir.Graph) inspect.Live,
) *httptest.Server {
	t.Helper()
	legacy, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hello"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{}, Voice: "test-voice", FastMaxTokens: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphbinding.New(legacy)
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
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
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

func getInspection(
	t *testing.T, server *httptest.Server, path, capability, deploymentToken string,
) (int, []byte, http.Header) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capability != "" {
		request.Header.Set(gateway.InspectionTokenHeader, capability)
	}
	if deploymentToken != "" {
		request.Header.Set("Authorization", "Bearer "+deploymentToken)
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
