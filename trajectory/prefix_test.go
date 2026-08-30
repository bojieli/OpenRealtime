package trajectory_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestPrefixIdentityGoldenChainAndDefensivePrefix(t *testing.T) {
	snapshot := prefixIdentityFixture()
	want := map[uint64]string{
		0: "sha256:d008652338e2b3982812adc81cea3850bbb13540161bac09d488f6f39226cdb2",
		1: "sha256:2a6ba301f9a262fdcf27307b26361230be01bd67219f3ae89016a697e4d72e2f",
		2: "sha256:570ad8aa2331482f257b65959a347eceb845e339e761a2844806619d9432bbf4",
	}
	for version := uint64(0); version <= snapshot.Version; version++ {
		identity, err := trajectory.IdentifyPrefix(snapshot, version)
		if err != nil {
			t.Fatal(err)
		}
		if identity.Version != version || identity.Digest != want[version] {
			t.Fatalf("prefix %d identity = %+v, want digest %s", version, identity, want[version])
		}
		if err := trajectory.VerifyPrefix(snapshot, identity); err != nil {
			t.Fatalf("verify prefix %d: %v", version, err)
		}
	}

	identity, err := trajectory.IdentifyPrefix(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := trajectory.Prefix(snapshot, identity)
	if err != nil {
		t.Fatal(err)
	}
	prefix.Items[1].CausalParentIDs[0] = "mutated-parent"
	prefix.Items[1].ProviderState[0] = '['
	prefix.Items[0].Observation.Media[0].Handle = "mutated-media"
	prefix.Items[0].Event.Source = "mutated-source"
	if snapshot.Items[1].CausalParentIDs[0] != "observation-1" ||
		string(snapshot.Items[1].ProviderState) != `{"z":2,"a":[true,null]}` {
		t.Fatalf("returned prefix aliases source snapshot: %+v", snapshot.Items[1])
	}
	if snapshot.Items[0].Observation.Media[0].Handle != "media-1" ||
		snapshot.Items[0].Event.Source != "audio" {
		t.Fatalf("returned prefix aliases nested observation state: %+v", snapshot.Items[0])
	}
}

func TestPrefixIdentityRejectsTamperAndMalformedInputs(t *testing.T) {
	snapshot := prefixIdentityFixture()
	identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name     string
		snapshot trajectory.Snapshot
		identity trajectory.PrefixIdentity
		want     string
	}{
		{
			name: "content tamper", snapshot: mutatePrefixSnapshot(snapshot, func(value *trajectory.Snapshot) {
				value.Items[0].Content = "changed"
			}), identity: identity, want: "digest mismatch",
		},
		{
			name: "opaque state tamper", snapshot: mutatePrefixSnapshot(snapshot, func(value *trajectory.Snapshot) {
				value.Items[1].ProviderState = json.RawMessage(`{"z":3,"a":[true,null]}`)
			}), identity: identity, want: "digest mismatch",
		},
		{
			name: "version drift", snapshot: trajectory.Snapshot{
				Version: snapshot.Version + 1, Items: snapshot.Items,
			}, identity: identity, want: "does not equal item count",
		},
		{
			name: "identity exceeds snapshot", snapshot: snapshot,
			identity: trajectory.PrefixIdentity{Version: 3, Digest: identity.Digest},
			want:     "exceeds snapshot version",
		},
		{
			name: "missing digest", snapshot: snapshot,
			identity: trajectory.PrefixIdentity{Version: 2}, want: "canonical SHA-256",
		},
		{
			name: "uppercase digest", snapshot: snapshot,
			identity: trajectory.PrefixIdentity{Version: 2, Digest: strings.ToUpper(identity.Digest)},
			want:     "canonical SHA-256",
		},
		{
			name: "nonhex digest", snapshot: snapshot,
			identity: trajectory.PrefixIdentity{Version: 2, Digest: "sha256:" + strings.Repeat("z", 64)},
			want:     "canonical SHA-256",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := trajectory.VerifyPrefix(testCase.snapshot, testCase.identity); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("VerifyPrefix error = %v, want %q", err, testCase.want)
			}
			if _, err := trajectory.Prefix(testCase.snapshot, testCase.identity); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Prefix error = %v, want %q", err, testCase.want)
			}
		})
	}

	malformed := snapshot
	malformed.Items = append([]trajectory.Item(nil), snapshot.Items...)
	malformed.Items[1].ProviderState = json.RawMessage(`{"unterminated":`)
	if _, err := trajectory.IdentifyPrefix(malformed, 2); err == nil ||
		!strings.Contains(err.Error(), "encode trajectory item 1") {
		t.Fatalf("malformed item error = %v", err)
	}
	if _, err := trajectory.IdentifyPrefix(snapshot, 3); err == nil ||
		!strings.Contains(err.Error(), "exceeds snapshot version") {
		t.Fatalf("excess prefix error = %v", err)
	}
}

