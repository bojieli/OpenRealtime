package runtime

import (
	"strings"
	"sync"
	"testing"
)

func TestServiceSetInstallIfAbsentPublishesCanonicalBatch(t *testing.T) {
	services := NewServiceSet()
	revisions, err := services.InstallIfAbsent(map[string]any{
		" deployment.codec ": "codec",
		"deployment.models":  "registry",
	})
	if err != nil {
		t.Fatalf("install service batch: %v", err)
	}
	if len(revisions) != 2 || revisions["deployment.codec"] != 1 || revisions["deployment.models"] != 1 {
		t.Fatalf("unexpected revisions: %#v", revisions)
	}
	for name, want := range map[string]any{
		"deployment.codec":  "codec",
		"deployment.models": "registry",
	} {
		got, revision, found := services.Lookup(name)
		if !found || got != want || revision != 1 {
			t.Fatalf("lookup %q = (%#v, %d, %t), want (%#v, 1, true)", name, got, revision, found, want)
		}
	}
}

func TestServiceSetInstallIfAbsentRejectsInvalidBatchWithoutWrites(t *testing.T) {
	var typedNil *struct{}
	tests := []struct {
		name    string
		entries map[string]any
		wantErr string
	}{
		{name: "empty", entries: map[string]any{}, wantErr: "at least one"},
		{name: "blank name", entries: map[string]any{"  ": "value"}, wantErr: "canonical names"},
		{name: "nil value", entries: map[string]any{"service": nil}, wantErr: "non-nil values"},
		{name: "typed nil value", entries: map[string]any{"service": typedNil}, wantErr: "non-nil values"},
		{
			name: "duplicate canonical name",
			entries: map[string]any{
				"service":   "first",
				" service ": "second",
			},
			wantErr: "duplicate canonical name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			services := NewServiceSet()
			if _, err := services.InstallIfAbsent(test.entries); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("InstallIfAbsent() error = %v, want substring %q", err, test.wantErr)
			}
			if len(services.services) != 0 {
				t.Fatalf("invalid batch published services: %#v", services.services)
			}
		})
	}

	var nilServices *ServiceSet
	if _, err := nilServices.InstallIfAbsent(map[string]any{"service": "value"}); err == nil {
		t.Fatal("nil ServiceSet accepted batch installation")
	}
	services := NewServiceSet()
	if _, err := services.Set("service", typedNil); err == nil || !strings.Contains(err.Error(), "non-nil") {
		t.Fatalf("Set accepted typed nil service: %v", err)
	}
}

func TestServiceSetInstallIfAbsentCollisionIsAllOrNothing(t *testing.T) {
	for _, existingName := range []string{"deployment.codec", "deployment.models"} {
		t.Run(existingName, func(t *testing.T) {
			services := NewServiceSet()
			if _, err := services.Set(existingName, "existing"); err != nil {
				t.Fatalf("set existing service: %v", err)
			}
			if _, err := services.InstallIfAbsent(map[string]any{
				"deployment.codec":  "new-codec",
				"deployment.models": "new-registry",
			}); err == nil {
				t.Fatal("InstallIfAbsent() accepted a colliding batch")
			}
			got, revision, found := services.Lookup(existingName)
			if !found || got != "existing" || revision != 1 {
				t.Fatalf("existing service changed: (%#v, %d, %t)", got, revision, found)
			}
			otherName := "deployment.codec"
			if existingName == otherName {
				otherName = "deployment.models"
			}
			if _, _, found := services.Lookup(otherName); found {
				t.Fatalf("non-colliding batch member %q was partially published", otherName)
			}
		})
	}
}

func TestServiceSetInstallIfAbsentConcurrentInstallersHaveOneCompleteWinner(t *testing.T) {
	services := NewServiceSet()
	type result struct {
		owner string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, owner := range []string{"first", "second"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := services.InstallIfAbsent(map[string]any{
				"deployment.codec":  owner + "-codec",
				"deployment.models": owner + "-registry",
			})
			results <- result{owner: owner, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	winner := ""
	failed := 0
	for result := range results {
		if result.err == nil {
			if winner != "" {
				t.Fatalf("multiple installers succeeded: %q and %q", winner, result.owner)
			}
			winner = result.owner
		} else {
			failed++
		}
	}
	if winner == "" || failed != 1 {
		t.Fatalf("winner = %q, failed = %d; want one winner and one failure", winner, failed)
	}
	codec, _, codecFound := services.Lookup("deployment.codec")
	registry, _, registryFound := services.Lookup("deployment.models")
	if !codecFound || !registryFound || codec != winner+"-codec" || registry != winner+"-registry" {
		t.Fatalf("mixed batch: codec=%#v registry=%#v winner=%q", codec, registry, winner)
	}
}

func TestServiceSetInstallIfAbsentLinearizesAgainstSet(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		services := NewServiceSet()
		start := make(chan struct{})
		installResult := make(chan error, 1)
		setResult := make(chan error, 1)
		go func() {
			<-start
			_, err := services.InstallIfAbsent(map[string]any{
				"deployment.codec":  "batch-codec",
				"deployment.models": "batch-registry",
			})
			installResult <- err
		}()
		go func() {
			<-start
			_, err := services.Set("deployment.codec", "replacement")
			setResult <- err
		}()
		close(start)
		installErr := <-installResult
		if err := <-setResult; err != nil {
			t.Fatalf("iteration %d: Set() error = %v", iteration, err)
		}

		codec, codecRevision, codecFound := services.Lookup("deployment.codec")
		registry, registryRevision, registryFound := services.Lookup("deployment.models")
		if !codecFound || codec != "replacement" {
			t.Fatalf("iteration %d: replacement missing: (%#v, %d, %t)", iteration, codec, codecRevision, codecFound)
		}
		if installErr == nil {
			if !registryFound || registry != "batch-registry" || registryRevision != 1 || codecRevision != 2 {
				t.Fatalf("iteration %d: successful batch was not complete before replacement: codec=(%#v,%d) registry=(%#v,%d,%t)", iteration, codec, codecRevision, registry, registryRevision, registryFound)
			}
		} else if registryFound || codecRevision != 1 {
			t.Fatalf("iteration %d: failed batch partially published: codec revision=%d registry=(%#v,%d,%t), error=%v", iteration, codecRevision, registry, registryRevision, registryFound, installErr)
		}
	}
}
