package authority_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/authority"
)

// A client-effect receipt is the audit evidence that one external effect was
// authorized. Its bounds were exercised only on the accepting path: the nil
// provider, the future-dated receipt, the tool-name and identity canonical
// forms, and the JSON depth preflight all had guards no test reached.

// A receipt that claims to have been issued after the current instant is not a
// clock problem to tolerate; it is either skew or a forged timestamp, and
// accepting it would let an effect be authorized before it was issued.
func TestFutureDatedEffectReceiptIsRefused(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	provider := newEffectProvider(t, &now)
	claims := effectClaims()
	receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.VerifyEffectReceipt(context.Background(), receipt, claims); err != nil {
		t.Fatalf("receipt at its issuing instant = %v, want accepted", err)
	}
	now.Store(time.Unix(1_800_000_000, 0).Add(-time.Millisecond).UnixNano())
	err = provider.VerifyEffectReceipt(context.Background(), receipt, claims)
	if !errors.Is(err, authority.ErrEffectReceiptInvalid) {
		t.Fatalf("receipt verified before it was issued = %v, want ErrEffectReceiptInvalid", err)
	}
}

func TestNilEffectProviderRefusesVerification(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	provider := newEffectProvider(t, &now)
	claims := effectClaims()
	receipt, err := provider.IssueEffectReceipt(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	var absent *authority.SealedEffectReceipts
	err = absent.VerifyEffectReceipt(context.Background(), receipt, claims)
	if !errors.Is(err, authority.ErrEffectReceiptClosed) {
		t.Fatalf("nil provider = %v, want ErrEffectReceiptClosed", err)
	}
}

func TestEffectReceiptClaimsMustBeCanonical(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	provider := newEffectProvider(t, &now)
	digest := "sha256:" + strings.Repeat("a", sha256.Size*2)
	for _, test := range []struct {
		name string
		edit func(*authority.EffectReceiptClaims)
		want string
	}{
		{
			name: "no tool name",
			edit: func(claims *authority.EffectReceiptClaims) { claims.Name = "" },
			want: "tool name is not canonical",
		},
		{
			name: "tool name beyond its bound",
			edit: func(claims *authority.EffectReceiptClaims) { claims.Name = strings.Repeat("a", 129) },
			want: "tool name is not canonical",
		},
		{
			name: "tool name starts with a digit",
			edit: func(claims *authority.EffectReceiptClaims) { claims.Name = "1display" },
			want: "tool name is not canonical",
		},
		{
			name: "tool name carries a separator",
			edit: func(claims *authority.EffectReceiptClaims) { claims.Name = "display artifact" },
			want: "tool name is not canonical",
		},
		{
			name: "no session identity",
			edit: func(claims *authority.EffectReceiptClaims) { claims.SessionID = "" },
			want: "session_id is not canonical",
		},
		{
			name: "session identity beyond its bound",
			edit: func(claims *authority.EffectReceiptClaims) { claims.SessionID = strings.Repeat("s", 257) },
			want: "session_id is not canonical",
		},
		{
			name: "session identity carries surrounding space",
			edit: func(claims *authority.EffectReceiptClaims) { claims.SessionID = " sess_a" },
			want: "session_id is not canonical",
		},
		{
			name: "call identity carries a control character",
			edit: func(claims *authority.EffectReceiptClaims) { claims.CallID = "call\x01a" },
			want: "call_id contains whitespace or control characters",
		},
		{
			name: "arguments digest is not a digest",
			edit: func(claims *authority.EffectReceiptClaims) { claims.ArgumentsDigest = "sha256:short" },
			want: "canonical arguments digest",
		},
		{
			name: "declaration digest is not a digest",
			edit: func(claims *authority.EffectReceiptClaims) { claims.DeclarationDigest = digest[:20] },
			want: "canonical declaration digest",
		},
		{
			name: "target carries a control character",
			edit: func(claims *authority.EffectReceiptClaims) { claims.Target = "browser\x7f" },
			want: "target contains whitespace or control characters",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := effectClaims()
			test.edit(&claims)
			_, issueErr := provider.IssueEffectReceipt(context.Background(), claims)
			if issueErr == nil || !strings.Contains(issueErr.Error(), test.want) {
				t.Fatalf("issue error = %v, want one containing %q", issueErr, test.want)
			}
			verifyErr := provider.VerifyEffectReceipt(context.Background(), "effect_receipt.x", claims)
			if verifyErr == nil || !strings.Contains(verifyErr.Error(), test.want) {
				t.Fatalf("verify error = %v, want one containing %q", verifyErr, test.want)
			}
		})
	}

	// The one identity that may be absent: an effect with no declared target.
	absentTarget := effectClaims()
	absentTarget.Target = ""
	if _, err := provider.IssueEffectReceipt(context.Background(), absentTarget); err != nil {
		t.Fatalf("claims without a target = %v, want accepted", err)
	}
}

// The depth preflight exists so an adversarial nesting cannot drive the strict
// duplicate-key walk into unbounded recursion. It has to count only structural
// delimiters, so a brace inside a string must not raise depth.
func TestCanonicalEffectArgumentsBoundNestingDepth(t *testing.T) {
	if _, _, err := authority.CanonicalEffectArguments(
		[]byte(`{"a":"{{{{{{{{[[[[[[[["}`),
	); err != nil {
		t.Fatalf("delimiters inside a string = %v, want accepted", err)
	}
	deep := []byte(strings.Repeat("[", 257) + strings.Repeat("]", 257))
	_, _, err := authority.CanonicalEffectArguments(deep)
	if err == nil || !strings.Contains(err.Error(), "exceed JSON depth") {
		t.Fatalf("deeply nested arguments = %v, want a depth refusal", err)
	}
	atLimit := []byte(`{"a":` + strings.Repeat("[", 254) + strings.Repeat("]", 254) + `}`)
	if _, _, err := authority.CanonicalEffectArguments(atLimit); err != nil &&
		strings.Contains(err.Error(), "exceed JSON depth") {
		t.Fatalf("arguments inside the depth bound were refused for depth: %v", err)
	}

	// Depth has to fall again on every closer. Three hundred sibling objects
	// are two deep, not three hundred deep, and a counter that only ever rose
	// would refuse this ordinary payload.
	siblings := []byte(`{"a":[` + strings.TrimSuffix(strings.Repeat(`{},`, 300), ",") + `]}`)
	if _, _, err := authority.CanonicalEffectArguments(siblings); err != nil {
		t.Fatalf("three hundred sibling objects = %v, want accepted", err)
	}
}
