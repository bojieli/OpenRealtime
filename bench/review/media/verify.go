package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

type regularArtifactReader func(*os.Root, string, int64) ([]byte, string, string, error)

type artifactAnchor struct {
	identity  os.FileInfo
	sha256    string
	sizeBytes int64
	mediaType string
}

type verificationArtifacts struct {
	anchors map[string]artifactAnchor
}

func newVerificationArtifacts() *verificationArtifacts {
	return &verificationArtifacts{anchors: make(map[string]artifactAnchor)}
}

func (artifacts *verificationArtifacts) read(
	root *os.Root, name string, maximum int64,
) ([]byte, string, string, error) {
	payload, sha, mediaType, identity, err := readRegularIdentity(root, name, maximum)
	if err != nil {
		return nil, "", "", err
	}
	actual := artifactAnchor{
		identity: identity, sha256: sha, sizeBytes: int64(len(payload)), mediaType: mediaType,
	}
	if expected, exists := artifacts.anchors[name]; exists {
		if !sameArtifactAnchor(expected, actual) {
			return nil, "", "", errors.New("verified attempt media artifact changed between reads")
		}
	} else {
		artifacts.anchors[name] = actual
	}
	return payload, sha, mediaType, nil
}

func sameArtifactAnchor(expected, actual artifactAnchor) bool {
	return expected.identity != nil && actual.identity != nil &&
		os.SameFile(expected.identity, actual.identity) &&
		expected.identity.Mode() == actual.identity.Mode() &&
		expected.identity.ModTime().Equal(actual.identity.ModTime()) &&
		expected.sha256 == actual.sha256 && expected.sizeBytes == actual.sizeBytes &&
		expected.mediaType == actual.mediaType
}

func (artifacts *verificationArtifacts) revalidate(root *os.Root) error {
	paths := make([]string, 0, len(artifacts.anchors))
	for path := range artifacts.anchors {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		expected := artifacts.anchors[path]
		_, sha, mediaType, identity, err := readRegularIdentity(root, path, expected.sizeBytes)
		actualSize := int64(-1)
		if identity != nil {
			actualSize = identity.Size()
		}
		actual := artifactAnchor{
			identity: identity, sha256: sha, sizeBytes: actualSize, mediaType: mediaType,
		}
		if err != nil || !sameArtifactAnchor(expected, actual) {
			return errors.New("attempt media artifact changed during verification")
		}
	}
	return nil
}

func (artifacts *verificationArtifacts) validateTree(root *os.Root, requireReadOnly bool) error {
	expectedFiles := make(map[string]struct{}, len(artifacts.anchors))
	expectedDirectories := map[string]struct{}{".": {}}
	for path := range artifacts.anchors {
		expectedFiles[path] = struct{}{}
		for directory := filepath.ToSlash(filepath.Dir(path)); directory != "."; directory = filepath.ToSlash(filepath.Dir(directory)) {
			expectedDirectories[directory] = struct{}{}
		}
	}
	return validateExactTree(root, expectedFiles, expectedDirectories, requireReadOnly)
}

type verifyBundleOptions struct {
	beforeFinalRevalidation func()
	closeRoot               func(*os.Root) error
}

// VerifyBundle strictly revalidates a completed read-only evidence directory.
// expectedManifestSHA256 must come from an externally retained CompletionReceipt.
func VerifyBundle(directory, expectedManifestSHA256 string) (Manifest, error) {
	return verifyBundle(directory, expectedManifestSHA256, verifyBundleOptions{})
}

