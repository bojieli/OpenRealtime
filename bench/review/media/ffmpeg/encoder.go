package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

// Encoder is an immutable snapshot of a sandboxed ffmpeg encoding toolchain.
// Instances have single-owner lifecycle semantics enforced by Claim.
type Encoder struct {
	state          pluginState
	chain          toolchain
	guard          sensitiveGuard
	descriptor     reviewmedia.EncoderDescriptor
	implementation []byte
	configuration  []byte
}

// NewEncoder resolves, fingerprints, and executes a bounded version check for
// the complete encoder toolchain under bubblewrap isolation.
func NewEncoder(ctx context.Context, options Options) (*Encoder, error) {
	guard, err := newSensitiveGuard(options.SensitiveValues)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Encoder, error) {
		guard.destroy()
		return nil, guard.redactError(ctx, "construct ffmpeg media encoder", err)
	}
	chain, err := buildToolchain(ctx, options, false, guard)
	if err != nil {
		return fail(err)
	}
	implementation, err := implementationSource()
	if err != nil {
		return fail(err)
	}
	configuration, err := chain.configuration("encoder", json.RawMessage(fixedEncodingIdentity))
	if err != nil {
		return fail(err)
	}
	if guard.rejects(implementation) || guard.rejects(configuration) {
		return fail(errors.New("ffmpeg encoder provenance contains a declared sensitive value"))
	}
	descriptor := reviewmedia.EncoderDescriptor{
		Name: "ffmpeg-sandboxed", Version: chain.ffmpegVersion,
		Implementation: review.ContentIdentity{
			Version: "openrealtime.ffmpeg-review.impl.v4", SHA256: digest(implementation),
		},
		ConfigurationSHA256: digest(configuration),
	}
	if err := descriptor.Validate(); err != nil {
		return fail(err)
	}
	return &Encoder{
		chain: chain, guard: guard, descriptor: descriptor,
		implementation: implementation, configuration: configuration,
	}, nil
}

func (encoder *Encoder) Descriptor() reviewmedia.EncoderDescriptor {
	if encoder == nil {
		return reviewmedia.EncoderDescriptor{}
	}
	encoder.state.mu.RLock()
	defer encoder.state.mu.RUnlock()
	if encoder.state.closed {
		return reviewmedia.EncoderDescriptor{}
	}
	return encoder.descriptor
}

func (encoder *Encoder) Implementation() []byte {
	if encoder == nil {
		return nil
	}
	encoder.state.mu.RLock()
	defer encoder.state.mu.RUnlock()
	return cloneBytes(encoder.implementation)
}

func (encoder *Encoder) Configuration() []byte {
	if encoder == nil {
		return nil
	}
	encoder.state.mu.RLock()
	defer encoder.state.mu.RUnlock()
	return cloneBytes(encoder.configuration)
}

func (encoder *Encoder) Claim(ctx context.Context) error {
	if encoder == nil {
		return errors.New("claim ffmpeg media encoder: nil plug-in")
	}
	return encoder.state.claim(ctx, "ffmpeg media encoder")
}

