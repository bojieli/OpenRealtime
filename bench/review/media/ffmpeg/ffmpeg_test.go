package ffmpeg_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	"github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
)

func TestRealFFmpegProducesAttestedSynchronizedReviewBundle(t *testing.T) {
	tools := requireTools(t)
	encoder, err := ffmpeg.NewEncoder(t.Context(), tools)
	if err != nil {
		t.Fatal(err)
	}
	attestor, err := ffmpeg.NewAttestor(t.Context(), tools)
	if err != nil {
		_ = encoder.Close()
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { makeTestTreeOwnerWritable(directory) })
	observedEncoder := &recordingEncoder{Encoder: encoder}
	observedAttestor := &recordingAttestor{Attestor: attestor}
	recorder, err := reviewmedia.New(t.Context(), reviewmedia.Config{
		Directory: directory, RequireAudio: true,
		ExpectedVideoSources: []string{"screen"}, Encoder: observedEncoder, Attestor: observedAttestor,
	})
	if err != nil {
		t.Fatal(err)
	}
	times := []float64{125.123, 425.456, 750.789}
	colors := []color.RGBA{{R: 0xff, A: 0xff}, {G: 0xff, A: 0xff}, {B: 0xff, A: 0xff}}
	for index := range times {
		if err := recorder.CaptureVideo(bench.SessionVideoCapture{
			Source: "screen", Width: 64, Height: 48, MediaType: "image/png",
			WireTimestamp: int64(1_000 + index), EpisodeAtMS: times[index],
			Data: pngFrame(t, 64, 48, colors[index]),
		}); err != nil {
			t.Fatal(err)
		}
	}
	room := sinePCM(30_000, 24_000, 440, 8_000)
	agent := sinePCM(9_600, 24_000, 880, 10_000)
	if err := recorder.CaptureAudio(bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: room,
		Agent: []bench.TimedAudioChunk{{AtMS: 300, PCM16: agent}},
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 1250.000)
	if err != nil {
		t.Fatalf("Finalize() = %v; encoder=%v; attestor=%v", err, observedEncoder.lastError(), observedAttestor.lastError())
	}
	manifest := receipt.Manifest
	if !manifest.Complete || manifest.AttemptEndUS != 1_250_000 ||
		manifest.Encoder == nil || manifest.Encoder.Descriptor.Name != "ffmpeg-sandboxed" ||
		manifest.Attestor == nil || manifest.Attestor.Descriptor.Capability != reviewmedia.FullDecodeAttestationCapability ||
		manifest.Audio == nil || len(manifest.Video) != 1 || manifest.Video[0].Playable == nil ||
		manifest.Video[0].PlayableSpec == nil || manifest.Video[0].AttestationReport == nil {
		t.Fatalf("manifest is incomplete: %+v", manifest)
	}
	spec := manifest.Video[0].PlayableSpec
	if spec.OutputSHA256 != manifest.Video[0].Playable.SHA256 || spec.VideoCodec != "h264" ||
		spec.Width != 64 || spec.Height != 48 || spec.EncodedWidth != 64 || spec.EncodedHeight != 48 ||
		spec.GeometryPolicy != reviewmedia.YUV420PPadRightBottomBlackToEvenV1 ||
		spec.AudioCodec != "aac" || spec.VideoStartUS != 125_123 || spec.VideoEndUS != 1_250_000 ||
		spec.AudioStartUS != 0 || spec.AudioEndUS != 1_250_000 || spec.VideoFrameCount != 3 {
		t.Fatalf("playable spec = %+v", *spec)
	}
	verified, err := reviewmedia.VerifyBundle(directory, receipt.ManifestSHA256)
	if err != nil || !verified.Complete || verified.Video[0].Playable.SHA256 != spec.OutputSHA256 {
		t.Fatalf("VerifyBundle() = %+v, %v", verified, err)
	}
	prepared, err := review.Prepare(review.Request{
		AttemptID: "integration/ffmpeg/1", Suite: "integration", Case: "ffmpeg", Trial: 1,
		RootDirectory: directory, Context: json.RawMessage(`{"deterministic_pass":true}`),
		Media: manifest.ReviewMedia(),
	})
	if err != nil || len(prepared.Media) != 2 {
		t.Fatalf("review.Prepare() media=%d error=%v", len(prepared.Media), err)
	}
	for _, item := range prepared.Media {
		if item.Validation != review.MediaValidationVersion {
			t.Fatalf("media %q validation = %q", item.Path, item.Validation)
		}
	}
	playable := filepath.Join(directory, filepath.FromSlash(manifest.Video[0].Playable.Path))
	assertDecodedVideoColors(t, tools.FFmpegPath, playable, colors)
	assertDecodedStereoAlignment(t, tools.FFmpegPath, playable, "1.250000")
	if info, err := os.Stat(playable); err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("sealed playable output stat = %+v, %v", info, err)
	}
}

