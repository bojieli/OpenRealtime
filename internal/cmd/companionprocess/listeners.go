package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
)

// These layouts are the stable 64-bit Darwin proc_info.h ABI used by both
// supported macOS architectures. The live syscall bridge is Darwin-only, but
// keeping the decoder platform-independent lets ordinary unit tests exercise
// every refusal without pretending Linux process data is Darwin evidence.
const (
	darwinAFInet                 = 2
	darwinAFInet6                = 30
	darwinSockStream             = 1
	darwinIPProtocolTCP          = 6
	darwinSocketInfoTCP          = 2
	darwinTCPStateListen         = 1
	darwinInternetFlagIPv4       = 1
	darwinInternetFlagIPv6       = 2
	darwinSocketProtocolBytes    = 528
	darwinSocketFDInfoBytes      = 792
	darwinTCPForeignPortOffset   = 0
	darwinTCPLocalPortOffset     = 4
	darwinTCPInternetFlagsOffset = 24
	darwinTCPForeignAddrOffset   = 32
	darwinTCPLocalAddrOffset     = 48
	darwinTCPStateOffset         = 80
)

type darwinVInfoStat struct {
	Device, modeAndLinks uint32
	Inode                uint64
	UID, GID             uint32
	AccessSeconds        int64
	AccessNanos          int64
	ModifySeconds        int64
	ModifyNanos          int64
	ChangeSeconds        int64
	ChangeNanos          int64
	BirthSeconds         int64
	BirthNanos           int64
	Size                 int64
	Blocks               int64
	BlockSize            int32
	Flags                uint32
	Generation           uint32
	DeviceID             uint32
	Spare                [2]int64
}

type darwinSocketBufferInfo struct {
	Bytes, HighWater, MemoryBytes, MemoryMax, LowWater uint32
	Flags, Timeout                                     int16
}

type darwinSocketInfo struct {
	Stat                   darwinVInfoStat
	Socket, ProtocolBlock  uint64
	Type, Protocol, Family int32
	Options, Linger        int16
	State, QueueLength     int16
	IncompleteQueueLength  int16
	QueueLimit, Timeout    int16
	Error                  uint16
	OutOfBandMark          uint32
	Receive, Send          darwinSocketBufferInfo
	Kind                   int32
	Reserved               uint32
	ProtocolInfo           [darwinSocketProtocolBytes]byte
}

type darwinProcessFileInfo struct {
	OpenFlags, Status uint32
	Offset            int64
	Type              int32
	GuardFlags        uint32
}

type darwinSocketFDInfo struct {
	File   darwinProcessFileInfo
	Socket darwinSocketInfo
}

type tcpListener struct {
	FileDescriptor int    `json:"fd"`
	Network        string `json:"network"`
	Address        string `json:"address"`
	Port           int    `json:"port"`
	Generation     uint64 `json:"generation"`
}

