package macos

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignedCompanionReceiptRoundTripBindsExactContractAppRunnerServerAndClients(t *testing.T) {
	receipt, contract, expected := fixtureSignedCompanionReceipt(t)
	contractPayload, contractDigest, err := MarshalSignedCompanionRunnerContract(contract)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(contractPayload, []byte{'\n'}) || contractDigest != receipt.ContractDigest {
		t.Fatalf("contract digest=%q receipt=%q", contractDigest, receipt.ContractDigest)
	}
	payload, digest, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || !bytes.HasSuffix(payload, []byte{'\n'}) {
		t.Fatalf("canonical receipt digest=%q payload=%q", digest, payload)
	}
	parsed, parsedDigest, err := ParseSignedCompanionReceipt(payload, expected)
	if err != nil {
		t.Fatal(err)
	}
	if parsedDigest != digest || parsed.Server.GraphFingerprint != receipt.Server.GraphFingerprint ||
		len(parsed.Sessions) != 2 || parsed.Sessions[0].Client != "browser" ||
		parsed.Sessions[1].Client != "macos_native" ||
		parsed.Sessions[0].SessionID == parsed.Sessions[1].SessionID {
		t.Fatalf("parsed signed companion receipt = %#v digest=%s", parsed, parsedDigest)
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "signed-companion.receipt.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, openedDigest, err := ReadSignedCompanionReceipt(path, expected)
	if err != nil {
		t.Fatal(err)
	}
	if openedDigest != digest || opened.InvocationNonce != expected.InvocationNonce {
		t.Fatalf("opened signed companion receipt = %#v digest=%s", opened, openedDigest)
	}
}

func TestSignedCompanionReceiptRejectsEveryRequiredAssertionDrift(t *testing.T) {
	mutations := map[string]func(*SignedCompanionReceipt){
		"version":             func(value *SignedCompanionReceipt) { value.FormatVersion++ },
		"schema":              func(value *SignedCompanionReceipt) { value.Schema += ".other" },
		"nonce":               func(value *SignedCompanionReceipt) { value.InvocationNonce = testSignedDigest('9') },
		"contract":            func(value *SignedCompanionReceipt) { value.ContractDigest = "" },
		"runner digest":       func(value *SignedCompanionReceipt) { value.Runner.Digest = "" },
		"app bundle":          func(value *SignedCompanionReceipt) { value.App.BundleDigest = "" },
		"app executable path": func(value *SignedCompanionReceipt) { value.App.ExecutablePath = "/tmp/outside" },
		"codesign identity":   func(value *SignedCompanionReceipt) { value.App.TeamIdentifier = "" },
		"cdhash":              func(value *SignedCompanionReceipt) { value.App.CDHash = strings.ToUpper(value.App.CDHash) },
		"server run":          func(value *SignedCompanionReceipt) { value.Server.RunIdentity = "" },
		"server graph":        func(value *SignedCompanionReceipt) { value.Server.GraphFingerprint = "" },
		"server profile":      func(value *SignedCompanionReceipt) { value.Server.ProfileFingerprint = "" },
		"launch profile":      func(value *SignedCompanionReceipt) { value.Server.LaunchProfileDigest = "" },
		"server process":      func(value *SignedCompanionReceipt) { value.Server.Process.PID = 0 },
		"population":          func(value *SignedCompanionReceipt) { value.Server.PopulationAfter.Completed-- },
		"missing native":      func(value *SignedCompanionReceipt) { value.Sessions = value.Sessions[:1] },
		"duplicate sessions":  func(value *SignedCompanionReceipt) { value.Sessions[1].SessionID = value.Sessions[0].SessionID },
		"session order":       func(value *SignedCompanionReceipt) { value.Sessions[0].Order = 2 },
		"session client":      func(value *SignedCompanionReceipt) { value.Sessions[1].Client = "browser" },
		"session id":          func(value *SignedCompanionReceipt) { value.Sessions[0].SessionID = "" },
		"client plan":         func(value *SignedCompanionReceipt) { value.Sessions[0].ClientPlanFingerprint = "" },
		"transport":           func(value *SignedCompanionReceipt) { value.Sessions[0].Transport = "websocket" },
		"changed graph":       func(value *SignedCompanionReceipt) { value.Sessions[1].ServerGraphFingerprint = testSignedDigest('8') },
		"changed process": func(value *SignedCompanionReceipt) {
			value.Sessions[1].ServerProcessIdentityDigest = testSignedDigest('8')
		},
		"microphone":               func(value *SignedCompanionReceipt) { value.Sessions[0].Media.MicrophoneFramesIn = 0 },
		"assistant audio":          func(value *SignedCompanionReceipt) { value.Sessions[0].Media.AssistantAudioFramesOut = 0 },
		"camera":                   func(value *SignedCompanionReceipt) { value.Sessions[0].Media.CameraFramesIn = 0 },
		"screen":                   func(value *SignedCompanionReceipt) { value.Sessions[0].Media.ScreenFramesIn = 0 },
		"microphone permission":    func(value *SignedCompanionReceipt) { value.Sessions[1].Permissions.MicrophoneGranted = false },
		"camera permission":        func(value *SignedCompanionReceipt) { value.Sessions[1].Permissions.CameraGranted = false },
		"screen permission":        func(value *SignedCompanionReceipt) { value.Sessions[1].Permissions.ScreenRecordingGranted = false },
		"unexpected accessibility": func(value *SignedCompanionReceipt) { value.Sessions[1].Permissions.AccessibilityGranted = true },
		"interruption":             func(value *SignedCompanionReceipt) { value.Sessions[0].Continuation.Interruptions = 0 },
		"continuation call":        func(value *SignedCompanionReceipt) { value.Sessions[0].Continuation.ToolCallsAfterInterruption = 0 },
		"continuation result":      func(value *SignedCompanionReceipt) { value.Sessions[0].Continuation.ToolResultsAfterInterruption++ },
		"tool":                     func(value *SignedCompanionReceipt) { value.Sessions[0].Tool.Results = 0 },
		"inspection":               func(value *SignedCompanionReceipt) { value.Sessions[1].Inspection.Snapshots = 0 },
		"completion":               func(value *SignedCompanionReceipt) { value.Sessions[1].Completed = false },
		"cleanup":                  func(value *SignedCompanionReceipt) { value.Cleanup.ListenersReleased = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			receipt, _, _ := fixtureSignedCompanionReceipt(t)
			mutate(&receipt)
			payload, _, err := MarshalSignedCompanionReceipt(receipt)
			if err == nil {
				_, _, expected := fixtureSignedCompanionReceipt(t)
				if _, _, err = ParseSignedCompanionReceipt(payload, expected); err == nil {
					t.Fatal("drifted signed companion receipt was accepted")
				}
			}
		})
	}
}

