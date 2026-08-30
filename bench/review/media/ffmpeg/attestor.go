package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// Attestor is separately owned from Encoder and proves that the exact output
// bytes fully decode with one audio and one video stream at the declared PTS.
type Attestor struct {
	state          pluginState
	chain          toolchain
	guard          sensitiveGuard
	descriptor     reviewmedia.AttestorDescriptor
	implementation []byte
	configuration  []byte
}

// NewAttestor resolves and fingerprints independent ffmpeg/ffprobe execution
// capabilities. A recorder must claim this instance separately from Encoder.
func NewAttestor(ctx context.Context, options Options) (*Attestor, error) {
	guard, err := newSensitiveGuard(options.SensitiveValues)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Attestor, error) {
		guard.destroy()
		return nil, guard.redactError(ctx, "construct ffmpeg media attestor", err)
	}
	chain, err := buildToolchain(ctx, options, true, guard)
	if err != nil {
		return fail(err)
	}
	implementation, err := implementationSource()
	if err != nil {
		return fail(err)
	}
	configuration, err := chain.configuration("full_decode_attestor", json.RawMessage(fixedAttestationIdentity))
	if err != nil {
		return fail(err)
	}
	if guard.rejects(implementation) || guard.rejects(configuration) {
		return fail(errors.New("ffmpeg attestor provenance contains a declared sensitive value"))
	}
	descriptor := reviewmedia.AttestorDescriptor{
		Name:       "ffmpeg-full-decode-sandboxed",
		Version:    chain.ffmpegVersion + "_" + chain.ffprobeVersion,
		Capability: reviewmedia.FullDecodeAttestationCapability,
		Implementation: review.ContentIdentity{
			Version: "openrealtime.ffmpeg-attestation.impl.v4", SHA256: digest(implementation),
		},
		ConfigurationSHA256: digest(configuration),
	}
	if err := descriptor.Validate(); err != nil {
		return fail(err)
	}
	return &Attestor{
		chain: chain, guard: guard, descriptor: descriptor,
		implementation: implementation, configuration: configuration,
	}, nil
}

func (attestor *Attestor) Descriptor() reviewmedia.AttestorDescriptor {
	if attestor == nil {
		return reviewmedia.AttestorDescriptor{}
	}
	attestor.state.mu.RLock()
	defer attestor.state.mu.RUnlock()
	if attestor.state.closed {
		return reviewmedia.AttestorDescriptor{}
	}
	return attestor.descriptor
}

func (attestor *Attestor) Implementation() []byte {
	if attestor == nil {
		return nil
	}
	attestor.state.mu.RLock()
	defer attestor.state.mu.RUnlock()
	return cloneBytes(attestor.implementation)
}

func (attestor *Attestor) Configuration() []byte {
	if attestor == nil {
		return nil
	}
	attestor.state.mu.RLock()
	defer attestor.state.mu.RUnlock()
	return cloneBytes(attestor.configuration)
}

func (attestor *Attestor) Claim(ctx context.Context) error {
	if attestor == nil {
		return errors.New("claim ffmpeg media attestor: nil plug-in")
	}
	return attestor.state.claim(ctx, "ffmpeg media attestor")
}

