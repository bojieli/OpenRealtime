package interaction

import "testing"

func TestControlSerializationQuarantineIdentityTracksSafeEnvelopeContract(t *testing.T) {
	descriptor := ControlSerializationQuarantineDescriptor()
	if descriptor.Revision != 2 ||
		descriptor.StateSchema != "schema://openrealtime/interaction/control-serialization-quarantine-state/v2" ||
		descriptor.ConfigSchema != "schema://openrealtime/interaction/control-serialization-quarantine-config/v2" {
		t.Fatalf("control serialization quarantine compatibility identity = revision %d, state %q, config %q",
			descriptor.Revision, descriptor.StateSchema, descriptor.ConfigSchema)
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	const digest = "sha256:f5d773fcdfe5fe831b37e853185bea03ef86276f5b30999532d8cecb33654f62"
	if identity.Name != descriptor.Name || identity.Revision != descriptor.Revision ||
		identity.Digest != digest {
		t.Fatalf("control serialization quarantine identity = %+v, want revision 2 digest %s",
			identity, digest)
	}
}
