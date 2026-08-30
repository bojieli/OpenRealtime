package presentation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
)

const (
	EndpointDirectoryFormatVersion uint64 = 1
	maximumEndpointURLBytes               = 64 << 10
)

// EndpointName is the closed vocabulary of public-wire destinations a
// presentation deployment may publish. Endpoint presence is capability: an
// omitted effect or resource endpoint must not be reconstructed from another
// endpoint's origin.
type EndpointName string

const (
	EndpointRealtimeWebSocket EndpointName = "realtime.websocket"
	EndpointRealtimeWebRTC    EndpointName = "realtime.webrtc"
	EndpointManagement        EndpointName = "management.canonical"
	EndpointEffects           EndpointName = "effects.local"
	EndpointArtifacts         EndpointName = "resources.artifacts"
	EndpointDownloads         EndpointName = "resources.downloads"
)

// Endpoint protocols are identities, not descriptive labels. A consumer asks
// for both a closed endpoint name and the exact protocol it implements.
const (
	ProtocolRealtimeWebSocket = "openai.realtime.websocket.v1"
	ProtocolRealtimeWebRTC    = "openai.realtime.webrtc.v1"
	ProtocolManagement        = "openrealtime.management.v1"
	ProtocolClientEffects     = "openrealtime.client-effects.v1"
	ProtocolHostArtifacts     = "openrealtime.host-artifacts.v1"
	ProtocolHostDownloads     = "openrealtime.host-downloads.v1"
)

// Endpoint is one exact, credential-free public-wire destination. URL may be
// on a different origin from every other entry; consumers may not infer a
// missing entry by rewriting a present URL.
type Endpoint struct {
	Name     EndpointName `json:"name"`
	Protocol string       `json:"protocol"`
	URL      string       `json:"url"`
}

// EndpointDirectory is an immutable, canonical endpoint set. Endpoints are
// ordered by Name and Fingerprint covers the complete versioned directory.
type EndpointDirectory struct {
	FormatVersion uint64     `json:"format_version"`
	Fingerprint   string     `json:"fingerprint"`
	Endpoints     []Endpoint `json:"endpoints"`
}

// FreezeEndpointDirectory validates, sorts, snapshots, and fingerprints an
// exact endpoint set. Empty is valid for a profile with no public-wire target;
// individual consumers still require their selected endpoint at mount.
func FreezeEndpointDirectory(source []Endpoint) (EndpointDirectory, error) {
	// Normalize nil and empty input to the same JSON array so equivalent empty
	// profiles cannot acquire distinct fingerprints through slice provenance.
	endpoints := append([]Endpoint{}, source...)
	if len(endpoints) > len(endpointSpecifications) {
		return EndpointDirectory{}, fmt.Errorf(
			"endpoint directory has %d entries; maximum is %d",
			len(endpoints), len(endpointSpecifications),
		)
	}
	for index := range endpoints {
		if err := validateEndpoint(endpoints[index]); err != nil {
			return EndpointDirectory{}, fmt.Errorf("endpoint directory entry %d: %w", index, err)
		}
	}
	sort.Slice(endpoints, func(left, right int) bool {
		return endpoints[left].Name < endpoints[right].Name
	})
	for index := 1; index < len(endpoints); index++ {
		if endpoints[index-1].Name == endpoints[index].Name {
			return EndpointDirectory{}, fmt.Errorf(
				"endpoint directory repeats %q", endpoints[index].Name,
			)
		}
	}
	directory := EndpointDirectory{
		FormatVersion: EndpointDirectoryFormatVersion,
		Endpoints:     endpoints,
	}
	payload, err := json.Marshal(directory)
	if err != nil {
		return EndpointDirectory{}, fmt.Errorf("fingerprint endpoint directory: %w", err)
	}
	digest := sha256.Sum256(payload)
	directory.Fingerprint = "sha256:" + hex.EncodeToString(digest[:])
	return directory, nil
}

