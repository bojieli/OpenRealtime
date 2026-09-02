package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	maxResourceIDBytes       = 64
	maxArtifactTitleBytes    = 512
	maxDownloadFilenameBytes = 255
	maxMediaTypeBytes        = 255
	maximumResourceEntries   = 256
)

var (
	// ErrResourceStoreClosed means a withdrawn plugin service was retained by a
	// stale caller. No content is admitted or returned after this error.
	ErrResourceStoreClosed = errors.New("presentation resource store is closed")
	// ErrResourceStoreQuiescing means reconciliation has temporarily closed
	// mutation admission while capturing exact restorable state.
	ErrResourceStoreQuiescing = errors.New("presentation resource store is quiescing for state capture")
)

// ResourceStoreLimits are the effective, fully bounded retention limits of
// one artifact or download provider. They are configuration, not mutable
// client input.
type ResourceStoreLimits struct {
	MaxEntries    int   `json:"max_entries"`
	MaxItemBytes  int64 `json:"max_item_bytes"`
	MaxTotalBytes int64 `json:"max_total_bytes"`
}

// ResourceStoreStats are a payload-free snapshot suitable for health and
// inspection surfaces. Bytes counts only retained model-authored content.
type ResourceStoreStats struct {
	Limits    ResourceStoreLimits `json:"limits"`
	Entries   int                 `json:"entries"`
	Bytes     int64               `json:"bytes"`
	Evictions uint64              `json:"evictions"`
	Closed    bool                `json:"closed"`
}

type resourceStoreConfig struct {
	MaxEntries    int   `json:"max_entries,omitempty"`
	MaxItemBytes  int64 `json:"max_item_bytes,omitempty"`
	MaxTotalBytes int64 `json:"max_total_bytes,omitempty"`
}

type resourceStorePolicy struct {
	name              string
	defaults          ResourceStoreLimits
	maximumItemBytes  int64
	maximumTotalBytes int64
}

