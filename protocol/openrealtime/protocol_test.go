package openrealtime_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// Backward compatibility is a property of the negotiation rather than of
// client discipline, so these are tests about the negotiation.

func TestABaseProtocolClientGetsAnOrdinarySession(t *testing.T) {
	// A client that never mentions the key is not represented here at all:
	// the server sees no request and echoes nothing. What this checks is the
	// next case up - a client that declares the version and nothing else.
	response, err := openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version}, openrealtime.Features())
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	if len(response.Enabled) != 0 || response.Video != nil {
		t.Fatalf("declaring nothing must enable nothing: %+v", response)
	}
}

func TestAnUnknownVersionIsRefusedRatherThanDowngraded(t *testing.T) {
	if _, err := openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version + 1}, openrealtime.Features()); err == nil {
		t.Fatal("a version this server does not speak must be refused")
	}
	if _, err := openrealtime.Negotiate(openrealtime.Request{}, openrealtime.Features()); err == nil {
		t.Fatal("version zero is not version one")
	}
}

func TestAnUnknownCapabilityNameIsRefused(t *testing.T) {
	if _, err := openrealtime.Negotiate(openrealtime.Request{
		Version: openrealtime.Version, Supports: []openrealtime.Feature{"telepathy"},
	}, openrealtime.Features()); err == nil {
		t.Fatal("a name this version does not define must be refused rather than guessed at")
	}
}

// A capability the server cannot provide is absent from the answer rather than
// fatal. The session works and the client can see what it did not get.
func TestAnUnprovidableCapabilityIsDeclinedWithoutFailing(t *testing.T) {
	response, err := openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version, Supports: openrealtime.Features()},
		[]openrealtime.Feature{openrealtime.FeatureObservations})
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	if len(response.Enabled) != 1 || response.Enabled[0] != openrealtime.FeatureObservations {
		t.Fatalf("expected observations only, got %+v", response.Enabled)
	}
	if response.Video != nil {
		t.Fatal("video limits must not be stated for a capability that was not enabled")
	}
}

func TestEnablingVideoStatesTheLimitsAClientMustConformTo(t *testing.T) {
	response, err := openrealtime.Negotiate(openrealtime.Request{
		Version: openrealtime.Version, Supports: []openrealtime.Feature{openrealtime.FeatureVideoInput},
	}, openrealtime.Features())
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	if response.Video == nil {
		t.Fatal("a client cannot conform to limits it was not told")
	}
	if response.Video.FPSCap <= 0 || response.Video.MaxDimension <= 0 || response.Video.MaxFrameBytes <= 0 {
		t.Fatalf("every stated limit must be a real bound: %+v", response.Video)
	}
}

// Perception is selected per session, inside the object that already exists.
func TestObserverSelection(t *testing.T) {
	available := []string{"audio", "video"}
	for _, test := range []struct {
		name     string
		asked    []string
		expected []string
		refused  bool
	}{
		{name: "nothing named gets the default set", expected: available},
		{name: "a named set is honoured", asked: []string{"video"}, expected: []string{"video"}},
		{name: "duplicates collapse", asked: []string{"audio", "audio"}, expected: []string{"audio"}},
		{name: "an absent observer is dropped", asked: []string{"audio", "lidar"}, expected: []string{"audio"}},
		{name: "a set naming nothing available is refused", asked: []string{"lidar"}, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := openrealtime.NegotiateSession(
				openrealtime.Request{Version: openrealtime.Version, Observers: test.asked},
				openrealtime.Features(), openrealtime.DefaultLimits(), available)
			if test.refused {
				if err == nil {
					t.Fatal("expected a refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("negotiate: %v", err)
			}
			if strings.Join(response.Observers, ",") != strings.Join(test.expected, ",") {
				t.Fatalf("expected %v, got %v", test.expected, response.Observers)
			}
			if strings.Join(response.AvailableObservers, ",") != strings.Join(available, ",") {
				t.Fatalf("a client must be told what it could have chosen, got %v", response.AvailableObservers)
			}
		})
	}
}

// Geometry is what makes a computer-use coordinate mean anything, so a source
// that could not ground one is refused.
func TestASourceDeclarationRequiresGeometry(t *testing.T) {
	for _, test := range []struct {
		name    string
		update  openrealtime.VideoSourceUpdate
		refused bool
	}{
		{
			name:   "an active source with geometry",
			update: openrealtime.VideoSourceUpdate{Source: "screen", State: openrealtime.SourceActive, Width: 1920, Height: 1080},
		},
		{
			name:    "an active source without geometry",
			update:  openrealtime.VideoSourceUpdate{Source: "screen", State: openrealtime.SourceActive},
			refused: true,
		},
		{
			name:    "no source name",
			update:  openrealtime.VideoSourceUpdate{State: openrealtime.SourceActive, Width: 1, Height: 1},
			refused: true,
		},
		{
			name:   "a closed source needs no geometry",
			update: openrealtime.VideoSourceUpdate{Source: "screen", State: openrealtime.SourceClosed},
		},
		{
			name:    "an unknown state",
			update:  openrealtime.VideoSourceUpdate{Source: "screen", State: "sleeping"},
			refused: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.update.Validate()
			if test.refused != (err != nil) {
				t.Fatalf("refused=%t, err=%v", test.refused, err)
			}
		})
	}
}

