package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const (
	managementOperation               = "management"
	maximumSessionManagementBodyBytes = 32 << 20
	maximumStaticManagementBodyBytes  = 64 << 20
	maximumSchemaManagementBodyBytes  = 129 << 20
	maximumAuthoringRequestBytes      = 16 << 20
	maximumAuthoringResponseBytes     = 64 << 20

	// ManagementIdentityHeader is non-secret response evidence produced only
	// after the host has rebound and validated an upstream resource. Browser
	// clients require it before exposing a static or authoring result.
	ManagementIdentityHeader = "OpenRealtime-Management-Identity"
)

// ManagementRelayFactory exposes only explicitly selected management API
// families. Session reads forward the short-lived capability negotiated by
// that session. Static catalog and authoring calls forward a separately
// supplied operator capability; the relay never mints one and never adds the
// deployment credential used by the realtime relay.
type ManagementRelayFactory struct {
	descriptor plugin.Descriptor
	client     *http.Client
	logger     *slog.Logger
}

func NewManagementRelayFactory(client *http.Client, logger *slog.Logger) *ManagementRelayFactory {
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ManagementRelayFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.presentation.host.management-relay", Revision: 8,
			Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
			Requires: []plugin.Requirement{
				{Contract: presentation.HTTPRoutesContract},
				{Contract: presentation.EndpointDirectoryContract},
			},
			Permissions: []plugin.Permission{{
				Kind: connectPermissionKind, Resource: connectPermissionResource,
				Operations: []string{managementOperation},
			}},
		},
		client: &copyClient, logger: logger,
	}
}

func (factory *ManagementRelayFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *ManagementRelayFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	if !mount.Permissions.Allows(connectPermissionKind, connectPermissionResource, managementOperation) {
		return errors.New("management relay lacks its deployment network-connect grant")
	}
	target, endpoint, err := lookupTargetEndpoint(
		mount.Services,
		presentation.EndpointManagement,
		presentation.ProtocolManagement,
	)
	if err != nil {
		return err
	}
	base, parseErr := url.Parse(endpoint.URL)
	if parseErr != nil {
		// EndpointDirectory validation already made this impossible. Keep the
		// mount fail-closed if a substituted service violates its Go contract.
		return errors.New("management relay received an invalid declared endpoint")
	}
	return registerRoutes(mount, []Route{
		{Pattern: "GET /client/v1/management/sessions/{session}/{resource}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				factory.relaySession(base, target, writer, request)
			},
		)},
		{Pattern: "GET /client/v1/management/graphs/{fingerprint}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				factory.relayGraph(base, target, writer, request)
			},
		)},
		{Pattern: "GET /client/v1/management/descriptors/elements/{name}/{revision}/{digest}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				factory.relayElementDescriptor(base, target, writer, request)
			},
		)},
		{Pattern: "GET /client/v1/management/descriptors/plugins/{name}/{revision}/{digest}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				factory.relayPluginDescriptor(base, target, writer, request)
			},
		)},
		{Pattern: "GET /client/v1/management/schemas/values/{fingerprint}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				factory.relayValuesSchema(base, target, writer, request)
			},
		)},
		{Pattern: "POST /client/v1/management/authoring/{action}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				factory.relayAuthoring(base, target, writer, request)
			},
		)},
	})
}

func (factory *ManagementRelayFactory) relaySession(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	session := request.PathValue("session")
	resource := request.PathValue("resource")
	if !management.CanonicalSessionID(session) ||
		(resource != "live" && resource != "model" && resource != "deltas" && resource != "trace") {
		http.NotFound(writer, request)
		return
	}
	query, err := boundedManagementQuery(request.URL.Query())
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	responseMaximum := int64(maximumSessionManagementBodyBytes)
	if resource == "model" {
		responseMaximum = int64(maximumStaticManagementBodyBytes)
	}
	factory.relayRequest(base, target, writer, request, relayRequest{
		method: http.MethodGet,
		path:   "/sessions/" + session + "/" + resource,
		query:  query, responseMaximum: responseMaximum,
	})
}

