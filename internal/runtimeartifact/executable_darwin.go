//go:build darwin

package runtimeartifact

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

const (
	darwinCodeSigningStatusOperation = 0
	darwinCodeDirectoryHashOperation = 5
	darwinCodeSigningBlobOperation   = 10
	darwinCodeSigningHeaderBytes     = 8
)

// Executable identifies the vnode-backed code-signing material the Darwin
// kernel accepted for this process. It never reopens os.Executable: that path
// can name replacement bytes after launch, whereas csops reads the active
// process text vnode and architecture-specific CodeDirectory.
func Executable(id string) (inspect.ArtifactIdentity, error) {
	statusBefore, err := darwinCodeSigningStatus()
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	cdhashBefore, err := darwinCodeDirectoryHash()
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	blob, err := darwinCodeSigningBlob()
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	cdhashAfter, err := darwinCodeDirectoryHash()
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	statusAfter, err := darwinCodeSigningStatus()
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	return signedDarwinExecutableIdentity(
		id, statusBefore, statusAfter, cdhashBefore, cdhashAfter, blob,
	)
}

func darwinCodeSigningStatus() (uint32, error) {
	var encoded [4]byte
	if errno := darwinCSOps(darwinCodeSigningStatusOperation, encoded[:]); errno != 0 {
		return 0, fmt.Errorf("read running executable code-signing status: %w", errno)
	}
	// Every Darwin architecture supported by this module is little-endian.
	return binary.LittleEndian.Uint32(encoded[:]), nil
}

func darwinCodeDirectoryHash() ([]byte, error) {
	value := make([]byte, darwinCodeDirectoryHashBytes)
	if errno := darwinCSOps(darwinCodeDirectoryHashOperation, value); errno != 0 {
		return nil, fmt.Errorf("read running executable CodeDirectory hash: %w", errno)
	}
	return value, nil
}

func darwinCodeSigningBlob() ([]byte, error) {
	header := make([]byte, darwinCodeSigningHeaderBytes)
	errno := darwinCSOps(darwinCodeSigningBlobOperation, header)
	if errno == 0 {
		return nil, fmt.Errorf("running executable has no retained code-signing blob")
	}
	if errno != syscall.ERANGE {
		return nil, fmt.Errorf("size running executable code-signing blob: %w", errno)
	}
	size := binary.BigEndian.Uint32(header[4:8])
	if size < 12 || size > maximumDarwinCodeSignatureBytes {
		return nil, fmt.Errorf("kernel reported an invalid code-signature size %d", size)
	}
	blob := make([]byte, int(size))
	if errno := darwinCSOps(darwinCodeSigningBlobOperation, blob); errno != 0 {
		return nil, fmt.Errorf("read running executable code-signing blob: %w", errno)
	}
	return blob, nil
}

func darwinCSOps(operation uintptr, destination []byte) syscall.Errno {
	if libcCSOpsTrampolineAddress == 0 {
		return syscall.ENOSYS
	}
	var address uintptr
	if len(destination) != 0 {
		address = uintptr(unsafe.Pointer(&destination[0]))
	}
	_, _, errno := runtimeArtifactSyscall6(
		libcCSOpsTrampolineAddress,
		uintptr(os.Getpid()), operation, address, uintptr(len(destination)), 0, 0,
	)
	runtime.KeepAlive(destination)
	return errno
}

var libcCSOpsTrampolineAddress uintptr

//go:cgo_import_dynamic libc_csops csops "/usr/lib/libSystem.B.dylib"

// runtimeArtifactSyscall6 is the same stable libSystem bridge used by
// golang.org/x/sys/unix on Darwin. It works in CGO_ENABLED=0 release builds.
func runtimeArtifactSyscall6(
	function, argument1, argument2, argument3, argument4, argument5, argument6 uintptr,
) (result1, result2 uintptr, errno syscall.Errno)

//go:linkname runtimeArtifactSyscall6 syscall.syscall6
