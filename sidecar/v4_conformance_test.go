package sidecar_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/sidecar"
)

func mediaDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.MediaModel",
		Revision:      1,
		Ports: []element.Port{
			{Name: "audio", Direction: element.Input, Type: element.Stream(element.Named("audio.InputFrame")), Cardinality: element.One},
			{Name: "video", Direction: element.Input, Type: element.Stream(element.Named("video.InputFrame")), Cardinality: element.One},
			{Name: "trigger", Direction: element.Input, Type: element.Trigger(element.Named("cognition.Generate")), Cardinality: element.One},
			{Name: "audio_out", Direction: element.Output, Type: element.Segmented(
				element.Named("Prepared", element.Named("speech.AudioFrame")), element.Named("speech.UtteranceID"),
			), Cardinality: element.One},
			{Name: "outcome", Direction: element.Output, Type: element.Event(element.Named("cognition.Outcome")), Cardinality: element.One},
		},
		Reaction: element.Reaction{
			Triggers: []string{"trigger"}, Outcomes: []string{"audio_out", "outcome"},
		},
	}
}

func audioWire(sampleRate int) sidecar.WireFormat {
	return sidecar.WireFormat{
		PayloadMode: sidecar.PayloadJSONBinary, MaxJSONBytes: 1024, MaxBinaryBytes: 8,
		Media: &sidecar.MediaFormat{
			Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
			SampleRateHz: sampleRate, Channels: 1, MaxFrameDurationMS: 100,
		},
	}
}

func videoWire() sidecar.WireFormat {
	return sidecar.WireFormat{
		PayloadMode: sidecar.PayloadJSONBinary, MaxJSONBytes: 1024, MaxBinaryBytes: 1024,
		Media: &sidecar.MediaFormat{
			Kind: sidecar.MediaVideo, Encoding: "image/jpeg", MaxWidth: 1920,
			MaxHeight: 1080, MaxFrameRateMilliHz: 30_000,
		},
	}
}

func validMediaHello(t testing.TB) sidecar.Message {
	t.Helper()
	descriptor := mediaDescriptor()
	ports := map[string]element.Port{}
	for _, port := range descriptor.Ports {
		ports[port.Name] = port
	}
	return sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &descriptor,
		SelectedPorts: []sidecar.PortSelection{
			{Name: "audio", Direction: element.Input, Type: ports["audio"].Type, Formats: []sidecar.WireFormat{audioWire(24_000), audioWire(48_000)}},
			{Name: "video", Direction: element.Input, Type: ports["video"].Type, Formats: []sidecar.WireFormat{videoWire()}},
			sidecar.JSONPortSelection("trigger", element.Input, ports["trigger"].Type),
			{Name: "audio_out", Direction: element.Output, Type: ports["audio_out"].Type, Formats: []sidecar.WireFormat{audioWire(24_000)}},
			sidecar.JSONPortSelection("outcome", element.Output, ports["outcome"].Type),
		},
	}
}

