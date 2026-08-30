package authority_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/authority"
)

func effectClaims() authority.EffectReceiptClaims {
	return authority.EffectReceiptClaims{
		SessionID: "sess_authority_a", CallID: "call_authority_a", Name: "display_artifact",
		ArgumentsDigest:   "sha256:" + strings.Repeat("a", sha256.Size*2),
		DeclarationDigest: "sha256:" + strings.Repeat("b", sha256.Size*2), Target: "browser-1",
	}
}

func newEffectProvider(t *testing.T, now *atomic.Int64) *authority.SealedEffectReceipts {
	t.Helper()
	random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, 1<<20))
	provider, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{
		TTL:    time.Second,
		Clock:  func() time.Time { return time.Unix(0, now.Load()).UTC() },
		Random: random,
	})
	if err != nil {
		t.Fatalf("new effect receipt provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

func TestSealedEffectReceiptAuthorizesOnlyTheExactShortLivedCall(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	provider := newEffectProvider(t, &now)
	claims := effectClaims()
	if !provider.Available() {
		t.Fatal("new receipt provider reported unavailable")
	}
	receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(receipt, "ore1.") || len(receipt) >= 1024 ||
		strings.Contains(receipt, claims.SessionID) || strings.Contains(receipt, claims.CallID) ||
		strings.Contains(receipt, claims.Name) || strings.Contains(receipt, claims.Target) {
		t.Fatalf("receipt is not compact and opaque: length=%d receipt=%q", len(receipt), receipt)
	}
	if err := provider.VerifyEffectReceipt(context.Background(), receipt, claims); err != nil {
		t.Fatalf("verify exact receipt: %v", err)
	}
	// Exact transport retransmission remains valid. The effect host's bounded
	// call ledger suppresses a duplicate irreversible dispatch.
	if err := provider.VerifyEffectReceipt(context.Background(), receipt, claims); err != nil {
		t.Fatalf("verify exact retransmission: %v", err)
	}
	secondReceipt, err := provider.IssueEffectReceipt(context.Background(), claims)
	if err != nil || secondReceipt == receipt {
		t.Fatalf("independent issuance reused an AEAD nonce: equal=%v err=%v", secondReceipt == receipt, err)
	}

	mutations := []struct {
		name string
		edit func(*authority.EffectReceiptClaims)
	}{
		{"session", func(value *authority.EffectReceiptClaims) { value.SessionID = "sess_authority_b" }},
		{"call", func(value *authority.EffectReceiptClaims) { value.CallID = "call_authority_b" }},
		{"tool", func(value *authority.EffectReceiptClaims) { value.Name = "publish_download" }},
		{"arguments", func(value *authority.EffectReceiptClaims) {
			value.ArgumentsDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{"declaration", func(value *authority.EffectReceiptClaims) {
			value.DeclarationDigest = "sha256:" + strings.Repeat("d", 64)
		}},
		{"target", func(value *authority.EffectReceiptClaims) { value.Target = "browser-2" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := claims
			mutation.edit(&changed)
			if err := provider.VerifyEffectReceipt(context.Background(), receipt, changed); !errors.Is(err, authority.ErrEffectReceiptMismatch) {
				t.Fatalf("cross-%s replay = %v", mutation.name, err)
			}
		})
	}

	now.Add(int64(time.Second))
	if err := provider.VerifyEffectReceipt(context.Background(), receipt, claims); !errors.Is(err, authority.ErrEffectReceiptExpired) {
		t.Fatalf("expired receipt = %v", err)
	}
}

func TestSealedEffectReceiptRejectsTamperKeyDriftAndLifecycleLoss(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	first := newEffectProvider(t, &now)
	secondRandom := bytes.NewReader(bytes.Repeat([]byte{0xa5}, 1<<20))
	second, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{
		TTL: time.Second, Clock: func() time.Time { return time.Unix(0, now.Load()) }, Random: secondRandom,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	claims := effectClaims()
	receipt, err := first.IssueEffectReceipt(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity() == "" || first.Identity() == second.Identity() ||
		strings.Contains(first.Identity(), receipt) {
		t.Fatalf("provider identities are not stable non-secret selectors: %q %q", first.Identity(), second.Identity())
	}
	if err := second.VerifyEffectReceipt(context.Background(), receipt, claims); !errors.Is(err, authority.ErrEffectReceiptInvalid) {
		t.Fatalf("cross-key receipt = %v", err)
	}

	encoded := strings.TrimPrefix(receipt, "ore1.")
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)/2] ^= 0x40
	tampered := "ore1." + base64.RawURLEncoding.EncodeToString(sealed)
	if err := first.VerifyEffectReceipt(context.Background(), tampered, claims); !errors.Is(err, authority.ErrEffectReceiptInvalid) {
		t.Fatalf("tampered receipt = %v", err)
	}
	for _, invalid := range []string{"", "ore1.", "ore1.***", receipt + "=", "ORE1." + encoded} {
		if err := first.VerifyEffectReceipt(context.Background(), invalid, claims); !errors.Is(err, authority.ErrEffectReceiptInvalid) {
			t.Fatalf("invalid receipt %q = %v", invalid, err)
		}
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if first.Available() {
		t.Fatal("zeroized receipt provider remained available")
	}
	if _, err := first.IssueEffectReceipt(context.Background(), claims); !errors.Is(err, authority.ErrEffectReceiptClosed) {
		t.Fatalf("issue after zeroization = %v", err)
	}
	if err := first.VerifyEffectReceipt(context.Background(), receipt, claims); !errors.Is(err, authority.ErrEffectReceiptClosed) {
		t.Fatalf("verify after zeroization = %v", err)
	}
}

func TestEffectReceiptRejectsMalformedClaimsAndCanceledWork(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	provider := newEffectProvider(t, &now)
	claims := effectClaims()
	invalid := []authority.EffectReceiptClaims{
		{},
		func() authority.EffectReceiptClaims { value := claims; value.SessionID = " session"; return value }(),
		func() authority.EffectReceiptClaims {
			value := claims
			value.CallID = strings.Repeat("x", 257)
			return value
		}(),
		func() authority.EffectReceiptClaims { value := claims; value.Name = "1bad"; return value }(),
		func() authority.EffectReceiptClaims {
			value := claims
			value.ArgumentsDigest = "sha256:ABC"
			return value
		}(),
		func() authority.EffectReceiptClaims {
			value := claims
			value.DeclarationDigest = "sha256:" + strings.Repeat("g", 64)
			return value
		}(),
		func() authority.EffectReceiptClaims { value := claims; value.Target = "two targets"; return value }(),
	}
	for index, value := range invalid {
		if _, err := provider.IssueEffectReceipt(context.Background(), value); err == nil {
			t.Fatalf("invalid claims %d were issued", index)
		}
	}
	canceled, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("test canceled")
	cancel(cause)
	if _, err := provider.IssueEffectReceipt(canceled, claims); !errors.Is(err, cause) {
		t.Fatalf("canceled issue = %v", err)
	}
	if err := provider.VerifyEffectReceipt(canceled, "ore1.unused", claims); !errors.Is(err, cause) {
		t.Fatalf("canceled verify = %v", err)
	}
	if _, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{TTL: 6 * time.Minute}); err == nil {
		t.Fatal("unbounded receipt TTL was accepted")
	}
}

func TestCanonicalEffectArgumentsIsOrderIndependentAndStrict(t *testing.T) {
	first, firstDigest, err := authority.CanonicalEffectArguments([]byte(`{"b":2,"a":{"y":2,"x":1},"n":1.25}`))
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := authority.CanonicalEffectArguments([]byte(` { "n" : 1.25, "a" : {"x":1,"y":2}, "b":2 } `))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || firstDigest != secondDigest || !strings.HasPrefix(firstDigest, "sha256:") {
		t.Fatalf("canonical values drifted: %s %s / %s %s", first, firstDigest, second, secondDigest)
	}
	for _, invalid := range []string{
		`{"a":1,"a":2}`, `[]`, `null`, `{"n":1e1000000}`, `{"a":1} {"b":2}`,
		`{"deep":` + strings.Repeat("[", 256) + `0` + strings.Repeat("]", 256) + `}`,
	} {
		if _, _, err := authority.CanonicalEffectArguments([]byte(invalid)); err == nil {
			t.Fatalf("non-strict arguments accepted: %s", invalid)
		}
	}
}

func TestCanonicalEffectArgumentsMatchesBrowserNumberSemantics(t *testing.T) {
	// Expected spellings are JSON.stringify outputs from the JavaScript client
	// parser. The Go gateway and host must hash exactly the same binary64
	// semantics after a browser has parsed and forwarded the arguments.
	tests := []struct {
		source string
		want   string
	}{
		{`-0`, `0`},
		{`-0.0e99`, `0`},
		{`1e-7`, `1e-7`},
		{`1e-6`, `0.000001`},
		{`1e20`, `100000000000000000000`},
		{`1e21`, `1e+21`},
		{`1e-9`, `1e-9`},
		{`9007199254740993`, `9007199254740992`},
		{`4.9406564584124654e-324`, `5e-324`},
		{`2.2250738585072014e-308`, `2.2250738585072014e-308`},
	}
	for _, test := range tests {
		t.Run(test.source, func(t *testing.T) {
			canonical, _, err := authority.CanonicalEffectArguments([]byte(
				`{"nested":[` + test.source + `],"number":` + test.source + `}`,
			))
			if err != nil {
				t.Fatal(err)
			}
			want := `{"nested":[` + test.want + `],"number":` + test.want + `}`
			if string(canonical) != want {
				t.Fatalf("browser parity = %s, want %s", canonical, want)
			}
		})
	}
}

func TestSealedEffectReceiptsSynchronizesConcurrentUseAndClose(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	provider := newEffectProvider(t, &now)
	claims := effectClaims()
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < 100; attempt++ {
				receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
				if errors.Is(err, authority.ErrEffectReceiptClosed) {
					return
				}
				if err != nil {
					t.Errorf("concurrent issue: %v", err)
					return
				}
				if err := provider.VerifyEffectReceipt(context.Background(), receipt, claims); err != nil &&
					!errors.Is(err, authority.ErrEffectReceiptClosed) {
					t.Errorf("concurrent verify: %v", err)
					return
				}
			}
		}()
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
}

func FuzzEffectReceiptTamperNeverAuthorizes(fuzz *testing.F) {
	fuzz.Add([]byte{0})
	fuzz.Add([]byte("ore1.invalid"))
	fuzz.Fuzz(func(t *testing.T, mutation []byte) {
		var now atomic.Int64
		now.Store(time.Unix(1_800_000_000, 0).UnixNano())
		provider := newEffectProvider(t, &now)
		claims := effectClaims()
		receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
		if err != nil {
			t.Fatal(err)
		}
		if len(mutation) == 0 {
			return
		}
		candidate := []byte(receipt)
		for index, value := range mutation {
			candidate[index%len(candidate)] ^= value | 1
		}
		if string(candidate) == receipt {
			return
		}
		if err := provider.VerifyEffectReceipt(context.Background(), string(candidate), claims); err == nil {
			t.Fatal("mutated receipt authorized")
		}
	})
}

func FuzzCanonicalEffectArguments(fuzz *testing.F) {
	fuzz.Add([]byte(`{"a":1}`))
	fuzz.Add([]byte(`{"a":1,"a":2}`))
	fuzz.Add([]byte(`[]`))
	fuzz.Fuzz(func(t *testing.T, input []byte) {
		canonical, digest, err := authority.CanonicalEffectArguments(input)
		if err != nil {
			return
		}
		next, nextDigest, err := authority.CanonicalEffectArguments(canonical)
		if err != nil || !bytes.Equal(canonical, next) || digest != nextDigest {
			t.Fatalf("canonicalization is not idempotent: %s %s / %s %s / %v", canonical, digest, next, nextDigest, err)
		}
	})
}

func BenchmarkEffectReceiptIssue(b *testing.B) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	provider, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{Clock: func() time.Time {
		return time.Unix(0, now.Load())
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = provider.Close() })
	claims := effectClaims()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := provider.IssueEffectReceipt(context.Background(), claims); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEffectReceiptVerify(b *testing.B) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	provider, err := authority.NewSealedEffectReceipts(authority.EffectReceiptOptions{Clock: func() time.Time {
		return time.Unix(0, now.Load())
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = provider.Close() })
	claims := effectClaims()
	receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := provider.VerifyEffectReceipt(context.Background(), receipt, claims); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCanonicalEffectArguments(b *testing.B) {
	arguments := []byte(`{"artifact_id":"report","html":"<main>bounded</main>","title":"Report"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(arguments)))
	for index := 0; index < b.N; index++ {
		if _, _, err := authority.CanonicalEffectArguments(arguments); err != nil {
			b.Fatal(err)
		}
	}
}
