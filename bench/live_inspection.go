package bench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

const maxLiveInspectionBytes = 8 << 20

// LiveInspectionClient reads the authenticated, session-scoped management
// snapshot negotiated on the Realtime connection. Endpoint supplies only the
// deployment origin; the server-issued access path selects the exact session.
// Scope is never used as a session lookup key.
type LiveInspectionClient struct {
	Endpoint        string
	DeploymentToken string
	HTTPClient      *http.Client
	Now             func() time.Time
}

// Resolver returns the strict live-resolution callback consumed by
// GraphAttestor. Construction validates and freezes all reviewed inputs so a
// caller cannot mutate the expected treatment after wiring the attestor.
func (client LiveInspectionClient) Resolver(
	graph ir.Graph, configuration ArtifactIdentity, expected LiveResolution,
) (func(context.Context, AttestationRequest) (LiveResolution, error), error) {
	if err := graph.Validate(); err != nil {
		return nil, fmt.Errorf("configure live inspection resolver: graph: %w", err)
	}
	if _, err := buildGraphEvidence(graph, configuration, expected); err != nil {
		return nil, fmt.Errorf("configure live inspection resolver: %w", err)
	}
	frozen, err := ir.Freeze(graph)
	if err != nil {
		return nil, fmt.Errorf("configure live inspection resolver: freeze graph: %w", err)
	}
	expected = canonicalResolution(expected)
	return func(ctx context.Context, request AttestationRequest) (LiveResolution, error) {
		if request.Inspection == nil {
			return LiveResolution{}, errors.New(
				"the session did not negotiate an authenticated live inspection capability",
			)
		}
		snapshot, err := client.Snapshot(ctx, *request.Inspection)
		if err != nil {
			return LiveResolution{}, err
		}
		return ResolutionFromInspection(frozen, configuration, expected, snapshot)
	}, nil
}

// Snapshot retrieves one payload-free live view. Credentials are sent only to
// the deployment origin and redirects are never followed.
func (client LiveInspectionClient) Snapshot(
	ctx context.Context, access openrealtime.InspectionAccess,
) (inspect.Live, error) {
	if ctx == nil {
		return inspect.Live{}, errors.New("read live inspection: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return inspect.Live{}, err
	}
	now := time.Now
	if client.Now != nil {
		now = client.Now
	}
	if err := validateInspectionAccess(access, now()); err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: %w", err)
	}
	target, err := inspectionTarget(client.Endpoint, access.Path)
	if err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: %w", err)
	}
	request.Header.Set(openrealtime.InspectionTokenHeader, access.Token)
	if token := strings.TrimSpace(client.DeploymentToken); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	httpClient := http.DefaultClient
	if client.HTTPClient != nil {
		httpClient = client.HTTPClient
	}
	bounded := &http.Client{
		Transport: httpClient.Transport,
		Timeout:   httpClient.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := bounded.Do(request)
	if err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return inspect.Live{}, fmt.Errorf("read live inspection: management endpoint returned %s",
			response.Status)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return inspect.Live{}, fmt.Errorf("read live inspection: response content type is %q",
			response.Header.Get("Content-Type"))
	}
	if !hasCacheControlDirective(response.Header.Values("Cache-Control"), "no-store") {
		return inspect.Live{}, errors.New("read live inspection: response is not marked no-store")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxLiveInspectionBytes+1))
	if err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: response body: %w", err)
	}
	if len(payload) > maxLiveInspectionBytes {
		return inspect.Live{}, fmt.Errorf("read live inspection: response exceeds %d bytes",
			maxLiveInspectionBytes)
	}
	if err := strictjson.Validate(payload); err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: strict JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var snapshot inspect.Live
	if err := decoder.Decode(&snapshot); err != nil {
		return inspect.Live{}, fmt.Errorf("read live inspection: decode: %w", err)
	}
	return snapshot.Clone(), nil
}

func hasCacheControlDirective(headers []string, required string) bool {
	required = strings.ToLower(strings.TrimSpace(required))
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			directive, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if strings.ToLower(strings.TrimSpace(directive)) == required {
				return true
			}
		}
	}
	return false
}

func validateInspectionAccess(access openrealtime.InspectionAccess, now time.Time) error {
	if access.SessionID == "" || access.SessionID != strings.TrimSpace(access.SessionID) ||
		len(access.SessionID) > 512 || strings.ContainsAny(access.SessionID, "\x00\r\n") {
		return errors.New("inspection access has an invalid session ID")
	}
	expectedPath := "/v1/realtime/sessions/" + url.PathEscape(access.SessionID) + "/live"
	if access.Path != expectedPath {
		return errors.New("inspection access path is not bound to its session ID")
	}
	if !strings.HasPrefix(access.Token, "mgmt_") || len(access.Token) > 512 ||
		strings.ContainsAny(access.Token, "\x00\r\n") {
		return errors.New("inspection access has an invalid capability")
	}
	encoded := strings.TrimPrefix(access.Token, "mgmt_")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return errors.New("inspection access has an invalid capability")
	}
	if access.ExpiresAtMS <= 0 || !now.Before(time.UnixMilli(access.ExpiresAtMS)) {
		return errors.New("inspection access has expired")
	}
	return nil
}

func inspectionTarget(endpoint, accessPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	if parsed.User != nil || parsed.Host == "" {
		return "", errors.New("Realtime endpoint has no credential-free deployment origin")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "wss", "https":
		parsed.Scheme = "https"
	case "ws", "http":
		if !loopbackHost(parsed.Hostname()) {
			return "", errors.New("live inspection requires TLS outside loopback")
		}
		parsed.Scheme = "http"
	default:
		return "", fmt.Errorf("unsupported Realtime endpoint scheme %q", parsed.Scheme)
	}
	parsed.Path = accessPath
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

func loopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