func BenchmarkV4ElementHandshake(b *testing.B) {
	hello := validMediaHello(b)
	fixture := mediaFixture()
	b.ReportAllocs()
	for range b.N {
		if _, err := fixture.Ready(hello); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkV4MediaFrameValidation(b *testing.B) {
	hello := validMediaHello(b)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		b.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		b.Fatal(err)
	}
	frame := elementMessage("audio", element.Input, hello.SelectedPorts[0].Type,
		[]byte(`{"stream_id":"microphone"}`), []byte{1, 2, 3, 4})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := contract.ValidateEngineFrame(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkV4MediaFrameFramingRoundTrip(b *testing.B) {
	frame := elementMessage(
		"audio", element.Input, element.Stream(element.Named("audio.InputFrame")),
		[]byte(`{"stream_id":"microphone"}`), make([]byte, 960),
	)
	var stream bytes.Buffer
	writer := sidecar.NewWriter(&stream)
	reader := sidecar.NewReader(&stream)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame.Payload)))
	b.ResetTimer()
	for range b.N {
		stream.Reset()
		if err := writer.Write(frame); err != nil {
			b.Fatal(err)
		}
		if _, err := reader.Read(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkV4OpaqueBinaryFrameValidation(b *testing.B) {
	hello := opaqueBinaryHello()
	ready, err := genericFixture().Ready(hello)
	if err != nil {
		b.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		b.Fatal(err)
	}
	frame := elementMessage(
		"blob", element.Input, hello.SelectedPorts[0].Type, nil, make([]byte, 960),
	)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame.Payload)))
	b.ResetTimer()
	for range b.N {
		if err := contract.ValidateEngineFrame(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func mediaFixture() sidecar.ElementConformanceFixture {
	return sidecar.ElementConformanceFixture{
		Runtime:  sidecar.ArtifactIdentity{ID: "runtime/media-sidecar", Revision: "4.2.0"},
		Provider: sidecar.ArtifactIdentity{ID: "provider/media-model", Revision: "2026-08-29"},
		Adapter:  sidecar.ArtifactIdentity{ID: "adapter/element-v4", Revision: "4.2.0"},
	}
}

func genericFixture() sidecar.ElementConformanceFixture {
	return sidecar.ElementConformanceFixture{
		Runtime:  sidecar.ArtifactIdentity{ID: "runtime/generic-sidecar", Revision: "4.2.0"},
		Provider: sidecar.ArtifactIdentity{ID: "provider/generic-element", Revision: "2026-08-29"},
		Adapter:  sidecar.ArtifactIdentity{ID: "adapter/element-v4", Revision: "4.2.0"},
	}
}

func opaqueBinaryHello() sidecar.Message {
	valueType := element.Stream(element.Named("artifact.Chunk"))
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.OpaqueBinary",
		Revision:      1,
		Ports: []element.Port{{
			Name: "blob", Direction: element.Input, Type: valueType.Clone(),
			Cardinality: element.One, DefaultDepth: 2,
		}},
		Reaction: element.Reaction{Triggers: []string{"blob"}},
	}
	return sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &descriptor,
		SelectedPorts: []sidecar.PortSelection{{
			Name: "blob", Direction: element.Input, Type: valueType,
			Formats: []sidecar.WireFormat{{
				PayloadMode: sidecar.PayloadBinary, MaxBinaryBytes: 1024,
			}},
		}},
	}
}

func TestV4OpaqueBinaryPortsDoNotAcquireMediaOrAudioSemantics(t *testing.T) {
	hello := opaqueBinaryHello()
	ready, err := genericFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}
	frame := elementMessage(
		"blob", element.Input, hello.SelectedPorts[0].Type, nil, []byte("opaque"),
	)
	if err := contract.ValidateEngineFrame(frame); err != nil {
		t.Fatalf("opaque typed binary frame: %v", err)
	}

	withJSON := frame.Clone()
	withJSON.Envelope.JSON = json.RawMessage(`{"unexpected":true}`)
	if err := contract.ValidateEngineFrame(withJSON); err == nil ||
		!strings.Contains(err.Error(), "binary-only") {
		t.Fatalf("JSON on opaque binary port error = %v", err)
	}
	withMedia := frame.Clone()
	metadata := audioFrameMetadata()
	withMedia.Envelope.Media = &metadata
	if err := contract.ValidateEngineFrame(withMedia); err == nil ||
		!strings.Contains(err.Error(), "did not negotiate live media metadata") {
		t.Fatalf("media authority on opaque binary port error = %v", err)
	}
	oversized := frame.Clone()
	oversized.Payload = make([]byte, 1025)
	oversized.PayloadBytes = len(oversized.Payload)
	if err := contract.ValidateEngineFrame(oversized); err == nil ||
		!strings.Contains(err.Error(), "maxima") {
		t.Fatalf("opaque binary bound error = %v", err)
	}
}

