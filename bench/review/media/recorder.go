// Package media retains the exact audio and video delivered during one
// benchmark attempt. Raw evidence is content-addressed before independently
// pinned encoder and full-decode attestor plug-ins derive review media.
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
)

const (
	ManifestFormat                    = "openrealtime.attempt-media"
	ManifestFormatVersion             = 3
	FullDecodeAttestationCapability   = "openrealtime.full-decode-av.v1"
	encoderImplementationPath         = "encoder.implementation"
	encoderConfigurationPath          = "encoder.configuration.json"
	attestorImplementationPath        = "attestor.implementation"
	attestorConfigurationPath         = "attestor.configuration.json"
	manifestPath                      = "manifest.json"
	pendingManifestPath               = ".manifest.pending"
	invalidManifestPath               = ".manifest.invalid"
	maximumFrames                     = 100_000
	maximumVideoSources               = 32
	maximumFrameBytes                 = 64 << 20
	maximumTotalBytes                 = int64(16 << 30)
	maximumPlayableBytes              = int64(128 << 20)
	maximumAttestationBytes           = 4 << 20
	maximumManifestBytes              = 64 << 20
	maximumEncoderImplementationBytes = 4 << 20
	maximumEncoderConfigurationBytes  = 1 << 20
	maximumSensitiveValues            = 256
	maximumSensitiveValueBytes        = 4 << 10
	maximumSensitiveBytes             = 256 << 10
)

// YUV420PPadRightBottomBlackToEvenV1 is the exact source-to-encoded
// geometry policy used by the retained-review encoder. Source pixels keep
// their coordinates; at most one opaque-black column and/or row is added at
// the right and bottom edges so yuv420p never crops an odd dimension.
const YUV420PPadRightBottomBlackToEvenV1 = "yuv420p-pad-right-bottom-black-to-even.v1"

// Config declares the exact evidence required for an attempt. Directory is a
// final create-only directory. SensitiveValues remain in memory and guard all
// plug-in provenance, reports, manifests, and errors from accidental retention.
type Config struct {
	Directory            string
	RequireAudio         bool
	ExpectedVideoSources []string
	Encoder              Encoder
	Attestor             Attestor
	SensitiveValues      []string
}

type EncoderDescriptor struct {
	Name                string                 `json:"name"`
	Version             string                 `json:"version"`
	Implementation      review.ContentIdentity `json:"implementation"`
	ConfigurationSHA256 string                 `json:"configuration_sha256"`
}

func (descriptor EncoderDescriptor) Validate() error {
	return validatePluginDescriptor("media encoder", descriptor.Name, descriptor.Version, "",
		descriptor.Implementation, descriptor.ConfigurationSHA256)
}

type AttestorDescriptor struct {
	Name                string                 `json:"name"`
	Version             string                 `json:"version"`
	Capability          string                 `json:"capability"`
	Implementation      review.ContentIdentity `json:"implementation"`
	ConfigurationSHA256 string                 `json:"configuration_sha256"`
}

func (descriptor AttestorDescriptor) Validate() error {
	if descriptor.Capability != FullDecodeAttestationCapability {
		return errors.New("media attestor does not declare the required full-decode capability")
	}
	return validatePluginDescriptor("media attestor", descriptor.Name, descriptor.Version, descriptor.Capability,
		descriptor.Implementation, descriptor.ConfigurationSHA256)
}

func validatePluginDescriptor(label, name, version, capability string,
	implementation review.ContentIdentity, configurationSHA string) error {
	for _, field := range []struct{ name, value string }{{"name", name}, {"version", version}} {
		if err := validateToken(label+" "+field.name, field.value, 256); err != nil {
			return err
		}
	}
	if capability != "" {
		if err := validateToken(label+" capability", capability, 256); err != nil {
			return err
		}
	}
	if err := validateToken(label+" implementation version", implementation.Version, 256); err != nil {
		return err
	}
	if !validDigest(implementation.SHA256) {
		return fmt.Errorf("%s implementation is not canonical SHA-256", label)
	}
	if !validDigest(configurationSHA) {
		return fmt.Errorf("%s configuration is not canonical SHA-256", label)
	}
	return nil
}

// EncodeRequest exposes only an isolated recorder-owned workspace. Input files
// are read-only and OutputPath is beneath the existing output/ subdirectory.
type EncodeRequest struct {
	Source         string
	Width          int
	Height         int
	FirstFrameUS   int64
	AttemptEndUS   int64
	AudioPath      string
	ConcatPath     string
	OutputPath     string
	ExpectedFrames int
	ExpectedAudio  string
	ExpectedConcat string
	WorkspaceRoot  string
}

type Encoder interface {
	Descriptor() EncoderDescriptor
	Implementation() []byte
	Configuration() []byte
	Claim(context.Context) error
	Encode(context.Context, EncodeRequest) error
	Close() error
}

