// Package profile resolves a strict, immutable deployment profile through an
// explicitly installed graph-application plugin. The profile contains data
// and exact identities only; factories remain host registrations and graph
// construction remains resource-free until the selected SessionProvider is
// started.
package profile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/strictyaml"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"gopkg.in/yaml.v3"
)

const (
	FormatVersion             uint64 = 1
	maximumConfigurationBytes        = 4 << 20
	maximumProfileBytes              = 8 << 20
	maximumApplicationPlugins        = 65_536
	maximumCanonicalTextBytes        = 64 << 10
)

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Selection pins one host-installed plugin registration. A symbolic
// reference without Artifact is deliberately insufficient: it would allow
// mutable installed code to retain the same deployment identity.
type Selection struct {
	Reference string                   `json:"reference" yaml:"reference"`
	Artifact  inspect.ArtifactIdentity `json:"artifact" yaml:"artifact"`
}

// Application selects the graph-application plugin and supplies its strict,
// plugin-owned configuration as canonical JSON. ParseYAML accepts a JSON-like
// YAML mapping, rejects ambiguous YAML features, and converts it to this form
// before the plugin sees it.
type Application struct {
	Selection
	Configuration json.RawMessage `json:"configuration" yaml:"-"`
}

// AdapterSelection repeats the exact public projection expected from the
// application plugin. Resolve checks this before graph/launch invokes any
// adapter binder.
type AdapterSelection struct {
	Reference       string                   `json:"reference" yaml:"reference"`
	RuntimeArtifact inspect.ArtifactIdentity `json:"runtime_artifact" yaml:"runtime_artifact"`
	ProfileName     string                   `json:"profile_name" yaml:"profile_name"`
	ProfileRevision uint64                   `json:"profile_revision" yaml:"profile_revision"`
}

func (selection AdapterSelection) launchSelection() graphlaunch.AdapterSelection {
	return graphlaunch.AdapterSelection{
		Reference: selection.Reference, RuntimeArtifact: selection.RuntimeArtifact,
		ProfileName: selection.ProfileName, ProfileRevision: selection.ProfileRevision,
	}
}

// Server declares the stable OpenAI-compatible protocol realm. It contains no
// listener or presentation setting: browser/native presentation hosts remain
// independent clients of the resulting Realtime and management APIs.
type Server struct {
	ProfileName     string `json:"profile_name" yaml:"profile_name"`
	ProfileRevision uint64 `json:"profile_revision" yaml:"profile_revision"`

	ProviderArtifact inspect.ArtifactIdentity `json:"provider_artifact" yaml:"provider_artifact"`
	GatewayArtifact  inspect.ArtifactIdentity `json:"gateway_artifact" yaml:"gateway_artifact"`

	Model                string              `json:"model" yaml:"model"`
	TranscriptionModel   string              `json:"transcription_model" yaml:"transcription_model"`
	TokenEnvironment     string              `json:"token_environment,omitempty" yaml:"token_environment,omitempty"`
	ValidateWire         bool                `json:"validate_wire" yaml:"validate_wire"`
	InspectionTokenTTLMS uint64              `json:"inspection_token_ttl_ms" yaml:"inspection_token_ttl_ms"`
	MaxAudioFrameBytes   int                 `json:"max_audio_frame_bytes" yaml:"max_audio_frame_bytes"`
	VideoLimits          openrealtime.Limits `json:"video_limits" yaml:"video_limits"`
}

// Document is one immutable graph/server launch intent. Plan records the
// complete source-to-resolution identity rather than only a graph name, so a
// values, deployment, lock, or selected-runtime change fails closed.
type Document struct {
	FormatVersion uint64               `json:"format_version" yaml:"format_version"`
	Name          string               `json:"name" yaml:"name"`
	Revision      uint64               `json:"revision" yaml:"revision"`
	Fingerprint   string               `json:"fingerprint" yaml:"fingerprint"`
	Application   Application          `json:"application" yaml:"-"`
	Plan          graphconfig.Identity `json:"plan" yaml:"plan"`
	Adapter       AdapterSelection     `json:"adapter" yaml:"adapter"`
	Server        Server               `json:"server" yaml:"server"`
}

// Clone returns an independently owned document.
func (document Document) Clone() Document {
	document.Application.Configuration = bytes.Clone(document.Application.Configuration)
	return document
}