func TestRealFFmpegPadsOddSourceGeometryWithoutChangingRawEvidence(t *testing.T) {
	tools := requireTools(t)
	encoder, err := ffmpeg.NewEncoder(t.Context(), tools)
	if err != nil {
		t.Fatal(err)
	}
	attestor, err := ffmpeg.NewAttestor(t.Context(), tools)
	if err != nil {
		_ = encoder.Close()
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { makeTestTreeOwnerWritable(directory) })
	recorder, err := reviewmedia.New(t.Context(), reviewmedia.Config{
		Directory: directory, RequireAudio: true,
		ExpectedVideoSources: []string{"screen"}, Encoder: encoder, Attestor: attestor,
	})
	if err != nil {
		t.Fatal(err)
	}
	const sourceWidth, sourceHeight = 1280, 577
	rawFrame := pngFrame(t, sourceWidth, sourceHeight, color.RGBA{R: 0xff, A: 0xff})
	if err := recorder.CaptureVideo(bench.SessionVideoCapture{
		Source: "screen", Width: sourceWidth, Height: sourceHeight, MediaType: "image/png",
		WireTimestamp: 1, EpisodeAtMS: 100, Data: rawFrame,
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.CaptureAudio(bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: sinePCM(240, 24_000, 440, 8_000),
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Manifest.Video) != 1 || len(receipt.Manifest.Video[0].Frames) != 1 ||
		receipt.Manifest.Video[0].PlayableSpec == nil {
		t.Fatalf("incomplete odd-geometry manifest: %+v", receipt.Manifest)
	}
	video := receipt.Manifest.Video[0]
	spec := video.PlayableSpec
	if video.Width != sourceWidth || video.Height != sourceHeight ||
		spec.Width != sourceWidth || spec.Height != sourceHeight ||
		spec.EncodedWidth != 1280 || spec.EncodedHeight != 578 ||
		spec.GeometryPolicy != reviewmedia.YUV420PPadRightBottomBlackToEvenV1 {
		t.Fatalf("odd-geometry playable spec = %+v; source=%dx%d", *spec, video.Width, video.Height)
	}
	rawSHA := fmt.Sprintf("sha256:%x", sha256.Sum256(rawFrame))
	retainedRaw, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(video.Frames[0].Path)))
	if err != nil || video.Frames[0].SHA256 != rawSHA ||
		video.Frames[0].SizeBytes != int64(len(rawFrame)) || !bytes.Equal(retainedRaw, rawFrame) {
		t.Fatalf("raw evidence changed: sha=%q size=%d read_error=%v equal=%t",
			video.Frames[0].SHA256, video.Frames[0].SizeBytes, err, bytes.Equal(retainedRaw, rawFrame))
	}
	var report struct {
		Schema          string                      `json:"schema"`
		SourceGeometry  struct{ Width, Height int } `json:"source_geometry"`
		EncodedGeometry struct{ Width, Height int } `json:"encoded_geometry"`
		GeometryPolicy  string                      `json:"geometry_policy"`
		PaddingRight    int                         `json:"padding_right_pixels"`
		PaddingBottom   int                         `json:"padding_bottom_pixels"`
		FullDecode      bool                        `json:"full_decode_completed"`
	}
	reportPayload, err := os.ReadFile(filepath.Join(directory, video.AttestationReport.Path))
	if err != nil || json.Unmarshal(reportPayload, &report) != nil ||
		report.Schema != "openrealtime.ffmpeg-full-decode-attestation.v4" ||
		report.SourceGeometry.Width != sourceWidth || report.SourceGeometry.Height != sourceHeight ||
		report.EncodedGeometry.Width != 1280 || report.EncodedGeometry.Height != 578 ||
		report.GeometryPolicy != reviewmedia.YUV420PPadRightBottomBlackToEvenV1 ||
		report.PaddingRight != 0 || report.PaddingBottom != 1 || !report.FullDecode {
		t.Fatalf("odd-geometry attestation = %+v, read_error=%v", report, err)
	}
	if _, err := reviewmedia.VerifyBundle(directory, receipt.ManifestSHA256); err != nil {
		t.Fatalf("VerifyBundle() = %v", err)
	}
	playable := filepath.Join(directory, filepath.FromSlash(video.Playable.Path))
	width, height := probeVideoGeometry(t, tools.FFprobePath, playable)
	if width != 1280 || height != 578 {
		t.Fatalf("decoded geometry = %dx%d, want 1280x578", width, height)
	}
	if got := decodedPresentedAudioFrames(t, tools.FFmpegPath, playable, 101_000); got != 2_424 {
		t.Fatalf("presented decoded audio frames = %d, want 2424", got)
	}
}