func verifyBundle(directory, expectedManifestSHA256 string, options verifyBundleOptions) (
	result Manifest, returnErr error,
) {
	if !validDigest(expectedManifestSHA256) {
		return Manifest{}, errors.New("expected media manifest digest is not canonical SHA-256")
	}
	absolute, err := validateNewDirectory(directory)
	if err != nil {
		return Manifest{}, errors.New("completed attempt media directory is invalid")
	}
	visible, err := os.Lstat(absolute)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return Manifest{}, errors.New("completed attempt media path is not a non-symlink directory")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return Manifest{}, errors.New("open completed attempt media directory")
	}
	closeRoot := options.closeRoot
	if closeRoot == nil {
		closeRoot = func(root *os.Root) error { return root.Close() }
	}
	defer func() {
		if err := closeRoot(root); err != nil {
			result = Manifest{}
			returnErr = joinCanonical(returnErr, errors.New("close verified attempt media root"))
		}
	}()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(visible, opened) {
		return Manifest{}, errors.New("completed attempt media directory identity changed")
	}
	artifacts := newVerificationArtifacts()
	payload, manifestSHA, mediaType, err := artifacts.read(root, manifestPath, maximumManifestBytes)
	if err != nil || manifestSHA != expectedManifestSHA256 ||
		(mediaType != "application/json" && mediaType != "text/plain; charset=utf-8") ||
		strictjson.Validate(payload) != nil {
		return Manifest{}, errors.New("attempt media completion marker failed identity or JSON validation")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, errors.New("decode attempt media completion marker")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("attempt media completion marker contains trailing JSON")
	}
	canonical, err := marshalManifest(manifest)
	if err != nil || !bytes.Equal(canonical, payload) || digest(canonical) != expectedManifestSHA256 {
		return Manifest{}, errors.New("attempt media completion marker is not canonical")
	}
	if err := validateManifestAndArtifacts(root, absolute, manifest, true, artifacts.read); err != nil {
		return Manifest{}, err
	}
	if options.beforeFinalRevalidation != nil {
		options.beforeFinalRevalidation()
	}
	if err := artifacts.revalidate(root); err != nil {
		return Manifest{}, err
	}
	if err := artifacts.validateTree(root, true); err != nil {
		return Manifest{}, err
	}
	finalPayload, finalSHA, _, err := artifacts.read(root, manifestPath, maximumManifestBytes)
	finalOpened, openedErr := root.Stat(".")
	finalVisible, visibleErr := os.Lstat(absolute)
	if err != nil || openedErr != nil || visibleErr != nil ||
		finalVisible.Mode()&os.ModeSymlink != 0 || !finalVisible.IsDir() ||
		!os.SameFile(opened, finalOpened) || !os.SameFile(finalOpened, finalVisible) ||
		finalSHA != expectedManifestSHA256 || !bytes.Equal(finalPayload, payload) {
		return Manifest{}, errors.New("attempt media directory or completion marker changed during verification")
	}
	return cloneManifest(manifest), nil
}