// Freeze validates a document, canonicalizes its plugin configuration, and
// binds its complete content to Fingerprint.
func Freeze(source Document) (Document, error) {
	document := source.Clone()
	document.Fingerprint = ""
	configuration, err := canonicalConfiguration(document.Application.Configuration)
	if err != nil {
		return Document{}, fmt.Errorf("graph launch profile application configuration: %w", err)
	}
	document.Application.Configuration = configuration
	if err := document.validateStructure(); err != nil {
		return Document{}, err
	}
	fingerprint, err := documentFingerprint(document)
	if err != nil {
		return Document{}, err
	}
	document.Fingerprint = fingerprint
	return document, nil
}

// Validate verifies structure, canonical plugin configuration, and the
// content fingerprint.
func (document Document) Validate() error {
	configuration, err := canonicalConfiguration(document.Application.Configuration)
	if err != nil {
		return fmt.Errorf("graph launch profile application configuration: %w", err)
	}
	if !bytes.Equal(configuration, document.Application.Configuration) {
		return errors.New("graph launch profile application configuration is not canonical JSON")
	}
	if err := document.validateStructure(); err != nil {
		return err
	}
	unsigned := document.Clone()
	unsigned.Fingerprint = ""
	want, err := documentFingerprint(unsigned)
	if err != nil {
		return err
	}
	if document.Fingerprint != want {
		return fmt.Errorf("graph launch profile fingerprint is %q, want %q", document.Fingerprint, want)
	}
	return nil
}

func (document Document) validateStructure() error {
	if document.FormatVersion != FormatVersion {
		return fmt.Errorf("graph launch profile uses format %d, want %d", document.FormatVersion, FormatVersion)
	}
	if err := canonicalText("profile name", document.Name); err != nil {
		return err
	}
	if document.Revision == 0 {
		return errors.New("graph launch profile requires a positive revision")
	}
	if err := validateSelection("application", document.Application.Selection); err != nil {
		return err
	}
	if err := document.Plan.Validate(); err != nil {
		return fmt.Errorf("graph launch profile plan: %w", err)
	}
	if err := canonicalText("adapter reference", document.Adapter.Reference); err != nil {
		return err
	}
	if err := document.Adapter.RuntimeArtifact.Validate(); err != nil {
		return fmt.Errorf("graph launch profile adapter artifact: %w", err)
	}
	if err := canonicalText("adapter profile name", document.Adapter.ProfileName); err != nil {
		return err
	}
	if document.Adapter.ProfileRevision == 0 {
		return errors.New("graph launch profile adapter requires a positive profile revision")
	}
	return validateServer(document.Server)
}

func validateServer(server Server) error {
	if err := canonicalText("server profile name", server.ProfileName); err != nil {
		return err
	}
	if server.ProfileRevision == 0 {
		return errors.New("graph launch profile server requires a positive profile revision")
	}
	if err := server.ProviderArtifact.Validate(); err != nil {
		return fmt.Errorf("graph launch profile server provider artifact: %w", err)
	}
	if err := server.GatewayArtifact.Validate(); err != nil {
		return fmt.Errorf("graph launch profile server gateway artifact: %w", err)
	}
	if err := canonicalText("server model", server.Model); err != nil {
		return err
	}
	if err := canonicalText("server transcription model", server.TranscriptionModel); err != nil {
		return err
	}
	if server.TokenEnvironment != "" && !environmentName.MatchString(server.TokenEnvironment) {
		return errors.New("graph launch profile token environment is not canonical")
	}
	if !server.ValidateWire {
		return errors.New("graph launch profile must validate the pinned Realtime wire schema")
	}
	if server.InspectionTokenTTLMS < 1_000 || server.InspectionTokenTTLMS > 86_400_000 {
		return errors.New("graph launch profile inspection token TTL must be between 1000 and 86400000 milliseconds")
	}
	if server.MaxAudioFrameBytes < 1 || server.MaxAudioFrameBytes > 64<<20 {
		return errors.New("graph launch profile max audio frame bytes must be between 1 and 67108864")
	}
	limits := server.VideoLimits
	if err := canonicalText("server video format", limits.Format); err != nil {
		return err
	}
	if limits.Format != strings.ToLower(limits.Format) ||
		(limits.Format != "jpeg" && limits.Format != "png" && limits.Format != "webp") {
		return errors.New("graph launch profile server video format must be jpeg, png, or webp")
	}
	if limits.FPSCap < 1 || limits.FPSCap > 240 ||
		limits.MaxDimension < 1 || limits.MaxDimension > 32_768 ||
		limits.MaxFrameBytes < 1 || limits.MaxFrameBytes > 64<<20 {
		return errors.New("graph launch profile video limits are outside production bounds")
	}
	return nil
}