func TestSignedCompanionReceiptRefusesReplaySubstitutionUnknownAndDuplicateWire(t *testing.T) {
	receipt, _, expected := fixtureSignedCompanionReceipt(t)
	payload, _, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*SignedCompanionReceiptExpectation){
		"nonce replay":             func(value *SignedCompanionReceiptExpectation) { value.InvocationNonce = testSignedDigest('7') },
		"contract replay":          func(value *SignedCompanionReceiptExpectation) { value.ContractDigest = testSignedDigest('7') },
		"runner substitution":      func(value *SignedCompanionReceiptExpectation) { value.Runner.Digest = testSignedDigest('7') },
		"application substitution": func(value *SignedCompanionReceiptExpectation) { value.App.ExecutableDigest = testSignedDigest('7') },
		"server substitution":      func(value *SignedCompanionReceiptExpectation) { value.Server.GraphFingerprint = testSignedDigest('7') },
		"client substitution": func(value *SignedCompanionReceiptExpectation) {
			value.Clients[0].ClientManifestFingerprint = testSignedDigest('7')
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := cloneSignedExpectation(expected)
			mutate(&changed)
			if _, _, err := ParseSignedCompanionReceipt(payload, changed); err == nil {
				t.Fatal("substituted signed companion expectation was accepted")
			}
		})
	}

	unknown := bytes.Replace(payload, []byte(`{"format_version":1,`),
		[]byte(`{"format_version":1,"unknown":true,`), 1)
	duplicate := bytes.Replace(payload, []byte(`{"format_version":1,`),
		[]byte(`{"format_version":1,"format_version":1,`), 1)
	pretty := append([]byte(" \n"), payload...)
	for name, candidate := range map[string][]byte{
		"unknown": unknown, "duplicate": duplicate, "noncanonical": pretty,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseSignedCompanionReceipt(candidate, expected); err == nil {
				t.Fatal("invalid signed companion wire was accepted")
			}
		})
	}
}

