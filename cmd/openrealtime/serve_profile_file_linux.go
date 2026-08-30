//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// secureReadServeProfileFile opens an absolute profile below a stable root
// descriptor. It refuses symlinks, magic links, hard links, blocking special
// files, and any identity or metadata change observed between the directory
// lookup, opened descriptor, read, and final lookup.
func secureReadServeProfileFile(
	ctx context.Context,
	path string,
	maximum int,
	afterOpen func() error,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("read graph launch profile: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if maximum < 1 {
		return nil, errors.New("read graph launch profile: invalid size limit")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("launch-profile path must be a clean absolute path")
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return nil, errors.New("launch-profile path must name a file")
	}

	rootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open graph launch profile root: %w", err)
	}
	defer unix.Close(rootFD)
	var rootBefore unix.Stat_t
	if err := unix.Fstat(rootFD, &rootBefore); err != nil {
		return nil, fmt.Errorf("stat graph launch profile root before open: %w", err)
	}

	parentPath := strings.TrimPrefix(filepath.Dir(path), "/")
	if parentPath == "" {
		parentPath = "."
	}
	parentFD, err := unix.Openat2(rootFD, parentPath, &unix.OpenHow{
		Flags: uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open graph launch profile parent: %w", err)
	}
	defer unix.Close(parentFD)
	var parentBefore unix.Stat_t
	if err := unix.Fstat(parentFD, &parentBefore); err != nil {
		return nil, fmt.Errorf("stat graph launch profile parent before open: %w", err)
	}

	var before unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("stat graph launch profile before open: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("graph launch profile is not a regular file")
	}
	if before.Nlink != 1 {
		return nil, errors.New("graph launch profile must have exactly one filesystem link")
	}
	if before.Size < 1 || before.Size > int64(maximum) {
		return nil, fmt.Errorf(
			"graph launch profile has %d bytes; expected 1..%d", before.Size, maximum,
		)
	}

	fd, err := unix.Openat(
		parentFD, base,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open graph launch profile: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open graph launch profile: invalid descriptor")
	}
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, fmt.Errorf("stat opened graph launch profile: %w", err)
	}
	if !sameServeProfileFile(before, opened) {
		return nil, errors.New("graph launch profile identity changed while it was opened")
	}
	if afterOpen != nil {
		if err := afterOpen(); err != nil {
			return nil, fmt.Errorf("graph launch profile after-open hook: %w", err)
		}
	}

	payload := make([]byte, 0, int(opened.Size))
	buffer := make([]byte, 32<<10)
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if len(payload)+count > maximum {
				return nil, fmt.Errorf(
					"graph launch profile grew beyond %d bytes while being read", maximum,
				)
			}
			payload = append(payload, buffer[:count]...)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read graph launch profile: %w", readErr)
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}

	var openedAfter, pathAfter, parentAfter, rootAfter unix.Stat_t
	if err := unix.Fstat(fd, &openedAfter); err != nil {
		return nil, fmt.Errorf("stat opened graph launch profile after read: %w", err)
	}
	if err := unix.Fstatat(parentFD, base, &pathAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("stat graph launch profile after read: %w", err)
	}
	if err := unix.Fstat(parentFD, &parentAfter); err != nil {
		return nil, fmt.Errorf("stat graph launch profile parent after read: %w", err)
	}
	if err := unix.Fstat(rootFD, &rootAfter); err != nil {
		return nil, fmt.Errorf("stat graph launch profile root after read: %w", err)
	}
	resolvedRootFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("reopen graph launch profile root after read: %w", err)
	}
	defer unix.Close(resolvedRootFD)
	var resolvedRoot unix.Stat_t
	if err := unix.Fstat(resolvedRootFD, &resolvedRoot); err != nil {
		return nil, fmt.Errorf("stat resolved graph launch profile root after read: %w", err)
	}
	resolvedParentFD, err := unix.Openat2(resolvedRootFD, parentPath, &unix.OpenHow{
		Flags: uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("reopen graph launch profile parent after read: %w", err)
	}
	defer unix.Close(resolvedParentFD)
	var resolvedParent unix.Stat_t
	if err := unix.Fstat(resolvedParentFD, &resolvedParent); err != nil {
		return nil, fmt.Errorf("stat resolved graph launch profile parent after read: %w", err)
	}
	if !sameServeProfileFile(opened, openedAfter) ||
		!sameServeProfileFile(opened, pathAfter) ||
		!sameServeProfileDirectory(parentBefore, parentAfter) ||
		!sameServeProfileDirectory(parentBefore, resolvedParent) ||
		!sameServeProfileDirectory(rootBefore, rootAfter) ||
		!sameServeProfileDirectory(rootBefore, resolvedRoot) ||
		int64(len(payload)) != openedAfter.Size {
		return nil, errors.New("graph launch profile changed while it was read")
	}
	return payload, nil
}

func sameServeProfileFile(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino &&
		left.Mode == right.Mode && left.Nlink == right.Nlink &&
		left.Uid == right.Uid && left.Gid == right.Gid &&
		left.Size == right.Size && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func sameServeProfileDirectory(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino &&
		left.Mode == right.Mode && left.Uid == right.Uid && left.Gid == right.Gid
}