type recordingEncoder struct {
	*ffmpeg.Encoder
	mu    sync.Mutex
	err   error
	calls int
}

func (encoder *recordingEncoder) Encode(ctx context.Context, request reviewmedia.EncodeRequest) error {
	err := encoder.Encoder.Encode(ctx, request)
	encoder.mu.Lock()
	encoder.err = err
	encoder.calls++
	encoder.mu.Unlock()
	return err
}

func (encoder *recordingEncoder) callCount() int {
	encoder.mu.Lock()
	defer encoder.mu.Unlock()
	return encoder.calls
}

func (encoder *recordingEncoder) lastError() error {
	encoder.mu.Lock()
	defer encoder.mu.Unlock()
	return encoder.err
}

type recordingAttestor struct {
	*ffmpeg.Attestor
	mu    sync.Mutex
	err   error
	calls int
}

func (attestor *recordingAttestor) Attest(
	ctx context.Context, request reviewmedia.AttestationRequest,
) (reviewmedia.Attestation, error) {
	result, err := attestor.Attestor.Attest(ctx, request)
	attestor.mu.Lock()
	attestor.err = err
	attestor.calls++
	attestor.mu.Unlock()
	return result, err
}

func (attestor *recordingAttestor) callCount() int {
	attestor.mu.Lock()
	defer attestor.mu.Unlock()
	return attestor.calls
}

func (attestor *recordingAttestor) lastError() error {
	attestor.mu.Lock()
	defer attestor.mu.Unlock()
	return attestor.err
}