func (attestor *Attestor) Attest(
	ctx context.Context, request reviewmedia.AttestationRequest,
) (result reviewmedia.Attestation, returnErr error) {
	if attestor == nil {
		return result, errors.New("ffmpeg media attestor is nil")
	}
	defer func() {
		returnErr = attestor.guard.redactError(ctx, "ffmpeg full-decode attestation", returnErr)
		if returnErr != nil {
			result = reviewmedia.Attestation{}
		}
	}()
	release, err := attestor.state.begin(ctx, "ffmpeg media attestor")
	if err != nil {
		return result, err
	}
	defer release()
	if err := validateAttestationRequest(request); err != nil {
		return result, err
	}
	if err := attestor.chain.recheck(ctx); err != nil {
		return result, err
	}
	root, _, err := snapshotAttestationInput(ctx, request)
	if err != nil {
		return result, err
	}
	defer func() {
		if cleanupErr := removePrivateTree(root); returnErr == nil && cleanupErr != nil {
			returnErr = errors.New("clean private ffmpeg attestation snapshot")
			result = reviewmedia.Attestation{}
		}
	}()
	mounts := &sandboxMounts{inputRoot: root}
	streamProbe, err := attestor.probe(ctx, mounts, []string{
		"-v", "error", "-show_entries",
		"stream=index,codec_type,codec_name,pix_fmt,width,height,sample_rate,channels,start_time,duration,time_base,nb_frames:format=format_name,start_time,duration",
		"-of", "json", "/work/input/output.mp4",
	})
	if err != nil {
		return result, err
	}
	videoProbe, err := attestor.probe(ctx, mounts, []string{
		"-v", "error", "-select_streams", "v:0", "-show_entries",
		"frame=best_effort_timestamp_time,pkt_duration_time", "-of", "json",
		"-show_frames", "/work/input/output.mp4",
	})
	if err != nil {
		return result, err
	}
	videoPacketProbe, err := attestor.probe(ctx, mounts, []string{
		"-v", "error", "-ignore_editlist", "1", "-select_streams", "v:0", "-show_entries",
		"packet=pts_time,duration_time", "-of", "json", "-show_packets", "/work/input/output.mp4",
	})
	if err != nil {
		return result, err
	}
	audioProbe, err := attestor.probe(ctx, mounts, []string{
		"-v", "error", "-select_streams", "a:0", "-show_entries",
		"frame=best_effort_timestamp,pkt_duration,nb_samples", "-of", "json",
		"-show_frames", "/work/input/output.mp4",
	})
	if err != nil {
		return result, err
	}
	decodeDiagnostic, err := attestor.chain.run(ctx, attestor.chain.ffmpeg, mounts, []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode",
		"-i", "/work/input/output.mp4", "-map", "0:v:0", "-map", "0:a:0",
		"-vsync", "0", "-f", "null", "-",
	}, maximumDiagnosticBytes)
	if err != nil {
		return result, attestor.guard.safeError(ctx, "full decode exact ffmpeg output", err, decodeDiagnostic)
	}
	if len(decodeDiagnostic) != 0 {
		return result, errors.New("full decode unexpectedly wrote standard output")
	}
	var audioTimeline audioTimelineEvidence
	spec, audioFrameCount, err := parseProbes(
		streamProbe, videoProbe, videoPacketProbe, audioProbe, request, &audioTimeline)
	if err != nil {
		return result, err
	}
	finalSHA, finalBytes, err := fileDigestContext(ctx, request.OutputPath, maximumOutputBytes)
	if err != nil || finalSHA != request.ExpectedOutputSHA256 || finalBytes != request.ExpectedOutputBytes {
		return result, errors.New("attested output changed during full decode")
	}
	if err := attestor.chain.recheck(ctx); err != nil {
		return result, err
	}
	geometry := yuv420pGeometry(request.Width, request.Height)
	report := attestationReport{
		Schema:                   "openrealtime.ffmpeg-full-decode-attestation.v4",
		Capability:               reviewmedia.FullDecodeAttestationCapability,
		SourceGeometry:           geometryEvidence{Width: geometry.sourceWidth, Height: geometry.sourceHeight},
		EncodedGeometry:          geometryEvidence{Width: geometry.encodedWidth, Height: geometry.encodedHeight},
		GeometryPolicy:           reviewmedia.YUV420PPadRightBottomBlackToEvenV1,
		PaddingRightPixels:       geometry.paddingRight(),
		PaddingBottomPixels:      geometry.paddingBottom(),
		ExpectedOutputSHA256:     request.ExpectedOutputSHA256,
		ExpectedOutputBytes:      request.ExpectedOutputBytes,
		ExpectedFramePTSUSSHA256: request.ExpectedFramePTSUSSHA256,
		StreamProbeSHA256:        digest(streamProbe), VideoFrameProbeSHA256: digest(videoProbe),
		VideoPacketProbeSHA256: digest(videoPacketProbe), AudioFrameProbeSHA256: digest(audioProbe),
		AudioFrameCount:         audioFrameCount,
		AudioDecodedSamples:     audioTimeline.DecodedSamples,
		AudioPresentedSamples:   audioTimeline.PresentedSamples,
		AudioTailPaddingSamples: audioTimeline.TailPaddingSamples,
		FullDecodeCompleted:     true, Spec: spec,
	}
	reportBytes, err := json.Marshal(report)
	if err != nil || strictjson.Validate(reportBytes) != nil || attestor.guard.rejects(reportBytes) {
		return result, errors.New("encode bounded full-decode attestation report")
	}
	return reviewmedia.Attestation{Spec: spec, Report: reportBytes}, nil
}

