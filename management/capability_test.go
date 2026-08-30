package management

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCapabilityRegistryScopesExpiryAndRevocation(t *testing.T) {
	registry := NewCapabilityRegistry()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return now }
	access, err := registry.Issue(time.Minute, []Grant{
		{Operation: ReadTrace, Resource: "sess-a"},
		{Operation: ReadSession, Resource: "sess-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if access.Token == "" || !access.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("incomplete access: %+v", access)
	}
	authorize := func(operation Operation, resource string) error {
		return registry.Authorize(context.Background(), AuthorizationRequest{
			Capability: access.Token, Operation: operation, Resource: resource,
		})
	}
	if err := authorize(ReadSession, "sess-a"); err != nil {
		t.Fatalf("exact grant refused: %v", err)
	}
	for _, attempt := range []struct {
		operation Operation
		resource  string
	}{
		{ReadSession, "sess-b"},
		{ApplyCandidate, "sess-a"},
	} {
		if err := authorize(attempt.operation, attempt.resource); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("over-broad authorization %s/%s = %v", attempt.operation, attempt.resource, err)
		}
	}
	registry.Revoke(access.Token)
	if err := authorize(ReadSession, "sess-a"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked bearer remained live: %v", err)
	}

	expiring, err := registry.Issue(time.Second, []Grant{{
		Operation: ReadGraph, Resource: "*",
	}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := registry.Authorize(context.Background(), AuthorizationRequest{
		Capability: expiring.Token, Operation: ReadGraph, Resource: "sha256:any",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("capability remained valid at expiry: %v", err)
	}
}

func TestCapabilityRegistryRejectsInvalidAndDuplicateGrants(t *testing.T) {
	registry := NewCapabilityRegistry()
	for _, grants := range [][]Grant{
		nil,
		{{Operation: "invented", Resource: "x"}},
		{{Operation: ReadGraph, Resource: ""}},
		{
			{Operation: ReadGraph, Resource: "x"},
			{Operation: ReadGraph, Resource: "x"},
		},
	} {
		if _, err := registry.Issue(time.Minute, grants); err == nil {
			t.Fatalf("invalid grants were accepted: %+v", grants)
		}
	}
	if _, err := registry.Issue(25*time.Hour, []Grant{{
		Operation: ReadGraph, Resource: "*",
	}}); err == nil {
		t.Fatal("unbounded capability TTL was accepted")
	}
}

func TestCapabilityRegistryScopedRevokerIsExactAndIdempotent(t *testing.T) {
	registry := NewCapabilityRegistry()
	first, revokeFirst, err := registry.IssueScoped(time.Minute, []Grant{{
		Operation: ReadSession, Resource: "sess-owner-a",
	}})
	if err != nil {
		t.Fatal(err)
	}
	second, revokeSecond, err := registry.IssueScoped(time.Minute, []Grant{{
		Operation: ReadSession, Resource: "sess-owner-b",
	}})
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(access Access, resource string) error {
		return registry.Authorize(context.Background(), AuthorizationRequest{
			Capability: access.Token, Operation: ReadSession, Resource: resource,
		})
	}
	if err := authorize(first, "sess-owner-a"); err != nil {
		t.Fatalf("first capability unavailable before revocation: %v", err)
	}
	if err := authorize(second, "sess-owner-b"); err != nil {
		t.Fatalf("second capability unavailable before revocation: %v", err)
	}

	revokeFirst()
	revokeFirst()
	if err := authorize(first, "sess-owner-a"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("owner revoker left its capability active: %v", err)
	}
	if err := authorize(second, "sess-owner-b"); err != nil {
		t.Fatalf("one owner revoker affected another capability: %v", err)
	}
	revokeSecond()
}
