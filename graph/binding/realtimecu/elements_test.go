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
		{ActivationDescriptor(), "sha256:c1c017e7da0cc435bcb9a128198fa71db29b1081e03cbda981e747d19780cb3f"},
	} {
		identity, err := test.descriptor.Identity()
		if err != nil {
			t.Fatal(err)
		}
		wantRevision := uint64(1)
		if identity.Name == ActivationReference {
			wantRevision = 8
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