func validateManifestAndArtifacts(root *os.Root, directory string, manifest Manifest, requireReadOnly bool,
	read regularArtifactReader) error {
	if read == nil {
		read = readRegular
	}
	if root == nil || manifest.Format != ManifestFormat || manifest.FormatVersion != ManifestFormatVersion ||
		!manifest.Complete || manifest.AttemptEndUS <= 0 {
		return errors.New("attempt media manifest header is invalid or incomplete")
	}
	expectedFiles := map[string]struct{}{}
	expectedDirectories := map[string]struct{}{".": {}}
	if requireReadOnly {
		expectedFiles[manifestPath] = struct{}{}
	}
	if manifest.Audio == nil && len(manifest.Video) > 0 {
		return errors.New("attempt media with video is missing time-aligned audio")
	}
	if manifest.Audio != nil {
		if manifest.Audio.Path != "audio.stereo.wav" || manifest.Audio.Kind != "audio" ||
			manifest.Audio.Role != "time_aligned_room_and_agent" || manifest.Audio.MediaType != "audio/wav" ||
			manifest.Audio.Validation != review.MediaValidationVersion || !validDigest(manifest.Audio.SHA256) {
			return errors.New("attempt media audio identity is invalid")
		}
		spec := manifest.Audio.AudioSpec
		if spec.Container != "wav" || spec.Encoding != "pcm_s16le" ||
			spec.SampleRateHz != reviewSampleRateHz || spec.Channels != reviewChannels || spec.Frames == 0 ||
			!slices.Equal(spec.ChannelLayout,
				[]string{"left:scripted_room_user", "right:agent_output"}) ||
			math.IsNaN(spec.DurationMS) || math.IsInf(spec.DurationMS, 0) ||
			math.Abs(spec.DurationMS-float64(spec.Frames)*1000/float64(spec.SampleRateHz)) > 1e-9 ||
			!audioCoveredByEnd(spec, manifest.AttemptEndUS) {
			return errors.New("attempt media audio shape or duration is invalid")
		}
		payload, sha, mediaType, err := read(root, manifest.Audio.Path, maximumTotalBytes)
		if err != nil || sha != manifest.Audio.SHA256 || mediaType != manifest.Audio.MediaType || len(payload) < 44 {
			return errors.New("attempt media audio bytes do not match the manifest")
		}
		expectedFiles[manifest.Audio.Path] = struct{}{}
	}

	if len(manifest.Video) == 0 {
		if manifest.Encoder != nil || manifest.Attestor != nil {
			return errors.New("attempt media without video contains unexpected plug-in provenance")
		}
	} else {
		if manifest.Encoder == nil || manifest.Attestor == nil {
			return errors.New("attempt video media is missing encoder or attestor provenance")
		}
		if err := validateEvidence(root, manifest.Encoder, manifest.Attestor, expectedFiles, read); err != nil {
			return err
		}
		expectedDirectories["frames"] = struct{}{}
	}

	previousSource := ""
	totalFrames := 0
	for index, source := range manifest.Video {
		if err := validateToken("manifest video source", source.Source, 128); err != nil ||
			strings.Contains(source.Source, "..") || (index > 0 && source.Source <= previousSource) ||
			source.Width <= 0 || source.Height <= 0 || source.Width > 16_384 || source.Height > 16_384 ||
			len(source.Frames) == 0 || len(source.Frames) > maximumFrames {
			return errors.New("attempt media video source identity, order, or dimensions are invalid")
		}
		previousSource = source.Source
		totalFrames += len(source.Frames)
		if totalFrames > maximumFrames {
			return errors.New("attempt media manifest exceeds the global frame bound")
		}
		frameDirectory := filepath.ToSlash(filepath.Join("frames", source.Source))
		expectedDirectories[frameDirectory] = struct{}{}
		lastAtUS, lastWire := int64(-1), int64(math.MinInt64)
		for frameIndex, frame := range source.Frames {
			extension := ".jpg"
			if frame.MediaType == "image/png" {
				extension = ".png"
			}
			expectedPath := filepath.ToSlash(filepath.Join(frameDirectory,
				formatFrameSequence(frameIndex+1)+extension))
			if frame.Sequence != frameIndex+1 || frame.Path != expectedPath || !validDigest(frame.SHA256) ||
				frame.SizeBytes <= 0 || frame.SizeBytes > maximumFrameBytes ||
				(frame.MediaType != "image/png" && frame.MediaType != "image/jpeg") ||
				frame.EpisodeAtUS < 0 || (lastAtUS >= 0 && frame.EpisodeAtUS-lastAtUS < 1000) ||
				frame.EpisodeAtUS >= manifest.AttemptEndUS || frame.WireTimestamp < lastWire {
				return errors.New("attempt media frame identity, timing, or order is invalid")
			}
			payload, sha, mediaType, err := read(root, frame.Path, maximumFrameBytes)
			if err != nil || sha != frame.SHA256 || mediaType != frame.MediaType ||
				int64(len(payload)) != frame.SizeBytes {
				return errors.New("attempt media frame bytes do not match the manifest")
			}
			if _, err := validateFrame(bench.SessionVideoCapture{
				Source: source.Source, Width: source.Width, Height: source.Height,
				MediaType: frame.MediaType, WireTimestamp: frame.WireTimestamp,
				EpisodeAtMS: float64(frame.EpisodeAtUS) / 1000, Data: payload,
			}); err != nil {
				return errors.New("attempt media frame is not fully decodable")
			}
			expectedFiles[frame.Path] = struct{}{}
			lastAtUS, lastWire = frame.EpisodeAtUS, frame.WireTimestamp
		}
		ptsSHA := framePTSDigest(source.Frames)
		if source.FramePTSUSSHA256 != ptsSHA {
			return errors.New("attempt media frame PTS digest is invalid")
		}
		timeline, err := buildConcatTimeline(source.Frames, manifest.AttemptEndUS)
		if err != nil || source.TimelinePath != source.Source+".ffconcat" ||
			source.TimelineSHA256 != digest(timeline) {
			return errors.New("attempt media frame timeline identity is invalid")
		}
		retainedTimeline, sha, _, err := read(root, source.TimelinePath, maximumManifestBytes)
		if err != nil || sha != source.TimelineSHA256 || !bytes.Equal(retainedTimeline, timeline) {
			return errors.New("attempt media frame timeline bytes are invalid")
		}
		expectedFiles[source.TimelinePath] = struct{}{}

		if source.Playable == nil || source.PlayableSpec == nil || source.AttestationReport == nil {
			return errors.New("attempt media video source lacks playable output or attestation")
		}
		playablePath := source.Source + ".review.mp4"
		if source.Playable.Path != playablePath || source.Playable.Kind != "video" ||
			source.Playable.Role != "time_aligned_"+source.Source+"_and_room_agent" ||
			source.Playable.MediaType != "video/mp4" ||
			source.Playable.Validation != review.MediaValidationVersion || !validDigest(source.Playable.SHA256) {
			return errors.New("attempt media playable identity is invalid")
		}
		playablePayload, playableSHA, playableType, err := read(root, playablePath, maximumPlayableBytes)
		if err != nil || playableSHA != source.Playable.SHA256 || playableType != "video/mp4" {
			return errors.New("attempt media playable bytes do not match the manifest")
		}
		request := AttestationRequest{
			Source: source.Source, Width: source.Width, Height: source.Height,
			FirstFrameUS: source.Frames[0].EpisodeAtUS, AttemptEndUS: manifest.AttemptEndUS,
			ExpectedFrameCount: len(source.Frames), ExpectedFramePTSUSSHA256: ptsSHA,
			ExpectedOutputSHA256: playableSHA, ExpectedOutputBytes: int64(len(playablePayload)),
		}
		if err := source.PlayableSpec.validate(request); err != nil {
			return err
		}
		expectedFiles[playablePath] = struct{}{}

		report := source.AttestationReport
		if report.Path != source.Source+".attestation.json" || report.MediaType != "application/json" ||
			!validDigest(report.SHA256) || report.SizeBytes <= 0 || report.SizeBytes > maximumAttestationBytes {
			return errors.New("attempt media attestation report identity is invalid")
		}
		reportPayload, reportSHA, reportType, err := read(root, report.Path, maximumAttestationBytes)
		if err != nil || reportSHA != report.SHA256 || int64(len(reportPayload)) != report.SizeBytes ||
			(reportType != "application/json" && reportType != "text/plain; charset=utf-8") ||
			strictjson.Validate(reportPayload) != nil ||
			!bytes.Contains(reportPayload, []byte(playableSHA)) ||
			!bytes.Contains(reportPayload, []byte(ptsSHA)) {
			return errors.New("attempt media attestation report is invalid or not bound to output and PTS digests")
		}
		expectedFiles[report.Path] = struct{}{}
	}

	prepared, err := prepareReviewMedia(context.Background(), directory, manifest.ReviewMedia(), sensitiveMatcher{})
	if err != nil || len(prepared) != len(manifest.ReviewMedia()) {
		return errors.New("attempt media review inputs failed independent structural validation")
	}
	if err := validateExactTree(root, expectedFiles, expectedDirectories, requireReadOnly); err != nil {
		return err
	}
	return nil
}