func TestV4NegotiatesEveryMediaPortAndEnforcesPayloadPairings(t *testing.T) {
	hello := validMediaHello(t)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range hello.SelectedPorts {
		negotiated, found := contract.NegotiatedPort(selected.Direction, selected.Name)
		if !found || !negotiated.Format.Equal(selected.Formats[0]) {
			t.Fatalf("port %s negotiation = %+v, found=%v", selected.Name, negotiated, found)
		}
	}

	audio := elementMessage("audio", element.Input, hello.SelectedPorts[0].Type,
		[]byte(`{"stream_id":"s1"}`), []byte{1, 2, 3, 4})
	if err := contract.ValidateEngineFrame(audio); err != nil {
		t.Fatalf("valid audio frame: %v", err)
	}
	withoutMedia := audio.Clone()
	withoutMedia.Envelope.Media = nil
	if err := contract.ValidateEngineFrame(withoutMedia); err == nil ||
		!strings.Contains(err.Error(), "explicit media frame metadata") {
		t.Fatalf("missing audio media metadata error = %v", err)
	}
	withoutBinary := audio.Clone()
	withoutBinary.Payload, withoutBinary.PayloadBytes = nil, 0
	if err := contract.ValidateEngineFrame(withoutBinary); err == nil ||
		!strings.Contains(err.Error(), "without a binary frame") {
		t.Fatalf("missing audio binary error = %v", err)
	}
	metadataOnlyBoundary := withoutBinary.Clone()
	metadataOnlyBoundary.Envelope.Media = nil
	if err := contract.ValidateEngineFrame(metadataOnlyBoundary); err != nil {
		t.Fatalf("generic JSON/binary port boundary without a binary delta: %v", err)
	}
	withoutJSON := audio.Clone()
	withoutJSON.Envelope.JSON = nil
	if err := contract.ValidateEngineFrame(withoutJSON); err == nil || !strings.Contains(err.Error(), "JSON metadata") {
		t.Fatalf("missing audio JSON error = %v", err)
	}
	oversized := audio.Clone()
	oversized.Payload = make([]byte, 9)
	oversized.PayloadBytes = len(oversized.Payload)
	if err := contract.ValidateEngineFrame(oversized); err == nil || !strings.Contains(err.Error(), "maxima") {
		t.Fatalf("oversized audio error = %v", err)
	}
	missingClaimedBody := audio.Clone()
	missingClaimedBody.Payload = nil
	if err := contract.ValidateEngineFrame(missingClaimedBody); err == nil ||
		!strings.Contains(err.Error(), "declares") {
		t.Fatalf("claimed-but-missing binary body error = %v", err)
	}

	trigger := elementMessage("trigger", element.Input, hello.SelectedPorts[2].Type,
		[]byte(`{"prompt":"hello"}`), []byte{1, 2})
	if err := contract.ValidateEngineFrame(trigger); err == nil || !strings.Contains(err.Error(), "JSON-only") {
		t.Fatalf("binary on JSON port error = %v", err)
	}

	// Any explicit JSON/binary format may carry a typed JSON-only boundary;
	// temporal semantics belong to the port type/codec, not a Segmented or
	// audio name embedded in the transport.
	audioOutType := hello.SelectedPorts[3].Type
	begin := elementMessage("audio_out", element.Output, audioOutType,
		[]byte(`{"boundary":"begin"}`), nil)
	if err := contract.ValidateSidecarFrame(begin); err != nil {
		t.Fatalf("segmented audio begin: %v", err)
	}
	delta := elementMessage("audio_out", element.Output, audioOutType,
		[]byte(`{"boundary":"delta"}`), []byte{1, 2, 3, 4})
	if err := contract.ValidateSidecarFrame(delta); err != nil {
		t.Fatalf("segmented audio delta: %v", err)
	}
	metadataOnly := begin.Clone()
	metadata := audioFrameMetadata()
	metadataOnly.Envelope.Media = &metadata
	if err := contract.ValidateSidecarFrame(metadataOnly); err == nil ||
		!strings.Contains(err.Error(), "without a binary frame") {
		t.Fatalf("metadata-only segmented boundary error = %v", err)
	}
}