func TestPluginLifecycleCancellationAndProvenance(t *testing.T) {
	tools := requireTools(t)
	secret := "declared-secret-that-must-never-appear"
	tools.SensitiveValues = []string{secret}
	encoder, err := ffmpeg.NewEncoder(t.Context(), tools)
	if err != nil {
		t.Fatal(err)
	}
	configuration := encoder.Configuration()
	if bytes.Contains(configuration, []byte(secret)) ||
		!bytes.Contains(configuration, []byte("executable_and_shared_library_closure")) ||
		!bytes.Contains(configuration, []byte("bwrap")) {
		t.Fatalf("encoder provenance is missing its closure or retained a secret: %s", configuration)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := encoder.Claim(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Claim(cancelled) = %v", err)
	}
	if err := encoder.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Claim(t.Context()); err == nil {
		t.Fatal("second encoder claim unexpectedly succeeded")
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if got := encoder.Configuration(); len(got) != 0 {
		t.Fatalf("closed encoder retained configuration: %d bytes", len(got))
	}
}

func TestRealFFmpegPreservesZeroAndSubmillisecondPresentationOffsets(t *testing.T) {
	tools := requireTools(t)
	tests := []struct {
		name         string
		timesMS      []float64
		attemptEndMS float64
		firstUS      int64
		endUS        int64
		wantReject   bool
	}{
		{"zero", []float64{0, 333.333, 666.666}, 1000, 0, 1_000_000, false},
		{"123us aligned nonmillisecond end", []float64{0.123, 400.456, 700.789}, 1000.125, 123, 1_000_125, false},
		{"999us", []float64{0.999, 400, 700}, 1000, 999, 1_000_000, false},
		{"nonrepresentable audio end", []float64{0.123, 400.456, 700.789}, 1000.123, 123, 1_000_123, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoder, err := ffmpeg.NewEncoder(t.Context(), tools)
			if err != nil {
				t.Fatal(err)
			}
			attestor, err := ffmpeg.NewAttestor(t.Context(), tools)
			if err != nil {
				_ = encoder.Close()
				t.Fatal(err)
			}
			observedEncoder := &recordingEncoder{Encoder: encoder}
			observedAttestor := &recordingAttestor{Attestor: attestor}
			directory := filepath.Join(t.TempDir(), "attempt")
			t.Cleanup(func() { makeTestTreeOwnerWritable(directory) })
			recorder, err := reviewmedia.New(t.Context(), reviewmedia.Config{
				Directory: directory, RequireAudio: true, ExpectedVideoSources: []string{"screen"},
				Encoder: observedEncoder, Attestor: observedAttestor,
			})
			if err != nil {
				t.Fatal(err)
			}
			for index, atMS := range test.timesMS {
				if err := recorder.CaptureVideo(bench.SessionVideoCapture{
					Source: "screen", Width: 32, Height: 24, MediaType: "image/png",
					WireTimestamp: int64(index + 1), EpisodeAtMS: atMS,
					Data: pngFrame(t, 32, 24, color.RGBA{R: uint8(40 + index*70), G: 80, B: 160, A: 255}),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := recorder.CaptureAudio(bench.SessionAudioCapture{
				SampleRateHz: 24_000, RoomPCM16: sinePCM(24_000, 24_000, 440, 8_000),
			}); err != nil {
				t.Fatal(err)
			}
			receipt, err := recorder.Finalize(t.Context(), test.attemptEndMS)
			if test.wantReject {
				if err == nil || observedEncoder.callCount() != 0 || observedAttestor.callCount() != 0 {
					t.Fatalf("nonrepresentable 24 kHz endpoint unexpectedly passed: receipt=%+v error=%v", receipt, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Finalize() = %v; encoder=%v; attestor=%v",
					err, observedEncoder.lastError(), observedAttestor.lastError())
			}
			spec := receipt.Manifest.Video[0].PlayableSpec
			if spec == nil || spec.VideoStartUS != test.firstUS || spec.VideoEndUS != test.endUS ||
				spec.AudioStartUS != 0 || spec.AudioEndUS != test.endUS || spec.VideoFrameCount != 3 {
				t.Fatalf("playable spec = %+v", spec)
			}
			var report struct {
				Schema                  string `json:"schema"`
				AudioDecodedSamples     int64  `json:"audio_decoded_samples"`
				AudioPresentedSamples   int64  `json:"audio_presented_samples"`
				AudioTailPaddingSamples int64  `json:"audio_tail_padding_samples"`
			}
			reportPath := filepath.Join(directory, receipt.Manifest.Video[0].AttestationReport.Path)
			payload, err := os.ReadFile(reportPath)
			if err != nil || json.Unmarshal(payload, &report) != nil {
				t.Fatalf("read attestation report: %v", err)
			}
			wantPresented := (test.endUS*24_000 + 999_999) / 1_000_000
			if report.Schema != "openrealtime.ffmpeg-full-decode-attestation.v4" ||
				report.AudioPresentedSamples != wantPresented ||
				report.AudioDecodedSamples-report.AudioPresentedSamples != report.AudioTailPaddingSamples {
				t.Fatalf("audio attestation = %+v", report)
			}
			playable := filepath.Join(directory, receipt.Manifest.Video[0].Playable.Path)
			if got := decodedPresentedAudioFrames(t, tools.FFmpegPath, playable, test.endUS); got != wantPresented {
				t.Fatalf("presented decoded audio frames = %d, want %d", got, wantPresented)
			}
		})
	}
}

func requireTools(t *testing.T) ffmpeg.Options {
	t.Helper()
	paths := make(map[string]string)
	for _, name := range []string{"ffmpeg", "ffprobe", "bwrap"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not installed", name)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		paths[name] = absolute
	}
	return ffmpeg.Options{
		FFmpegPath: paths["ffmpeg"], FFprobePath: paths["ffprobe"], BubblewrapPath: paths["bwrap"],
	}
}

func probeVideoGeometry(t *testing.T, binaryPath, mediaPath string) (int, int) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binaryPath, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "json", mediaPath)
	command.Env = []string{"LANG=C", "LC_ALL=C", "AV_LOG_FORCE_NOCOLOR=1"}
	payload, err := command.Output()
	if err != nil {
		t.Fatalf("probe video geometry: %v", err)
	}
	var envelope struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Streams) != 1 {
		t.Fatalf("decode video geometry probe: payload=%q error=%v", payload, err)
	}
	return envelope.Streams[0].Width, envelope.Streams[0].Height
}

func assertDecodedVideoColors(t *testing.T, binaryPath, mediaPath string, expected []color.RGBA) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binaryPath,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-i", mediaPath,
		"-map", "0:v:0", "-vsync", "0", "-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	command.Env = []string{"LANG=C", "LC_ALL=C", "AV_LOG_FORCE_NOCOLOR=1"}
	payload, err := command.Output()
	if err != nil {
		t.Fatalf("decode video frames: %v", err)
	}
	const frameBytes = 64 * 48 * 3
	if len(payload) != len(expected)*frameBytes {
		t.Fatalf("decoded video bytes = %d, want %d", len(payload), len(expected)*frameBytes)
	}
	for index, want := range expected {
		frame := payload[index*frameBytes : (index+1)*frameBytes]
		var totals [3]int64
		for offset := 0; offset < len(frame); offset += 3 {
			for channel := range totals {
				totals[channel] += int64(frame[offset+channel])
			}
		}
		dominant := 0
		if want.G > want.R && want.G > want.B {
			dominant = 1
		} else if want.B > want.R && want.B > want.G {
			dominant = 2
		}
		for channel := range totals {
			if channel != dominant && totals[dominant] < totals[channel]*4 {
				t.Fatalf("frame %d decoded color totals = %v", index, totals)
			}
		}
	}
}

func assertDecodedStereoAlignment(t *testing.T, binaryPath, mediaPath, presentationDuration string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binaryPath,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-i", mediaPath,
		"-map", "0:a:0", "-t", presentationDuration,
		"-ac", "2", "-ar", "24000", "-c:a", "pcm_s16le", "-f", "s16le", "-")
	command.Env = []string{"LANG=C", "LC_ALL=C", "AV_LOG_FORCE_NOCOLOR=1"}
	payload, err := command.Output()
	if err != nil {
		t.Fatalf("decode stereo audio: %v", err)
	}
	if len(payload) != 30_000*4 {
		t.Fatalf("decoded stereo bytes = %d, want %d", len(payload), 30_000*4)
	}
	leftEarly, rightEarly := channelRMS(payload, 0, 4_800)
	leftActive, rightActive := channelRMS(payload, 8_400, 14_400)
	leftTail, rightTail := channelRMS(payload, 22_000, 28_000)
	if leftEarly < 2_000 || leftActive < 2_000 || leftTail < 2_000 ||
		rightEarly > 500 || rightActive < 2_000 || rightTail > 500 {
		t.Fatalf("decoded channel RMS early=(%.1f,%.1f) active=(%.1f,%.1f) tail=(%.1f,%.1f)",
			leftEarly, rightEarly, leftActive, rightActive, leftTail, rightTail)
	}
}

