package macos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/management"
)

const (
	SignedCompanionReceiptFormatVersion uint64 = 1
	SignedCompanionReceiptSchema               = "openrealtime/macos/signed-companion-e2e-receipt/v1"
	SignedCompanionRunnerContractSchema        = "openrealtime/macos/signed-companion-e2e-runner-contract/v1"
	SignedCompanionReceiptMaximumBytes         = 1 << 20
)

// SignedCompanionReceipt is the create-only, machine-readable result required
// from the trusted Darwin runner. It binds the signed application and runner
// identities to two ordered clients traversing one unchanged server process.
// It is deliberately independent of every presentation view implementation.
type SignedCompanionReceipt struct {
	FormatVersion   uint64                  `json:"format_version"`
	Schema          string                  `json:"schema"`
	InvocationNonce string                  `json:"invocation_nonce"`
	ContractDigest  string                  `json:"contract_digest"`
	Runner          SignedRunnerIdentity    `json:"runner"`
	App             SignedAppIdentity       `json:"app"`
	Server          SignedServerEvidence    `json:"server"`
	Sessions        []SignedSessionEvidence `json:"sessions"`
	Cleanup         SignedCompanionCleanup  `json:"cleanup"`
}

type SignedRunnerIdentity struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// SignedAppIdentity combines exact bundle and executable bytes with the
// independently observed code-directory identity. Digests use canonical
// lowercase sha256 values; CDHash retains the platform code-directory hash.
type SignedAppIdentity struct {
	BundlePath                  string `json:"bundle_path"`
	BundleDigest                string `json:"bundle_digest"`
	ExecutablePath              string `json:"executable_path"`
	ExecutableDigest            string `json:"executable_digest"`
	BundleIdentifier            string `json:"bundle_identifier"`
	SigningIdentifier           string `json:"signing_identifier"`
	TeamIdentifier              string `json:"team_identifier"`
	CDHash                      string `json:"cdhash"`
	AuthorityChainDigest        string `json:"authority_chain_digest"`
	DesignatedRequirementDigest string `json:"designated_requirement_digest"`
}

type SignedProcessIdentity struct {
	PID              uint64 `json:"pid"`
	StartIdentity    string `json:"start_identity"`
	ExecutablePath   string `json:"executable_path"`
	ExecutableDigest string `json:"executable_digest"`
}

type SignedSessionPopulation struct {
	Started   uint64 `json:"started"`
	Completed uint64 `json:"completed"`
	Failed    uint64 `json:"failed"`
}

type SignedServerEvidence struct {
	RunIdentity         string                  `json:"run_identity"`
	GraphFingerprint    string                  `json:"graph_fingerprint"`
	ProfileFingerprint  string                  `json:"profile_fingerprint"`
	LaunchProfilePath   string                  `json:"launch_profile_path"`
	LaunchProfileDigest string                  `json:"launch_profile_digest"`
	Process             SignedProcessIdentity   `json:"process"`
	PopulationBefore    SignedSessionPopulation `json:"population_before"`
	PopulationAfter     SignedSessionPopulation `json:"population_after"`
}

// SignedServerExpectation is selected by the Go release gate before the
// trusted runner starts. Dynamic process identifiers and session counts remain
// evidence, while executable, graph, profile, and run identities are inputs.
type SignedServerExpectation struct {
	RunIdentity         string `json:"run_identity"`
	GraphFingerprint    string `json:"graph_fingerprint"`
	ProfileFingerprint  string `json:"profile_fingerprint"`
	ExecutablePath      string `json:"executable_path"`
	ExecutableDigest    string `json:"executable_digest"`
	LaunchProfilePath   string `json:"launch_profile_path"`
	LaunchProfileDigest string `json:"launch_profile_digest"`
}

type SignedMediaAssertion struct {
	MicrophoneFramesIn      uint64 `json:"microphone_frames_in"`
	AssistantAudioFramesOut uint64 `json:"assistant_audio_frames_out"`
	CameraFramesIn          uint64 `json:"camera_frames_in"`
	ScreenFramesIn          uint64 `json:"screen_frames_in"`
	EvidenceDigest          string `json:"evidence_digest"`
}

