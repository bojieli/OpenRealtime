// Package client implements the UI-independent OpenRealtime management API.
// It is the executable reference used by headless clients and by platform
// adapters: redirects are refused, capabilities never enter URLs, responses
// are bounded and strictly decoded, and returned identities are rebound to the
// exact request before they are exposed to a renderer or operator.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
)

const (
	defaultTimeout = 30 * time.Second
	maxJSONBytes   = 64 << 20
	maxTraceBytes  = inspect.MaxLiveTraceBytes
	// The exact schema is encoded as a JSON string. Every byte in an already
	// valid JSON artifact expands by at most two when quoted, plus bounded
	// envelope metadata.
	maxSchemaResourceBytes = 129 << 20
	maxAuthoringRequest    = 16 << 20
	maxReconcileRequest    = 64 << 20
)

// CapabilitySource supplies the narrow token for one operation and resource.
// Implementations may mint short-lived session tokens or select an operator
// capability; the client never caches the returned token.
type CapabilitySource interface {
	Capability(context.Context, management.Operation, string) (string, error)
}

type CapabilityFunc func(context.Context, management.Operation, string) (string, error)

func (function CapabilityFunc) Capability(
	ctx context.Context, operation management.Operation, resource string,
) (string, error) {
	if function == nil {
		return "", management.ErrUnauthorized
	}
	return function(ctx, operation, resource)
}

// StaticCapability is useful for a single narrowly scoped test or local
// process. Production callers should normally mint per-session capabilities.
type StaticCapability string

func (capability StaticCapability) Capability(
	_ context.Context, _ management.Operation, _ string,
) (string, error) {
	return string(capability), nil
}

type Config struct {
	BaseURL      string
	HTTPClient   *http.Client
	Capabilities CapabilitySource
	Timeout      time.Duration
}

// Client implements every management service over HTTP. A deployment can
// expose only a subset of route families; absent families return ErrNotFound.
type Client struct {
	base         url.URL
	http         *http.Client
	capabilities CapabilitySource
	timeout      time.Duration
}

