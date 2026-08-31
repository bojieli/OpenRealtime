// Command companionproof validates the exact redacted management responses
// captured by the hosted browser and macOS companion clients. It is an
// internal release-gate helper, not part of the OpenRealtime public CLI.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/management"
	"golang.org/x/sys/unix"
)

const maximumSnapshotBytes = 32 << 20

var capabilityPattern = regexp.MustCompile(`mgmt_[A-Za-z0-9_-]{16,512}`)
var noncePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type options struct {
	browserPath          string
	browserSession       string
	nativePath           string
	nativeSession        string
	invocationNonce      string
	serverIdentityDigest string
}

type snapshotReceipt struct {
	Order                uint64    `json:"order"`
	Client               string    `json:"client"`
	Transport            string    `json:"transport"`
	Resource             string    `json:"resource"`
	ResponseURL          string    `json:"response_url"`
	SessionID            string    `json:"session_id"`
	PayloadDigest        string    `json:"payload_digest"`
	PayloadBytes         int       `json:"payload_bytes"`
	Sequence             uint64    `json:"sequence"`
	ObservedAt           time.Time `json:"observed_at"`
	StableIdentityDigest string    `json:"stable_identity_digest"`
}

type receipt struct {
	Schema               string          `json:"schema"`
	InvocationNonce      string          `json:"invocation_nonce"`
	ServerIdentityDigest string          `json:"server_identity_digest"`
	Browser              snapshotReceipt `json:"browser"`
	Native               snapshotReceipt `json:"native"`
	Graph                struct {
		ID                   string `json:"id"`
		Revision             uint64 `json:"revision"`
		Fingerprint          string `json:"fingerprint"`
		ConfigurationDigest  string `json:"configuration_digest"`
		StableIdentityDigest string `json:"stable_identity_digest"`
	} `json:"graph"`
}

type stableNode struct {
	ID         string                 `json:"id"`
	Resolution inspect.NodeResolution `json:"resolution"`
}

type stableSnapshot struct {
	FormatVersion uint64                            `json:"format_version"`
	GraphID       string                            `json:"graph_id"`
	GraphRevision uint64                            `json:"graph_revision"`
	Fingerprint   string                            `json:"fingerprint"`
	Configuration inspect.ArtifactIdentity          `json:"configuration"`
	Deployment    *inspect.DeploymentEvidence       `json:"deployment,omitempty"`
	Adapter       *inspect.SessionAdapterResolution `json:"adapter,omitempty"`
	Nodes         []stableNode                      `json:"nodes"`
}

type snapshotReadStage uint8

const (
	snapshotParentOpened snapshotReadStage = iota + 1
	snapshotEntryChecked
	snapshotArtifactOpened
	snapshotPayloadRead
)

type snapshotReadCheckpoint func(snapshotReadStage) error

type snapshotObjectIdentity struct {
	device          uint64
	inode           uint64
	mode            uint32
	links           uint64
	owner           uint32
	size            int64
	modifiedSeconds int64
	modifiedNanos   int64
	statusSeconds   int64
	statusNanos     int64
}

func main() {
	var value options
	flag.StringVar(&value.browserPath, "browser", "", "create-only browser live snapshot")
	flag.StringVar(&value.browserSession, "browser-session", "", "browser session identity")
	flag.StringVar(&value.nativePath, "native", "", "create-only native live snapshot")
	flag.StringVar(&value.nativeSession, "native-session", "", "native session identity")
	flag.StringVar(&value.invocationNonce, "nonce", "", "hosted invocation nonce")
	flag.StringVar(&value.serverIdentityDigest, "server-identity-digest", "", "frozen server identity digest")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("unexpected positional argument"))
	}
	result, err := validate(value)
	if err != nil {
		fatal(err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		fatal(fmt.Errorf("encode receipt: %w", err))
	}
	fmt.Printf("OPENREALTIME_COMPANION_MANAGEMENT_RECEIPT %s\n", payload)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "companion management proof: %v\n", err)
	os.Exit(1)
}