func TestV4MediaFramesMustConformToTheExactNegotiatedProfile(t *testing.T) {
	hello := validMediaHello(t)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}

	audio := elementMessage("audio", element.Input, hello.SelectedPorts[0].Type,
		[]byte(`{"stream_id":"s1"}`), []byte{1, 2, 3, 4})
	for name, mutate := range map[string]func(*sidecar.MediaFrameMetadata){
		"kind": func(metadata *sidecar.MediaFrameMetadata) {
			*metadata = videoFrameMetadata()
		},
		"encoding":      func(metadata *sidecar.MediaFrameMetadata) { metadata.Encoding = "audio/opus" },
		"sample format": func(metadata *sidecar.MediaFrameMetadata) { metadata.SampleFormat = "f32le" },
		"sample rate":   func(metadata *sidecar.MediaFrameMetadata) { metadata.SampleRateHz = 48_000 },
		"channels":      func(metadata *sidecar.MediaFrameMetadata) { metadata.Channels = 2 },
		"duration":      func(metadata *sidecar.MediaFrameMetadata) { metadata.FrameDurationMS = 101 },
	} {
		t.Run("audio "+name, func(t *testing.T) {
			frame := audio.Clone()
			mutate(frame.Envelope.Media)
			if err := contract.ValidateEngineFrame(frame); err == nil ||
				!strings.Contains(err.Error(), "media profile") {
				t.Fatalf("mismatched audio %s error = %v", name, err)
			}
		})
	}

	video := elementMessage("video", element.Input, hello.SelectedPorts[1].Type,
		[]byte(`{"stream_id":"screen"}`), []byte{0xff, 0xd8, 0xff})
	if err := contract.ValidateEngineFrame(video); err != nil {
		t.Fatalf("valid video frame: %v", err)
	}
	for name, mutate := range map[string]func(*sidecar.MediaFrameMetadata){
		"encoding": func(metadata *sidecar.MediaFrameMetadata) { metadata.Encoding = "image/png" },
		"width":    func(metadata *sidecar.MediaFrameMetadata) { metadata.Width = 1921 },
		"height":   func(metadata *sidecar.MediaFrameMetadata) { metadata.Height = 1081 },
		"rate":     func(metadata *sidecar.MediaFrameMetadata) { metadata.FrameRateMilliHz = 30_001 },
	} {
		t.Run("video "+name, func(t *testing.T) {
			frame := video.Clone()
			mutate(frame.Envelope.Media)
			if err := contract.ValidateEngineFrame(frame); err == nil ||
				!strings.Contains(err.Error(), "media profile") {
				t.Fatalf("mismatched video %s error = %v", name, err)
			}
		})
	}

	nonMedia := elementMessage("trigger", element.Input, hello.SelectedPorts[2].Type,
		[]byte(`{"prompt":"hello"}`), nil)
	metadata := audioFrameMetadata()
	nonMedia.Envelope.Media = &metadata
	if err := contract.ValidateEngineFrame(nonMedia); err == nil ||
		!strings.Contains(err.Error(), "did not negotiate live media metadata") {
		t.Fatalf("metadata on non-media port error = %v", err)
	}
}

func TestV4MediaMetadataRoundTripsAsProtocolEvidence(t *testing.T) {
	hello := validMediaHello(t)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}
	want := elementMessage("audio", element.Input, hello.SelectedPorts[0].Type,
		[]byte(`{"stream_id":"s1"}`), []byte{1, 2, 3, 4})
	var stream bytes.Buffer
	if err := sidecar.NewWriter(&stream).Write(want); err != nil {
		t.Fatal(err)
	}
	got, err := sidecar.NewReader(&stream).Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.Envelope == nil || got.Envelope.Media == nil || want.Envelope.Media == nil ||
		*got.Envelope.Media != *want.Envelope.Media {
		t.Fatalf("media metadata changed across framing: got %+v want %+v", got.Envelope, want.Envelope)
	}
	if err := contract.ValidateEngineFrame(got); err != nil {
		t.Fatalf("round-tripped media frame: %v", err)
	}
}

func TestElementConformanceFixtureReturnsIndependentEvidence(t *testing.T) {
	hello := validMediaHello(t)
	adapter := sidecar.ArtifactIdentity{ID: "adapter/extra", Revision: "1.0.0"}
	fixture := mediaFixture()
	fixture.Capabilities = []sidecar.CapabilityIdentity{{
		Name: "model.extra", Contract: "test.Contract",
		Provider: sidecar.ArtifactIdentity{ID: "provider/extra", Revision: "1.0.0"},
		Adapter:  &adapter,
	}}
	ready, err := fixture.Ready(hello)
	if err != nil {
		t.Fatal(err)
	}

	fixture.Capabilities[0].Provider.ID = "provider/mutated"
	adapter.ID = "adapter/mutated"
	hello.SelectedPorts[0].Formats[0].Media.SampleRateHz = 96_000
	if ready.ResolvedCapabilities[0].Provider.ID != "provider/extra" ||
		ready.ResolvedCapabilities[0].Adapter == nil ||
		ready.ResolvedCapabilities[0].Adapter.ID != "adapter/extra" {
		t.Fatalf("fixture readiness evidence aliased its inputs: %+v", ready.ResolvedCapabilities[0])
	}
	if got := ready.NegotiatedPorts[0].Format.Media.SampleRateHz; got != 24_000 {
		t.Fatalf("fixture negotiated media profile was mutated through Hello: %d", got)
	}
}

