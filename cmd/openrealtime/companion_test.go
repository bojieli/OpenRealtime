package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/macos"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
)

func TestCompanionSupervisesExactPublicProcessesWithNoClientLaunch(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	serverAddress := companionFreeAddress(t)
	webRTCAddress := companionFreeAddress(t)
	presentationAddress := companionFreeAddress(t)
	logPath := filepath.Join(t.TempDir(), "children.jsonl")
	secret := "companion-secret-must-not-escape"
	var browserLaunches, macOSLaunches atomic.Int64
	readyChannel := make(chan companionReady, 1)
	output := &companionLockedBuffer{}
	runtime := companionRuntime{
		executable: executable,
		prefix:     []string{"-test.run=^TestCompanionHelperProcess$", "--"},
		environment: append(os.Environ(),
			"OPENREALTIME_COMPANION_HELPER=1",
			"OPENREALTIME_COMPANION_HELPER_LOG="+logPath,
			"OPENREALTIME_COMPANION_TEST_TOKEN="+secret,
		),
		goos:        "linux",
		httpClient:  &http.Client{Timeout: time.Second},
		childOutput: output,
		launchBrowser: func(context.Context, string) error {
			browserLaunches.Add(1)
			return nil
		},
		launchMacOS: func(context.Context, string, string) error {
			macOSLaunches.Add(1)
			return nil
		},
		onReady: func(ready companionReady) { readyChannel <- ready },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runCompanionContext(ctx, []string{
			"-server-listen", serverAddress,
			"-webrtc-listen", webRTCAddress,
			"-presentation-listen", presentationAddress,
			"-token-env", "OPENREALTIME_COMPANION_TEST_TOKEN",
			"-client", "none",
			"-ready-timeout", "10s",
			"-shutdown-timeout", "2s",
			"--", "-binding", "upstream", "-upstream-model", "model with spaces",
		}, output, runtime)
	}()

	var ready companionReady
	select {
	case ready = <-readyChannel:
	case err := <-done:
		t.Fatalf("companion stopped before readiness: %v\n%s", err, output.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("companion readiness timed out\n%s", output.String())
	}
	if ready.ServerURL != "http://"+serverAddress ||
		ready.WebRTCURL != "http://"+webRTCAddress+"/v1/realtime/calls" ||
		ready.PresentationURL != "http://"+presentationAddress ||
		ready.ManagementURL != "http://"+serverAddress+management.APIPrefix {
		t.Fatalf("companion ready projection = %#v", ready)
	}
	if ready.BrowserManifest.Platform != "browser" || ready.BrowserManifest.Fingerprint == "" {
		t.Fatalf("companion manifest = %#v", ready.BrowserManifest)
	}
	if ready.NativeEndpointFile == "" || !ready.NativeEndpointFileRetain {
		t.Fatalf("custom presentation did not publish native endpoint file: %#v", ready)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(ready.NativeEndpointFile)) })
	payload, err := os.ReadFile(ready.NativeEndpointFile)
	if err != nil {
		t.Fatal(err)
	}
	var directory presentation.EndpointDirectory
	if err := json.Unmarshal(payload, &directory); err != nil {
		t.Fatal(err)
	}
	if err := directory.Validate(); err != nil {
		t.Fatal(err)
	}
	if directory.Fingerprint != ready.NativeEndpointDirectory.Fingerprint {
		t.Fatalf("native file fingerprint = %s, want %s", directory.Fingerprint, ready.NativeEndpointDirectory.Fingerprint)
	}
	if _, err := macos.NewNativeBundleForDistributionWithEndpointDirectory(
		macos.NativeObserverDeveloperDistribution, directory,
	); err != nil {
		t.Fatal(err)
	}
	if browserLaunches.Load() != 0 || macOSLaunches.Load() != 0 {
		t.Fatalf("none policy launched browser=%d macos=%d", browserLaunches.Load(), macOSLaunches.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("companion shutdown: %v\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("companion did not stop children\n%s", output.String())
	}
	text := output.String()
	for _, required := range []string{
		"[serve] helper serve stdout", "[serve] helper serve stderr",
		"[present] helper present stdout", "[present] helper present stderr",
		"OpenRealtime companion ready", "client       none",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("companion output omitted %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, secret) {
		t.Fatal("companion output exposed the credential value")
	}

	logPayload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logPayload), secret) {
		t.Fatal("companion placed the credential value in child argv")
	}
	var records []companionHelperRecord
	for _, line := range bytes.Split(bytes.TrimSpace(logPayload), []byte{'\n'}) {
		var record companionHelperRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 4 || records[0].Event != "serve-start" ||
		records[1].Event != "present-start" || records[2].Event != "present-stop" ||
		records[3].Event != "serve-stop" {
		t.Fatalf("companion lifecycle = %#v", records)
	}
	serveArguments, presentArguments := records[0].Arguments, records[1].Arguments
	companionRequireArgument(t, serveArguments, "-listen", serverAddress)
	companionRequireArgument(t, serveArguments, "-webrtc-listen", webRTCAddress)
	companionRequireArgument(t, serveArguments, "-webrtc-allow-origin", "http://"+presentationAddress)
	companionRequireArgument(t, serveArguments, "-token-env", "OPENREALTIME_COMPANION_TEST_TOKEN")
	companionRequireArgument(t, serveArguments, "-upstream-model", "model with spaces")
	companionRequireArgument(t, presentArguments, "-endpoint", "ws://"+serverAddress+"/v1/realtime")
	companionRequireArgument(t, presentArguments, "-webrtc-endpoint", "http://"+webRTCAddress+"/v1/realtime/calls")
	companionRequireArgument(t, presentArguments, "-management-endpoint", "http://"+serverAddress+management.APIPrefix)
	if !companionHasArgument(presentArguments, "-native-websocket-relay") {
		t.Fatalf("presentation argv omitted explicit native relay: %#v", presentArguments)
	}
}

func TestCompanionParserRejectsOwnedServeFlagsAndUnsafeSelections(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "owned listen", arguments: []string{"--", "-listen", "127.0.0.1:9999"}, want: "owned by companion"},
		{name: "owned equals", arguments: []string{"--", "--webrtc-listen=127.0.0.1:9999"}, want: "owned by companion"},
		{name: "missing delimiter", arguments: []string{"value"}, want: "literal --"},
		{name: "unsafe server", arguments: []string{"-server-listen", "0.0.0.0:8765"}, want: "must be loopback"},
		{name: "shared port", arguments: []string{"-server-listen", "127.0.0.1:8767"}, want: "must be distinct"},
		{name: "unknown client", arguments: []string{"-client", "desktop"}, want: "unknown companion client"},
		{name: "token value not environment", arguments: []string{"-token-env", "literal-secret=value"}, want: "environment name is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCompanionOptions(test.arguments, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompanionRejectsNonWebRTCManifestBeforeReady(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := companionRuntime{
		executable: executable,
		prefix:     []string{"-test.run=^TestCompanionHelperProcess$", "--"},
		environment: append(os.Environ(),
			"OPENREALTIME_COMPANION_HELPER=1",
			"OPENREALTIME_COMPANION_HELPER_MANIFEST=websocket",
		),
		goos:        "linux",
		httpClient:  &http.Client{Timeout: time.Second},
		childOutput: io.Discard,
	}
	var output bytes.Buffer
	err = runCompanionContext(ctx, []string{
		"-server-listen", companionFreeAddress(t),
		"-webrtc-listen", companionFreeAddress(t),
		"-presentation-listen", companionFreeAddress(t),
		"-client", "none", "-ready-timeout", "350ms", "-shutdown-timeout", "2s",
	}, &output, runtime)
	if err == nil || !strings.Contains(err.Error(), "served manifest does not match browser-developer-webrtc") {
		t.Fatalf("manifest identity error = %v", err)
	}
	if strings.Contains(output.String(), "companion ready") {
		t.Fatal("companion announced readiness for the wrong browser profile")
	}
}

