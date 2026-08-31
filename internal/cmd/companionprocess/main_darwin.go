//go:build darwin

// Command companionprocess freezes the exact Darwin process population used
// by the hosted companion release gate. It is internal test infrastructure,
// not a server or presentation feature.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/runtimeartifact"
	"github.com/bojieli/OpenRealtime/management"
	"golang.org/x/sys/unix"
)

type processIdentity struct {
	PID           int    `json:"pid"`
	PPID          int    `json:"ppid"`
	PGID          int    `json:"pgid"`
	StartSeconds  int64  `json:"start_seconds"`
	StartMicros   int32  `json:"start_micros"`
	Executable    string `json:"executable"`
	ArgumentsHash string `json:"arguments_digest"`
	RuntimeDigest string `json:"runtime_digest"`
}

type processReceipt struct {
	Schema       string          `json:"schema"`
	Companion    processIdentity `json:"companion"`
	Server       processIdentity `json:"server"`
	Presentation processIdentity `json:"presentation"`
}

type processSample struct {
	identity processIdentity
	args     []string
}

type kernelIdentity struct {
	pid, ppid, pgid int
	seconds         int64
	micros          int32
}

func main() {
	binaryPath := flag.String("binary", "", "exact signed companion executable")
	companionPID := flag.Int("companion-pid", 0, "companion supervisor PID")
	runtimeDigest := flag.String("runtime-digest", "", "runtime digest frozen from server health")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("unexpected positional argument"))
	}
	receipt, err := inspectPopulation(*binaryPath, *companionPID, *runtimeDigest)
	if err != nil {
		fatal(err)
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("OPENREALTIME_COMPANION_PROCESS_RECEIPT %s\n", payload)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "companion process proof: %v\n", err)
	os.Exit(1)
}

func inspectPopulation(binaryPath string, companionPID int, expectedDigest string) (processReceipt, error) {
	if !filepath.IsAbs(binaryPath) || filepath.Clean(binaryPath) != binaryPath || companionPID <= 1 ||
		!management.CanonicalDigest(expectedDigest) {
		return processReceipt{}, errors.New("exact binary, PID, and runtime digest are required")
	}
	info, err := os.Lstat(binaryPath)
	if err != nil || !info.Mode().IsRegular() {
		return processReceipt{}, errors.New("companion binary is not one regular file")
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return processReceipt{}, fmt.Errorf("read Darwin process table: %w", err)
	}
	children := make([]int, 0, 2)
	for _, process := range processes {
		if int(process.Eproc.Ppid) == companionPID {
			children = append(children, int(process.Proc.P_pid))
		}
	}
	sort.Ints(children)
	if len(children) != 2 {
		return processReceipt{}, fmt.Errorf("companion direct child count is %d, want 2", len(children))
	}
	parent, err := sampleProcess(companionPID)
	if err != nil {
		return processReceipt{}, fmt.Errorf("companion: %w", err)
	}
	if !reflect.DeepEqual(parent.args, expectedCompanionArguments(binaryPath)) {
		return processReceipt{}, errors.New("companion argv is not exact")
	}
	var server, presentation processSample
	for _, pid := range children {
		sample, sampleErr := sampleProcess(pid)
		if sampleErr != nil {
			return processReceipt{}, sampleErr
		}
		if sample.identity.PPID != companionPID || sample.identity.PGID != pid ||
			sample.identity.Executable != binaryPath {
			return processReceipt{}, errors.New("companion child ancestry, process group, or executable is not exact")
		}
		switch {
		case reflect.DeepEqual(sample.args, expectedServerArguments(binaryPath)):
			if server.identity.PID != 0 {
				return processReceipt{}, errors.New("duplicate exact server child")
			}
			server = sample
		case reflect.DeepEqual(sample.args, expectedPresentationArguments(binaryPath)):
			if presentation.identity.PID != 0 {
				return processReceipt{}, errors.New("duplicate exact presentation child")
			}
			presentation = sample
		default:
			return processReceipt{}, errors.New("companion child argv is not an exact serve or present role")
		}
	}
	if server.identity.PID == 0 || presentation.identity.PID == 0 {
		return processReceipt{}, errors.New("serve and presentation child population is incomplete")
	}
	for _, sample := range []processSample{parent, server, presentation} {
		if sample.identity.Executable != binaryPath || sample.identity.RuntimeDigest != expectedDigest {
			return processReceipt{}, errors.New("process executable or active code-signature identity differs from server health")
		}
	}
	for _, child := range []processSample{server, presentation} {
		members := 0
		for _, process := range processes {
			if int(process.Eproc.Pgid) == child.identity.PID {
				members++
				if int(process.Proc.P_pid) != child.identity.PID {
					return processReceipt{}, errors.New("companion child process group has an unexpected member")
				}
			}
		}
		if members != 1 {
			return processReceipt{}, errors.New("companion child process group is not exact")
		}
	}
	return processReceipt{
		Schema:    "openrealtime/companion/process-receipt/v1",
		Companion: parent.identity, Server: server.identity, Presentation: presentation.identity,
	}, nil
}