func (attestor *Attestor) probe(
	ctx context.Context, mounts *sandboxMounts, arguments []string,
) ([]byte, error) {
	output, err := attestor.chain.run(ctx, attestor.chain.ffprobe, mounts, arguments, maximumProbeBytes)
	if err != nil {
		return nil, attestor.guard.safeError(ctx, "probe exact ffmpeg output", err, output)
	}
	if len(output) == 0 || len(output) > maximumProbeBytes || strictjson.Validate(output) != nil ||
		attestor.guard.rejects(output) {
		return nil, errors.New("ffprobe returned invalid, sensitive, or oversized JSON")
	}
	return output, nil
}

func (attestor *Attestor) Close() error {
	if attestor == nil {
		return nil
	}
	attestor.state.mu.Lock()
	if attestor.state.closed {
		attestor.state.mu.Unlock()
		return nil
	}
	attestor.state.closed = true
	clear(attestor.implementation)
	clear(attestor.configuration)
	attestor.implementation = nil
	attestor.configuration = nil
	attestor.chain = toolchain{}
	attestor.descriptor = reviewmedia.AttestorDescriptor{}
	attestor.state.mu.Unlock()
	attestor.guard.destroy()
	return nil
}

type probeEnvelope struct {
	Programs []json.RawMessage `json:"programs"`
	Streams  []probeStream     `json:"streams"`
	Format   probeFormat       `json:"format"`
}

