package sidecar

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
)

const (
	MaxElementPorts           = 256
	MaxPortWireFormats        = 16
	MaxElementCapabilities    = 512
	MaxElementRequirements    = 256
	MaxElementJSONBytes       = 512 << 10
	MaxElementBinaryBytes     = 16 << 20
	MaxElementIdentifierBytes = 1024
	MaxEnvelopeParents        = 256
)

// PayloadMode says which payload lanes one negotiated port frame must use.
// The envelope and its correlation metadata always remain in the JSON header.
type PayloadMode string

const (
	PayloadJSON       PayloadMode = "json"
	PayloadBinary     PayloadMode = "binary"
	PayloadJSONBinary PayloadMode = "json_binary"
)

// MediaKind is deliberately small: files and other opaque binary values may
// negotiate a binary PayloadMode without pretending to be live media.
type MediaKind string

const (
	MediaAudio MediaKind = "audio"
	MediaVideo MediaKind = "video"
)

// MediaFormat is one exact, bounded profile offered for a selected port.
// A Ready frame chooses the complete WireFormat containing it; negotiation
// never relies on omitted fields meaning provider-specific defaults.
type MediaFormat struct {
	Kind                MediaKind `json:"kind"`
	Encoding            string    `json:"encoding"`
	SampleFormat        string    `json:"sample_format,omitempty"`
	SampleRateHz        int       `json:"sample_rate_hz,omitempty"`
	Channels            int       `json:"channels,omitempty"`
	MaxFrameDurationMS  int       `json:"max_frame_duration_ms,omitempty"`
	MaxWidth            int       `json:"max_width,omitempty"`
	MaxHeight           int       `json:"max_height,omitempty"`
	MaxFrameRateMilliHz int       `json:"max_frame_rate_millihz,omitempty"`
}

// MediaFrameMetadata is the protocol-level description of one binary media
// body. It is deliberately separate from the element payload JSON: transport
// conformance must not depend on guessing fields in provider-specific values.
// Audio rate/channel identities are exact while duration is bounded by the
// negotiated maximum. Video encoding is exact while geometry and rate are
// bounded by the negotiated maxima.
type MediaFrameMetadata struct {
	Kind             MediaKind `json:"kind"`
	Encoding         string    `json:"encoding"`
	SampleFormat     string    `json:"sample_format,omitempty"`
	SampleRateHz     int       `json:"sample_rate_hz,omitempty"`
	Channels         int       `json:"channels,omitempty"`
	FrameDurationMS  int       `json:"frame_duration_ms,omitempty"`
	Width            int       `json:"width,omitempty"`
	Height           int       `json:"height,omitempty"`
	FrameRateMilliHz int       `json:"frame_rate_millihz,omitempty"`
}

// Validate rejects incomplete, cross-kind, or unbounded frame metadata.
func (metadata MediaFrameMetadata) Validate() error {
	if metadata.Kind != MediaAudio && metadata.Kind != MediaVideo {
		return fmt.Errorf("unknown media frame kind %q", metadata.Kind)
	}
	if err := validateMediaIdentifier("frame encoding", metadata.Encoding); err != nil {
		return err
	}
	switch metadata.Kind {
	case MediaAudio:
		if err := validateMediaIdentifier("frame sample format", metadata.SampleFormat); err != nil {
			return err
		}
		if metadata.SampleRateHz <= 0 || metadata.SampleRateHz > 768_000 ||
			metadata.Channels <= 0 || metadata.Channels > 64 ||
			metadata.FrameDurationMS <= 0 || metadata.FrameDurationMS > 10_000 {
			return errors.New("audio frame metadata requires bounded sample rate, channels, and duration")
		}
		if metadata.Width != 0 || metadata.Height != 0 || metadata.FrameRateMilliHz != 0 {
			return errors.New("audio frame metadata cannot declare video geometry or frame rate")
		}
	case MediaVideo:
		if metadata.Width <= 0 || metadata.Width > 32_768 ||
			metadata.Height <= 0 || metadata.Height > 32_768 ||
			metadata.FrameRateMilliHz <= 0 || metadata.FrameRateMilliHz > 1_000_000 {
			return errors.New("video frame metadata requires bounded geometry and frame rate")
		}
		if metadata.SampleFormat != "" || metadata.SampleRateHz != 0 ||
			metadata.Channels != 0 || metadata.FrameDurationMS != 0 {
			return errors.New("video frame metadata cannot declare audio sample fields")
		}
	}
	return nil
}