type SignedPermissionAssertion struct {
	MicrophoneGranted      bool   `json:"microphone_granted"`
	CameraGranted          bool   `json:"camera_granted"`
	ScreenRecordingGranted bool   `json:"screen_recording_granted"`
	AccessibilityRequired  bool   `json:"accessibility_required"`
	AccessibilityGranted   bool   `json:"accessibility_granted"`
	EvidenceDigest         string `json:"evidence_digest"`
}

type SignedContinuationAssertion struct {
	Interruptions                uint64 `json:"interruptions"`
	ToolCallsAfterInterruption   uint64 `json:"tool_calls_after_interruption"`
	ToolResultsAfterInterruption uint64 `json:"tool_results_after_interruption"`
	EvidenceDigest               string `json:"evidence_digest"`
}

type SignedToolAssertion struct {
	Calls          uint64 `json:"calls"`
	Results        uint64 `json:"results"`
	EvidenceDigest string `json:"evidence_digest"`
}

type SignedInspectionAssertion struct {
	Snapshots        uint64 `json:"snapshots"`
	GraphFingerprint string `json:"graph_fingerprint"`
	EvidenceDigest   string `json:"evidence_digest"`
}

type SignedSessionEvidence struct {
	Order                         uint64                      `json:"order"`
	Client                        string                      `json:"client"`
	SessionID                     string                      `json:"session_id"`
	ClientProfile                 string                      `json:"client_profile"`
	Transport                     string                      `json:"transport"`
	ClientPlanFingerprint         string                      `json:"client_plan_fingerprint"`
	ClientManifestFingerprint     string                      `json:"client_manifest_fingerprint"`
	ClientImplementationDigest    string                      `json:"client_implementation_digest"`
	TransportImplementationDigest string                      `json:"transport_implementation_digest"`
	ServerRunIdentity             string                      `json:"server_run_identity"`
	ServerGraphFingerprint        string                      `json:"server_graph_fingerprint"`
	ServerProfileFingerprint      string                      `json:"server_profile_fingerprint"`
	ServerProcessIdentityDigest   string                      `json:"server_process_identity_digest"`
	SessionEvidenceDigest         string                      `json:"session_evidence_digest"`
	Media                         SignedMediaAssertion        `json:"media"`
	Permissions                   SignedPermissionAssertion   `json:"permissions"`
	Continuation                  SignedContinuationAssertion `json:"continuation"`
	Tool                          SignedToolAssertion         `json:"tool"`
	Inspection                    SignedInspectionAssertion   `json:"inspection"`
	Completed                     bool                        `json:"completed"`
}

type SignedCompanionCleanup struct {
	BrowserExited       bool   `json:"browser_exited"`
	NativeAppExited     bool   `json:"native_app_exited"`
	PresentationExited  bool   `json:"presentation_exited"`
	ServerExited        bool   `json:"server_exited"`
	ListenersReleased   bool   `json:"listeners_released"`
	EndpointFileRemoved bool   `json:"endpoint_file_removed"`
	MediaReleased       bool   `json:"media_released"`
	EvidenceDigest      string `json:"evidence_digest"`
}

type SignedCompanionReceiptExpectation struct {
	InvocationNonce string
	ContractDigest  string
	Runner          SignedRunnerIdentity
	App             SignedAppIdentity
	Server          SignedServerExpectation
	Clients         []SignedClientExpectation
	SensitiveValues []string
}

type SignedClientExpectation struct {
	Order                         uint64 `json:"order"`
	Client                        string `json:"client"`
	ClientProfile                 string `json:"client_profile"`
	Transport                     string `json:"transport"`
	ClientPlanFingerprint         string `json:"client_plan_fingerprint"`
	ClientManifestFingerprint     string `json:"client_manifest_fingerprint"`
	ClientImplementationDigest    string `json:"client_implementation_digest"`
	TransportImplementationDigest string `json:"transport_implementation_digest"`
	AccessibilityRequired         bool   `json:"accessibility_required"`
}

type SignedCompanionRunnerContract struct {
	FormatVersion   uint64                    `json:"format_version"`
	Schema          string                    `json:"schema"`
	InvocationNonce string                    `json:"invocation_nonce"`
	Runner          SignedRunnerIdentity      `json:"runner"`
	App             SignedAppIdentity         `json:"app"`
	Server          SignedServerExpectation   `json:"server"`
	Clients         []SignedClientExpectation `json:"clients"`
}