func validate(value options) (receipt, error) {
	if value.browserPath == "" || value.nativePath == "" ||
		!management.CanonicalSessionID(value.browserSession) ||
		!management.CanonicalSessionID(value.nativeSession) ||
		value.browserSession == value.nativeSession ||
		!noncePattern.MatchString(value.invocationNonce) ||
		!management.CanonicalDigest(value.serverIdentityDigest) {
		return receipt{}, errors.New("two distinct canonical sessions and snapshot paths are required")
	}
	browser, browserPayload, err := readSnapshot(value.browserPath)
	if err != nil {
		return receipt{}, fmt.Errorf("browser snapshot: %w", err)
	}
	native, nativePayload, err := readSnapshot(value.nativePath)
	if err != nil {
		return receipt{}, fmt.Errorf("native snapshot: %w", err)
	}
	browserStable, err := stableIdentity(browser)
	if err != nil {
		return receipt{}, fmt.Errorf("browser stable identity: %w", err)
	}
	nativeStable, err := stableIdentity(native)
	if err != nil {
		return receipt{}, fmt.Errorf("native stable identity: %w", err)
	}
	if !reflect.DeepEqual(browserStable, nativeStable) {
		return receipt{}, errors.New("browser and native snapshots do not identify one unchanged runtime graph")
	}
	stablePayload, err := json.Marshal(browserStable)
	if err != nil {
		return receipt{}, fmt.Errorf("encode stable identity: %w", err)
	}
	stableDigest := digest(stablePayload)
	result := receipt{
		Schema:               "openrealtime/companion/management-receipt/v1",
		InvocationNonce:      value.invocationNonce,
		ServerIdentityDigest: value.serverIdentityDigest,
	}
	result.Browser = makeSnapshotReceipt(
		1, "browser", "webrtc",
		"http://127.0.0.1:18767/client/v1/management/sessions/"+
			url.PathEscape(value.browserSession)+"/live",
		value.browserSession, browser, browserPayload, stableDigest,
	)
	result.Native = makeSnapshotReceipt(
		2, "macos_native", "websocket",
		"http://127.0.0.1:18767/client/v1/management/sessions/"+
			url.PathEscape(value.nativeSession)+"/live",
		value.nativeSession, native, nativePayload, stableDigest,
	)
	result.Graph.ID = browser.GraphID
	result.Graph.Revision = browser.GraphRevision
	result.Graph.Fingerprint = browser.Fingerprint
	result.Graph.ConfigurationDigest = browser.Configuration.Digest
	result.Graph.StableIdentityDigest = stableDigest
	return result, nil
}

func readSnapshot(path string) (inspect.Live, []byte, error) {
	return readSnapshotWithCheckpoint(path, nil)
}

