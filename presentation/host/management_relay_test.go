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
	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
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
	renameInput := management.RenameDocumentRequest{
		Document: management.AuthoringDocument{
			Path: "relay.yaml", Source: managementRelayNormalizedSource(t, "relay.yaml",
				"graph relay {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n"),
		},
		Node: "source", NewName: "camera",
	}
	renameResult := managementRelayRenameResult(t, renameInput)
	removeInput := management.RemoveDocumentEdgeRequest{
		Document: renameInput.Document, Edge: "source.out->source.in",
	}
	removeResult := managementRelayEdgeRemovalResult(t, removeInput)
	createInput := management.CreateDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path: "relay.json",
			Source: managementRelayNormalizedSource(t, "relay.json",
				"graph relay {\n    test.Managed :: source;\n    test.Managed :: sink;\n}\n"),
			Revision: 4,
		},
		ExpectedFingerprint: "sha256:" + strings.Repeat("a", 64), Edge: "restored",
		From: management.AuthoringEdgeEndpoint{Node: "source", Port: "out"},
		To:   management.AuthoringEdgeEndpoint{Node: "sink", Port: "in"}, Delivery: string(syntax.Lossless),
	}
	createResult := managementRelayEdgeCreationResult(t, createInput)

	type observation struct{ method, path, capability, authorization string }
	seen := make(chan observation, 12)
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
		case management.APIPrefix + "/authoring/rename":
			var input management.RenameDocumentRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode rename request: %v", err)
			}
			_ = json.NewEncoder(writer).Encode(renameResult)
		case management.APIPrefix + "/authoring/remove-edge":
			var input management.RemoveDocumentEdgeRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode edge-removal request: %v", err)
			}
			_ = json.NewEncoder(writer).Encode(removeResult)
		case management.APIPrefix + "/authoring/create-edge":
			var input management.CreateDocumentEdgeRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode edge-creation request: %v", err)
			}
			_ = json.NewEncoder(writer).Encode(createResult)
		case management.APIPrefix + "/authoring/read":
			var input management.SourceReadRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode source-read request: %v", err)
			}
			result, resultErr := management.NewSourceReadResult(input, "graph browser_relay {\n}\n")
			if resultErr != nil {
				t.Errorf("construct source-read result: %v", resultErr)
			}
			_ = json.NewEncoder(writer).Encode(result)
		case management.APIPrefix + "/authoring/write":
			var input management.SourceWriteRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode source publication request: %v", err)
			}
			receipt, receiptErr := management.NewSourceWriteReceipt(input, false)
			if receiptErr != nil {
				t.Errorf("construct source publication receipt: %v", receiptErr)
			}
			_ = json.NewEncoder(writer).Encode(receipt)
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

	renameBody, err := json.Marshal(renameInput)
	if err != nil {
		t.Fatal(err)
	}
	renameRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/rename", bytes.NewReader(renameBody))
	renameRequest.Header.Set("Content-Type", "application/json")
	renameRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	renameRequest.Header.Set("Authorization", "Bearer must-not-cross")
	renameResponse, err := http.DefaultClient.Do(renameRequest)
	if err != nil {
		t.Fatal(err)
	}
	renamePayload, _ := io.ReadAll(renameResponse.Body)
	renameResponse.Body.Close()
	if renameResponse.StatusCode != http.StatusOK ||
		renameResponse.Header.Get(ManagementIdentityHeader) !=
			"authoring:rename:"+renameResult.Edits.SourceDigest ||
		strings.Contains(string(renamePayload), operatorCapability) {
		t.Fatalf("rename relay status=%d identity=%q body=%s", renameResponse.StatusCode,
			renameResponse.Header.Get(ManagementIdentityHeader), renamePayload)
	}

	removeBody, err := json.Marshal(removeInput)
	if err != nil {
		t.Fatal(err)
	}
	removeRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/remove-edge", bytes.NewReader(removeBody))
	removeRequest.Header.Set("Content-Type", "application/json")
	removeRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	removeRequest.Header.Set("Authorization", "Bearer must-not-cross")
	removeResponse, err := http.DefaultClient.Do(removeRequest)
	if err != nil {
		t.Fatal(err)
	}
	removePayload, _ := io.ReadAll(removeResponse.Body)
	removeResponse.Body.Close()
	if removeResponse.StatusCode != http.StatusOK ||
		removeResponse.Header.Get(ManagementIdentityHeader) !=
			"authoring:edge.remove:"+removeResult.Edits.SourceDigest ||
		strings.Contains(string(removePayload), operatorCapability) {
		t.Fatalf("edge-removal relay status=%d identity=%q body=%s", removeResponse.StatusCode,
			removeResponse.Header.Get(ManagementIdentityHeader), removePayload)
	}

	createBody, err := json.Marshal(createInput)
	if err != nil {
		t.Fatal(err)
	}
	createRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/create-edge", bytes.NewReader(createBody))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	createRequest.Header.Set("Authorization", "Bearer must-not-cross")
	createResponse, err := http.DefaultClient.Do(createRequest)
	if err != nil {
		t.Fatal(err)
	}
	createPayload, _ := io.ReadAll(createResponse.Body)
	createResponse.Body.Close()
	wantCreateIdentity := "authoring:edge.create:" + createResult.PreviousFingerprint + ":" +
		createResult.CandidateFingerprint + ":" + createResult.Edits.SourceDigest
	if createResponse.StatusCode != http.StatusOK ||
		createResponse.Header.Get(ManagementIdentityHeader) != wantCreateIdentity ||
		strings.Contains(string(createPayload), operatorCapability) {
		t.Fatalf("edge-creation relay status=%d identity=%q body=%s", createResponse.StatusCode,
			createResponse.Header.Get(ManagementIdentityHeader), createPayload)
	}

	readInput := management.SourceReadRequest{
		FormatVersion: management.SourceReadFormatVersion,
		RootIdentity:  "sha256:" + strings.Repeat("e", 64),
		Path:          "browser/relay.ortg",
	}
	readBody, err := json.Marshal(readInput)
	if err != nil {
		t.Fatal(err)
	}
	wantReadResult, err := management.NewSourceReadResult(readInput, "graph browser_relay {\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	readRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/read", bytes.NewReader(readBody))
	readRequest.Header.Set("Content-Type", "application/json")
	readRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	readRequest.Header.Set("Authorization", "Bearer must-not-cross")
	readResponse, err := http.DefaultClient.Do(readRequest)
	if err != nil {
		t.Fatal(err)
	}
	readPayload, _ := io.ReadAll(readResponse.Body)
	readResponse.Body.Close()
	if readResponse.StatusCode != http.StatusOK ||
		readResponse.Header.Get(ManagementIdentityHeader) != "authoring:read:"+wantReadResult.ResultDigest ||
		strings.Contains(string(readPayload), operatorCapability) {
		t.Fatalf("source-read relay status=%d identity=%q body=%s", readResponse.StatusCode,
			readResponse.Header.Get(ManagementIdentityHeader), readPayload)
	}

	writeInput := management.SourceWriteRequest{
		FormatVersion: management.SourceWriteFormatVersion,
		RootIdentity:  "sha256:" + strings.Repeat("e", 64),
		Mode:          management.SourceCreate,
		Path:          "browser/relay.ortg",
		Source:        "graph browser_relay {\n}\n",
	}
	writeBody, err := json.Marshal(writeInput)
	if err != nil {
		t.Fatal(err)
	}
	wantWriteReceipt, err := management.NewSourceWriteReceipt(writeInput, false)
	if err != nil {
		t.Fatal(err)
	}
	writeRequest, _ := http.NewRequest(http.MethodPost,
		hostServer.URL+"/client/v1/management/authoring/write", bytes.NewReader(writeBody))
	writeRequest.Header.Set("Content-Type", "application/json")
	writeRequest.Header.Set(management.CapabilityHeader, operatorCapability)
	writeRequest.Header.Set("Authorization", "Bearer must-not-cross")
	writeResponse, err := http.DefaultClient.Do(writeRequest)
	if err != nil {
		t.Fatal(err)
	}
	writePayload, _ := io.ReadAll(writeResponse.Body)
	writeResponse.Body.Close()
	if writeResponse.StatusCode != http.StatusOK ||
		writeResponse.Header.Get(ManagementIdentityHeader) != "authoring:write:"+wantWriteReceipt.ReceiptDigest ||
		strings.Contains(string(writePayload), operatorCapability) {
		t.Fatalf("source publication relay status=%d identity=%q body=%s", writeResponse.StatusCode,
			writeResponse.Header.Get(ManagementIdentityHeader), writePayload)
	}

	for range len(checks) + 6 {
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

func TestManagementRelayRejectsRenameThatOmitsOneGraphReference(t *testing.T) {
	input := management.RenameDocumentRequest{
		Document: management.AuthoringDocument{
			Path: "relay.ortg", Source: "graph relay {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Node: "source", NewName: "camera",
	}
	forged := managementRelayRenameResult(t, input)
	forged.Edits.Edits = forged.Edits.Edits[:len(forged.Edits.Edits)-1]
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(forged)
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/client/v1/management/authoring/rename", bytes.NewReader(payload))
	request.SetPathValue("action", "rename")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "operator_rename_secret")
	response := httptest.NewRecorder()
	NewManagementRelayFactory(nil, nil).relayAuthoring(
		base, relayTarget{DialTimeout: 15 * time.Second}, response, request,
	)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("forged rename status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestManagementRelayRejectsEdgeRemovalThatMutatesAnotherSourceByte(t *testing.T) {
	input := management.RemoveDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path: "relay.ortg", Source: "graph relay {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Edge: "source.out->source.in",
	}
	forged := managementRelayEdgeRemovalResult(t, input)
	forged.Edits.Edits[0].NewText += "\n"
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(forged)
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/client/v1/management/authoring/remove-edge", bytes.NewReader(payload))
	request.SetPathValue("action", "remove-edge")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "operator_edge_secret")
	response := httptest.NewRecorder()
	NewManagementRelayFactory(nil, nil).relayAuthoring(
		base, relayTarget{DialTimeout: 15 * time.Second}, response, request,
	)
	if response.Code != http.StatusBadGateway || response.Header().Get(ManagementIdentityHeader) != "" {
		t.Fatalf("forged edge removal status=%d identity=%q body=%s", response.Code,
			response.Header().Get(ManagementIdentityHeader), response.Body.String())
	}
}