func TestCompanionHelperProcess(t *testing.T) {
	if os.Getenv("OPENREALTIME_COMPANION_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatal("helper command is absent")
	}
	arguments := append([]string(nil), os.Args[separator+1:]...)
	command := arguments[0]
	arguments = arguments[1:]
	record := func(event string) {
		path := os.Getenv("OPENREALTIME_COMPANION_HELPER_LOG")
		if path == "" {
			return
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(file).Encode(companionHelperRecord{Event: event, Arguments: arguments}); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "serve":
		record("serve-start")
		fmt.Println("helper serve stdout")
		fmt.Fprintln(os.Stderr, "helper serve stderr")
		server := companionHelperHealthServer(t, companionArgumentValue(t, arguments, "-listen"))
		webRTC := companionHelperHealthServer(t, companionArgumentValue(t, arguments, "-webrtc-listen"))
		<-ctx.Done()
		companionHelperShutdown(t, webRTC)
		companionHelperShutdown(t, server)
		record("serve-stop")
	case "present":
		record("present-start")
		fmt.Println("helper present stdout")
		fmt.Fprintln(os.Stderr, "helper present stderr")
		var bundle *presentationbrowser.Bundle
		var err error
		if os.Getenv("OPENREALTIME_COMPANION_HELPER_MANIFEST") == "websocket" {
			bundle, err = presentationbrowser.ObserverDeveloperBundle()
		} else {
			bundle, err = presentationbrowser.ObserverDeveloperWebRTCBundle()
		}
		if err != nil {
			t.Fatal(err)
		}
		payload, err := presentation.MarshalManifest(bundle.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /client/v1/manifest", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(payload)
		})
		server := companionHelperServer(t, companionArgumentValue(t, arguments, "-listen"), mux)
		<-ctx.Done()
		companionHelperShutdown(t, server)
		record("present-stop")
	default:
		t.Fatalf("unknown helper command %q", command)
	}
}

type companionHelperRecord struct {
	Event     string   `json:"event"`
	Arguments []string `json:"arguments"`
}

func companionHelperHealthServer(t *testing.T, address string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	})
	return companionHelperServer(t, address, mux)
}

func companionHelperServer(t *testing.T, address string, handler http.Handler) *http.Server {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "helper server:", err)
		}
	}()
	return server
}

func companionHelperShutdown(t *testing.T, server *http.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func companionFreeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func companionArgumentValue(t *testing.T, arguments []string, name string) string {
	t.Helper()
	for index := range arguments {
		if arguments[index] == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	t.Fatalf("argument %s is absent from %#v", name, arguments)
	return ""
}

func companionRequireArgument(t *testing.T, arguments []string, name, value string) {
	t.Helper()
	if got := companionArgumentValue(t, arguments, name); got != value {
		t.Fatalf("argument %s = %q, want %q in %#v", name, got, value, arguments)
	}
}

func companionHasArgument(arguments []string, want string) bool {
	for _, argument := range arguments {
		if argument == want {
			return true
		}
	}
	return false
}

type companionLockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *companionLockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(value)
}

func (buffer *companionLockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}
