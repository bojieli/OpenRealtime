package model

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/elements/acoustic"
	"github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
)

func TestStandardCodecReconstructsGraphNativeAudioAndCognition(t *testing.T) {
	codec := NewStandardJSONCodec()
	inputPCM := make([]byte, 960)
	for index := range inputPCM {
		inputPCM[index] = byte(index % 251)
	}
	input := acoustic.InputFrame{
		StreamID: "microphone-1",
		Frame: perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 10,
			PCM16LE: inputPCM, SampleRateHz: 24_000,
		},
	}
	encodedInput, err := codec.Encode(AudioInputType(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedInput.Binary, inputPCM) || encodedInput.Media == nil ||
		encodedInput.Media.FrameDurationMS != 20 || encodedInput.Media.SampleRateHz != 24_000 {
		t.Fatalf("encoded native audio = %+v", encodedInput)
	}
	if bytes.Contains(encodedInput.JSON, []byte("AAECAw")) {
		t.Fatalf("PCM leaked into audio JSON: %s", encodedInput.JSON)
	}
	decodedInput, err := codec.Decode(AudioInputType(), encodedInput)
	if err != nil {
		t.Fatal(err)
	}
	nativeInput, ok := decodedInput.(*acoustic.InputFrame)
	if !ok || nativeInput.StreamID != input.StreamID || !bytes.Equal(nativeInput.Frame.PCM16LE, inputPCM) {
		t.Fatalf("decoded native audio = %T %#v", decodedInput, decodedInput)
	}
	encodedInput.Binary[0] ^= 0xff
	if nativeInput.Frame.PCM16LE[0] == encodedInput.Binary[0] {
		t.Fatal("decoded native audio aliases wire memory")
	}

	image := []byte{0xff, 0xd8, 0xff, 0xd9}
	video := VideoInputFrame{
		StreamID: "camera-front", FrameRateMilliHz: 30_000,
		Frame: perception.Frame{
			Kind: perception.FrameImage, Source: "camera.front", CapturedNS: 11,
			Image: image, MIMEType: "image/jpeg", Width: 640, Height: 480,
		},
	}
	encodedVideo, err := codec.Encode(VideoInputType(), video)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedVideo.Binary, image) || encodedVideo.Media == nil ||
		encodedVideo.Media.Encoding != "image/jpeg" || encodedVideo.Media.Width != 640 ||
		encodedVideo.Media.Height != 480 || encodedVideo.Media.FrameRateMilliHz != 30_000 {
		t.Fatalf("encoded native video = %+v", encodedVideo)
	}
	decodedVideo, err := codec.Decode(VideoInputType(), encodedVideo)
	if err != nil {
		t.Fatal(err)
	}
	nativeVideo, ok := decodedVideo.(*VideoInputFrame)
	if !ok || nativeVideo.StreamID != video.StreamID || !bytes.Equal(nativeVideo.Frame.Image, image) {
		t.Fatalf("decoded native video = %T %#v", decodedVideo, decodedVideo)
	}
	encodedVideo.Binary[0] ^= 0xff
	if nativeVideo.Frame.Image[0] == encodedVideo.Binary[0] {
		t.Fatal("decoded native video aliases wire memory")
	}

	preparedPCM := make([]byte, 960)
	prepared := speech.AudioFrame{
		Kind: speech.AudioChunk, UtteranceID: "utterance-1",
		Chunk: v1.SpeechChunk{
			ChunkID: "chunk-1", CandidateID: "utterance-1", SampleRateHz: 24_000,
			PCM16LE: preparedPCM, Final: true,
		},
	}
	encodedPrepared, err := codec.Encode(PreparedAudioType(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	var metadataOnly speech.AudioFrame
	if err := json.Unmarshal(encodedPrepared.JSON, &metadataOnly); err != nil {
		t.Fatal(err)
	}
	if len(metadataOnly.Chunk.PCM16LE) != 0 || !bytes.Equal(encodedPrepared.Binary, preparedPCM) {
		t.Fatalf("prepared audio did not split metadata/body: JSON=%s binary=%d",
			encodedPrepared.JSON, len(encodedPrepared.Binary))
	}
	decodedPrepared, err := codec.Decode(PreparedAudioType(), encodedPrepared)
	if err != nil {
		t.Fatal(err)
	}
	nativePrepared, ok := decodedPrepared.(*speech.AudioFrame)
	if !ok || nativePrepared.Kind != speech.AudioChunk ||
		!bytes.Equal(nativePrepared.Chunk.PCM16LE, preparedPCM) {
		t.Fatalf("decoded prepared audio = %T %#v", decodedPrepared, decodedPrepared)
	}

	outcome := cognition.Outcome{
		Kind: cognition.OutcomeSucceeded, Operation: "generate", RunID: "run-1", DurationNS: 42,
	}
	encodedOutcome, err := codec.Encode(OutcomeType(), outcome)
	if err != nil {
		t.Fatal(err)
	}
	decodedOutcome, err := codec.Decode(OutcomeType(), encodedOutcome)
	if err != nil {
		t.Fatal(err)
	}
	nativeOutcome, ok := decodedOutcome.(*cognition.Outcome)
	if !ok || *nativeOutcome != outcome {
		t.Fatalf("decoded cognition outcome = %T %#v", decodedOutcome, decodedOutcome)
	}

	activity := binding.ActivityEvent{Started: true, ItemID: "speech-1", AudioStartMS: 120}
	encodedActivity, err := codec.Encode(ActivityType(), activity)
	if err != nil {
		t.Fatal(err)
	}
	decodedActivity, err := codec.Decode(ActivityType(), encodedActivity)
	if err != nil {
		t.Fatal(err)
	}
	nativeActivity, ok := decodedActivity.(*binding.ActivityEvent)
	if !ok || *nativeActivity != activity {
		t.Fatalf("decoded native activity = %T %#v", decodedActivity, decodedActivity)
	}
}

func BenchmarkStandardCodecAudioRoundTrip(b *testing.B) {
	codec := NewStandardJSONCodec()
	input := acoustic.InputFrame{
		StreamID: "microphone-1",
		Frame: perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 10,
			PCM16LE: make([]byte, 960), SampleRateHz: 24_000,
		},
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(input.Frame.PCM16LE)))
	b.ResetTimer()
	for range b.N {
		encoded, err := codec.Encode(AudioInputType(), input)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := codec.Decode(AudioInputType(), encoded); err != nil {
			b.Fatal(err)
		}
	}
}

