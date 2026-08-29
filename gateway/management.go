package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// InspectionTokenHeader carries the session-scoped management capability.
// Keeping it out of the URL prevents ordinary access logs, browser history,
// and referrer headers from retaining the secret.
const InspectionTokenHeader = "OpenRealtime-Inspection-Token"

const (
	defaultInspectionTokenTTL = time.Hour
	inspectionTokenBytes      = 32
	inspectionTokenPrefix     = "ins_"
)

var (
	errInspectionNotFound    = errors.New("live inspection capability not found")
	errInspectionUnavailable = errors.New("runtime has no complete live inspection evidence")
)

// liveInspectionSource is deliberately optional. A legacy runtime that only
// exposes Status cannot be promoted into graph evidence by the gateway.
type liveInspectionSource interface {
	Live() inspect.Live
}

type inspectionNodeIdentity struct {
	element        element.Identity
	implementation string
}

type inspectionMountIdentity struct {
	graphID       string
	graphRevision uint64
	fingerprint   string
	configuration inspect.ArtifactIdentity
	nodes         map[string]inspectionNodeIdentity
}

type inspectionEntry struct {
	id        uint64
	sessionID string
	expires   time.Time
	source    liveInspectionSource
	mount     inspectionMountIdentity
}

type inspectionLease struct {
	key       [sha256.Size]byte
	sessionID string
}

func (lease inspectionLease) empty() bool { return lease.sessionID == "" }

// inspectionRegistry holds no conversation payload and never stores bearer
// tokens in plaintext: the random capability is returned once to its session,
// while only its SHA-256 lookup key remains server-side.
type inspectionRegistry struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]inspectionEntry
	ttl     time.Duration
	now     func() time.Time
	next    atomic.Uint64
}

func newInspectionRegistry(ttl time.Duration) *inspectionRegistry {
	if ttl == 0 {
		ttl = defaultInspectionTokenTTL
	}
	return &inspectionRegistry{
		entries: make(map[[sha256.Size]byte]inspectionEntry), ttl: ttl, now: time.Now,
	}
}

func (registry *inspectionRegistry) issue(
	sessionID string, source liveInspectionSource,
) (inspectionLease, openrealtime.InspectionAccess, error) {
	if registry == nil || source == nil {
		return inspectionLease{}, openrealtime.InspectionAccess{}, errInspectionUnavailable
	}
	snapshot := source.Live()
	mount, err := inspectionIdentity(snapshot)
	if err != nil {
		return inspectionLease{}, openrealtime.InspectionAccess{},
			fmt.Errorf("%w: %v", errInspectionUnavailable, err)
	}
	now := registry.now()
	for attempt := 0; attempt < 4; attempt++ {
		raw := make([]byte, inspectionTokenBytes)
		if _, err := rand.Read(raw); err != nil {
			return inspectionLease{}, openrealtime.InspectionAccess{},
				fmt.Errorf("generate live inspection capability: %w", err)
		}
		token := inspectionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
		key := sha256.Sum256([]byte(token))
		entry := inspectionEntry{
			id: registry.next.Add(1), sessionID: sessionID,
			expires: now.Add(registry.ttl), source: source, mount: mount,
		}
		registry.mu.Lock()
		registry.purgeExpiredLocked(now)
		if _, collision := registry.entries[key]; !collision {
			registry.entries[key] = entry
			registry.mu.Unlock()
			return inspectionLease{key: key, sessionID: sessionID}, openrealtime.InspectionAccess{
				SessionID: sessionID,
				Path:      "/v1/realtime/sessions/" + url.PathEscape(sessionID) + "/live",
				Token:     token, ExpiresAtMS: entry.expires.UnixMilli(),
			}, nil
		}
		registry.mu.Unlock()
	}
	return inspectionLease{}, openrealtime.InspectionAccess{},
		errors.New("generate unique live inspection capability")
}

func (registry *inspectionRegistry) revoke(lease inspectionLease) {
	if registry == nil || lease.empty() {
		return
	}
	registry.mu.Lock()
	delete(registry.entries, lease.key)
	registry.mu.Unlock()
}