func TestManagementRelayRejectsEdgeCreationThatMutatesAnotherSourceByte(t *testing.T) {
	input := management.CreateDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path:   "relay.ortg",
			Source: "graph relay {\n    test.Managed :: source;\n    test.Managed :: sink;\n}\n",
		},
		ExpectedFingerprint: "sha256:" + strings.Repeat("a", 64), Edge: "restored",
		From: management.AuthoringEdgeEndpoint{Node: "source", Port: "out"},
		To:   management.AuthoringEdgeEndpoint{Node: "sink", Port: "in"}, Delivery: string(syntax.Lossless),
	}
	forged := managementRelayEdgeCreationResult(t, input)
	forged.Edits.Edits[0].NewText += "\n"
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(forged)
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/client/v1/management/authoring/create-edge", bytes.NewReader(payload))
	request.SetPathValue("action", "create-edge")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "operator_edge_secret")
	response := httptest.NewRecorder()
	NewManagementRelayFactory(nil, nil).relayAuthoring(
		base, relayTarget{DialTimeout: 15 * time.Second}, response, request,
	)
	if response.Code != http.StatusBadGateway || response.Header().Get(ManagementIdentityHeader) != "" {
		t.Fatalf("forged edge creation status=%d identity=%q body=%s", response.Code,
			response.Header().Get(ManagementIdentityHeader), response.Body.String())
	}
}