// MarshalSignedCompanionReceipt returns the only accepted wire encoding: one
// compact JSON object followed by a newline. External runners can implement
// this schema in any language; the release gate independently reopens and
// verifies their create-only file.
func MarshalSignedCompanionReceipt(receipt SignedCompanionReceipt) ([]byte, string, error) {
	if err := receipt.Validate(); err != nil {
		return nil, "", err
	}
	payload, err := marshalSignedCanonical(receipt)
	if err != nil {
		return nil, "", err
	}
	return payload, signedReceiptDigest(payload), nil
}

func MarshalSignedCompanionRunnerContract(
	contract SignedCompanionRunnerContract,
) ([]byte, string, error) {
	if err := contract.Validate(); err != nil {
		return nil, "", err
	}
	payload, err := marshalSignedCanonical(contract)
	if err != nil {
		return nil, "", err
	}
	return payload, signedReceiptDigest(payload), nil
}

// WriteSignedCompanionRunnerContract publishes the independently generated
// invocation contract once. Its parent is a gate-owned private temporary
// directory, never a caller-provided receipt location.
func WriteSignedCompanionRunnerContract(
	path string, contract SignedCompanionRunnerContract,
) (string, error) {
	if err := validateSignedAbsolutePath(path, "runner contract"); err != nil {
		return "", err
	}
	payload, digest, err := MarshalSignedCompanionRunnerContract(contract)
	if err != nil {
		return "", err
	}
	file, finish, err := createSignedFile(path, 0o600, true)
	if err != nil {
		return "", fmt.Errorf("create signed companion runner contract: %w", err)
	}
	_, writeErr := file.Write(payload)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	writeErr = errors.Join(writeErr, finish())
	if writeErr != nil {
		return "", fmt.Errorf("write signed companion runner contract: %w", writeErr)
	}
	return digest, nil
}

// WriteVerifiedSignedCompanionReceipt publishes only the exact bytes already
// reopened and verified from the gate-owned runner receipt. Release artifacts
// therefore never use a caller-selected file as behavioral input.
func WriteVerifiedSignedCompanionReceipt(path string, payload []byte) error {
	return writeVerifiedSignedCompanionArtifact(path, payload, "receipt")
}

// WriteVerifiedSignedCompanionContract publishes the exact gate-generated
// invocation contract next to the independently verified runner receipt.
func WriteVerifiedSignedCompanionContract(path string, payload []byte) error {
	return writeVerifiedSignedCompanionArtifact(path, payload, "contract")
}

func writeVerifiedSignedCompanionArtifact(path string, payload []byte, name string) error {
	if err := validateSignedAbsolutePath(path, "verified "+name); err != nil {
		return err
	}
	file, finish, err := createSignedFile(path, 0o400, false)
	if err != nil {
		return fmt.Errorf("create verified signed companion %s: %w", name, err)
	}
	_, writeErr := file.Write(payload)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	writeErr = errors.Join(writeErr, finish())
	if writeErr != nil {
		return fmt.Errorf("write verified signed companion %s: %w", name, writeErr)
	}
	return nil
}

func ParseSignedCompanionReceipt(
	payload []byte, expected SignedCompanionReceiptExpectation,
) (SignedCompanionReceipt, string, error) {
	if len(payload) == 0 || len(payload) > SignedCompanionReceiptMaximumBytes {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt size is invalid")
	}
	if err := validateSignedSensitivePayload(payload, expected.SensitiveValues); err != nil {
		return SignedCompanionReceipt{}, "", err
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: SignedCompanionReceiptMaximumBytes, MaxDepth: 8,
		MaxTokens: 2048, MaxObjectMembers: 40, MaxArrayElements: 2,
		MaxKeyBytes: 64, MaxTotalKeyBytes: 8 << 10, MaxWorkBytes: 4 << 20,
	}); err != nil {
		return SignedCompanionReceipt{}, "", fmt.Errorf("validate signed companion receipt JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var receipt SignedCompanionReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return SignedCompanionReceipt{}, "", fmt.Errorf("decode signed companion receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return SignedCompanionReceipt{}, "", fmt.Errorf("decode signed companion receipt: %w", err)
	}
	canonical, digest, err := MarshalSignedCompanionReceipt(receipt)
	if err != nil {
		return SignedCompanionReceipt{}, "", err
	}
	if !bytes.Equal(payload, canonical) {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt is not canonical")
	}
	if receipt.InvocationNonce != expected.InvocationNonce {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt invocation nonce changed")
	}
	if receipt.ContractDigest != expected.ContractDigest {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt runner contract changed")
	}
	if !reflect.DeepEqual(receipt.Runner, expected.Runner) {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt runner identity changed")
	}
	if !reflect.DeepEqual(receipt.App, expected.App) {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt application identity changed")
	}
	if receipt.Server.expectation() != expected.Server {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt server identity changed")
	}
	if !reflect.DeepEqual(receipt.clientProjection(), expected.Clients) {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt client or transport identity changed")
	}
	return receipt, digest, nil
}

