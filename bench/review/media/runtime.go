package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

func joinCanonical(primary, cleanup error) error {
	if primary == nil {
		return cleanup
	}
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, cleanup)
}

type recorderOperations struct {
	closeRoot                       func(*os.Root) error
	removeUnexpectedManifest        func(*os.Root) error
	invalidatePublishedManifestRoot func(*os.Root) error
	removeOwnedAttempt              func(string, os.FileInfo) error
	verifyBundle                    func(string, string) (Manifest, error)
}

func defaultRecorderOperations() recorderOperations {
	return recorderOperations{
		closeRoot: func(root *os.Root) error {
			if root == nil {
				return nil
			}
			return root.Close()
		},
		removeUnexpectedManifest:        removeUnexpectedManifest,
		invalidatePublishedManifestRoot: invalidatePublishedManifestRoot,
		removeOwnedAttempt:              removeOwnedAttempt,
		verifyBundle:                    VerifyBundle,
	}
}

func (operations recorderOperations) withDefaults() recorderOperations {
	defaults := defaultRecorderOperations()
	if operations.closeRoot == nil {
		operations.closeRoot = defaults.closeRoot
	}
	if operations.removeUnexpectedManifest == nil {
		operations.removeUnexpectedManifest = defaults.removeUnexpectedManifest
	}
	if operations.invalidatePublishedManifestRoot == nil {
		operations.invalidatePublishedManifestRoot = defaults.invalidatePublishedManifestRoot
	}
	if operations.removeOwnedAttempt == nil {
		operations.removeOwnedAttempt = defaults.removeOwnedAttempt
	}
	if operations.verifyBundle == nil {
		operations.verifyBundle = defaults.verifyBundle
	}
	return operations
}

func validateAttestation(attestation Attestation, request AttestationRequest, sensitive sensitiveMatcher) error {
	if err := attestation.Spec.validate(request); err != nil {
		return err
	}
	if len(attestation.Report) == 0 || len(attestation.Report) > maximumAttestationBytes ||
		strictjson.Validate(attestation.Report) != nil {
		return errors.New("full-decode attestation report is empty, oversized, or invalid JSON")
	}
	trimmed := bytes.TrimSpace(attestation.Report)
	if len(trimmed) == 0 || trimmed[0] != '{' ||
		!bytes.Contains(attestation.Report, []byte(request.ExpectedOutputSHA256)) ||
		!bytes.Contains(attestation.Report, []byte(request.ExpectedFramePTSUSSHA256)) {
		return errors.New("full-decode attestation report is not an object bound to output and PTS digests")
	}
	if sensitive.contains(attestation.Report) {
		return errors.New("full-decode attestation report contains a sensitive value")
	}
	return nil
}

func bindReviewValidation(ctx context.Context, directory string, manifest *Manifest, sensitive sensitiveMatcher) error {
	if manifest == nil {
		return errors.New("attempt media manifest is nil")
	}
	prepared, err := prepareReviewMedia(ctx, directory, manifest.ReviewMedia(), sensitive)
	if err != nil {
		return errors.New("validate retained review media")
	}
	validationByPath := make(map[string]string, len(prepared))
	for _, item := range prepared {
		validationByPath[item.Path] = item.Validation
	}
	if manifest.Audio != nil {
		manifest.Audio.Validation = validationByPath[manifest.Audio.Path]
	}
	for index := range manifest.Video {
		if manifest.Video[index].Playable != nil {
			manifest.Video[index].Playable.Validation = validationByPath[manifest.Video[index].Playable.Path]
		}
	}
	return nil
}

func equalEncoderSnapshot(left, right encoderSnapshot) bool {
	return left.descriptor == right.descriptor &&
		bytes.Equal(left.implementation, right.implementation) &&
		bytes.Equal(left.configuration, right.configuration)
}

func equalAttestorSnapshot(left, right attestorSnapshot) bool {
	return left.descriptor == right.descriptor &&
		bytes.Equal(left.implementation, right.implementation) &&
		bytes.Equal(left.configuration, right.configuration)
}

