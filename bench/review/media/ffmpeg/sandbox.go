package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

type sandboxMounts struct {
	inputRoot  string
	outputRoot string
}

func (chain toolchain) run(
	ctx context.Context,
	binary string,
	mounts *sandboxMounts,
	arguments []string,
	maximumStdout int,
) ([]byte, error) {
	if err := chain.recheck(ctx); err != nil {
		return nil, err
	}
	bubblewrapArguments := []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
		"--setenv", "LANG", "C", "--setenv", "LC_ALL", "C",
		"--setenv", "AV_LOG_FORCE_NOCOLOR", "1", "--setenv", "PATH", "/usr/bin:/bin",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/etc/alternatives", "/etc/alternatives",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
	}
	if mounts != nil {
		if err := validateOwnedDirectory(mounts.inputRoot); err != nil {
			return nil, errors.New("sandbox input root changed before launch")
		}
		bubblewrapArguments = append(bubblewrapArguments,
			"--dir", "/work", "--ro-bind", mounts.inputRoot, "/work/input")
		if mounts.outputRoot != "" {
			if err := validateOwnedDirectory(mounts.outputRoot); err != nil {
				return nil, errors.New("sandbox output root changed before launch")
			}
			bubblewrapArguments = append(bubblewrapArguments,
				"--bind", mounts.outputRoot, "/work/output")
		}
		bubblewrapArguments = append(bubblewrapArguments, "--chdir", "/work/input")
	}
	bubblewrapArguments = append(bubblewrapArguments, "--", binary)
	bubblewrapArguments = append(bubblewrapArguments, arguments...)
	command := exec.CommandContext(ctx, chain.bubblewrap, bubblewrapArguments...)
	command.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	stdout := &boundedBuffer{maximum: maximumStdout}
	diagnostics := &boundedBuffer{maximum: maximumDiagnosticBytes}
	command.Stdout, command.Stderr = stdout, diagnostics
	runErr := command.Run()
	if cause := ctx.Err(); cause != nil {
		return cloneBytes(diagnostics.Bytes()), cause
	}
	if runErr != nil {
		return cloneBytes(diagnostics.Bytes()), errors.New("sandboxed media process failed")
	}
	if stdout.seen > maximumStdout || diagnostics.seen > maximumDiagnosticBytes {
		return cloneBytes(diagnostics.Bytes()), errors.New("sandboxed media process output exceeded its bound")
	}
	if err := chain.recheck(ctx); err != nil {
		return nil, err
	}
	return cloneBytes(stdout.Bytes()), nil
}

type encodeSnapshot struct {
	root                string
	audioPath           string
	videoPath           string
	outputRoot          string
	lastFrameDurationUS int64
}

type timelineEntry struct {
	path       string
	ptsUS      int64
	durationUS int64
}

func snapshotEncodeInputs(ctx context.Context, request reviewmedia.EncodeRequest) (encodeSnapshot, error) {
	root, err := os.MkdirTemp("", "openrealtime-ffmpeg-encode-")
	if err != nil {
		return encodeSnapshot{}, errors.New("create private ffmpeg input snapshot")
	}
	fail := func(message string) (encodeSnapshot, error) {
		_ = removePrivateTree(root)
		return encodeSnapshot{}, errors.New(message)
	}
	framesRoot := filepath.Join(root, "frames")
	sourceRoot := filepath.Join(framesRoot, request.Source)
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		return fail("create private ffmpeg frame snapshot")
	}
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		return fail("create private ffmpeg output staging directory")
	}
	audioPath := filepath.Join(root, "audio.stereo.wav")
	if _, _, err := copyStableFile(ctx, request.AudioPath, audioPath, maximumInputBytes, request.ExpectedAudio, 0); err != nil {
		return fail("snapshot exact ffmpeg audio input")
	}
	exactConcatPath := filepath.Join(root, "timeline.source.ffconcat")
	concat, _, err := copyStableFile(ctx, request.ConcatPath, exactConcatPath, maximumDiagnosticBytes,
		request.ExpectedConcat, maximumDiagnosticBytes)
	if err != nil {
		return fail("snapshot exact ffmpeg timeline input")
	}
	entries, err := parseConcat(concat, request)
	if err != nil {
		return fail("validate exact ffmpeg timeline input")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			_ = removePrivateTree(root)
			return encodeSnapshot{}, err
		}
		sourcePath := filepath.Join(request.WorkspaceRoot, filepath.FromSlash(entry.path))
		destinationPath := filepath.Join(root, filepath.FromSlash(entry.path))
		if _, _, err := copyStableFile(ctx, sourcePath, destinationPath, maximumInputBytes, "", 0); err != nil {
			return fail("snapshot stable ffmpeg frame input")
		}
	}
	videoPath := filepath.Join(root, "timeline.mkv")
	if err := writeExactMatroska(ctx, videoPath, root, entries, request.Width, request.Height); err != nil {
		return fail("derive exact microsecond video timeline")
	}
	for _, path := range []string{sourceRoot, framesRoot, root} {
		if err := os.Chmod(path, 0o500); err != nil {
			return fail("seal private ffmpeg input snapshot")
		}
	}
	return encodeSnapshot{
		root: root, audioPath: audioPath, videoPath: videoPath, outputRoot: outputRoot,
		lastFrameDurationUS: entries[len(entries)-1].durationUS,
	}, nil
}