func TestNegotiatedContractOwnsItsHelloAndReadyBaselines(t *testing.T) {
	hello := validMediaHello(t)
	hello.ElementConfig = []byte(`{"voice":"first"}`)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}
	baselineReady := ready.Clone()

	hello.ElementDescriptor.Ports[0].Type.Arguments[0].Name = "audio.Mutated"
	hello.ElementConfig[10] = 'X'
	hello.SelectedPorts[0].Formats[0].Media.SampleRateHz = 96_000
	ready.RuntimeArtifact.Revision = "mutated"
	ready.ResolvedCapabilities[0].Provider.Revision = "mutated"
	ready.NegotiatedPorts[0].Format.Media.SampleRateHz = 96_000

	negotiated, found := contract.NegotiatedPort(element.Input, "audio")
	if !found || negotiated.Format.Media == nil || negotiated.Format.Media.SampleRateHz != 24_000 {
		t.Fatalf("contract negotiation aliases handshake memory: %+v, found=%v", negotiated, found)
	}
	negotiated.Format.Media.SampleRateHz = 12_000
	again, _ := contract.NegotiatedPort(element.Input, "audio")
	if again.Format.Media.SampleRateHz != 24_000 {
		t.Fatalf("NegotiatedPort returned aliased format state: %+v", again)
	}
	if err := contract.ValidateReady(baselineReady); err != nil {
		t.Fatalf("caller mutation changed retained config baseline: %v", err)
	}
}

func TestV4ReadyAttestsTheExactAppliedElementConfig(t *testing.T) {
	nonWire := validMediaHello(t)
	nonWire.ElementConfig = json.RawMessage(" {\n  \"voice\": \"test\"\n}")
	if err := nonWire.Validate(); err == nil || !strings.Contains(err.Error(), "compact on-wire") {
		t.Fatalf("non-wire-stable element config error = %v", err)
	}
	if _, err := mediaFixture().Ready(nonWire); err == nil || !strings.Contains(err.Error(), "compact on-wire") {
		t.Fatalf("fixture accepted config bytes the writer would change: %v", err)
	}

	hello := validMediaHello(t)
	hello.ElementConfig = json.RawMessage(`{"voice":"test"}`)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	want := sidecar.ElementConfigDigest(hello.ElementConfig)
	if ready.AppliedConfigDigest != want {
		t.Fatalf("fixture applied config digest = %q, want %q", ready.AppliedConfigDigest, want)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}

	omitted := ready.Clone()
	omitted.AppliedConfigDigest = ""
	if err := omitted.Validate(); err == nil || !strings.Contains(err.Error(), "config digest") {
		t.Fatalf("omitted config digest error = %v", err)
	}
	nonCanonical := ready.Clone()
	nonCanonical.AppliedConfigDigest = strings.ToUpper(want)
	if err := nonCanonical.Validate(); err == nil || !strings.Contains(err.Error(), "canonical SHA-256") {
		t.Fatalf("non-canonical config digest error = %v", err)
	}
	mismatched := ready.Clone()
	mismatched.AppliedConfigDigest = sidecar.ElementConfigDigest(json.RawMessage(`{"voice":"other"}`))
	if _, err := sidecar.NegotiateElementSession(hello, mismatched); err == nil ||
		!strings.Contains(err.Error(), "expected") {
		t.Fatalf("mismatched config digest error = %v", err)
	}
	if err := contract.ValidateReady(mismatched); err == nil ||
		!strings.Contains(err.Error(), "expected") {
		t.Fatalf("post-ready config mutation error = %v", err)
	}

	reordered := hello.Clone()
	reordered.ElementConfig = json.RawMessage(`{"unused":true,"voice":"test"}`)
	if sidecar.ElementConfigDigest(reordered.ElementConfig) == want {
		t.Fatal("config attestation ignored exact compact bytes")
	}
}