func parseResourceStoreConfig(raw []byte, policy resourceStorePolicy) (ResourceStoreLimits, error) {
	if err := strictjson.Validate(raw); err != nil {
		return ResourceStoreLimits{}, fmt.Errorf("%s config: %w", policy.name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config resourceStoreConfig
	if err := decoder.Decode(&config); err != nil {
		return ResourceStoreLimits{}, fmt.Errorf("%s config: %w", policy.name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ResourceStoreLimits{}, fmt.Errorf("%s config has trailing JSON value", policy.name)
	} else if !errors.Is(err, io.EOF) {
		return ResourceStoreLimits{}, fmt.Errorf("%s config trailing data: %w", policy.name, err)
	}
	if config.MaxEntries == 0 {
		config.MaxEntries = policy.defaults.MaxEntries
	}
	if config.MaxItemBytes == 0 {
		config.MaxItemBytes = policy.defaults.MaxItemBytes
	}
	if config.MaxTotalBytes == 0 {
		config.MaxTotalBytes = policy.defaults.MaxTotalBytes
	}
	if config.MaxEntries < 1 || config.MaxEntries > maximumResourceEntries {
		return ResourceStoreLimits{}, fmt.Errorf("%s max_entries must be in [1,%d]",
			policy.name, maximumResourceEntries)
	}
	if config.MaxItemBytes < 1 || config.MaxItemBytes > policy.maximumItemBytes {
		return ResourceStoreLimits{}, fmt.Errorf("%s max_item_bytes must be in [1,%d]",
			policy.name, policy.maximumItemBytes)
	}
	if config.MaxTotalBytes < config.MaxItemBytes || config.MaxTotalBytes > policy.maximumTotalBytes {
		return ResourceStoreLimits{}, fmt.Errorf(
			"%s max_total_bytes must be in [max_item_bytes,%d]", policy.name, policy.maximumTotalBytes)
	}
	return ResourceStoreLimits{
		MaxEntries: config.MaxEntries, MaxItemBytes: config.MaxItemBytes,
		MaxTotalBytes: config.MaxTotalBytes,
	}, nil
}

type resourceLifecycle struct {
	closed  bool
	active  int
	drained chan struct{}
}

func newResourceLifecycle() resourceLifecycle {
	return resourceLifecycle{drained: make(chan struct{})}
}

// beginLocked and finishLocked are called while the owning store mutex is
// held. An admitted handler keeps the lifecycle active until its last write.
func (lifecycle *resourceLifecycle) beginLocked() bool {
	if lifecycle.closed {
		return false
	}
	lifecycle.active++
	return true
}

func (lifecycle *resourceLifecycle) finishLocked() {
	if lifecycle.active <= 0 {
		panic("presentation resource lifecycle finished without an active request")
	}
	lifecycle.active--
	if lifecycle.closed && lifecycle.active == 0 {
		close(lifecycle.drained)
	}
}

func (lifecycle *resourceLifecycle) closeLocked() <-chan struct{} {
	if !lifecycle.closed {
		lifecycle.closed = true
		if lifecycle.active == 0 {
			close(lifecycle.drained)
		}
	}
	return lifecycle.drained
}

func waitForResourceDrain(ctx context.Context, drained <-chan struct{}) error {
	if ctx == nil {
		return errors.New("close presentation resource store: nil context")
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("close presentation resource store: %w", ctx.Err())
	}
}

func validateResourceID(kind, id string) error {
	if id == "" || len(id) > maxResourceIDBytes {
		return fmt.Errorf("%s id must be 1-%d ASCII letters, digits, dash, or underscore",
			kind, maxResourceIDBytes)
	}
	for index := 0; index < len(id); index++ {
		character := id[index]
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return fmt.Errorf("%s id must be 1-%d ASCII letters, digits, dash, or underscore",
				kind, maxResourceIDBytes)
		}
	}
	return nil
}

func validateArtifactTitle(title string) (string, error) {
	if title == "" {
		return "", nil
	}
	if !utf8.ValidString(title) || title != strings.TrimSpace(title) || len(title) > maxArtifactTitleBytes {
		return "", fmt.Errorf("artifact title must be canonical UTF-8 of at most %d bytes",
			maxArtifactTitleBytes)
	}
	for _, character := range title {
		if unicode.IsControl(character) {
			return "", errors.New("artifact title cannot contain control characters")
		}
	}
	return title, nil
}

func validateDownloadFilename(filename string) error {
	if filename == "" || filename == "." || filename == ".." ||
		!utf8.ValidString(filename) || filename != strings.TrimSpace(filename) ||
		len(filename) > maxDownloadFilenameBytes || strings.ContainsAny(filename, `/\\`) {
		return fmt.Errorf("download filename must be one canonical UTF-8 name of at most %d bytes",
			maxDownloadFilenameBytes)
	}
	for _, character := range filename {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return errors.New("download filename cannot contain control or formatting characters")
		}
	}
	return nil
}

func canonicalMediaType(mediaType string) (string, error) {
	if mediaType == "" {
		return "application/octet-stream", nil
	}
	if mediaType != strings.TrimSpace(mediaType) || len(mediaType) > maxMediaTypeBytes {
		return "", fmt.Errorf("download media type must be canonical and at most %d bytes", maxMediaTypeBytes)
	}
	base, parameters, err := mime.ParseMediaType(mediaType)
	if err != nil || !strings.Contains(base, "/") || strings.Contains(base, "*") {
		return "", fmt.Errorf("invalid download media type %q", mediaType)
	}
	canonical := mime.FormatMediaType(base, parameters)
	if canonical == "" || len(canonical) > maxMediaTypeBytes {
		return "", fmt.Errorf("invalid download media type %q", mediaType)
	}
	return canonical, nil
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func wipe(content []byte) {
	clear(content)
}

var errResourceRevisionMismatch = errors.New("presentation resource revision no longer matches")

// validateResourceRevision makes a metadata reference immutable when its
// version/digest query is used. The unqualified route remains a latest-value
// discovery/compatibility endpoint, but a partially qualified or extended
// query is never interpreted loosely.
func validateResourceRevision(request *http.Request, version uint64, digest string) error {
	if request == nil || request.URL == nil {
		return errors.New("presentation resource request is unavailable")
	}
	if request.URL.RawQuery == "" {
		return nil
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(query) != 2 || len(query["version"]) != 1 || len(query["digest"]) != 1 {
		return errors.New("presentation resource revision query must contain one version and one digest")
	}
	wantVersion, err := strconv.ParseUint(query.Get("version"), 10, 64)
	if err != nil || wantVersion == 0 || query.Get("version") != strconv.FormatUint(wantVersion, 10) {
		return errors.New("presentation resource version is not canonical")
	}
	wantDigest := query.Get("digest")
	if len(wantDigest) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(wantDigest, "sha256:") || wantDigest != strings.ToLower(wantDigest) {
		return errors.New("presentation resource digest is not canonical")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(wantDigest, "sha256:")); err != nil {
		return errors.New("presentation resource digest is not canonical")
	}
	if wantVersion != version || wantDigest != digest {
		return errResourceRevisionMismatch
	}
	return nil
}

func escapeHTMLTitle(title string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(title)
}
