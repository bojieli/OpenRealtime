package session_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestMediaStoreRetainsAndResolvesByHandle(t *testing.T) {
	store, err := session.NewMediaStore(session.MediaConfig{Scheduler: clock.NewManual(0)})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	reference, err := store.Retain(trajectory.MediaRef{MIMEType: "image/jpeg", Source: "screen"}, []byte("frame"))
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if reference.Handle == "" || reference.Bytes != 5 {
		t.Fatalf("unexpected reference %+v", reference)
	}
	media, err := store.Resolve(reference.Handle)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(media.Bytes) != "frame" {
		t.Fatalf("unexpected media %q", media.Bytes)
	}
	media.Bytes[0] = 'X'
	again, _ := store.Resolve(reference.Handle)
	if string(again.Bytes) != "frame" {
		t.Fatal("resolved media must be a copy")
	}
}

func TestMediaRetentionIsBoundedByCount(t *testing.T) {
	store, err := session.NewMediaStore(session.MediaConfig{MaxItems: 2, Scheduler: clock.NewManual(0)})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	var handles []string
	for index := 0; index < 4; index++ {
		reference, err := store.Retain(trajectory.MediaRef{MIMEType: "image/jpeg"}, []byte{byte(index)})
		if err != nil {
			t.Fatalf("retain %d: %v", index, err)
		}
		handles = append(handles, reference.Handle)
	}
	if _, err := store.Resolve(handles[0]); !errors.Is(err, session.ErrMediaExpired) {
		t.Fatalf("expected the oldest handle to be released, got %v", err)
	}
	if _, err := store.Resolve(handles[3]); err != nil {
		t.Fatalf("newest handle must still resolve: %v", err)
	}
	if metrics := store.Metrics(); metrics.LiveItems != 2 || metrics.Evicted != 2 {
		t.Fatalf("unexpected metrics %+v", metrics)
	}
}

func TestMediaRetentionIsBoundedByWindow(t *testing.T) {
	scheduler := clock.NewManual(0)
	store, err := session.NewMediaStore(session.MediaConfig{Window: time.Second, Scheduler: scheduler})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	reference, err := store.Retain(trajectory.MediaRef{MIMEType: "image/jpeg"}, []byte("f"))
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	scheduler.AdvanceNS(uint64(999 * time.Millisecond))
	if _, err := store.Resolve(reference.Handle); err != nil {
		t.Fatalf("handle expired early: %v", err)
	}
	scheduler.AdvanceNS(uint64(2 * time.Millisecond))
	if _, err := store.Resolve(reference.Handle); !errors.Is(err, session.ErrMediaExpired) {
		t.Fatalf("expected window expiry, got %v", err)
	}
}

func TestMediaRetentionIsBoundedByBytes(t *testing.T) {
	store, err := session.NewMediaStore(session.MediaConfig{MaxBytes: 8, Scheduler: clock.NewManual(0)})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.Retain(trajectory.MediaRef{MIMEType: "image/jpeg"}, make([]byte, 9)); err == nil {
		t.Fatal("expected an oversized attachment to be rejected outright")
	}
	first, _ := store.Retain(trajectory.MediaRef{MIMEType: "image/jpeg"}, make([]byte, 5))
	if _, err := store.Retain(trajectory.MediaRef{MIMEType: "image/jpeg"}, make([]byte, 5)); err != nil {
		t.Fatalf("retain second: %v", err)
	}
	if _, err := store.Resolve(first.Handle); !errors.Is(err, session.ErrMediaExpired) {
		t.Fatalf("expected byte-bound eviction, got %v", err)
	}
}

func TestDuplicateHandleIsRejected(t *testing.T) {
	store, _ := session.NewMediaStore(session.MediaConfig{Scheduler: clock.NewManual(0)})
	if _, err := store.Retain(trajectory.MediaRef{Handle: "h", MIMEType: "image/jpeg"}, []byte("a")); err != nil {
		t.Fatalf("retain: %v", err)
	}
	if _, err := store.Retain(trajectory.MediaRef{Handle: "h", MIMEType: "image/jpeg"}, []byte("b")); err == nil {
		t.Fatal("expected duplicate handle rejection")
	}
}