func (registry *inspectionRegistry) snapshot(sessionID, token string) (inspect.Live, error) {
	key, valid := inspectionTokenKey(token)
	if registry == nil || !valid {
		return inspect.Live{}, errInspectionNotFound
	}
	now := registry.now()
	registry.mu.Lock()
	registry.purgeExpiredLocked(now)
	entry, found := registry.entries[key]
	if !found || !constantTimeTextEqual(entry.sessionID, sessionID) || !now.Before(entry.expires) {
		registry.mu.Unlock()
		return inspect.Live{}, errInspectionNotFound
	}
	registry.mu.Unlock()

	snapshot := entry.source.Live()
	identity, err := inspectionIdentity(snapshot)
	if err != nil || !sameInspectionMount(entry.mount, identity) {
		registry.revoke(inspectionLease{key: key, sessionID: sessionID})
		return inspect.Live{}, errInspectionUnavailable
	}

	// Recheck after calling the runtime. Revocation or expiry that raced the
	// snapshot must win before a response is admitted.
	now = registry.now()
	registry.mu.Lock()
	current, found := registry.entries[key]
	if !found || current.id != entry.id || !constantTimeTextEqual(current.sessionID, sessionID) ||
		!now.Before(current.expires) {
		registry.mu.Unlock()
		return inspect.Live{}, errInspectionNotFound
	}
	registry.mu.Unlock()
	return redactInspection(snapshot), nil
}

func constantTimeTextEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (registry *inspectionRegistry) purgeExpiredLocked(now time.Time) {
	for key, entry := range registry.entries {
		if !now.Before(entry.expires) {
			delete(registry.entries, key)
		}
	}
}

