package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestManagementRelayForwardsOnlyNarrowSessionCapability(t *testing.T) {
	type observed struct {
		path, query, capability, authorization string
	}
	seen := make(chan observed, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- observed{
			path: request.URL.Path, query: request.URL.RawQuery,
			capability:    request.Header.Get(management.CapabilityHeader),
			authorization: request.Header.Get("Authorization"),
		}
		if request.URL.Query().Get("after") == "redirect" {
			http.Redirect(writer, request, "/captured", http.StatusFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"format_version": 1})
	}))
	defer backend.Close()

	router := NewRouterFactory()
	target := NewEndpointDirectoryFactory()
	relay := NewManagementRelayFactory(nil, nil)
	factories := []pluginruntime.Factory{relay, target, router}
	plan := makeHostPlan(t, factories)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values := testEndpointDirectoryValues(t, "", presentation.Endpoint{
		Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
		URL: backend.URL + management.APIPrefix,
	})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": values},
		Permissions: map[string][]plugin.Permission{"management": {{
			Kind: connectPermissionKind, Resource: connectPermissionResource,
			Operations: []string{managementOperation},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	hostServer := httptest.NewServer(handler)
	defer hostServer.Close()

	request, _ := http.NewRequest(http.MethodGet,
		hostServer.URL+"/client/v1/management/sessions/sess-1/deltas?after=7&limit=32", nil)
	request.Header.Set(management.CapabilityHeader, "mgmt_scoped")
	request.Header.Set("Authorization", "Bearer must-not-cross")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(payload), `"format_version":1`) {
		t.Fatalf("relay response status=%d body=%s", response.StatusCode, payload)
	}
	got := <-seen
	if got.path != management.APIPrefix+"/sessions/sess-1/deltas" ||
		got.query != "after=7&limit=32" || got.capability != "mgmt_scoped" || got.authorization != "" {
		t.Fatalf("forwarded request = %+v", got)
	}
	if response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("relay response headers = %v", response.Header)
	}
	modelRequest, _ := http.NewRequest(http.MethodGet,
		hostServer.URL+"/client/v1/management/sessions/sess-1/model", nil)
	modelRequest.Header.Set(management.CapabilityHeader, "mgmt_scoped")
	modelResponse, err := http.DefaultClient.Do(modelRequest)
	if err != nil {
		t.Fatal(err)
	}
	modelResponse.Body.Close()
	modelForward := <-seen
	if modelResponse.StatusCode != http.StatusOK ||
		modelForward.path != management.APIPrefix+"/sessions/sess-1/model" ||
		modelForward.capability != "mgmt_scoped" || modelForward.authorization != "" {
		t.Fatalf("forwarded session model = status %d request %+v", modelResponse.StatusCode, modelForward)
	}

	badQuery, _ := http.NewRequest(http.MethodGet,
		hostServer.URL+"/client/v1/management/sessions/sess-1/live?token=leak", nil)
	badQuery.Header.Set(management.CapabilityHeader, "mgmt_scoped")
	badResponse, err := http.DefaultClient.Do(badQuery)
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown management query status = %d", badResponse.StatusCode)
	}

	repeated := httptest.NewRequest(http.MethodGet,
		"/client/v1/management/sessions/sess-1/live", nil)
	repeated.Header.Add(management.CapabilityHeader, "mgmt_one")
	repeated.Header.Add(management.CapabilityHeader, "mgmt_two")
	repeatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(repeatedResponse, repeated)
	if repeatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("repeated management capability status = %d", repeatedResponse.Code)
	}

	if err := mounted.Unmount(context.Background(), "management"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, hostServer.URL+"/client/v1/management/sessions/sess-1/live", http.StatusNotFound)
}

func TestManagementRelayRefusesRedirectFollowing(t *testing.T) {
	var captured atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/captured" {
			captured.Store(true)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(writer, request, "/captured", http.StatusFound)
	}))
	defer backend.Close()

	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	factory := NewManagementRelayFactory(nil, nil)
	request := httptest.NewRequest(http.MethodGet,
		"/client/v1/management/sessions/sess-1/live", nil)
	request.SetPathValue("session", "sess-1")
	request.SetPathValue("resource", "live")
	request.Header.Set(management.CapabilityHeader, "mgmt_scoped")
	response := httptest.NewRecorder()
	factory.relay(base, relayTarget{DialTimeout: 15 * time.Second}, response, request)
	if response.Code != http.StatusFound || captured.Load() {
		t.Fatalf("redirect response=%d followed=%t", response.Code, captured.Load())
	}
}

func TestBoundedManagementQueryUsesTraceCursorAndPageLimits(t *testing.T) {
	accepted, err := boundedManagementQuery(map[string][]string{
		"after": {"18446744073709551615"}, "limit": {"4096"},
	})
	if err != nil || accepted.Get("after") != "18446744073709551615" || accepted.Get("limit") != "4096" {
		t.Fatalf("accepted management query = %v, %v", accepted, err)
	}
	for _, query := range []map[string][]string{
		{"limit": {"0"}}, {"limit": {"4097"}}, {"after": {"-1"}},
		{"after": {"1", "2"}}, {"unknown": {"1"}},
	} {
		if _, err := boundedManagementQuery(query); err == nil {
			t.Fatalf("management query unexpectedly accepted %v", query)
		}
	}
}

