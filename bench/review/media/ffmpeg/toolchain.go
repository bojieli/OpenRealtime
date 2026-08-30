package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const maximumToolFileBytes = int64(1 << 30)

type fileSnapshot struct {
	Path         string `json:"path"`
	ResolvedPath string `json:"resolved_path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	Mode         uint32 `json:"mode"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
}

type toolchain struct {
	ffmpeg         string
	ffprobe        string
	bubblewrap     string
	files          []fileSnapshot
	ffmpegVersion  string
	ffprobeVersion string
}

type toolchainConfiguration struct {
	Schema         string          `json:"schema"`
	Role           string          `json:"role"`
	FFmpeg         string          `json:"ffmpeg"`
	FFprobe        string          `json:"ffprobe,omitempty"`
	Bubblewrap     string          `json:"bubblewrap"`
	FFmpegVersion  string          `json:"ffmpeg_version"`
	FFprobeVersion string          `json:"ffprobe_version,omitempty"`
	Files          []fileSnapshot  `json:"executable_and_shared_library_closure"`
	Fixed          json.RawMessage `json:"fixed_contract"`
}

func buildToolchain(ctx context.Context, options Options, needProbe bool, guard sensitiveGuard) (toolchain, error) {
	if ctx == nil {
		return toolchain{}, errors.New("resolve ffmpeg toolchain: nil context")
	}
	if err := ctx.Err(); err != nil {
		return toolchain{}, err
	}
	ffmpegPath, err := resolveExecutable(options.FFmpegPath, "ffmpeg")
	if err != nil {
		return toolchain{}, err
	}
	bubblewrapPath, err := resolveExecutable(options.BubblewrapPath, "bwrap")
	if err != nil {
		return toolchain{}, err
	}
	ffprobePath := ""
	if needProbe {
		ffprobePath, err = resolveExecutable(options.FFprobePath, "ffprobe")
		if err != nil {
			return toolchain{}, err
		}
	}
	lddPath, err := resolveExecutable("", "ldd")
	if err != nil {
		return toolchain{}, err
	}

	dependencyTargets := []string{ffmpegPath, bubblewrapPath}
	if needProbe {
		dependencyTargets = append(dependencyTargets, ffprobePath)
	}
	paths := append(slicesClone(dependencyTargets), lddPath)
	for _, binary := range dependencyTargets {
		dependencies, dependencyErr := dependencyPaths(ctx, lddPath, binary)
		if dependencyErr != nil {
			return toolchain{}, dependencyErr
		}
		paths = append(paths, dependencies...)
	}
	paths = canonicalPaths(paths)
	files := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		snapshot, snapshotErr := snapshotTrustedFile(ctx, path)
		if snapshotErr != nil {
			return toolchain{}, snapshotErr
		}
		files = append(files, snapshot)
	}
	chain := toolchain{ffmpeg: ffmpegPath, ffprobe: ffprobePath, bubblewrap: bubblewrapPath, files: files}
	if err := chain.recheck(ctx); err != nil {
		return toolchain{}, err
	}
	ffmpegOutput, err := chain.run(ctx, ffmpegPath, nil, []string{"-version"}, maximumDiagnosticBytes)
	if err != nil {
		return toolchain{}, guard.safeError(ctx, "query ffmpeg version", err, ffmpegOutput)
	}
	chain.ffmpegVersion, err = parseVersion(ffmpegOutput, "ffmpeg")
	if err != nil || guard.rejects(ffmpegOutput) {
		return toolchain{}, errors.New("query ffmpeg version failed")
	}
	if needProbe {
		probeOutput, probeErr := chain.run(ctx, ffprobePath, nil, []string{"-version"}, maximumDiagnosticBytes)
		if probeErr != nil {
			return toolchain{}, guard.safeError(ctx, "query ffprobe version", probeErr, probeOutput)
		}
		chain.ffprobeVersion, err = parseVersion(probeOutput, "ffprobe")
		if err != nil || guard.rejects(probeOutput) {
			return toolchain{}, errors.New("query ffprobe version failed")
		}
	}
	if err := chain.recheck(ctx); err != nil {
		return toolchain{}, err
	}
	return chain, nil
}

func (chain toolchain) configuration(role string, fixed json.RawMessage) ([]byte, error) {
	configuration, err := json.Marshal(toolchainConfiguration{
		Schema: "openrealtime.ffmpeg-toolchain.v2", Role: role,
		FFmpeg: chain.ffmpeg, FFprobe: chain.ffprobe, Bubblewrap: chain.bubblewrap,
		FFmpegVersion: chain.ffmpegVersion, FFprobeVersion: chain.ffprobeVersion,
		Files: append([]fileSnapshot(nil), chain.files...), Fixed: fixed,
	})
	if err != nil {
		return nil, errors.New("encode ffmpeg toolchain configuration")
	}
	return configuration, nil
}

