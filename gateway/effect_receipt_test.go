package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
	"net/http/httptest"
)

func startServerWithEffectIssuer(
	t *testing.T, fastProvider, slowProvider *scripted,
	issuer authority.EffectReceiptIssuerProvider,
) *httptest.Server {
	t.Helper()
	// Build through the same graph-backed cascade as the main gateway harness;
	// only the independently replaceable receipt issuer differs.
	legacy, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "show report"}, nil },
		Fast:       fastProvider, Slow: slowProvider, Speech: toneSpeech{},
		Voice: "test-voice", FastMaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("new cascade: %v", err)
	}
	bind, err := graphbinding.New(legacy)
	if err != nil {
		t.Fatalf("mount cascade through graph: %v", err)
	}
	server, err := gateway.New(gateway.Config{
		Binding: bind, Model: "openrealtime-test", ValidateWire: true,
		ClientEffectIssuer: issuer,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestNegotiatedClientEffectReceivesExactServerIssuedAuthority(t *testing.T) {
	provider, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	digest := "sha256:" + strings.Repeat("d", 64)
	arguments := json.RawMessage(`{"title":"Report","artifact_id":"report","html":"<main>42</main>"}`)
	server := startServerWithEffectIssuer(t, fast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Working.",
	}}), slow([]continuation.Event{{
		Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_effect_1", Name: "display_artifact", Arguments: arguments,
		},
	}}), provider)
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	sessionID := created["session"].(map[string]any)["id"].(string)
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
		"openrealtime": map[string]any{
			"version": openrealtime.Version, "supports": []string{"client.effects"},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "display_artifact", "description": "display an artifact",
			"parameters": map[string]any{"type": "object"},
			"openrealtime": map[string]any{
				"target": "browser-1",
				"client_effect": map[string]any{
					"version": 1, "declaration_digest": digest,
					// This unknown client assertion must be ignored, never
					// echoed or substituted for a server receipt.
					"authority": "client-minted",
				},
			},
		}},
	}})
	updated := client.await("session.updated", 5*time.Second)
	updatedSession := updated["session"].(map[string]any)
	extension := updatedSession["openrealtime"].(map[string]any)
	enabled := extension["enabled"].([]any)
	if len(enabled) != 1 || enabled[0] != "client.effects" {
		t.Fatalf("client effects negotiation = %+v", enabled)
	}
	definition := updatedSession["tools"].([]any)[0].(map[string]any)
	declared := definition["openrealtime"].(map[string]any)["client_effect"].(map[string]any)
	if declared["declaration_digest"] != digest || declared["authority"] != nil {
		t.Fatalf("tool declaration gained client authority: %+v", declared)
	}

	client.speak()
	done := client.await("response.function_call_arguments.done", 10*time.Second)
	emitted := done["openrealtime"].(map[string]any)["client_effect"].(map[string]any)
	receipt, _ := emitted["authority"].(string)
	if emitted["declaration_digest"] != digest || receipt == "" || receipt == "client-minted" {
		t.Fatalf("server call extension = %+v", emitted)
	}
	_, argumentsDigest, err := authority.CanonicalEffectArguments(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.VerifyEffectReceipt(context.Background(), receipt, authority.EffectReceiptClaims{
		SessionID: sessionID, CallID: "call_effect_1", Name: "display_artifact",
		ArgumentsDigest: argumentsDigest, DeclarationDigest: digest, Target: "browser-1",
	}); err != nil {
		t.Fatalf("gateway receipt did not bind the exact call: %v", err)
	}
}

func TestClientEffectIsAbsentWithoutNegotiationEvenWhenIssuerExists(t *testing.T) {
	provider, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	server := startServerWithEffectIssuer(t, fast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Working.",
	}}), slow([]continuation.Event{{
		Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_base", Name: "ordinary", Arguments: json.RawMessage(`{}`),
		},
	}}), provider)
	client := dial(t, server)
	created := client.await("session.created", 5*time.Second)
	if _, found := created["session"].(map[string]any)["openrealtime"]; found {
		t.Fatal("issuer availability volunteered an unnegotiated extension")
	}
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "ordinary", "description": "ordinary tool",
			"parameters": map[string]any{"type": "object"},
		}},
	}})
	updated := client.await("session.updated", 5*time.Second)
	if _, found := updated["session"].(map[string]any)["openrealtime"]; found {
		t.Fatal("ordinary update gained an unnegotiated extension")
	}
	client.speak()
	done := client.await("response.function_call_arguments.done", 10*time.Second)
	if _, found := done["openrealtime"]; found {
		t.Fatalf("ordinary call gained effect authority: %+v", done)
	}
}

func TestClientEffectDeclarationFailsClosedWithoutIssuer(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"openrealtime": map[string]any{
			"version": 1, "supports": []string{"client.effects"},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "display_artifact", "description": "display",
			"parameters": map[string]any{"type": "object"},
			"openrealtime": map[string]any{"client_effect": map[string]any{
				"version": 1, "declaration_digest": digest,
			}},
		}},
	}})
	failure := client.await("error", 5*time.Second)
	message := failure["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "requires negotiated client.effects") {
		t.Fatalf("missing issuer failure = %q", message)
	}
	// The rejected declaration is atomic and does not poison an ordinary
	// follow-up update on the same base-compatible connection.
	client.configurePCM16(nil)
	updated := client.await("session.updated", 5*time.Second)
	if _, found := updated["session"].(map[string]any)["openrealtime"]; found {
		t.Fatal("rejected client-effect negotiation mutated the session")
	}
}