func (snapshot encodeSnapshot) close() error { return removePrivateTree(snapshot.root) }

func snapshotAttestationInput(
	ctx context.Context, request reviewmedia.AttestationRequest,
) (root, outputPath string, err error) {
	root, err = os.MkdirTemp("", "openrealtime-ffmpeg-attest-")
	if err != nil {
		return "", "", errors.New("create private ffmpeg attestation snapshot")
	}
	fail := func(message string) (string, string, error) {
		_ = removePrivateTree(root)
		return "", "", errors.New(message)
	}
	outputPath = filepath.Join(root, "output.mp4")
	_, copiedBytes, copyErr := copyStableFile(ctx, request.OutputPath, outputPath,
		maximumOutputBytes, request.ExpectedOutputSHA256, 0)
	if copyErr != nil || copiedBytes != request.ExpectedOutputBytes {
		return fail("snapshot exact ffmpeg attestation input")
	}
	if chmodErr := os.Chmod(root, 0o500); chmodErr != nil {
		return fail("seal private ffmpeg attestation snapshot")
	}
	return root, outputPath, nil
}

func removePrivateTree(path string) error {
	if path == "" {
		return nil
	}
	_ = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else if entry.Type().IsRegular() {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
	return os.RemoveAll(path)
}

func copyStableFile(
	ctx context.Context, source, destination string, maximum int64, expectedDigest string, retainMaximum int,
) ([]byte, int64, error) {
	if ctx == nil {
		return nil, 0, errors.New("copy stable file: nil context")
	}
	file, info, err := openStableRegular(source, maximum)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return nil, 0, errors.New("create private file snapshot")
	}
	removeDestination := true
	defer func() {
		if removeDestination {
			_ = os.Remove(destination)
		}
	}()
	hasher := sha256.New()
	var buffer *boundedBuffer
	writers := []io.Writer{destinationFile, hasher}
	if retainMaximum > 0 {
		buffer = &boundedBuffer{maximum: retainMaximum}
		writers = append(writers, buffer)
	}
	writer := io.MultiWriter(writers...)
	count, copyErr := io.Copy(writer, io.LimitReader(&contextReader{ctx: ctx, reader: file}, maximum+1))
	syncErr := destinationFile.Sync()
	closeErr := destinationFile.Close()
	if cause := ctx.Err(); cause != nil {
		return nil, 0, cause
	}
	if copyErr != nil || syncErr != nil || closeErr != nil || count != info.Size() || count > maximum {
		return nil, 0, errors.New("copy private file snapshot")
	}
	if err := verifyOpenIdentity(source, file, info); err != nil {
		return nil, 0, err
	}
	actualDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if expectedDigest != "" && actualDigest != expectedDigest {
		return nil, 0, errors.New("stable file does not match its expected digest")
	}
	var payload []byte
	if buffer != nil {
		if buffer.seen != int(count) || buffer.seen > buffer.maximum {
			return nil, 0, errors.New("stable file snapshot is too large to retain in memory")
		}
		payload = cloneBytes(buffer.Bytes())
	}
	removeDestination = false
	return payload, count, nil
}

