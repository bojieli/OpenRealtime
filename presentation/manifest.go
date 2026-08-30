package presentation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/plugin"
)

const ManifestFormatVersion = 1

// ManifestImplementation attests the platform implementation selected for a
// client-plan entry. Artifact identity describes installed code, not the
// descriptor, whose exact identity is already in Plan.
type ManifestImplementation struct {
	Entry          string                   `json:"entry"`
	Implementation string                   `json:"implementation"`
	Artifact       inspect.ArtifactIdentity `json:"artifact"`
	Entrypoint     string                   `json:"entrypoint,omitempty"`
}

// ManifestAsset maps one descriptor-owned asset to its content-addressed host
// path. Paths carry no credential and are always same-origin relative URLs.
type ManifestAsset struct {
	Entry     string `json:"entry"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Path      string `json:"path"`
}

// ManifestEndpoint declares a host capability that actually exists in the
// selected host profile. Client code does not infer endpoints from UI names.
type ManifestEndpoint struct {
	Name          string `json:"name"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Protocol      string `json:"protocol,omitempty"`
	CatalogDigest string `json:"catalog_digest,omitempty"`
}

// ManifestGrant is the deployment-selected permission subset for one client
// entry. Descriptor permissions are ceilings and never become grants merely by
// appearing in the plan.
type ManifestGrant struct {
	Entry       string              `json:"entry"`
	Permissions []plugin.Permission `json:"permissions"`
}

// ClientManifest is the immutable browser/native boot input. Configuration
// values, secrets, tokens, and session payloads are deliberately absent.
type ClientManifest struct {
	FormatVersion   uint64                   `json:"format_version"`
	Platform        string                   `json:"platform"`
	Fingerprint     string                   `json:"fingerprint"`
	Plan            plugin.Plan              `json:"plan"`
	Implementations []ManifestImplementation `json:"implementations"`
	Assets          []ManifestAsset          `json:"assets,omitempty"`
	Endpoints       []ManifestEndpoint       `json:"endpoints,omitempty"`
	Grants          []ManifestGrant          `json:"grants,omitempty"`
}

// FreezeManifest canonicalizes declaration-order-insensitive rows and binds a
// digest to the complete client composition.
func FreezeManifest(manifest ClientManifest) (ClientManifest, error) {
	result := manifest.Clone()
	result.Fingerprint = ""
	result.canonicalize()
	if err := result.validateStructure(); err != nil {
		return ClientManifest{}, err
	}
	fingerprint, err := manifestDigest(result)
	if err != nil {
		return ClientManifest{}, err
	}
	result.Fingerprint = fingerprint
	return result, nil
}

// Validate checks structure, exact plan/asset coverage, and fingerprint.
func (manifest ClientManifest) Validate() error {
	want := manifest.Clone()
	want.Fingerprint = ""
	want.canonicalize()
	if err := want.validateStructure(); err != nil {
		return err
	}
	fingerprint, err := manifestDigest(want)
	if err != nil {
		return err
	}
	if manifest.Fingerprint != fingerprint {
		return fmt.Errorf("client manifest fingerprint is %q, want %q", manifest.Fingerprint, fingerprint)
	}
	return nil
}

