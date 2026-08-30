package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
)

type fixtureEncoder struct {
	mu             sync.Mutex
	descriptor     EncoderDescriptor
	implementation []byte
	configuration  []byte
	claimed        atomic.Bool
	claimErr       error
	claimHook      func()
	claimStarted   chan struct{}
	afterClaimHook func()
	mode           string
	externalFinal  string
	requests       []EncodeRequest
	started        chan struct{}
	closeCalls     atomic.Int32
}

func newFixtureEncoder(mode string) *fixtureEncoder {
	implementation := []byte("fixture encoder implementation v2")
	configuration := []byte(`{"codec":"fixture-h264-aac","sandbox":"fixture"}`)
	return &fixtureEncoder{
		mode: mode, implementation: implementation, configuration: configuration,
		descriptor: EncoderDescriptor{
			Name: "fixture_encoder", Version: "fixture_2",
			Implementation: review.ContentIdentity{
				Version: "openrealtime.fixture-encoder.impl.v2", SHA256: digest(implementation),
			},
			ConfigurationSHA256: digest(configuration),
		},
	}
}

func (encoder *fixtureEncoder) Descriptor() EncoderDescriptor {
	if encoder.claimed.Load() && encoder.afterClaimHook != nil {
		encoder.afterClaimHook()
	}
	encoder.mu.Lock()
	defer encoder.mu.Unlock()
	return encoder.descriptor
}

func (encoder *fixtureEncoder) Implementation() []byte {
	encoder.mu.Lock()
	defer encoder.mu.Unlock()
	return slices.Clone(encoder.implementation)
}

func (encoder *fixtureEncoder) Configuration() []byte {
	encoder.mu.Lock()
	defer encoder.mu.Unlock()
	return slices.Clone(encoder.configuration)
}

func (encoder *fixtureEncoder) Claim(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if encoder.claimErr != nil {
		return encoder.claimErr
	}
	if !encoder.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture encoder already claimed")
	}
	if encoder.claimStarted != nil {
		close(encoder.claimStarted)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	if encoder.claimHook != nil {
		encoder.claimHook()
	}
	return nil
}