func parseConcat(payload []byte, request reviewmedia.EncodeRequest) ([]timelineEntry, error) {
	if len(payload) == 0 || payload[len(payload)-1] != '\n' || strings.ContainsRune(string(payload), '\x00') {
		return nil, errors.New("ffconcat is not canonical newline-terminated text")
	}
	lines := strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n")
	if len(lines) != 2+2*request.ExpectedFrames || lines[0] != "ffconcat version 1.0" {
		return nil, errors.New("ffconcat shape does not match the expected frame count")
	}
	entries := make([]timelineEntry, request.ExpectedFrames)
	totalDurationUS := int64(0)
	for index := 0; index < request.ExpectedFrames; index++ {
		path, err := parseConcatPath(lines[1+2*index], request.Source, index+1)
		if err != nil {
			return nil, err
		}
		durationUS, err := parseConcatDuration(lines[2+2*index])
		if err != nil {
			return nil, err
		}
		if totalDurationUS > request.AttemptEndUS-durationUS {
			return nil, errors.New("ffconcat duration overflows the attempt")
		}
		totalDurationUS += durationUS
		entries[index] = timelineEntry{
			path: path, ptsUS: request.FirstFrameUS + totalDurationUS - durationUS, durationUS: durationUS,
		}
	}
	sentinel, err := parseConcatPath(lines[len(lines)-1], request.Source, request.ExpectedFrames)
	if err != nil || sentinel != entries[len(entries)-1].path {
		return nil, errors.New("ffconcat final-frame sentinel is invalid")
	}
	if request.FirstFrameUS+totalDurationUS != request.AttemptEndUS {
		return nil, errors.New("ffconcat durations do not cover the declared attempt interval")
	}
	return entries, nil
}

func parseConcatPath(line, source string, sequence int) (string, error) {
	value, ok := strings.CutPrefix(line, "file '")
	if !ok || !strings.HasSuffix(value, "'") {
		return "", errors.New("ffconcat file entry is malformed")
	}
	value = strings.TrimSuffix(value, "'")
	prefix := "frames/" + source + "/" + fmt.Sprintf("%06d", sequence)
	if value != prefix+".png" && value != prefix+".jpg" {
		return "", errors.New("ffconcat file entry is not the expected canonical frame")
	}
	return value, nil
}

func parseConcatDuration(line string) (int64, error) {
	value, ok := strings.CutPrefix(line, "duration ")
	if !ok || len(value) < 8 || value[len(value)-7] != '.' || len(value)-7 < 1 {
		return 0, errors.New("ffconcat duration is not canonical microsecond precision")
	}
	seconds, err := strconv.ParseUint(value[:len(value)-7], 10, 63)
	if err != nil {
		return 0, errors.New("ffconcat duration seconds are invalid")
	}
	fraction, err := strconv.ParseUint(value[len(value)-6:], 10, 20)
	if err != nil || seconds > uint64(math.MaxInt64-int64(fraction))/1_000_000 {
		return 0, errors.New("ffconcat duration microseconds are invalid")
	}
	durationUS := int64(seconds)*1_000_000 + int64(fraction)
	if durationUS < 1000 {
		return 0, errors.New("ffconcat duration is below one millisecond")
	}
	return durationUS, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(payload []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(payload)
}

func fileDigestContext(ctx context.Context, path string, maximum int64) (string, int64, error) {
	if ctx == nil {
		return "", 0, errors.New("hash file: nil context")
	}
	file, info, err := openStableRegular(path, maximum)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	count, err := io.Copy(hasher, io.LimitReader(&contextReader{ctx: ctx, reader: file}, maximum+1))
	if cause := ctx.Err(); cause != nil {
		return "", 0, cause
	}
	if err != nil || count != info.Size() || count > maximum {
		return "", 0, errors.New("file changed while hashing")
	}
	if err := verifyOpenIdentity(path, file, info); err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), count, nil
}