func readSnapshotWithCheckpoint(
	path string,
	checkpoint snapshotReadCheckpoint,
) (inspect.Live, []byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." ||
		filepath.Base(path) == string(filepath.Separator) {
		return inspect.Live{}, nil, errors.New("artifact path is not canonical and absolute")
	}

	parentPath := filepath.Dir(path)
	base := filepath.Base(path)
	var parentPathBefore unix.Stat_t
	if err := unix.Lstat(parentPath, &parentPathBefore); err != nil {
		return inspect.Live{}, nil, fmt.Errorf("inspect artifact parent: %w", err)
	}
	if err := validateSnapshotParent(parentPathBefore); err != nil {
		return inspect.Live{}, nil, err
	}
	parentDescriptor, err := unix.Open(parentPath,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return inspect.Live{}, nil, fmt.Errorf("open artifact parent without following links: %w", err)
	}
	parent := os.NewFile(uintptr(parentDescriptor), parentPath)
	if parent == nil {
		_ = unix.Close(parentDescriptor)
		return inspect.Live{}, nil, errors.New("open artifact parent: invalid descriptor")
	}
	defer parent.Close()

	var parentOpened unix.Stat_t
	if err := unix.Fstat(parentDescriptor, &parentOpened); err != nil {
		return inspect.Live{}, nil, fmt.Errorf("inspect opened artifact parent: %w", err)
	}
	if err := validateSnapshotParent(parentOpened); err != nil {
		return inspect.Live{}, nil, err
	}
	if !sameSnapshotAuthority(parentPathBefore, parentOpened) {
		return inspect.Live{}, nil, errors.New("artifact parent identity changed while it was opened")
	}
	if err := runSnapshotCheckpoint(checkpoint, snapshotParentOpened); err != nil {
		return inspect.Live{}, nil, err
	}

	var entryBefore unix.Stat_t
	if err := unix.Fstatat(parentDescriptor, base, &entryBefore, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return inspect.Live{}, nil, fmt.Errorf("inspect artifact entry: %w", err)
	}
	if err := validateSnapshotArtifact(entryBefore); err != nil {
		return inspect.Live{}, nil, err
	}
	if err := runSnapshotCheckpoint(checkpoint, snapshotEntryChecked); err != nil {
		return inspect.Live{}, nil, err
	}

	artifactDescriptor, err := unix.Openat(parentDescriptor, base,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return inspect.Live{}, nil, fmt.Errorf("open artifact without following symbolic links: %w", err)
	}
	file := os.NewFile(uintptr(artifactDescriptor), path)
	if file == nil {
		_ = unix.Close(artifactDescriptor)
		return inspect.Live{}, nil, errors.New("open artifact: invalid descriptor")
	}
	defer file.Close()

	var opened unix.Stat_t
	if err := unix.Fstat(artifactDescriptor, &opened); err != nil {
		return inspect.Live{}, nil, fmt.Errorf("inspect opened artifact: %w", err)
	}
	if err := validateSnapshotArtifact(opened); err != nil {
		return inspect.Live{}, nil, err
	}
	if snapshotIdentity(entryBefore) != snapshotIdentity(opened) {
		return inspect.Live{}, nil, errors.New("artifact identity changed while it was opened")
	}
	if err := runSnapshotCheckpoint(checkpoint, snapshotArtifactOpened); err != nil {
		return inspect.Live{}, nil, err
	}

	payload, err := io.ReadAll(io.LimitReader(file, maximumSnapshotBytes+1))
	if err != nil || len(payload) == 0 || len(payload) > maximumSnapshotBytes ||
		int64(len(payload)) != opened.Size {
		return inspect.Live{}, nil, errors.New("artifact could not be read within the response limit")
	}
	if err := runSnapshotCheckpoint(checkpoint, snapshotPayloadRead); err != nil {
		return inspect.Live{}, nil, err
	}

	var after unix.Stat_t
	if err := unix.Fstat(artifactDescriptor, &after); err != nil ||
		snapshotIdentity(opened) != snapshotIdentity(after) {
		return inspect.Live{}, nil, errors.New("artifact changed while it was read")
	}
	var entryAfter unix.Stat_t
	if err := unix.Fstatat(parentDescriptor, base, &entryAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		snapshotIdentity(after) != snapshotIdentity(entryAfter) {
		return inspect.Live{}, nil, errors.New("artifact path no longer identifies the opened artifact")
	}
	var parentPathAfter unix.Stat_t
	if err := unix.Lstat(parentPath, &parentPathAfter); err != nil ||
		!sameSnapshotAuthority(parentOpened, parentPathAfter) {
		return inspect.Live{}, nil, errors.New("artifact parent path no longer identifies the opened directory")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumSnapshotBytes,
	}); err != nil {
		return inspect.Live{}, nil, fmt.Errorf("strict JSON: %w", err)
	}
	if capabilityPattern.Match(payload) {
		return inspect.Live{}, nil, errors.New("artifact contains capability-shaped material")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var snapshot inspect.Live
	if err := decoder.Decode(&snapshot); err != nil {
		return inspect.Live{}, nil, fmt.Errorf("decode canonical live response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return inspect.Live{}, nil, errors.New("live response contains a trailing JSON value")
		}
		return inspect.Live{}, nil, fmt.Errorf("decode trailing live response: %w", err)
	}
	if err := management.ValidateSessionSnapshot(snapshot); err != nil {
		return inspect.Live{}, nil, err
	}
	if snapshot.Sequence == 0 || snapshot.ObservedAt.IsZero() || snapshot.State != "running" ||
		snapshot.Error != "" {
		return inspect.Live{}, nil, errors.New("live response is not a running sequenced observation")
	}
	for id, node := range snapshot.Nodes {
		if node.Error != "" || node.State == "failed" || node.State == "error" {
			return inspect.Live{}, nil, fmt.Errorf("live response node %q is failed", id)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return inspect.Live{}, nil, err
	}
	redacted, err := json.Marshal(management.RedactLive(snapshot))
	if err != nil {
		return inspect.Live{}, nil, err
	}
	if !bytes.Equal(encoded, redacted) {
		return inspect.Live{}, nil, errors.New("live response is not the idempotently redacted public projection")
	}
	return snapshot, payload, nil
}

func runSnapshotCheckpoint(checkpoint snapshotReadCheckpoint, stage snapshotReadStage) error {
	if checkpoint == nil {
		return nil
	}
	if err := checkpoint(stage); err != nil {
		return fmt.Errorf("artifact read checkpoint: %w", err)
	}
	return nil
}

func validateSnapshotParent(stat unix.Stat_t) error {
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != unix.S_IFDIR || mode&0o777 != 0o700 {
		return errors.New("artifact parent is not one private directory")
	}
	if int(stat.Uid) != os.Geteuid() {
		return errors.New("artifact parent has another owner")
	}
	return nil
}

func validateSnapshotArtifact(stat unix.Stat_t) error {
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != unix.S_IFREG || mode&0o777 != 0o600 || stat.Size <= 0 ||
		stat.Size > maximumSnapshotBytes {
		return errors.New("artifact must be one 0600 regular file within the response limit")
	}
	if int(stat.Uid) != os.Geteuid() || uint64(stat.Nlink) != 1 {
		return errors.New("artifact owner or link count is invalid")
	}
	return nil
}

func sameSnapshotAuthority(left, right unix.Stat_t) bool {
	leftMode := uint32(left.Mode)
	rightMode := uint32(right.Mode)
	return uint64(left.Dev) == uint64(right.Dev) && left.Ino == right.Ino &&
		leftMode == rightMode && left.Uid == right.Uid
}

func snapshotIdentity(stat unix.Stat_t) snapshotObjectIdentity {
	return snapshotObjectIdentity{
		device:          uint64(stat.Dev),
		inode:           stat.Ino,
		mode:            uint32(stat.Mode),
		links:           uint64(stat.Nlink),
		owner:           stat.Uid,
		size:            stat.Size,
		modifiedSeconds: stat.Mtim.Sec,
		modifiedNanos:   stat.Mtim.Nsec,
		statusSeconds:   stat.Ctim.Sec,
		statusNanos:     stat.Ctim.Nsec,
	}
}

func stableIdentity(snapshot inspect.Live) (stableSnapshot, error) {
	if snapshot.Configuration == nil {
		return stableSnapshot{}, errors.New("configuration identity is absent")
	}
	result := stableSnapshot{
		FormatVersion: snapshot.FormatVersion,
		GraphID:       snapshot.GraphID,
		GraphRevision: snapshot.GraphRevision,
		Fingerprint:   snapshot.Fingerprint,
		Configuration: *snapshot.Configuration,
		Deployment:    snapshot.Deployment,
		Adapter:       snapshot.Adapter,
		Nodes:         make([]stableNode, 0, len(snapshot.Nodes)),
	}
	for id, node := range snapshot.Nodes {
		if node.Resolution == nil {
			return stableSnapshot{}, fmt.Errorf("node %q has no resolution", id)
		}
		result.Nodes = append(result.Nodes, stableNode{ID: id, Resolution: node.Resolution.Clone()})
	}
	sort.Slice(result.Nodes, func(left, right int) bool {
		return result.Nodes[left].ID < result.Nodes[right].ID
	})
	return result, nil
}

func makeSnapshotReceipt(
	order uint64, client, transport, responseURL, session string,
	snapshot inspect.Live, payload []byte, stableDigest string,
) snapshotReceipt {
	return snapshotReceipt{
		Order: order, Client: client, Transport: transport, Resource: "live",
		ResponseURL: responseURL,
		SessionID:   session, PayloadDigest: digest(payload), PayloadBytes: len(payload),
		Sequence: snapshot.Sequence, ObservedAt: snapshot.ObservedAt,
		StableIdentityDigest: stableDigest,
	}
}

func digest(payload []byte) string {
	value := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(value[:])
}
