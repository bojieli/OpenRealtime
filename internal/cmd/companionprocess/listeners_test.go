package main

import (
	"encoding/binary"
	"strings"
	"testing"
	"unsafe"
)

func TestDarwinSocketLayout(t *testing.T) {
	if got := int(unsafe.Sizeof(darwinSocketFDInfo{})); got != darwinSocketFDInfoBytes {
		t.Fatalf("darwin socket_fdinfo size = %d, want %d", got, darwinSocketFDInfoBytes)
	}
	if got := unsafe.Offsetof(darwinSocketFDInfo{}.Socket); got != 24 {
		t.Fatalf("darwin socket_info offset = %d, want 24", got)
	}
	if got := unsafe.Offsetof(darwinSocketFDInfo{}.Socket) +
		unsafe.Offsetof(darwinSocketFDInfo{}.Socket.ProtocolInfo); got != 264 {
		t.Fatalf("Darwin socket protocol offset = %d, want 264", got)
	}
}

func TestParseDarwinTCPListener(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*darwinSocketFDInfo)
		listening bool
		want      tcpListener
		wantError string
	}{
		{
			name: "exact IPv4 loopback",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{127, 0, 0, 1}, 18765, 41)
			},
			listening: true,
			want: tcpListener{FileDescriptor: 9, Network: "tcp4", Address: "127.0.0.1",
				Port: 18765, Generation: 41},
		},
		{
			name: "IPv4 wildcard remains visible",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{0, 0, 0, 0}, 18765, 42)
			},
			listening: true,
			want: tcpListener{FileDescriptor: 9, Network: "tcp4", Address: "0.0.0.0",
				Port: 18765, Generation: 42},
		},
		{
			name: "IPv6 loopback remains visible",
			mutate: func(info *darwinSocketFDInfo) {
				address := make([]byte, 16)
				address[15] = 1
				setDarwinListener(info, darwinAFInet6, darwinInternetFlagIPv6,
					address, 18765, 43)
			},
			listening: true,
			want: tcpListener{FileDescriptor: 9, Network: "tcp6", Address: "::1",
				Port: 18765, Generation: 43},
		},
		{
			name: "connected TCP socket is not a listener",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{127, 0, 0, 1}, 18765, 44)
				binary.LittleEndian.PutUint32(info.Socket.ProtocolInfo[darwinTCPStateOffset:], 4)
			},
		},
		{
			name: "non TCP socket is ignored",
			mutate: func(info *darwinSocketFDInfo) {
				info.Socket.Kind = 3
			},
		},
		{
			name: "wrong protocol",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{127, 0, 0, 1}, 18765, 45)
				info.Socket.Protocol = 17
			},
			wantError: "inconsistent family",
		},
		{
			name: "family flag mismatch",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv6,
					[]byte{127, 0, 0, 1}, 18765, 46)
			},
			wantError: "inconsistent address family",
		},
		{
			name: "foreign endpoint",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{127, 0, 0, 1}, 18765, 47)
				binary.BigEndian.PutUint16(info.Socket.ProtocolInfo[darwinTCPForeignPortOffset:], 1234)
			},
			wantError: "foreign endpoint",
		},
		{
			name: "zero local port",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{127, 0, 0, 1}, 0, 48)
			},
			wantError: "no local port",
		},
		{
			name: "zero generation",
			mutate: func(info *darwinSocketFDInfo) {
				setDarwinListener(info, darwinAFInet, darwinInternetFlagIPv4,
					[]byte{127, 0, 0, 1}, 18765, 0)
			},
			wantError: "generation identity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var info darwinSocketFDInfo
			test.mutate(&info)
			got, listening, err := parseDarwinTCPListener(9, &info)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("parse error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if listening != test.listening || got != test.want {
				t.Fatalf("listener = (%+v, %t), want (%+v, %t)",
					got, listening, test.want, test.listening)
			}
		})
	}
}

func TestValidateHostedListeners(t *testing.T) {
	exact := func(fd, port int) tcpListener {
		return tcpListener{FileDescriptor: fd, Network: "tcp4", Address: "127.0.0.1",
			Port: port, Generation: uint64(fd + 100)}
	}
	server := []tcpListener{exact(8, 18765), exact(9, 18766)}
	presentation := []tcpListener{exact(7, 18767)}
	if err := validateHostedListeners([]tcpListener{}, server, presentation); err != nil {
		t.Fatalf("exact listener population: %v", err)
	}
	tests := []struct {
		name         string
		companion    []tcpListener
		server       []tcpListener
		presentation []tcpListener
		want         string
	}{
		{"supervisor listener", []tcpListener{exact(3, 18764)}, server, presentation, "supervisor"},
		{"missing server listener", nil, server[:1], presentation, "count"},
		{"extra server listener", nil, append(append([]tcpListener{}, server...), exact(10, 18768)), presentation, "count"},
		{"wrong server port", nil, []tcpListener{exact(8, 18765), exact(9, 18768)}, presentation, "unexpected"},
		{"wildcard server", nil, []tcpListener{{FileDescriptor: 8, Network: "tcp4", Address: "0.0.0.0", Port: 18765}, exact(9, 18766)}, presentation, "loopback"},
		{"IPv6 server", nil, []tcpListener{{FileDescriptor: 8, Network: "tcp6", Address: "::1", Port: 18765}, exact(9, 18766)}, presentation, "loopback"},
		{"duplicate descriptor", nil, []tcpListener{exact(8, 18765), exact(8, 18766)}, presentation, "repeats"},
		{"zero generation", nil, []tcpListener{{FileDescriptor: 8, Network: "tcp4", Address: "127.0.0.1", Port: 18765}, exact(9, 18766)}, presentation, "generation"},
		{"wrong presentation", nil, server, []tcpListener{exact(7, 18768)}, "unexpected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateHostedListeners(test.companion, test.server, test.presentation)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func setDarwinListener(info *darwinSocketFDInfo, family int32, flags byte,
	address []byte, port int, generation uint64,
) {
	info.Socket.Kind = darwinSocketInfoTCP
	info.Socket.Type = darwinSockStream
	info.Socket.Protocol = darwinIPProtocolTCP
	info.Socket.Family = family
	protocol := info.Socket.ProtocolInfo[:]
	binary.BigEndian.PutUint16(protocol[darwinTCPLocalPortOffset:], uint16(port))
	binary.LittleEndian.PutUint64(protocol[8:], generation)
	protocol[darwinTCPInternetFlagsOffset] = flags
	if family == darwinAFInet {
		copy(protocol[darwinTCPLocalAddrOffset+12:], address)
	} else {
		copy(protocol[darwinTCPLocalAddrOffset:], address)
	}
	binary.LittleEndian.PutUint32(protocol[darwinTCPStateOffset:], darwinTCPStateListen)
}
