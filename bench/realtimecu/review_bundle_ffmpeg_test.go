package realtimecu

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	reviewffmpeg "github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
	"github.com/bojieli/OpenRealtime/internal/testgate"
)

type realtimeCUFFmpegFactory struct {
	options reviewffmpeg.Options
}

func (factory realtimeCUFFmpegFactory) NewReviewVideo(
	ctx context.Context, _ EvidenceAttempt,
) (reviewmedia.Encoder, reviewmedia.Attestor, error) {
	encoder, err := reviewffmpeg.NewEncoder(ctx, factory.options)
	if err != nil {
		return nil, nil, err
	}
	attestor, err := reviewffmpeg.NewAttestor(ctx, factory.options)
	if err != nil {
		_ = encoder.Close()
		return nil, nil, err
	}
	return encoder, attestor, nil
}

func TestReviewBundleRealFFmpegFullDecodeExactSixteenOptInEndToEnd(t *testing.T) {
	if os.Getenv("OPENREALTIME_REALTIME_CU_FFMPEG_E2E") != "1" {
		t.Skip("set OPENREALTIME_REALTIME_CU_FFMPEG_E2E=1 to run real FFmpeg full-decode E2E")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "bwrap"} {
		if _, err := exec.LookPath(tool); err != nil {
			testgate.Missing(t, tool)
		}
	}
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: realtimeCUFFmpegFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases, err := Select(nil, nil)
	if err != nil || len(cases) != 16 {
		t.Fatalf("Select() cases=%d error=%v", len(cases), err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16,
	}
	for _, item := range cases {
		result.Tasks = append(result.Tasks, fixtureReviewAttempt(t, bundle, item, true))
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err != nil {
		t.Fatalf("exact-16 FFmpeg FinishSuite() = %v", err)
	}
	sourceReceipt, ok := bundle.SourceReceipt()
	if !ok {
		t.Fatal("real FFmpeg run has no deterministic source receipt")
	}
	manifest, err := VerifyReviewSourceReceipt(directory, sourceReceipt)
	if err != nil || !manifest.Complete || len(manifest.Attempts) != 16 || manifest.Reportable {
		t.Fatalf("VerifyReviewSourceReceipt() manifest=%+v error=%v", manifest, err)
	}
	for _, attempt := range manifest.Attempts {
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(attempt.MediaBundle.Path))
		mediaManifest, err := reviewmedia.VerifyBundle(
			mediaDirectory, attempt.MediaBundle.ManifestSHA256,
		)
		wantVideo := 1
		if strings.HasPrefix(attempt.Case, "camera-smoke-stop/") {
			wantVideo = 2
		}
		if err != nil || mediaManifest.Encoder == nil || mediaManifest.Attestor == nil ||
			mediaManifest.Attestor.Descriptor.Capability != reviewmedia.FullDecodeAttestationCapability ||
			!strings.Contains(mediaManifest.Encoder.Descriptor.Name, "ffmpeg") ||
			!strings.Contains(mediaManifest.Attestor.Descriptor.Name, "ffmpeg-full-decode") ||
			len(mediaManifest.Video) != wantVideo {
			t.Fatalf("real FFmpeg media manifest for %s=%+v error=%v", attempt.Case, mediaManifest, err)
		}
		for _, video := range mediaManifest.Video {
			if video.Playable == nil || video.AttestationReport == nil {
				t.Fatalf("real FFmpeg source %s/%s lacks playable attestation", attempt.Case, video.Source)
			}
			reportPayload, err := os.ReadFile(filepath.Join(
				mediaDirectory, filepath.FromSlash(video.AttestationReport.Path),
			))
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				FullDecodeCompleted bool  `json:"full_decode_completed"`
				AudioDecodedSamples int64 `json:"audio_decoded_samples"`
			}
			if err := json.Unmarshal(reportPayload, &report); err != nil ||
				!report.FullDecodeCompleted || report.AudioDecodedSamples <= 0 {
				t.Fatalf("FFmpeg attestation report=%s error=%v", reportPayload, err)
			}
			playable := filepath.Join(mediaDirectory, filepath.FromSlash(video.Playable.Path))
			command := exec.CommandContext(t.Context(), "ffmpeg",
				"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode",
				"-i", playable, "-map", "0:v:0", "-map", "0:a:0", "-vsync", "0", "-f", "null", "-",
			)
			if output, err := command.CombinedOutput(); err != nil || len(output) != 0 {
				t.Fatalf("independent FFmpeg full decode %s/%s error=%v output=%q",
					attempt.Case, video.Source, err, output)
			}
		}
	}
	finalReceipt, ok := bundle.Receipt()
	if !ok || finalReceipt.SourceManifestSHA256 != sourceReceipt.ManifestSHA256 {
		t.Fatalf("real FFmpeg final receipt=%+v", finalReceipt)
	}
	finalManifest, err := VerifyReviewBundleReceipt(directory, finalReceipt)
	if err != nil || !finalManifest.Complete || finalManifest.Reportable {
		t.Fatalf("real FFmpeg final manifest=%+v error=%v", finalManifest, err)
	}
}
