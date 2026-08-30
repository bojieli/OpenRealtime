// Package ffmpeg provides independently claimable, sandboxed encoder and
// full-decode attestor plug-ins for retained benchmark review media. The
// package is benchmark tooling only; no Realtime server or client imports it.
package ffmpeg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

const (
	maximumDiagnosticBytes = 64 << 10
	maximumProbeBytes      = 64 << 20
	maximumInputBytes      = int64(16 << 30)
	maximumOutputBytes     = int64(128 << 20)
	maximumFrames          = 100_000
	maximumDimension       = 16_384
	maximumSensitiveValues = 256
	maximumSensitiveBytes  = 1 << 20
	maximumSensitiveValue  = 4 << 10
)

const fixedEncodingIdentity = `{"audio_bitrate":"192k","audio_codec":"aac","audio_filter":"apad+atrim","format":"classic_mp4","geometry":"yuv420p-pad-right-bottom-black-to-even.v1","implementation":"openrealtime.ffmpeg-review.v4","intermediate":"minimal-matroska-v_mjpeg-timescale-1000ns-quality-95","metadata":"stripped","movflags":"+faststart","movie_timescale_patch":1000000,"pixel_format":"yuv420p","preset":"medium","submillisecond_edit_placeholder_us":1000,"timing":"three-stage-relative-h264-with-private-end-sentinel-trim-packet-offset-and-validated-editlist-upgrade","use_editlist":true,"video_b_frames":0,"video_codec":"libx264","video_crf":18,"video_gop":1}`

const fixedAttestationIdentity = `{"capability":"openrealtime.full-decode-av.v1","decode":"all-declared-audio-and-video-frames","format":"mp4","geometry":"source-and-decoded-yuv420p-pad-right-bottom-black-to-even.v1","implementation":"openrealtime.ffmpeg-attestation.v4","probe":"strict-bounded-json","timing":"microsecond-frame-pts+raw-video-packets+presented-audio-samples+stream-extents"}`

//go:embed ffmpeg.go encoder.go attestor.go matroska.go mp4.go sandbox.go toolchain.go
var sourceFS embed.FS

var sourceFiles = []string{"attestor.go", "encoder.go", "ffmpeg.go", "matroska.go", "mp4.go", "sandbox.go", "toolchain.go"}

type videoGeometry struct {
	sourceWidth, sourceHeight   int
	encodedWidth, encodedHeight int
}

func yuv420pGeometry(width, height int) videoGeometry {
	return videoGeometry{
		sourceWidth: width, sourceHeight: height,
		encodedWidth: width + width%2, encodedHeight: height + height%2,
	}
}

func (geometry videoGeometry) paddingRight() int { return geometry.encodedWidth - geometry.sourceWidth }
func (geometry videoGeometry) paddingBottom() int {
	return geometry.encodedHeight - geometry.sourceHeight
}

// Options selects a root-owned, non-writable local toolchain at this explicit
// composition boundary. Empty paths resolve the named tools from PATH.
// SensitiveValues are never serialized and guard diagnostics and provenance.
type Options struct {
	FFmpegPath      string
	FFprobePath     string
	BubblewrapPath  string
	SensitiveValues []string
}

type pluginState struct {
	mu      sync.RWMutex
	claimed bool
	closed  bool
}

