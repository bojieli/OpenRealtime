//go:build darwin

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	darwinProcessListFDsFlavor = 1
	darwinProcessSocketFDType  = 2
	darwinProcessSocketFlavor  = 3
	darwinProcessFDInfoBytes   = 8
	maximumProcessFDListBytes  = 1 << 20
	stableListenerAttempts     = 4
)

// Refuse compilation if Go's 64-bit layout ever drifts from proc_info.h.
var _ [darwinSocketFDInfoBytes - int(unsafe.Sizeof(darwinSocketFDInfo{}))]byte
var _ [int(unsafe.Sizeof(darwinSocketFDInfo{})) - darwinSocketFDInfoBytes]byte

func stableTCPListeners(pid int) ([]tcpListener, error) {
	var lastErr error
	for attempt := 0; attempt < stableListenerAttempts; attempt++ {
		before, err := readTCPListeners(pid)
		if err != nil {
			lastErr = err
			continue
		}
		after, err := readTCPListeners(pid)
		if err != nil {
			lastErr = err
			continue
		}
		if reflect.DeepEqual(before, after) {
			return before, nil
		}
		lastErr = errors.New("TCP listener population changed while it was inspected")
	}
	return nil, fmt.Errorf("freeze TCP listener population: %w", lastErr)
}

func readTCPListeners(pid int) ([]tcpListener, error) {
	if pid <= 0 {
		return nil, errors.New("TCP listener PID must be positive")
	}
	needed, err := callProcPIDInfo(pid, darwinProcessListFDsFlavor, 0, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("size Darwin descriptor table: %w", err)
	}
	if needed < darwinProcessFDInfoBytes || needed > maximumProcessFDListBytes ||
		needed%darwinProcessFDInfoBytes != 0 {
		return nil, fmt.Errorf("Darwin descriptor table size %d is outside its bound", needed)
	}
	table := make([]byte, needed)
	filled, err := callProcPIDInfo(pid, darwinProcessListFDsFlavor, 0,
		unsafe.Pointer(&table[0]), len(table))
	if err != nil {
		return nil, fmt.Errorf("read Darwin descriptor table: %w", err)
	}
	runtime.KeepAlive(table)
	if filled <= 0 || filled >= len(table) || filled%darwinProcessFDInfoBytes != 0 {
		return nil, errors.New("Darwin descriptor table was empty, truncated, or malformed")
	}
	seen := make(map[int]struct{}, filled/darwinProcessFDInfoBytes)
	listeners := make([]tcpListener, 0, 4)
	for offset := 0; offset < filled; offset += darwinProcessFDInfoBytes {
		fd := int(int32(binary.LittleEndian.Uint32(table[offset:])))
		if fd < 0 {
			return nil, errors.New("Darwin descriptor table contains a negative descriptor")
		}
		if _, duplicate := seen[fd]; duplicate {
			return nil, fmt.Errorf("Darwin descriptor table repeats descriptor %d", fd)
		}
		seen[fd] = struct{}{}
		if binary.LittleEndian.Uint32(table[offset+4:]) != darwinProcessSocketFDType {
			continue
		}
		var socket darwinSocketFDInfo
		returned, socketErr := callProcPIDFDInfo(pid, fd, darwinProcessSocketFlavor,
			unsafe.Pointer(&socket), darwinSocketFDInfoBytes)
		if socketErr != nil {
			return nil, fmt.Errorf("read Darwin socket descriptor %d: %w", fd, socketErr)
		}
		runtime.KeepAlive(&socket)
		if returned != darwinSocketFDInfoBytes {
			return nil, fmt.Errorf("Darwin socket descriptor %d returned %d bytes, want %d",
				fd, returned, darwinSocketFDInfoBytes)
		}
		listener, listening, parseErr := parseDarwinTCPListener(fd, &socket)
		if parseErr != nil {
			return nil, fmt.Errorf("decode Darwin socket descriptor %d: %w", fd, parseErr)
		}
		if listening {
			listeners = append(listeners, listener)
		}
	}
	sortTCPListeners(listeners)
	return listeners, nil
}

func callProcPIDInfo(pid, flavor int, argument uint64, destination unsafe.Pointer, size int) (int, error) {
	if libcProcPIDInfoTrampolineAddress == 0 {
		return 0, syscall.ENOSYS
	}
	result, _, errno := companionProcessSyscall6(
		libcProcPIDInfoTrampolineAddress,
		uintptr(pid), uintptr(flavor), uintptr(argument), uintptr(destination), uintptr(size), 0,
	)
	runtime.KeepAlive(destination)
	if errno != 0 {
		return 0, errno
	}
	if result == 0 || result > maximumProcessFDListBytes {
		return 0, errors.New("proc_pidinfo returned no bounded process data")
	}
	return int(result), nil
}

func callProcPIDFDInfo(pid, fd, flavor int, destination unsafe.Pointer, size int) (int, error) {
	if libcProcPIDFDInfoTrampolineAddress == 0 {
		return 0, syscall.ENOSYS
	}
	result, _, errno := companionProcessSyscall6(
		libcProcPIDFDInfoTrampolineAddress,
		uintptr(pid), uintptr(fd), uintptr(flavor), uintptr(destination), uintptr(size), 0,
	)
	runtime.KeepAlive(destination)
	if errno != 0 {
		return 0, errno
	}
	if result == 0 || result > uintptr(size) {
		return 0, errors.New("proc_pidfdinfo returned no bounded socket data")
	}
	return int(result), nil
}

var libcProcPIDInfoTrampolineAddress uintptr
var libcProcPIDFDInfoTrampolineAddress uintptr

//go:cgo_import_dynamic libc_proc_pidinfo proc_pidinfo "/usr/lib/libproc.dylib"
//go:cgo_import_dynamic libc_proc_pidfdinfo proc_pidfdinfo "/usr/lib/libproc.dylib"

// companionProcessSyscall6 is the libSystem bridge used by x/sys/unix on
// Darwin. Dynamic imports keep this release-gate helper compatible with the
// repository's CGO_ENABLED=0 builds.
func companionProcessSyscall6(
	function, argument1, argument2, argument3, argument4, argument5, argument6 uintptr,
) (result1, result2 uintptr, errno syscall.Errno)

//go:linkname companionProcessSyscall6 syscall.syscall6
