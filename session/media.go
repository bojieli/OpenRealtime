package session

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// ErrMediaExpired means a handle named media that retention has released. It
// is an ordinary outcome rather than a failure: the persistent text an
// observer committed is what survives, and images are an optional attachment
// with a bounded window.
var ErrMediaExpired = errors.New("media handle is no longer retained")

// Media is one retained frame or attachment.
type Media struct {
	Ref   trajectory.MediaRef
	Bytes []byte
}

// MediaConfig bounds retention. Every bound is enforced; the first one that
// binds is the one that evicts.
type MediaConfig struct {
	// MaxItems bounds how many attachments are retained at once.
	MaxItems int
	// MaxBytes bounds total retained size.
	MaxBytes int
	// Window bounds age. Zero means age is not a bound.
	Window    time.Duration
	Scheduler clock.Scheduler
}

type mediaEntry struct {
	media      Media
	retainedNS uint64
}

// MediaStore holds attachment bytes outside the trajectory.
//
// The trajectory is copied for every continuation request, so it carries
// handles and never bytes. This is where the bytes live, under bounds a
// deployment sets, and a handle that has aged out simply stops resolving.
type MediaStore struct {
	mu        sync.Mutex
	config    MediaConfig
	scheduler clock.Scheduler
	order     []string
	entries   map[string]mediaEntry
	bytes     int
	sequence  atomic.Uint64

	retained atomic.Uint64
	evicted  atomic.Uint64
	misses   atomic.Uint64
}

// NewMediaStore creates a bounded attachment store.
func NewMediaStore(config MediaConfig) (*MediaStore, error) {
	if config.MaxItems < 0 || config.MaxBytes < 0 || config.Window < 0 {
		return nil, errors.New("media retention bounds cannot be negative")
	}
	if config.MaxItems == 0 {
		config.MaxItems = 32
	}
	if config.MaxBytes == 0 {
		config.MaxBytes = 64 << 20
	}
	scheduler := config.Scheduler
	if scheduler == nil {
		scheduler = clock.NewSystem()
	}
	return &MediaStore{
		config: config, scheduler: scheduler, entries: make(map[string]mediaEntry),
	}, nil
}

// Retain stores bytes and returns the handle that names them. The caller keeps
// the returned reference and puts it on a trajectory item; it must not keep
// the bytes.
func (store *MediaStore) Retain(reference trajectory.MediaRef, payload []byte) (trajectory.MediaRef, error) {
	if len(payload) == 0 {
		return trajectory.MediaRef{}, errors.New("retained media must be non-empty")
	}
	if reference.MIMEType == "" {
		return trajectory.MediaRef{}, errors.New("retained media requires a MIME type")
	}
	if store.config.MaxBytes > 0 && len(payload) > store.config.MaxBytes {
		return trajectory.MediaRef{}, fmt.Errorf("media of %d bytes exceeds the %d byte retention bound", len(payload), store.config.MaxBytes)
	}
	if reference.Handle == "" {
		reference.Handle = "media_" + strconv.FormatUint(store.sequence.Add(1), 10)
	}
	reference.Bytes = len(payload)
	if reference.CapturedNS == 0 {
		reference.CapturedNS = store.scheduler.NowNS()
	}
	entry := mediaEntry{
		media:      Media{Ref: reference, Bytes: slices.Clone(payload)},
		retainedNS: store.scheduler.NowNS(),
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.entries[reference.Handle]; exists {
		return trajectory.MediaRef{}, fmt.Errorf("duplicate media handle %q", reference.Handle)
	}
	store.entries[reference.Handle] = entry
	store.order = append(store.order, reference.Handle)
	store.bytes += len(payload)
	store.retained.Add(1)
	store.evictLocked()
	return reference, nil
}

// Resolve returns retained media, or ErrMediaExpired once retention released
// it. A provider adapter that cannot use media never calls this at all.
func (store *MediaStore) Resolve(handle string) (Media, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.expireLocked()
	entry, exists := store.entries[handle]
	if !exists {
		store.misses.Add(1)
		return Media{}, fmt.Errorf("%w: %s", ErrMediaExpired, handle)
	}
	return Media{Ref: entry.media.Ref, Bytes: slices.Clone(entry.media.Bytes)}, nil
}

// Release drops one handle early, for a caller that knows it is finished.
func (store *MediaStore) Release(handle string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.removeLocked(handle)
}

// MediaMetrics is operational telemetry with no media content in it.
type MediaMetrics struct {
	Retained     uint64 `json:"retained"`
	Evicted      uint64 `json:"evicted"`
	Misses       uint64 `json:"misses"`
	LiveItems    int    `json:"live_items"`
	LiveBytes    int    `json:"live_bytes"`
	MaxItems     int    `json:"max_items"`
	MaxBytes     int    `json:"max_bytes"`
	WindowMillis int64  `json:"window_ms"`
}

func (store *MediaStore) Metrics() MediaMetrics {
	store.mu.Lock()
	items, bytes := len(store.order), store.bytes
	store.mu.Unlock()
	return MediaMetrics{
		Retained: store.retained.Load(), Evicted: store.evicted.Load(), Misses: store.misses.Load(),
		LiveItems: items, LiveBytes: bytes,
		MaxItems: store.config.MaxItems, MaxBytes: store.config.MaxBytes,
		WindowMillis: store.config.Window.Milliseconds(),
	}
}

func (store *MediaStore) evictLocked() {
	store.expireLocked()
	for len(store.order) > store.config.MaxItems || (store.config.MaxBytes > 0 && store.bytes > store.config.MaxBytes) {
		if len(store.order) == 0 {
			return
		}
		store.removeLocked(store.order[0])
	}
}

func (store *MediaStore) expireLocked() {
	if store.config.Window <= 0 {
		return
	}
	now := store.scheduler.NowNS()
	window := uint64(store.config.Window.Nanoseconds())
	for len(store.order) > 0 {
		entry, exists := store.entries[store.order[0]]
		if !exists {
			store.order = store.order[1:]
			continue
		}
		if now < entry.retainedNS+window {
			return
		}
		store.removeLocked(store.order[0])
	}
}

func (store *MediaStore) removeLocked(handle string) {
	entry, exists := store.entries[handle]
	if !exists {
		return
	}
	delete(store.entries, handle)
	store.bytes -= len(entry.media.Bytes)
	if index := slices.Index(store.order, handle); index >= 0 {
		store.order = slices.Delete(store.order, index, index+1)
	}
	store.evicted.Add(1)
}
