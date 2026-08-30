package trajectory

import (
	"encoding/json"
	"fmt"
	"testing"
)

var (
	benchmarkPrefixIdentity PrefixIdentity
	benchmarkPrefixStore    *Store
)

func BenchmarkIdentifyPrefix(b *testing.B) {
	for _, size := range []int{1, 16, 256, 4096} {
		b.Run(fmt.Sprintf("items=%d", size), func(b *testing.B) {
			snapshot := benchmarkPrefixSnapshot(size)
			b.ReportAllocs()
			b.SetBytes(benchmarkPrefixWireBytes(snapshot))
			b.ResetTimer()
			for b.Loop() {
				identity, err := IdentifyPrefix(snapshot, snapshot.Version)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkPrefixIdentity = identity
			}
		})
	}
}

// BenchmarkPrefixIdentityRepeatedAppends measures the work done when every
// committed version is identified by hashing its complete prefix again. It
// intentionally keeps snapshot construction outside the timed region so the
// result isolates prefix identity work from Store cloning and validation.
func BenchmarkPrefixIdentityRepeatedAppends(b *testing.B) {
	for _, size := range []int{16, 64, 256} {
		b.Run(fmt.Sprintf("items=%d", size), func(b *testing.B) {
			snapshot := benchmarkPrefixSnapshot(size)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var identity PrefixIdentity
				for version := uint64(1); version <= snapshot.Version; version++ {
					var err error
					identity, err = IdentifyPrefix(snapshot, version)
					if err != nil {
						b.Fatal(err)
					}
				}
				benchmarkPrefixIdentity = identity
			}
		})
	}
}

// BenchmarkPrefixIdentityIncrementalAppends measures the same sequence of
// committed versions using the chain accumulator retained by Store. Each item
// is encoded and linked once.
func BenchmarkPrefixIdentityIncrementalAppends(b *testing.B) {
	for _, size := range []int{16, 64, 256} {
		b.Run(fmt.Sprintf("items=%d", size), func(b *testing.B) {
			snapshot := benchmarkPrefixSnapshot(size)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				digest := prefixSeed()
				for index, item := range snapshot.Items {
					var err error
					digest, err = prefixItem(digest, uint64(index+1), item)
					if err != nil {
						b.Fatal(err)
					}
				}
				benchmarkPrefixIdentity = prefixIdentity(snapshot.Version, digest)
			}
		})
	}
}

// BenchmarkStoreCommittedPrefixSnapshot compares the old state-element path
// (defensive Snapshot followed by a complete IdentifyPrefix pass) with the
// cached identity returned alongside the same defensive Snapshot.
func BenchmarkStoreCommittedPrefixSnapshot(b *testing.B) {
	for _, size := range []int{1, 16, 256, 4096} {
		snapshot := benchmarkPrefixSnapshot(size)
		store := NewStore()
		if err := store.AppendBatch(snapshot.Items); err != nil {
			b.Fatalf("populate %d-item store: %v", size, err)
		}
		b.Run(fmt.Sprintf("full_rehash/items=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				current := store.Snapshot()
				identity, err := IdentifyPrefix(current, current.Version)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkPrefixIdentity = identity
			}
		})
		b.Run(fmt.Sprintf("cached_identity/items=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, identity, err := store.SnapshotWithPrefixIdentity()
				if err != nil {
					b.Fatal(err)
				}
				benchmarkPrefixIdentity = identity
			}
		})
	}
}

// BenchmarkStoreAppendPrefixTracking makes the incremental maintenance cost
// visible for Store users. Prefix tracking is lazy, so stores that do not ask
// for identities retain the inactive path.
func BenchmarkStoreAppendPrefixTracking(b *testing.B) {
	for _, size := range []int{16, 256} {
		snapshot := benchmarkPrefixSnapshot(size)
		b.Run(fmt.Sprintf("inactive/items=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				store := NewStore()
				if err := store.AppendBatch(snapshot.Items); err != nil {
					b.Fatal(err)
				}
				benchmarkPrefixStore = store
			}
		})
		b.Run(fmt.Sprintf("active/items=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				store := NewStore()
				store.prefixTracking = true
				store.prefixDigest = prefixSeed()
				if err := store.AppendBatch(snapshot.Items); err != nil {
					b.Fatal(err)
				}
				benchmarkPrefixStore = store
			}
		})
	}
}

// BenchmarkStoreRepeatedCommittedPrefixes models the TrajectoryStore element:
// each single-item transaction is followed by a defensive state snapshot and
// a prefix identity. The full-rehash case is the pre-optimization behavior;
// cached is the incremental store path used by the element now.
func BenchmarkStoreRepeatedCommittedPrefixes(b *testing.B) {
	for _, size := range []int{16, 64, 256} {
		snapshot := benchmarkPrefixSnapshot(size)
		b.Run(fmt.Sprintf("full_rehash/items=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				store := NewStore()
				for _, item := range snapshot.Items {
					if err := store.Append(item); err != nil {
						b.Fatal(err)
					}
					current := store.Snapshot()
					identity, err := IdentifyPrefix(current, current.Version)
					if err != nil {
						b.Fatal(err)
					}
					benchmarkPrefixIdentity = identity
				}
				benchmarkPrefixStore = store
			}
		})
		b.Run(fmt.Sprintf("cached/items=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				store := NewStore()
				if _, _, err := store.SnapshotWithPrefixIdentity(); err != nil {
					b.Fatal(err)
				}
				for _, item := range snapshot.Items {
					if err := store.Append(item); err != nil {
						b.Fatal(err)
					}
					_, identity, err := store.SnapshotWithPrefixIdentity()
					if err != nil {
						b.Fatal(err)
					}
					benchmarkPrefixIdentity = identity
				}
				benchmarkPrefixStore = store
			}
		})
	}
}

func benchmarkPrefixSnapshot(size int) Snapshot {
	items := make([]Item, size)
	for index := range items {
		item := Item{
			ID:             fmt.Sprintf("observation-%06d", index),
			Kind:           KindObservation,
			MonotonicNS:    uint64(index + 1),
			SourceRevision: uint64(index + 1),
			Producer:       Producer{Phase: PhaseUser},
			Content:        "representative committed observation payload",
			Observation: &ObservationMeta{
				Observer: "audio", Source: "microphone", Authority: AuthorityUser,
				Media: []MediaRef{{
					Handle: fmt.Sprintf("media-%06d", index), MIMEType: "audio/pcm",
					Source: "microphone", Bytes: 640,
				}},
			},
			Event: &EventMetadata{
				EventID: fmt.Sprintf("event-%06d", index), Type: "audio.transcript",
				Source: "audio", Channel: "microphone", OccurredNS: uint64(index + 1),
			},
		}
		if index != 0 {
			item.CausalParentIDs = []string{items[index-1].ID}
		}
		items[index] = item
	}
	return Snapshot{Version: uint64(size), Items: items}
}

func benchmarkPrefixWireBytes(snapshot Snapshot) int64 {
	var total int64
	for _, item := range snapshot.Items {
		wire, err := json.Marshal(item)
		if err != nil {
			panic(err)
		}
		total += int64(len(wire))
	}
	return total
}