func inspectionTokenKey(token string) ([sha256.Size]byte, bool) {
	if !strings.HasPrefix(token, inspectionTokenPrefix) {
		return [sha256.Size]byte{}, false
	}
	encoded := strings.TrimPrefix(token, inspectionTokenPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != inspectionTokenBytes {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256([]byte(token)), true
}

func inspectionIdentity(snapshot inspect.Live) (inspectionMountIdentity, error) {
	if snapshot.FormatVersion != inspect.LiveFormatVersion {
		return inspectionMountIdentity{}, fmt.Errorf("live format is %d", snapshot.FormatVersion)
	}
	if snapshot.GraphID == "" || snapshot.GraphID != strings.TrimSpace(snapshot.GraphID) ||
		snapshot.GraphRevision == 0 || !canonicalSHA256(snapshot.Fingerprint) {
		return inspectionMountIdentity{}, errors.New("live graph identity is incomplete")
	}
	if snapshot.Configuration == nil {
		return inspectionMountIdentity{}, errors.New("live configuration identity is missing")
	}
	if err := snapshot.Configuration.Validate(); err != nil {
		return inspectionMountIdentity{}, fmt.Errorf("live configuration identity: %w", err)
	}
	if snapshot.Configuration.Digest == "" {
		return inspectionMountIdentity{}, errors.New("live configuration identity has no digest")
	}
	if len(snapshot.Nodes) == 0 {
		return inspectionMountIdentity{}, errors.New("live snapshot has no nodes")
	}
	nodes := make(map[string]inspectionNodeIdentity, len(snapshot.Nodes))
	for nodeID, node := range snapshot.Nodes {
		if nodeID == "" || nodeID != strings.TrimSpace(nodeID) || node.Resolution == nil {
			return inspectionMountIdentity{}, fmt.Errorf("live node %q has no exact resolution", nodeID)
		}
		if err := element.ValidateIdentity(node.Resolution.Element); err != nil {
			return inspectionMountIdentity{}, fmt.Errorf("live node %q element: %w", nodeID, err)
		}
		if node.Resolution.Implementation == "" ||
			node.Resolution.Implementation != strings.TrimSpace(node.Resolution.Implementation) {
			return inspectionMountIdentity{}, fmt.Errorf("live node %q implementation is missing", nodeID)
		}
		if err := node.Resolution.Runtime.Validate(); err != nil {
			return inspectionMountIdentity{}, fmt.Errorf("live node %q runtime: %w", nodeID, err)
		}
		nodes[nodeID] = inspectionNodeIdentity{
			element: node.Resolution.Element, implementation: node.Resolution.Implementation,
		}
	}
	return inspectionMountIdentity{
		graphID: snapshot.GraphID, graphRevision: snapshot.GraphRevision,
		fingerprint: snapshot.Fingerprint, configuration: *snapshot.Configuration,
		nodes: nodes,
	}, nil
}

func sameInspectionMount(left, right inspectionMountIdentity) bool {
	if left.graphID != right.graphID || left.graphRevision != right.graphRevision ||
		left.fingerprint != right.fingerprint || left.configuration != right.configuration ||
		len(left.nodes) != len(right.nodes) {
		return false
	}
	for node, identity := range left.nodes {
		if right.nodes[node] != identity {
			return false
		}
	}
	return true
}

func canonicalSHA256(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

// redactInspection removes identifiers and free-form errors that can inherit
// client payload while retaining every graph/config/runtime identity needed by
// an attestor. Non-empty errors remain non-empty so redaction cannot turn a
// failed mount into apparently healthy evidence.
func redactInspection(snapshot inspect.Live) inspect.Live {
	result := snapshot.Clone()
	if result.Error != "" {
		result.Error = "redacted"
	}
	for id, node := range result.Nodes {
		node.LastTriggerID = ""
		node.LastOutcome = ""
		if node.Error != "" {
			node.Error = "redacted"
		}
		result.Nodes[id] = node
	}
	for id, edge := range result.Edges {
		edge.LastItemID = ""
		result.Edges[id] = edge
	}
	keys := make([]string, 0, len(result.Flows))
	for key := range result.Flows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	flows := make(map[string]inspect.FlowLive, len(keys))
	for index, key := range keys {
		flow := result.Flows[key]
		identity := fmt.Sprintf("flow_%06d", index+1)
		flow.Correlation = identity
		flows[identity] = flow
	}
	result.Flows = flows
	return result
}

func (server *Server) inspectLive(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if !server.authorized(request) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	snapshot, err := server.inspections.snapshot(
		request.PathValue("session"), strings.TrimSpace(request.Header.Get(InspectionTokenHeader)),
	)
	if errors.Is(err, errInspectionNotFound) {
		// A cross-session token, a guessed token, an expired token, and an
		// unknown session are deliberately indistinguishable.
		http.NotFound(writer, request)
		return
	}
	if err != nil {
		http.Error(writer, "live inspection unavailable", http.StatusConflict)
		return
	}
	if err := json.NewEncoder(writer).Encode(snapshot); err != nil {
		server.config.Logger.Error("encode live inspection", "error", err)
	}
}

func inspectionDebugEnabled(debug *openrealtime.DebugResponse) bool {
	return debug != nil && debug.Enabled &&
		(len(debug.Categories) == 0 || containsDebugCategory(debug.Categories, openrealtime.DebugSession))
}

func containsDebugCategory(categories []openrealtime.DebugCategory, want openrealtime.DebugCategory) bool {
	for _, category := range categories {
		if category == want {
			return true
		}
	}
	return false
}

func (session *session) inspectionAccess() (*openrealtime.InspectionAccess, error) {
	session.inspectionMu.Lock()
	defer session.inspectionMu.Unlock()
	if session.inspectionClosed {
		return nil, nil
	}
	// Every explicit negotiation rotates the capability. The session retains
	// only the lookup digest, never the bearer it has already returned.
	session.config.inspections.revoke(session.inspectionLease)
	session.inspectionLease = inspectionLease{}
	source, ok := session.runtime.(liveInspectionSource)
	if !ok {
		return nil, nil
	}
	lease, access, err := session.config.inspections.issue(session.id, source)
	if errors.Is(err, errInspectionUnavailable) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	session.inspectionLease = lease
	return &access, nil
}

func (session *session) disableInspection() {
	session.inspectionMu.Lock()
	defer session.inspectionMu.Unlock()
	session.config.inspections.revoke(session.inspectionLease)
	session.inspectionLease = inspectionLease{}
}

func (session *session) closeInspection() {
	session.inspectionMu.Lock()
	defer session.inspectionMu.Unlock()
	session.inspectionClosed = true
	session.config.inspections.revoke(session.inspectionLease)
	session.inspectionLease = inspectionLease{}
}
