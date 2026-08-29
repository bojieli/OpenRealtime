// Package media contains graph-native retained-media and attachment-resolution
// elements. Large payloads cross only explicit retention or resolution edges;
// trajectories and ordinary context channels carry bounded opaque handles.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	retainedMediaRuntimeID       = "builtin://openrealtime/elements/media.RetainedMedia"
	attachmentResolverRuntimeID  = "builtin://openrealtime/elements/media.ResolveAttachment"
	mediaImplementationRevision  = "implementation:1"
	maximumConfigurationByteSize = 1 << 30
	maximumIdentifierBytes       = 256
	maximumReasonBytes           = 1024
)

var (
	retainRequestType = element.Request(
		element.Named("media.RetainRequest"), element.Named("flow.RequestID"),
	)
	retainResultType = element.Reply(
		element.Named("media.RetainResult"), element.Named("flow.RequestID"),
	)
	fetchRequestType = element.Request(
		element.Named("media.FetchRequest"), element.Named("flow.RequestID"),
	)
	fetchResultType = element.Reply(
		element.Named("media.FetchResult"), element.Named("flow.RequestID"),
	)
	releaseRequestType = element.Request(
		element.Named("media.ReleaseRequest"), element.Named("flow.RequestID"),
	)
	releaseResultType = element.Reply(
		element.Named("media.ReleaseResult"), element.Named("flow.RequestID"),
	)
	returnLeaseRequestType = element.Request(
		element.Named("media.ReturnLeaseRequest"), element.Named("flow.RequestID"),
	)
	returnLeaseResultType = element.Reply(
		element.Named("media.ReturnLeaseResult"), element.Named("flow.RequestID"),
	)
	storeCancelType      = element.Interrupt(element.Named("flow.RequestID"))
	retentionOutcomeType = element.Event(
		element.Named("media.RetentionOutcome"),
	)
	retentionMetricsType = element.State(
		element.Named("media.RetentionMetrics"),
	)
	resolveRequestType = element.Request(
		element.Named("media.ResolveRequest"), element.Named("flow.RequestID"),
	)
	resolvedAttachmentType = element.Reply(
		element.Named("media.UntrustedContent"), element.Named("flow.RequestID"),
	)
	resolverOutcomeType = element.Event(
		element.Named("media.ResolverOutcome"),
	)
)

func RetainRequestType() element.Type      { return retainRequestType.Clone() }
func RetainResultType() element.Type       { return retainResultType.Clone() }
func FetchRequestType() element.Type       { return fetchRequestType.Clone() }
func FetchResultType() element.Type        { return fetchResultType.Clone() }
func ReleaseRequestType() element.Type     { return releaseRequestType.Clone() }
func ReleaseResultType() element.Type      { return releaseResultType.Clone() }
func ReturnLeaseRequestType() element.Type { return returnLeaseRequestType.Clone() }
func ReturnLeaseResultType() element.Type  { return returnLeaseResultType.Clone() }
func StoreCancelType() element.Type        { return storeCancelType.Clone() }
func RetentionOutcomeType() element.Type   { return retentionOutcomeType.Clone() }
func RetentionMetricsType() element.Type   { return retentionMetricsType.Clone() }
func ResolveRequestType() element.Type     { return resolveRequestType.Clone() }
func ResolvedAttachmentType() element.Type { return resolvedAttachmentType.Clone() }
func ResolverOutcomeType() element.Type    { return resolverOutcomeType.Clone() }

// AttachmentKind is carried in addition to MIME type so policy can distinguish
// an image shown by a participant from a file or an opaque future attachment.
type AttachmentKind string

const (
	AttachmentImage AttachmentKind = "image"
	AttachmentFile  AttachmentKind = "file"
	AttachmentOther AttachmentKind = "attachment"
)

func (kind AttachmentKind) validate() error {
	switch kind {
	case AttachmentImage, AttachmentFile, AttachmentOther:
		return nil
	default:
		return fmt.Errorf("unsupported attachment kind %q", kind)
	}
}

// RetainRequest is the only payload that introduces raw attachment bytes into
// the retained-media state. SourceRevision binds the bytes to the observation
// revision that caused retention; Scope prevents a handle from crossing a
// deployment-selected retention boundary.
type RetainRequest struct {
	AttachmentID   string         `json:"attachment_id"`
	Kind           AttachmentKind `json:"kind"`
	Name           string         `json:"name,omitempty"`
	MIMEType       string         `json:"mime_type"`
	Content        []byte         `json:"-"`
	SHA256         string         `json:"sha256,omitempty"`
	Source         string         `json:"source"`
	Scope          string         `json:"scope"`
	SourceRevision uint64         `json:"source_revision"`
	CapturedNS     uint64         `json:"captured_ns,omitempty"`
	Width          int            `json:"width,omitempty"`
	Height         int            `json:"height,omitempty"`
}