func TestStoreSnapshotPrefixIdentityMatchesGoldenAlgorithm(t *testing.T) {
	store := trajectory.NewStore()
	assertIdentity := func(stage string) trajectory.PrefixIdentity {
		t.Helper()
		snapshot, identity, err := store.SnapshotWithPrefixIdentity()
		if err != nil {
			t.Fatalf("%s: snapshot with prefix identity: %v", stage, err)
		}
		want, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
		if err != nil {
			t.Fatalf("%s: identify returned snapshot: %v", stage, err)
		}
		if identity != want {
			t.Fatalf("%s: cached identity = %+v, want %+v", stage, identity, want)
		}
		if err := trajectory.VerifyPrefix(snapshot, identity); err != nil {
			t.Fatalf("%s: cached identity does not verify: %v", stage, err)
		}
		if len(snapshot.Items) != 0 {
			snapshot.Items[0].Content = "mutated defensive snapshot"
			fresh, freshIdentity, err := store.SnapshotWithPrefixIdentity()
			if err != nil {
				t.Fatalf("%s: fresh snapshot with prefix identity: %v", stage, err)
			}
			if fresh.Items[0].Content == snapshot.Items[0].Content || freshIdentity != identity {
				t.Fatalf("%s: returned snapshot aliases store or changed cached identity", stage)
			}
		}
		return identity
	}

	assertIdentity("empty")
	if err := store.Append(trajectory.Item{
		ID: "observation-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	assertIdentity("single append")
	if err := store.AppendBatch([]trajectory.Item{
		{
			ID: "assistant-1", Kind: trajectory.KindAssistant, MonotonicNS: 2,
			CausalParentIDs: []string{"observation-1"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "hi",
		},
		{
			ID: "observation-2", Kind: trajectory.KindObservation, MonotonicNS: 3,
			CausalParentIDs: []string{"assistant-1"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "next",
		},
	}); err != nil {
		t.Fatal(err)
	}
	beforeRejected := assertIdentity("batch append with normalized assistant visibility")
	if err := store.Append(trajectory.Item{
		ID: "observation-2", Kind: trajectory.KindObservation, MonotonicNS: 4,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "duplicate",
	}); err == nil {
		t.Fatal("duplicate append unexpectedly succeeded")
	}
	if afterRejected := assertIdentity("rejected append"); afterRejected != beforeRejected {
		t.Fatalf("rejected transaction changed cached identity: before=%+v after=%+v",
			beforeRejected, afterRejected)
	}

	lateStore := trajectory.NewStore()
	if err := lateStore.AppendBatch([]trajectory.Item{
		{
			ID: "late-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "one",
		},
		{
			ID: "late-2", Kind: trajectory.KindObservation, MonotonicNS: 2,
			CausalParentIDs: []string{"late-1"},
			Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "two",
		},
	}); err != nil {
		t.Fatal(err)
	}
	lateSnapshot, lateIdentity, err := lateStore.SnapshotWithPrefixIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lateWant, err := trajectory.IdentifyPrefix(lateSnapshot, lateSnapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	if lateIdentity != lateWant {
		t.Fatalf("identity enabled after population = %+v, want %+v", lateIdentity, lateWant)
	}
}

func prefixIdentityFixture() trajectory.Snapshot {
	return trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
		{
			ID: "observation-1", Kind: trajectory.KindObservation, MonotonicNS: 10,
			SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
			Observation: &trajectory.ObservationMeta{
				Observer: "audio", Source: "microphone", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{
					Handle: "media-1", MIMEType: "audio/pcm", Source: "microphone", Bytes: 640,
				}},
			},
			Event: &trajectory.EventMetadata{
				EventID: "event-1", Type: "audio.transcript", Source: "audio",
				Channel: "microphone", OccurredNS: 9,
			},
		},
		{
			ID: "reasoning-1", Kind: trajectory.KindReasoning, MonotonicNS: 20,
			CausalParentIDs: []string{"observation-1"}, SourceRevision: 7,
			InvocationID: "run-1", Producer: trajectory.Producer{
				Phase: trajectory.PhaseSlow, Provider: "provider", Model: "model",
				ReasoningEffort: "high", SpeechAuthority: "silent",
			},
			Content: "prepared reasoning", ProviderStateType: "provider.state/v1",
			ProviderState: json.RawMessage(`{"z":2,"a":[true,null]}`),
		},
	}}
}

func mutatePrefixSnapshot(
	source trajectory.Snapshot, mutate func(*trajectory.Snapshot),
) trajectory.Snapshot {
	identity, err := trajectory.IdentifyPrefix(source, source.Version)
	if err != nil {
		panic(err)
	}
	result, err := trajectory.Prefix(source, identity)
	if err != nil {
		panic(err)
	}
	mutate(&result)
	return result
}