func (encoder *Encoder) Encode(ctx context.Context, request reviewmedia.EncodeRequest) (returnErr error) {
	if encoder == nil {
		return errors.New("ffmpeg media encoder is nil")
	}
	defer func() {
		returnErr = encoder.guard.redactError(ctx, "ffmpeg media encode", returnErr)
	}()
	release, err := encoder.state.begin(ctx, "ffmpeg media encoder")
	if err != nil {
		return err
	}
	defer release()
	if err := validateEncodeRequest(request); err != nil {
		return err
	}
	if err := encoder.chain.recheck(ctx); err != nil {
		return err
	}
	snapshot, err := snapshotEncodeInputs(ctx, request)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := snapshot.close(); returnErr == nil && cleanupErr != nil {
			returnErr = errors.New("clean private ffmpeg encode snapshot")
		}
	}()
	outputName := request.Source + ".review.mp4"
	privateOutput := filepath.Join(snapshot.outputRoot, outputName)
	privateEncodedName := request.Source + ".encoded.mp4"
	privateEncoded := filepath.Join(snapshot.outputRoot, privateEncodedName)
	privateVideoName := request.Source + ".video.mp4"
	privateVideo := filepath.Join(snapshot.outputRoot, privateVideoName)
	attemptDuration := formatSecondsUS(request.AttemptEndUS)
	audioFilter := "apad,atrim=end=" + attemptDuration
	// FFmpeg 4.4's classic MP4 muxer uses a fixed 1 kHz movie clock. Reserve a
	// leading edit-list entry with a private 1 ms placeholder when the real
	// positive offset is sub-millisecond; patchClassicMP4Timeline replaces it
	// with the exact microsecond value before publication and attestation.
	muxVideoOffsetUS := request.FirstFrameUS
	if muxVideoOffsetUS > 0 && muxVideoOffsetUS < 1_000 {
		muxVideoOffsetUS = 1_000
	}
	videoArguments := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-n",
		"-i", "/work/input/timeline.mkv", "-map", "0:v:0",
		"-vf", "settb=expr=1/1000000,setpts=PTS-STARTPTS",
		"-vsync", "0", "-time_base:v", "1:1000000", "-enc_time_base:v", "1:1000000",
		"-c:v", "libx264", "-preset", "medium", "-crf", "18", "-bf", "0", "-g", "1",
		"-pix_fmt", "yuv420p",
		"-video_track_timescale", "1000000",
		"-an", "-map_metadata", "-1", "-map_chapters", "-1", "-metadata", "encoder=",
		"-movflags", "+faststart", "-avoid_negative_ts", "disabled",
		"/work/output/" + privateEncodedName,
	}
	diagnostic, err := encoder.chain.run(ctx, encoder.chain.ffmpeg,
		&sandboxMounts{inputRoot: snapshot.root, outputRoot: snapshot.outputRoot},
		videoArguments, maximumDiagnosticBytes)
	if err != nil {
		return encoder.guard.safeError(ctx, "ffmpeg exact video encode", err, diagnostic)
	}
	if _, _, err := fileDigestContext(ctx, privateEncoded, maximumOutputBytes); err != nil {
		return errors.New("ffmpeg did not produce a bounded private video stream")
	}
	trimArguments := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-n", "-copyts",
		"-i", "/work/output/" + privateEncodedName, "-map", "0:v:0", "-c:v", "copy",
		"-copytb", "1", "-frames:v", strconv.Itoa(request.ExpectedFrames),
		"-video_track_timescale", "1000000", "-an", "-map_metadata", "-1", "-map_chapters", "-1",
		"-metadata", "encoder=", "-movflags", "+faststart", "-avoid_negative_ts", "disabled",
		"/work/output/" + privateVideoName,
	}
	diagnostic, err = encoder.chain.run(ctx, encoder.chain.ffmpeg,
		&sandboxMounts{inputRoot: snapshot.root, outputRoot: snapshot.outputRoot},
		trimArguments, maximumDiagnosticBytes)
	if err != nil {
		return encoder.guard.safeError(ctx, "ffmpeg exact sentinel trim", err, diagnostic)
	}
	if _, _, err := fileDigestContext(ctx, privateVideo, maximumOutputBytes); err != nil {
		return errors.New("ffmpeg did not produce a bounded trimmed video stream")
	}
	muxArguments := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-n", "-copyts",
		"-i", "/work/output/" + privateVideoName, "-i", "/work/input/audio.stereo.wav",
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-copytb", "1",
		"-bsf:v", "setts=pts=PTS+" + strconv.FormatInt(muxVideoOffsetUS, 10) +
			":dts=DTS+" + strconv.FormatInt(muxVideoOffsetUS, 10),
		"-video_track_timescale", "1000000",
		"-af", audioFilter, "-c:a", "aac", "-b:a", "192k",
		"-map_metadata", "-1", "-map_chapters", "-1", "-metadata", "encoder=",
		"-movflags", "+faststart", "-use_editlist", "1",
		"-avoid_negative_ts", "disabled", "-t", attemptDuration,
		"/work/output/" + outputName,
	}
	diagnostic, err = encoder.chain.run(ctx, encoder.chain.ffmpeg,
		&sandboxMounts{inputRoot: snapshot.root, outputRoot: snapshot.outputRoot},
		muxArguments, maximumDiagnosticBytes)
	if err != nil {
		return encoder.guard.safeError(ctx, "ffmpeg exact audio/video mux", err, diagnostic)
	}
	if err := patchClassicMP4Timeline(ctx, privateOutput, request, snapshot.lastFrameDurationUS); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	privateSHA, privateBytes, err := fileDigestContext(ctx, privateOutput, maximumOutputBytes)
	if err != nil {
		return errors.New("ffmpeg did not produce a bounded regular output")
	}
	_, copiedBytes, err := copyStableFile(ctx, privateOutput, request.OutputPath,
		maximumOutputBytes, privateSHA, 0)
	if err != nil || copiedBytes != privateBytes {
		_ = os.Remove(request.OutputPath)
		return errors.New("publish exact ffmpeg output exclusively")
	}
	finalSHA, finalBytes, err := fileDigestContext(ctx, request.OutputPath, maximumOutputBytes)
	if err != nil || finalSHA != privateSHA || finalBytes != privateBytes {
		_ = os.Remove(request.OutputPath)
		return errors.New("published ffmpeg output does not match its private snapshot")
	}
	// The recorder performs the durability sync through an O_RDWR descriptor
	// before it seals and publishes the final evidence bundle.
	if err := os.Chmod(request.OutputPath, 0o600); err != nil {
		_ = os.Remove(request.OutputPath)
		return errors.New("prepare published ffmpeg output for recorder durability sync")
	}
	if err := encoder.chain.recheck(ctx); err != nil {
		_ = os.Remove(request.OutputPath)
		return err
	}
	return nil
}

func (encoder *Encoder) Close() error {
	if encoder == nil {
		return nil
	}
	encoder.state.mu.Lock()
	if encoder.state.closed {
		encoder.state.mu.Unlock()
		return nil
	}
	encoder.state.closed = true
	clear(encoder.implementation)
	clear(encoder.configuration)
	encoder.implementation = nil
	encoder.configuration = nil
	encoder.chain = toolchain{}
	encoder.descriptor = reviewmedia.EncoderDescriptor{}
	encoder.state.mu.Unlock()
	encoder.guard.destroy()
	return nil
}

func formatSecondsUS(microseconds int64) string {
	seconds := microseconds / 1_000_000
	fraction := microseconds % 1_000_000
	return fmt.Sprintf("%d.%06d", seconds, fraction)
}