func TestV4RejectsMissingAmbiguousUnofferedAndDriftingNegotiation(t *testing.T) {
	hello := validMediaHello(t)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}

	missingFormat := hello.Clone()
	missingFormat.SelectedPorts[0].Formats = nil
	if err := missingFormat.Validate(); err == nil || !strings.Contains(err.Error(), "wire format") {
		t.Fatalf("missing media format error = %v", err)
	}
	duplicateOffer := hello.Clone()
	duplicateOffer.SelectedPorts[0].Formats = append(duplicateOffer.SelectedPorts[0].Formats,
		duplicateOffer.SelectedPorts[0].Formats[0])
	if err := duplicateOffer.Validate(); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate offer error = %v", err)
	}
	missingNegotiation := ready.Clone()
	missingNegotiation.NegotiatedPorts = missingNegotiation.NegotiatedPorts[1:]
	if _, err := sidecar.NegotiateElementSession(hello, missingNegotiation); err == nil ||
		!strings.Contains(err.Error(), "did not negotiate") {
		t.Fatalf("missing negotiation error = %v", err)
	}
	duplicateNegotiation := ready.Clone()
	duplicateNegotiation.NegotiatedPorts = append(duplicateNegotiation.NegotiatedPorts,
		duplicateNegotiation.NegotiatedPorts[0])
	if _, err := sidecar.NegotiateElementSession(hello, duplicateNegotiation); err == nil ||
		!strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate negotiation error = %v", err)
	}
	unoffered := ready.Clone()
	unoffered.NegotiatedPorts[0].Format.Media.SampleRateHz = 44_100
	if _, err := sidecar.NegotiateElementSession(hello, unoffered); err == nil ||
		!strings.Contains(err.Error(), "unoffered") {
		t.Fatalf("unoffered format error = %v", err)
	}

	ambiguous := ready.Clone()
	for _, capability := range ready.ResolvedCapabilities {
		if capability.Name == sidecar.PortCapabilityName(element.Input, "audio") {
			copy := capability
			copy.Provider.ID = "provider/second-media-model"
			ambiguous.ResolvedCapabilities = append(ambiguous.ResolvedCapabilities, copy)
			break
		}
	}
	if _, err := sidecar.NegotiateElementSession(hello, ambiguous); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous capability error = %v", err)
	}

	runtimeDrift := ready.Clone()
	runtimeDrift.RuntimeArtifact.Revision = "4.3.0"
	if err := contract.ValidateReady(runtimeDrift); err == nil ||
		!strings.Contains(err.Error(), "changed after readiness") {
		t.Fatalf("runtime drift error = %v", err)
	}
	formatDrift := ready.Clone()
	formatDrift.NegotiatedPorts[0].Format = hello.SelectedPorts[0].Formats[1]
	if err := contract.ValidateReady(formatDrift); err == nil ||
		!strings.Contains(err.Error(), "changed after readiness") {
		t.Fatalf("format drift error = %v", err)
	}
}

func TestV4StrictNestedFieldsAndBoundedHeaders(t *testing.T) {
	hello := validMediaHello(t)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}
	frame := elementMessage("trigger", element.Input, hello.SelectedPorts[2].Type,
		[]byte(`{"prompt":"one","prompt":"two"}`), nil)
	if err := contract.ValidateEngineFrame(frame); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("nested duplicate JSON error = %v", err)
	}
	frame = elementMessage("trigger", element.Input, hello.SelectedPorts[2].Type,
		[]byte(`{"prompt":"one"}`), nil)
	frame.Envelope.CausalParents = []string{"same", "same"}
	if err := contract.ValidateEngineFrame(frame); err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Fatalf("duplicate parent error = %v", err)
	}

	unknown := sidecar.NewReader(strings.NewReader("{\"type\":\"bye\",\"invented\":true}\n"))
	if _, err := unknown.Read(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	oversized := sidecar.NewReader(bytes.NewReader(append(
		[]byte("{\"type\":\"log\",\"text\":\""),
		append(bytes.Repeat([]byte{'x'}, (1<<20)+1), []byte("\"}\n")...)...,
	)))
	if _, err := oversized.Read(); err == nil || !strings.Contains(err.Error(), "header exceeds") {
		t.Fatalf("oversized header error = %v", err)
	}
}