// AttachmentHandle is immutable, bounded metadata plus the opaque capability
// needed to resolve or release one retained item. Payload-free runtime tracing
// never records Capability. Callers must still treat it as a secret.
type AttachmentHandle struct {
	Handle         string         `json:"handle"`
	Capability     string         `json:"capability"`
	AttachmentID   string         `json:"attachment_id"`
	Kind           AttachmentKind `json:"kind"`
	Name           string         `json:"name,omitempty"`
	MIMEType       string         `json:"mime_type"`
	SHA256         string         `json:"sha256"`
	Bytes          int            `json:"bytes"`
	Source         string         `json:"source"`
	Scope          string         `json:"scope"`
	SourceRevision uint64         `json:"source_revision"`
	CapturedNS     uint64         `json:"captured_ns,omitempty"`
	Width          int            `json:"width,omitempty"`
	Height         int            `json:"height,omitempty"`
}

// MediaRef is the deliberately capability-free trajectory representation.
// Resolution capability remains on a separately routed AttachmentHandle.
func (handle AttachmentHandle) MediaRef() trajectory.MediaRef {
	return trajectory.MediaRef{
		Handle: handle.Handle, MIMEType: handle.MIMEType, Source: handle.Source,
		Width: handle.Width, Height: handle.Height, Bytes: handle.Bytes,
		CapturedNS: handle.CapturedNS,
	}
}

type ResultKind string

const (
	ResultSucceeded ResultKind = "succeeded"
	ResultCanceled  ResultKind = "canceled"
	ResultRefused   ResultKind = "refused"
	ResultFailed    ResultKind = "failed"
	ResultExpired   ResultKind = "expired"
	ResultIgnored   ResultKind = "ignored"
)

type RetainResult struct {
	Kind    ResultKind       `json:"kind"`
	Handle  AttachmentHandle `json:"handle,omitempty"`
	Code    string           `json:"code,omitempty"`
	Message string           `json:"message,omitempty"`
}

type FetchRequest struct {
	Handle                 AttachmentHandle `json:"handle"`
	ExpectedSHA256         string           `json:"expected_sha256,omitempty"`
	ExpectedSourceRevision uint64           `json:"expected_source_revision,omitempty"`
}

type FetchResult struct {
	Kind    ResultKind       `json:"kind"`
	Handle  AttachmentHandle `json:"handle,omitempty"`
	Lease   BlobLease        `json:"lease,omitempty"`
	Code    string           `json:"code,omitempty"`
	Message string           `json:"message,omitempty"`
}

type ReleaseRequest struct {
	Handle     string `json:"handle"`
	Capability string `json:"capability"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason,omitempty"`
}

type ReleaseResult struct {
	Kind    ResultKind `json:"kind"`
	Handle  string     `json:"handle,omitempty"`
	Code    string     `json:"code,omitempty"`
	Message string     `json:"message,omitempty"`
}

// BlobLease is an opaque, read-only view of one retained blob. The backing
// bytes are unexported and copied exactly once when retention crosses its
// trust boundary. Open returns a reader that copies only into caller-owned
// read buffers and observes revocation between reads. The consumer must route
// a ReturnLeaseRequest after it finishes; eviction and handle release stop new
// fetches but cannot invalidate an already delivered lease silently.
type BlobLease struct {
	LeaseID string           `json:"lease_id"`
	Handle  AttachmentHandle `json:"handle"`
	state   *blobLeaseState
}

type immutableBlob struct {
	digest string
	bytes  []byte
}

type blobLeaseState struct {
	mu        sync.RWMutex
	id        string
	sessionID string
	handle    AttachmentHandle
	blob      *immutableBlob
	revoked   bool
}

func newImmutableBlob(content []byte) *immutableBlob {
	copied := slices.Clone(content)
	return &immutableBlob{digest: digestContent(copied), bytes: copied}
}

func newBlobLease(id, sessionID string, handle AttachmentHandle, blob *immutableBlob) BlobLease {
	state := &blobLeaseState{id: id, sessionID: sessionID, handle: handle, blob: blob}
	return BlobLease{LeaseID: id, Handle: handle, state: state}
}

// Open returns a fresh read-only cursor. It never exposes the backing slice.
func (lease BlobLease) Open() (io.ReadCloser, error) {
	if err := lease.validateIdentity(); err != nil {
		return nil, err
	}
	return &leaseReader{state: lease.state}, nil
}

