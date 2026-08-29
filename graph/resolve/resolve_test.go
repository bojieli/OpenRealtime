package resolve_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/resolve"
)

func TestUpdateAndLockedResolutionAreDeterministic(t *testing.T) {
	catalog := resolve.NewCatalog()
	for _, revision := range []uint64{1, 2} {
		if err := catalog.Register(descriptor("test.Source", revision)); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.Register(descriptor("test.Sink", 1)); err != nil {
		t.Fatal(err)
	}
	updated, err := resolve.Resolve(catalog, []string{"test.Source", "test.Sink", "test.Source"}, resolve.Lock{}, resolve.Update)
	if err != nil {
		t.Fatal(err)
	}
	identity, found := updated.Lock.Lookup("test.Source")
	if !found || identity.Revision != 2 {
		t.Fatalf("source lock = %+v, found %v", identity, found)
	}
	first, err := updated.Lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := resolve.ParseLock(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parsed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("lock is not deterministic:\n%s\n%s", first, second)
	}
	locked, err := resolve.Resolve(catalog, []string{"test.Sink", "test.Source"}, parsed, resolve.Locked)
	if err != nil {
		t.Fatal(err)
	}
	if got := locked.Descriptors["test.Source"].Revision; got != 2 {
		t.Fatalf("resolved revision = %d, want 2", got)
	}
}

func TestLockedResolutionRejectsMissingAndStaleEntries(t *testing.T) {
	catalog := resolve.NewCatalog()
	initial := descriptor("test.Source", 1)
	if err := catalog.Register(initial); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve.Resolve(catalog, []string{"test.Source"}, resolve.NewLock(), resolve.Locked); err == nil ||
		!strings.Contains(err.Error(), "absent") {
		t.Fatalf("missing lock error = %v", err)
	}

	identity, err := initial.Identity()
	if err != nil {
		t.Fatal(err)
	}
	identity.Digest = "sha256:" + strings.Repeat("0", 64)
	lock := resolve.NewLock()
	lock.Entries = []resolve.Entry{{Reference: initial.Name, Identity: identity}}
	if _, err := resolve.Resolve(catalog, []string{initial.Name}, lock, resolve.Locked); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale lock error = %v", err)
	}
}

func TestDescriptorRevisionCannotChangeContent(t *testing.T) {
	catalog := resolve.NewCatalog()
	left := descriptor("test.Source", 1)
	right := left.Clone()
	right.Ports[0].DefaultDepth = 99
	if err := catalog.Register(left); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(right); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("registration error = %v", err)
	}
}

func descriptor(name string, revision uint64) element.Descriptor {
	direction := element.Output
	portName := "out"
	if strings.HasSuffix(name, "Sink") {
		direction = element.Input
		portName = "in"
	}
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          name,
		Revision:      revision,
		Ports: []element.Port{{
			Name: portName, Direction: direction, Cardinality: element.One,
			Type: element.Event(element.Named("test.Value")), DefaultDepth: 4,
		}},
	}
}
