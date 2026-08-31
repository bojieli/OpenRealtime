//go:build darwin

package main

import (
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStableTCPListenersReadsLiveDarwinKernelSocket(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	endpoint := listener.Addr().(*net.TCPAddr)
	listeners, err := stableTCPListeners(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range listeners {
		if current.Network == "tcp4" && current.Address == "127.0.0.1" &&
			current.Port == endpoint.Port && current.FileDescriptor >= 0 {
			return
		}
	}
	t.Fatalf("live Darwin listener 127.0.0.1:%d absent from %+v", endpoint.Port, listeners)
}

func TestSnapshotPopulationRejectsDescendantsAndGroupIntruders(t *testing.T) {
	const companionPID, serverPID, presentationPID = 100, 101, 102
	exact := []unix.KinfoProc{
		testKinfoProc(companionPID, 50, 50, 10),
		testKinfoProc(serverPID, companionPID, serverPID, 11),
		testKinfoProc(presentationPID, companionPID, presentationPID, 12),
	}
	snapshot, err := snapshotPopulation(exact, companionPID, []int{serverPID, presentationPID})
	if err != nil {
		t.Fatalf("exact population: %v", err)
	}
	if len(snapshot.Processes) != 3 || len(snapshot.Groups) != 2 {
		t.Fatalf("exact population snapshot = %+v", snapshot)
	}
	tests := []struct {
		name string
		add  unix.KinfoProc
		want string
	}{
		{"descendant", testKinfoProc(103, serverPID, 103, 13), "descendant"},
		{"group intruder", testKinfoProc(103, 50, serverPID, 13), "process group"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			population := append(append([]unix.KinfoProc{}, exact...), test.add)
			_, err := snapshotPopulation(population, companionPID, []int{serverPID, presentationPID})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("snapshot error = %v, want %q", err, test.want)
			}
		})
	}
}

func testKinfoProc(pid, ppid, pgid int, start int64) unix.KinfoProc {
	var process unix.KinfoProc
	process.Proc.P_pid = int32(pid)
	process.Eproc.Ppid = int32(ppid)
	process.Eproc.Pgid = int32(pgid)
	process.Proc.P_starttime.Sec = start
	process.Proc.P_starttime.Usec = int32(pid)
	return process
}