func validateSelection(label string, selection Selection) error {
	if err := canonicalText(label+" plugin reference", selection.Reference); err != nil {
		return err
	}
	if err := selection.Artifact.Validate(); err != nil {
		return fmt.Errorf("graph launch profile %s plugin artifact: %w", label, err)
	}
	return nil
}

func canonicalText(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) ||
		len(value) > maximumCanonicalTextBytes || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("graph launch profile %s is not canonical", label)
	}
	return nil
}

func canonicalConfiguration(source []byte) ([]byte, error) {
	if len(source) == 0 {
		return nil, errors.New("is required")
	}
	if len(source) > maximumConfigurationBytes {
		return nil, fmt.Errorf("has %d bytes; maximum is %d", len(source), maximumConfigurationBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON object: %w", err)
	}
	if value == nil {
		return nil, errors.New("must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode trailing JSON: %w", err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON object: %w", err)
	}
	if len(payload) > maximumConfigurationBytes {
		return nil, fmt.Errorf("has %d canonical bytes; maximum is %d", len(payload), maximumConfigurationBytes)
	}
	return payload, nil
}

func documentFingerprint(document Document) (string, error) {
	payload, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode graph launch profile identity: %w", err)
	}
	digest := sha256.New()
	digest.Write([]byte("openrealtime.graph-launch-profile/v1\x00"))
	digest.Write(payload)
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

// ApplicationFactory converts plugin-owned configuration into a complete,
// resource-free graph launch config. It must not resolve secrets, dial a
// provider, mount a graph, or acquire a listener.
type ApplicationFactory func(context.Context, json.RawMessage) (graphlaunch.Config, error)

// Registration is one host-installed graph application plugin.
type Registration struct {
	Reference string
	Artifact  inspect.ArtifactIdentity
	Factory   ApplicationFactory
}

// Registry is an immutable, duplicate-free application-plugin inventory.
type Registry struct {
	applications map[string]Registration
}

// NewRegistry snapshots and validates a broad host plugin inventory.
func NewRegistry(source []Registration) (*Registry, error) {
	if len(source) > maximumApplicationPlugins {
		return nil, fmt.Errorf("graph application registry has %d entries; maximum is %d", len(source), maximumApplicationPlugins)
	}
	applications := make(map[string]Registration, len(source))
	for index, registration := range source {
		selection := Selection{Reference: registration.Reference, Artifact: registration.Artifact}
		if err := validateSelection(fmt.Sprintf("application registration %d", index), selection); err != nil {
			return nil, err
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf("graph application registration %d has a nil factory", index)
		}
		if _, duplicate := applications[registration.Reference]; duplicate {
			return nil, fmt.Errorf("graph application reference %q is registered more than once", registration.Reference)
		}
		applications[registration.Reference] = registration
	}
	return &Registry{applications: applications}, nil
}

// Resolve exact-matches the selected application registration, validates its
// declared adapter selection, and invokes the generic graph launcher. No
// session resource is acquired by this operation.
func (registry *Registry) Resolve(ctx context.Context, profile Document) (graphlaunch.Result, error) {
	if ctx == nil {
		return graphlaunch.Result{}, errors.New("resolve graph launch profile: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return graphlaunch.Result{}, err
	}
	if err := profile.Validate(); err != nil {
		return graphlaunch.Result{}, err
	}
	if registry == nil {
		return graphlaunch.Result{}, errors.New("resolve graph launch profile: nil application registry")
	}
	registration, found := registry.applications[profile.Application.Reference]
	if !found {
		return graphlaunch.Result{}, fmt.Errorf("resolve graph launch profile: application registry is missing %q", profile.Application.Reference)
	}
	if registration.Artifact != profile.Application.Artifact {
		return graphlaunch.Result{}, fmt.Errorf("resolve graph launch profile: application %q runtime artifact drifted", profile.Application.Reference)
	}
	config, err := registration.Factory(ctx, bytes.Clone(profile.Application.Configuration))
	if err != nil {
		return graphlaunch.Result{}, fmt.Errorf("resolve graph launch profile application %q: %w", profile.Application.Reference, err)
	}
	wantAdapter := profile.Adapter.launchSelection()
	if config.Adapter != wantAdapter {
		return graphlaunch.Result{}, fmt.Errorf("resolve graph launch profile: application %q adapter selection drifted", profile.Application.Reference)
	}
	result, err := graphlaunch.New(ctx, config)
	if err != nil {
		return graphlaunch.Result{}, fmt.Errorf("resolve graph launch profile: %w", err)
	}
	if result.Plan == nil || result.Plan.Identity() != profile.Plan {
		return graphlaunch.Result{}, errors.New("resolve graph launch profile: prepared plan identity drifted")
	}
	if result.Binding == nil || result.Binding.Name() != profile.Adapter.ProfileName {
		return graphlaunch.Result{}, errors.New("resolve graph launch profile: prepared adapter profile identity drifted")
	}
	return result, nil
}

type yamlConfiguration struct {
	node *yaml.Node
}

func (configuration *yamlConfiguration) UnmarshalYAML(node *yaml.Node) error {
	configuration.node = cloneYAMLNode(node)
	return nil
}

type yamlApplication struct {
	Reference     string                   `yaml:"reference"`
	Artifact      inspect.ArtifactIdentity `yaml:"artifact"`
	Configuration yamlConfiguration        `yaml:"configuration"`
}

type yamlPlanIdentity struct {
	FormatVersion          uint64 `yaml:"format_version"`
	GraphID                string `yaml:"graph_id"`
	GraphRevision          uint64 `yaml:"graph_revision"`
	SourceDigest           string `yaml:"source_digest"`
	LockDigest             string `yaml:"lock_digest"`
	ValuesDigest           string `yaml:"values_digest"`
	ChannelsDigest         string `yaml:"channels_digest"`
	ValuesSchemaDigest     string `yaml:"values_schema_digest"`
	PublicDeploymentDigest string `yaml:"public_deployment_digest"`
	ResolutionDigest       string `yaml:"resolution_digest"`
	GraphFingerprint       string `yaml:"graph_fingerprint"`
	PlanFingerprint        string `yaml:"plan_fingerprint"`
}

func yamlPlan(source graphconfig.Identity) yamlPlanIdentity {
	return yamlPlanIdentity{
		FormatVersion: source.FormatVersion, GraphID: source.GraphID,
		GraphRevision: source.GraphRevision, SourceDigest: source.SourceDigest,
		LockDigest: source.LockDigest, ValuesDigest: source.ValuesDigest,
		ChannelsDigest: source.ChannelsDigest, ValuesSchemaDigest: source.ValuesSchemaDigest,
		PublicDeploymentDigest: source.PublicDeploymentDigest,
		ResolutionDigest:       source.ResolutionDigest, GraphFingerprint: source.GraphFingerprint,
		PlanFingerprint: source.PlanFingerprint,
	}
}

func (source yamlPlanIdentity) plan() graphconfig.Identity {
	return graphconfig.Identity{
		FormatVersion: source.FormatVersion, GraphID: source.GraphID,
		GraphRevision: source.GraphRevision, SourceDigest: source.SourceDigest,
		LockDigest: source.LockDigest, ValuesDigest: source.ValuesDigest,
		ChannelsDigest: source.ChannelsDigest, ValuesSchemaDigest: source.ValuesSchemaDigest,
		PublicDeploymentDigest: source.PublicDeploymentDigest,
		ResolutionDigest:       source.ResolutionDigest, GraphFingerprint: source.GraphFingerprint,
		PlanFingerprint: source.PlanFingerprint,
	}
}

type yamlVideoLimits struct {
	Format        string `yaml:"format"`
	FPSCap        int    `yaml:"fps_cap"`
	MaxDimension  int    `yaml:"max_dimension"`
	MaxFrameBytes int    `yaml:"max_frame_bytes"`
}

type yamlServer struct {
	ProfileName     string `yaml:"profile_name"`
	ProfileRevision uint64 `yaml:"profile_revision"`

	ProviderArtifact inspect.ArtifactIdentity `yaml:"provider_artifact"`
	GatewayArtifact  inspect.ArtifactIdentity `yaml:"gateway_artifact"`

	Model                string          `yaml:"model"`
	TranscriptionModel   string          `yaml:"transcription_model"`
	TokenEnvironment     string          `yaml:"token_environment,omitempty"`
	ValidateWire         bool            `yaml:"validate_wire"`
	InspectionTokenTTLMS uint64          `yaml:"inspection_token_ttl_ms"`
	MaxAudioFrameBytes   int             `yaml:"max_audio_frame_bytes"`
	VideoLimits          yamlVideoLimits `yaml:"video_limits"`
}

func yamlServerFrom(source Server) yamlServer {
	return yamlServer{
		ProfileName: source.ProfileName, ProfileRevision: source.ProfileRevision,
		ProviderArtifact: source.ProviderArtifact, GatewayArtifact: source.GatewayArtifact,
		Model: source.Model, TranscriptionModel: source.TranscriptionModel,
		TokenEnvironment: source.TokenEnvironment, ValidateWire: source.ValidateWire,
		InspectionTokenTTLMS: source.InspectionTokenTTLMS,
		MaxAudioFrameBytes:   source.MaxAudioFrameBytes,
		VideoLimits: yamlVideoLimits{
			Format: source.VideoLimits.Format, FPSCap: source.VideoLimits.FPSCap,
			MaxDimension:  source.VideoLimits.MaxDimension,
			MaxFrameBytes: source.VideoLimits.MaxFrameBytes,
		},
	}
}

func (source yamlServer) server() Server {
	return Server{
		ProfileName: source.ProfileName, ProfileRevision: source.ProfileRevision,
		ProviderArtifact: source.ProviderArtifact, GatewayArtifact: source.GatewayArtifact,
		Model: source.Model, TranscriptionModel: source.TranscriptionModel,
		TokenEnvironment: source.TokenEnvironment, ValidateWire: source.ValidateWire,
		InspectionTokenTTLMS: source.InspectionTokenTTLMS,
		MaxAudioFrameBytes:   source.MaxAudioFrameBytes,
		VideoLimits: openrealtime.Limits{
			Format: source.VideoLimits.Format, FPSCap: source.VideoLimits.FPSCap,
			MaxDimension:  source.VideoLimits.MaxDimension,
			MaxFrameBytes: source.VideoLimits.MaxFrameBytes,
		},
	}
}

type yamlDocument struct {
	FormatVersion uint64           `yaml:"format_version"`
	Name          string           `yaml:"name"`
	Revision      uint64           `yaml:"revision"`
	Fingerprint   string           `yaml:"fingerprint"`
	Application   yamlApplication  `yaml:"application"`
	Plan          yamlPlanIdentity `yaml:"plan"`
	Adapter       AdapterSelection `yaml:"adapter"`
	Server        yamlServer       `yaml:"server"`
}

// ParseYAML decodes one strict YAML document and verifies its fingerprint.
// Application configuration is constrained to the JSON data model before it
// is delegated to the selected plugin.
func ParseYAML(path string, source []byte) (Document, error) {
	if len(source) == 0 {
		return Document{}, errors.New("decode graph launch profile: empty profile")
	}
	if len(source) > maximumProfileBytes {
		return Document{}, fmt.Errorf("decode graph launch profile: %d bytes exceeds maximum %d", len(source), maximumProfileBytes)
	}
	var raw yamlDocument
	if _, err := strictyaml.Decode(path, source, &raw); err != nil {
		return Document{}, fmt.Errorf("decode graph launch profile: %w", err)
	}
	configuration, err := yamlConfigurationJSON(raw.Application.Configuration.node)
	if err != nil {
		return Document{}, fmt.Errorf("decode graph launch profile application configuration: %w", err)
	}
	document := Document{
		FormatVersion: raw.FormatVersion, Name: raw.Name, Revision: raw.Revision,
		Fingerprint: raw.Fingerprint,
		Application: Application{
			Selection:     Selection{Reference: raw.Application.Reference, Artifact: raw.Application.Artifact},
			Configuration: configuration,
		},
		Plan: raw.Plan.plan(), Adapter: raw.Adapter, Server: raw.Server.server(),
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

type yamlOutputApplication struct {
	Reference     string                   `yaml:"reference"`
	Artifact      inspect.ArtifactIdentity `yaml:"artifact"`
	Configuration *yaml.Node               `yaml:"configuration"`
}

type yamlOutputDocument struct {
	FormatVersion uint64                `yaml:"format_version"`
	Name          string                `yaml:"name"`
	Revision      uint64                `yaml:"revision"`
	Fingerprint   string                `yaml:"fingerprint"`
	Application   yamlOutputApplication `yaml:"application"`
	Plan          yamlPlanIdentity      `yaml:"plan"`
	Adapter       AdapterSelection      `yaml:"adapter"`
	Server        yamlServer            `yaml:"server"`
}

// MarshalYAML returns deterministic, newline-terminated YAML for one already
// frozen profile. ParseYAML accepts the result without changing its identity.
func MarshalYAML(document Document) ([]byte, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(document.Application.Configuration))
	decoder.UseNumber()
	var configurationValue any
	if err := decoder.Decode(&configurationValue); err != nil {
		return nil, fmt.Errorf("decode canonical application configuration: %w", err)
	}
	configuration, err := jsonValueYAMLNode(configurationValue)
	if err != nil {
		return nil, fmt.Errorf("encode application configuration as YAML: %w", err)
	}
	payload, err := yaml.Marshal(yamlOutputDocument{
		FormatVersion: document.FormatVersion, Name: document.Name,
		Revision: document.Revision, Fingerprint: document.Fingerprint,
		Application: yamlOutputApplication{
			Reference: document.Application.Reference, Artifact: document.Application.Artifact,
			Configuration: configuration,
		},
		Plan: yamlPlan(document.Plan), Adapter: document.Adapter,
		Server: yamlServerFrom(document.Server),
	})
	if err != nil {
		return nil, fmt.Errorf("encode graph launch profile YAML: %w", err)
	}
	return payload, nil
}

func yamlConfigurationJSON(node *yaml.Node) ([]byte, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, errors.New("must be a mapping")
	}
	if err := validateJSONYAMLNode(node); err != nil {
		return nil, err
	}
	value, err := yamlNodeJSONValue(node)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("convert YAML configuration to JSON: %w", err)
	}
	return canonicalConfiguration(payload)
}

func yamlNodeJSONValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		result := make(map[string]any, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			value, err := yamlNodeJSONValue(node.Content[index+1])
			if err != nil {
				return nil, err
			}
			result[node.Content[index].Value] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, len(node.Content))
		for index, child := range node.Content {
			value, err := yamlNodeJSONValue(child)
			if err != nil {
				return nil, err
			}
			result[index] = value
		}
		return result, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			value, err := strconv.ParseBool(node.Value)
			if err != nil {
				return nil, fmt.Errorf("invalid JSON boolean %q", node.Value)
			}
			return value, nil
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		case "!!str":
			return node.Value, nil
		}
	}
	return nil, fmt.Errorf("YAML node kind %d tag %q is outside the JSON data model", node.Kind, node.Tag)
}

func jsonValueYAMLNode(value any) (*yaml.Node, error) {
	switch typed := value.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(typed)}, nil
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: typed}, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(typed.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: typed.String()}, nil
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range typed {
			child, err := jsonValueYAMLNode(item)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	case map[string]any:
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, err := jsonValueYAMLNode(typed[key])
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
		return node, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", value)
	}
}

func validateJSONYAMLNode(node *yaml.Node) error {
	allowed := false
	switch node.Kind {
	case yaml.MappingNode:
		allowed = node.Tag == "!!map"
	case yaml.SequenceNode:
		allowed = node.Tag == "!!seq"
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null", "!!bool", "!!int", "!!float", "!!str":
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("YAML tag %q is outside the JSON data model", node.Tag)
	}
	for _, child := range node.Content {
		if err := validateJSONYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func cloneYAMLNode(source *yaml.Node) *yaml.Node {
	if source == nil {
		return nil
	}
	result := *source
	result.Content = make([]*yaml.Node, len(source.Content))
	for index, child := range source.Content {
		result.Content[index] = cloneYAMLNode(child)
	}
	return &result
}