// Size reports the immutable backing length without exposing its bytes.
func (lease BlobLease) Size() (int, error) {
	if err := lease.validateIdentity(); err != nil {
		return 0, err
	}
	lease.state.mu.RLock()
	defer lease.state.mu.RUnlock()
	if lease.state.revoked || lease.state.blob == nil {
		return 0, errors.New("media blob lease is revoked")
	}
	return len(lease.state.blob.bytes), nil
}

func (lease BlobLease) validateIdentity() error {
	if lease.state == nil || strings.TrimSpace(lease.LeaseID) == "" {
		return errors.New("media blob lease has no live backing state")
	}
	lease.state.mu.RLock()
	defer lease.state.mu.RUnlock()
	if lease.state.id != lease.LeaseID || lease.state.handle != lease.Handle {
		return errors.New("media blob lease identity was altered")
	}
	if lease.state.revoked || lease.state.blob == nil {
		return errors.New("media blob lease is revoked")
	}
	if lease.state.blob.digest != lease.Handle.SHA256 ||
		len(lease.state.blob.bytes) != lease.Handle.Bytes {
		return errors.New("media blob lease backing identity drifted")
	}
	return nil
}

// validateFor binds a live lease to the complete retained handle and to the
// session in which retention issued it. It deliberately remains package-local:
// graph consumers can read or return a lease, but cannot mint a new authority
// check around a detached/serialized value.
func (lease BlobLease) validateFor(sessionID string, handle AttachmentHandle) error {
	if lease.state == nil || lease.LeaseID == "" {
		return errors.New("media blob lease has no live backing state")
	}
	lease.state.mu.RLock()
	defer lease.state.mu.RUnlock()
	if lease.state.id != lease.LeaseID || lease.state.handle != lease.Handle {
		return errors.New("media blob lease identity was altered")
	}
	if lease.state.revoked || lease.state.blob == nil {
		return errors.New("media blob lease is revoked")
	}
	if lease.state.blob.digest != lease.Handle.SHA256 ||
		len(lease.state.blob.bytes) != lease.Handle.Bytes {
		return errors.New("media blob lease backing identity drifted")
	}
	if lease.state.sessionID != sessionID {
		return errors.New("media blob lease crossed a session boundary")
	}
	if lease.Handle != handle || lease.state.handle != handle {
		return errors.New("media blob lease does not match the complete attachment handle")
	}
	return nil
}

func (lease BlobLease) sessionID() string {
	if lease.state == nil {
		return ""
	}
	lease.state.mu.RLock()
	defer lease.state.mu.RUnlock()
	return lease.state.sessionID
}

func (lease BlobLease) revoke() {
	if lease.state == nil {
		return
	}
	lease.state.mu.Lock()
	lease.state.revoked = true
	lease.state.blob = nil
	lease.state.mu.Unlock()
}

type leaseReader struct {
	mu     sync.Mutex
	state  *blobLeaseState
	offset int
	closed bool
}

