package host

import (
	"context"
	"encoding/json"
	"testing"
)

func BenchmarkEffectsFactoryDescriptorAndCatalog(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		factory, err := NewEffectsFactory(EffectsOptions{})
		if err != nil {
			b.Fatal(err)
		}
		if len(factory.DeclarationsDocument()) == 0 {
			b.Fatal("empty declaration catalog")
		}
	}
}

func BenchmarkEffectWireCallDecode(b *testing.B) {
	payload := []byte(`{"type":"call","session_id":"sess_bench","id":"call_bench","name":"display_artifact","arguments":{"artifact_id":"bench","title":"Bench","html":"<p>42</p>"},"authority":"signed-fixture"}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		envelope, err := decodeEffectEnvelope(payload)
		if err != nil || envelope.Type != "call" {
			b.Fatal(err)
		}
		call, err := decodeEffectCall(payload)
		if err != nil || call.ID != "call_bench" {
			b.Fatal(err)
		}
	}
}

func BenchmarkEffectSchemaAdmission(b *testing.B) {
	tools, err := builtinEffectTools()
	if err != nil {
		b.Fatal(err)
	}
	arguments := []byte(`{"artifact_id":"bench","title":"Bench","html":"<p>42</p>"}`)
	b.SetBytes(int64(len(arguments)))
	b.ReportAllocs()
	for b.Loop() {
		canonical, digest, err := canonicalEffectArguments(tools[0].schema, arguments)
		if err != nil || len(canonical) == 0 || digest == "" {
			b.Fatal(err)
		}
	}
}

func BenchmarkEffectHubIdempotencyReplayLookup(b *testing.B) {
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		b.Fatal(err)
	}
	limits, err := parseEffectConfig(json.RawMessage(`{}`))
	if err != nil {
		b.Fatal(err)
	}
	hub, err := newEffectsHub(
		context.Background(), limits, factory.tools, denyEffectAuthority{}, nil, factory.CatalogDigest(),
	)
	if err != nil {
		b.Fatal(err)
	}
	session := &effectSession{
		hub: hub, scopeID: "00000000000000000000000000000000", ctx: context.Background(),
		decisions: make(map[string]*pendingEffectConfirmation),
	}
	call := EffectCall{
		ScopeID: session.scopeID, SessionID: "sess_bench", CallID: "call_bench",
		Name: "display_artifact", Arguments: json.RawMessage(`{"artifact_id":"bench","html":"x","title":"Bench"}`),
		ArgumentsDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeclarationDigest: factory.tools[0].declaration.Digest,
	}
	tool := &factory.tools[0]
	if _, owner, err := session.reserveCall(call, tool); err != nil || !owner {
		b.Fatalf("seed call: owner=%v err=%v", owner, err)
	}
	b.ReportAllocs()
	for b.Loop() {
		state, owner, err := session.reserveCall(call, tool)
		if err != nil || owner || state == nil {
			b.Fatalf("replay lookup: owner=%v state=%p err=%v", owner, state, err)
		}
	}
}
