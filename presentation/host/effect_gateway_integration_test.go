package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/internal/testserver"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/coder/websocket"
)

type receiptIntegrationHost struct {
	mounted *pluginruntime.Mounted
	server  *httptest.Server
	effects *EffectsFactory
}

func mountReceiptIntegrationHost(
	t *testing.T, effects *EffectsFactory, authorityFactory *EffectReceiptAuthorityFactory,
) *receiptIntegrationHost {
	t.Helper()
	router := NewRouterFactory()
	artifacts := NewArtifactStoreFactory()
	downloads := NewDownloadStoreFactory()
	factories := []pluginruntime.Factory{router, artifacts, downloads, authorityFactory, effects}
	ids := []string{"router", "artifacts", "downloads", "authority", "effects"}
	catalog := plugin.NewCatalog()
	registry := pluginruntime.NewRegistry()
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	for index, factory := range factories {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: ids[index], Plugin: factory.Descriptor().Name, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "presentation.effect-receipt.integration", Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries,
		Exports: []plugin.ProfileExport{
			{Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name},
			{Name: "effects", Provider: "effects", Service: presentation.EffectsContract.Name},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	permissions := map[string][]plugin.Permission{
		"artifacts": artifacts.Descriptor().Permissions,
		"downloads": downloads.Descriptor().Permissions,
		"effects":   effects.Descriptor().Permissions,
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"artifacts": json.RawMessage(`{}`), "downloads": json.RawMessage(`{}`),
			"effects": json.RawMessage(`{}`),
		},
		Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	handlerValue, contract, _, _, err := mounted.Export("http")
	if err != nil || contract != presentation.HTTPHandlerContract {
		_ = mounted.Close(context.Background())
		t.Fatalf("export integration host: %v, %#v", err, contract)
	}
	handler, ok := handlerValue.(http.Handler)
	if !ok {
		_ = mounted.Close(context.Background())
		t.Fatalf("integration HTTP export type = %T", handlerValue)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		if err := mounted.Close(context.Background()); err != nil {
			t.Errorf("close integration effect host: %v", err)
		}
	})
	return &receiptIntegrationHost{mounted: mounted, server: server, effects: effects}
}

type gatewayEffectEmission struct {
	sessionID         string
	callID            string
	name              string
	arguments         map[string]any
	declarationDigest string
	receipt           string
}

func gatewayEffectFixtureCall(
	t *testing.T, protocolURL string, declaration EffectDeclaration, digest, target string,
) gatewayEffectEmission {
	t.Helper()
	connection, _, err := websocket.Dial(context.Background(), protocolURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	created := awaitGatewayFixtureEvent(t, connection, "session.created")
	sessionObject, _ := created["session"].(map[string]any)
	sessionID, _ := sessionObject["id"].(string)
	toolExtension := map[string]any{"client_effect": map[string]any{
		"version": 1, "declaration_digest": digest,
	}}
	if target != "" {
		toolExtension["target"] = target
	}
	sendGatewayFixtureEvent(t, connection, map[string]any{
		"type": "session.update", "session": map[string]any{
			"type": "realtime", "output_modalities": []string{"text"},
			"openrealtime": map[string]any{
				"version": 1, "supports": []string{"client.effects"},
			},
			"tools": []map[string]any{{
				"type": "function", "name": declaration.Name,
				"description": declaration.Description, "parameters": declaration.Parameters,
				"openrealtime": toolExtension,
			}},
		},
	})
	updated := awaitGatewayFixtureEvent(t, connection, "session.updated")
	updatedSession, _ := updated["session"].(map[string]any)
	extension, _ := updatedSession["openrealtime"].(map[string]any)
	enabled, _ := extension["enabled"].([]any)
	if len(enabled) != 1 || enabled[0] != "client.effects" {
		t.Fatalf("integration client-effects negotiation = %#v", extension)
	}
	sendGatewayFixtureEvent(t, connection, map[string]any{
		"type": "conversation.item.create", "item": map[string]any{
			"type": "message", "role": "user", "content": []map[string]any{{
				"type": "input_text", "text": "show the artifact",
			}},
		},
	})
	done := awaitGatewayFixtureEvent(t, connection, "response.function_call_arguments.done")
	argumentsText, _ := done["arguments"].(string)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(argumentsText), &arguments); err != nil {
		t.Fatalf("decode gateway effect arguments: %v", err)
	}
	callExtension, _ := done["openrealtime"].(map[string]any)
	clientEffect, _ := callExtension["client_effect"].(map[string]any)
	emission := gatewayEffectEmission{
		sessionID: sessionID, callID: stringValue(done["call_id"]), name: stringValue(done["name"]),
		arguments: arguments, declarationDigest: stringValue(clientEffect["declaration_digest"]),
		receipt: stringValue(clientEffect["authority"]),
	}
	if emission.sessionID == "" || emission.callID == "" || emission.name != declaration.Name ||
		emission.declarationDigest != digest || emission.receipt == "" {
		t.Fatalf("gateway effect emission = %#v event=%#v", emission, done)
	}
	return emission
}

func sendGatewayFixtureEvent(t *testing.T, connection *websocket.Conn, event map[string]any) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func awaitGatewayFixtureEvent(
	t *testing.T, connection *websocket.Conn, want string,
) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		kind, payload, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("await gateway %s: %v", want, err)
		}
		if kind != websocket.MessageText {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		kindName, _ := event["type"].(string)
		if kindName == "error" && want != "error" {
			t.Fatalf("gateway error while awaiting %s: %#v", want, event)
		}
		if kindName == want {
			return event
		}
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func TestGatewayReceiptCrossesTheComposableHostAndRejectsEveryRebinding(t *testing.T) {
	provider, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	effects, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	authorityFactory, err := NewEffectReceiptAuthorityFactory(provider)
	if err != nil {
		t.Fatal(err)
	}
	host := mountReceiptIntegrationHost(t, effects, authorityFactory)
	declaration := effects.tools[0].declaration.clone()
	stack := testserver.Start(t, testserver.Config{
		Transcript: "show the artifact", ToolName: declaration.Name,
		ToolArguments:      `{"artifact_id":"gateway","title":"Gateway","html":"<main>sealed end to end</main>"}`,
		ClientEffectIssuer: provider,
	})

	emission := gatewayEffectFixtureCall(t, stack.ProtocolURL, declaration, declaration.Digest, "")
	socket := dialEffectTestSocket(t, host.server)
	socket.send(effectFixtureCall(
		emission.sessionID, emission.callID, emission.name, emission.arguments, emission.receipt,
	))
	success := socket.receive()
	if success.Error != nil || success.Artifact == nil || success.Artifact.ID != "gateway" {
		t.Fatalf("gateway-to-host effect = %#v", success)
	}

	changedArguments := map[string]any{
		"artifact_id": "gateway", "title": "Gateway", "html": "<main>changed</main>",
	}
	socket.send(effectFixtureCall(
		emission.sessionID, emission.callID, emission.name, changedArguments, emission.receipt,
	))
	if result := socket.receive(); result.Error == nil || result.Error.Code != "authority_denied" {
		t.Fatalf("cross-arguments replay = %#v", result)
	}
	socket.send(effectFixtureCall(
		emission.sessionID, emission.callID+"_other", emission.name, emission.arguments, emission.receipt,
	))
	if result := socket.receive(); result.Error == nil || result.Error.Code != "authority_denied" {
		t.Fatalf("cross-call replay = %#v", result)
	}
	otherSession := dialEffectTestSocket(t, host.server)
	otherSession.send(effectFixtureCall(
		emission.sessionID+"_other", emission.callID, emission.name, emission.arguments, emission.receipt,
	))
	if result := otherSession.receive(); result.Error == nil || result.Error.Code != "authority_denied" {
		t.Fatalf("cross-session replay = %#v", result)
	}

	wrongTarget := gatewayEffectFixtureCall(t, stack.ProtocolURL, declaration, declaration.Digest, "browser-wrong")
	targetSocket := dialEffectTestSocket(t, host.server)
	targetSocket.send(effectFixtureCall(
		wrongTarget.sessionID, wrongTarget.callID, wrongTarget.name,
		wrongTarget.arguments, wrongTarget.receipt,
	))
	if result := targetSocket.receive(); result.Error == nil || result.Error.Code != "authority_denied" {
		t.Fatalf("cross-target receipt = %#v", result)
	}

	wrongDigest := "sha256:" + strings.Repeat("e", 64)
	drifted := gatewayEffectFixtureCall(t, stack.ProtocolURL, declaration, wrongDigest, "")
	declarationSocket := dialEffectTestSocket(t, host.server)
	declarationSocket.send(effectFixtureCall(
		drifted.sessionID, drifted.callID, drifted.name, drifted.arguments, drifted.receipt,
	))
	if result := declarationSocket.receive(); result.Error == nil || result.Error.Code != "authority_denied" {
		t.Fatalf("cross-declaration receipt = %#v", result)
	}
}

func TestGatewayReceiptExpiresBeforeTheHostBoundary(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	provider, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{
		TTL: time.Second, Clock: func() time.Time { return time.Unix(0, now.Load()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	effects, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	authorityFactory, err := NewEffectReceiptAuthorityFactory(provider)
	if err != nil {
		t.Fatal(err)
	}
	host := mountReceiptIntegrationHost(t, effects, authorityFactory)
	declaration := effects.tools[0].declaration.clone()
	stack := testserver.Start(t, testserver.Config{
		ToolName:           declaration.Name,
		ToolArguments:      `{"artifact_id":"expired","title":"Expired","html":"<p>expired</p>"}`,
		ClientEffectIssuer: provider,
	})
	emission := gatewayEffectFixtureCall(t, stack.ProtocolURL, declaration, declaration.Digest, "")
	now.Add(int64(time.Second))
	socket := dialEffectTestSocket(t, host.server)
	socket.send(effectFixtureCall(
		emission.sessionID, emission.callID, emission.name, emission.arguments, emission.receipt,
	))
	result := socket.receive()
	if result.Error == nil || result.Error.Code != "authority_denied" {
		t.Fatalf("expired gateway receipt = %#v", result)
	}
}

func TestEffectReceiptProviderLossCascadesTheHostAndRevokesGatewayIssuance(t *testing.T) {
	provider, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	effects, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	authorityFactory, err := NewEffectReceiptAuthorityFactory(provider)
	if err != nil {
		t.Fatal(err)
	}
	host := mountReceiptIntegrationHost(t, effects, authorityFactory)
	stack := testserver.Start(t, testserver.Config{ClientEffectIssuer: provider})
	if err := host.mounted.Unmount(context.Background(), "authority"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, host.server.URL+"/client/v1/effects", 404)
	_, err = provider.IssueEffectReceipt(context.Background(), effectauthority.EffectReceiptClaims{
		SessionID: "sess_lost", CallID: "call_lost", Name: "display_artifact",
		ArgumentsDigest:   "sha256:" + strings.Repeat("a", 64),
		DeclarationDigest: "sha256:" + strings.Repeat("b", 64),
	})
	if !errors.Is(err, effectauthority.ErrEffectReceiptClosed) {
		t.Fatalf("provider loss retained gateway issuance authority: %v", err)
	}
	connection, _, err := websocket.Dial(context.Background(), stack.ProtocolURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	awaitGatewayFixtureEvent(t, connection, "session.created")
	sendGatewayFixtureEvent(t, connection, map[string]any{
		"type": "session.update", "session": map[string]any{
			"type": "realtime", "openrealtime": map[string]any{
				"version": 1, "supports": []string{"client.effects"},
			},
		},
	})
	updated := awaitGatewayFixtureEvent(t, connection, "session.updated")
	sessionObject, _ := updated["session"].(map[string]any)
	extension, _ := sessionObject["openrealtime"].(map[string]any)
	if enabled, _ := extension["enabled"].([]any); len(enabled) != 0 {
		t.Fatalf("gateway advertised authority after provider loss: %#v", extension)
	}
}

func TestRealGatewayDeclinesClientEffectsWithoutAnIssuer(t *testing.T) {
	stack := testserver.Start(t, testserver.Config{})
	connection, _, err := websocket.Dial(context.Background(), stack.ProtocolURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(websocket.StatusNormalClosure, "done") })
	awaitGatewayFixtureEvent(t, connection, "session.created")
	digest := "sha256:" + strings.Repeat("f", 64)
	sendGatewayFixtureEvent(t, connection, map[string]any{
		"type": "session.update", "session": map[string]any{
			"type": "realtime",
			"openrealtime": map[string]any{
				"version": openrealtime.Version, "supports": []string{"client.effects"},
			},
			"tools": []map[string]any{{
				"type": "function", "name": "display_artifact", "description": "display",
				"parameters": map[string]any{"type": "object"},
				"openrealtime": map[string]any{"client_effect": map[string]any{
					"version": 1, "declaration_digest": digest,
				}},
			}},
		},
	})
	failure := awaitGatewayFixtureEvent(t, connection, "error")
	detail, _ := failure["error"].(map[string]any)
	if !strings.Contains(stringValue(detail["message"]), "requires negotiated client.effects") {
		t.Fatalf("feature decline = %#v", failure)
	}
}