func (chain toolchain) recheck(ctx context.Context) error {
	if ctx == nil {
		return errors.New("verify ffmpeg toolchain: nil context")
	}
	for _, expected := range chain.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, err := snapshotTrustedFile(ctx, expected.Path)
		if err != nil || actual != expected {
			return errors.New("ffmpeg executable or shared-library closure drifted")
		}
	}
	return ctx.Err()
}

func resolveExecutable(configured, name string) (string, error) {
	if len(configured) > 4096 || !utf8.ValidString(configured) {
		return "", fmt.Errorf("%s executable path is invalid or oversized", name)
	}
	for _, character := range configured {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%s executable path contains a control character", name)
		}
	}
	path := strings.TrimSpace(configured)
	if configured != "" && path == "" {
		return "", fmt.Errorf("%s executable path is empty after trimming", name)
	}
	if path == "" {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("resolve %s executable", name)
		}
		path = resolved
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable path", name)
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s must be an executable regular non-symlink file", name)
	}
	if !pathWithin("/usr", absolute) {
		return "", fmt.Errorf("%s must be beneath the read-only /usr sandbox mount", name)
	}
	if err := validateTrustedResolvedPath(absolute); err != nil {
		return "", fmt.Errorf("%s executable is not root-owned and non-writable", name)
	}
	return absolute, nil
}

func snapshotTrustedFile(ctx context.Context, path string) (fileSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return fileSnapshot{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fileSnapshot{}, errors.New("resolve ffmpeg toolchain file")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || !pathWithin("/usr", resolved) {
		return fileSnapshot{}, errors.New("ffmpeg toolchain file escapes the read-only /usr closure")
	}
	if err := validateTrustedResolvedPath(resolved); err != nil {
		return fileSnapshot{}, errors.New("ffmpeg toolchain file is not root-owned and non-writable")
	}
	sha, size, err := fileDigestContext(ctx, resolved, maximumToolFileBytes)
	if err != nil {
		return fileSnapshot{}, errors.New("fingerprint ffmpeg toolchain file")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fileSnapshot{}, errors.New("stat ffmpeg toolchain file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileSnapshot{}, errors.New("read ffmpeg toolchain file identity")
	}
	if current, err := filepath.EvalSymlinks(absolute); err != nil || current != resolved {
		return fileSnapshot{}, errors.New("ffmpeg toolchain symlink closure drifted")
	}
	return fileSnapshot{
		Path: absolute, ResolvedPath: resolved, SHA256: sha, SizeBytes: size,
		Mode: uint32(info.Mode().Perm()), Device: uint64(stat.Dev), Inode: stat.Ino,
	}, nil
}

func validateTrustedResolvedPath(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || !pathWithin("/usr", resolved) {
		return errors.New("path is not a resolved /usr path")
	}
	current := resolved
	for {
		info, statErr := os.Stat(current)
		if statErr != nil || info.Mode()&0o022 != 0 {
			return errors.New("path has a writable component")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errors.New("path has a non-root-owned component")
		}
		if current == "/usr" {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return errors.New("path escapes /usr")
		}
		current = parent
	}
	return nil
}

func dependencyPaths(ctx context.Context, lddPath, binary string) ([]string, error) {
	command := exec.CommandContext(ctx, lddPath, binary)
	command.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	output := &boundedBuffer{maximum: 2 << 20}
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		return nil, errors.New("discover ffmpeg shared-library closure")
	}
	if output.seen > 2<<20 || bytes.Contains(output.Bytes(), []byte("not found")) {
		return nil, errors.New("ffmpeg shared-library closure is missing or oversized")
	}
	var result []string
	for _, line := range strings.Split(string(output.Bytes()), "\n") {
		fields := strings.Fields(line)
		for _, field := range fields {
			if strings.HasPrefix(field, "/") {
				result = append(result, strings.TrimSpace(field))
				break
			}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("ffmpeg shared-library closure was empty")
	}
	return canonicalPaths(result), nil
}

func canonicalPaths(paths []string) []string {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			set[path] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func slicesClone(values []string) []string { return append([]string(nil), values...) }

func parseVersion(output []byte, tool string) (string, error) {
	line, _, _ := bytes.Cut(output, []byte{'\n'})
	fields := strings.Fields(string(line))
	if len(fields) < 3 || fields[0] != tool || fields[1] != "version" {
		return "", fmt.Errorf("%s returned an unrecognized version", tool)
	}
	var value strings.Builder
	value.WriteString(tool + "-")
	for _, character := range fields[2] {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '.' ||
			character == '-' || character == '_' {
			value.WriteRune(character)
		} else {
			value.WriteByte('_')
		}
	}
	if value.Len() <= len(tool)+1 || value.Len() > 128 {
		return "", fmt.Errorf("%s returned an invalid version", tool)
	}
	return value.String(), nil
}