func (manifest ClientManifest) validateStructure() error {
	if manifest.FormatVersion != ManifestFormatVersion {
		return fmt.Errorf("client manifest uses format %d, want %d",
			manifest.FormatVersion, ManifestFormatVersion)
	}
	if manifest.Platform == "" || manifest.Platform != strings.ToLower(strings.TrimSpace(manifest.Platform)) {
		return fmt.Errorf("client manifest has invalid platform %q", manifest.Platform)
	}
	if err := manifest.Plan.Validate(); err != nil {
		return fmt.Errorf("client manifest plan: %w", err)
	}
	if manifest.Plan.Realm != plugin.ClientRealm {
		return fmt.Errorf("client manifest plan has realm %s, want %s", manifest.Plan.Realm, plugin.ClientRealm)
	}
	entries := make(map[string]plugin.PlannedEntry, len(manifest.Plan.Entries))
	for _, entry := range manifest.Plan.Entries {
		entries[entry.Entry.ID] = entry
	}
	implementations := make(map[string]ManifestImplementation, len(manifest.Implementations))
	for _, implementation := range manifest.Implementations {
		if _, found := entries[implementation.Entry]; !found {
			return fmt.Errorf("client manifest implementation names absent entry %q", implementation.Entry)
		}
		if _, duplicate := implementations[implementation.Entry]; duplicate {
			return fmt.Errorf("client manifest repeats implementation for entry %q", implementation.Entry)
		}
		implementations[implementation.Entry] = implementation
		if implementation.Implementation == "" ||
			implementation.Implementation != strings.TrimSpace(implementation.Implementation) {
			return fmt.Errorf("client manifest entry %s has invalid implementation name", implementation.Entry)
		}
		if err := implementation.Artifact.Validate(); err != nil {
			return fmt.Errorf("client manifest entry %s artifact: %w", implementation.Entry, err)
		}
	}
	for entry := range entries {
		if _, found := implementations[entry]; !found {
			return fmt.Errorf("client manifest has no implementation for entry %q", entry)
		}
	}

	wantAssets := make(map[string]plugin.Asset)
	for entryID, entry := range entries {
		if !slices.Contains(entry.Descriptor.Platforms, manifest.Platform) &&
			!slices.Contains(entry.Descriptor.Platforms, "portable") {
			return fmt.Errorf("client manifest entry %s does not support platform %s",
				entryID, manifest.Platform)
		}
		for _, asset := range entry.Descriptor.Assets {
			wantAssets[entryID+"\x00"+asset.Name] = asset
		}
	}
	seenAssets := make(map[string]struct{}, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		key := asset.Entry + "\x00" + asset.Name
		want, found := wantAssets[key]
		if !found {
			return fmt.Errorf("client manifest asset %s/%s is not declared by its plugin",
				asset.Entry, asset.Name)
		}
		if _, duplicate := seenAssets[key]; duplicate {
			return fmt.Errorf("client manifest repeats asset %s/%s", asset.Entry, asset.Name)
		}
		seenAssets[key] = struct{}{}
		if asset.MediaType != want.MediaType || asset.Digest != want.Digest {
			return fmt.Errorf("client manifest asset %s/%s does not match its exact descriptor",
				asset.Entry, asset.Name)
		}
		if err := validateAssetPath(asset.Path, asset.Digest); err != nil {
			return fmt.Errorf("client manifest asset %s/%s: %w", asset.Entry, asset.Name, err)
		}
	}
	for key := range wantAssets {
		if _, found := seenAssets[key]; !found {
			return fmt.Errorf("client manifest omits descriptor asset %s", strings.ReplaceAll(key, "\x00", "/"))
		}
	}
	for entryID, implementation := range implementations {
		if manifest.Platform == "browser" && implementation.Entrypoint == "" {
			return fmt.Errorf("client manifest browser entry %s has no module entrypoint", entryID)
		}
		if implementation.Entrypoint == "" {
			continue
		}
		asset, found := wantAssets[entryID+"\x00"+implementation.Entrypoint]
		if !found {
			return fmt.Errorf("client manifest entry %s names undeclared entrypoint %q",
				entryID, implementation.Entrypoint)
		}
		if asset.MediaType != "text/javascript" && asset.MediaType != "application/javascript" {
			return fmt.Errorf("client manifest entry %s entrypoint %q is not JavaScript",
				entryID, implementation.Entrypoint)
		}
	}
	grantEntries := make(map[string]struct{}, len(manifest.Grants))
	for _, row := range manifest.Grants {
		entry, found := entries[row.Entry]
		if !found {
			return fmt.Errorf("client manifest grant names absent entry %q", row.Entry)
		}
		if _, duplicate := grantEntries[row.Entry]; duplicate {
			return fmt.Errorf("client manifest repeats grants for entry %q", row.Entry)
		}
		grantEntries[row.Entry] = struct{}{}
		seen := make(map[string]struct{}, len(row.Permissions))
		for _, grant := range row.Permissions {
			key := grant.Kind + "\x00" + grant.Resource
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("client manifest entry %s repeats permission %s/%s",
					row.Entry, grant.Kind, grant.Resource)
			}
			seen[key] = struct{}{}
			var ceiling *plugin.Permission
			for index := range entry.Descriptor.Permissions {
				candidate := &entry.Descriptor.Permissions[index]
				if candidate.Kind == grant.Kind && candidate.Resource == grant.Resource {
					ceiling = candidate
					break
				}
			}
			if ceiling == nil {
				return fmt.Errorf("client manifest entry %s permission %s/%s exceeds its descriptor ceiling",
					row.Entry, grant.Kind, grant.Resource)
			}
			if grant.Authority != ceiling.Authority {
				return fmt.Errorf("client manifest entry %s permission %s/%s changes authority",
					row.Entry, grant.Kind, grant.Resource)
			}
			if len(grant.Operations) == 0 {
				return fmt.Errorf("client manifest entry %s permission %s/%s has no operations",
					row.Entry, grant.Kind, grant.Resource)
			}
			operations := make(map[string]struct{}, len(grant.Operations))
			for _, operation := range grant.Operations {
				if _, duplicate := operations[operation]; operation == "" || duplicate ||
					!slices.Contains(ceiling.Operations, operation) {
					return fmt.Errorf("client manifest entry %s permission %s/%s operation %q exceeds its descriptor ceiling",
						row.Entry, grant.Kind, grant.Resource, operation)
				}
				operations[operation] = struct{}{}
			}
		}
	}

	endpoints := make(map[string]struct{}, len(manifest.Endpoints))
	for _, endpoint := range manifest.Endpoints {
		if endpoint.Name == "" || endpoint.Name != strings.TrimSpace(endpoint.Name) {
			return errors.New("client manifest has an invalid endpoint name")
		}
		if _, duplicate := endpoints[endpoint.Name]; duplicate {
			return fmt.Errorf("client manifest repeats endpoint %q", endpoint.Name)
		}
		endpoints[endpoint.Name] = struct{}{}
		if endpoint.Method != strings.ToUpper(endpoint.Method) || endpoint.Method == "" {
			return fmt.Errorf("client manifest endpoint %s has invalid method %q", endpoint.Name, endpoint.Method)
		}
		if _, ok := standardMethods[endpoint.Method]; !ok {
			return fmt.Errorf("client manifest endpoint %s has unsupported method %q", endpoint.Name, endpoint.Method)
		}
		if err := validateRelativePath(endpoint.Path); err != nil {
			return fmt.Errorf("client manifest endpoint %s: %w", endpoint.Name, err)
		}
		if endpoint.Protocol != "" {
			if endpoint.Protocol != strings.ToLower(strings.TrimSpace(endpoint.Protocol)) ||
				len(endpoint.Protocol) > 256 {
				return fmt.Errorf("client manifest endpoint %s has invalid protocol %q",
					endpoint.Name, endpoint.Protocol)
			}
			for _, character := range endpoint.Protocol {
				if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
					character == '.' || character == '-' || character == '_' {
					continue
				}
				return fmt.Errorf("client manifest endpoint %s has invalid protocol %q",
					endpoint.Name, endpoint.Protocol)
			}
		}
		if endpoint.CatalogDigest != "" {
			if endpoint.Protocol == "" {
				return fmt.Errorf("client manifest endpoint %s has a catalog digest without a protocol",
					endpoint.Name)
			}
			const prefix = "sha256:"
			hexadecimal := strings.TrimPrefix(endpoint.CatalogDigest, prefix)
			if !strings.HasPrefix(endpoint.CatalogDigest, prefix) || len(hexadecimal) != sha256.Size*2 ||
				hexadecimal != strings.ToLower(hexadecimal) {
				return fmt.Errorf("client manifest endpoint %s has invalid catalog digest",
					endpoint.Name)
			}
			if _, err := hex.DecodeString(hexadecimal); err != nil {
				return fmt.Errorf("client manifest endpoint %s has invalid catalog digest: %w",
					endpoint.Name, err)
			}
		}
	}
	return nil
}

var standardMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {},
}

func validateAssetPath(path, digest string) error {
	if err := validateRelativePath(path); err != nil {
		return err
	}
	want := "/client/v1/modules/" + strings.TrimPrefix(digest, "sha256:")
	if path != want {
		return fmt.Errorf("path is %q, want content address %q", path, want)
	}
	return nil
}

func validateRelativePath(path string) error {
	parsed, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		strings.Contains(path, "..") {
		return fmt.Errorf("invalid same-origin path %q", path)
	}
	return nil
}

func (manifest *ClientManifest) canonicalize() {
	sort.Slice(manifest.Implementations, func(left, right int) bool {
		return manifest.Implementations[left].Entry < manifest.Implementations[right].Entry
	})
	sort.Slice(manifest.Assets, func(left, right int) bool {
		if manifest.Assets[left].Entry != manifest.Assets[right].Entry {
			return manifest.Assets[left].Entry < manifest.Assets[right].Entry
		}
		return manifest.Assets[left].Name < manifest.Assets[right].Name
	})
	sort.Slice(manifest.Endpoints, func(left, right int) bool {
		return manifest.Endpoints[left].Name < manifest.Endpoints[right].Name
	})
	for rowIndex := range manifest.Grants {
		for permissionIndex := range manifest.Grants[rowIndex].Permissions {
			sort.Strings(manifest.Grants[rowIndex].Permissions[permissionIndex].Operations)
		}
		sort.Slice(manifest.Grants[rowIndex].Permissions, func(left, right int) bool {
			leftPermission := manifest.Grants[rowIndex].Permissions[left]
			rightPermission := manifest.Grants[rowIndex].Permissions[right]
			if leftPermission.Kind != rightPermission.Kind {
				return leftPermission.Kind < rightPermission.Kind
			}
			return leftPermission.Resource < rightPermission.Resource
		})
	}
	sort.Slice(manifest.Grants, func(left, right int) bool {
		return manifest.Grants[left].Entry < manifest.Grants[right].Entry
	})
}

