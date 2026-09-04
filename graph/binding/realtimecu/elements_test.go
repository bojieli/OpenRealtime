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
		{CancellationCoordinatorDescriptor(), "sha256:fc227d6bac1d353f099dc7555ff52ae87ad099360875f421b5a8a9f9eb9eb648"},
	} {
		identity, err := test.descriptor.Identity()
		if err != nil {
			t.Fatal(err)
		}
		wantRevision := uint64(1)
		if identity.Name == ActivationReference {
			wantRevision = 12
		} else if identity.Name == CancellationCoordinatorReference {
			wantRevision = 3
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