func (encoder *fixtureEncoder) Encode(ctx context.Context, request EncodeRequest) error {
	encoder.mu.Lock()
	encoder.requests = append(encoder.requests, request)
	mode, started := encoder.mode, encoder.started
	encoder.mu.Unlock()
	if mode == "cancel" {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return context.Cause(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode == "secret_error" {
		return errors.New("encoder exposed super-secret-value")
	}
	if mode == "mutate_audio" {
		if err := os.Chmod(request.AudioPath, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(request.AudioPath, []byte("mutated"), 0o600); err != nil {
			return err
		}
	}
	if mode == "mutate_final" {
		path := filepath.Join(encoder.externalFinal, "audio.stereo.wav")
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte("mutated-final"), 0o600); err != nil {
			return err
		}
	}
	if mode == "forge_marker" {
		if err := os.WriteFile(filepath.Join(request.WorkspaceRoot, manifestPath),
			[]byte(`{"complete":true}`), 0o600); err != nil {
			return err
		}
	}
	if filepath.Dir(request.OutputPath) != filepath.Join(request.WorkspaceRoot, "output") {
		return errors.New("output is not isolated beneath output/")
	}
	// A real encoder binds the output bytes to this source's frame stream. Keep
	// the hermetic encoder faithful to that contract so a multi-source attempt
	// does not manufacture byte-identical review artifacts with different roles.
	payload := structuralAVMP4Fixture(request.Source)
	if mode == "symlink_output" {
		target := filepath.Join(filepath.Dir(request.OutputPath), "target.mp4")
		if err := os.WriteFile(target, payload, 0o600); err != nil {
			return err
		}
		return os.Symlink("target.mp4", request.OutputPath)
	}
	file, err := os.OpenFile(request.OutputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if mode == "descriptor_drift" {
		encoder.mu.Lock()
		encoder.descriptor.Version = "fixture_3"
		encoder.mu.Unlock()
	}
	return nil
}

func (encoder *fixtureEncoder) Close() error {
	encoder.closeCalls.Add(1)
	if encoder.mode == "close_error" {
		return errors.New("close exposed super-secret-value")
	}
	return nil
}

type fixtureAttestor struct {
	mu             sync.Mutex
	descriptor     AttestorDescriptor
	implementation []byte
	configuration  []byte
	claimed        atomic.Bool
	claimErr       error
	claimHook      func()
	claimStarted   chan struct{}
	mode           string
	requests       []AttestationRequest
	started        chan struct{}
	closeCalls     atomic.Int32
	closeHook      func()
}

func newFixtureAttestor(mode string) *fixtureAttestor {
	implementation := []byte("independent fixture full decoder v2")
	configuration := []byte(`{"decoder":"fixture-full-decode","isolation":"fixture"}`)
	return &fixtureAttestor{
		mode: mode, implementation: implementation, configuration: configuration,
		descriptor: AttestorDescriptor{
			Name: "fixture_full_decoder", Version: "fixture_2",
			Capability: FullDecodeAttestationCapability,
			Implementation: review.ContentIdentity{
				Version: "openrealtime.fixture-attestor.impl.v2", SHA256: digest(implementation),
			},
			ConfigurationSHA256: digest(configuration),
		},
	}
}

func (attestor *fixtureAttestor) Descriptor() AttestorDescriptor {
	attestor.mu.Lock()
	defer attestor.mu.Unlock()
	return attestor.descriptor
}

func (attestor *fixtureAttestor) Implementation() []byte {
	attestor.mu.Lock()
	defer attestor.mu.Unlock()
	return slices.Clone(attestor.implementation)
}

func (attestor *fixtureAttestor) Configuration() []byte {
	attestor.mu.Lock()
	defer attestor.mu.Unlock()
	return slices.Clone(attestor.configuration)
}

func (attestor *fixtureAttestor) Claim(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if attestor.claimErr != nil {
		return attestor.claimErr
	}
	if !attestor.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture attestor already claimed")
	}
	if attestor.claimStarted != nil {
		close(attestor.claimStarted)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	if attestor.claimHook != nil {
		attestor.claimHook()
	}
	return nil
}

func (attestor *fixtureAttestor) Attest(ctx context.Context, request AttestationRequest) (Attestation, error) {
	attestor.mu.Lock()
	attestor.requests = append(attestor.requests, request)
	mode, started := attestor.mode, attestor.started
	attestor.mu.Unlock()
	if mode == "cancel" {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return Attestation{}, context.Cause(ctx)
	}
	if err := ctx.Err(); err != nil {
		return Attestation{}, err
	}
	if mode == "secret_error" {
		return Attestation{}, errors.New("attestor exposed super-secret-value")
	}
	spec := PlayableSpec{
		OutputSHA256: request.ExpectedOutputSHA256,
		Container:    "mp4", VideoCodec: "h264", PixelFormat: "yuv420p",
		Width: request.Width, Height: request.Height,
		EncodedWidth: request.Width + request.Width%2, EncodedHeight: request.Height + request.Height%2,
		GeometryPolicy: YUV420PPadRightBottomBlackToEvenV1,
		AudioCodec:     "aac", AudioSampleRateHz: reviewSampleRateHz, AudioChannels: reviewChannels,
		DurationUS: request.AttemptEndUS, AudioStartUS: 0, AudioEndUS: request.AttemptEndUS,
		VideoStartUS: request.FirstFrameUS, VideoEndUS: request.AttemptEndUS,
		VideoFrameCount:       request.ExpectedFrameCount,
		VideoFramePTSUSSHA256: request.ExpectedFramePTSUSSHA256,
	}
	if mode == "bad_spec" {
		spec.OutputSHA256 = digest([]byte("wrong"))
	}
	report, _ := json.Marshal(map[string]any{
		"decoder": "fixture", "full_decode": true,
		"output_sha256":       request.ExpectedOutputSHA256,
		"frame_pts_us_sha256": request.ExpectedFramePTSUSSHA256,
	})
	if mode == "bad_report" {
		report = []byte(`{"full_decode":true}`)
	}
	if mode == "array_report" {
		report, _ = json.Marshal([]string{request.ExpectedOutputSHA256, request.ExpectedFramePTSUSSHA256})
	}
	if mode == "secret_report" {
		report = []byte(`{"detail":"super-secret-value"}`)
	}
	if mode == "mutate_output" {
		if err := os.WriteFile(request.OutputPath,
			append(structuralAVMP4Fixture(request.Source), 0), 0o600); err != nil {
			return Attestation{}, err
		}
	}
	if mode == "swap_output" {
		payload, err := os.ReadFile(request.OutputPath)
		if err != nil {
			return Attestation{}, err
		}
		if err := os.Remove(request.OutputPath); err != nil {
			return Attestation{}, err
		}
		if err := os.WriteFile(request.OutputPath, payload, 0o600); err != nil {
			return Attestation{}, err
		}
	}
	if mode == "descriptor_drift" {
		attestor.mu.Lock()
		attestor.descriptor.Version = "fixture_3"
		attestor.mu.Unlock()
	}
	return Attestation{Spec: spec, Report: report}, nil
}

func (attestor *fixtureAttestor) Close() error {
	attestor.closeCalls.Add(1)
	if attestor.closeHook != nil {
		attestor.closeHook()
	}
	if attestor.mode == "close_error" {
		return errors.New("close exposed super-secret-value")
	}
	return nil
}

func TestRecorderRetainsExactMultiSourceMediaAndReceipt(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "attempt")
	t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
	encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
	recorder, err := New(t.Context(), Config{
		Directory: directory, RequireAudio: true,
		ExpectedVideoSources: []string{"screen", "camera"}, Encoder: encoder, Attestor: attestor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	camera := jpegFixture(t, 64, 48, color.RGBA{G: 0xff, A: 0xff})
	screen := pngFixture(t, 64, 48, color.RGBA{B: 0xff, A: 0xff})
	for _, capture := range []bench.SessionVideoCapture{
		{Source: "camera", Width: 64, Height: 48, MediaType: "image/jpeg", WireTimestamp: 100,
			EpisodeAtMS: 50, Data: camera},
		{Source: "screen", Width: 64, Height: 48, MediaType: "image/png", WireTimestamp: 101,
			EpisodeAtMS: 75.125, Data: screen},
	} {
		if err := recorder.CaptureVideo(capture); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := recorder.Finalize(t.Context(), 500)
	if err != nil {
		t.Fatal(err)
	}
	manifest := receipt.Manifest
	if !manifest.Complete || manifest.AttemptEndUS != 500_000 ||
		manifest.Encoder == nil || manifest.Attestor == nil || len(manifest.Video) != 2 ||
		manifest.Video[0].Source != "camera" || manifest.Video[1].Source != "screen" ||
		manifest.Video[0].Playable == nil || manifest.Video[1].Playable == nil ||
		manifest.Video[0].Playable.SHA256 == manifest.Video[1].Playable.SHA256 ||
		manifest.Video[0].Frames[0].EpisodeAtUS != 50_000 ||
		manifest.Video[1].Frames[0].EpisodeAtUS != 75_125 || !validDigest(receipt.ManifestSHA256) {
		t.Fatalf("receipt = %+v", receipt)
	}
	verified, err := VerifyBundle(directory, receipt.ManifestSHA256)
	if err != nil || !equalManifest(verified, manifest) {
		t.Fatalf("VerifyBundle() = %+v, %v", verified, err)
	}
	if encoder.closeCalls.Load() != 1 || attestor.closeCalls.Load() != 1 {
		t.Fatalf("plug-in closes encoder=%d attestor=%d", encoder.closeCalls.Load(), attestor.closeCalls.Load())
	}
	for _, request := range encoder.requests {
		if pathInside(directory, request.WorkspaceRoot) || filepath.Dir(request.OutputPath) !=
			filepath.Join(request.WorkspaceRoot, "output") || pathInside(directory, request.AudioPath) ||
			pathInside(directory, request.ConcatPath) {
			t.Fatalf("encoder received a final-root or invalid staging path: %+v", request)
		}
	}
	for _, request := range attestor.requests {
		if pathInside(directory, request.WorkspaceRoot) || pathInside(directory, request.OutputPath) ||
			request.ExpectedOutputBytes <= 0 || !validDigest(request.ExpectedOutputSHA256) {
			t.Fatalf("attestor request = %+v", request)
		}
	}
	manifest.Video[0].Frames[0].Path = "mutated"
	again, err := recorder.Finalize(t.Context(), 999)
	if err != nil || again.Manifest.Video[0].Frames[0].Path == "mutated" ||
		again.Manifest.AttemptEndUS != 500_000 || encoder.closeCalls.Load() != 1 {
		t.Fatalf("idempotent Finalize() = %+v, %v", again, err)
	}
}

func TestRecorderCompletesAudioOnlyWithoutMediaPlugins(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
	recorder, err := New(t.Context(), Config{Directory: directory, RequireAudio: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 100)
	if err != nil || !receipt.Manifest.Complete || receipt.Manifest.Encoder != nil ||
		receipt.Manifest.Attestor != nil || len(receipt.Manifest.Video) != 0 {
		t.Fatalf("Finalize() = %+v, %v", receipt, err)
	}
	if _, err := VerifyBundle(directory, receipt.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBundleRejectsExternalHardLink(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "attempt")
	t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
	recorder, err := New(t.Context(), Config{Directory: directory, RequireAudio: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(
		filepath.Join(directory, "audio.stereo.wav"),
		filepath.Join(parent, "outside.wav"),
	); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := VerifyBundle(directory, receipt.ManifestSHA256); err == nil {
		t.Fatal("VerifyBundle accepted media with an external hard link")
	}
}

func TestRecorderRejectsImplicitDuplicateAndSubMillisecondTiming(t *testing.T) {
	for _, test := range []struct {
		name       string
		timestamps []float64
	}{
		{"duplicate", []float64{10, 10}},
		{"sub millisecond", []float64{10, 10.999}},
		{"noncanonical microsecond", []float64{0.0001}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := mustRecorder(t, filepath.Join(t.TempDir(), "attempt"), []string{"screen"},
				newFixtureEncoder(""), newFixtureAttestor(""), nil)
			for index, timestamp := range test.timestamps {
				err := recorder.CaptureVideo(bench.SessionVideoCapture{
					Source: "screen", Width: 64, Height: 48, MediaType: "image/png",
					WireTimestamp: int64(index), EpisodeAtMS: timestamp,
					Data: pngFixture(t, 64, 48, color.RGBA{A: 0xff}),
				})
				if index == len(test.timestamps)-1 && err == nil {
					t.Fatalf("CaptureVideo(%v) unexpectedly succeeded", timestamp)
				}
			}
			_ = recorder.Abort()
		})
	}

	for _, test := range []struct {
		name  string
		endMS float64
	}{
		{"noncanonical end", 500.0001},
		{"int64 conversion boundary", float64(math.MaxInt64) / 1000},
		{"before audio", 50},
		{"at final frame", 100},
		{"sub-millisecond final interval", 100.999},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := mustRecorder(t, filepath.Join(t.TempDir(), "attempt"), []string{"screen"},
				newFixtureEncoder(""), newFixtureAttestor(""), nil)
			captureStandard(t, recorder)
			if receipt, err := recorder.Finalize(t.Context(), test.endMS); err == nil || receipt.Manifest.Complete {
				t.Fatalf("Finalize(%v) = %+v, %v", test.endMS, receipt, err)
			}
		})
	}
}

func TestRecorderBindsExactAttemptEndAndCanonicalFramePTS(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
	recorder := mustRecorder(t, directory, []string{"screen"}, encoder, attestor, nil)
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	first := pngFixture(t, 64, 48, color.RGBA{R: 0x40, A: 0xff})
	firstRetained := slices.Clone(first)
	for index, capture := range []bench.SessionVideoCapture{
		{Source: "screen", Width: 64, Height: 48, MediaType: "image/png",
			WireTimestamp: 10, EpisodeAtMS: 125.123, Data: first},
		{Source: "screen", Width: 64, Height: 48, MediaType: "image/png",
			WireTimestamp: 11, EpisodeAtMS: 800, Data: pngFixture(t, 64, 48, color.RGBA{G: 0x80, A: 0xff})},
	} {
		if err := recorder.CaptureVideo(capture); err != nil {
			t.Fatalf("CaptureVideo(%d) = %v", index, err)
		}
		if index == 0 {
			first[0] ^= 0xff
		}
	}
	receipt, err := recorder.Finalize(t.Context(), 1_250)
	if err != nil {
		t.Fatal(err)
	}
	wantTimeline := "ffconcat version 1.0\n" +
		"file 'frames/screen/000001.png'\nduration 0.674877\n" +
		"file 'frames/screen/000002.png'\nduration 0.450000\n" +
		"file 'frames/screen/000002.png'\n"
	timeline, err := os.ReadFile(filepath.Join(directory, "screen.ffconcat"))
	if err != nil || string(timeline) != wantTimeline {
		t.Fatalf("timeline = %q, %v; want %q", timeline, err, wantTimeline)
	}
	wantPTS := digest([]byte("125123\n800000\n"))
	video := receipt.Manifest.Video[0]
	if video.FramePTSUSSHA256 != wantPTS || video.TimelineSHA256 != digest([]byte(wantTimeline)) ||
		video.PlayableSpec == nil || video.PlayableSpec.VideoStartUS != 125_123 ||
		video.PlayableSpec.VideoEndUS != 1_250_000 || video.PlayableSpec.DurationUS != 1_250_000 ||
		len(encoder.requests) != 1 || encoder.requests[0].FirstFrameUS != 125_123 ||
		encoder.requests[0].AttemptEndUS != 1_250_000 || encoder.requests[0].ExpectedFrames != 2 ||
		len(attestor.requests) != 1 || attestor.requests[0].ExpectedFramePTSUSSHA256 != wantPTS {
		t.Fatalf("exact timing contract was not retained: video=%+v encode=%+v attest=%+v",
			video, encoder.requests, attestor.requests)
	}
	retained, err := os.ReadFile(filepath.Join(directory, "frames/screen/000001.png"))
	if err != nil || !bytes.Equal(retained, firstRetained) {
		t.Fatalf("retained sent frame changed with caller buffer: equal=%v err=%v",
			bytes.Equal(retained, firstRetained), err)
	}
}

func TestRecorderAudioEndpointRequiresAnExactSampleBoundary(t *testing.T) {
	t.Run("125 microseconds succeeds", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "attempt")
		recorder, err := New(t.Context(), Config{Directory: directory, RequireAudio: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
		if err := recorder.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{1, 2, 3},
		}); err != nil {
			t.Fatal(err)
		}
		receipt, err := recorder.Finalize(t.Context(), 0.125)
		if err != nil || !receipt.Manifest.Complete || receipt.Manifest.AttemptEndUS != 125 ||
			receipt.Manifest.Audio == nil || receipt.Manifest.Audio.AudioSpec.Frames != 3 {
			t.Fatalf("Finalize(125us) = %+v, %v", receipt, err)
		}
	})

	t.Run("124 microseconds fails", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "attempt")
		recorder, err := New(t.Context(), Config{Directory: directory, RequireAudio: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
		if err := recorder.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{1},
		}); err != nil {
			t.Fatal(err)
		}
		if receipt, err := recorder.Finalize(t.Context(), 0.124); err == nil || receipt.Manifest.Complete {
			t.Fatalf("Finalize(124us) = %+v, %v", receipt, err)
		}
		if _, err := os.Lstat(filepath.Join(directory, manifestPath)); !os.IsNotExist(err) {
			t.Fatalf("unaligned attempt exposed a completion marker: %v", err)
		}
	})

	t.Run("rejected before video derivation", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "attempt")
		encoder := newFixtureEncoder("")
		recorder := mustRecorder(t, directory, []string{"screen"}, encoder, newFixtureAttestor(""), nil)
		captureStandard(t, recorder)
		if receipt, err := recorder.Finalize(t.Context(), 500.124); err == nil || receipt.Manifest.Complete {
			t.Fatalf("Finalize(500124us) = %+v, %v", receipt, err)
		}
		if len(encoder.requests) != 0 {
			t.Fatalf("unaligned audio endpoint invoked encoder %d times", len(encoder.requests))
		}
		if _, err := os.Lstat(filepath.Join(directory, "screen.ffconcat")); !os.IsNotExist(err) {
			t.Fatalf("unaligned endpoint retained a derived timeline: %v", err)
		}
	})
}