func TestStandardCodecRejectsConflictingMediaEvidence(t *testing.T) {
	codec := NewStandardJSONCodec()
	prepared := speech.AudioFrame{
		Kind: speech.AudioChunk, UtteranceID: "utterance-1",
		Chunk: v1.SpeechChunk{
			ChunkID: "chunk-1", CandidateID: "utterance-1", SampleRateHz: 24_000,
			PCM16LE: make([]byte, 960),
		},
	}
	encoded, err := codec.Encode(PreparedAudioType(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	wrongRate := encoded.clone()
	wrongRate.Media.SampleRateHz = 48_000
	if _, err := codec.Decode(PreparedAudioType(), wrongRate); err == nil ||
		!strings.Contains(err.Error(), "metadata mismatch") {
		t.Fatalf("conflicting audio rate error = %v", err)
	}

	duplicated := encoded.clone()
	duplicated.JSON, err = json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(PreparedAudioType(), duplicated); err == nil ||
		!strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicated PCM error = %v", err)
	}

	metadataOnly := EncodedPayload{
		JSON: encoded.JSON,
		Media: &sidecar.MediaFrameMetadata{
			Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
			SampleRateHz: 24_000, Channels: 1, FrameDurationMS: 20,
		},
	}
	if _, err := codec.Decode(PreparedAudioType(), metadataOnly); err == nil ||
		!strings.Contains(err.Error(), "requires a binary body") {
		t.Fatalf("metadata-without-body error = %v", err)
	}

	conflictingAudio := acoustic.InputFrame{
		StreamID: "microphone-1",
		Frame: perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", PCM16LE: make([]byte, 960),
			SampleRateHz: 24_000, MIMEType: "image/jpeg", Width: 1, Height: 1,
		},
	}
	if _, err := codec.Encode(AudioInputType(), conflictingAudio); err == nil ||
		!strings.Contains(err.Error(), "image fields") {
		t.Fatalf("audio/image field conflict error = %v", err)
	}
	conflictingVideo := VideoInputFrame{
		StreamID: "camera-1", FrameRateMilliHz: 30_000,
		Frame: perception.Frame{
			Kind: perception.FrameImage, Source: "camera", Image: []byte{1},
			MIMEType: "image/jpeg", Width: 1, Height: 1, SampleRateHz: 24_000,
		},
	}
	if _, err := codec.Encode(VideoInputType(), conflictingVideo); err == nil ||
		!strings.Contains(err.Error(), "audio fields") {
		t.Fatalf("video/audio field conflict error = %v", err)
	}
}
