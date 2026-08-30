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
		{ActivationDescriptor(), "sha256:40478a099ac75c1adcdcb39f09adcf47f8ffb58929e42abd7c78e2d5641779fa"},
	} {
		identity, err := test.descriptor.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if identity.Revision != 1 || identity.Digest != test.digest {
			t.Fatalf("%s identity = %+v, want revision 1 digest %s",
				identity.Name, identity, test.digest)
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
