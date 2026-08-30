package host

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestEffectReceiptAuthorityIsAnIndependentDescriptorLockedProvider(t *testing.T) {
	provider, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewEffectReceiptAuthorityFactory(provider)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := factory.Descriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "openrealtime.presentation.host.effect-receipt-authority" ||
		descriptor.Realm != plugin.PresentationHostRealm ||
		!slices.Equal(descriptor.Provides, []plugin.Contract{presentation.EffectAuthorityContract}) ||
		len(descriptor.Requires) != 0 || len(descriptor.Permissions) != 0 ||
		len(descriptor.Assets) != 0 || descriptor.ConfigSchema != nil ||
		descriptor.Lifecycle.DisposeTimeoutMS == 0 {
		t.Fatalf("effect receipt authority descriptor = %#v", descriptor)
	}
	identity, err := descriptor.Identity()
	if err != nil || identity.Digest == "" {
		t.Fatalf("effect receipt authority identity = %#v, %v", identity, err)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), provider.Identity()) || strings.Contains(string(encoded), "authority") &&
		strings.Contains(string(encoded), "ore1.") {
		t.Fatalf("descriptor retained runtime key identity or authority evidence: %s", encoded)
	}

	effects, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := attemptEffectMount(t, effects, factory, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	claims := effectauthority.EffectReceiptClaims{
		SessionID: "sess_lifecycle", CallID: "call_lifecycle", Name: "display_artifact",
		ArgumentsDigest:   "sha256:" + strings.Repeat("a", 64),
		DeclarationDigest: "sha256:" + strings.Repeat("b", 64),
	}
	if _, err := provider.IssueEffectReceipt(context.Background(), claims); err != nil {
		t.Fatalf("mounted provider could not issue: %v", err)
	}
	if err := mounted.Unmount(context.Background(), "authority"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.IssueEffectReceipt(context.Background(), claims); !errors.Is(err, effectauthority.ErrEffectReceiptClosed) {
		t.Fatalf("authority unmount did not revoke and zeroize issuer: %v", err)
	}
}

func TestEffectReceiptAuthorityExecutesOnlyTheServerSealedHostCall(t *testing.T) {
	provider, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	declaration := factory.tools[0].declaration
	arguments := json.RawMessage(`{"html":"<main>sealed</main>","title":"Sealed","artifact_id":"sealed"}`)
	_, argumentsDigest, err := effectauthority.CanonicalEffectArguments(arguments)
	if err != nil {
		t.Fatal(err)
	}
	claims := effectauthority.EffectReceiptClaims{
		SessionID: "sess_sealed", CallID: "call_sealed", Name: declaration.Name,
		ArgumentsDigest: argumentsDigest, DeclarationDigest: declaration.Digest,
		Target: declaration.Target,
	}
	receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	authority := &effectReceiptAuthority{provider: provider, identity: provider.Identity()}
	host := mountEffectTestHost(t, factory, authority, nil)
	socket := dialEffectTestSocket(t, host.server)
	var argumentObject map[string]any
	if err := json.Unmarshal(arguments, &argumentObject); err != nil {
		t.Fatal(err)
	}
	socket.send(effectFixtureCall(
		claims.SessionID, claims.CallID, claims.Name, argumentObject, receipt,
	))
	result := socket.receive()
	if result.Error != nil || result.Artifact == nil || result.Artifact.ID != "sealed" {
		t.Fatalf("sealed effect result = %#v", result)
	}

	// Readdressing the same receipt to another call is rejected before the
	// idempotent dispatcher and store boundary.
	socket.send(effectFixtureCall(
		claims.SessionID, "call_replayed", claims.Name, argumentObject, receipt,
	))
	replayed := socket.receive()
	if replayed.Error == nil || replayed.Error.Code != "authority_denied" ||
		host.effects.Stats().Executed != 1 {
		t.Fatalf("cross-call replay = %#v stats=%#v", replayed, host.effects.Stats())
	}

	// A wire mutation cannot be turned into a fresh authorization by the
	// client; it fails cryptographic verification.
	tampered := []byte(receipt)
	tampered[len(tampered)-1] ^= 1
	socket.send(effectFixtureCall(
		claims.SessionID, "call_tampered", claims.Name, argumentObject, string(tampered),
	))
	tamperResult := socket.receive()
	if tamperResult.Error == nil || tamperResult.Error.Code != "authority_denied" ||
		host.effects.Stats().Executed != 1 {
		t.Fatalf("tampered receipt = %#v stats=%#v", tamperResult, host.effects.Stats())
	}
}

type driftingEffectVerifier struct {
	mu       sync.Mutex
	identity string
	drift    bool
}

func (provider *driftingEffectVerifier) Identity() string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.identity
}

func (provider *driftingEffectVerifier) VerifyEffectReceipt(
	context.Context, string, effectauthority.EffectReceiptClaims,
) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.drift {
		provider.identity = "drifted"
	}
	return nil
}

func (*driftingEffectVerifier) Close() error { return nil }

func TestEffectReceiptAuthorityRejectsNilAndLiveProviderDrift(t *testing.T) {
	var missing *driftingEffectVerifier
	if factory, err := NewEffectReceiptAuthorityFactory(missing); err == nil || factory != nil {
		t.Fatalf("typed-nil verifier factory = %#v, %v", factory, err)
	}
	provider := &driftingEffectVerifier{identity: "stable", drift: true}
	authority := &effectReceiptAuthority{provider: provider, identity: "stable"}
	decision, err := authority.Authorize(context.Background(), EffectAuthorityRequest{
		SessionID: "sess_drift", CallID: "call_drift", Name: "display_artifact",
		ArgumentsDigest:   "sha256:" + strings.Repeat("a", 64),
		DeclarationDigest: "sha256:" + strings.Repeat("b", 64),
		Evidence:          "opaque",
	})
	if err == nil || decision != (EffectAuthorityDecision{}) || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("live verifier drift = %#v, %v", decision, err)
	}
}