func TestV4RejectsFieldsOutsideEachFrameVocabulary(t *testing.T) {
	hello := validMediaHello(t)
	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}

	badHello := hello.Clone()
	badHello.Final = true
	if err := badHello.Validate(); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("unrelated hello field error = %v", err)
	}
	badReady := ready.Clone()
	badReady.Act = "answer"
	if err := badReady.Validate(); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("unrelated ready field error = %v", err)
	}
	badFrame := elementMessage("trigger", element.Input, hello.SelectedPorts[2].Type,
		[]byte(`{"prompt":"hello"}`), nil)
	badFrame.Level = "debug"
	if err := contract.ValidateEngineFrame(badFrame); err == nil ||
		!strings.Contains(err.Error(), "outside its port envelope") {
		t.Fatalf("unrelated element-frame field error = %v", err)
	}
	badLog := sidecar.Message{
		Type: sidecar.TypeLog, Text: "loading",
		RuntimeArtifact: sidecar.ArtifactIdentity{ID: "runtime/drift", Revision: "1.0.0"},
	}
	if err := contract.ValidateSidecarFrame(badLog); err == nil ||
		!strings.Contains(err.Error(), "unrelated fields") {
		t.Fatalf("identity fields on log error = %v", err)
	}
	badBye := sidecar.Message{Type: sidecar.TypeBye, Port: "audio"}
	if err := contract.ValidateEngineFrame(badBye); err == nil ||
		!strings.Contains(err.Error(), "unrelated fields") {
		t.Fatalf("port fields on bye error = %v", err)
	}
}

func TestV4MediaIdentifiersAreCanonicalAndBounded(t *testing.T) {
	hello := validMediaHello(t)
	malformed := hello.Clone()
	malformed.SelectedPorts[0].Formats[0].Media.Encoding = "audio/pcm\nshadow"
	if err := malformed.Validate(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("malformed encoding error = %v", err)
	}
	oversized := hello.Clone()
	oversized.SelectedPorts[0].Formats[0].Media.SampleFormat = strings.Repeat("s", 129)
	if err := oversized.Validate(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("oversized sample format error = %v", err)
	}
}

func TestV4ExplicitWireFormatsRatherThanTypeNamesOwnMediaSemantics(t *testing.T) {
	hello := validMediaHello(t)
	explicitMedia := hello.Clone()
	explicitMedia.SelectedPorts[2].Formats = []sidecar.WireFormat{audioWire(24_000)}
	ready, err := mediaFixture().Ready(explicitMedia)
	if err != nil {
		t.Fatalf("explicit media codec on arbitrary typed port: %v", err)
	}
	contract, err := sidecar.NegotiateElementSession(explicitMedia, ready)
	if err != nil {
		t.Fatal(err)
	}
	frame := elementMessage("trigger", element.Input, explicitMedia.SelectedPorts[2].Type,
		[]byte(`{"prompt":"binary codec"}`), []byte{1, 2, 3, 4})
	metadata := audioFrameMetadata()
	frame.Envelope.Media = &metadata
	if err := contract.ValidateEngineFrame(frame); err != nil {
		t.Fatalf("arbitrary typed port with explicit media codec: %v", err)
	}

	jsonAudio := hello.Clone()
	jsonAudio.SelectedPorts[0].Formats = []sidecar.WireFormat{sidecar.JSONWireFormat()}
	ready, err = mediaFixture().Ready(jsonAudio)
	if err != nil {
		t.Fatalf("JSON codec for audio-named graph type: %v", err)
	}
	contract, err = sidecar.NegotiateElementSession(jsonAudio, ready)
	if err != nil {
		t.Fatal(err)
	}
	jsonFrame := elementMessage("audio", element.Input, jsonAudio.SelectedPorts[0].Type,
		[]byte(`{"reference":"artifact://frame-1"}`), nil)
	if err := contract.ValidateEngineFrame(jsonFrame); err != nil {
		t.Fatalf("audio-named type was given transport authority over an explicit JSON codec: %v", err)
	}

	assertV4AmbiguousIdentityBytes(t, hello)
}

func assertV4AmbiguousIdentityBytes(t *testing.T, hello sidecar.Message) {
	t.Helper()
	badRequirement := hello.Clone()
	badRequirement.RequiredCapabilities = []sidecar.CapabilityRequirement{{
		Name: "model.feature", Contract: "contract\x00shadow",
	}}
	if err := badRequirement.Validate(); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("ambiguous requirement contract error = %v", err)
	}

	ready, err := mediaFixture().Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	badCapability := ready.Clone()
	badCapability.ResolvedCapabilities[0].Contract += "\nshadow"
	if err := badCapability.Validate(); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("ambiguous capability contract error = %v", err)
	}
	badRuntime := ready.Clone()
	badRuntime.RuntimeArtifact.ID += "\x00shadow"
	if err := badRuntime.Validate(); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("ambiguous runtime identity error = %v", err)
	}
	badDigest := sidecar.ArtifactIdentity{
		ID: "runtime/digested", Digest: "sha256:" + strings.Repeat("AB", 32),
	}
	if err := badDigest.Validate(); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("uppercase digest error = %v", err)
	}

	oversizedPort := elementMessage(strings.Repeat("p", sidecar.MaxElementIdentifierBytes+1),
		element.Input, hello.SelectedPorts[2].Type, []byte(`{"prompt":"hello"}`), nil)
	if err := oversizedPort.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized frame port error = %v", err)
	}
}

