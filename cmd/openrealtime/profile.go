package main

import (
	"errors"
	"strings"

	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// runtimeProfile is the normalized deployment shape. It does not own a
// second runtime: both levels use the same cascade and select roles, observers,
// and policies through its existing configuration seams.
type runtimeProfile struct {
	Name   string
	Voice  bool
	Vision bool
	Reflex bool
}

// normalizeProfile translates the profile plus legacy flags into the existing
// serve configuration. The voice zero/default is an identity transform; that
// property is what keeps every pre-profile voice deployment byte-for-byte in
// the same construction path.
func normalizeProfile(options serveOptions) (serveOptions, error) {
	if options.reflexTokens < 0 || options.reflexTimeout < 0 {
		return serveOptions{}, errors.New("visual reflex token limit and timeout cannot be negative")
	}
	name := strings.ToLower(strings.TrimSpace(options.profile))
	reflex := strings.TrimSpace(options.reflexModel) != ""
	observers, err := perception.ParseObserverSet(options.observers)
	if err != nil {
		return serveOptions{}, err
	}
	legacyVision := observers != perception.SetAudioOnly
	if name == "" {
		name = "voice"
	}
	// Naming a reflex is already an unambiguous request for vision when the
	// profile flag itself was not typed. This keeps role flags composable while
	// still letting an explicit -profile voice reject a contradictory setup.
	if (reflex || legacyVision) && !options.chose("profile") && name == "voice" {
		name = "voice+vision"
	}
	profile := runtimeProfile{Name: name, Voice: true, Reflex: reflex}
	switch name {
	case "voice":
		if reflex || legacyVision {
			return serveOptions{}, errors.New("visual observers and a visual reflex require -profile voice+vision")
		}
		return options, nil
	case "voice+vision", "voice-vision", "multimodal":
		profile.Name = "voice+vision"
		profile.Vision = true
	default:
		return serveOptions{}, errors.New("profile must be voice or voice+vision")
	}
	bindingName := strings.ToLower(strings.TrimSpace(options.binding))
	if bindingName == "" {
		bindingName = "cascade"
	}
	if bindingName != "cascade" {
		capabilities, capabilityErr := parseStackCapabilities(options.sidecarCapabilities)
		if capabilityErr != nil {
			return serveOptions{}, capabilityErr
		}
		if (bindingName != "sidecar" && bindingName != "omni" && bindingName != "omni+text-policy") ||
			!capabilities.VisualInput {
			return serveOptions{}, errors.New(
				"a non-cascade voice+vision profile requires a sidecar binding with visual-input capability")
		}
		if options.sidecarProtocol != 0 && options.sidecarProtocol < sidecar.VersionMultimodal {
			return serveOptions{}, errors.New("sidecar voice+vision requires protocol v3")
		}
	}
	if !options.chose("observers") {
		options.observers = "audio+video"
	}
	// A reflex must reach pixels without waiting for narration. Keyframe-only
	// uses the same observer but puts no narrator model on the reaction path.
	// Operators can explicitly select keyframe+narration when persistent rich
	// descriptions are worth that added latency.
	if profile.Reflex && !options.chose("observer-components") {
		options.components = "keyframe"
	}
	observers, err = perception.ParseObserverSet(options.observers)
	if err != nil {
		return serveOptions{}, err
	}
	if observers == perception.SetAudioOnly {
		return serveOptions{}, errors.New("the voice+vision profile requires a video observer")
	}
	components, err := perception.ParseComponents(options.components)
	if err != nil {
		return serveOptions{}, err
	}
	if profile.Reflex && components == perception.ComponentNarrationOnly {
		return serveOptions{}, errors.New("the visual reflex requires keyframe or keyframe+narration observer components")
	}
	options.profile = profile.Name
	return options, nil
}