func TestAudioEndpointAlignmentAvoidsTimestampMultiplicationOverflow(t *testing.T) {
	const quantumUS = int64(125)
	aligned := int64(math.MaxInt64) - int64(math.MaxInt64)%quantumUS
	if !audioEndpointAligned(aligned, 24_000) {
		t.Fatalf("near-MaxInt64 aligned endpoint %d was rejected", aligned)
	}
	if audioEndpointAligned(aligned-1, 24_000) || audioEndpointAligned(-1, 24_000) ||
		audioEndpointAligned(aligned, 0) {
		t.Fatal("audio endpoint alignment accepted an unaligned or invalid boundary")
	}
}

func TestRecorderIsolatesPluginsAndFailsClosedOnMutations(t *testing.T) {
	for _, test := range []struct {
		name         string
		encoderMode  string
		attestorMode string
		wantSuccess  bool
	}{
		{"workspace marker cannot forge final", "forge_marker", "", true},
		{"staged input mutation", "mutate_audio", "", false},
		{"symlink output", "symlink_output", "", false},
		{"final raw mutation", "mutate_final", "", false},
		{"output mutation after digest", "", "mutate_output", false},
		{"output inode swap after digest", "", "swap_output", false},
		{"attestation spec mismatch", "", "bad_spec", false},
		{"attestation report unbound", "", "bad_report", false},
		{"attestation report is not an object", "", "array_report", false},
		{"attestation report secret", "", "secret_report", false},
		{"encoder descriptor drift", "descriptor_drift", "", false},
		{"attestor descriptor drift", "", "descriptor_drift", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			encoder := newFixtureEncoder(test.encoderMode)
			encoder.externalFinal = directory
			recorder := mustRecorder(t, directory, []string{"screen"},
				encoder, newFixtureAttestor(test.attestorMode),
				[]string{"super-secret-value"})
			captureStandard(t, recorder)
			receipt, err := recorder.Finalize(t.Context(), 500)
			if test.wantSuccess {
				if err != nil || !receipt.Manifest.Complete {
					t.Fatalf("Finalize() = %+v, %v", receipt, err)
				}
				payload, readErr := os.ReadFile(filepath.Join(directory, manifestPath))
				if readErr != nil || bytes.Equal(payload, []byte(`{"complete":true}`)) {
					t.Fatalf("final marker was forged: %q, %v", payload, readErr)
				}
				return
			}
			if err == nil || receipt.Manifest.Complete || strings.Contains(err.Error(), "super-secret-value") {
				t.Fatalf("Finalize() = %+v, %v", receipt, err)
			}
			if _, statErr := os.Lstat(filepath.Join(directory, manifestPath)); !os.IsNotExist(statErr) {
				t.Fatalf("failed attempt has a completion marker: %v", statErr)
			}
		})
	}
}