func decodedPresentedAudioFrames(t *testing.T, binaryPath, mediaPath string, durationUS int64) int64 {
	t.Helper()
	command := exec.CommandContext(t.Context(), binaryPath,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-i", mediaPath,
		"-map", "0:a:0", "-t", fmt.Sprintf("%d.%06d", durationUS/1_000_000, durationUS%1_000_000),
		"-ac", "2", "-ar", "24000", "-c:a", "pcm_s16le", "-f", "s16le", "-")
	command.Env = []string{"LANG=C", "LC_ALL=C", "AV_LOG_FORCE_NOCOLOR=1"}
	payload, err := command.Output()
	if err != nil || len(payload)%4 != 0 {
		t.Fatalf("decode presented audio: bytes=%d error=%v", len(payload), err)
	}
	return int64(len(payload) / 4)
}

func makeTestTreeOwnerWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else if entry.Type().IsRegular() {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}

func channelRMS(payload []byte, startFrame, endFrame int) (float64, float64) {
	var left, right float64
	for frame := startFrame; frame < endFrame; frame++ {
		offset := frame * 4
		leftSample := int16(binary.LittleEndian.Uint16(payload[offset : offset+2]))
		rightSample := int16(binary.LittleEndian.Uint16(payload[offset+2 : offset+4]))
		left += float64(leftSample) * float64(leftSample)
		right += float64(rightSample) * float64(rightSample)
	}
	count := float64(endFrame - startFrame)
	return math.Sqrt(left / count), math.Sqrt(right / count)
}

func sinePCM(frames, sampleRate int, frequency, amplitude float64) []int16 {
	result := make([]int16, frames)
	for index := range result {
		result[index] = int16(math.Round(amplitude * math.Sin(2*math.Pi*frequency*float64(index)/float64(sampleRate))))
	}
	return result
}

func pngFrame(t *testing.T, width, height int, fill color.RGBA) []byte {
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