func validateEvidence(root *os.Root, encoder *EncoderEvidence, attestor *AttestorEvidence,
	expectedFiles map[string]struct{}, read regularArtifactReader) error {
	if encoder.ImplementationPath != encoderImplementationPath ||
		encoder.ConfigurationPath != encoderConfigurationPath || encoder.Descriptor.Validate() != nil ||
		attestor.ImplementationPath != attestorImplementationPath ||
		attestor.ConfigurationPath != attestorConfigurationPath || attestor.Descriptor.Validate() != nil {
		return errors.New("attempt media plug-in provenance identity is invalid")
	}
	for _, artifact := range []struct {
		path, expected string
		maximum        int64
		configuration  bool
	}{
		{encoder.ImplementationPath, encoder.Descriptor.Implementation.SHA256, maximumEncoderImplementationBytes, false},
		{encoder.ConfigurationPath, encoder.Descriptor.ConfigurationSHA256, maximumEncoderConfigurationBytes, true},
		{attestor.ImplementationPath, attestor.Descriptor.Implementation.SHA256, maximumEncoderImplementationBytes, false},
		{attestor.ConfigurationPath, attestor.Descriptor.ConfigurationSHA256, maximumEncoderConfigurationBytes, true},
	} {
		payload, sha, _, err := read(root, artifact.path, artifact.maximum)
		if err != nil || sha != artifact.expected || (artifact.configuration && strictjson.Validate(payload) != nil) {
			return errors.New("attempt media plug-in provenance bytes are invalid")
		}
		expectedFiles[artifact.path] = struct{}{}
	}
	return nil
}

func validateExactTree(root *os.Root, expectedFiles, expectedDirectories map[string]struct{},
	requireReadOnly bool) error {
	seenFiles := make(map[string]struct{}, len(expectedFiles))
	seenDirectories := make(map[string]struct{}, len(expectedDirectories))
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("completed attempt media contains a symlink")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if _, expected := expectedDirectories[path]; !expected {
				return errors.New("completed attempt media contains an unexpected directory")
			}
			if requireReadOnly && info.Mode().Perm()&0o222 != 0 {
				return errors.New("completed attempt media directory is writable")
			}
			seenDirectories[path] = struct{}{}
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("completed attempt media contains a non-regular artifact")
		}
		if _, expected := expectedFiles[path]; !expected {
			return errors.New("completed attempt media contains an unexpected file")
		}
		if requireReadOnly && info.Mode().Perm()&0o222 != 0 {
			return errors.New("completed attempt media artifact is writable")
		}
		seenFiles[path] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seenFiles) != len(expectedFiles) || len(seenDirectories) != len(expectedDirectories) {
		return errors.New("completed attempt media tree is incomplete")
	}
	return nil
}

func formatFrameSequence(sequence int) string {
	text := "000000" + strconvItoa(sequence)
	return text[len(text)-6:]
}

func strconvItoa(value int) string {
	// Keeping path formatting here ensures verifier and writer share one policy.
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%10]
		value /= 10
	}
	return string(buffer[index:])
}