func TestFrameDecodingEnforcesTheDeclaredLimit(t *testing.T) {
	limits := openrealtime.DefaultLimits()
	limits.MaxFrameBytes = 16

	payload := base64.StdEncoding.EncodeToString(make([]byte, 8))
	decoded, err := openrealtime.VideoFrameAppend{Source: "screen", Frame: payload}.Decode(limits)
	if err != nil || len(decoded) != 8 {
		t.Fatalf("a conforming frame must decode: %d bytes, err=%v", len(decoded), err)
	}

	oversized := base64.StdEncoding.EncodeToString(make([]byte, 64))
	if _, err := (openrealtime.VideoFrameAppend{Source: "screen", Frame: oversized}).Decode(limits); err == nil {
		t.Fatal("a frame past the declared limit must be refused")
	}
	if _, err := (openrealtime.VideoFrameAppend{Source: "screen", Frame: "not base64!!"}).Decode(limits); err == nil {
		t.Fatal("a malformed frame must be refused")
	}
	if _, err := (openrealtime.VideoFrameAppend{Frame: payload}).Decode(limits); err == nil {
		t.Fatal("a frame from no source must be refused")
	}
	if _, err := (openrealtime.VideoFrameAppend{Source: "screen"}).Decode(limits); err == nil {
		t.Fatal("a frame with no image must be refused")
	}
}

// The extension is additive: a server that has never heard of it still reads
// every object it appears in.
func TestTheToolExtensionIsIgnorableByABaseServer(t *testing.T) {
	encoded, err := json.Marshal(map[string]any{
		"type": "function", "name": "computer.click",
		"parameters":   map[string]any{"type": "object"},
		"openrealtime": openrealtime.ToolExtension{Confirm: "always", Target: "browser-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var ignoring struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(encoded, &ignoring); err != nil {
		t.Fatalf("a base server must still decode the definition: %v", err)
	}
	if ignoring.Type != "function" || ignoring.Name != "computer.click" {
		t.Fatalf("the base fields must be untouched: %+v", ignoring)
	}
}

// The wire addition is three events, and every one of them is namespaced.
func TestTheWireAdditionIsThreeNamespacedEvents(t *testing.T) {
	names := []string{
		openrealtime.EventVideoSourceUpdate,
		openrealtime.EventVideoFrameAppend,
		openrealtime.EventObservationAdded,
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "openrealtime.") {
			t.Fatalf("every added event must be namespaced: %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("duplicate event name %q", name)
		}
		seen[name] = struct{}{}
	}
}

func TestParseFeatureAcceptsOnlyWhatThisVersionDefines(t *testing.T) {
	for _, name := range openrealtime.Features() {
		if _, err := openrealtime.ParseFeature(string(name)); err != nil {
			t.Fatalf("%q must parse: %v", name, err)
		}
	}
	if _, err := openrealtime.ParseFeature("video"); err == nil {
		t.Fatal("a near-miss must be refused rather than corrected")
	}
}

// A selection nothing will honour is refused rather than confirmed.
//
// Only a binding that runs its own perception has observers to select from.
// The others - the ones whose model owns its perception - advertise none, and
// used to accept whatever a client named because the filter that rejects
// unknown names was skipped when there was nothing to compare against. The
// client got its own list back as the negotiated set, complete with names that
// exist nowhere, and believed it had enabled something.
func TestObserversAreRefusedWhereNoneCanBeSelected(t *testing.T) {
	_, err := openrealtime.NegotiateSession(
		openrealtime.Request{Version: openrealtime.Version, Observers: []string{"video"}},
		nil, openrealtime.Limits{}, nil)
	if err == nil {
		t.Fatal("a binding with no observers must refuse a selection rather than confirm it")
	}
	if !strings.Contains(err.Error(), "no selectable observers") {
		t.Fatalf("the refusal must say why, got %v", err)
	}
}

// An invented name is refused even where observers do exist, which is the
// case that always worked and has to keep working.
func TestAnInventedObserverIsRefused(t *testing.T) {
	_, err := openrealtime.NegotiateSession(
		openrealtime.Request{Version: openrealtime.Version, Observers: []string{"invented"}},
		nil, openrealtime.Limits{}, []string{"audio", "video"})
	if err == nil {
		t.Fatal("an observer that exists nowhere must be refused")
	}
	if !strings.Contains(err.Error(), "audio, video") {
		t.Fatalf("the refusal must name what is available, got %v", err)
	}
}

// And a client that names a subset of what exists still gets exactly that
// subset - the filtering this fix tightened must not have become all-or-none.
func TestASubsetOfAvailableObserversIsHonoured(t *testing.T) {
	response, err := openrealtime.NegotiateSession(
		openrealtime.Request{Version: openrealtime.Version, Observers: []string{"audio", "invented"}},
		nil, openrealtime.Limits{}, []string{"audio", "video"})
	if err != nil {
		t.Fatalf("a partly-recognised selection is honoured, not refused: %v", err)
	}
	if len(response.Observers) != 1 || response.Observers[0] != "audio" {
		t.Fatalf("the answer must carry what was actually enabled, got %v", response.Observers)
	}
}