func TestRecorderInitializationFailureLeavesExactPathRetryable(t *testing.T) {
	for _, phase := range []string{
		"encoder_claim", "attestor_claim", "sensitive_configuration", "base64_sensitive_configuration",
		"json_escaped_sensitive_configuration", "sensitive_attestor_implementation",
		"encoder_claim_drift", "attestor_claim_drift",
	} {
		t.Run(phase, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
			config := Config{Directory: directory, RequireAudio: true,
				ExpectedVideoSources: []string{"screen"}, Encoder: encoder, Attestor: attestor}
			switch phase {
			case "encoder_claim":
				encoder.claimErr = errors.New("super-secret-value")
			case "attestor_claim":
				attestor.claimErr = errors.New("super-secret-value")
			case "sensitive_configuration":
				config.SensitiveValues = []string{"super-secret-value"}
				encoder.configuration = []byte(`{"token":"super-secret-value"}`)
				encoder.descriptor.ConfigurationSHA256 = digest(encoder.configuration)
			case "base64_sensitive_configuration":
				config.SensitiveValues = []string{"super-secret-value"}
				encoder.configuration = []byte(`{"token":"c3VwZXItc2VjcmV0LXZhbHVl"}`)
				encoder.descriptor.ConfigurationSHA256 = digest(encoder.configuration)
			case "json_escaped_sensitive_configuration":
				config.SensitiveValues = []string{"<super&secret>"}
				encoder.configuration = []byte(`{"token":"\u003csuper\u0026secret\u003e"}`)
				encoder.descriptor.ConfigurationSHA256 = digest(encoder.configuration)
			case "sensitive_attestor_implementation":
				config.SensitiveValues = []string{"super-secret-value"}
				attestor.implementation = []byte("implementation embeds super-secret-value")
				attestor.descriptor.Implementation.SHA256 = digest(attestor.implementation)
			case "encoder_claim_drift":
				encoder.claimHook = func() {
					encoder.mu.Lock()
					encoder.descriptor.Version = "fixture_claim_drift"
					encoder.mu.Unlock()
				}
			case "attestor_claim_drift":
				attestor.claimHook = func() {
					attestor.mu.Lock()
					attestor.configuration = []byte(`{"decoder":"drifted"}`)
					attestor.descriptor.ConfigurationSHA256 = digest(attestor.configuration)
					attestor.mu.Unlock()
				}
			}
			_, err := New(t.Context(), config)
			if err == nil || strings.Contains(err.Error(), "super-secret-value") {
				t.Fatalf("New() error = %v", err)
			}
			if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
				t.Fatalf("failed constructor consumed exact path: %v", statErr)
			}
			retry := mustRecorder(t, directory, []string{"screen"},
				newFixtureEncoder(""), newFixtureAttestor(""), nil)
			if err := retry.Abort(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecorderConstructorCancellationIsCanonicalAndRetryable(t *testing.T) {
	for _, phase := range []string{"before_claim", "encoder_claim", "attestor_claim", "post_claim_snapshot"} {
		t.Run(phase, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var started chan struct{}
			switch phase {
			case "before_claim":
				cancel(errors.New("super-secret-value"))
			case "encoder_claim":
				encoder.claimStarted = make(chan struct{})
				started = encoder.claimStarted
			case "attestor_claim":
				attestor.claimStarted = make(chan struct{})
				started = attestor.claimStarted
			case "post_claim_snapshot":
				encoder.afterClaimHook = func() { cancel(errors.New("super-secret-value")) }
			}
			type result struct {
				recorder *Recorder
				err      error
			}
			resultChannel := make(chan result, 1)
			go func() {
				recorder, err := New(ctx, Config{
					Directory: directory, RequireAudio: true, ExpectedVideoSources: []string{"screen"},
					Encoder: encoder, Attestor: attestor, SensitiveValues: []string{"super-secret-value"},
				})
				resultChannel <- result{recorder, err}
			}()
			if started != nil {
				<-started
				cancel(errors.New("super-secret-value"))
			}
			got := <-resultChannel
			if got.recorder != nil || !errors.Is(got.err, context.Canceled) ||
				strings.Contains(got.err.Error(), "super-secret-value") {
				t.Fatalf("New() = %v, %v", got.recorder, got.err)
			}
			if _, err := os.Lstat(directory); !os.IsNotExist(err) {
				t.Fatalf("canceled constructor consumed exact path: %v", err)
			}
			retry := mustRecorder(t, directory, []string{"screen"},
				newFixtureEncoder(""), newFixtureAttestor(""), nil)
			if err := retry.Abort(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecorderCleansDirectoryAfterPostCreateArtifactFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
	interferenceDone := make(chan struct{})
	encoder.claimHook = func() {
		go func() {
			defer close(interferenceDone)
			for {
				if info, err := os.Lstat(directory); err == nil && info.IsDir() {
					_ = os.Mkdir(filepath.Join(directory, "frames"), 0o700)
					return
				}
			}
		}()
	}
	_, err := New(t.Context(), Config{
		Directory: directory, RequireAudio: true, ExpectedVideoSources: []string{"screen"},
		Encoder: encoder, Attestor: attestor,
	})
	<-interferenceDone
	if err == nil {
		t.Fatal("interfered post-create initialization unexpectedly succeeded")
	}
	if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("post-create failure retained its owned attempt directory: %v", statErr)
	}
	retry := mustRecorder(t, directory, []string{"screen"},
		newFixtureEncoder(""), newFixtureAttestor(""), nil)
	if err := retry.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderCanonicalizesMidPluginCancellationAndSecretErrors(t *testing.T) {
	for _, phase := range []string{
		"encode_cancel", "attest_cancel", "close_cancel", "encode_error", "attest_error", "close_error",
	} {
		t.Run(phase, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
			var started chan struct{}
			switch phase {
			case "encode_cancel":
				encoder.mode, encoder.started = "cancel", make(chan struct{})
				started = encoder.started
			case "attest_cancel":
				attestor.mode, attestor.started = "cancel", make(chan struct{})
				started = attestor.started
			case "encode_error":
				encoder.mode = "secret_error"
			case "attest_error":
				attestor.mode = "secret_error"
			case "close_error":
				attestor.mode = "close_error"
			}
			recorder := mustRecorder(t, directory, []string{"screen"}, encoder, attestor,
				[]string{"super-secret-value"})
			captureStandard(t, recorder)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if phase == "close_cancel" {
				attestor.closeHook = func() { cancel(errors.New("super-secret-value")) }
			}
			type result struct {
				receipt CompletionReceipt
				err     error
			}
			resultChannel := make(chan result, 1)
			go func() {
				receipt, err := recorder.Finalize(ctx, 500)
				resultChannel <- result{receipt, err}
			}()
			if started != nil {
				<-started
				cancel(errors.New("super-secret-value"))
			}
			got := <-resultChannel
			if got.err == nil || got.receipt.Manifest.Complete || strings.Contains(got.err.Error(), "super-secret-value") {
				t.Fatalf("Finalize() = %+v, %v", got.receipt, got.err)
			}
			if strings.HasSuffix(phase, "_cancel") && !errors.Is(got.err, context.Canceled) {
				t.Fatalf("cancellation error = %v", got.err)
			}
			if _, statErr := os.Lstat(filepath.Join(directory, manifestPath)); !os.IsNotExist(statErr) {
				t.Fatalf("failed attempt has completion marker: %v", statErr)
			}
		})
	}
}

func TestRecorderPublishesNoMarkerWhenFinalArtifactImportFails(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	recorder := mustRecorder(t, directory, []string{"screen"},
		newFixtureEncoder(""), newFixtureAttestor(""), nil)
	captureStandard(t, recorder)
	if err := os.WriteFile(filepath.Join(directory, "screen.attestation.json"),
		[]byte(`{"external":"conflict"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if receipt, err := recorder.Finalize(t.Context(), 500); err == nil || receipt.Manifest.Complete {
		t.Fatalf("Finalize() = %+v, %v", receipt, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, manifestPath)); !os.IsNotExist(err) {
		t.Fatalf("failed finalization exposed a completion marker: %v", err)
	}
}

func TestVerifyBundleRejectsMutationExtrasAndSymlinks(t *testing.T) {
	for _, mutation := range []string{
		"manifest", "manifest_replacement", "media", "extra", "symlink", "directory_symlink",
	} {
		t.Run(mutation, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			recorder := mustRecorder(t, directory, []string{"screen"},
				newFixtureEncoder(""), newFixtureAttestor(""), nil)
			captureStandard(t, recorder)
			receipt, err := recorder.Finalize(t.Context(), 500)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "manifest":
				path := filepath.Join(directory, manifestPath)
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				payload[len(payload)-2] ^= 1
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatal(err)
				}
			case "manifest_replacement":
				path := filepath.Join(directory, manifestPath)
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				payload[bytes.Index(payload, []byte(ManifestFormat))] = 'x'
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, payload, 0o400); err != nil {
					t.Fatal(err)
				}
			case "media":
				path := filepath.Join(directory, "screen.review.mp4")
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(structuralAVMP4Fixture("screen"), 0), 0o600); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(directory, "extra.txt"), []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("audio.stereo.wav", filepath.Join(directory, "extra-link")); err != nil {
					t.Fatal(err)
				}
			case "directory_symlink":
				moved := directory + "-moved"
				t.Cleanup(func() { _ = makeTreeOwnerWritable(moved) })
				if err := os.Rename(directory, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(moved), directory); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := VerifyBundle(directory, receipt.ManifestSHA256); err == nil {
				t.Fatal("VerifyBundle unexpectedly accepted mutated evidence")
			}
		})
	}
}

func TestRecorderRemovesUnexpectedCompletionMarkerAndAbortIsIdempotent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	recorder := mustRecorder(t, directory, []string{"screen"},
		newFixtureEncoder(""), newFixtureAttestor(""), nil)
	if err := os.WriteFile(filepath.Join(directory, manifestPath), []byte(`{"complete":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Finalize(t.Context(), 500); err == nil {
		t.Fatal("Finalize accepted unexpected marker")
	}
	if _, err := os.Lstat(filepath.Join(directory, manifestPath)); !os.IsNotExist(err) {
		t.Fatalf("unexpected marker remained: %v", err)
	}
	if err := recorder.Abort(); err == nil {
		// Finalize already closed it; idempotent return of its stored error is expected.
		t.Fatal("Abort after failed Finalize unexpectedly erased its stored error")
	}

	other := mustRecorder(t, filepath.Join(t.TempDir(), "attempt"), []string{"screen"},
		newFixtureEncoder(""), newFixtureAttestor(""), nil)
	if err := other.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := other.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedMarkerInvalidationIsBoundToTheOriginalDirectory(t *testing.T) {
	makeCompleted := func(t *testing.T, directory string) CompletionReceipt {
		t.Helper()
		recorder := mustRecorder(t, directory, []string{"screen"},
			newFixtureEncoder(""), newFixtureAttestor(""), nil)
		captureStandard(t, recorder)
		receipt, err := recorder.Finalize(t.Context(), 500)
		if err != nil {
			t.Fatal(err)
		}
		return receipt
	}

	t.Run("original", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "attempt")
		receipt := makeCompleted(t, directory)
		identity, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := invalidatePublishedManifest(directory, identity); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(directory, manifestPath)); !os.IsNotExist(err) {
			t.Fatalf("invalidated marker still exists: %v", err)
		}
		if _, err := VerifyBundle(directory, receipt.ManifestSHA256); err == nil {
			t.Fatal("invalidated bundle unexpectedly verified")
		}
	})

	t.Run("replacement", func(t *testing.T) {
		parent := t.TempDir()
		directory := filepath.Join(parent, "attempt")
		_ = makeCompleted(t, directory)
		identity, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		moved := directory + "-moved"
		t.Cleanup(func() { _ = makeTreeOwnerWritable(moved) })
		if err := os.Rename(directory, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := []byte(`{"complete":"replacement"}`)
		if err := os.WriteFile(filepath.Join(directory, manifestPath), marker, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := invalidatePublishedManifest(directory, identity); err == nil {
			t.Fatal("invalidation accepted a replacement directory")
		}
		retained, err := os.ReadFile(filepath.Join(directory, manifestPath))
		if err != nil || !bytes.Equal(retained, marker) {
			t.Fatalf("replacement marker was touched: %q, %v", retained, err)
		}
	})
}

func mustRecorder(t testing.TB, directory string, sources []string,
	encoder *fixtureEncoder, attestor *fixtureAttestor, sensitive []string) *Recorder {
	t.Helper()
	t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
	recorder, err := New(context.Background(), Config{
		Directory: directory, RequireAudio: true, ExpectedVideoSources: sources,
		Encoder: encoder, Attestor: attestor, SensitiveValues: sensitive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func captureStandard(t testing.TB, recorder *Recorder) {
	t.Helper()
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	for index, timestamp := range []float64{0, 100} {
		if err := recorder.CaptureVideo(bench.SessionVideoCapture{
			Source: "screen", Width: 64, Height: 48, MediaType: "image/png",
			WireTimestamp: int64(1000 + index), EpisodeAtMS: timestamp,
			Data: pngFixture(t, 64, 48, color.RGBA{R: uint8(index * 100), A: 0xff}),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func testAudioCapture() bench.SessionAudioCapture {
	room := make([]int16, 2_400)
	agent := make([]int16, 1_200)
	for index := range room {
		room[index] = int16(index%200 - 100)
	}
	for index := range agent {
		agent[index] = int16(100 - index%200)
	}
	return bench.SessionAudioCapture{SampleRateHz: 24_000, RoomPCM16: room,
		Agent: []bench.TimedAudioChunk{{AtMS: 25, PCM16: agent}}}
}

func pngFixture(t testing.TB, width, height int, fill color.RGBA) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			frame.SetRGBA(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, frame); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func jpegFixture(t testing.TB, width, height int, fill color.RGBA) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			frame.SetRGBA(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, frame, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func structuralAVMP4Fixture(source string) []byte {
	fileType := append([]byte("mp42"), []byte{0, 0, 0, 0}...)
	fileType = append(fileType, []byte("mp42")...)
	movie := append(isoTrack("vide", "avc1"), isoTrack("soun", "mp4a")...)
	mediaData := append([]byte("fixture-source:"), source...)
	return bytes.Join([][]byte{isoBox("ftyp", fileType), isoBox("moov", movie), isoBox("mdat", mediaData)}, nil)
}

func isoTrack(handler, codec string) []byte {
	handlerData := make([]byte, 12)
	copy(handlerData[8:12], handler)
	stsdData := make([]byte, 8)
	binary.BigEndian.PutUint32(stsdData[4:8], 1)
	stsdData = append(stsdData, isoBox(codec, nil)...)
	media := append(isoBox("hdlr", handlerData), isoBox("minf", isoBox("stbl", isoBox("stsd", stsdData)))...)
	return isoBox("trak", isoBox("mdia", media))
}

func isoBox(kind string, data []byte) []byte {
	result := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(result[:4], uint32(len(result)))
	copy(result[4:8], kind)
	copy(result[8:], data)
	return result
}

func pathInside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func equalManifest(left, right Manifest) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}
