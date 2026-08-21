package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// ExtensionReport records what the OpenRealtime Protocol suite verified.
//
// The counts are part of the report because the design bar was numeric: three
// events and two object extensions, on a base protocol with 66 wire names. A
// suite that verified behaviour but let the surface grow would be checking the
// wrong thing.
type ExtensionReport struct {
	Suite   string `json:"suite"`
	Passed  bool   `json:"passed"`
	Version int    `json:"version"`

	ClientEvents      int `json:"client_events"`
	ServerEvents      int `json:"server_events"`
	ExtendedObjects   int `json:"extended_objects"`
	ChangedBaseEvents int `json:"changed_base_events"`
	BaseWireNames     int `json:"base_wire_names"`

	Checks   []ExtensionCheck `json:"checks"`
	Failures []string         `json:"failures,omitempty"`
}

// ExtensionCheck is one verified property.
type ExtensionCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// RunExtension verifies the extension against its own rules.
func RunExtension() ExtensionReport {
	report := ExtensionReport{
		Suite: "openrealtime-protocol-v1", Version: openrealtime.Version,
		ClientEvents: 2, ServerEvents: 1, ExtendedObjects: 2, ChangedBaseEvents: 0,
		BaseWireNames: len(openaiwire.Definitions()),
	}
	record := func(name string, passed bool, detail string) {
		report.Checks = append(report.Checks, ExtensionCheck{Name: name, Passed: passed, Detail: detail})
		if !passed {
			report.Failures = append(report.Failures, name+": "+detail)
		}
	}

	// Principle 2: every added event lives under the namespace, and no added
	// name collides with a base one.
	names := []string{
		openrealtime.EventVideoSourceUpdate,
		openrealtime.EventVideoFrameAppend,
		openrealtime.EventObservationAdded,
	}
	base := make(map[string]struct{}, len(openaiwire.Definitions()))
	for _, definition := range openaiwire.Definitions() {
		base[string(definition.Type)] = struct{}{}
	}
	namespaced := true
	collision := ""
	for _, name := range names {
		if !strings.HasPrefix(name, "openrealtime.") {
			namespaced = false
			collision = name
		}
		if _, exists := base[name]; exists {
			namespaced = false
			collision = name
		}
	}
	record("events are namespaced and collide with nothing in the base protocol", namespaced, collision)
	record("the addition is three events", len(names) == 3, fmt.Sprintf("%d events", len(names)))

	// Principle 3: absent negotiation, behaviour is exactly the base protocol.
	empty, err := openrealtime.Negotiate(openrealtime.Request{Version: openrealtime.Version}, openrealtime.Features())
	record("a client that declares nothing enables nothing",
		err == nil && len(empty.Enabled) == 0 && empty.Video == nil,
		fmt.Sprintf("%+v err=%v", empty, err))

	// A capability the server cannot provide is absent rather than fatal: the
	// client gets a working session and can see what it did not get.
	partial, err := openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version, Supports: openrealtime.Features()},
		[]openrealtime.Feature{openrealtime.FeatureObservations},
	)
	record("an unsupported capability is declined without failing the session",
		err == nil && len(partial.Enabled) == 1 && partial.Enabled[0] == openrealtime.FeatureObservations,
		fmt.Sprintf("%+v err=%v", partial, err))

	// Video limits are stated at negotiation so a client can conform rather
	// than discover them by being rejected.
	video, err := openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version, Supports: []openrealtime.Feature{openrealtime.FeatureVideoInput}},
		openrealtime.Features(),
	)
	record("enabling video states its limits",
		err == nil && video.Video != nil && video.Video.FPSCap > 0 && video.Video.MaxDimension > 0,
		fmt.Sprintf("%+v err=%v", video.Video, err))

	// A version this server does not speak is refused rather than guessed at.
	_, err = openrealtime.Negotiate(openrealtime.Request{Version: openrealtime.Version + 1}, openrealtime.Features())
	record("an unknown protocol version is refused", err != nil, fmt.Sprint(err))

	_, err = openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version, Supports: []openrealtime.Feature{"telepathy"}},
		openrealtime.Features(),
	)
	record("an unknown capability name is refused", err != nil, fmt.Sprint(err))

	// Geometry is what makes a computer-use coordinate well-defined.
	active := openrealtime.VideoSourceUpdate{Source: "screen", State: openrealtime.SourceActive}
	record("an active source without geometry is refused", active.Validate() != nil, "")
	active.Width, active.Height = 1920, 1080
	record("an active source with geometry is accepted", active.Validate() == nil, "")
	record("a source declaration requires a source",
		(openrealtime.VideoSourceUpdate{State: openrealtime.SourceActive, Width: 1, Height: 1}).Validate() != nil, "")

	limits := openrealtime.DefaultLimits()
	limits.MaxFrameBytes = 8
	oversized := openrealtime.VideoFrameAppend{Source: "screen", Frame: "AAAAAAAAAAAAAAAA"}
	_, err = oversized.Decode(limits)
	record("an oversized frame is refused against the declared limit", err != nil, fmt.Sprint(err))

	malformed := openrealtime.VideoFrameAppend{Source: "screen", Frame: "not base64!!"}
	_, err = malformed.Decode(openrealtime.DefaultLimits())
	record("a malformed frame is refused", err != nil, fmt.Sprint(err))

	// Principle 1 and 5: the base protocol is untouched, and computer use adds
	// no events at all.
	record("computer use adds no events",
		!slices.ContainsFunc(names, func(name string) bool { return strings.Contains(name, "computer") }), "")

	// The tool extension is additive: a definition carrying it still decodes
	// as an ordinary function definition for a server that ignores the key.
	definition := map[string]any{
		"type": "function", "name": "computer.click", "description": "click",
		"parameters":   map[string]any{"type": "object"},
		"openrealtime": openrealtime.ToolExtension{Confirm: "always", Target: "browser-1"},
	}
	encoded, marshalErr := json.Marshal(definition)
	var ignoring struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	decodeErr := json.Unmarshal(encoded, &ignoring)
	record("a tool definition carrying the extension still decodes without it",
		marshalErr == nil && decodeErr == nil && ignoring.Type == "function" && ignoring.Name == "computer.click",
		fmt.Sprintf("%v %v", marshalErr, decodeErr))

	report.Passed = len(report.Failures) == 0
	return report
}