func (manifest ClientManifest) Clone() ClientManifest {
	result := manifest
	result.Plan = manifest.Plan.Clone()
	result.Implementations = slices.Clone(manifest.Implementations)
	result.Assets = slices.Clone(manifest.Assets)
	result.Endpoints = slices.Clone(manifest.Endpoints)
	result.Grants = make([]ManifestGrant, len(manifest.Grants))
	for rowIndex := range manifest.Grants {
		result.Grants[rowIndex] = manifest.Grants[rowIndex]
		result.Grants[rowIndex].Permissions = make([]plugin.Permission, len(manifest.Grants[rowIndex].Permissions))
		for permissionIndex := range manifest.Grants[rowIndex].Permissions {
			result.Grants[rowIndex].Permissions[permissionIndex] = manifest.Grants[rowIndex].Permissions[permissionIndex]
			result.Grants[rowIndex].Permissions[permissionIndex].Operations =
				slices.Clone(manifest.Grants[rowIndex].Permissions[permissionIndex].Operations)
		}
	}
	return result
}

func manifestDigest(manifest ClientManifest) (string, error) {
	copy := manifest
	copy.Fingerprint = ""
	payload, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// MarshalManifest returns deterministic, newline-terminated strict JSON.
func MarshalManifest(manifest ClientManifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	canonical := manifest.Clone()
	canonical.canonicalize()
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// ParseManifest rejects duplicate/unknown fields and verifies all identities.
func ParseManifest(source []byte) (ClientManifest, error) {
	if err := strictjson.Validate(source); err != nil {
		return ClientManifest{}, fmt.Errorf("decode client manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var manifest ClientManifest
	if err := decoder.Decode(&manifest); err != nil {
		return ClientManifest{}, fmt.Errorf("decode client manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ClientManifest{}, errors.New("decode client manifest: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return ClientManifest{}, fmt.Errorf("decode client manifest trailing data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return ClientManifest{}, err
	}
	manifest.canonicalize()
	return manifest, nil
}
