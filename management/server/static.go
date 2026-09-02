package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type StaticAPIFactory struct{ descriptor plugin.Descriptor }

func NewStaticAPIFactory() *StaticAPIFactory {
	return &StaticAPIFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.static-api", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: management.HTTPRoutesContract},
			{Contract: management.AuthorizerContract},
			{Contract: management.StaticCatalogContract},
		},
	}}
}

func (factory *StaticAPIFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *StaticAPIFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	authorizer, err := lookupService[management.Authorizer](mount.Services, management.AuthorizerContract)
	if err != nil {
		return err
	}
	catalog, err := lookupService[management.StaticCatalog](mount.Services, management.StaticCatalogContract)
	if err != nil {
		return err
	}
	return registerRoutes(mount, []Route{
		{Pattern: "GET " + management.APIPrefix + "/graphs/{fingerprint}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				fingerprint := request.PathValue("fingerprint")
				if err := authorize(request, authorizer, management.ReadGraph, fingerprint); err != nil {
					writeServiceError(writer, err)
					return
				}
				graph, err := catalog.Graph(request.Context(), fingerprint)
				if err == nil {
					err = graph.Validate()
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, graph)
			},
		)},
		{Pattern: "GET " + management.APIPrefix + "/descriptors/elements/{name}/{revision}/{digest}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				resource := request.PathValue("name") + "@" + request.PathValue("revision") + ":" + request.PathValue("digest")
				if err := authorize(request, authorizer, management.ReadDescriptor, "element:"+resource); err != nil {
					writeServiceError(writer, err)
					return
				}
				identity, err := elementIdentity(request)
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				descriptor, err := catalog.ElementDescriptor(request.Context(), identity)
				if err == nil {
					live, identityErr := descriptor.Identity()
					if identityErr != nil || live != identity {
						err = fmt.Errorf("%w: element catalog returned another identity", management.ErrConflict)
					}
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, descriptor)
			},
		)},
		{Pattern: "GET " + management.APIPrefix + "/descriptors/plugins/{name}/{revision}/{digest}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				resource := request.PathValue("name") + "@" + request.PathValue("revision") + ":" + request.PathValue("digest")
				if err := authorize(request, authorizer, management.ReadDescriptor, "plugin:"+resource); err != nil {
					writeServiceError(writer, err)
					return
				}
				identity, err := pluginIdentity(request)
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				descriptor, err := catalog.PluginDescriptor(request.Context(), identity)
				if err == nil {
					live, identityErr := descriptor.Identity()
					if identityErr != nil || live != identity {
						err = fmt.Errorf("%w: plugin catalog returned another identity", management.ErrConflict)
					}
				}
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, descriptor)
			},
		)},
		{Pattern: "GET " + management.APIPrefix + "/schemas/values/{fingerprint}", Handler: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				if err := validateQuery(request); err != nil {
					writeServiceError(writer, err)
					return
				}
				fingerprint := request.PathValue("fingerprint")
				if err := authorize(request, authorizer, management.ReadSchema, fingerprint); err != nil {
					writeServiceError(writer, err)
					return
				}
				bundle, err := catalog.ValuesSchema(request.Context(), fingerprint)
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				resource, err := management.NewValuesSchemaResource(bundle)
				if err != nil {
					writeServiceError(writer, err)
					return
				}
				writeJSON(writer, http.StatusOK, resource)
			},
		)},
	})
}

func (factory *StaticAPIFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return prepareAPIRouteCandidate(candidate.Services, factory.Mount, func(services pluginruntime.Services) error {
		if _, err := lookupService[management.Authorizer](services, management.AuthorizerContract); err != nil {
			return err
		}
		_, err := lookupService[management.StaticCatalog](services, management.StaticCatalogContract)
		return err
	})
}

var _ pluginruntime.CandidatePreMounter = (*StaticAPIFactory)(nil)

func elementIdentity(request *http.Request) (element.Identity, error) {
	revision, err := strconv.ParseUint(request.PathValue("revision"), 10, 64)
	if err != nil {
		return element.Identity{}, fmt.Errorf("%w: element revision", management.ErrInvalid)
	}
	identity := element.Identity{
		Name: request.PathValue("name"), Revision: revision, Digest: request.PathValue("digest"),
	}
	if err := element.ValidateIdentity(identity); err != nil {
		return element.Identity{}, fmt.Errorf("%w: element identity: %v", management.ErrInvalid, err)
	}
	return identity, nil
}

func pluginIdentity(request *http.Request) (plugin.Identity, error) {
	revision, err := strconv.ParseUint(request.PathValue("revision"), 10, 64)
	if err != nil {
		return plugin.Identity{}, fmt.Errorf("%w: plugin revision", management.ErrInvalid)
	}
	identity := plugin.Identity{
		Name: request.PathValue("name"), Revision: revision, Digest: request.PathValue("digest"),
	}
	if err := plugin.ValidateIdentity(identity); err != nil {
		return plugin.Identity{}, fmt.Errorf("%w: plugin identity: %v", management.ErrInvalid, err)
	}
	return identity, nil
}
