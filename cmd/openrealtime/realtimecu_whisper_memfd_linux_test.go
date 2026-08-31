//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRealtimeCUWhisperServiceHandleRequiresExactWriteSealedMemfd(t *testing.T) {
	content := []byte("reviewed service fixture")
	sealed, err := unix.MemfdCreate("openrealtime-whisper-service-v2", unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(sealed) })
	if _, err := unix.Write(sealed, content); err != nil {
		t.Fatal(err)
	}
	wantSeals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(uintptr(sealed), unix.F_ADD_SEALS, wantSeals); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/proc/self/fd/%d", sealed)
	if err := verifyRealtimeCUWhisperServiceHandle(path); err != nil {
		t.Fatal(err)
	}
	digest, err := digestRealtimeCUOpenRegularFile(context.Background(), path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if digest != wantDigest {
		t.Fatalf("sealed service digest = %q, want %q", digest, wantDigest)
	}

	unsealed, err := unix.MemfdCreate("openrealtime-whisper-service-v2", unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(unsealed) })
	if err := verifyRealtimeCUWhisperServiceHandle(fmt.Sprintf("/proc/self/fd/%d", unsealed)); err == nil {
		t.Fatal("unsealed Whisper service handle was accepted")
	}
	wrongName, err := unix.MemfdCreate("openrealtime-whisper-service-v2-extra", unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(wrongName) })
	if _, err := unix.Write(wrongName, content); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(wrongName), unix.F_ADD_SEALS, wantSeals); err != nil {
		t.Fatal(err)
	}
	if err := verifyRealtimeCUWhisperServiceHandle(fmt.Sprintf("/proc/self/fd/%d", wrongName)); err == nil {
		t.Fatal("wrong-name sealed Whisper service handle was accepted")
	}
}

func TestWhisperPythonServiceIdentityRejectsEveryUnsealedOrMisnamedHandle(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(filepath.Clean(filepath.Join(workingDirectory, "..", "..")),
		"tools", "whisper", "server.py")
	program := `
import fcntl, hashlib, importlib.util, os, pathlib, sys, tempfile
spec = importlib.util.spec_from_file_location("reviewed_whisper_server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)

def reject(label, handle):
    try:
        server.sealed_service_digest(f"/proc/self/fd/{handle}")
    except RuntimeError:
        return
    raise SystemExit(label + " was accepted")

regular_path = pathlib.Path(tempfile.mkdtemp()) / "service.py"
regular_path.write_bytes(b"regular")
regular = os.open(regular_path, os.O_RDONLY)
reject("regular file", regular)
deleted = os.open(regular_path, os.O_RDONLY)
regular_path.unlink()
reject("deleted regular file", deleted)

required = (fcntl.F_SEAL_SEAL | fcntl.F_SEAL_SHRINK |
            fcntl.F_SEAL_GROW | fcntl.F_SEAL_WRITE)
wrong_name = os.memfd_create("different-service", os.MFD_ALLOW_SEALING)
os.write(wrong_name, b"reviewed")
fcntl.fcntl(wrong_name, fcntl.F_ADD_SEALS, required)
reject("wrong-name sealed memfd", wrong_name)

unsealed = os.memfd_create("openrealtime-whisper-service-v2", os.MFD_ALLOW_SEALING)
os.write(unsealed, b"reviewed")
reject("unsealed exact-name memfd", unsealed)

sealed = os.memfd_create("openrealtime-whisper-service-v2", os.MFD_ALLOW_SEALING)
payload = b"reviewed"
os.write(sealed, payload)
fcntl.fcntl(sealed, fcntl.F_ADD_SEALS, required)
got = server.sealed_service_digest(f"/proc/self/fd/{sealed}")
want = "sha256:" + hashlib.sha256(payload).hexdigest()
if got != want:
    raise SystemExit("exact sealed memfd digest differs")
`
	command := exec.Command(python, "-I", "-S", "-B", "-c", program, service)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python sealed-service identity guard failed: %v\n%s", err, output)
	}
}