func New(config Config) (*Client, error) {
	base, err := url.Parse(config.BaseURL)
	if err != nil || base == nil || !base.IsAbs() || base.Host == "" {
		return nil, fmt.Errorf("create management client: %w: base URL must be absolute", management.ErrInvalid)
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, fmt.Errorf("create management client: %w: base URL cannot contain credentials, path, query, or fragment", management.ErrInvalid)
	}
	switch base.Scheme {
	case "https":
	case "http":
		if !loopbackHost(base.Hostname()) {
			return nil, fmt.Errorf("create management client: %w: plaintext HTTP is allowed only on loopback", management.ErrInvalid)
		}
	default:
		return nil, fmt.Errorf("create management client: %w: URL scheme must be http or https", management.ErrInvalid)
	}
	if nilInterface(config.Capabilities) {
		return nil, fmt.Errorf("create management client: %w: capability source is required", management.ErrInvalid)
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 || timeout > 10*time.Minute {
		return nil, fmt.Errorf("create management client: %w: timeout is outside 1ns..10m", management.ErrInvalid)
	}
	transport := config.HTTPClient
	if transport == nil {
		transport = http.DefaultClient
	}
	copy := *transport
	copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	base.Path, base.RawPath = "", ""
	return &Client{base: *base, http: &copy, capabilities: config.Capabilities, timeout: timeout}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (client *Client) Graph(ctx context.Context, fingerprint string) (ir.Graph, error) {
	if !management.CanonicalDigest(fingerprint) {
		return ir.Graph{}, management.ErrInvalid
	}
	var graph ir.Graph
	err := client.get(ctx, []string{"graphs", fingerprint}, nil,
		management.ReadGraph, fingerprint, maxJSONBytes, &graph)
	if err == nil {
		if validateErr := graph.Validate(); validateErr != nil || graph.Fingerprint != fingerprint {
			err = fmt.Errorf("%w: server returned another or invalid graph", management.ErrConflict)
		}
	}
	return graph, err
}

func (client *Client) ElementDescriptor(
	ctx context.Context, identity element.Identity,
) (element.Descriptor, error) {
	if err := element.ValidateIdentity(identity); err != nil {
		return element.Descriptor{}, fmt.Errorf("%w: element identity: %v", management.ErrInvalid, err)
	}
	resource := "element:" + identity.Name + "@" + strconv.FormatUint(identity.Revision, 10) + ":" + identity.Digest
	var descriptor element.Descriptor
	err := client.get(ctx, []string{"descriptors", "elements", identity.Name,
		strconv.FormatUint(identity.Revision, 10), identity.Digest}, nil,
		management.ReadDescriptor, resource, maxJSONBytes, &descriptor)
	if err == nil {
		actual, identityErr := descriptor.Identity()
		if identityErr != nil || actual != identity {
			err = fmt.Errorf("%w: server returned another element descriptor", management.ErrConflict)
		}
	}
	return descriptor, err
}

func (client *Client) PluginDescriptor(
	ctx context.Context, identity plugin.Identity,
) (plugin.Descriptor, error) {
	if err := plugin.ValidateIdentity(identity); err != nil {
		return plugin.Descriptor{}, fmt.Errorf("%w: plugin identity: %v", management.ErrInvalid, err)
	}
	resource := "plugin:" + identity.Name + "@" + strconv.FormatUint(identity.Revision, 10) + ":" + identity.Digest
	var descriptor plugin.Descriptor
	err := client.get(ctx, []string{"descriptors", "plugins", identity.Name,
		strconv.FormatUint(identity.Revision, 10), identity.Digest}, nil,
		management.ReadDescriptor, resource, maxJSONBytes, &descriptor)
	if err == nil {
		actual, identityErr := descriptor.Identity()
		if identityErr != nil || actual != identity {
			err = fmt.Errorf("%w: server returned another plugin descriptor", management.ErrConflict)
		}
	}
	return descriptor, err
}

func (client *Client) ValuesSchema(ctx context.Context, fingerprint string) (schema.Bundle, error) {
	if !management.CanonicalDigest(fingerprint) {
		return schema.Bundle{}, management.ErrInvalid
	}
	var resource management.ValuesSchemaResource
	err := client.get(ctx, []string{"schemas", "values", fingerprint}, nil,
		management.ReadSchema, fingerprint, maxSchemaResourceBytes, &resource)
	if err != nil {
		return schema.Bundle{}, err
	}
	return resource.Bundle()
}

func (client *Client) Snapshot(ctx context.Context, session string) (inspect.Live, error) {
	if !management.CanonicalSessionID(session) {
		return inspect.Live{}, management.ErrInvalid
	}
	var snapshot inspect.Live
	err := client.get(ctx, []string{"sessions", session, "live"}, nil,
		management.ReadSession, session, maxJSONBytes, &snapshot)
	if err == nil {
		err = management.ValidateSessionSnapshot(snapshot)
	}
	if err != nil {
		return inspect.Live{}, err
	}
	return management.RedactLive(snapshot), nil
}

func (client *Client) Model(ctx context.Context, session string) (inspect.Model, error) {
	if !management.CanonicalSessionID(session) {
		return inspect.Model{}, management.ErrInvalid
	}
	var model inspect.Model
	err := client.get(ctx, []string{"sessions", session, "model"}, nil,
		management.ReadSession, session, maxJSONBytes, &model)
	if err == nil {
		err = management.ValidateInspectionModel(model)
	}
	return model, err
}

func (client *Client) Deltas(
	ctx context.Context, session string, after uint64, limit uint32,
) (management.DeltaPage, error) {
	if !management.CanonicalSessionID(session) || limit == 0 || limit > 4096 {
		return management.DeltaPage{}, management.ErrInvalid
	}
	query := url.Values{
		"after": {strconv.FormatUint(after, 10)}, "limit": {strconv.FormatUint(uint64(limit), 10)},
	}
	var page management.DeltaPage
	err := client.get(ctx, []string{"sessions", session, "deltas"}, query,
		management.ReadSession, session, maxTraceBytes, &page)
	if err == nil {
		err = management.ValidateDeltaPage(session, after, limit, page)
	}
	return page, err
}

func (client *Client) Trace(ctx context.Context, session string) (inspect.LiveTrace, error) {
	if !management.CanonicalSessionID(session) {
		return inspect.LiveTrace{}, management.ErrInvalid
	}
	var trace inspect.LiveTrace
	err := client.get(ctx, []string{"sessions", session, "trace"}, nil,
		management.ReadTrace, session, maxTraceBytes, &trace)
	if err == nil {
		err = trace.Validate()
		if err != nil {
			err = fmt.Errorf("%w: server returned an invalid live trace: %v", management.ErrConflict, err)
		}
	}
	return trace, err
}

func (client *Client) Analyze(
	ctx context.Context, document management.AuthoringDocument,
) (management.AnalysisResult, error) {
	if len(document.Source) > 1<<20 {
		return management.AnalysisResult{}, management.ErrInvalid
	}
	var result management.AnalysisResult
	err := client.post(ctx, []string{"authoring", "analyze"}, management.AnalyzeDocument,
		"authoring", document, maxAuthoringRequest, maxJSONBytes, &result)
	if err == nil {
		err = management.ValidateAnalysisResult(document, result)
	}
	return result, err
}

func (client *Client) Compile(
	ctx context.Context, document management.AuthoringDocument,
) (management.CompileResult, error) {
	if len(document.Source) > 1<<20 {
		return management.CompileResult{}, management.ErrInvalid
	}
	var result management.CompileResult
	err := client.post(ctx, []string{"authoring", "compile"}, management.CompileDocument,
		"authoring", document, maxAuthoringRequest, maxJSONBytes, &result)
	if err == nil {
		err = management.ValidateCompileResult(document, result)
	}
	return result, err
}

func (client *Client) Render(
	ctx context.Context, request management.RenderRequest,
) (management.RenderResult, error) {
	if err := request.Graph.Validate(); err != nil {
		return management.RenderResult{}, fmt.Errorf("%w: graph IR: %v", management.ErrInvalid, err)
	}
	var result management.RenderResult
	err := client.post(ctx, []string{"authoring", "render"}, management.RenderGraph,
		"authoring", request, maxAuthoringRequest, maxJSONBytes, &result)
	if err == nil && (result.Fingerprint != request.Graph.Fingerprint || result.Format != request.Format) {
		err = fmt.Errorf("%w: server rendered another graph", management.ErrConflict)
	}
	return result, err
}

func (client *Client) Publish(
	ctx context.Context, request management.SourceWriteRequest,
) (management.SourceWriteReceipt, error) {
	if err := management.ValidateSourceWriteRequest(request); err != nil {
		return management.SourceWriteReceipt{}, err
	}
	operation := management.CreateSource
	if request.Mode == management.SourceUpdate {
		operation = management.UpdateSource
	}
	var receipt management.SourceWriteReceipt
	err := client.post(ctx, []string{"authoring", "write"}, operation,
		request.RootIdentity, request, maxAuthoringRequest, maxJSONBytes, &receipt)
	if err == nil {
		err = management.ValidateSourceWriteReceipt(request, receipt)
	}
	return receipt, err
}

func (client *Client) Apply(
	ctx context.Context, request management.ReconciliationRequest,
) (management.ReconciliationReceipt, error) {
	if !management.CanonicalSessionID(request.SessionID) ||
		!management.CanonicalDigest(request.ExpectedFingerprint) ||
		!management.CanonicalDigest(request.ValuesFingerprint) ||
		!management.CanonicalDigest(request.DeploymentFingerprint) ||
		request.Candidate.Validate() != nil {
		return management.ReconciliationReceipt{}, management.ErrInvalid
	}
	var receipt management.ReconciliationReceipt
	err := client.post(ctx, []string{"reconciliations"}, management.ApplyCandidate,
		request.SessionID, request, maxReconcileRequest, maxJSONBytes, &receipt)
	if err == nil {
		err = management.ValidateReconciliationReceipt(request, receipt)
	}
	return receipt, err
}

func (client *Client) get(
	ctx context.Context, segments []string, query url.Values,
	operation management.Operation, resource string, maximum int64, destination any,
) error {
	return client.do(ctx, http.MethodGet, segments, query, operation, resource, nil, maximum, destination)
}

func (client *Client) post(
	ctx context.Context, segments []string, operation management.Operation,
	resource string, source any, bodyMaximum, responseMaximum int64, destination any,
) error {
	payload, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("%w: encode request: %v", management.ErrInvalid, err)
	}
	if int64(len(payload)) > bodyMaximum {
		return fmt.Errorf("%w: management request exceeds %d bytes", management.ErrInvalid, bodyMaximum)
	}
	return client.do(ctx, http.MethodPost, segments, nil, operation, resource,
		payload, responseMaximum, destination)
}

func (client *Client) do(
	ctx context.Context, method string, segments []string, query url.Values,
	operation management.Operation, resource string, body []byte, maximum int64, destination any,
) error {
	if client == nil || ctx == nil {
		return fmt.Errorf("%w: nil management client or context", management.ErrInvalid)
	}
	capability, err := client.capabilities.Capability(ctx, operation, resource)
	if err != nil {
		return fmt.Errorf("%w: acquire management capability", management.ErrUnauthorized)
	}
	if capability == "" || capability != strings.TrimSpace(capability) || strings.ContainsAny(capability, "\x00\r\n") {
		return management.ErrUnauthorized
	}
	endpoint := client.base
	path := management.APIPrefix
	for _, segment := range segments {
		if segment == "" || strings.Contains(segment, "/") {
			return fmt.Errorf("%w: invalid management resource segment", management.ErrInvalid)
		}
		path += "/" + segment
	}
	endpoint.Path = path
	endpoint.RawQuery = query.Encode()
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	var source io.Reader
	if body != nil {
		source = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestContext, method, endpoint.String(), source)
	if err != nil {
		return fmt.Errorf("%w: create management request", management.ErrInvalid)
	}
	request.Header.Set(management.CapabilityHeader, capability)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: management request: %w", management.ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return statusError(response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("%w: management response is not JSON", management.ErrConflict)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return fmt.Errorf("%w: read management response: %v", management.ErrUnavailable, err)
	}
	if int64(len(payload)) > maximum {
		return fmt.Errorf("%w: management response exceeds %d bytes", management.ErrConflict, maximum)
	}
	if err := strictjson.Validate(payload); err != nil {
		return fmt.Errorf("%w: management response is not strict JSON", management.ErrConflict)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: management response shape: %v", management.ErrConflict, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing management response", management.ErrConflict)
	}
	return nil
}

func statusError(status int) error {
	switch status {
	case http.StatusBadRequest:
		return management.ErrInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		return management.ErrUnauthorized
	case http.StatusNotFound:
		return management.ErrNotFound
	case http.StatusConflict:
		return management.ErrConflict
	case http.StatusServiceUnavailable:
		return management.ErrUnavailable
	default:
		return fmt.Errorf("%w: management endpoint returned HTTP %d", management.ErrUnavailable, status)
	}
}

var (
	_ management.StaticCatalog     = (*Client)(nil)
	_ management.SessionInspection = (*Client)(nil)
	_ management.Authoring         = (*Client)(nil)
	_ management.SourcePublication = (*Client)(nil)
	_ management.Reconciliation    = (*Client)(nil)
	_ CapabilitySource             = CapabilityFunc(nil)
	_ CapabilitySource             = StaticCapability("")
)