func TestSignedCompanionReceiptCredentialScanCoversEscapedValuesAndRejectsShortSecrets(t *testing.T) {
	receipt, _, expected := fixtureSignedCompanionReceipt(t)
	receipt.App.SigningIdentifier = `runner-credential-<secret>\\"value`
	expected.App = receipt.App
	payload, _, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	expected.SensitiveValues = []string{`credential-<secret>\\"value`}
	if _, _, err := ParseSignedCompanionReceipt(payload, expected); err == nil ||
		!strings.Contains(err.Error(), "credential") {
		t.Fatalf("escaped credential scan error = %v", err)
	}
	expected.SensitiveValues = []string{"short"}
	if _, _, err := ParseSignedCompanionReceipt(payload, expected); err == nil ||
		!strings.Contains(err.Error(), "too short") {
		t.Fatalf("short credential error = %v", err)
	}
}

func TestSignedCompanionRunnerContractAndVerifiedReceiptAreCreateOnly(t *testing.T) {
	receipt, contract, expected := fixtureSignedCompanionReceipt(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	contractPath := filepath.Join(root, "runner-contract.json")
	digest, err := WriteSignedCompanionRunnerContract(contractPath, contract)
	if err != nil || digest != receipt.ContractDigest {
		t.Fatalf("write contract digest=%q error=%v", digest, err)
	}
	if _, err := WriteSignedCompanionRunnerContract(contractPath, contract); err == nil {
		t.Fatal("runner contract was overwritten")
	}
	contractPayload, _, err := MarshalSignedCompanionRunnerContract(contract)
	if err != nil {
		t.Fatal(err)
	}
	verifiedContractPath := filepath.Join(root, "verified-contract.json")
	if err := WriteVerifiedSignedCompanionContract(verifiedContractPath, contractPayload); err != nil {
		t.Fatal(err)
	}
	if err := WriteVerifiedSignedCompanionContract(verifiedContractPath, contractPayload); err == nil {
		t.Fatal("verified contract was overwritten")
	}
	payload, _, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	verifiedPath := filepath.Join(root, "verified-receipt.json")
	if err := WriteVerifiedSignedCompanionReceipt(verifiedPath, payload); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseSignedCompanionReceipt(mustReadFile(t, verifiedPath), expected); err != nil {
		t.Fatal(err)
	}
	if err := WriteVerifiedSignedCompanionReceipt(verifiedPath, payload); err == nil {
		t.Fatal("verified receipt was overwritten")
	}
}

func TestReadSignedCompanionReceiptRejectsPublicModeSymlinkAndExternalHardlink(t *testing.T) {
	receipt, _, expected := fixtureSignedCompanionReceipt(t)
	payload, _, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "receipt.json")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSignedCompanionReceipt(path, expected); err == nil {
		t.Fatal("publicly readable signed companion receipt was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "receipt-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSignedCompanionReceipt(link, expected); err == nil {
		t.Fatal("symlinked signed companion receipt was accepted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSignedCompanionReceipt(path, expected); err == nil {
		t.Fatal("externally hardlinked signed companion receipt was accepted")
	}
}

func TestSignedCompanionExecutableAndBundleDigestsRefuseAliasesAndTrackBytes(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "openrealtime")
	if err := os.WriteFile(executable, []byte("exact executable bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := signedRegularFileDigest(executable, "test executable")
	if err != nil || digest == "" {
		t.Fatalf("executable digest=%q error=%v", digest, err)
	}
	alias := filepath.Join(root, "openrealtime-alias")
	if err := os.Symlink(executable, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := signedRegularFileDigest(alias, "test executable"); err == nil {
		t.Fatal("symlinked executable was accepted")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(executable, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := signedRegularFileDigest(executable, "test executable"); err == nil {
		t.Fatal("hardlinked executable was accepted")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}

	bundle := filepath.Join(root, "OpenRealtime.app")
	resource := filepath.Join(bundle, "Contents", "Resources", "fixture.txt")
	if err := os.MkdirAll(filepath.Dir(resource), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("fixture.txt", filepath.Join(filepath.Dir(resource), "current")); err != nil {
		t.Fatal(err)
	}
	first, err := signedBundleDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := signedBundleDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("bundle digest did not change with exact resource bytes")
	}
	if err := os.Symlink("../../../../outside", filepath.Join(filepath.Dir(resource), "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := signedBundleDigest(bundle); err == nil {
		t.Fatal("bundle symlink escaping its root was accepted")
	}
}

func fixtureSignedCompanionReceipt(t *testing.T) (
	SignedCompanionReceipt, SignedCompanionRunnerContract, SignedCompanionReceiptExpectation,
) {
	t.Helper()
	process := SignedProcessIdentity{
		PID: 4242, StartIdentity: "darwin-start-001",
		ExecutablePath: "/opt/openrealtime/bin/openrealtime", ExecutableDigest: testSignedDigest('4'),
	}
	processDigest, err := DigestSignedProcessIdentity(process)
	if err != nil {
		t.Fatal(err)
	}
	app := SignedAppIdentity{
		BundlePath: "/Applications/OpenRealtime Developer.app", BundleDigest: testSignedDigest('1'),
		ExecutablePath:   "/Applications/OpenRealtime Developer.app/Contents/MacOS/OpenRealtime Developer",
		ExecutableDigest: testSignedDigest('2'), BundleIdentifier: "com.openrealtime.developer",
		SigningIdentifier: "com.openrealtime.developer", TeamIdentifier: "TEAMIDENTIFIER",
		CDHash: strings.Repeat("a", 40), AuthorityChainDigest: testSignedDigest('3'),
		DesignatedRequirementDigest: testSignedDigest('5'),
	}
	runner := SignedRunnerIdentity{Path: "/opt/openrealtime/bin/signed-companion-runner", Digest: testSignedDigest('6')}
	graph, profile := testSignedDigest('a'), testSignedDigest('b')
	server := SignedServerExpectation{
		RunIdentity: testSignedDigest('e'), GraphFingerprint: graph, ProfileFingerprint: profile,
		ExecutablePath: process.ExecutablePath, ExecutableDigest: process.ExecutableDigest,
		LaunchProfilePath:   "/opt/openrealtime/profiles/signed-macos.yaml",
		LaunchProfileDigest: testSignedDigest('9'),
	}
	clients := []SignedClientExpectation{
		{Order: 1, Client: "browser", ClientProfile: "openrealtime.browser.observer-developer-webrtc",
			Transport: "webrtc", ClientPlanFingerprint: testSignedDigest('c'),
			ClientManifestFingerprint: testSignedDigest('d'), ClientImplementationDigest: testSignedDigest('1'),
			TransportImplementationDigest: testSignedDigest('2')},
		{Order: 2, Client: "macos_native", ClientProfile: "openrealtime.macos.observer-developer",
			Transport: "websocket", ClientPlanFingerprint: testSignedDigest('d'),
			ClientManifestFingerprint: testSignedDigest('e'), ClientImplementationDigest: testSignedDigest('2'),
			TransportImplementationDigest: testSignedDigest('3')},
	}
	contract := SignedCompanionRunnerContract{
		FormatVersion: SignedCompanionReceiptFormatVersion, Schema: SignedCompanionRunnerContractSchema,
		InvocationNonce: testSignedDigest('0'), Runner: runner, App: app, Server: server, Clients: clients,
	}
	_, contractDigest, err := MarshalSignedCompanionRunnerContract(contract)
	if err != nil {
		t.Fatal(err)
	}
	receipt := SignedCompanionReceipt{
		FormatVersion: SignedCompanionReceiptFormatVersion, Schema: SignedCompanionReceiptSchema,
		InvocationNonce: contract.InvocationNonce, ContractDigest: contractDigest, Runner: runner, App: app,
		Server: SignedServerEvidence{
			RunIdentity: server.RunIdentity, GraphFingerprint: graph, ProfileFingerprint: profile, Process: process,
			LaunchProfilePath: server.LaunchProfilePath, LaunchProfileDigest: server.LaunchProfileDigest,
			PopulationBefore: SignedSessionPopulation{Started: 7, Completed: 6, Failed: 1},
			PopulationAfter:  SignedSessionPopulation{Started: 9, Completed: 8, Failed: 1},
		},
		Cleanup: SignedCompanionCleanup{
			BrowserExited: true, NativeAppExited: true, PresentationExited: true, ServerExited: true,
			ListenersReleased: true, EndpointFileRemoved: true, MediaReleased: true,
			EvidenceDigest: testSignedDigest('f'),
		},
	}
	for index, client := range clients {
		receipt.Sessions = append(receipt.Sessions, SignedSessionEvidence{
			Order: client.Order, Client: client.Client, SessionID: "session-0" + string(rune('1'+index)),
			ClientProfile: client.ClientProfile, Transport: client.Transport,
			ClientPlanFingerprint:         client.ClientPlanFingerprint,
			ClientManifestFingerprint:     client.ClientManifestFingerprint,
			ClientImplementationDigest:    client.ClientImplementationDigest,
			TransportImplementationDigest: client.TransportImplementationDigest,
			ServerRunIdentity:             server.RunIdentity, ServerGraphFingerprint: graph,
			ServerProfileFingerprint: profile, ServerProcessIdentityDigest: processDigest,
			SessionEvidenceDigest: testSignedDigest(byte('6' + index)),
			Media: SignedMediaAssertion{
				MicrophoneFramesIn: 24, AssistantAudioFramesOut: 25,
				CameraFramesIn: 5, ScreenFramesIn: 6, EvidenceDigest: testSignedDigest(byte('8' + index)),
			},
			Permissions: SignedPermissionAssertion{
				MicrophoneGranted: true, CameraGranted: true, ScreenRecordingGranted: true,
				AccessibilityRequired: client.AccessibilityRequired,
				AccessibilityGranted:  client.AccessibilityRequired,
				EvidenceDigest:        testSignedDigest(byte('7' + index)),
			},
			Continuation: SignedContinuationAssertion{
				Interruptions: 1, ToolCallsAfterInterruption: 1, ToolResultsAfterInterruption: 1,
				EvidenceDigest: testSignedDigest(byte('a' + index)),
			},
			Tool: SignedToolAssertion{Calls: 1, Results: 1, EvidenceDigest: testSignedDigest(byte('3' + index))},
			Inspection: SignedInspectionAssertion{
				Snapshots: 1, GraphFingerprint: graph, EvidenceDigest: testSignedDigest(byte('5' + index)),
			},
			Completed: true,
		})
	}
	expected := SignedCompanionReceiptExpectation{
		InvocationNonce: receipt.InvocationNonce, ContractDigest: contractDigest,
		Runner: runner, App: app, Server: server, Clients: append([]SignedClientExpectation(nil), clients...),
	}
	return receipt, contract, expected
}

func cloneSignedExpectation(source SignedCompanionReceiptExpectation) SignedCompanionReceiptExpectation {
	result := source
	result.Clients = append([]SignedClientExpectation(nil), source.Clients...)
	result.SensitiveValues = append([]string(nil), source.SensitiveValues...)
	return result
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func testSignedDigest(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}
