//go:build darwin && signed_macos_e2e

package macos

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
)

const (
	signedAppEnvironment              = "OPENREALTIME_SIGNED_APP"
	signedRunnerEnvironment           = "OPENREALTIME_MACOS_E2E_RUNNER"
	signedLaunchProfileEnvironment    = "OPENREALTIME_MACOS_E2E_LAUNCH_PROFILE"
	signedGraphEnvironment            = "OPENREALTIME_MACOS_E2E_GRAPH_FINGERPRINT"
	signedServerProfileEnvironment    = "OPENREALTIME_MACOS_E2E_SERVER_PROFILE_FINGERPRINT"
	signedVerifiedContractEnvironment = "OPENREALTIME_SIGNED_MACOS_VERIFIED_CONTRACT"
	signedVerifiedReceiptEnvironment  = "OPENREALTIME_SIGNED_MACOS_VERIFIED_RECEIPT"
)

func TestSignedNativeRunnerReleaseGate(t *testing.T) {
	if os.Getenv("OPENREALTIME_SIGNED_MACOS_E2E") != "1" {
		t.Fatal("signed native release gate requires OPENREALTIME_SIGNED_MACOS_E2E=1")
	}
	repositoryRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	appPath := requireSignedEnvironmentPath(t, signedAppEnvironment, ".app")
	runnerPath := requireSignedEnvironmentPath(t, signedRunnerEnvironment, "")
	launchProfilePath := requireSignedEnvironmentPath(t, signedLaunchProfileEnvironment, "")
	graphFingerprint := requireSignedDigestEnvironment(t, signedGraphEnvironment)
	serverProfileFingerprint := requireSignedDigestEnvironment(t, signedServerProfileEnvironment)
	verifiedContractPath := requireSignedEnvironmentPath(t, signedVerifiedContractEnvironment, ".json")
	verifiedReceiptPath := requireSignedEnvironmentPath(t, signedVerifiedReceiptEnvironment, ".json")
	if verifiedContractPath == verifiedReceiptPath {
		t.Fatal("signed companion verified contract and receipt paths must be distinct")
	}

	root, err := os.MkdirTemp("", "openrealtime-signed-companion-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove signed companion gate directory: %v", err)
		}
	})

	serverPath := filepath.Join(root, "openrealtime")
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	build := exec.Command(goBinary, "build", "-trimpath", "-o", serverPath, "./cmd/openrealtime")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build exact signed-companion server executable: %v\n%s", err, output)
	}

	appBefore := captureSignedAppIdentity(t, appPath)
	runnerBefore := SignedRunnerIdentity{
		Path:   runnerPath,
		Digest: mustSignedRegularFileDigest(t, runnerPath, "trusted runner"),
	}
	serverDigest := mustSignedRegularFileDigest(t, serverPath, "gate-built server executable")
	launchProfileDigest := mustSignedRegularFileDigest(t, launchProfilePath, "server launch profile")
	clients := signedCompanionClientExpectations(t)
	contract := SignedCompanionRunnerContract{
		FormatVersion:   SignedCompanionReceiptFormatVersion,
		Schema:          SignedCompanionRunnerContractSchema,
		InvocationNonce: randomSignedDigest(t),
		Runner:          runnerBefore,
		App:             appBefore,
		Server: SignedServerExpectation{
			RunIdentity:         randomSignedDigest(t),
			GraphFingerprint:    graphFingerprint,
			ProfileFingerprint:  serverProfileFingerprint,
			ExecutablePath:      serverPath,
			ExecutableDigest:    serverDigest,
			LaunchProfilePath:   launchProfilePath,
			LaunchProfileDigest: launchProfileDigest,
		},
		Clients: clients,
	}
	contractPath := filepath.Join(root, "runner-contract.json")
	runnerReceiptPath := filepath.Join(root, "runner-receipt.json")
	secrets := signedCompanionSensitiveValues(t)
	contractPayload, contractDigest, err := MarshalSignedCompanionRunnerContract(contract)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSignedSensitivePayload(contractPayload, secrets); err != nil {
		t.Fatalf("validate signed companion runner contract: %v", err)
	}
	writtenDigest, err := WriteSignedCompanionRunnerContract(contractPath, contract)
	if err != nil || writtenDigest != contractDigest {
		t.Fatalf("publish signed companion runner contract digest=%q: %v", writtenDigest, err)
	}
	if _, err := os.Lstat(runnerReceiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gate-owned runner receipt path existed before launch: %v", err)
	}

	command := exec.Command("./verify-signed-e2e.sh")
	command.Env = signedCompanionEnvironment(os.Environ(), map[string]string{
		"OPENREALTIME_SIGNED_MACOS_E2E":         "1",
		signedAppEnvironment:                    appPath,
		signedRunnerEnvironment:                 runnerPath,
		"OPENREALTIME_MACOS_E2E_CONTRACT":       contractPath,
		"OPENREALTIME_MACOS_E2E_RUNNER_RECEIPT": runnerReceiptPath,
		"OPENREALTIME_MACOS_E2E_NONCE":          contract.InvocationNonce,
	})
	output, runErr := command.CombinedOutput()
	if runErr != nil {
		t.Fatalf("signed native runner failed: %v\n%s", runErr, redactSignedOutput(output, secrets))
	}

	expected := SignedCompanionReceiptExpectation{
		InvocationNonce: contract.InvocationNonce,
		ContractDigest:  contractDigest,
		Runner:          runnerBefore,
		App:             appBefore,
		Server:          contract.Server,
		Clients:         append([]SignedClientExpectation(nil), clients...),
		SensitiveValues: secrets,
	}
	receipt, receiptDigest, err := ReadSignedCompanionReceipt(runnerReceiptPath, expected)
	if err != nil {
		t.Fatalf("verify trusted runner receipt: %v", err)
	}
	receiptPayload, reopenedDigest, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil || reopenedDigest != receiptDigest {
		t.Fatalf("reopen trusted runner receipt digest=%q: %v", reopenedDigest, err)
	}
	if digest := mustSignedRegularFileDigest(t, contractPath, "runner contract"); digest != contractDigest {
		t.Fatalf("runner contract changed: %s, want %s", digest, contractDigest)
	}
	if after := captureSignedAppIdentity(t, appPath); !reflect.DeepEqual(after, appBefore) {
		t.Fatal("signed application identity changed during the external run")
	}
	if digest := mustSignedRegularFileDigest(t, runnerPath, "trusted runner"); digest != runnerBefore.Digest {
		t.Fatal("trusted runner identity changed during the external run")
	}
	if digest := mustSignedRegularFileDigest(t, serverPath, "gate-built server executable"); digest != serverDigest {
		t.Fatal("gate-built server executable changed during the external run")
	}
	if digest := mustSignedRegularFileDigest(t, launchProfilePath, "server launch profile"); digest != launchProfileDigest {
		t.Fatal("server launch profile changed during the external run")
	}
	if err := WriteVerifiedSignedCompanionContract(verifiedContractPath, contractPayload); err != nil {
		t.Fatal(err)
	}
	if err := WriteVerifiedSignedCompanionReceipt(verifiedReceiptPath, receiptPayload); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("signed macOS companion contract verified %s\n", contractDigest)
	fmt.Printf("signed macOS companion receipt verified %s\n", receiptDigest)
}