// ReadSignedCompanionReceipt reopens an externally written receipt without
// following a final symlink and refuses replacement between path and handle
// checks. The caller must establish that path did not exist before launching
// the trusted runner.
func ReadSignedCompanionReceipt(
	path string, expected SignedCompanionReceiptExpectation,
) (SignedCompanionReceipt, string, error) {
	if err := validateSignedAbsolutePath(path, "receipt"); err != nil {
		return SignedCompanionReceipt{}, "", err
	}
	parentInfo, err := privateSignedParent(path)
	if err != nil {
		return SignedCompanionReceipt{}, "", err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return SignedCompanionReceipt{}, "", fmt.Errorf("open signed companion receipt parent: %w", err)
	}
	defer root.Close()
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return SignedCompanionReceipt{}, "", fmt.Errorf("inspect signed companion receipt: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&0o077 != 0 || pathInfo.Size() <= 0 ||
		pathInfo.Size() > SignedCompanionReceiptMaximumBytes {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt must be one private bounded regular file")
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return SignedCompanionReceipt{}, "", fmt.Errorf("open signed companion receipt: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(pathInfo, openedInfo) || fileidentity.RequireSingleLink(file) != nil {
		_ = file.Close()
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt path and opened file differ")
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, SignedCompanionReceiptMaximumBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(payload) > SignedCompanionReceiptMaximumBytes {
		return SignedCompanionReceipt{}, "", fmt.Errorf("read signed companion receipt: %w", errors.Join(readErr, closeErr))
	}
	finalInfo, err := os.Lstat(path)
	parentAfter, parentErr := os.Lstat(filepath.Dir(path))
	if err != nil || parentErr != nil || !os.SameFile(pathInfo, finalInfo) ||
		!os.SameFile(parentInfo, parentAfter) || finalInfo.Size() != int64(len(payload)) {
		return SignedCompanionReceipt{}, "", errors.New("signed companion receipt changed while reading")
	}
	return ParseSignedCompanionReceipt(payload, expected)
}

func (receipt SignedCompanionReceipt) Validate() error {
	if receipt.FormatVersion != SignedCompanionReceiptFormatVersion || receipt.Schema != SignedCompanionReceiptSchema {
		return errors.New("signed companion receipt version or schema is invalid")
	}
	if !management.CanonicalDigest(receipt.InvocationNonce) {
		return errors.New("signed companion receipt invocation nonce is invalid")
	}
	if !management.CanonicalDigest(receipt.ContractDigest) {
		return errors.New("signed companion runner contract digest is invalid")
	}
	if err := receipt.Runner.Validate(); err != nil {
		return err
	}
	if err := receipt.App.Validate(); err != nil {
		return err
	}
	if err := receipt.Server.Validate(); err != nil {
		return err
	}
	if len(receipt.Sessions) != 2 {
		return errors.New("signed companion receipt requires exactly browser then native sessions")
	}
	processDigest, err := DigestSignedProcessIdentity(receipt.Server.Process)
	if err != nil {
		return err
	}
	clients := []string{"browser", "macos_native"}
	for index := range receipt.Sessions {
		if err := receipt.Sessions[index].validate(
			uint64(index+1), clients[index], receipt.Server, processDigest,
		); err != nil {
			return fmt.Errorf("signed companion session %d: %w", index+1, err)
		}
	}
	if receipt.Sessions[0].SessionID == receipt.Sessions[1].SessionID {
		return errors.New("signed companion browser and native session IDs must be distinct")
	}
	if err := receipt.Cleanup.Validate(); err != nil {
		return err
	}
	return nil
}

func (contract SignedCompanionRunnerContract) Validate() error {
	if contract.FormatVersion != SignedCompanionReceiptFormatVersion ||
		contract.Schema != SignedCompanionRunnerContractSchema ||
		!management.CanonicalDigest(contract.InvocationNonce) {
		return errors.New("signed companion runner contract identity is invalid")
	}
	if err := contract.Runner.Validate(); err != nil {
		return err
	}
	if err := contract.App.Validate(); err != nil {
		return err
	}
	if err := contract.Server.Validate(); err != nil {
		return err
	}
	return validateSignedClientExpectations(contract.Clients)
}

func (identity SignedRunnerIdentity) Validate() error {
	if err := validateSignedAbsolutePath(identity.Path, "runner"); err != nil {
		return err
	}
	if !management.CanonicalDigest(identity.Digest) {
		return errors.New("signed companion runner digest is invalid")
	}
	return nil
}

func (identity SignedAppIdentity) Validate() error {
	if err := validateSignedAbsolutePath(identity.BundlePath, "application bundle"); err != nil {
		return err
	}
	if filepath.Ext(identity.BundlePath) != ".app" {
		return errors.New("signed companion application bundle must end in .app")
	}
	if err := validateSignedAbsolutePath(identity.ExecutablePath, "application executable"); err != nil {
		return err
	}
	prefix := strings.TrimSuffix(identity.BundlePath, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(identity.ExecutablePath, prefix) {
		return errors.New("signed companion executable is outside the application bundle")
	}
	for name, digest := range map[string]string{
		"bundle": identity.BundleDigest, "executable": identity.ExecutableDigest,
		"authority chain":        identity.AuthorityChainDigest,
		"designated requirement": identity.DesignatedRequirementDigest,
	} {
		if !management.CanonicalDigest(digest) {
			return fmt.Errorf("signed companion %s digest is invalid", name)
		}
	}
	for name, value := range map[string]string{
		"bundle identifier":  identity.BundleIdentifier,
		"signing identifier": identity.SigningIdentifier,
		"team identifier":    identity.TeamIdentifier,
	} {
		if !validSignedToken(value, 256) {
			return fmt.Errorf("signed companion %s is invalid", name)
		}
	}
	if len(identity.CDHash) < 40 || len(identity.CDHash) > 128 || !lowerHex(identity.CDHash) {
		return errors.New("signed companion CDHash is invalid")
	}
	return nil
}

func (identity SignedProcessIdentity) Validate() error {
	if identity.PID == 0 || !validSignedToken(identity.StartIdentity, 256) {
		return errors.New("signed companion server process identity is invalid")
	}
	if err := validateSignedAbsolutePath(identity.ExecutablePath, "server executable"); err != nil {
		return err
	}
	if !management.CanonicalDigest(identity.ExecutableDigest) {
		return errors.New("signed companion server executable digest is invalid")
	}
	return nil
}

func DigestSignedProcessIdentity(identity SignedProcessIdentity) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return signedReceiptDigest(payload), nil
}

func (evidence SignedServerEvidence) Validate() error {
	if err := evidence.expectation().Validate(); err != nil {
		return err
	}
	if err := evidence.Process.Validate(); err != nil {
		return err
	}
	if evidence.PopulationAfter.Started != evidence.PopulationBefore.Started+2 ||
		evidence.PopulationAfter.Completed != evidence.PopulationBefore.Completed+2 ||
		evidence.PopulationAfter.Failed != evidence.PopulationBefore.Failed {
		return errors.New("signed companion server population does not prove exactly two successful sessions")
	}
	return nil
}

func (expectation SignedServerExpectation) Validate() error {
	if !management.CanonicalDigest(expectation.RunIdentity) ||
		!management.CanonicalDigest(expectation.GraphFingerprint) ||
		!management.CanonicalDigest(expectation.ProfileFingerprint) {
		return errors.New("signed companion expected server identity is invalid")
	}
	if err := validateSignedAbsolutePath(expectation.ExecutablePath, "expected server executable"); err != nil {
		return err
	}
	if err := validateSignedAbsolutePath(expectation.LaunchProfilePath, "expected server launch profile"); err != nil {
		return err
	}
	if !management.CanonicalDigest(expectation.ExecutableDigest) ||
		!management.CanonicalDigest(expectation.LaunchProfileDigest) {
		return errors.New("signed companion expected server executable or launch-profile digest is invalid")
	}
	return nil
}

func (evidence SignedServerEvidence) expectation() SignedServerExpectation {
	return SignedServerExpectation{
		RunIdentity: evidence.RunIdentity, GraphFingerprint: evidence.GraphFingerprint,
		ProfileFingerprint:  evidence.ProfileFingerprint,
		ExecutablePath:      evidence.Process.ExecutablePath,
		ExecutableDigest:    evidence.Process.ExecutableDigest,
		LaunchProfilePath:   evidence.LaunchProfilePath,
		LaunchProfileDigest: evidence.LaunchProfileDigest,
	}
}

func (evidence SignedSessionEvidence) validate(
	order uint64, client string, server SignedServerEvidence, processDigest string,
) error {
	if evidence.Order != order || evidence.Client != client || !evidence.Completed {
		return errors.New("ordered completed client identity is invalid")
	}
	if !validSignedToken(evidence.SessionID, 256) || !validSignedToken(evidence.ClientProfile, 256) {
		return errors.New("session or client profile identity is invalid")
	}
	if (client == "browser" && evidence.Transport != "webrtc") ||
		(client == "macos_native" && evidence.Transport != "websocket") {
		return errors.New("session transport identity is invalid")
	}
	for name, digest := range map[string]string{
		"client plan":              evidence.ClientPlanFingerprint,
		"client manifest":          evidence.ClientManifestFingerprint,
		"client implementation":    evidence.ClientImplementationDigest,
		"transport implementation": evidence.TransportImplementationDigest,
		"session evidence":         evidence.SessionEvidenceDigest,
	} {
		if !management.CanonicalDigest(digest) {
			return fmt.Errorf("%s digest is invalid", name)
		}
	}
	if evidence.ServerRunIdentity != server.RunIdentity ||
		evidence.ServerGraphFingerprint != server.GraphFingerprint ||
		evidence.ServerProfileFingerprint != server.ProfileFingerprint ||
		evidence.ServerProcessIdentityDigest != processDigest {
		return errors.New("session is not bound to the unchanged server identity")
	}
	if evidence.Media.MicrophoneFramesIn == 0 || evidence.Media.AssistantAudioFramesOut == 0 ||
		evidence.Media.CameraFramesIn == 0 || evidence.Media.ScreenFramesIn == 0 ||
		!management.CanonicalDigest(evidence.Media.EvidenceDigest) {
		return errors.New("session media assertion is incomplete")
	}
	if !evidence.Permissions.MicrophoneGranted || !evidence.Permissions.CameraGranted ||
		!evidence.Permissions.ScreenRecordingGranted ||
		(evidence.Permissions.AccessibilityRequired && !evidence.Permissions.AccessibilityGranted) ||
		(!evidence.Permissions.AccessibilityRequired && evidence.Permissions.AccessibilityGranted) ||
		!management.CanonicalDigest(evidence.Permissions.EvidenceDigest) {
		return errors.New("session permission-path assertion is incomplete")
	}
	if evidence.Continuation.Interruptions == 0 ||
		evidence.Continuation.ToolCallsAfterInterruption == 0 ||
		evidence.Continuation.ToolResultsAfterInterruption != evidence.Continuation.ToolCallsAfterInterruption ||
		!management.CanonicalDigest(evidence.Continuation.EvidenceDigest) {
		return errors.New("session interruption-to-tool-continuation assertion is incomplete")
	}
	if evidence.Tool.Calls == 0 || evidence.Tool.Results != evidence.Tool.Calls ||
		evidence.Tool.Calls < evidence.Continuation.ToolCallsAfterInterruption ||
		!management.CanonicalDigest(evidence.Tool.EvidenceDigest) {
		return errors.New("session tool assertion is incomplete")
	}
	if evidence.Inspection.Snapshots == 0 ||
		evidence.Inspection.GraphFingerprint != server.GraphFingerprint ||
		!management.CanonicalDigest(evidence.Inspection.EvidenceDigest) {
		return errors.New("session inspection assertion is incomplete")
	}
	return nil
}

func (receipt SignedCompanionReceipt) clientProjection() []SignedClientExpectation {
	result := make([]SignedClientExpectation, 0, len(receipt.Sessions))
	for _, session := range receipt.Sessions {
		result = append(result, SignedClientExpectation{
			Order: session.Order, Client: session.Client, ClientProfile: session.ClientProfile,
			Transport: session.Transport, ClientPlanFingerprint: session.ClientPlanFingerprint,
			ClientManifestFingerprint:     session.ClientManifestFingerprint,
			ClientImplementationDigest:    session.ClientImplementationDigest,
			TransportImplementationDigest: session.TransportImplementationDigest,
			AccessibilityRequired:         session.Permissions.AccessibilityRequired,
		})
	}
	return result
}

func validateSignedClientExpectations(clients []SignedClientExpectation) error {
	if len(clients) != 2 {
		return errors.New("signed companion contract requires exactly two client identities")
	}
	wantClients, wantTransports := []string{"browser", "macos_native"}, []string{"webrtc", "websocket"}
	for index, client := range clients {
		if client.Order != uint64(index+1) || client.Client != wantClients[index] ||
			client.Transport != wantTransports[index] || !validSignedToken(client.ClientProfile, 256) ||
			!management.CanonicalDigest(client.ClientPlanFingerprint) ||
			!management.CanonicalDigest(client.ClientManifestFingerprint) ||
			!management.CanonicalDigest(client.ClientImplementationDigest) ||
			!management.CanonicalDigest(client.TransportImplementationDigest) {
			return fmt.Errorf("signed companion client expectation %d is invalid", index+1)
		}
	}
	return nil
}

func (cleanup SignedCompanionCleanup) Validate() error {
	if !cleanup.BrowserExited || !cleanup.NativeAppExited || !cleanup.PresentationExited ||
		!cleanup.ServerExited || !cleanup.ListenersReleased || !cleanup.EndpointFileRemoved ||
		!cleanup.MediaReleased || !management.CanonicalDigest(cleanup.EvidenceDigest) {
		return errors.New("signed companion cleanup evidence is incomplete")
	}
	return nil
}

func validateSignedAbsolutePath(value, name string) error {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\x00\r\n") || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("signed companion %s path is invalid", name)
	}
	return nil
}

func validSignedToken(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func lowerHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func signedReceiptDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func signedRegularFileDigest(filePath, name string) (string, error) {
	if err := validateSignedAbsolutePath(filePath, name); err != nil {
		return "", err
	}
	visible, err := os.Lstat(filePath)
	if err != nil || !visible.Mode().IsRegular() || visible.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("signed companion %s must be an exact regular file", name)
	}
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return "", fmt.Errorf("open signed companion %s parent: %w", name, err)
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("open signed companion %s: %w", name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(visible, opened) || fileidentity.RequireSingleLink(file) != nil {
		return "", fmt.Errorf("signed companion %s opened identity changed", name)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash signed companion %s: %w", name, err)
	}
	after, statErr := file.Stat()
	visibleAfter, visibleErr := os.Lstat(filePath)
	if statErr != nil || visibleErr != nil || !os.SameFile(opened, after) ||
		!os.SameFile(after, visibleAfter) || opened.Size() != after.Size() ||
		opened.Mode() != after.Mode() || !opened.ModTime().Equal(after.ModTime()) {
		return "", fmt.Errorf("signed companion %s changed while hashing", name)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

// signedBundleDigest binds every ordinary app-bundle path, mode, symlink
// target, and regular-file payload in lexical order. It never follows bundle
// symlinks and rejects links that escape the bundle namespace.
func signedBundleDigest(bundlePath string) (string, error) {
	if err := validateSignedAbsolutePath(bundlePath, "application bundle"); err != nil {
		return "", err
	}
	visible, err := os.Lstat(bundlePath)
	if err != nil || !visible.IsDir() || visible.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("signed companion application bundle is not an exact directory")
	}
	root, err := os.OpenRoot(bundlePath)
	if err != nil {
		return "", fmt.Errorf("open signed companion application bundle: %w", err)
	}
	defer root.Close()
	paths := make([]string, 0, 256)
	if err := fs.WalkDir(root.FS(), ".", func(entryPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		paths = append(paths, entryPath)
		return nil
	}); err != nil {
		return "", fmt.Errorf("inventory signed companion application bundle: %w", err)
	}
	sort.Strings(paths)
	digest := sha256.New()
	for _, entryPath := range paths {
		info, err := root.Lstat(entryPath)
		if err != nil {
			return "", fmt.Errorf("inspect signed companion bundle entry %q: %w", entryPath, err)
		}
		_, _ = fmt.Fprintf(digest, "%s\x00%o\x00%d\x00", entryPath, info.Mode(), info.Size())
		switch {
		case info.IsDir():
			_, _ = io.WriteString(digest, "directory\x00")
		case info.Mode().IsRegular():
			file, err := root.Open(entryPath)
			if err != nil {
				return "", fmt.Errorf("open signed companion bundle entry %q: %w", entryPath, err)
			}
			opened, statErr := file.Stat()
			entryDigest := sha256.New()
			_, copyErr := io.Copy(entryDigest, file)
			after, afterErr := file.Stat()
			closeErr := file.Close()
			visibleAfter, visibleErr := root.Lstat(entryPath)
			if statErr != nil || copyErr != nil || afterErr != nil || closeErr != nil || visibleErr != nil ||
				!os.SameFile(info, opened) || !os.SameFile(opened, after) || !os.SameFile(after, visibleAfter) ||
				opened.Size() != after.Size() || opened.Mode() != after.Mode() ||
				!opened.ModTime().Equal(after.ModTime()) {
				return "", fmt.Errorf("signed companion bundle entry %q changed while hashing", entryPath)
			}
			_, _ = fmt.Fprintf(digest, "regular\x00%x\x00", entryDigest.Sum(nil))
		case info.Mode()&os.ModeSymlink != 0:
			target, err := root.Readlink(entryPath)
			if err != nil || target == "" || path.IsAbs(target) {
				return "", fmt.Errorf("signed companion bundle symlink %q is invalid", entryPath)
			}
			resolved := path.Clean(path.Join(path.Dir(entryPath), target))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return "", fmt.Errorf("signed companion bundle symlink %q escapes the bundle", entryPath)
			}
			_, _ = fmt.Fprintf(digest, "symlink\x00%s\x00", target)
		default:
			return "", fmt.Errorf("signed companion bundle entry %q has unsupported type", entryPath)
		}
	}
	visibleAfter, err := os.Lstat(bundlePath)
	if err != nil || !os.SameFile(visible, visibleAfter) || visible.Mode() != visibleAfter.Mode() {
		return "", errors.New("signed companion application bundle identity changed while hashing")
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func marshalSignedCanonical(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func validateSignedSensitivePayload(payload []byte, sensitive []string) error {
	for _, secret := range sensitive {
		if secret == "" {
			continue
		}
		if len(secret) < 8 {
			return errors.New("signed companion credential is too short for safe evidence scanning")
		}
		encoded, err := marshalSignedCanonical(secret)
		if err != nil {
			return errors.New("encode signed companion credential for scanning")
		}
		encoded = bytes.TrimSuffix(encoded, []byte{'\n'})
		if len(encoded) >= 2 {
			encoded = encoded[1 : len(encoded)-1]
		}
		if bytes.Contains(payload, []byte(secret)) || bytes.Contains(payload, encoded) {
			return errors.New("signed companion evidence contains a credential")
		}
	}
	return nil
}

func privateSignedParent(path string) (os.FileInfo, error) {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect signed companion private parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("signed companion receipt parent must be one private directory")
	}
	return info, nil
}

func createSignedFile(path string, mode os.FileMode, privateParent bool) (
	*os.File, func() error, error,
) {
	if err := validateSignedAbsolutePath(path, "output"); err != nil {
		return nil, nil, err
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 ||
		(privateParent && parentInfo.Mode().Perm()&0o077 != 0) {
		return nil, nil, errors.New("signed companion output parent is invalid")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, fmt.Errorf("open signed companion output parent: %w", err)
	}
	file, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	finish := func() error {
		var result error
		if linkErr := fileidentity.RequireSingleLink(file); linkErr != nil {
			result = errors.Join(result, linkErr)
		}
		opened, statErr := file.Stat()
		visible, visibleErr := os.Lstat(path)
		parentAfter, parentErr := os.Lstat(parent)
		if statErr != nil || visibleErr != nil || parentErr != nil ||
			!os.SameFile(parentInfo, parentAfter) || !os.SameFile(opened, visible) {
			result = errors.Join(result, errors.New("signed companion output identity changed"))
		}
		result = errors.Join(result, file.Close(), root.Close())
		return result
	}
	return file, finish, nil
}