func managementRelayGraph(t testing.TB) (ir.Graph, element.Descriptor) {
	t.Helper()
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Managed", Revision: 1,
		Ports: []element.Port{{
			Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Value")),
			Cardinality: element.One,
		}},
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "relay-managed", Revision: 1,
		Nodes: []ir.Node{{
			ID: "node", Element: identity, Ports: []ir.Port{{
				Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Value")),
				Cardinality: element.One, DefaultDepth: 1,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph, descriptor
}

func TestManagementRelayWhitelistsAndRebindsStaticAndAuthoringResources(t *testing.T) {
	graph, elementDescriptor := managementRelayGraph(t)
	elementIdentity, err := elementDescriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	pluginDescriptor := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion, Name: "test.ManagedPlugin", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"browser"},
		Permissions: []plugin.Permission{{
			Kind: "network.connect", Resource: "fixture", Operations: []string{"read"},
		}},
	}
	pluginIdentity, err := pluginDescriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	schemaBytes := []byte("{\"type\":\"object\"}\n")
	schemaSum := sha256.Sum256(schemaBytes)
	schemaDigest := fmt.Sprintf("sha256:%x", schemaSum)
	schemaResource, err := management.NewValuesSchemaResource(schema.Bundle{
		Schema: schemaBytes, Digest: schemaDigest, Complete: true,
		Nodes: []schema.NodeContract{}, Contracts: []schema.ContractResolution{}, Unresolved: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	type observation struct{ method, path, capability, authorization string }
	seen := make(chan observation, 8)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- observation{
			method: request.Method, path: request.URL.Path,
			capability:    request.Header.Get(management.CapabilityHeader),
			authorization: request.Header.Get("Authorization"),
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case management.APIPrefix + "/graphs/" + graph.Fingerprint:
			_ = json.NewEncoder(writer).Encode(graph)
		case management.APIPrefix + "/descriptors/elements/" + elementIdentity.Name + "/1/" + elementIdentity.Digest:
			_ = json.NewEncoder(writer).Encode(elementDescriptor)
		case management.APIPrefix + "/descriptors/plugins/" + pluginIdentity.Name + "/1/" + pluginIdentity.Digest:
			_ = json.NewEncoder(writer).Encode(pluginDescriptor)
		case management.APIPrefix + "/schemas/values/" + graph.Fingerprint:
			_ = json.NewEncoder(writer).Encode(schemaResource)
		case management.APIPrefix + "/authoring/render":
			var input management.RenderRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode render request: %v", err)
			}
			text, renderErr := inspect.DOT(input.Graph)
			if renderErr != nil {
				t.Errorf("render DOT fixture: %v", renderErr)
			}
			_ = json.NewEncoder(writer).Encode(management.RenderResult{
				Fingerprint: input.Graph.Fingerprint, Format: input.Format, Text: text,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer backend.Close()

	router := NewRouterFactory()
	target := NewEndpointDirectoryFactory()
	relay := NewManagementRelayFactory(nil, nil)
	factories := []pluginruntime.Factory{relay, target, router}
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values := testEndpointDirectoryValues(t, "", presentation.Endpoint{
		Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
		URL: backend.URL + management.APIPrefix,
	})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: makeHostPlan(t, factories), Registry: registry,
		Values: map[string]json.RawMessage{"target": values},
		Permissions: map[string][]plugin.Permission{"management": {{
			Kind: connectPermissionKind, Resource: connectPermissionResource,
			Operations: []string{managementOperation},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	hostServer := httptest.NewServer(handler)
	defer hostServer.Close()

	const operatorCapability = "operator_must_stay_in_header"
	checks := []struct {
		path, identity string
	}{
		{"/client/v1/management/graphs/" + graph.Fingerprint, "graph:" + graph.Fingerprint},
		{"/client/v1/management/descriptors/elements/" + elementIdentity.Name + "/1/" + elementIdentity.Digest,
			"element:" + elementIdentity.Name + "@1:" + elementIdentity.Digest},
		{"/client/v1/management/descriptors/plugins/" + pluginIdentity.Name + "/1/" + pluginIdentity.Digest,
			"plugin:" + pluginIdentity.Name + "@1:" + pluginIdentity.Digest},
		{"/client/v1/management/schemas/values/" + graph.Fingerprint,
			"schema:" + graph.Fingerprint + ":" + schemaDigest},
	}
	for _, check := range checks {
		request, _ := http.NewRequest(http.MethodGet, hostServer.URL+check.path, nil)
		request.Header.Set(management.CapabilityHeader, operatorCapability)
		request.Header.Set("Authorization", "Bearer must-not-cross")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get(ManagementIdentityHeader) != check.identity ||
			strings.Contains(string(payload), operatorCapability) {
			t.Fatalf("static relay %s status=%d identity=%q body=%s", check.path,
				response.StatusCode, response.Header.Get(ManagementIdentityHeader), payload)
		}
	}

	renderBody, _ := json.Marshal(management.RenderRequest{Graph: graph, Format: management.RenderDOT})
	renderRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/render", bytes.NewReader(renderBody))
	renderRequest.Header.Set("Content-Type", "application/json")
	renderRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	renderRequest.Header.Set("Authorization", "Bearer must-not-cross")
	renderResponse, err := http.DefaultClient.Do(renderRequest)
	if err != nil {
		t.Fatal(err)
	}
	renderPayload, _ := io.ReadAll(renderResponse.Body)
	renderResponse.Body.Close()
	wantRenderIdentity := "authoring:render:dot:" + graph.Fingerprint
	if renderResponse.StatusCode != http.StatusOK ||
		renderResponse.Header.Get(ManagementIdentityHeader) != wantRenderIdentity ||
		strings.Contains(string(renderPayload), operatorCapability) {
		t.Fatalf("authoring relay status=%d identity=%q body=%s", renderResponse.StatusCode,
			renderResponse.Header.Get(ManagementIdentityHeader), renderPayload)
	}

	for range len(checks) + 1 {
		observation := <-seen
		if observation.capability != operatorCapability || observation.authorization != "" {
			t.Fatalf("management relay crossed credential planes: %+v", observation)
		}
	}
	badQuery, _ := http.NewRequest(http.MethodGet,
		hostServer.URL+"/client/v1/management/graphs/"+graph.Fingerprint+"?token="+operatorCapability, nil)
	badQuery.Header.Set(management.CapabilityHeader, operatorCapability)
	badResponse, err := http.DefaultClient.Do(badQuery)
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("static capability query status = %d", badResponse.StatusCode)
	}
	assertStatus(t, hostServer.URL+"/client/v1/management/authoring/unknown", http.StatusMethodNotAllowed)
	unknownRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/unknown", strings.NewReader(`{}`))
	unknownRequest.Header.Set("Content-Type", "application/json")
	unknownRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	unknownResponse, err := http.DefaultClient.Do(unknownRequest)
	if err != nil {
		t.Fatal(err)
	}
	unknownResponse.Body.Close()
	if unknownResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown authoring action status = %d", unknownResponse.StatusCode)
	}
	if err := mounted.Unmount(context.Background(), "management"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, hostServer.URL+checks[0].path, http.StatusNotFound)
}

func TestManagementRelayRejectsMalformedMismatchedAndOversizedStaticResponsesWithoutCredentialLogs(t *testing.T) {
	graph, _ := managementRelayGraph(t)
	canonical, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 0:
			_, _ = writer.Write(bytes.Replace(canonical, []byte(`{"format_version":1`),
				[]byte(`{"format_version":1,"format_version":1`), 1))
		case 1:
			forged := graph
			forged.ID = "another-graph"
			_ = json.NewEncoder(writer).Encode(forged)
		case 2:
			writer.Header().Set("Content-Length", strconv.FormatInt(maximumStaticManagementBodyBytes+1, 10))
			_, _ = writer.Write([]byte(`{}`))
		case 3:
			_, _ = writer.Write([]byte(`{"format_version":1,"operator_log_secret":true}`))
		}
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	factory := NewManagementRelayFactory(nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	for testMode := int32(0); testMode < 4; testMode++ {
		mode.Store(testMode)
		request := httptest.NewRequest(http.MethodGet,
			"/client/v1/management/graphs/"+graph.Fingerprint, nil)
		request.SetPathValue("fingerprint", graph.Fingerprint)
		request.Header.Set(management.CapabilityHeader, "operator_log_secret")
		response := httptest.NewRecorder()
		factory.relayGraph(base, relayTarget{DialTimeout: 15 * time.Second}, response, request)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("malformed static mode %d status=%d body=%s", testMode, response.Code, response.Body.String())
		}
	}
	if strings.Contains(logs.String(), "operator_log_secret") {
		t.Fatalf("management relay logs retained operator credential: %s", logs.String())
	}
}

func TestManagementRelayRejectsRenderContentThatOnlyClaimsTheRequestedIdentity(t *testing.T) {
	graph, _ := managementRelayGraph(t)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(management.RenderResult{
			Fingerprint: graph.Fingerprint,
			Format:      management.RenderDOT,
			Text:        "digraph forged {}",
		})
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(management.RenderRequest{Graph: graph, Format: management.RenderDOT})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/client/v1/management/authoring/render", bytes.NewReader(payload))
	request.SetPathValue("action", "render")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "operator_render_secret")
	response := httptest.NewRecorder()
	NewManagementRelayFactory(nil, nil).relayAuthoring(
		base, relayTarget{DialTimeout: 15 * time.Second}, response, request,
	)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("forged render status=%d body=%s", response.Code, response.Body.String())
	}
}