func managementRelayRenameResult(
	t testing.TB, input management.RenameDocumentRequest,
) management.RenameDocumentResult {
	t.Helper()
	var edits editor.EditSet
	var err error
	if strings.HasSuffix(input.Document.Path, ".ortg") {
		document, analyzeErr := editor.Analyze(input.Document.Path, []byte(input.Document.Source),
			resolve.NewCatalog(), editor.DefaultLimits())
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		edits, err = document.RenameNodeID(input.Node, input.NewName)
	} else {
		document, analyzeErr := editor.AnalyzeNormalized(input.Document.Path,
			[]byte(input.Document.Source), editor.DefaultLimits())
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		edits, err = document.RenameNodeID(input.Node, input.NewName)
	}
	if err != nil {
		t.Fatal(err)
	}
	result := management.RenameDocumentResult{
		Node: input.Node, NewName: input.NewName, Edits: edits,
	}
	if err := management.ValidateRenameDocumentResult(input, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func managementRelayEdgeRemovalResult(
	t testing.TB, input management.RemoveDocumentEdgeRequest,
) management.RemoveDocumentEdgeResult {
	t.Helper()
	var edits editor.EditSet
	var err error
	if strings.HasSuffix(input.Document.Path, ".ortg") {
		document, analyzeErr := editor.Analyze(input.Document.Path, []byte(input.Document.Source),
			resolve.NewCatalog(), editor.DefaultLimits())
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		edits, err = document.RemoveEdgeID(input.Edge)
	} else {
		document, analyzeErr := editor.AnalyzeNormalized(input.Document.Path,
			[]byte(input.Document.Source), editor.DefaultLimits())
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		edits, err = document.RemoveEdgeID(input.Edge)
	}
	if err != nil {
		t.Fatal(err)
	}
	result := management.RemoveDocumentEdgeResult{Edge: input.Edge, Edits: edits}
	if err := management.ValidateRemoveDocumentEdgeResult(input, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func managementRelayEdgeCreationResult(
	t testing.TB, input management.CreateDocumentEdgeRequest,
) management.CreateDocumentEdgeResult {
	t.Helper()
	var edits editor.EditSet
	var err error
	if strings.HasSuffix(input.Document.Path, ".ortg") {
		document, analyzeErr := editor.Analyze(input.Document.Path, []byte(input.Document.Source),
			resolve.NewCatalog(), editor.DefaultLimits())
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		edits, err = document.CreateEdgeID(input.Edge,
			syntax.Endpoint{Node: input.From.Node, Port: input.From.Port},
			syntax.Endpoint{Node: input.To.Node, Port: input.To.Port}, syntax.Delivery(input.Delivery))
	} else {
		document, analyzeErr := editor.AnalyzeNormalized(input.Document.Path,
			[]byte(input.Document.Source), editor.DefaultLimits())
		if analyzeErr != nil {
			t.Fatal(analyzeErr)
		}
		edits, err = document.CreateEdgeID(input.Edge,
			syntax.Endpoint{Node: input.From.Node, Port: input.From.Port},
			syntax.Endpoint{Node: input.To.Node, Port: input.To.Port}, syntax.Delivery(input.Delivery))
	}
	if err != nil {
		t.Fatal(err)
	}
	candidate := sha256.Sum256([]byte(input.ExpectedFingerprint + "\x00" + input.Edge))
	result := management.CreateDocumentEdgeResult{
		Edge: input.Edge, PreviousFingerprint: input.ExpectedFingerprint,
		CandidateFingerprint: fmt.Sprintf("sha256:%x", candidate), Edits: edits,
	}
	if err := management.ValidateCreateDocumentEdgeResult(input, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func managementRelayNormalizedSource(t testing.TB, path, source string) string {
	t.Helper()
	file, err := syntax.Parse("relay.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	document := manifest.FromSyntax(file)
	var encoded []byte
	if strings.HasSuffix(path, ".json") {
		encoded, err = manifest.MarshalJSON(document)
	} else {
		encoded, err = manifest.MarshalYAML(document)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestManagementRelayRejectsForgedSourcePublicationReceipt(t *testing.T) {
	input := management.SourceWriteRequest{
		FormatVersion: management.SourceWriteFormatVersion,
		RootIdentity:  "sha256:" + strings.Repeat("7", 64),
		Mode:          management.SourceCreate,
		Path:          "relay/forged.ortg",
		Source:        "graph relay_forged {\n}\n",
	}
	receipt, err := management.NewSourceWriteReceipt(input, false)
	if err != nil {
		t.Fatal(err)
	}
	receipt.SourceDigest = "sha256:" + strings.Repeat("8", 64)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(receipt)
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/client/v1/management/authoring/write", bytes.NewReader(payload))
	request.SetPathValue("action", "write")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "operator_source_secret")
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	NewManagementRelayFactory(nil, slog.New(slog.NewJSONHandler(&logs, nil))).relayAuthoring(
		base, relayTarget{DialTimeout: 15 * time.Second}, response, request,
	)
	if response.Code != http.StatusBadGateway || response.Header().Get(ManagementIdentityHeader) != "" {
		t.Fatalf("forged source receipt status=%d identity=%q body=%s", response.Code,
			response.Header().Get(ManagementIdentityHeader), response.Body.String())
	}
	if strings.Contains(logs.String(), "operator_source_secret") {
		t.Fatalf("source receipt rejection logged operator capability: %s", logs.String())
	}
}

func TestManagementRelayRejectsForgedSourceReadResult(t *testing.T) {
	input := management.SourceReadRequest{
		FormatVersion: management.SourceReadFormatVersion,
		RootIdentity:  "sha256:" + strings.Repeat("7", 64),
		Path:          "relay/forged.ortg",
	}
	result, err := management.NewSourceReadResult(input, "graph relay_forged {\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	result.SourceDigest = "sha256:" + strings.Repeat("8", 64)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(result)
	}))
	defer backend.Close()
	base, err := url.Parse(backend.URL + management.APIPrefix)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/client/v1/management/authoring/read", bytes.NewReader(payload))
	request.SetPathValue("action", "read")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(management.CapabilityHeader, "operator_source_secret")
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	NewManagementRelayFactory(nil, slog.New(slog.NewJSONHandler(&logs, nil))).relayAuthoring(
		base, relayTarget{DialTimeout: 15 * time.Second}, response, request,
	)
	if response.Code != http.StatusBadGateway || response.Header().Get(ManagementIdentityHeader) != "" {
		t.Fatalf("forged source-read result status=%d identity=%q body=%s", response.Code,
			response.Header().Get(ManagementIdentityHeader), response.Body.String())
	}
	if strings.Contains(logs.String(), "operator_source_secret") {
		t.Fatalf("source-read rejection logged operator capability: %s", logs.String())
	}
}