func (reader *leaseReader) Read(destination []byte) (int, error) {
	if reader == nil {
		return 0, errors.New("media blob lease reader is closed")
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.state == nil || reader.closed {
		return 0, errors.New("media blob lease reader is closed")
	}
	reader.state.mu.RLock()
	defer reader.state.mu.RUnlock()
	if reader.state.revoked || reader.state.blob == nil {
		return 0, errors.New("media blob lease is revoked")
	}
	if reader.offset >= len(reader.state.blob.bytes) {
		return 0, io.EOF
	}
	read := copy(destination, reader.state.blob.bytes[reader.offset:])
	reader.offset += read
	return read, nil
}

func (reader *leaseReader) Close() error {
	if reader == nil {
		return nil
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.closed = true
	return nil
}

// ReturnLeaseRequest explicitly ends one delivered read lease.
type ReturnLeaseRequest struct {
	Lease  BlobLease `json:"lease"`
	Reason string    `json:"reason,omitempty"`
}

type ReturnLeaseResult struct {
	Kind    ResultKind `json:"kind"`
	LeaseID string     `json:"lease_id,omitempty"`
	Handle  string     `json:"handle,omitempty"`
	Code    string     `json:"code,omitempty"`
	Message string     `json:"message,omitempty"`
}

type StoreCancel struct {
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type RetentionOutcome struct {
	Kind       ResultKind `json:"kind"`
	Operation  string     `json:"operation"`
	RequestID  string     `json:"request_id,omitempty"`
	Handle     string     `json:"handle,omitempty"`
	Crossed    bool       `json:"crossed_boundary,omitempty"`
	Code       string     `json:"code,omitempty"`
	Message    string     `json:"message,omitempty"`
	FinishedNS uint64     `json:"finished_ns,omitempty"`
}

type RetentionMetrics struct {
	Retained        uint64 `json:"retained"`
	Resolved        uint64 `json:"resolved"`
	Released        uint64 `json:"released"`
	Evicted         uint64 `json:"evicted"`
	Misses          uint64 `json:"misses"`
	Leases          uint64 `json:"leases"`
	Returned        uint64 `json:"returned_leases"`
	Revoked         uint64 `json:"revoked_leases"`
	ActiveLeases    int    `json:"active_leases"`
	LeasedBytes     int    `json:"leased_bytes"`
	MaxActiveLeases int    `json:"max_active_leases"`
	LiveItems       int    `json:"live_items"`
	LiveBytes       int    `json:"live_bytes"`
	MaxItems        int    `json:"max_items"`
	MaxBytes        int    `json:"max_bytes"`
	WindowMS        int64  `json:"window_ms"`
}

type ResolveRequest struct {
	Handle                 AttachmentHandle `json:"handle"`
	AcceptMIMETypes        []string         `json:"accept_mime_types,omitempty"`
	MaxBytes               int              `json:"max_bytes,omitempty"`
	ExpectedSourceRevision uint64           `json:"expected_source_revision,omitempty"`
}

// ResolvedAttachment stays a media.UntrustedContent reply. A parser or vision
// element must explicitly turn it into an observer-authority observation; it
// cannot type-connect to user/system text or executable actions.
type ResolvedAttachment struct {
	Kind    ResultKind       `json:"kind"`
	Handle  AttachmentHandle `json:"handle,omitempty"`
	Lease   BlobLease        `json:"lease,omitempty"`
	Code    string           `json:"code,omitempty"`
	Message string           `json:"message,omitempty"`
}

type ResolverOutcome struct {
	Kind       ResultKind `json:"kind"`
	RequestID  string     `json:"request_id,omitempty"`
	Handle     string     `json:"handle,omitempty"`
	Bytes      int        `json:"bytes,omitempty"`
	Code       string     `json:"code,omitempty"`
	Message    string     `json:"message,omitempty"`
	FinishedNS uint64     `json:"finished_ns,omitempty"`
}

func cloneRetainRequest(request RetainRequest) RetainRequest {
	return request
}

func cloneFetchResult(result FetchResult) FetchResult {
	return result
}

func cloneResolveRequest(request ResolveRequest) ResolveRequest {
	request.AcceptMIMETypes = slices.Clone(request.AcceptMIMETypes)
	return request
}

func canonicalSHA256(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return "", errors.New("content digest must be a canonical SHA-256 digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, prefix)); err != nil {
		return "", fmt.Errorf("invalid content digest: %w", err)
	}
	if value != strings.ToLower(value) {
		return "", errors.New("content digest must use lowercase hexadecimal")
	}
	return value, nil
}

func validateIdentifier(label, value string, required bool) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("%s is required", label)
	}
	if value != trimmed {
		return fmt.Errorf("%s must not have surrounding whitespace", label)
	}
	if len(value) > maximumIdentifierBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maximumIdentifierBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return fmt.Errorf("%s contains whitespace or control characters", label)
		}
	}
	return nil
}

func boundedReason(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	if len(value) <= maximumReasonBytes {
		return value
	}
	const suffix = "…"
	value = value[:maximumReasonBytes-len(suffix)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}

func digestContent(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func canonicalMIMEType(value string) (string, error) {
	value = strings.TrimSpace(value)
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(mediaType, "/") {
		if err == nil {
			err = errors.New("MIME type requires a type and subtype")
		}
		return "", fmt.Errorf("invalid MIME type %q: %w", value, err)
	}
	canonical := mime.FormatMediaType(strings.ToLower(mediaType), parameters)
	if canonical == "" {
		return "", fmt.Errorf("invalid MIME type %q", value)
	}
	return canonical, nil
}

func reportLiveResolution(
	reporter element.ResolutionReporter, runtimeID string, capabilities []string,
) error {
	artifact := liveidentity.Artifact{ID: runtimeID, Revision: mediaImplementationRevision}
	resolved := make([]element.CapabilityResolution, 0, len(capabilities))
	for _, capability := range capabilities {
		resolved = append(resolved, liveidentity.Capability(
			capability, "openrealtime.media/v1", artifact, liveidentity.Artifact{},
		))
	}
	return liveidentity.Report(reporter, artifact, resolved)
}

func runtimeDependencies(services element.Services) (graphruntime.Clock, error) {
	value, _, found := services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("media element has no runtime clock service")
	}
	clock, ok := value.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", value)
	}
	return clock, nil
}

func sequenceDependency(services element.Services) (*graphruntime.SequenceAllocator, error) {
	value, _, found := services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("media element has no runtime sequence service")
	}
	sequences, ok := value.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", value)
	}
	return sequences, nil
}