func (metadata MediaFrameMetadata) validateAgainst(format MediaFormat) error {
	if err := metadata.Validate(); err != nil {
		return err
	}
	if metadata.Kind != format.Kind {
		return fmt.Errorf("media frame kind is %s, negotiated %s", metadata.Kind, format.Kind)
	}
	if metadata.Encoding != format.Encoding {
		return fmt.Errorf("media frame encoding is %q, negotiated %q", metadata.Encoding, format.Encoding)
	}
	switch format.Kind {
	case MediaAudio:
		if metadata.SampleFormat != format.SampleFormat ||
			metadata.SampleRateHz != format.SampleRateHz || metadata.Channels != format.Channels {
			return fmt.Errorf(
				"audio frame profile is %s/%dHz/%dch, negotiated %s/%dHz/%dch",
				metadata.SampleFormat, metadata.SampleRateHz, metadata.Channels,
				format.SampleFormat, format.SampleRateHz, format.Channels,
			)
		}
		if metadata.FrameDurationMS > format.MaxFrameDurationMS {
			return fmt.Errorf("audio frame duration is %dms, negotiated maximum is %dms",
				metadata.FrameDurationMS, format.MaxFrameDurationMS)
		}
	case MediaVideo:
		if metadata.Width > format.MaxWidth || metadata.Height > format.MaxHeight ||
			metadata.FrameRateMilliHz > format.MaxFrameRateMilliHz {
			return fmt.Errorf(
				"video frame is %dx%d at %d millihertz, negotiated maxima are %dx%d at %d millihertz",
				metadata.Width, metadata.Height, metadata.FrameRateMilliHz,
				format.MaxWidth, format.MaxHeight, format.MaxFrameRateMilliHz,
			)
		}
	}
	return nil
}

// WireFormat combines payload-lane rules, byte limits, and an optional live
// media profile. Maxima are part of the selected identity, so peers cannot
// silently raise allocation limits after readiness.
type WireFormat struct {
	PayloadMode    PayloadMode  `json:"payload_mode"`
	MaxJSONBytes   int          `json:"max_json_bytes,omitempty"`
	MaxBinaryBytes int          `json:"max_binary_bytes,omitempty"`
	Media          *MediaFormat `json:"media,omitempty"`
}

// PortNegotiation is the exact format selected by Ready for one Hello port.
type PortNegotiation struct {
	Name      string            `json:"name"`
	Direction element.Direction `json:"direction"`
	Format    WireFormat        `json:"format"`
}

// JSONWireFormat is the strict default for ordinary typed values.
func JSONWireFormat() WireFormat {
	return WireFormat{PayloadMode: PayloadJSON, MaxJSONBytes: MaxElementJSONBytes}
}

// JSONPortSelection constructs the ordinary non-binary v4 port offer.
func JSONPortSelection(name string, direction element.Direction, valueType element.Type) PortSelection {
	return PortSelection{
		Name: name, Direction: direction, Type: valueType.Clone(),
		Formats: []WireFormat{JSONWireFormat()},
	}
}

// MediaKindForType is a convenience for deployment adapters that want to
// propose standard media defaults. It is not protocol authority: protocol v4
// accepts arbitrary concrete port types, and only the explicit WireFormat
// negotiated for a port decides whether that port has a live-media lane.
// A type containing both standard kinds is ambiguous to this convenience
// classifier and must be configured explicitly.
func MediaKindForType(value element.Type) (MediaKind, bool, error) {
	kinds := map[MediaKind]bool{}
	var visit func(element.Type)
	visit = func(current element.Type) {
		name := strings.ToLower(current.Name)
		leaf := name
		if separator := strings.LastIndexByte(leaf, '.'); separator >= 0 {
			leaf = leaf[separator+1:]
		}
		// A reference to media is ordinary typed metadata, not an in-band live
		// media body. Use bounded carrier suffixes rather than substring matches
		// so names such as FrameReference or FrameRate do not acquire a binary
		// transport requirement merely because they mention a frame.
		carrier := !strings.Contains(leaf, "reference") &&
			(strings.HasSuffix(leaf, "frame") || strings.HasSuffix(leaf, "frames") ||
				strings.HasSuffix(leaf, "batch") || strings.HasSuffix(leaf, "chunk") ||
				strings.HasSuffix(leaf, "samples"))
		switch {
		case carrier && (strings.HasPrefix(name, "audio.") || strings.HasPrefix(name, "speech.audio")):
			kinds[MediaAudio] = true
		case carrier && (strings.HasPrefix(name, "video.") || strings.HasPrefix(name, "vision.video") ||
			strings.HasPrefix(name, "image.") || strings.HasPrefix(name, "vision.image")):
			kinds[MediaVideo] = true
		}
		for _, argument := range current.Arguments {
			visit(argument)
		}
	}
	visit(value)
	if len(kinds) > 1 {
		return "", false, fmt.Errorf("port type %s contains both audio and video contracts", value.String())
	}
	for kind := range kinds {
		return kind, true, nil
	}
	return "", false, nil
}