// relay retains the narrow session-resource helper used by compatibility
// tests and downstream embedders while the mounted route also exposes the new
// static and authoring families through their own whitelisted handlers.
func (factory *ManagementRelayFactory) relay(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	factory.relaySession(base, target, writer, request)
}

func (factory *ManagementRelayFactory) relayGraph(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	fingerprint := request.PathValue("fingerprint")
	if !management.CanonicalDigest(fingerprint) || len(request.URL.Query()) != 0 {
		http.Error(writer, "invalid graph management resource", http.StatusBadRequest)
		return
	}
	factory.relayRequest(base, target, writer, request, relayRequest{
		method: http.MethodGet, path: "/graphs/" + fingerprint,
		responseMaximum: maximumStaticManagementBodyBytes,
		validate: func(payload []byte) (string, error) {
			var graph ir.Graph
			if err := decodeRelayJSON(payload, &graph); err != nil {
				return "", err
			}
			if err := graph.Validate(); err != nil || graph.Fingerprint != fingerprint {
				return "", errors.New("management graph response changed immutable identity")
			}
			return "graph:" + fingerprint, nil
		},
	})
}

func (factory *ManagementRelayFactory) relayElementDescriptor(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	identity, identityText, ok := relayElementIdentity(request)
	if !ok {
		http.Error(writer, "invalid element descriptor identity", http.StatusBadRequest)
		return
	}
	factory.relayRequest(base, target, writer, request, relayRequest{
		method: http.MethodGet,
		path: "/descriptors/elements/" + identity.Name + "/" +
			strconv.FormatUint(identity.Revision, 10) + "/" + identity.Digest,
		responseMaximum: maximumStaticManagementBodyBytes,
		validate: func(payload []byte) (string, error) {
			var descriptor element.Descriptor
			if err := decodeRelayJSON(payload, &descriptor); err != nil {
				return "", err
			}
			actual, err := descriptor.Identity()
			if err != nil || actual != identity {
				return "", errors.New("management element descriptor changed immutable identity")
			}
			return "element:" + identityText, nil
		},
	})
}

func (factory *ManagementRelayFactory) relayPluginDescriptor(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	identity, identityText, ok := relayPluginIdentity(request)
	if !ok {
		http.Error(writer, "invalid plugin descriptor identity", http.StatusBadRequest)
		return
	}
	factory.relayRequest(base, target, writer, request, relayRequest{
		method: http.MethodGet,
		path: "/descriptors/plugins/" + identity.Name + "/" +
			strconv.FormatUint(identity.Revision, 10) + "/" + identity.Digest,
		responseMaximum: maximumStaticManagementBodyBytes,
		validate: func(payload []byte) (string, error) {
			var descriptor plugin.Descriptor
			if err := decodeRelayJSON(payload, &descriptor); err != nil {
				return "", err
			}
			actual, err := descriptor.Identity()
			if err != nil || actual != identity {
				return "", errors.New("management plugin descriptor changed immutable identity")
			}
			return "plugin:" + identityText, nil
		},
	})
}

func (factory *ManagementRelayFactory) relayValuesSchema(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	fingerprint := request.PathValue("fingerprint")
	if !management.CanonicalDigest(fingerprint) || len(request.URL.Query()) != 0 {
		http.Error(writer, "invalid values-schema identity", http.StatusBadRequest)
		return
	}
	factory.relayRequest(base, target, writer, request, relayRequest{
		method:          http.MethodGet,
		path:            "/schemas/values/" + fingerprint,
		responseMaximum: maximumSchemaManagementBodyBytes,
		validate: func(payload []byte) (string, error) {
			var resource management.ValuesSchemaResource
			if err := decodeRelayJSON(payload, &resource); err != nil {
				return "", err
			}
			if _, err := resource.Bundle(); err != nil {
				return "", errors.New("management values schema changed immutable identity")
			}
			return "schema:" + fingerprint + ":" + resource.Digest, nil
		},
	})
}