type probeStream struct {
	Index       int    `json:"index"`
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	PixelFormat string `json:"pix_fmt"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	SampleRate  string `json:"sample_rate"`
	Channels    int    `json:"channels"`
	StartTime   string `json:"start_time"`
	Duration    string `json:"duration"`
	TimeBase    string `json:"time_base"`
	FrameCount  string `json:"nb_frames"`
}

type probeFormat struct {
	Name      string `json:"format_name"`
	StartTime string `json:"start_time"`
	Duration  string `json:"duration"`
}

type frameEnvelope struct {
	Frames []probeFrame `json:"frames"`
}

type probeFrame struct {
	PTS      string `json:"best_effort_timestamp_time"`
	Duration string `json:"pkt_duration_time"`
}

type audioFrameEnvelope struct {
	Frames []probeAudioFrame `json:"frames"`
}

type probeAudioFrame struct {
	PTS      int64 `json:"best_effort_timestamp"`
	Duration int64 `json:"pkt_duration"`
	Samples  int64 `json:"nb_samples"`
}

type audioTimelineEvidence struct {
	FrameCount         int
	DecodedSamples     int64
	PresentedSamples   int64
	TailPaddingSamples int64
}

type packetEnvelope struct {
	Packets []probePacket `json:"packets"`
}

type probePacket struct {
	PTS      string `json:"pts_time"`
	Duration string `json:"duration_time"`
}

func parseProbes(
	streamPayload, videoPayload, videoPacketPayload, audioPayload []byte,
	request reviewmedia.AttestationRequest, audioTimeline *audioTimelineEvidence,
) (reviewmedia.PlayableSpec, int, error) {
	if audioTimeline == nil {
		return reviewmedia.PlayableSpec{}, 0, errors.New("ffprobe audio timeline output is nil")
	}
	var streams probeEnvelope
	if err := decodeExactJSON(streamPayload, &streams); err != nil {
		return reviewmedia.PlayableSpec{}, 0, fmt.Errorf("ffprobe stream report JSON: %w", err)
	}
	if len(streams.Programs) != 0 || len(streams.Streams) != 2 || !containsCSVToken(streams.Format.Name, "mp4") {
		return reviewmedia.PlayableSpec{}, 0, fmt.Errorf(
			"ffprobe stream report is not one MP4 audio/video pair (streams=%d format=%q)",
			len(streams.Streams), streams.Format.Name)
	}
	formatStart, err := parseDecimalUS(streams.Format.StartTime)
	if err != nil || formatStart != 0 {
		return reviewmedia.PlayableSpec{}, 0, errors.New("ffprobe format does not start at zero")
	}
	formatDuration, err := parseDecimalUS(streams.Format.Duration)
	if err != nil || formatDuration != request.AttemptEndUS {
		return reviewmedia.PlayableSpec{}, 0, fmt.Errorf(
			"ffprobe format duration does not exactly cover the attempt (got=%d want=%d raw=%q)",
			formatDuration, request.AttemptEndUS, streams.Format.Duration)
	}
	var video, audio *probeStream
	for index := range streams.Streams {
		stream := &streams.Streams[index]
		switch stream.CodecType {
		case "video":
			if video != nil {
				return reviewmedia.PlayableSpec{}, 0, errors.New("ffprobe contains multiple video streams")
			}
			video = stream
		case "audio":
			if audio != nil {
				return reviewmedia.PlayableSpec{}, 0, errors.New("ffprobe contains multiple audio streams")
			}
			audio = stream
		default:
			return reviewmedia.PlayableSpec{}, 0, errors.New("ffprobe contains an undeclared stream")
		}
	}
	geometry := yuv420pGeometry(request.Width, request.Height)
	if video == nil || audio == nil || video.Index == audio.Index || video.CodecName != "h264" ||
		video.PixelFormat != "yuv420p" || video.Width != geometry.encodedWidth ||
		video.Height != geometry.encodedHeight ||
		video.TimeBase != "1/1000000" || audio.CodecName != "aac" || audio.SampleRate != "24000" ||
		audio.Channels != 2 || audio.TimeBase != "1/24000" {
		return reviewmedia.PlayableSpec{}, 0, errors.New("ffprobe stream codecs, shape, or time bases violate the exact contract")
	}
	videoStart, videoEnd, err := streamExtent(*video)
	if err != nil || videoStart != request.FirstFrameUS || videoEnd != request.AttemptEndUS {
		return reviewmedia.PlayableSpec{}, 0, fmt.Errorf(
			"ffprobe video stream extent violates the exact attempt timing (got=%d..%d want=%d..%d raw=%q+%q)",
			videoStart, videoEnd, request.FirstFrameUS, request.AttemptEndUS, video.StartTime, video.Duration)
	}
	audioStart, audioEnd, err := streamExtent(*audio)
	if err != nil || audioStart != 0 || audioEnd != request.AttemptEndUS {
		return reviewmedia.PlayableSpec{}, 0, fmt.Errorf(
			"ffprobe audio stream extent violates the exact attempt timing (got=%d..%d want=0..%d raw=%q+%q)",
			audioStart, audioEnd, request.AttemptEndUS, audio.StartTime, audio.Duration)
	}
	videoPTS, err := parseVideoFramePTS(videoPayload, request.ExpectedFrameCount,
		request.FirstFrameUS)
	if err != nil || digestPTS(videoPTS) != request.ExpectedFramePTSUSSHA256 {
		return reviewmedia.PlayableSpec{}, 0,
			errors.New("decoded video frame PTS do not match the retained evidence timeline")
	}
	packetPTS, err := parseVideoPacketTimeline(videoPacketPayload, request.ExpectedFrameCount,
		0, request.AttemptEndUS-request.FirstFrameUS)
	if err != nil {
		return reviewmedia.PlayableSpec{}, 0,
			fmt.Errorf("encoded video packet timing does not exactly cover the retained evidence timeline: %w", err)
	}
	for index := range packetPTS {
		packetPTS[index] += request.FirstFrameUS
	}
	if digestPTS(packetPTS) != request.ExpectedFramePTSUSSHA256 {
		return reviewmedia.PlayableSpec{}, 0,
			errors.New("encoded video packet PTS digest does not match the retained evidence timeline")
	}
	audioEvidence, err := parseAudioFrameTimeline(audioPayload, request.AttemptEndUS)
	if err != nil {
		return reviewmedia.PlayableSpec{}, 0,
			fmt.Errorf("decoded audio frames do not continuously cover the attempt: %w", err)
	}
	*audioTimeline = audioEvidence
	if video.FrameCount != "" && video.FrameCount != "N/A" {
		count, parseErr := strconv.Atoi(video.FrameCount)
		if parseErr != nil || count != request.ExpectedFrameCount {
			return reviewmedia.PlayableSpec{}, 0, errors.New("video stream frame count disagrees with decoded frame count")
		}
	}
	spec := reviewmedia.PlayableSpec{
		OutputSHA256: request.ExpectedOutputSHA256, Container: "mp4",
		VideoCodec: "h264", PixelFormat: "yuv420p",
		Width: request.Width, Height: request.Height,
		EncodedWidth: video.Width, EncodedHeight: video.Height,
		GeometryPolicy: reviewmedia.YUV420PPadRightBottomBlackToEvenV1,
		AudioCodec:     "aac", AudioSampleRateHz: 24_000, AudioChannels: 2,
		DurationUS: request.AttemptEndUS, AudioStartUS: audioStart, AudioEndUS: audioEnd,
		VideoStartUS: videoStart, VideoEndUS: videoEnd, VideoFrameCount: len(videoPTS),
		VideoFramePTSUSSHA256: digestPTS(videoPTS),
	}
	return spec, audioEvidence.FrameCount, nil
}

func streamExtent(stream probeStream) (int64, int64, error) {
	start, err := parseDecimalUS(stream.StartTime)
	if err != nil {
		return 0, 0, err
	}
	duration, err := parseDecimalUS(stream.Duration)
	if err != nil || duration <= 0 || start > math.MaxInt64-duration {
		return 0, 0, errors.New("ffprobe stream duration is invalid")
	}
	return start, start + duration, nil
}

func parseAudioFrameTimeline(payload []byte, attemptEndUS int64) (audioTimelineEvidence, error) {
	var envelope audioFrameEnvelope
	if err := decodeJSONSubset(payload, &envelope); err != nil || len(envelope.Frames) == 0 ||
		len(envelope.Frames) > 1_000_000 || attemptEndUS <= 0 ||
		attemptEndUS > (math.MaxInt64-999_999)/24_000 {
		return audioTimelineEvidence{}, errors.New("ffprobe decoded audio frame report has an invalid shape")
	}
	// AAC timestamps are exact integer samples at the stream's independently
	// attested 24 kHz time base. A presentation edit may end between sample
	// boundaries, so decoded coverage ends on the first sample boundary at or
	wantedEndSamples := (attemptEndUS*24_000 + 999_999) / 1_000_000
	pts := make([]int64, len(envelope.Frames))
	durations := make([]int64, len(envelope.Frames))
	decodedSamples := int64(0)
	for index, frame := range envelope.Frames {
		if frame.PTS < 0 || frame.Duration <= 0 || frame.Samples <= 0 || frame.Samples > 8192 ||
			frame.Duration > frame.Samples || index < len(envelope.Frames)-1 && frame.Duration != frame.Samples {
			return audioTimelineEvidence{}, fmt.Errorf(
				"ffprobe decoded audio frame shape is invalid (index=%d pts=%d duration=%d samples=%d)",
				index, frame.PTS, frame.Duration, frame.Samples)
		}
		pts[index], durations[index] = frame.PTS, frame.Duration
		decodedSamples += frame.Samples
		if index > 0 && (pts[index] <= pts[index-1] ||
			pts[index-1] > math.MaxInt64-durations[index-1] ||
			pts[index-1]+durations[index-1] != pts[index]) {
			return audioTimelineEvidence{}, errors.New("ffprobe decoded audio frames are not strictly increasing and continuous")
		}
	}
	last := len(pts) - 1
	if pts[0] != 0 || pts[last] > math.MaxInt64-durations[last] ||
		pts[last]+durations[last] != wantedEndSamples {
		return audioTimelineEvidence{}, fmt.Errorf(
			"ffprobe decoded audio frames do not cover the declared presentation (got=%d..%d want=0..%d)",
			pts[0], pts[last]+durations[last], wantedEndSamples)
	}
	if decodedSamples < wantedEndSamples || decodedSamples-wantedEndSamples >= envelope.Frames[last].Samples {
		return audioTimelineEvidence{}, errors.New("ffprobe decoded AAC padding is invalid")
	}
	return audioTimelineEvidence{
		FrameCount: len(envelope.Frames), DecodedSamples: decodedSamples,
		PresentedSamples: wantedEndSamples, TailPaddingSamples: decodedSamples - wantedEndSamples,
	}, nil
}

func parseVideoFramePTS(payload []byte, expectedCount int, expectedStart int64) ([]int64, error) {
	var envelope frameEnvelope
	if err := decodeJSONSubset(payload, &envelope); err != nil || expectedCount <= 0 ||
		len(envelope.Frames) != expectedCount || len(envelope.Frames) > maximumFrames {
		return nil, errors.New("ffprobe decoded video frame report has an invalid shape")
	}
	pts := make([]int64, len(envelope.Frames))
	for index, frame := range envelope.Frames {
		value, err := parseDecimalUS(frame.PTS)
		if err != nil || value < 0 || index > 0 && value <= pts[index-1] {
			return nil, errors.New("ffprobe decoded video frame PTS are invalid")
		}
		pts[index] = value
	}
	if pts[0] != expectedStart {
		return nil, errors.New("ffprobe decoded video does not start at the declared first frame")
	}
	return pts, nil
}

func parseVideoPacketTimeline(
	payload []byte, expectedCount int, expectedStart, expectedEnd int64,
) ([]int64, error) {
	var envelope packetEnvelope
	if err := decodeJSONSubset(payload, &envelope); err != nil || expectedCount <= 0 ||
		len(envelope.Packets) != expectedCount || len(envelope.Packets) > maximumFrames {
		return nil, errors.New("ffprobe video packet report has an invalid shape")
	}
	pts := make([]int64, len(envelope.Packets))
	durations := make([]int64, len(envelope.Packets))
	for index, packet := range envelope.Packets {
		value, err := parseDecimalUS(packet.PTS)
		if err != nil || value < 0 {
			return nil, errors.New("ffprobe video packet PTS is invalid")
		}
		duration, err := parseDecimalUS(packet.Duration)
		if err != nil || duration <= 0 {
			return nil, errors.New("ffprobe video packet duration is invalid")
		}
		pts[index], durations[index] = value, duration
		if index > 0 && (pts[index] <= pts[index-1] ||
			pts[index-1] > math.MaxInt64-durations[index-1] ||
			pts[index-1]+durations[index-1] != pts[index]) {
			return nil, errors.New("ffprobe video packets are not strictly increasing and continuous")
		}
	}
	if pts[0] != expectedStart || pts[len(pts)-1] > math.MaxInt64-durations[len(durations)-1] ||
		pts[len(pts)-1]+durations[len(durations)-1] != expectedEnd {
		return nil, fmt.Errorf(
			"ffprobe video packets do not exactly cover the declared stream extent (got=%d..%d want=%d..%d)",
			pts[0], pts[len(pts)-1]+durations[len(durations)-1], expectedStart, expectedEnd)
	}
	return pts, nil
}

func parseDecimalUS(value string) (int64, error) {
	if value == "" || strings.HasPrefix(value, "+") {
		return 0, errors.New("empty or noncanonical decimal time")
	}
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = strings.TrimPrefix(value, "-")
	}
	whole, fraction, hasFraction := strings.Cut(value, ".")
	if whole == "" || len(whole) > 1 && whole[0] == '0' || !hasFraction || len(fraction) != 6 {
		return 0, errors.New("time is not canonical microsecond precision")
	}
	seconds, err := strconv.ParseUint(whole, 10, 63)
	if err != nil {
		return 0, errors.New("time seconds are invalid")
	}
	microseconds, err := strconv.ParseUint(fraction, 10, 20)
	if err != nil || seconds > uint64(math.MaxInt64-int64(microseconds))/1_000_000 {
		return 0, errors.New("time microseconds are invalid")
	}
	result := int64(seconds)*1_000_000 + int64(microseconds)
	if negative {
		if result == 0 {
			return 0, errors.New("negative zero time is noncanonical")
		}
		result = -result
	}
	return result, nil
}

func digestPTS(values []int64) string {
	var buffer bytes.Buffer
	for _, value := range values {
		buffer.WriteString(strconv.FormatInt(value, 10))
		buffer.WriteByte('\n')
	}
	return digest(buffer.Bytes())
}

func decodeExactJSON(payload []byte, target any) error {
	if strictjson.Validate(payload) != nil {
		return errors.New("JSON is not strict")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func decodeJSONSubset(payload []byte, target any) error {
	if strictjson.Validate(payload) != nil {
		return errors.New("JSON is not strict")
	}
	return json.Unmarshal(payload, target)
}

func containsCSVToken(value, expected string) bool {
	for _, token := range strings.Split(value, ",") {
		if token == expected {
			return true
		}
	}
	return false
}

type attestationReport struct {
	Schema                   string                   `json:"schema"`
	Capability               string                   `json:"capability"`
	SourceGeometry           geometryEvidence         `json:"source_geometry"`
	EncodedGeometry          geometryEvidence         `json:"encoded_geometry"`
	GeometryPolicy           string                   `json:"geometry_policy"`
	PaddingRightPixels       int                      `json:"padding_right_pixels"`
	PaddingBottomPixels      int                      `json:"padding_bottom_pixels"`
	ExpectedOutputSHA256     string                   `json:"expected_output_sha256"`
	ExpectedOutputBytes      int64                    `json:"expected_output_bytes"`
	ExpectedFramePTSUSSHA256 string                   `json:"expected_frame_pts_us_sha256"`
	StreamProbeSHA256        string                   `json:"stream_probe_sha256"`
	VideoFrameProbeSHA256    string                   `json:"video_frame_probe_sha256"`
	VideoPacketProbeSHA256   string                   `json:"video_packet_probe_sha256"`
	AudioFrameProbeSHA256    string                   `json:"audio_frame_probe_sha256"`
	AudioFrameCount          int                      `json:"audio_frame_count"`
	AudioDecodedSamples      int64                    `json:"audio_decoded_samples"`
	AudioPresentedSamples    int64                    `json:"audio_presented_samples"`
	AudioTailPaddingSamples  int64                    `json:"audio_tail_padding_samples"`
	FullDecodeCompleted      bool                     `json:"full_decode_completed"`
	Spec                     reviewmedia.PlayableSpec `json:"spec"`
}

type geometryEvidence struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
