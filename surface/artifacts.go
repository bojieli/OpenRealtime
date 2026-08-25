package surface

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxArtifactBytes bounds one artifact's HTML.
//
// Generous, because the interesting artifacts are dashboards with their own
// script and style inlined, and mean, because this is a model's output being
// held in a developer's memory and something has to say no.
const DefaultMaxArtifactBytes = 2 << 20

// maxArtifacts bounds how many are retained. An agent that renders a new
// artifact every turn for an hour should not grow this process without limit,
// and the ones a developer wants are the recent ones.
const maxArtifacts = 64

// Artifact is one piece of generative UI: HTML the agent wrote, for a person
// to look at.
//
// It is worth being clear about what this is not. It is not a protocol
// addition, it is not a new event, and it is not a namespace anybody has to
// implement. It is an ordinary function tool that this process declares and
// this process runs, exactly like reading a file - the difference is only that
// what comes back is rendered rather than read. Any Realtime server can drive
// it, including one that has never heard of OpenRealtime, because function
// calling is all it needs.
type Artifact struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	HTML  string `json:"-"`
	// Version increments on every update to the same id. It is what makes an
	// artifact self-updating rather than merely replaceable: the page reloads
	// the frame when the version moves, so an agent that revises a dashboard
	// mid-session revises what the person is already looking at instead of
	// stacking a second copy underneath it.
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Bytes     int       `json:"bytes"`
}

// ArtifactStore holds what the agent rendered and serves it to the frame.
type ArtifactStore struct {
	maxBytes int

	mu        sync.RWMutex
	artifacts map[string]*Artifact
	order     []string
}

// NewArtifactStore returns a store. A non-positive limit takes the default.
func NewArtifactStore(maxBytes int) *ArtifactStore {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxArtifactBytes
	}
	return &ArtifactStore{maxBytes: maxBytes, artifacts: make(map[string]*Artifact)}
}

// MaxBytes is the per-artifact limit, so the tool description can state it.
// A model told the limit writes to it; a model that discovers it by being
// refused has already wasted the turn.
func (store *ArtifactStore) MaxBytes() int { return store.maxBytes }

// Put stores or updates an artifact and returns what the caller should tell
// the model.
func (store *ArtifactStore) Put(id, title, html string) (*Artifact, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("an artifact requires an id")
	}
	if !validArtifactID(id) {
		return nil, fmt.Errorf(
			"artifact id %q must be 1-64 characters of letters, digits, dash, or underscore", id)
	}
	if strings.TrimSpace(html) == "" {
		return nil, errors.New("an artifact requires html")
	}
	if len(html) > store.maxBytes {
		return nil, fmt.Errorf("this artifact is %d bytes and the limit is %d; render less at once",
			len(html), store.maxBytes)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	existing, updating := store.artifacts[id]
	if !updating {
		existing = &Artifact{ID: id}
		store.artifacts[id] = existing
		store.order = append(store.order, id)
		store.evictLocked()
	}
	existing.Title = strings.TrimSpace(title)
	if existing.Title == "" {
		existing.Title = id
	}
	existing.HTML = html
	existing.Bytes = len(html)
	existing.Version++
	existing.UpdatedAt = time.Now()
	snapshot := *existing
	return &snapshot, nil
}

// Get returns one artifact.
func (store *ArtifactStore) Get(id string) (*Artifact, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	artifact, exists := store.artifacts[id]
	if !exists {
		return nil, false
	}
	snapshot := *artifact
	return &snapshot, true
}

// List returns the artifacts, oldest first.
func (store *ArtifactStore) List() []Artifact {
	store.mu.RLock()
	defer store.mu.RUnlock()
	listed := make([]Artifact, 0, len(store.order))
	for _, id := range store.order {
		if artifact, exists := store.artifacts[id]; exists {
			listed = append(listed, *artifact)
		}
	}
	return listed
}

func (store *ArtifactStore) evictLocked() {
	for len(store.order) > maxArtifacts {
		oldest := store.order[0]
		store.order = store.order[1:]
		delete(store.artifacts, oldest)
	}
}

func validArtifactID(id string) bool {
	if len(id) > 64 {
		return false
	}
	for _, character := range id {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// ServeHTTP serves one artifact into the frame that displays it.
//
// The response carries its own content security policy, and that is the whole
// reason artifacts are a route rather than a srcdoc or a blob. A frame loaded
// from srcdoc inherits the embedding document's policy, so model-authored
// inline script would either not run or would force this application's own
// policy open far enough that it did. Here the artifact gets a policy written
// for an artifact: its own inline script and style run, and it can reach
// nothing. default-src 'none' with no connect-src means no fetch, no
// XMLHttpRequest, no WebSocket, and no image from anywhere but the document
// itself - so HTML a model wrote cannot send what it was shown to anybody.
//
// The frame element supplies the other half by sandboxing without
// allow-same-origin, which withholds the origin: the artifact cannot read this
// page's DOM, cookies, or storage. What it can still do is postMessage to its
// parent, which is deliberate and is how a person clicking a button inside an
// artifact reaches the session - as an ordinary user message, on the channel
// user messages already use.
func (store *ArtifactStore) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	artifact, exists := store.Get(request.PathValue("id"))
	if !exists {
		http.Error(writer, "no such artifact", http.StatusNotFound)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"img-src data: blob:; font-src data:; form-action 'none'; base-uri 'none'; "+
			"frame-ancestors 'self'")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Artifact-Version", strconv.Itoa(artifact.Version))
	_, _ = writer.Write([]byte(artifact.HTML))
}