func (factory *ManagementRelayFactory) relayAuthoring(
	base *url.URL, target relayTarget, writer http.ResponseWriter, request *http.Request,
) {
	action := request.PathValue("action")
	if action != "analyze" && action != "rename" && action != "remove-edge" && action != "compile" && action != "render" &&
		action != "read" && action != "write" {
		http.NotFound(writer, request)
		return
	}
	if len(request.URL.Query()) != 0 {
		http.Error(writer, "authoring requests do not accept query parameters", http.StatusBadRequest)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(writer, "authoring request must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if request.ContentLength > maximumAuthoringRequestBytes {
		http.Error(writer, "authoring request exceeds limit", http.StatusRequestEntityTooLarge)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, maximumAuthoringRequestBytes+1))
	if err != nil || len(payload) > maximumAuthoringRequestBytes || strictjson.Validate(payload) != nil {
		http.Error(writer, "authoring request is not bounded strict JSON", http.StatusBadRequest)
		return
	}
	spec := relayRequest{
		method: http.MethodPost, path: "/authoring/" + action,
		body: payload, responseMaximum: maximumAuthoringResponseBytes,
	}
	switch action {
	case "analyze":
		var document management.AuthoringDocument
		if err := decodeRelayJSON(payload, &document); err != nil {
			http.Error(writer, "invalid authoring document", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var result management.AnalysisResult
			if err := decodeRelayJSON(response, &result); err != nil {
				return "", err
			}
			if err := management.ValidateAnalysisResult(document, result); err != nil {
				return "", err
			}
			return "authoring:analyze:" + result.SourceDigest, nil
		}
	case "rename":
		var input management.RenameDocumentRequest
		if err := decodeRelayJSON(payload, &input); err != nil ||
			management.ValidateRenameDocumentRequest(input) != nil {
			http.Error(writer, "invalid authoring rename request", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var result management.RenameDocumentResult
			if err := decodeRelayJSON(response, &result); err != nil {
				return "", err
			}
			if err := management.ValidateRenameDocumentResult(input, result); err != nil {
				return "", err
			}
			return "authoring:rename:" + result.Edits.SourceDigest, nil
		}
	case "remove-edge":
		var input management.RemoveDocumentEdgeRequest
		if err := decodeRelayJSON(payload, &input); err != nil ||
			management.ValidateRemoveDocumentEdgeRequest(input) != nil {
			http.Error(writer, "invalid authoring edge-removal request", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var result management.RemoveDocumentEdgeResult
			if err := decodeRelayJSON(response, &result); err != nil {
				return "", err
			}
			if err := management.ValidateRemoveDocumentEdgeResult(input, result); err != nil {
				return "", err
			}
			return "authoring:edge.remove:" + result.Edits.SourceDigest, nil
		}
	case "compile":
		var document management.AuthoringDocument
		if err := decodeRelayJSON(payload, &document); err != nil {
			http.Error(writer, "invalid authoring document", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var result management.CompileResult
			if err := decodeRelayJSON(response, &result); err != nil {
				return "", err
			}
			if err := management.ValidateCompileResult(document, result); err != nil {
				return "", err
			}
			return "authoring:compile:" + result.Graph.Fingerprint, nil
		}
	case "render":
		var input management.RenderRequest
		if err := decodeRelayJSON(payload, &input); err != nil || input.Graph.Validate() != nil ||
			(input.Format != management.RenderModel && input.Format != management.RenderMermaid &&
				input.Format != management.RenderDOT) {
			http.Error(writer, "invalid graph rendering request", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var result management.RenderResult
			if err := decodeRelayJSON(response, &result); err != nil {
				return "", err
			}
			if err := validateRelayRender(input, result); err != nil {
				return "", err
			}
			return "authoring:render:" + string(result.Format) + ":" + result.Fingerprint, nil
		}
	case "read":
		var input management.SourceReadRequest
		if err := decodeRelayJSON(payload, &input); err != nil || management.ValidateSourceReadRequest(input) != nil {
			http.Error(writer, "invalid source-read request", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var result management.SourceReadResult
			if err := decodeRelayJSON(response, &result); err != nil {
				return "", err
			}
			if err := management.ValidateSourceReadResult(input, result); err != nil {
				return "", err
			}
			return "authoring:read:" + result.ResultDigest, nil
		}
	case "write":
		var input management.SourceWriteRequest
		if err := decodeRelayJSON(payload, &input); err != nil || management.ValidateSourceWriteRequest(input) != nil {
			http.Error(writer, "invalid source publication request", http.StatusBadRequest)
			return
		}
		spec.validate = func(response []byte) (string, error) {
			var receipt management.SourceWriteReceipt
			if err := decodeRelayJSON(response, &receipt); err != nil {
				return "", err
			}
			if err := management.ValidateSourceWriteReceipt(input, receipt); err != nil {
				return "", err
			}
			return "authoring:write:" + receipt.ReceiptDigest, nil
		}
	}
	factory.relayRequest(base, target, writer, request, spec)
}

func validateRelayRender(input management.RenderRequest, result management.RenderResult) error {
	if result.Fingerprint != input.Graph.Fingerprint || result.Format != input.Format {
		return errors.New("management renderer changed immutable graph identity")
	}
	switch input.Format {
	case management.RenderModel:
		expected, err := inspect.Build(input.Graph)
		if err != nil || result.Model == nil || result.Text != "" {
			return errors.New("management renderer returned a mismatched model")
		}
		actualJSON, actualErr := json.Marshal(result.Model)
		expectedJSON, expectedErr := json.Marshal(expected)
		if actualErr != nil || expectedErr != nil || !bytes.Equal(actualJSON, expectedJSON) {
			return errors.New("management renderer returned a mismatched model")
		}
	case management.RenderMermaid:
		expected, err := inspect.Mermaid(input.Graph)
		if err != nil || result.Model != nil || result.Text != expected {
			return errors.New("management renderer returned mismatched Mermaid text")
		}
	case management.RenderDOT:
		expected, err := inspect.DOT(input.Graph)
		if err != nil || result.Model != nil || result.Text != expected {
			return errors.New("management renderer returned mismatched DOT text")
		}
	default:
		return errors.New("management renderer returned an unsupported format")
	}
	return nil
}

type relayRequest struct {
	method          string
	path            string
	query           url.Values
	body            []byte
	responseMaximum int64
	validate        func([]byte) (string, error)
}

func (factory *ManagementRelayFactory) relayRequest(
	base *url.URL, target relayTarget, writer http.ResponseWriter,
	request *http.Request, spec relayRequest,
) {
	capability, ok := managementCapability(request)
	if !ok {
		http.Error(writer, "one canonical management capability is required", http.StatusUnauthorized)
		return
	}
	destination := *base
	destination.Path = strings.TrimSuffix(base.Path, "/") + "/" + strings.TrimPrefix(spec.path, "/")
	destination.RawPath = ""
	destination.RawQuery = ""
	if spec.query != nil {
		destination.RawQuery = spec.query.Encode()
	}
	ctx, cancel := context.WithTimeout(request.Context(), target.DialTimeout)
	defer cancel()
	var source io.Reader
	if spec.body != nil {
		source = bytes.NewReader(spec.body)
	}
	proxied, err := http.NewRequestWithContext(ctx, spec.method, destination.String(), source)
	if err != nil {
		http.Error(writer, "invalid management target", http.StatusInternalServerError)
		return
	}
	proxied.Header.Set(management.CapabilityHeader, capability)
	proxied.Header.Set("Accept", "application/json")
	if spec.body != nil {
		proxied.Header.Set("Content-Type", "application/json")
	}
	response, err := factory.client.Do(proxied)
	if err != nil {
		// A custom transport error can contain request details. The operator
		// capability is deliberately not made available to the log sink.
		factory.logger.Warn("presentation management relay request failed")
		http.Error(writer, "management endpoint unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.ContentLength > spec.responseMaximum {
		http.Error(writer, "management endpoint returned an invalid bounded response", http.StatusBadGateway)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, spec.responseMaximum+1))
	if err != nil || int64(len(payload)) > spec.responseMaximum {
		http.Error(writer, "management endpoint returned an invalid bounded response", http.StatusBadGateway)
		return
	}
	identity := ""
	if response.StatusCode == http.StatusOK && spec.validate != nil {
		mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if mediaErr != nil || mediaType != "application/json" {
			http.Error(writer, "management endpoint returned an invalid response type", http.StatusBadGateway)
			return
		}
		identity, err = spec.validate(payload)
		if err != nil {
			// Validation errors may quote attacker-controlled response fields.
			// Keep the diagnostic category observable without logging payload data.
			factory.logger.Warn("presentation management relay rejected an identity mismatch")
			http.Error(writer, "management endpoint returned a mismatched response", http.StatusBadGateway)
			return
		}
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	if etag := response.Header.Get("ETag"); etag != "" {
		writer.Header().Set("ETag", etag)
	}
	if identity != "" {
		writer.Header().Set(ManagementIdentityHeader, identity)
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(payload)
}

func managementCapability(request *http.Request) (string, bool) {
	values := request.Header.Values(management.CapabilityHeader)
	if len(values) != 1 || values[0] == "" || values[0] != strings.TrimSpace(values[0]) ||
		strings.ContainsAny(values[0], "\x00\r\n") || len(values[0]) > 512 {
		return "", false
	}
	return values[0], true
}

func relayElementIdentity(request *http.Request) (element.Identity, string, bool) {
	revision, err := strconv.ParseUint(request.PathValue("revision"), 10, 64)
	identity := element.Identity{
		Name: request.PathValue("name"), Revision: revision, Digest: request.PathValue("digest"),
	}
	if err != nil || element.ValidateIdentity(identity) != nil || len(request.URL.Query()) != 0 {
		return element.Identity{}, "", false
	}
	text := identity.Name + "@" + strconv.FormatUint(identity.Revision, 10) + ":" + identity.Digest
	return identity, text, true
}

func relayPluginIdentity(request *http.Request) (plugin.Identity, string, bool) {
	revision, err := strconv.ParseUint(request.PathValue("revision"), 10, 64)
	identity := plugin.Identity{
		Name: request.PathValue("name"), Revision: revision, Digest: request.PathValue("digest"),
	}
	if err != nil || plugin.ValidateIdentity(identity) != nil || len(request.URL.Query()) != 0 {
		return plugin.Identity{}, "", false
	}
	text := identity.Name + "@" + strconv.FormatUint(identity.Revision, 10) + ":" + identity.Digest
	return identity, text, true
}

func decodeRelayJSON(payload []byte, destination any) error {
	if err := strictjson.Validate(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing management JSON value")
		}
		return err
	}
	return nil
}

func boundedManagementQuery(input url.Values) (url.Values, error) {
	result := make(url.Values)
	for name, values := range input {
		if (name != "after" && name != "limit") || len(values) != 1 {
			return nil, errors.New("management query accepts one after and one limit value")
		}
		bits := 64
		if name == "limit" {
			bits = 32
		}
		value, err := strconv.ParseUint(values[0], 10, bits)
		if err != nil || (name == "limit" && (value == 0 || value > 4096)) {
			return nil, fmt.Errorf("management query %s is out of bounds", name)
		}
		result.Set(name, strconv.FormatUint(value, 10))
	}
	return result, nil
}

var _ pluginruntime.Factory = (*ManagementRelayFactory)(nil)
