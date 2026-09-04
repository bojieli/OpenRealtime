package realtimecu

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/element/codec"
)

func TestRealtimeCUElementDescriptorIdentitiesAreStable(t *testing.T) {
	for _, test := range []struct {
		descriptor element.Descriptor
		digest     string
	}{
		{ObservationCommitDescriptor(), "sha256:3f6a2ed8c53bf3e8ef8e4e645b25691332f3b6c6f93283a839b4bb03088cbd28"},
		{ActivationDescriptor(), "sha256:c88c12b977dfc0c44bcda8317002fabe72f2d32a38c61418a81a092d053a9dce"},
		{CancellationCoordinatorDescriptor(), "sha256:ed399beac0fddb56f30490311e86f1f46011392db9bc3aa2a85db033562dfb0a"},
	} {
		identity, err := test.descriptor.Identity()
		if err != nil {
			t.Fatal(err)
		}
		wantRevision := uint64(1)
		if identity.Name == ActivationReference {
			wantRevision = 12
		} else if identity.Name == CancellationCoordinatorReference {
			wantRevision = 2
		}
		if identity.Revision != wantRevision || identity.Digest != test.digest {
			t.Fatalf("%s identity = %+v, want revision %d digest %s",
				identity.Name, identity, wantRevision, test.digest)
		}
	}
}

func TestRealtimeCUElementDescriptorBundleMatchesImplementations(t *testing.T) {
	want, err := codec.MarshalJSON(codec.New(
		ObservationCommitDescriptor(), ActivationDescriptor(),
		CancellationCoordinatorDescriptor(),
	))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "..", "graphs", "components",
		"realtime-computer-use", "elements.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s drifted from the registered Realtime-CU descriptors", path)
	}
}