func signedCompanionClientExpectations(t *testing.T) []SignedClientExpectation {
	t.Helper()
	browser, err := presentationbrowser.ObserverDeveloperWebRTCBundle()
	if err != nil {
		t.Fatal(err)
	}
	native, err := NewNativeObserverDeveloperBundle()
	if err != nil {
		t.Fatal(err)
	}
	return []SignedClientExpectation{
		signedClientExpectation(t, 1, "browser", "webrtc", browser.Profile.Name,
			browser.Plan.Fingerprint, browser.Manifest, false),
		signedClientExpectation(t, 2, "macos_native", "websocket", native.Profile.Name,
			native.Plan.Fingerprint, native.Manifest, false),
	}
}

func signedClientExpectation(
	t *testing.T, order uint64, client, transport, profileName, planFingerprint string,
	manifest presentation.ClientManifest, accessibilityRequired bool,
) SignedClientExpectation {
	t.Helper()
	implementations, err := json.Marshal(manifest.Implementations)
	if err != nil {
		t.Fatal(err)
	}
	transportDigest := ""
	for _, implementation := range manifest.Implementations {
		if implementation.Entry == "transport" {
			transportDigest = implementation.Artifact.Digest
		}
	}
	expectation := SignedClientExpectation{
		Order: order, Client: client, ClientProfile: profileName, Transport: transport,
		ClientPlanFingerprint: planFingerprint, ClientManifestFingerprint: manifest.Fingerprint,
		ClientImplementationDigest:    signedReceiptDigest(implementations),
		TransportImplementationDigest: transportDigest,
		AccessibilityRequired:         accessibilityRequired,
	}
	if !management.CanonicalDigest(expectation.ClientPlanFingerprint) ||
		!management.CanonicalDigest(expectation.ClientManifestFingerprint) ||
		!management.CanonicalDigest(expectation.ClientImplementationDigest) ||
		!management.CanonicalDigest(expectation.TransportImplementationDigest) ||
		expectation.ClientProfile == "" {
		t.Fatalf("derived signed client expectation is incomplete: %+v", expectation)
	}
	return expectation
}