func TestV4RejectsSelectedPortsWhoseDerivedCapabilityCannotBeBounded(t *testing.T) {
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.OversizedContract", Revision: 1,
		Ports: []element.Port{{
			Name: "trigger", Direction: element.Input,
			Type:        element.Trigger(element.Named(strings.Repeat("T", sidecar.MaxElementIdentifierBytes))),
			Cardinality: element.One,
		}},
	}
	hello := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &descriptor,
		SelectedPorts: []sidecar.PortSelection{sidecar.JSONPortSelection(
			"trigger", element.Input, descriptor.Ports[0].Type,
		)},
	}
	if err := hello.Validate(); err == nil || !strings.Contains(err.Error(), "capability contract exceeds") {
		t.Fatalf("unprovable derived port capability error = %v", err)
	}
}

func TestV4ReservesDerivedPortCapabilityRequirements(t *testing.T) {
	hello := validMediaHello(t)
	hello.RequiredCapabilities = []sidecar.CapabilityRequirement{{
		Name:     sidecar.PortCapabilityName(element.Input, "audio"),
		Contract: hello.SelectedPorts[0].Type.String(),
	}}
	if err := hello.Validate(); err == nil || !strings.Contains(err.Error(), "reserved port capability namespace") {
		t.Fatalf("explicit derived-port requirement error = %v", err)
	}
}

func TestMediaKindDistinguishesInlineBodiesFromTypedReferences(t *testing.T) {
	for _, body := range []element.Type{
		element.Stream(element.Named("audio.InputFrame")),
		element.Event(element.Named("video.InlineFrame")),
		element.Trigger(element.Named("image.FrameBatch")),
		element.Segmented(element.Named("Prepared", element.Named("speech.AudioFrame")),
			element.Named("speech.UtteranceID")),
	} {
		if _, found, err := sidecar.MediaKindForType(body); err != nil || !found {
			t.Fatalf("in-band media type %s = found %v, error %v", body.String(), found, err)
		}
	}
	for _, reference := range []element.Type{
		element.Event(element.Named("video.FrameReference")),
		element.Stream(element.Named("video.ReferenceBatch")),
		element.State(element.Named("video.FrameRate")),
	} {
		if kind, found, err := sidecar.MediaKindForType(reference); err != nil || found {
			t.Fatalf("media reference type %s = kind %q found %v, error %v",
				reference.String(), kind, found, err)
		}
	}
}

func elementMessage(
	port string, direction element.Direction, valueType element.Type,
	jsonBody, binaryBody []byte,
) sidecar.Message {
	wire := sidecar.WireEnvelope{
		Type: valueType.Clone(), ItemID: port + "-item", JSON: append([]byte(nil), jsonBody...),
	}
	if len(binaryBody) != 0 {
		var metadata sidecar.MediaFrameMetadata
		switch port {
		case "audio", "audio_out":
			metadata = audioFrameMetadata()
		case "video":
			metadata = videoFrameMetadata()
		}
		if metadata.Kind != "" {
			wire.Media = &metadata
		}
	}
	message := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: port, Envelope: &wire,
		Payload: append([]byte(nil), binaryBody...),
	}
	message.PayloadBytes = len(message.Payload)
	_ = direction // direction is documented at each call site and checked by the contract.
	return message
}

func audioFrameMetadata() sidecar.MediaFrameMetadata {
	return sidecar.MediaFrameMetadata{
		Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
		SampleRateHz: 24_000, Channels: 1, FrameDurationMS: 20,
	}
}

func videoFrameMetadata() sidecar.MediaFrameMetadata {
	return sidecar.MediaFrameMetadata{
		Kind: sidecar.MediaVideo, Encoding: "image/jpeg", Width: 1280, Height: 720,
		FrameRateMilliHz: 30_000,
	}
}