func (state *pluginState) claim(ctx context.Context, label string) error {
	if ctx == nil {
		return fmt.Errorf("claim %s: nil context", label)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.closed || state.claimed {
		return fmt.Errorf("%s ownership was already claimed or closed", label)
	}
	state.claimed = true
	return nil
}

func (state *pluginState) begin(ctx context.Context, label string) (func(), error) {
	if ctx == nil {
		return nil, fmt.Errorf("use %s: nil context", label)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state.mu.RLock()
	if state.closed || !state.claimed {
		state.mu.RUnlock()
		return nil, fmt.Errorf("%s is not claimed or is closed", label)
	}
	return state.mu.RUnlock, nil
}

func (state *pluginState) close() {
	state.mu.Lock()
	state.closed = true
	state.mu.Unlock()
}

type sensitiveGuard struct {
	contains func([]byte) bool
	destroy  func()
}

func newSensitiveGuard(values []string) (sensitiveGuard, error) {
	if len(values) > maximumSensitiveValues {
		return sensitiveGuard{}, errors.New("ffmpeg sensitive value count exceeds the bounded limit")
	}
	owned := make([][]byte, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	total := 0
	for _, value := range values {
		if value == "" || len(value) > maximumSensitiveValue {
			for _, secret := range owned {
				clear(secret)
			}
			return sensitiveGuard{}, errors.New("ffmpeg sensitive values violate the bounded byte contract")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		if len(value) > maximumSensitiveBytes-total {
			for _, secret := range owned {
				clear(secret)
			}
			return sensitiveGuard{}, errors.New("ffmpeg sensitive values violate the bounded byte contract")
		}
		seen[value] = struct{}{}
		total += len(value)
		owned = append(owned, []byte(value))
	}
	return sensitiveGuard{
		contains: func(payload []byte) bool {
			for _, secret := range owned {
				if len(secret) > 0 && bytes.Contains(payload, secret) {
					return true
				}
			}
			return false
		},
		destroy: func() {
			for _, secret := range owned {
				clear(secret)
			}
			owned = nil
		},
	}, nil
}

func (guard sensitiveGuard) rejects(payload []byte) bool {
	return guard.contains != nil && guard.contains(payload)
}

func (guard sensitiveGuard) safeError(ctx context.Context, label string, err error, diagnostic []byte) error {
	if ctx != nil {
		if cause := ctx.Err(); cause != nil {
			return cause
		}
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if guard.rejects([]byte(err.Error())) || guard.rejects(diagnostic) {
		return errors.New(label + " failed")
	}
	message := strings.Join(strings.Fields(string(diagnostic)), " ")
	if message == "" {
		return errors.New(label + " failed")
	}
	return fmt.Errorf("%s failed: %s", label, message)
}

func (guard sensitiveGuard) redactError(ctx context.Context, label string, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil {
		if cause := ctx.Err(); cause != nil {
			return cause
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if guard.rejects([]byte(err.Error())) {
		return errors.New(label + " failed")
	}
	return err
}

func implementationSource() ([]byte, error) {
	var result []byte
	for _, name := range sourceFiles {
		payload, err := sourceFS.ReadFile(name)
		if err != nil {
			return nil, errors.New("read embedded ffmpeg plug-in implementation")
		}
		result = append(result, []byte("file "+name+"\n")...)
		result = append(result, payload...)
		result = append(result, '\n')
	}
	return result, nil
}

func validateEncodeRequest(request reviewmedia.EncodeRequest) error {
	if !validSource(request.Source) || request.Width <= 0 || request.Width > maximumDimension ||
		request.Height <= 0 || request.Height > maximumDimension ||
		int64(request.Width)*int64(request.Height) > 64<<20 ||
		request.ExpectedFrames <= 0 || request.ExpectedFrames > maximumFrames ||
		request.FirstFrameUS < 0 || request.AttemptEndUS <= request.FirstFrameUS || request.AttemptEndUS > math.MaxUint32 ||
		!audioEndpointAligned(request.AttemptEndUS) ||
		!validDigest(request.ExpectedAudio) || !validDigest(request.ExpectedConcat) {
		return errors.New("ffmpeg encode request has invalid source, dimensions, timing, frame count, or digests")
	}
	return validateWorkspacePaths(request.WorkspaceRoot, request.Source, request.AudioPath,
		request.ConcatPath, request.OutputPath, false)
}

func validateAttestationRequest(request reviewmedia.AttestationRequest) error {
	if !validSource(request.Source) || request.Width <= 0 || request.Width > maximumDimension ||
		request.Height <= 0 || request.Height > maximumDimension ||
		int64(request.Width)*int64(request.Height) > 64<<20 ||
		request.ExpectedFrameCount <= 0 || request.ExpectedFrameCount > maximumFrames ||
		request.FirstFrameUS < 0 || request.AttemptEndUS <= request.FirstFrameUS || request.AttemptEndUS > math.MaxUint32 ||
		!audioEndpointAligned(request.AttemptEndUS) ||
		!validDigest(request.ExpectedFramePTSUSSHA256) || !validDigest(request.ExpectedOutputSHA256) ||
		request.ExpectedOutputBytes <= 0 || request.ExpectedOutputBytes > maximumOutputBytes {
		return errors.New("ffmpeg attestation request has invalid source, dimensions, timing, frame count, size, or digests")
	}
	return validateWorkspacePaths(request.WorkspaceRoot, request.Source, "", "", request.OutputPath, true)
}

func audioEndpointAligned(microseconds int64) bool {
	return microseconds >= 0 && microseconds <= math.MaxInt64/24_000 &&
		microseconds*24_000%1_000_000 == 0
}

func validSource(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, `/\\`) {
		return false
	}
	for _, character := range value {
		if !(unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func validateWorkspacePaths(rootPath, source, audioPath, concatPath, outputPath string, outputMustExist bool) error {
	root, err := filepath.Abs(rootPath)
	if err != nil || root == filepath.Dir(root) {
		return errors.New("ffmpeg workspace root is invalid")
	}
	if err := validateOwnedDirectory(root); err != nil {
		return errors.New("ffmpeg workspace root is not an isolated directory")
	}
	outputDirectory := filepath.Join(root, "output")
	if err := validateOwnedDirectory(outputDirectory); err != nil {
		return errors.New("ffmpeg workspace output is not an isolated directory")
	}
	wantOutput := filepath.Join(outputDirectory, source+".review.mp4")
	actualOutput, err := filepath.Abs(outputPath)
	if err != nil || actualOutput != wantOutput {
		return errors.New("ffmpeg output path does not match the declared workspace and source")
	}
	outputInfo, outputErr := os.Lstat(actualOutput)
	if outputMustExist {
		if outputErr != nil || outputInfo.Mode()&os.ModeSymlink != 0 || !outputInfo.Mode().IsRegular() {
			return errors.New("ffmpeg output path is not a regular non-symlink file")
		}
	} else if outputErr == nil || !os.IsNotExist(outputErr) {
		return errors.New("ffmpeg output path must not exist")
	}
	for label, path := range map[string]string{"audio": audioPath, "timeline": concatPath} {
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil || !pathWithin(root, absolute) || absolute == root || absolute == outputDirectory {
			return fmt.Errorf("ffmpeg %s path escapes the isolated workspace", label)
		}
	}
	return nil
}

func validateOwnedDirectory(path string) error {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return errors.New("path is not a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("directory contains a symlink")
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func cloneBytes(payload []byte) []byte { return slices.Clone(payload) }

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

type boundedBuffer struct {
	buffer  bytes.Buffer
	maximum int
	seen    int
}

func (buffer *boundedBuffer) Write(payload []byte) (int, error) {
	if buffer.maximum < 0 || buffer.seen > buffer.maximum || len(payload) > buffer.maximum-buffer.seen {
		buffer.seen = buffer.maximum + 1
	} else {
		buffer.seen += len(payload)
	}
	remaining := buffer.maximum - buffer.buffer.Len()
	if remaining > 0 {
		_, _ = buffer.buffer.Write(payload[:min(remaining, len(payload))])
	}
	return len(payload), nil
}

func (buffer *boundedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }

func fileDigest(path string, maximum int64) (string, int64, error) {
	file, info, err := openStableRegular(path, maximum)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	count, err := io.Copy(hasher, io.LimitReader(file, maximum+1))
	if err != nil || count != info.Size() {
		return "", 0, errors.New("file changed while hashing")
	}
	if err := verifyOpenIdentity(path, file, info); err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), count, nil
}

func openStableRegular(path string, maximum int64) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return nil, nil, errors.New("path is not a bounded regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("open bounded regular file")
	}
	opened, openErr := file.Stat()
	visible, visibleErr := os.Lstat(path)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(before, opened) ||
		!os.SameFile(opened, visible) || opened.Size() != before.Size() {
		_ = file.Close()
		return nil, nil, errors.New("file identity changed while opening")
	}
	return file, opened, nil
}

func verifyOpenIdentity(path string, file *os.File, opened os.FileInfo) error {
	after, statErr := file.Stat()
	visible, visibleErr := os.Lstat(path)
	if statErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(opened, after) ||
		!os.SameFile(after, visible) || after.Size() != opened.Size() {
		return errors.New("file identity changed while reading")
	}
	return nil
}

func roundedUS(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(math.MaxInt64)/1e6 {
		return 0, errors.New("time is not finite and nonnegative")
	}
	return int64(math.Round(value * 1e6)), nil
}

var _ reviewmedia.Encoder = (*Encoder)(nil)
var _ reviewmedia.Attestor = (*Attestor)(nil)