func cloneWireFormats(source []WireFormat) []WireFormat {
	result := slices.Clone(source)
	for index := range result {
		if result[index].Media != nil {
			media := *result[index].Media
			result[index].Media = &media
		}
	}
	return result
}

// CloneWireFormats returns recursively independent format offers.
func CloneWireFormats(source []WireFormat) []WireFormat { return cloneWireFormats(source) }

// ValidatePortWireFormats checks formats against the selected graph type.
func ValidatePortWireFormats(valueType element.Type, formats []WireFormat) error {
	return validatePortFormats(PortSelection{Type: valueType, Formats: formats})
}

func (format WireFormat) equal(other WireFormat) bool {
	if format.PayloadMode != other.PayloadMode || format.MaxJSONBytes != other.MaxJSONBytes ||
		format.MaxBinaryBytes != other.MaxBinaryBytes {
		return false
	}
	if format.Media == nil || other.Media == nil {
		return format.Media == nil && other.Media == nil
	}
	return *format.Media == *other.Media
}

// Equal reports whether two formats describe the same negotiated wire
// contract, including payload limits and the complete media profile.
func (format WireFormat) Equal(other WireFormat) bool { return format.equal(other) }

func validateWireFormat(format WireFormat) error {
	switch format.PayloadMode {
	case PayloadJSON:
		if format.MaxJSONBytes <= 0 || format.MaxJSONBytes > MaxElementJSONBytes || format.MaxBinaryBytes != 0 {
			return fmt.Errorf("json payload mode requires max_json_bytes in [1,%d] and no binary allowance", MaxElementJSONBytes)
		}
	case PayloadBinary:
		if format.MaxJSONBytes != 0 || format.MaxBinaryBytes <= 0 || format.MaxBinaryBytes > MaxElementBinaryBytes {
			return fmt.Errorf("binary payload mode requires max_binary_bytes in [1,%d] and no JSON allowance", MaxElementBinaryBytes)
		}
	case PayloadJSONBinary:
		if format.MaxJSONBytes <= 0 || format.MaxJSONBytes > MaxElementJSONBytes ||
			format.MaxBinaryBytes <= 0 || format.MaxBinaryBytes > MaxElementBinaryBytes {
			return errors.New("json_binary payload mode requires positive bounded JSON and binary maxima")
		}
	default:
		return fmt.Errorf("unknown payload mode %q", format.PayloadMode)
	}
	if format.Media == nil {
		return nil
	}
	media := *format.Media
	if media.Kind != MediaAudio && media.Kind != MediaVideo {
		return fmt.Errorf("unknown media kind %q", media.Kind)
	}
	if err := validateMediaIdentifier("encoding", media.Encoding); err != nil {
		return err
	}
	if format.PayloadMode == PayloadJSON {
		return errors.New("live media format must negotiate a binary payload lane")
	}
	switch media.Kind {
	case MediaAudio:
		if err := validateMediaIdentifier("sample format", media.SampleFormat); err != nil {
			return err
		}
		if media.SampleRateHz <= 0 || media.SampleRateHz > 768_000 ||
			media.Channels <= 0 || media.Channels > 64 ||
			media.MaxFrameDurationMS <= 0 || media.MaxFrameDurationMS > 10_000 {
			return errors.New("audio media requires bounded sample rate, channels, and frame duration")
		}
		if media.MaxWidth != 0 || media.MaxHeight != 0 || media.MaxFrameRateMilliHz != 0 {
			return errors.New("audio media cannot declare video geometry or frame rate")
		}
	case MediaVideo:
		if media.MaxWidth <= 0 || media.MaxWidth > 32_768 || media.MaxHeight <= 0 || media.MaxHeight > 32_768 ||
			media.MaxFrameRateMilliHz <= 0 || media.MaxFrameRateMilliHz > 1_000_000 {
			return errors.New("video media requires bounded geometry and frame rate")
		}
		if media.SampleFormat != "" || media.SampleRateHz != 0 || media.Channels != 0 ||
			media.MaxFrameDurationMS != 0 {
			return errors.New("video media cannot declare audio sample fields")
		}
	}
	return nil
}

