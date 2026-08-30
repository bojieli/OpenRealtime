//go:build linux

package runtimeartifact

import (
	"fmt"
	"os"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"golang.org/x/sys/unix"
)

// Executable hashes the image the kernel mapped for this process. Opening
// /proc/self/exe is materially different from resolving os.Executable and
// reopening that pathname: package replacement can rename a different file
// over the launch path, while this magic link continues to name the mapped
// inode.
func Executable(id string) (inspect.ArtifactIdentity, error) {
	fd, err := unix.Open("/proc/self/exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("open mapped executable image: %w", err)
	}
	file := os.NewFile(uintptr(fd), "/proc/self/exe")
	if file == nil {
		_ = unix.Close(fd)
		return inspect.ArtifactIdentity{}, fmt.Errorf("open mapped executable image: invalid descriptor")
	}
	defer file.Close()
	return openedFile(id, file, "the mapped /proc/self/exe image")
}