func parseDarwinTCPListener(fd int, info *darwinSocketFDInfo) (tcpListener, bool, error) {
	if info == nil || fd < 0 {
		return tcpListener{}, false, errors.New("Darwin socket record is invalid")
	}
	if info.Socket.Kind != darwinSocketInfoTCP {
		return tcpListener{}, false, nil
	}
	if info.Socket.Type != darwinSockStream || info.Socket.Protocol != darwinIPProtocolTCP ||
		(info.Socket.Family != darwinAFInet && info.Socket.Family != darwinAFInet6) {
		return tcpListener{}, false, errors.New("Darwin TCP socket has an inconsistent family, type, or protocol")
	}
	protocol := info.Socket.ProtocolInfo[:]
	state := int32(binary.LittleEndian.Uint32(protocol[darwinTCPStateOffset:]))
	if state != darwinTCPStateListen {
		return tcpListener{}, false, nil
	}
	if binary.BigEndian.Uint16(protocol[darwinTCPForeignPortOffset:]) != 0 ||
		!allZero(protocol[darwinTCPForeignAddrOffset:darwinTCPLocalAddrOffset]) {
		return tcpListener{}, false, errors.New("Darwin listening socket unexpectedly names a foreign endpoint")
	}
	port := int(binary.BigEndian.Uint16(protocol[darwinTCPLocalPortOffset:]))
	if port == 0 {
		return tcpListener{}, false, errors.New("Darwin listening socket has no local port")
	}
	generation := binary.LittleEndian.Uint64(protocol[8:16])
	if generation == 0 {
		return tcpListener{}, false, errors.New("Darwin listening socket has no kernel generation identity")
	}
	flags := protocol[darwinTCPInternetFlagsOffset]
	local := protocol[darwinTCPLocalAddrOffset : darwinTCPLocalAddrOffset+16]
	var network string
	var address netip.Addr
	switch info.Socket.Family {
	case darwinAFInet:
		if flags != darwinInternetFlagIPv4 || !allZero(local[:12]) {
			return tcpListener{}, false, errors.New("Darwin IPv4 listener has an inconsistent address family")
		}
		network = "tcp4"
		address = netip.AddrFrom4([4]byte(local[12:16]))
	case darwinAFInet6:
		if flags&darwinInternetFlagIPv6 == 0 {
			return tcpListener{}, false, errors.New("Darwin IPv6 listener has an inconsistent address family")
		}
		network = "tcp6"
		address = netip.AddrFrom16([16]byte(local))
	}
	return tcpListener{
		FileDescriptor: fd,
		Network:        network,
		Address:        address.String(),
		Port:           port,
		Generation:     generation,
	}, true, nil
}

func allZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}

func sortTCPListeners(listeners []tcpListener) {
	sort.Slice(listeners, func(left, right int) bool {
		if listeners[left].Port != listeners[right].Port {
			return listeners[left].Port < listeners[right].Port
		}
		if listeners[left].Network != listeners[right].Network {
			return listeners[left].Network < listeners[right].Network
		}
		if listeners[left].Address != listeners[right].Address {
			return listeners[left].Address < listeners[right].Address
		}
		return listeners[left].FileDescriptor < listeners[right].FileDescriptor
	})
}

func validateHostedListeners(companion, server, presentation []tcpListener) error {
	if len(companion) != 0 {
		return errors.New("companion supervisor unexpectedly owns a TCP listener")
	}
	if err := validateRoleListeners("serve", server, []int{18765, 18766}); err != nil {
		return err
	}
	return validateRoleListeners("presentation", presentation, []int{18767})
}

func validateRoleListeners(role string, listeners []tcpListener, expectedPorts []int) error {
	if len(listeners) != len(expectedPorts) {
		return fmt.Errorf("%s TCP listener count is %d, want %d", role, len(listeners), len(expectedPorts))
	}
	wanted := make(map[int]struct{}, len(expectedPorts))
	for _, port := range expectedPorts {
		wanted[port] = struct{}{}
	}
	descriptors := make(map[int]struct{}, len(listeners))
	for _, listener := range listeners {
		if listener.Network != "tcp4" || listener.Address != "127.0.0.1" {
			return fmt.Errorf("%s TCP listener is not exact IPv4 loopback", role)
		}
		if _, ok := wanted[listener.Port]; !ok {
			return fmt.Errorf("%s owns unexpected TCP listener port %d", role, listener.Port)
		}
		delete(wanted, listener.Port)
		if listener.FileDescriptor < 0 {
			return fmt.Errorf("%s TCP listener has an invalid descriptor", role)
		}
		if listener.Generation == 0 {
			return fmt.Errorf("%s TCP listener has no kernel generation identity", role)
		}
		if _, duplicate := descriptors[listener.FileDescriptor]; duplicate {
			return fmt.Errorf("%s TCP listener repeats descriptor %d", role, listener.FileDescriptor)
		}
		descriptors[listener.FileDescriptor] = struct{}{}
	}
	if len(wanted) != 0 {
		return fmt.Errorf("%s TCP listener population is incomplete", role)
	}
	return nil
}