func snapshotEncoder(ctx context.Context, encoder Encoder, sensitive sensitiveMatcher) (encoderSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return encoderSnapshot{}, err
	}
	descriptor := encoder.Descriptor()
	if err := ctx.Err(); err != nil {
		return encoderSnapshot{}, err
	}
	implementation := encoder.Implementation()
	if err := ctx.Err(); err != nil {
		return encoderSnapshot{}, err
	}
	configuration := encoder.Configuration()
	if err := ctx.Err(); err != nil {
		return encoderSnapshot{}, err
	}
	if err := descriptor.Validate(); err != nil {
		return encoderSnapshot{}, err
	}
	if err := validatePluginArtifacts(descriptor.Implementation.SHA256, descriptor.ConfigurationSHA256,
		implementation, configuration, sensitive); err != nil {
		return encoderSnapshot{}, err
	}
	encoded, _ := json.Marshal(descriptor)
	if sensitive.contains(encoded) {
		return encoderSnapshot{}, errors.New("media encoder descriptor contains a sensitive value")
	}
	return encoderSnapshot{descriptor, slices.Clone(implementation), slices.Clone(configuration)}, nil
}

func snapshotAttestor(ctx context.Context, attestor Attestor, sensitive sensitiveMatcher) (attestorSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return attestorSnapshot{}, err
	}
	descriptor := attestor.Descriptor()
	if err := ctx.Err(); err != nil {
		return attestorSnapshot{}, err
	}
	implementation := attestor.Implementation()
	if err := ctx.Err(); err != nil {
		return attestorSnapshot{}, err
	}
	configuration := attestor.Configuration()
	if err := ctx.Err(); err != nil {
		return attestorSnapshot{}, err
	}
	if err := descriptor.Validate(); err != nil {
		return attestorSnapshot{}, err
	}
	if err := validatePluginArtifacts(descriptor.Implementation.SHA256, descriptor.ConfigurationSHA256,
		implementation, configuration, sensitive); err != nil {
		return attestorSnapshot{}, err
	}
	encoded, _ := json.Marshal(descriptor)
	if sensitive.contains(encoded) {
		return attestorSnapshot{}, errors.New("media attestor descriptor contains a sensitive value")
	}
	return attestorSnapshot{descriptor, slices.Clone(implementation), slices.Clone(configuration)}, nil
}

func validatePluginArtifacts(implementationSHA, configurationSHA string,
	implementation, configuration []byte, sensitive sensitiveMatcher) error {
	if len(implementation) == 0 || len(implementation) > maximumEncoderImplementationBytes ||
		digest(implementation) != implementationSHA {
		return errors.New("media plug-in implementation artifact drifted")
	}
	if len(configuration) == 0 || len(configuration) > maximumEncoderConfigurationBytes ||
		strictjson.Validate(configuration) != nil || digest(configuration) != configurationSHA {
		return errors.New("media plug-in configuration artifact drifted")
	}
	if sensitive.contains(implementation) || sensitive.contains(configuration) {
		return errors.New("media plug-in provenance contains a sensitive value")
	}
	return nil
}