// Validate checks structure, canonical order, and the stored fingerprint.
func (directory EndpointDirectory) Validate() error {
	if directory.FormatVersion != EndpointDirectoryFormatVersion {
		return fmt.Errorf(
			"unsupported endpoint directory format %d", directory.FormatVersion,
		)
	}
	frozen, err := FreezeEndpointDirectory(directory.Endpoints)
	if err != nil {
		return err
	}
	if !slices.Equal(frozen.Endpoints, directory.Endpoints) {
		return errors.New("endpoint directory entries are not in canonical order")
	}
	if frozen.Fingerprint != directory.Fingerprint {
		return errors.New("endpoint directory fingerprint does not match its contents")
	}
	return nil
}

// Clone returns a recursively independent immutable snapshot.
func (directory EndpointDirectory) Clone() EndpointDirectory {
	directory.Endpoints = slices.Clone(directory.Endpoints)
	return directory
}

// Lookup returns a copy of one declared endpoint. Absence is not an invitation
// to infer a URL from another entry.
func (directory EndpointDirectory) Lookup(name EndpointName) (Endpoint, bool) {
	index, found := slices.BinarySearchFunc(directory.Endpoints, name,
		func(endpoint Endpoint, target EndpointName) int {
			return strings.Compare(string(endpoint.Name), string(target))
		})
	if !found {
		return Endpoint{}, false
	}
	return directory.Endpoints[index], true
}

// Require resolves one endpoint only when both its name and exact protocol
// match the consumer's frozen contract.
func (directory EndpointDirectory) Require(name EndpointName, protocol string) (Endpoint, error) {
	endpoint, found := directory.Lookup(name)
	if !found {
		return Endpoint{}, fmt.Errorf("endpoint directory does not declare %q", name)
	}
	if endpoint.Protocol != protocol {
		return Endpoint{}, fmt.Errorf(
			"endpoint directory %q protocol is %q, want %q",
			name, endpoint.Protocol, protocol,
		)
	}
	return endpoint, nil
}

type endpointSpecification struct {
	protocol string
	schemes  []string
	base     bool
}

var endpointSpecifications = map[EndpointName]endpointSpecification{
	EndpointRealtimeWebSocket: {protocol: ProtocolRealtimeWebSocket, schemes: []string{"ws", "wss"}},
	EndpointRealtimeWebRTC:    {protocol: ProtocolRealtimeWebRTC, schemes: []string{"http", "https"}},
	EndpointManagement:        {protocol: ProtocolManagement, schemes: []string{"http", "https"}, base: true},
	EndpointEffects:           {protocol: ProtocolClientEffects, schemes: []string{"ws", "wss"}, base: true},
	EndpointArtifacts:         {protocol: ProtocolHostArtifacts, schemes: []string{"http", "https"}, base: true},
	EndpointDownloads:         {protocol: ProtocolHostDownloads, schemes: []string{"http", "https"}, base: true},
}

func validateEndpoint(endpoint Endpoint) error {
	specification, known := endpointSpecifications[endpoint.Name]
	if !known {
		return fmt.Errorf("unknown endpoint name %q", endpoint.Name)
	}
	if endpoint.Protocol != specification.protocol {
		return fmt.Errorf(
			"endpoint %q protocol is %q, want %q",
			endpoint.Name, endpoint.Protocol, specification.protocol,
		)
	}
	if endpoint.URL == "" || endpoint.URL != strings.TrimSpace(endpoint.URL) ||
		len(endpoint.URL) > maximumEndpointURLBytes || strings.ContainsAny(endpoint.URL, "\x00\r\n") {
		return fmt.Errorf("endpoint %q has a non-canonical URL", endpoint.Name)
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.RawPath != "" ||
		(parsed.Path != "" && !strings.HasPrefix(parsed.Path, "/")) || parsed.String() != endpoint.URL {
		return fmt.Errorf("endpoint %q has an invalid credential-free absolute URL", endpoint.Name)
	}
	allowed := false
	for _, scheme := range specification.schemes {
		allowed = allowed || parsed.Scheme == scheme
	}
	if !allowed {
		return fmt.Errorf("endpoint %q uses unsupported URL scheme %q", endpoint.Name, parsed.Scheme)
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("endpoint %q URL contains a relative path segment", endpoint.Name)
		}
	}
	if specification.base && (parsed.RawQuery != "" || (len(parsed.Path) > 1 && strings.HasSuffix(parsed.Path, "/"))) {
		return fmt.Errorf("endpoint %q requires a query-free canonical base URL", endpoint.Name)
	}
	return nil
}