type AttestationRequest struct {
	Source                   string
	Width                    int
	Height                   int
	FirstFrameUS             int64
	AttemptEndUS             int64
	ExpectedFrameCount       int
	ExpectedFramePTSUSSHA256 string
	OutputPath               string
	WorkspaceRoot            string
	ExpectedOutputSHA256     string
	ExpectedOutputBytes      int64
}

type Attestation struct {
	Spec   PlayableSpec
	Report []byte
}

type Attestor interface {
	Descriptor() AttestorDescriptor
	Implementation() []byte
	Configuration() []byte
	Claim(context.Context) error
	Attest(context.Context, AttestationRequest) (Attestation, error)
	Close() error
}

type AudioSpec struct {
	Container     string   `json:"container"`
	Encoding      string   `json:"encoding"`
	SampleRateHz  uint32   `json:"sample_rate_hz"`
	Channels      uint16   `json:"channels"`
	ChannelLayout []string `json:"channel_layout"`
	Frames        uint64   `json:"frames"`
	DurationMS    float64  `json:"duration_ms"`
}

type AudioArtifact struct {
	review.Media
	AudioSpec AudioSpec `json:"audio"`
}

type Frame struct {
	Sequence      int    `json:"sequence"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	SizeBytes     int64  `json:"size_bytes"`
	MediaType     string `json:"media_type"`
	WireTimestamp int64  `json:"wire_timestamp_ms"`
	EpisodeAtUS   int64  `json:"episode_at_us"`
}

type AttestationArtifact struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

type VideoSource struct {
	Source            string               `json:"source"`
	Width             int                  `json:"width"`
	Height            int                  `json:"height"`
	Frames            []Frame              `json:"frames"`
	FramePTSUSSHA256  string               `json:"frame_pts_us_sha256"`
	TimelinePath      string               `json:"timeline_path"`
	TimelineSHA256    string               `json:"timeline_sha256"`
	Playable          *review.Media        `json:"playable,omitempty"`
	PlayableSpec      *PlayableSpec        `json:"playable_spec,omitempty"`
	AttestationReport *AttestationArtifact `json:"attestation_report,omitempty"`
}

// PlayableSpec binds the full decode to exact output bytes and canonical PTS.
// Width and Height are the exact captured source geometry; EncodedWidth and
// EncodedHeight are the independently decoded playable geometry.
type PlayableSpec struct {
	OutputSHA256          string `json:"output_sha256"`
	Container             string `json:"container"`
	VideoCodec            string `json:"video_codec"`
	PixelFormat           string `json:"pixel_format"`
	Width                 int    `json:"width"`
	Height                int    `json:"height"`
	EncodedWidth          int    `json:"encoded_width"`
	EncodedHeight         int    `json:"encoded_height"`
	GeometryPolicy        string `json:"geometry_policy"`
	AudioCodec            string `json:"audio_codec"`
	AudioSampleRateHz     uint32 `json:"audio_sample_rate_hz"`
	AudioChannels         uint16 `json:"audio_channels"`
	DurationUS            int64  `json:"duration_us"`
	AudioStartUS          int64  `json:"audio_start_us"`
	AudioEndUS            int64  `json:"audio_end_us"`
	VideoStartUS          int64  `json:"video_start_us"`
	VideoEndUS            int64  `json:"video_end_us"`
	VideoFrameCount       int    `json:"video_frame_count"`
	VideoFramePTSUSSHA256 string `json:"video_frame_pts_us_sha256"`
}

func (spec PlayableSpec) validate(request AttestationRequest) error {
	wantEncodedWidth, wantEncodedHeight := paddedYUV420PGeometry(request.Width, request.Height)
	if spec.OutputSHA256 != request.ExpectedOutputSHA256 || spec.Container != "mp4" ||
		spec.VideoCodec != "h264" || spec.PixelFormat != "yuv420p" ||
		spec.Width != request.Width || spec.Height != request.Height ||
		spec.EncodedWidth != wantEncodedWidth || spec.EncodedHeight != wantEncodedHeight ||
		spec.GeometryPolicy != YUV420PPadRightBottomBlackToEvenV1 ||
		spec.AudioCodec != "aac" || spec.AudioSampleRateHz != reviewSampleRateHz ||
		spec.AudioChannels != reviewChannels || spec.DurationUS != request.AttemptEndUS ||
		spec.AudioStartUS != 0 || spec.AudioEndUS != request.AttemptEndUS ||
		spec.VideoStartUS != request.FirstFrameUS || spec.VideoEndUS != request.AttemptEndUS ||
		spec.VideoFrameCount != request.ExpectedFrameCount ||
		spec.VideoFramePTSUSSHA256 != request.ExpectedFramePTSUSSHA256 {
		return errors.New("full-decode attestation does not match the exact synchronized output contract")
	}
	return nil
}

func paddedYUV420PGeometry(width, height int) (int, int) {
	return width + width%2, height + height%2
}

type EncoderEvidence struct {
	Descriptor         EncoderDescriptor `json:"descriptor"`
	ImplementationPath string            `json:"implementation_path"`
	ConfigurationPath  string            `json:"configuration_path"`
}

type AttestorEvidence struct {
	Descriptor         AttestorDescriptor `json:"descriptor"`
	ImplementationPath string             `json:"implementation_path"`
	ConfigurationPath  string             `json:"configuration_path"`
}

type Manifest struct {
	Format        string            `json:"format"`
	FormatVersion int               `json:"format_version"`
	Complete      bool              `json:"complete"`
	AttemptEndUS  int64             `json:"attempt_end_us"`
	Audio         *AudioArtifact    `json:"audio,omitempty"`
	Video         []VideoSource     `json:"video,omitempty"`
	Encoder       *EncoderEvidence  `json:"encoder,omitempty"`
	Attestor      *AttestorEvidence `json:"attestor,omitempty"`
}

func (manifest Manifest) ReviewMedia() []review.Media {
	var result []review.Media
	if manifest.Audio != nil {
		result = append(result, manifest.Audio.Media)
	}
	for _, source := range manifest.Video {
		if source.Playable != nil {
			result = append(result, *source.Playable)
		}
	}
	return result
}

type CompletionReceipt struct {
	Manifest       Manifest `json:"manifest"`
	ManifestSHA256 string   `json:"manifest_sha256"`
}

type sourceState struct {
	name      string
	width     int
	height    int
	lastAtUS  int64
	lastWire  int64
	frames    []Frame
	totalSize int64
}

type encoderSnapshot struct {
	descriptor     EncoderDescriptor
	implementation []byte
	configuration  []byte
}

type attestorSnapshot struct {
	descriptor     AttestorDescriptor
	implementation []byte
	configuration  []byte
}

type Recorder struct {
	mu                sync.Mutex
	directory         string
	root              *os.Root
	directoryIdentity os.FileInfo
	requireAudio      bool
	expected          []string
	expectedSet       map[string]struct{}
	encoder           Encoder
	attestor          Attestor
	encoderPinned     encoderSnapshot
	attestorPinned    attestorSnapshot
	encoderClaimed    bool
	attestorClaimed   bool
	encoderClosed     bool
	attestorClosed    bool
	sensitive         sensitiveMatcher
	audio             *AudioArtifact
	sources           map[string]*sourceState
	totalBytes        int64
	totalFrames       int
	closed            bool
	finalReceipt      CompletionReceipt
	closeErr          error
	operations        recorderOperations
}

// New validates and claims both plug-ins before exclusively creating the final
// directory. Post-create failures remove the still-owned directory for retry.
func New(ctx context.Context, config Config) (*Recorder, error) {
	return newRecorder(ctx, config, defaultRecorderOperations())
}

func newRecorder(ctx context.Context, config Config, operations recorderOperations) (*Recorder, error) {
	operations = operations.withDefaults()
	if ctx == nil {
		return nil, errors.New("create attempt media recorder: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sensitive, err := newSensitiveMatcher(config.SensitiveValues)
	if err != nil {
		return nil, err
	}
	directory, err := validateNewDirectory(config.Directory)
	if err != nil {
		return nil, errors.New("attempt media directory is invalid")
	}
	expected, err := canonicalSources(config.ExpectedVideoSources)
	if err != nil {
		return nil, err
	}
	if !config.RequireAudio && len(expected) == 0 {
		return nil, errors.New("attempt media must require audio or at least one video source")
	}
	if len(expected) > 0 && !config.RequireAudio {
		return nil, errors.New("playable video capture requires time-aligned audio")
	}
	if err := guardStrings(sensitive, append([]string{directory}, expected...)...); err != nil {
		return nil, err
	}

	var encoderPinned encoderSnapshot
	var attestorPinned attestorSnapshot
	if len(expected) > 0 {
		if nilInterface(config.Encoder) || nilInterface(config.Attestor) {
			return nil, errors.New("video capture requires encoder and independent full-decode attestor plug-ins")
		}
		encoderPinned, err = snapshotEncoder(ctx, config.Encoder, sensitive)
		if err != nil {
			return nil, err
		}
		attestorPinned, err = snapshotAttestor(ctx, config.Attestor, sensitive)
		if err != nil {
			return nil, err
		}
	} else if !nilInterface(config.Encoder) || !nilInterface(config.Attestor) {
		return nil, errors.New("media plug-ins were supplied without expected video sources")
	}

	encoderClaimed, attestorClaimed := false, false
	closeClaimed := func() error {
		failed := false
		if attestorClaimed {
			attestorClaimed = false
			if config.Attestor.Close() != nil {
				failed = true
			}
		}
		if encoderClaimed {
			encoderClaimed = false
			if config.Encoder.Close() != nil {
				failed = true
			}
		}
		if failed {
			return errors.New("close claimed media plug-ins")
		}
		return nil
	}
	if len(expected) > 0 {
		claimErr := config.Encoder.Claim(ctx)
		if claimErr == nil {
			// A successful Claim transfers ownership even if cancellation becomes
			// observable immediately after the call returns.
			encoderClaimed = true
		}
		if cause := ctx.Err(); cause != nil {
			return nil, joinCanonical(cause, closeClaimed())
		}
		if claimErr != nil {
			return nil, errors.New("claim media encoder ownership")
		}
		claimErr = config.Attestor.Claim(ctx)
		if claimErr == nil {
			attestorClaimed = true
		}
		if cause := ctx.Err(); cause != nil {
			return nil, joinCanonical(cause, closeClaimed())
		}
		if claimErr != nil {
			return nil, joinCanonical(errors.New("claim media attestor ownership"), closeClaimed())
		}
		claimedEncoder, snapshotErr := snapshotEncoder(ctx, config.Encoder, sensitive)
		if cause := ctx.Err(); cause != nil {
			return nil, joinCanonical(cause, closeClaimed())
		}
		if snapshotErr != nil || !equalEncoderSnapshot(claimedEncoder, encoderPinned) {
			return nil, joinCanonical(errors.New("media encoder identity changed while claiming ownership"), closeClaimed())
		}
		claimedAttestor, snapshotErr := snapshotAttestor(ctx, config.Attestor, sensitive)
		if cause := ctx.Err(); cause != nil {
			return nil, joinCanonical(cause, closeClaimed())
		}
		if snapshotErr != nil || !equalAttestorSnapshot(claimedAttestor, attestorPinned) {
			return nil, joinCanonical(errors.New("media attestor identity changed while claiming ownership"), closeClaimed())
		}
	}
	if cause := ctx.Err(); cause != nil {
		return nil, joinCanonical(cause, closeClaimed())
	}

	root, identity, err := createAttemptRoot(directory)
	if err != nil {
		return nil, joinCanonical(errors.New("create attempt media directory exclusively"), closeClaimed())
	}
	recorder := &Recorder{
		directory: directory, root: root, directoryIdentity: identity,
		requireAudio: config.RequireAudio, expected: expected,
		expectedSet: make(map[string]struct{}, len(expected)),
		sources:     make(map[string]*sourceState, len(expected)),
		encoder:     config.Encoder, attestor: config.Attestor,
		encoderPinned: encoderPinned, attestorPinned: attestorPinned,
		encoderClaimed: encoderClaimed, attestorClaimed: attestorClaimed,
		sensitive:  sensitive,
		operations: operations,
	}
	fail := func(primary error) (*Recorder, error) {
		cleanupErr := recorder.closePlugins()
		if operations.closeRoot(root) != nil {
			cleanupErr = joinCanonical(cleanupErr, errors.New("close attempt media directory during constructor cleanup"))
		}
		recorder.root = nil
		if operations.removeOwnedAttempt(directory, identity) != nil {
			cleanupErr = joinCanonical(cleanupErr, errors.New("remove owned attempt media directory during constructor cleanup"))
		}
		return nil, joinCanonical(primary, cleanupErr)
	}
	if len(expected) > 0 {
		for _, artifact := range []struct {
			path string
			data []byte
		}{
			{encoderImplementationPath, encoderPinned.implementation},
			{encoderConfigurationPath, encoderPinned.configuration},
			{attestorImplementationPath, attestorPinned.implementation},
			{attestorConfigurationPath, attestorPinned.configuration},
		} {
			if err := writeExclusive(root, artifact.path, artifact.data, 0o400); err != nil {
				return fail(errors.New("retain media plug-in provenance"))
			}
		}
	}
	if len(expected) > 0 {
		if err := root.Mkdir("frames", 0o700); err != nil || syncDirectory(root, ".") != nil {
			return fail(errors.New("create attempt frame directory"))
		}
	}
	for _, source := range expected {
		recorder.expectedSet[source] = struct{}{}
		path := filepath.ToSlash(filepath.Join("frames", source))
		if err := root.Mkdir(path, 0o700); err != nil || syncDirectory(root, "frames") != nil {
			return fail(errors.New("create attempt frame source directory"))
		}
		recorder.sources[source] = &sourceState{name: source, lastAtUS: -1, lastWire: math.MinInt64}
	}
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	return recorder, nil
}

func (recorder *Recorder) Directory() string {
	if recorder == nil {
		return ""
	}
	return recorder.directory
}

func (recorder *Recorder) CaptureAudio(capture bench.SessionAudioCapture) error {
	if recorder == nil {
		return errors.New("attempt media recorder is nil")
	}
	wav, spec, err := EncodeStereoWAV(capture)
	if err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.closed {
		return errors.New("attempt media recorder is closed")
	}
	if err := recorder.verifyDirectory(); err != nil {
		return err
	}
	if recorder.audio != nil {
		return errors.New("attempt audio was already captured")
	}
	if err := recorder.addBytes(int64(len(wav))); err != nil {
		return err
	}
	const path = "audio.stereo.wav"
	if err := writeExclusive(recorder.root, path, wav, 0o400); err != nil {
		recorder.totalBytes -= int64(len(wav))
		return errors.New("retain attempt audio")
	}
	recorder.audio = &AudioArtifact{
		Media: review.Media{Kind: "audio", Role: "time_aligned_room_and_agent", Path: path,
			SHA256: digest(wav), MediaType: "audio/wav"},
		AudioSpec: spec,
	}
	return nil
}

// CaptureVideo persists owned bytes immediately. Episode times are canonical
// microseconds and each source must advance by at least one millisecond.
func (recorder *Recorder) CaptureVideo(capture bench.SessionVideoCapture) error {
	if recorder == nil {
		return errors.New("attempt media recorder is nil")
	}
	episodeUS, err := validateFrame(capture)
	if err != nil {
		return err
	}
	payload := slices.Clone(capture.Data)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.closed {
		return errors.New("attempt media recorder is closed")
	}
	if err := recorder.verifyDirectory(); err != nil {
		return err
	}
	state, expected := recorder.sources[capture.Source]
	if !expected {
		return errors.New("video source was not declared for this attempt")
	}
	if len(state.frames) >= maximumFrames || recorder.totalFrames >= maximumFrames {
		return fmt.Errorf("video source exceeds %d frames", maximumFrames)
	}
	if state.width != 0 && (state.width != capture.Width || state.height != capture.Height) {
		return errors.New("video source dimensions changed during the attempt")
	}
	if state.lastAtUS >= 0 && episodeUS-state.lastAtUS < 1000 {
		return errors.New("video frame episode times must increase by at least 1000 microseconds")
	}
	if state.lastWire > capture.WireTimestamp {
		return errors.New("video source wire timestamps moved backwards")
	}
	if err := recorder.addBytes(int64(len(payload))); err != nil {
		return err
	}
	extension := ".jpg"
	if capture.MediaType == "image/png" {
		extension = ".png"
	}
	sequence := len(state.frames) + 1
	path := filepath.ToSlash(filepath.Join("frames", capture.Source,
		fmt.Sprintf("%06d%s", sequence, extension)))
	if err := writeExclusive(recorder.root, path, payload, 0o400); err != nil {
		recorder.totalBytes -= int64(len(payload))
		return errors.New("retain video frame")
	}
	state.width, state.height = capture.Width, capture.Height
	state.lastAtUS, state.lastWire = episodeUS, capture.WireTimestamp
	state.totalSize += int64(len(payload))
	recorder.totalFrames++
	state.frames = append(state.frames, Frame{
		Sequence: sequence, Path: path, SHA256: digest(payload), SizeBytes: int64(len(payload)),
		MediaType: capture.MediaType, WireTimestamp: capture.WireTimestamp, EpisodeAtUS: episodeUS,
	})
	return nil
}

// Finalize derives, independently attests, and imports review media. The caller
// supplies the exact attempt end; no implicit display tail is added.
func (recorder *Recorder) Finalize(ctx context.Context, attemptEndMS float64) (CompletionReceipt, error) {
	if recorder == nil {
		return CompletionReceipt{}, errors.New("attempt media recorder is nil")
	}
	if ctx == nil {
		return CompletionReceipt{}, errors.New("finalize attempt media: nil context")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.closed {
		return cloneReceipt(recorder.finalReceipt), recorder.closeErr
	}
	recorder.closed = true
	fail := func(err error) (CompletionReceipt, error) {
		if closeErr := recorder.closePlugins(); closeErr != nil && err == nil {
			err = closeErr
		} else if closeErr != nil {
			err = joinCanonical(err, closeErr)
		}
		markerPresent := false
		markerStateKnown := true
		markerReceipt := CompletionReceipt{}
		if recorder.root != nil {
			if cleanupErr := recorder.operations.removeUnexpectedManifest(recorder.root); cleanupErr != nil {
				err = joinCanonical(err, errors.New("remove attempt media completion marker during failure cleanup"))
			}
			var stateErr error
			markerPresent, stateErr = regularEntryExists(recorder.root, manifestPath)
			if stateErr != nil {
				markerStateKnown = false
				err = joinCanonical(err, errors.New("inspect attempt media completion marker during failure cleanup"))
			}
			if markerPresent || !markerStateKnown {
				markerReceipt = completionMarkerReceipt(recorder.root)
			}
			if closeErr := recorder.operations.closeRoot(recorder.root); closeErr != nil {
				err = joinCanonical(err, errors.New("close attempt media directory during failure cleanup"))
			}
			recorder.root = nil
		}
		var residual CompletionReceipt
		if markerPresent || !markerStateKnown {
			if removeErr := recorder.operations.removeOwnedAttempt(
				recorder.directory, recorder.directoryIdentity); removeErr != nil {
				err = joinCanonical(err, errors.New("residual attempt media completion marker remains"))
				residual = markerReceipt
			}
		}
		if err == nil {
			err = errors.New("finalize attempt media")
		}
		recorder.finalReceipt = cloneReceipt(residual)
		recorder.closeErr = err
		return cloneReceipt(residual), err
	}
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	if err := recorder.verifyDirectory(); err != nil {
		return fail(err)
	}
	if present, err := regularEntryExists(recorder.root, manifestPath); err != nil || present {
		return fail(errors.New("unexpected attempt media completion marker"))
	}
	attemptEndUS, err := canonicalTimeUS("attempt end", attemptEndMS)
	if err != nil || attemptEndUS <= 0 {
		return fail(errors.New("attempt end must be a positive finite canonical microsecond timestamp"))
	}
	if recorder.requireAudio && recorder.audio == nil {
		return fail(errors.New("attempt media is missing required audio"))
	}
	if recorder.audio != nil {
		if !audioEndpointAligned(attemptEndUS, recorder.audio.AudioSpec.SampleRateHz) {
			return fail(errors.New("attempt end is not exactly representable on the captured audio sample clock"))
		}
		if !audioCoveredByEnd(recorder.audio.AudioSpec, attemptEndUS) {
			return fail(errors.New("attempt end precedes captured audio"))
		}
		if err := recorder.verifyAudio(); err != nil {
			return fail(err)
		}
	}
	if len(recorder.expected) > 0 {
		if err := recorder.verifyPlugins(ctx); err != nil {
			return fail(err)
		}
	}

	manifest := Manifest{Format: ManifestFormat, FormatVersion: ManifestFormatVersion,
		AttemptEndUS: attemptEndUS, Audio: cloneAudio(recorder.audio)}
	if len(recorder.expected) > 0 {
		manifest.Encoder = &EncoderEvidence{Descriptor: recorder.encoderPinned.descriptor,
			ImplementationPath: encoderImplementationPath, ConfigurationPath: encoderConfigurationPath}
		manifest.Attestor = &AttestorEvidence{Descriptor: recorder.attestorPinned.descriptor,
			ImplementationPath: attestorImplementationPath, ConfigurationPath: attestorConfigurationPath}
	}

	for _, name := range recorder.expected {
		if cause := ctx.Err(); cause != nil {
			return fail(cause)
		}
		state := recorder.sources[name]
		if len(state.frames) == 0 {
			return fail(errors.New("attempt media is missing a required video source"))
		}
		if state.frames[len(state.frames)-1].EpisodeAtUS >= attemptEndUS {
			return fail(errors.New("attempt end must be after every captured video frame"))
		}
		timeline, err := buildConcatTimeline(state.frames, attemptEndUS)
		if err != nil {
			return fail(err)
		}
		timelinePath := name + ".ffconcat"
		if err := writeExclusive(recorder.root, timelinePath, timeline, 0o400); err != nil {
			return fail(errors.New("retain frame timeline"))
		}
		timelineSHA := digest(timeline)
		workspace, err := recorder.stageSource(name, state, timelinePath, timeline, timelineSHA)
		if err != nil {
			return fail(err)
		}
		cleanupWorkspace := func(primary error) error {
			if cleanupErr := workspace.cleanup(); cleanupErr != nil {
				return joinCanonical(primary, errors.New("clean isolated media workspace after failure"))
			}
			return primary
		}
		request := EncodeRequest{
			Source: name, Width: state.width, Height: state.height,
			FirstFrameUS: state.frames[0].EpisodeAtUS, AttemptEndUS: attemptEndUS,
			AudioPath:      filepath.Join(workspace.path, filepath.FromSlash(recorder.audio.Path)),
			ConcatPath:     filepath.Join(workspace.path, filepath.FromSlash(timelinePath)),
			OutputPath:     filepath.Join(workspace.path, "output", name+".review.mp4"),
			ExpectedFrames: len(state.frames), ExpectedAudio: recorder.audio.SHA256,
			ExpectedConcat: timelineSHA, WorkspaceRoot: workspace.path,
		}
		encodeErr := recorder.encoder.Encode(ctx, request)
		if cause := ctx.Err(); cause != nil {
			return fail(cleanupWorkspace(cause))
		}
		if encodeErr != nil {
			return fail(cleanupWorkspace(errors.New("media encoder failed")))
		}
		if err := recorder.verifyPlugins(ctx); err != nil {
			return fail(cleanupWorkspace(err))
		}
		if err := recorder.verifyWorkspaceInputs(workspace, state, timelinePath, timelineSHA); err != nil {
			return fail(cleanupWorkspace(err))
		}
		outputRelative := filepath.ToSlash(filepath.Join("output", name+".review.mp4"))
		outputPayload, outputSHA, outputType, outputAnchor, err := syncAndReadRegular(
			workspace.root, outputRelative, maximumPlayableBytes)
		if err != nil || outputType != "video/mp4" {
			return fail(cleanupWorkspace(errors.New("media encoder output is not a synced bounded MP4")))
		}
		closeOutput := func() error {
			if outputAnchor == nil {
				return nil
			}
			err := outputAnchor.Close()
			outputAnchor = nil
			return err
		}
		cleanupOutput := func(primary error) error {
			if err := closeOutput(); err != nil {
				primary = joinCanonical(primary, errors.New("close attested media output during failure cleanup"))
			}
			return cleanupWorkspace(primary)
		}
		if recorder.sensitive.contains(outputPayload) {
			return fail(cleanupOutput(errors.New("media encoder output contains a sensitive value")))
		}
		ptsSHA := framePTSDigest(state.frames)
		attestationRequest := AttestationRequest{
			Source: name, Width: state.width, Height: state.height,
			FirstFrameUS: state.frames[0].EpisodeAtUS, AttemptEndUS: attemptEndUS,
			ExpectedFrameCount: len(state.frames), ExpectedFramePTSUSSHA256: ptsSHA,
			OutputPath: request.OutputPath, WorkspaceRoot: workspace.path,
			ExpectedOutputSHA256: outputSHA, ExpectedOutputBytes: int64(len(outputPayload)),
		}
		attestation, attestErr := recorder.attestor.Attest(ctx, attestationRequest)
		if cause := ctx.Err(); cause != nil {
			return fail(cleanupOutput(cause))
		}
		if attestErr != nil {
			return fail(cleanupOutput(errors.New("media full-decode attestor failed")))
		}
		if err := validateAttestation(attestation, attestationRequest, recorder.sensitive); err != nil {
			return fail(cleanupOutput(err))
		}
		if err := recorder.verifyPlugins(ctx); err != nil {
			return fail(cleanupOutput(err))
		}
		if err := recorder.verifyWorkspaceInputs(workspace, state, timelinePath, timelineSHA); err != nil {
			return fail(cleanupOutput(err))
		}
		afterPayload, afterSHA, afterType, err := readRegular(
			workspace.root, outputRelative, maximumPlayableBytes)
		anchoredIdentity, anchorErr := outputAnchor.Stat()
		afterIdentity, identityErr := workspace.root.Lstat(outputRelative)
		if err != nil || anchorErr != nil || identityErr != nil ||
			!os.SameFile(anchoredIdentity, afterIdentity) ||
			afterSHA != outputSHA || afterType != outputType ||
			!bytes.Equal(afterPayload, outputPayload) {
			return fail(cleanupOutput(errors.New("attested media output changed after its core digest")))
		}
		if err := closeOutput(); err != nil {
			return fail(cleanupWorkspace(errors.New("close attested media output descriptor")))
		}
		playablePath := name + ".review.mp4"
		if err := writeExclusive(recorder.root, playablePath, outputPayload, 0o400); err != nil {
			return fail(cleanupWorkspace(errors.New("import attested media output")))
		}
		reportPath := name + ".attestation.json"
		if err := writeExclusive(recorder.root, reportPath, attestation.Report, 0o400); err != nil {
			return fail(cleanupWorkspace(errors.New("retain media attestation report")))
		}
		if err := workspace.cleanup(); err != nil {
			return fail(errors.New("clean isolated media workspace"))
		}
		playable := review.Media{Kind: "video", Role: "time_aligned_" + name + "_and_room_agent",
			Path: playablePath, SHA256: outputSHA, MediaType: "video/mp4"}
		spec := attestation.Spec
		manifest.Video = append(manifest.Video, VideoSource{
			Source: name, Width: state.width, Height: state.height,
			Frames: slices.Clone(state.frames), FramePTSUSSHA256: ptsSHA,
			TimelinePath: timelinePath, TimelineSHA256: timelineSHA,
			Playable: &playable, PlayableSpec: &spec,
			AttestationReport: &AttestationArtifact{Path: reportPath, SHA256: digest(attestation.Report),
				SizeBytes: int64(len(attestation.Report)), MediaType: "application/json"},
		})
	}
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	if len(recorder.expected) > 0 {
		if err := recorder.verifyPlugins(ctx); err != nil {
			return fail(err)
		}
	}
	closeErr := recorder.closePlugins()
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	if err := recorder.verifyFinalRawInputs(); err != nil {
		return fail(err)
	}
	if err := bindReviewValidation(ctx, recorder.directory, &manifest, recorder.sensitive); err != nil {
		if cause := ctx.Err(); cause != nil {
			return fail(cause)
		}
		return fail(err)
	}
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	manifest.Complete = true
	if err := validateManifestAndArtifacts(recorder.root, recorder.directory, manifest, false, readRegular); err != nil {
		return fail(err)
	}
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	if err := hardenBundleBeforeCommit(recorder.root); err != nil {
		return fail(errors.New("make attempt evidence read-only"))
	}
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	payload, err := marshalManifest(manifest)
	if err != nil || recorder.sensitive.contains(payload) {
		return fail(errors.New("encode secret-free canonical attempt media manifest"))
	}
	manifestSHA := digest(payload)
	if cause := ctx.Err(); cause != nil {
		return fail(cause)
	}
	if err := publishManifest(recorder.root, payload, manifestSHA); err != nil {
		return fail(errors.New("publish attempt media completion marker"))
	}
	publishedReceipt := CompletionReceipt{Manifest: cloneManifest(manifest), ManifestSHA256: manifestSHA}
	verified, verifyErr := recorder.operations.verifyBundle(recorder.directory, manifestSHA)
	if verifyErr == nil {
		verifiedPayload, marshalErr := marshalManifest(verified)
		if marshalErr != nil || !bytes.Equal(verifiedPayload, payload) {
			verifyErr = errors.New("published attempt media verifier returned a mismatched manifest")
		}
	}
	if verifyErr != nil {
		resultErr := errors.New("verify published attempt media bundle")
		if invalidateErr := recorder.operations.invalidatePublishedManifestRoot(recorder.root); invalidateErr != nil {
			resultErr = joinCanonical(resultErr,
				errors.New("invalidate published attempt media completion marker"))
		}
		markerPresent, markerErr := regularEntryExists(recorder.root, manifestPath)
		if markerErr != nil {
			markerPresent = true
			resultErr = joinCanonical(resultErr,
				errors.New("inspect published attempt media completion marker after invalidation"))
		}
		if closeErr := recorder.operations.closeRoot(recorder.root); closeErr != nil {
			resultErr = joinCanonical(resultErr,
				errors.New("close invalid published attempt media directory"))
		}
		recorder.root = nil
		if markerPresent {
			if removeErr := recorder.operations.removeOwnedAttempt(
				recorder.directory, recorder.directoryIdentity); removeErr != nil {
				resultErr = joinCanonical(resultErr,
					errors.New("residual published attempt media completion marker remains"))
				recorder.finalReceipt = cloneReceipt(publishedReceipt)
				recorder.closeErr = resultErr
				return cloneReceipt(publishedReceipt), resultErr
			}
		}
		recorder.closeErr = resultErr
		return CompletionReceipt{}, resultErr
	}
	receipt := CompletionReceipt{Manifest: verified, ManifestSHA256: manifestSHA}
	recorder.finalReceipt = cloneReceipt(receipt)
	if closeErr := recorder.operations.closeRoot(recorder.root); closeErr != nil {
		recorder.root = nil
		recorder.closeErr = errors.New("close verified attempt media directory")
		return cloneReceipt(receipt), recorder.closeErr
	}
	recorder.root = nil
	return cloneReceipt(receipt), nil
}

// Abort closes owned plug-ins without publishing a completion marker. Partial
// raw evidence is retained for diagnosis.
func (recorder *Recorder) Abort() error {
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.closed {
		return recorder.closeErr
	}
	recorder.closed = true
	err := recorder.closePlugins()
	markerPresent := false
	markerStateKnown := true
	if recorder.root != nil {
		if cleanupErr := recorder.operations.removeUnexpectedManifest(recorder.root); cleanupErr != nil {
			err = joinCanonical(err, errors.New("remove attempt media completion marker during abort"))
		}
		var stateErr error
		markerPresent, stateErr = regularEntryExists(recorder.root, manifestPath)
		if stateErr != nil {
			markerStateKnown = false
			err = joinCanonical(err, errors.New("inspect attempt media completion marker during abort"))
		}
		if closeErr := recorder.operations.closeRoot(recorder.root); closeErr != nil {
			err = joinCanonical(err, errors.New("close attempt media directory during abort"))
		}
		recorder.root = nil
	}
	if markerPresent || !markerStateKnown {
		if removeErr := recorder.operations.removeOwnedAttempt(
			recorder.directory, recorder.directoryIdentity); removeErr != nil {
			err = joinCanonical(err, errors.New("residual attempt media completion marker remains after abort"))
		}
	}
	recorder.closeErr = err
	return err
}