func captureSignedAppIdentity(t *testing.T, appPath string) SignedAppIdentity {
	t.Helper()
	info, err := os.Lstat(appPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || filepath.Ext(appPath) != ".app" {
		t.Fatalf("signed application must be one exact .app directory: %v", err)
	}
	if output, err := exec.Command("codesign", "--verify", "--deep", "--strict", "--verbose=2", appPath).CombinedOutput(); err != nil {
		t.Fatalf("verify signed application: %v\n%s", err, output)
	}
	signatureOutput, err := exec.Command("codesign", "--display", "--verbose=4", appPath).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect signed application: %v\n%s", err, signatureOutput)
	}
	signature := parseSignedCodeIdentity(t, signatureOutput)
	bundleIdentifier := signedPlistValue(t, appPath, "CFBundleIdentifier")
	executableName := signedPlistValue(t, appPath, "CFBundleExecutable")
	if executableName != filepath.Base(executableName) || executableName == "." {
		t.Fatalf("signed application executable name is invalid: %q", executableName)
	}
	executablePath := filepath.Join(appPath, "Contents", "MacOS", executableName)
	requirementOutput, err := exec.Command("codesign", "-d", "-r-", appPath).CombinedOutput()
	if err != nil {
		t.Fatalf("read signed application designated requirement: %v\n%s", err, requirementOutput)
	}
	requirement := ""
	for _, line := range strings.Split(strings.TrimSpace(string(requirementOutput)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "designated =>") {
			if requirement != "" {
				t.Fatal("signed application has multiple designated requirements")
			}
			requirement = line
		}
	}
	if requirement == "" {
		t.Fatal("signed application has no designated requirement")
	}
	identity := SignedAppIdentity{
		BundlePath:                  appPath,
		BundleDigest:                mustSignedBundleDigest(t, appPath),
		ExecutablePath:              executablePath,
		ExecutableDigest:            mustSignedRegularFileDigest(t, executablePath, "signed application executable"),
		BundleIdentifier:            bundleIdentifier,
		SigningIdentifier:           signature.identifier,
		TeamIdentifier:              signature.teamIdentifier,
		CDHash:                      signature.cdhash,
		AuthorityChainDigest:        signedReceiptDigest([]byte(strings.Join(signature.authorities, "\n") + "\n")),
		DesignatedRequirementDigest: signedReceiptDigest([]byte(requirement + "\n")),
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("validate signed application identity: %v", err)
	}
	return identity
}

type signedCodeIdentity struct {
	identifier     string
	teamIdentifier string
	cdhash         string
	authorities    []string
}

func parseSignedCodeIdentity(t *testing.T, payload []byte) signedCodeIdentity {
	t.Helper()
	result := signedCodeIdentity{}
	for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "Signature=adhoc":
			t.Fatal("signed release gate refuses an ad-hoc application")
		case strings.HasPrefix(line, "Identifier="):
			result.identifier = strings.TrimPrefix(line, "Identifier=")
		case strings.HasPrefix(line, "TeamIdentifier="):
			result.teamIdentifier = strings.TrimPrefix(line, "TeamIdentifier=")
		case strings.HasPrefix(line, "CDHash="):
			result.cdhash = strings.ToLower(strings.TrimPrefix(line, "CDHash="))
		case strings.HasPrefix(line, "Authority="):
			result.authorities = append(result.authorities, strings.TrimPrefix(line, "Authority="))
		}
	}
	if result.identifier == "" || result.teamIdentifier == "" || result.cdhash == "" || len(result.authorities) == 0 {
		t.Fatalf("signed application code identity is incomplete: %+v", result)
	}
	return result
}

func signedPlistValue(t *testing.T, appPath, key string) string {
	t.Helper()
	infoPath := filepath.Join(appPath, "Contents", "Info.plist")
	output, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", infoPath).CombinedOutput()
	if err != nil {
		t.Fatalf("read signed application %s: %v\n%s", key, err, output)
	}
	value := strings.TrimSpace(string(output))
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		t.Fatalf("signed application %s is invalid", key)
	}
	return value
}

func requireSignedEnvironmentPath(t *testing.T, name, suffix string) string {
	t.Helper()
	value := os.Getenv(name)
	if err := validateSignedAbsolutePath(value, name); err != nil ||
		(suffix != "" && !strings.HasSuffix(value, suffix)) {
		t.Fatalf("%s must be an exact absolute %s path: %v", name, suffix, err)
	}
	return value
}

func requireSignedDigestEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if !management.CanonicalDigest(value) {
		t.Fatalf("%s must be a canonical SHA-256 identity", name)
	}
	return value
}

func randomSignedDigest(t *testing.T) string {
	t.Helper()
	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func mustSignedRegularFileDigest(t *testing.T, path, name string) string {
	t.Helper()
	digest, err := signedRegularFileDigest(path, name)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func mustSignedBundleDigest(t *testing.T, path string) string {
	t.Helper()
	digest, err := signedBundleDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func signedCompanionSensitiveValues(t *testing.T) []string {
	t.Helper()
	values := make([]string, 0, 8)
	for _, name := range []string{
		"OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"OPENREALTIME_MANAGEMENT_TOKEN", "OPENREALTIME_GATEWAY_TOKEN",
	} {
		if value := os.Getenv(name); value != "" {
			if len(value) < 8 {
				t.Fatalf("%s is too short for safe signed-receipt scanning", name)
			}
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func redactSignedOutput(payload []byte, secrets []string) string {
	result := append([]byte(nil), payload...)
	for _, secret := range secrets {
		result = bytes.ReplaceAll(result, []byte(secret), []byte("[REDACTED]"))
	}
	return string(result)
}

func signedCompanionEnvironment(source []string, overrides map[string]string) []string {
	result := make([]string, 0, len(source)+len(overrides))
	for _, entry := range source {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+overrides[name])
	}
	return result
}