func sampleProcess(pid int) (processSample, error) {
	before, err := readKernelIdentity(pid)
	if err != nil {
		return processSample{}, err
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return processSample{}, fmt.Errorf("read process argv: %w", err)
	}
	defer func() {
		for index := range raw {
			raw[index] = 0
		}
	}()
	executable, args, err := parseProcessArguments(raw)
	if err != nil {
		return processSample{}, err
	}
	artifact, err := runtimeartifact.ExecutableForPID("go://openrealtime/hosted-companion", pid)
	if err != nil {
		return processSample{}, err
	}
	after, err := readKernelIdentity(pid)
	if err != nil || before != after {
		return processSample{}, errors.New("process identity changed while it was inspected")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return processSample{}, err
	}
	return processSample{identity: processIdentity{
		PID: before.pid, PPID: before.ppid, PGID: before.pgid,
		StartSeconds: before.seconds, StartMicros: before.micros,
		Executable: executable, ArgumentsHash: digest(encoded), RuntimeDigest: artifact.Digest,
	}, args: args}, nil
}

func readKernelIdentity(pid int) (kernelIdentity, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || process == nil || int(process.Proc.P_pid) != pid {
		return kernelIdentity{}, errors.New("process does not have one stable kernel identity")
	}
	return kernelIdentity{
		pid: pid, ppid: int(process.Eproc.Ppid), pgid: int(process.Eproc.Pgid),
		seconds: process.Proc.P_starttime.Sec, micros: process.Proc.P_starttime.Usec,
	}, nil
}

func parseProcessArguments(raw []byte) (string, []string, error) {
	if len(raw) < 6 || len(raw) > 4<<20 {
		return "", nil, errors.New("process argv buffer is outside its bound")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	if argc <= 0 || argc > 4096 {
		return "", nil, errors.New("process argc is outside its bound")
	}
	offset := 4
	end := bytes.IndexByte(raw[offset:], 0)
	if end <= 0 {
		return "", nil, errors.New("process executable path is absent")
	}
	executable := string(raw[offset : offset+end])
	offset += end + 1
	for offset < len(raw) && raw[offset] == 0 {
		offset++
	}
	args := make([]string, 0, argc)
	for len(args) < argc {
		if offset >= len(raw) {
			return "", nil, errors.New("process argv is truncated")
		}
		end = bytes.IndexByte(raw[offset:], 0)
		if end < 0 {
			return "", nil, errors.New("process argv is unterminated")
		}
		value := raw[offset : offset+end]
		if !utf8.Valid(value) {
			return "", nil, errors.New("process argv is not UTF-8")
		}
		args = append(args, string(value))
		offset += end + 1
	}
	if !utf8.ValidString(executable) || !filepath.IsAbs(executable) || args[0] != executable {
		return "", nil, errors.New("process executable and argv[0] are not exact")
	}
	return executable, args, nil
}

func digest(payload []byte) string {
	value := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(value[:])
}

func expectedCompanionArguments(binary string) []string {
	return []string{binary, "companion", "-server-listen", "127.0.0.1:18765",
		"-webrtc-listen", "127.0.0.1:18766", "-presentation-listen", "127.0.0.1:18767",
		"-client", "none", "-token-env", "OPENREALTIME_HOSTED_COMPANION_TOKEN",
		"-ready-timeout", "90s", "-shutdown-timeout", "10s", "--",
		"-slow-provider", "vllm", "-slow-model", "hosted-companion-smoke"}
}

func expectedServerArguments(binary string) []string {
	return []string{binary, "serve", "-listen", "127.0.0.1:18765", "-webrtc-listen",
		"127.0.0.1:18766", "-webrtc-allow-origin", "http://127.0.0.1:18767",
		"-model", "openrealtime", "-token-env", "OPENREALTIME_HOSTED_COMPANION_TOKEN",
		"-shutdown-timeout", "10s", "-slow-provider", "vllm", "-slow-model",
		"hosted-companion-smoke"}
}

func expectedPresentationArguments(binary string) []string {
	return []string{binary, "present", "-listen", "127.0.0.1:18767", "-endpoint",
		"ws://127.0.0.1:18765/v1/realtime", "-webrtc-endpoint",
		"http://127.0.0.1:18766/v1/realtime/calls", "-management-endpoint",
		"http://127.0.0.1:18765/openrealtime/v1", "-model", "openrealtime",
		"-token-env", "OPENREALTIME_HOSTED_COMPANION_TOKEN", "-client-profile",
		"browser-developer-webrtc", "-native-websocket-relay", "-shutdown-timeout", "10s"}
}
