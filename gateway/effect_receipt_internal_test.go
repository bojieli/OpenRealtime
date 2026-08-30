package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type recordingEffectIssuer struct {
	mu      sync.Mutex
	claims  []effectauthority.EffectReceiptClaims
	receipt string
	failAt  int
	err     error
}

func (issuer *recordingEffectIssuer) IssueEffectReceipt(
	_ context.Context, claims effectauthority.EffectReceiptClaims,
) (string, error) {
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	issuer.claims = append(issuer.claims, claims)
	if issuer.failAt > 0 && len(issuer.claims) == issuer.failAt {
		return "", issuer.err
	}
	return issuer.receipt, nil
}

func (*recordingEffectIssuer) Available() bool { return true }

func effectReceiptSession(issuer effectauthority.EffectReceiptIssuerProvider) *session {
	digest := "sha256:" + strings.Repeat("d", 64)
	return &session{
		ctx: context.Background(), id: "sess_internal",
		config: Config{ClientEffectIssuer: issuer},
		settings: settings{
			extension: openrealtime.Response{
				Version: 1, Enabled: []openrealtime.Feature{openrealtime.FeatureClientEffects},
			},
			tools: []action.ToolSpec{{
				Name: "display_artifact", Description: "display", Parameters: json.RawMessage(`{"type":"object"}`),
				Target: "browser-1",
			}},
			clientEffects: map[string]openrealtime.ClientEffectDeclaration{
				"display_artifact": {Version: 1, DeclarationDigest: digest},
			},
		},
	}
}

func TestPrepareClientEffectCallsBindsGatewayOwnedSessionAndWholeBatch(t *testing.T) {
	issuer := &recordingEffectIssuer{receipt: "opaque-server-receipt"}
	session := effectReceiptSession(issuer)
	calls := []trajectory.ToolCall{
		{CallID: "call_1", Name: "display_artifact", Arguments: json.RawMessage(`{"z":2,"a":1.0}`)},
		{CallID: "call_2", Name: "ordinary", Arguments: json.RawMessage(`{}`)},
	}
	prepared, err := session.prepareClientEffectCalls(context.Background(), calls)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 || prepared[0].extension == nil || prepared[1].extension != nil ||
		prepared[0].extension.Authority != "opaque-server-receipt" {
		t.Fatalf("prepared calls = %#v", prepared)
	}
	issuer.mu.Lock()
	issued := append([]effectauthority.EffectReceiptClaims(nil), issuer.claims...)
	issuer.mu.Unlock()
	_, argumentsDigest, err := effectauthority.CanonicalEffectArguments(calls[0].Arguments)
	if err != nil {
		t.Fatal(err)
	}
	want := effectauthority.EffectReceiptClaims{
		SessionID: "sess_internal", CallID: "call_1", Name: "display_artifact",
		ArgumentsDigest:   argumentsDigest,
		DeclarationDigest: "sha256:" + strings.Repeat("d", 64), Target: "browser-1",
	}
	if len(issued) != 1 || issued[0] != want {
		t.Fatalf("issuer claims = %#v, want %#v", issued, want)
	}
}

func TestPrepareClientEffectCallsFailsClosedBeforePartialEmission(t *testing.T) {
	calls := []trajectory.ToolCall{
		{CallID: "call_1", Name: "display_artifact", Arguments: json.RawMessage(`{}`)},
		{CallID: "call_2", Name: "display_artifact", Arguments: json.RawMessage(`{}`)},
	}
	for _, test := range []struct {
		name   string
		issuer effectauthority.EffectReceiptIssuerProvider
	}{
		{"missing", nil},
		{"issuer refusal", &recordingEffectIssuer{
			receipt: "opaque", failAt: 2, err: errors.New("secret provider diagnostic must not escape"),
		}},
		{"invalid receipt", &recordingEffectIssuer{receipt: "two receipts"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := effectReceiptSession(test.issuer)
			prepared, err := session.prepareClientEffectCalls(context.Background(), calls)
			if err == nil || prepared != nil {
				t.Fatalf("fail-closed preparation = %#v, %v", prepared, err)
			}
			if strings.Contains(err.Error(), "secret provider diagnostic") {
				t.Fatalf("issuer diagnostic escaped into gateway evidence: %v", err)
			}
		})
	}
}

type blockingEffectIssuer struct {
	entered chan struct{}
	release chan struct{}
}

func (issuer blockingEffectIssuer) IssueEffectReceipt(
	ctx context.Context, _ effectauthority.EffectReceiptClaims,
) (string, error) {
	select {
	case issuer.entered <- struct{}{}:
	default:
	}
	select {
	case <-issuer.release:
		return "opaque", nil
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

func (blockingEffectIssuer) Available() bool { return true }

func TestPrepareClientEffectCallsRejectsDeclarationDriftDuringIssuance(t *testing.T) {
	issuer := blockingEffectIssuer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	session := effectReceiptSession(issuer)
	done := make(chan error, 1)
	go func() {
		_, err := session.prepareClientEffectCalls(context.Background(), []trajectory.ToolCall{{
			CallID: "call_drift", Name: "display_artifact", Arguments: json.RawMessage(`{}`),
		}})
		done <- err
	}()
	select {
	case <-issuer.entered:
	case <-time.After(time.Second):
		t.Fatal("issuer did not start")
	}
	session.settingsMu.Lock()
	session.settings.clientEffects["display_artifact"] = openrealtime.ClientEffectDeclaration{
		Version: 1, DeclarationDigest: "sha256:" + strings.Repeat("e", 64),
	}
	session.settingsMu.Unlock()
	close(issuer.release)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "drifted") {
			t.Fatalf("declaration drift = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drift check did not finish")
	}
}

func BenchmarkGatewayPrepareClientEffectCall(b *testing.B) {
	provider, err := effectauthority.NewSealedEffectReceipts(effectauthority.EffectReceiptOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = provider.Close() })
	session := effectReceiptSession(provider)
	calls := []trajectory.ToolCall{{
		CallID: "call_bench", Name: "display_artifact",
		Arguments: json.RawMessage(`{"artifact_id":"report","html":"<main>42</main>","title":"Report"}`),
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := session.prepareClientEffectCalls(context.Background(), calls); err != nil {
			b.Fatal(err)
		}
	}
}