func validateMediaIdentifier(label, value string) error {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) ||
		value != strings.ToLower(value) {
		return fmt.Errorf("media %s must be a bounded canonical lowercase identifier", label)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			strings.ContainsRune("-._+/", character) {
			continue
		}
		return fmt.Errorf("media %s must be a bounded canonical lowercase identifier", label)
	}
	return nil
}

func validatePortFormats(selection PortSelection) error {
	kind, mediaRequired, err := MediaKindForType(selection.Type)
	if err != nil {
		return err
	}
	for index, format := range selection.Formats {
		if mediaRequired && format.Media == nil {
			return fmt.Errorf("wire format %d: %s port wire format requires an explicit media profile",
				index, kind)
		}
		if !mediaRequired && format.Media != nil {
			return fmt.Errorf("wire format %d: non-media port type %s cannot declare a live media profile",
				index, selection.Type.String())
		}
		if mediaRequired && format.Media != nil && format.Media.Kind != kind {
			return fmt.Errorf("wire format %d: port type requires %s media, format declares %s",
				index, kind, format.Media.Kind)
		}
	}
	return validateExplicitPortFormats(selection)
}

// validateExplicitPortFormats validates only the declared wire contract. The
// protocol-v4 handshake uses this path so a provider-defined type name cannot
// acquire transport authority. ValidatePortWireFormats retains the stricter
// standard-media convenience policy used by existing deployment adapters.
func validateExplicitPortFormats(selection PortSelection) error {
	if err := selection.Type.ValidateConcretePort(); err != nil {
		return fmt.Errorf("selected port type: %w", err)
	}
	if len(selection.Formats) == 0 {
		return errors.New("selected port requires at least one wire format")
	}
	if len(selection.Formats) > MaxPortWireFormats {
		return fmt.Errorf("selected port offers more than %d wire formats", MaxPortWireFormats)
	}
	seen := make([]WireFormat, 0, len(selection.Formats))
	for index, format := range selection.Formats {
		if err := validateWireFormat(format); err != nil {
			return fmt.Errorf("wire format %d: %w", index, err)
		}
		for _, previous := range seen {
			if format.equal(previous) {
				return fmt.Errorf("wire format %d is duplicated", index)
			}
		}
		seen = append(seen, format)
	}
	return nil
}

func canonicalNegotiations(source []PortNegotiation) ([]PortNegotiation, error) {
	if len(source) > MaxElementPorts {
		return nil, fmt.Errorf("ready negotiates more than %d ports", MaxElementPorts)
	}
	result := slices.Clone(source)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		negotiation := &result[index]
		if err := validateBoundedIdentifier("negotiated port name", negotiation.Name); err != nil {
			return nil, fmt.Errorf("negotiated port %d: %w", index, err)
		}
		if negotiation.Direction != element.Input && negotiation.Direction != element.Output {
			return nil, fmt.Errorf("negotiated port %s has invalid direction %q", negotiation.Name, negotiation.Direction)
		}
		if err := validateWireFormat(negotiation.Format); err != nil {
			return nil, fmt.Errorf("negotiated port %s: %w", negotiation.Name, err)
		}
		negotiation.Format = cloneWireFormats([]WireFormat{negotiation.Format})[0]
		key := portKey(negotiation.Direction, negotiation.Name)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("port %s is negotiated more than once", negotiation.Name)
		}
		seen[key] = struct{}{}
	}
	sort.Slice(result, func(left, right int) bool {
		return portKey(result[left].Direction, result[left].Name) <
			portKey(result[right].Direction, result[right].Name)
	})
	return result, nil
}

func validateBoundedIdentifier(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > MaxElementIdentifierBytes ||
		strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s is empty, non-canonical, or exceeds %d bytes", label, MaxElementIdentifierBytes)
	}
	return nil
}

func portKey(direction element.Direction, name string) string {
	return string(direction) + "\x00" + name
}