func (recorder *Recorder) verifyPlugins(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !recorder.encoderClaimed || recorder.encoderClosed || nilInterface(recorder.encoder) ||
		!recorder.attestorClaimed || recorder.attestorClosed || nilInterface(recorder.attestor) {
		return errors.New("media plug-ins are not exclusively owned and active")
	}
	if recorder.encoder.Descriptor() != recorder.encoderPinned.descriptor {
		return errors.New("media encoder identity changed during capture")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	implementation := recorder.encoder.Implementation()
	if err := ctx.Err(); err != nil {
		return err
	}
	configuration := recorder.encoder.Configuration()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !bytes.Equal(implementation, recorder.encoderPinned.implementation) ||
		!bytes.Equal(configuration, recorder.encoderPinned.configuration) {
		return errors.New("media encoder artifacts changed during capture")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if recorder.attestor.Descriptor() != recorder.attestorPinned.descriptor {
		return errors.New("media attestor identity changed during capture")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	implementation = recorder.attestor.Implementation()
	if err := ctx.Err(); err != nil {
		return err
	}
	configuration = recorder.attestor.Configuration()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !bytes.Equal(implementation, recorder.attestorPinned.implementation) ||
		!bytes.Equal(configuration, recorder.attestorPinned.configuration) {
		return errors.New("media attestor artifacts changed during capture")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, artifact := range []struct {
		path, expected string
		maximum        int64
	}{
		{encoderImplementationPath, recorder.encoderPinned.descriptor.Implementation.SHA256, maximumEncoderImplementationBytes},
		{encoderConfigurationPath, recorder.encoderPinned.descriptor.ConfigurationSHA256, maximumEncoderConfigurationBytes},
		{attestorImplementationPath, recorder.attestorPinned.descriptor.Implementation.SHA256, maximumEncoderImplementationBytes},
		{attestorConfigurationPath, recorder.attestorPinned.descriptor.ConfigurationSHA256, maximumEncoderConfigurationBytes},
	} {
		_, actual, _, err := readRegular(recorder.root, artifact.path, artifact.maximum)
		if err != nil || actual != artifact.expected {
			return errors.New("retained media plug-in provenance changed")
		}
	}
	return nil
}

func (recorder *Recorder) closePlugins() error {
	failed := false
	if recorder.attestorClaimed && !recorder.attestorClosed && !nilInterface(recorder.attestor) {
		recorder.attestorClosed = true
		if recorder.attestor.Close() != nil {
			failed = true
		}
	}
	if recorder.encoderClaimed && !recorder.encoderClosed && !nilInterface(recorder.encoder) {
		recorder.encoderClosed = true
		if recorder.encoder.Close() != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("close media plug-ins")
	}
	return nil
}

type stageWorkspace struct {
	path     string
	root     *os.Root
	identity os.FileInfo
}

func (workspace *stageWorkspace) cleanup() error {
	if workspace == nil || workspace.path == "" {
		return nil
	}
	closeRoot := func() error {
		if workspace.root == nil {
			return nil
		}
		err := workspace.root.Close()
		workspace.root = nil
		if err != nil {
			return errors.New("close isolated media workspace")
		}
		return nil
	}
	if workspace.identity == nil {
		resultErr := closeRoot()
		path := workspace.path
		workspace.path = ""
		if err := os.RemoveAll(path); err != nil {
			resultErr = joinCanonical(resultErr, errors.New("remove incomplete isolated media workspace"))
		}
		return resultErr
	}
	visible, visibleErr := os.Lstat(workspace.path)
	if visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		workspace.identity == nil || !os.SameFile(visible, workspace.identity) {
		closeErr := closeRoot()
		workspace.path = ""
		return joinCanonical(errors.New("isolated media workspace identity changed before cleanup"), closeErr)
	}
	resultErr := closeRoot()
	path := workspace.path
	workspace.path = ""
	if err := makeTreeOwnerWritable(path); err != nil && !os.IsNotExist(err) {
		resultErr = joinCanonical(resultErr, errors.New("make isolated media workspace removable"))
	}
	if err := os.RemoveAll(path); err != nil {
		resultErr = joinCanonical(resultErr, errors.New("remove isolated media workspace"))
	}
	return resultErr
}

func (recorder *Recorder) stageSource(name string, state *sourceState,
	timelinePath string, timeline []byte, timelineSHA string) (*stageWorkspace, error) {
	directory, err := os.MkdirTemp("", "openrealtime-media-stage-")
	if err != nil {
		return nil, errors.New("create isolated media workspace")
	}
	workspace := &stageWorkspace{path: directory}
	cleanupError := func(message string) (*stageWorkspace, error) {
		return nil, joinCanonical(errors.New(message), workspace.cleanup())
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return cleanupError("open isolated media workspace")
	}
	workspace.root = root
	identity, err := root.Stat(".")
	if err == nil {
		workspace.identity = identity
	}
	visible, visibleErr := os.Lstat(directory)
	if err != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(identity, visible) {
		return cleanupError("bind isolated media workspace identity")
	}
	if err := root.Mkdir("frames", 0o700); err != nil {
		return cleanupError("create isolated frame directory")
	}
	frameDirectory := filepath.ToSlash(filepath.Join("frames", name))
	if err := root.Mkdir(frameDirectory, 0o700); err != nil {
		return cleanupError("create isolated frame source directory")
	}
	if err := root.Mkdir("output", 0o700); err != nil {
		return cleanupError("create isolated encoder output directory")
	}
	audioPayload, audioSHA, _, err := readRegular(recorder.root, recorder.audio.Path, maximumTotalBytes)
	if err != nil || audioSHA != recorder.audio.SHA256 ||
		writeExclusive(root, recorder.audio.Path, audioPayload, 0o400) != nil {
		return cleanupError("stage exact attempt audio")
	}
	if digest(timeline) != timelineSHA || writeExclusive(root, timelinePath, timeline, 0o400) != nil {
		return cleanupError("stage exact frame timeline")
	}
	for _, frame := range state.frames {
		payload, sha, mediaType, err := readRegular(recorder.root, frame.Path, maximumFrameBytes)
		if err != nil || sha != frame.SHA256 || mediaType != frame.MediaType ||
			int64(len(payload)) != frame.SizeBytes || writeExclusive(root, frame.Path, payload, 0o400) != nil {
			return cleanupError("stage exact video frame")
		}
	}
	for _, path := range []string{frameDirectory, "frames"} {
		directory, err := root.Open(path)
		if err != nil || directory.Chmod(0o500) != nil || directory.Sync() != nil || directory.Close() != nil {
			if directory != nil {
				_ = directory.Close()
			}
			return cleanupError("make isolated media inputs read-only")
		}
	}
	for _, path := range []string{frameDirectory, "frames", "output", "."} {
		if err := syncDirectory(root, path); err != nil {
			return cleanupError("sync isolated media workspace")
		}
	}
	return workspace, nil
}

func (recorder *Recorder) verifyWorkspaceInputs(workspace *stageWorkspace, state *sourceState,
	timelinePath, timelineSHA string) error {
	if workspace == nil || workspace.root == nil || workspace.identity == nil {
		return errors.New("isolated media workspace is closed")
	}
	opened, openErr := workspace.root.Stat(".")
	visible, visibleErr := os.Lstat(workspace.path)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(workspace.identity, opened) || !os.SameFile(opened, visible) {
		return errors.New("isolated media workspace identity changed")
	}
	_, sha, mediaType, err := readRegular(workspace.root, recorder.audio.Path, maximumTotalBytes)
	if err != nil || sha != recorder.audio.SHA256 || mediaType != recorder.audio.MediaType {
		return errors.New("media plug-in modified staged audio")
	}
	_, sha, _, err = readRegular(workspace.root, timelinePath, maximumManifestBytes)
	if err != nil || sha != timelineSHA {
		return errors.New("media plug-in modified staged frame timeline")
	}
	for _, frame := range state.frames {
		payload, sha, mediaType, err := readRegular(workspace.root, frame.Path, maximumFrameBytes)
		if err != nil || sha != frame.SHA256 || mediaType != frame.MediaType ||
			int64(len(payload)) != frame.SizeBytes {
			return errors.New("media plug-in modified a staged frame")
		}
	}
	return nil
}

func (recorder *Recorder) verifyFinalRawInputs() error {
	if err := recorder.verifyAudio(); err != nil {
		return err
	}
	for _, name := range recorder.expected {
		state := recorder.sources[name]
		for _, frame := range state.frames {
			payload, sha, mediaType, err := readRegular(recorder.root, frame.Path, maximumFrameBytes)
			if err != nil || sha != frame.SHA256 || mediaType != frame.MediaType ||
				int64(len(payload)) != frame.SizeBytes {
				return errors.New("retained raw video frame changed")
			}
		}
	}
	return nil
}

func (recorder *Recorder) verifyDirectory() error {
	if recorder == nil || recorder.root == nil || recorder.directoryIdentity == nil {
		return errors.New("attempt media directory is closed")
	}
	opened, openErr := recorder.root.Stat(".")
	visible, visibleErr := os.Lstat(recorder.directory)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(recorder.directoryIdentity, opened) ||
		!os.SameFile(opened, visible) {
		return errors.New("attempt media directory identity changed")
	}
	return nil
}

func (recorder *Recorder) verifyAudio() error {
	if recorder.audio == nil {
		return nil
	}
	payload, sha, mediaType, err := readRegular(recorder.root, recorder.audio.Path, maximumTotalBytes)
	if err != nil || sha != recorder.audio.SHA256 || mediaType != recorder.audio.MediaType || len(payload) < 44 {
		return errors.New("retained attempt audio was modified or removed")
	}
	return nil
}

func (recorder *Recorder) addBytes(size int64) error {
	if size < 0 || recorder.totalBytes > maximumTotalBytes-size {
		return errors.New("attempt raw media exceeds its bounded size")
	}
	recorder.totalBytes += size
	return nil
}

func audioCoveredByEnd(spec AudioSpec, attemptEndUS int64) bool {
	if spec.SampleRateHz == 0 || attemptEndUS < 0 || spec.Frames > math.MaxUint64/1_000_000 {
		return false
	}
	if uint64(attemptEndUS) > math.MaxUint64/uint64(spec.SampleRateHz) {
		return true
	}
	return uint64(attemptEndUS)*uint64(spec.SampleRateHz) >= spec.Frames*1_000_000
}

// audioEndpointAligned avoids multiplying a potentially large timestamp by
// the sample rate. At 24 kHz, gcd(24_000, 1_000_000) is 8_000, so integer-
// microsecond endpoints occur exactly every 125 microseconds.
func audioEndpointAligned(attemptEndUS int64, sampleRateHz uint32) bool {
	if attemptEndUS < 0 || sampleRateHz == 0 {
		return false
	}
	microsecondsPerSecond := int64(1_000_000)
	left, right := microsecondsPerSecond, int64(sampleRateHz)
	for right != 0 {
		left, right = right, left%right
	}
	quantumUS := microsecondsPerSecond / left
	return attemptEndUS%quantumUS == 0
}

func buildConcatTimeline(frames []Frame, attemptEndUS int64) ([]byte, error) {
	if len(frames) == 0 || attemptEndUS <= frames[len(frames)-1].EpisodeAtUS {
		return nil, errors.New("frame timeline requires frames before the exact attempt end")
	}
	var output bytes.Buffer
	output.WriteString("ffconcat version 1.0\n")
	for index, frame := range frames {
		endUS := attemptEndUS
		if index+1 < len(frames) {
			endUS = frames[index+1].EpisodeAtUS
		}
		if endUS-frame.EpisodeAtUS < 1000 {
			return nil, errors.New("frame timeline intervals must be at least one millisecond")
		}
		output.WriteString("file '")
		output.WriteString(frame.Path)
		output.WriteString("'\nduration ")
		output.WriteString(formatDurationUS(endUS - frame.EpisodeAtUS))
		output.WriteByte('\n')
	}
	output.WriteString("file '")
	output.WriteString(frames[len(frames)-1].Path)
	output.WriteString("'\n")
	return output.Bytes(), nil
}

func formatDurationUS(durationUS int64) string {
	return strconv.FormatInt(durationUS/1_000_000, 10) + "." +
		fmtSixDigits(durationUS%1_000_000)
}

func fmtSixDigits(value int64) string {
	text := strconv.FormatInt(value, 10)
	return strings.Repeat("0", 6-len(text)) + text
}

func framePTSDigest(frames []Frame) string {
	var canonical strings.Builder
	for _, frame := range frames {
		canonical.WriteString(strconv.FormatInt(frame.EpisodeAtUS, 10))
		canonical.WriteByte('\n')
	}
	return digest([]byte(canonical.String()))
}

func canonicalSources(values []string) ([]string, error) {
	if len(values) > maximumVideoSources {
		return nil, errors.New("attempt media has too many video sources")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateToken("expected video source", value, 128); err != nil {
			return nil, err
		}
		if strings.Contains(value, "..") {
			return nil, errors.New("expected video source contains a noncanonical dot sequence")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("expected video source is duplicated")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validateToken(label, value string, maximum int) error {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return errors.New(label + " is empty, oversized, or noncanonical")
	}
	for _, character := range value {
		if !(unicode.IsLetter(character) || unicode.IsDigit(character) ||
			character == '-' || character == '_' || character == '.') {
			return errors.New(label + " contains unsupported characters")
		}
	}
	if value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return errors.New(label + " is not a safe path token")
	}
	return nil
}

func marshalManifest(manifest Manifest) ([]byte, error) {
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func cloneAudio(source *AudioArtifact) *AudioArtifact {
	if source == nil {
		return nil
	}
	copy := *source
	copy.AudioSpec.ChannelLayout = slices.Clone(source.AudioSpec.ChannelLayout)
	return &copy
}

func cloneManifest(source Manifest) Manifest {
	copy := source
	copy.Audio = cloneAudio(source.Audio)
	if source.Encoder != nil {
		evidence := *source.Encoder
		copy.Encoder = &evidence
	}
	if source.Attestor != nil {
		evidence := *source.Attestor
		copy.Attestor = &evidence
	}
	copy.Video = make([]VideoSource, len(source.Video))
	for index := range source.Video {
		copy.Video[index] = source.Video[index]
		copy.Video[index].Frames = slices.Clone(source.Video[index].Frames)
		if source.Video[index].Playable != nil {
			playable := *source.Video[index].Playable
			copy.Video[index].Playable = &playable
		}
		if source.Video[index].PlayableSpec != nil {
			spec := *source.Video[index].PlayableSpec
			copy.Video[index].PlayableSpec = &spec
		}
		if source.Video[index].AttestationReport != nil {
			report := *source.Video[index].AttestationReport
			copy.Video[index].AttestationReport = &report
		}
	}
	return copy
}

func cloneReceipt(source CompletionReceipt) CompletionReceipt {
	return CompletionReceipt{Manifest: cloneManifest(source.Manifest), ManifestSHA256: source.ManifestSHA256}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
