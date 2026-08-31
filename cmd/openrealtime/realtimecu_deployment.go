package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

const (
	realtimeCULocalDeploymentEnvironment = "OPENREALTIME_CU_LOCAL_DEPLOYMENT"
	realtimeCUDeploymentAttestationV1    = "openrealtime.local-procfs-filesystem-deployment.v1"
	realtimeCUQwenArtifactID             = "hf://Qwen/Qwen3-VL-30B-A3B-Instruct-FP8"
	realtimeCUSenseVoiceArtifactID       = "modelscope://iic/SenseVoiceSmall"
	realtimeCUWhisperArtifactID          = "hf://mobiuslabsgmbh/faster-whisper-large-v3-turbo"
	realtimeCUWhisperServicePathEnv      = "OPENREALTIME_CU_WHISPER_SERVICE_PATH"
	realtimeCUWhisperDependencyRootsEnv  = "OPENREALTIME_CU_WHISPER_DEPENDENCY_ROOTS"
	realtimeCUWhisperServiceArgument     = "/proc/self/fd/3"
	realtimeCUWhisperServiceLogicalID    = "memfd://openrealtime-whisper-service-v2"
	realtimeCUWhisperServiceDigestV2     = "sha256:9187b0f04d47ea36d94b1decf25c32f2df095ad098364c2026e3ce49d8637d4a"
	maximumRealtimeCUDeploymentFiles     = 200_000
	maximumRealtimeCUDeploymentBytes     = int64(128 << 30)
	maximumRealtimeCUBehaviorEnvironment = 128
	maximumRealtimeCUBehaviorEnvBytes    = 64 << 10
)

var realtimeCUWhisperRuntimePackages = []string{
	"81d243bd2c585b0f4821__mypyc", "PIL", "_brotli", "_cffi_backend",
	"av", "brotli", "certifi", "cffi", "chardet", "charset_normalizer",
	"coloredlogs", "ctranslate2", "defusedxml", "dill", "faster_whisper",
	"filelock", "flatbuffers", "flint", "fsspec", "hf_xet",
	"huggingface_hub", "humanfriendly", "idna", "jinja2", "markupsafe",
	"mpmath", "numpy", "onnxruntime", "packaging", "pynvml", "regex",
	"requests", "safetensors", "socks", "sympy", "tokenizers", "torch",
	"torchgen", "tqdm", "transformers", "typing_extensions", "urllib3",
	"yaml",
}

// realtimeCUDeploymentVerifier is the provider-neutral deployment-proof seam.
// Resolve derives identities from the selected live backends; Verify rechecks
// the opaque in-process proof before readiness and every provider factory open.
// No caller-supplied digest is treated as authority.
type realtimeCUDeploymentVerifier interface {
	Resolve(context.Context) (realtimeCUDeploymentIdentities, error)
	Verify(context.Context, realtimeCUDeploymentIdentities) error
}

// realtimeCUScopedDeploymentVerifier lets one application session revalidate
// each selected backend exactly once at its own factory boundary. The generic
// verifier remains the provider-neutral fallback for remote deployment plug-ins
// that only expose one atomic deployment receipt.
type realtimeCUScopedDeploymentVerifier interface {
	VerifyModel(context.Context, inspect.ArtifactIdentity) error
	VerifyObserver(context.Context, inspect.ArtifactIdentity, inspect.ArtifactIdentity) error
}

type realtimeCUBackendAttestor interface {
	Attest(context.Context) (inspect.ArtifactIdentity, any, error)
	Revalidate(context.Context, inspect.ArtifactIdentity, any) error
}

type composedRealtimeCUDeploymentVerifier struct {
	mu        sync.Mutex
	model     realtimeCUBackendAttestor
	asr       realtimeCUBackendAttestor
	resolved  bool
	identity  realtimeCUDeploymentIdentities
	modelSeal any
	asrSeal   any
}

func newComposedRealtimeCUDeploymentVerifier(
	model, asr realtimeCUBackendAttestor,
) (*composedRealtimeCUDeploymentVerifier, error) {
	if nilRealtimeCUDeploymentInterface(model) || nilRealtimeCUDeploymentInterface(asr) {
		return nil, errors.New("Realtime-CU deployment verifier requires model and ASR attestors")
	}
	return &composedRealtimeCUDeploymentVerifier{model: model, asr: asr}, nil
}

func (verifier *composedRealtimeCUDeploymentVerifier) Resolve(
	ctx context.Context,
) (realtimeCUDeploymentIdentities, error) {
	if ctx == nil {
		return realtimeCUDeploymentIdentities{}, errors.New("resolve Realtime-CU deployments: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return realtimeCUDeploymentIdentities{}, cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if verifier.resolved {
		if err := verifier.verifyLocked(ctx, verifier.identity); err != nil {
			return realtimeCUDeploymentIdentities{}, err
		}
		return verifier.identity, nil
	}
	model, modelSeal, err := verifier.model.Attest(ctx)
	if err != nil {
		return realtimeCUDeploymentIdentities{}, fmt.Errorf("attest Realtime-CU model deployment: %w", err)
	}
	asr, asrSeal, err := verifier.asr.Attest(ctx)
	if err != nil {
		return realtimeCUDeploymentIdentities{}, fmt.Errorf("attest Realtime-CU ASR deployment: %w", err)
	}
	identity := realtimeCUDeploymentIdentities{Model: model, ASR: asr, Vision: model}
	if err := identity.validate(); err != nil {
		return realtimeCUDeploymentIdentities{}, err
	}
	verifier.identity = identity
	verifier.modelSeal = modelSeal
	verifier.asrSeal = asrSeal
	verifier.resolved = true
	return identity, nil
}

func (verifier *composedRealtimeCUDeploymentVerifier) Verify(
	ctx context.Context, expected realtimeCUDeploymentIdentities,
) error {
	if ctx == nil {
		return errors.New("verify Realtime-CU deployments: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if !verifier.resolved {
		return errors.New("Realtime-CU deployments were not independently resolved")
	}
	return verifier.verifyLocked(ctx, expected)
}

func (verifier *composedRealtimeCUDeploymentVerifier) VerifyModel(
	ctx context.Context, expected inspect.ArtifactIdentity,
) error {
	if ctx == nil {
		return errors.New("verify Realtime-CU model deployment: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if !verifier.resolved {
		return errors.New("Realtime-CU deployments were not independently resolved")
	}
	if expected != verifier.identity.Model || verifier.identity.Vision != verifier.identity.Model {
		return errors.New("Realtime-CU model deployment identity differs from live attestation")
	}
	return verifier.model.Revalidate(ctx, expected, verifier.modelSeal)
}

func (verifier *composedRealtimeCUDeploymentVerifier) VerifyObserver(
	ctx context.Context, expectedASR, expectedVision inspect.ArtifactIdentity,
) error {
	if ctx == nil {
		return errors.New("verify Realtime-CU observer deployments: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if !verifier.resolved {
		return errors.New("Realtime-CU deployments were not independently resolved")
	}
	if expectedASR != verifier.identity.ASR || expectedVision != verifier.identity.Vision ||
		expectedVision != verifier.identity.Model {
		return errors.New("Realtime-CU observer deployment identities differ from live attestation")
	}
	// The selected application always opens the model plug-in as well as this
	// observer. Its factory boundary revalidates the shared Qwen model/vision
	// listener; this boundary independently revalidates the selected ASR.
	return verifier.asr.Revalidate(ctx, expectedASR, verifier.asrSeal)
}

func (verifier *composedRealtimeCUDeploymentVerifier) verifyLocked(
	ctx context.Context, expected realtimeCUDeploymentIdentities,
) error {
	if !reflect.DeepEqual(expected, verifier.identity) {
		return errors.New("Realtime-CU expected deployment identities differ from live attestation")
	}
	if err := verifier.model.Revalidate(ctx, expected.Model, verifier.modelSeal); err != nil {
		return fmt.Errorf("revalidate Realtime-CU model deployment: %w", err)
	}
	if err := verifier.asr.Revalidate(ctx, expected.ASR, verifier.asrSeal); err != nil {
		return fmt.Errorf("revalidate Realtime-CU ASR deployment: %w", err)
	}
	if expected.Vision != expected.Model {
		return errors.New("Realtime-CU local vision deployment must be the attested model deployment")
	}
	return nil
}

type realtimeCUProcessSnapshot struct {
	PID                   int
	StartTime             string
	Arguments             []string
	Executable            string
	ExecutableHandle      string
	WorkingDir            string
	Environment           map[string]string
	EnvironmentDigest     string
	opaqueEnvironmentSeal string
}

type realtimeCUProcessSource interface {
	Listener(context.Context, int) (realtimeCUProcessSnapshot, error)
}

type realtimeCUBackendMaterial struct {
	ArtifactID       string
	Revision         string
	LoadedDigest     string
	DependencyDigest string
	ServicePath      string
	ServiceDigest    string
	Roots            []realtimeCUMaterialRoot
}

type realtimeCUMaterialRoot struct {
	Label string
	Path  string
}

type realtimeCUBackendAttestorConfig struct {
	Name             string
	Port             int
	ProcessSource    realtimeCUProcessSource
	ValidateProcess  func(realtimeCUProcessSnapshot) error
	Probe            func(context.Context, realtimeCUProcessSnapshot, realtimeCUBackendMaterial) error
	Material         func(context.Context, realtimeCUProcessSnapshot) (realtimeCUBackendMaterial, error)
	afterInitialHash func() error
}

type localRealtimeCUBackendAttestor struct {
	config realtimeCUBackendAttestorConfig
}

type realtimeCUBackendSeal struct {
	Process  realtimeCUProcessSnapshot
	Material realtimeCUBackendMaterial
	Roots    []realtimeCUMaterialRoot
	Files    []realtimeCUFileSeal
}

type realtimeCUFileSeal struct {
	Key            string
	Path           string
	ResolvedPath   string
	Size           int64
	Mode           os.FileMode
	ModifiedNS     int64
	ChangeIdentity string
	identity       os.FileInfo
}

func newLocalRealtimeCUBackendAttestor(
	config realtimeCUBackendAttestorConfig,
) (*localRealtimeCUBackendAttestor, error) {
	if strings.TrimSpace(config.Name) == "" || config.Name != strings.TrimSpace(config.Name) ||
		config.Port <= 0 || config.Port > 65535 || nilRealtimeCUDeploymentInterface(config.ProcessSource) ||
		config.ValidateProcess == nil || config.Probe == nil || config.Material == nil {
		return nil, errors.New("Realtime-CU local backend attestor configuration is incomplete")
	}
	return &localRealtimeCUBackendAttestor{config: config}, nil
}

func nilRealtimeCUDeploymentInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (attestor *localRealtimeCUBackendAttestor) Attest(
	ctx context.Context,
) (inspect.ArtifactIdentity, any, error) {
	if ctx == nil {
		return inspect.ArtifactIdentity{}, nil, errors.New("attest Realtime-CU backend: nil context")
	}
	process, err := attestor.config.ProcessSource.Listener(ctx, attestor.config.Port)
	if err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	if err := attestor.config.ValidateProcess(process); err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	material, err := attestor.config.Material(ctx, process)
	if err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	if err := attestor.config.Probe(ctx, process, material); err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	files, digest, err := hashRealtimeCUDeploymentMaterial(ctx, attestor.config.Name, process, material)
	if err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	if attestor.config.afterInitialHash != nil {
		if err := attestor.config.afterInitialHash(); err != nil {
			return inspect.ArtifactIdentity{}, nil, err
		}
	}
	// A second complete enumeration/hash closes the first pass's whole-tree
	// window. Each individual pass is handle-bound; the exact file set, handle
	// identities, and digest must agree before an identity can escape Resolve.
	verifiedFiles, verifiedDigest, err := hashRealtimeCUDeploymentMaterial(
		ctx, attestor.config.Name, process, material,
	)
	if err != nil || verifiedDigest != digest || !sameRealtimeCUFileSeals(files, verifiedFiles) {
		return inspect.ArtifactIdentity{}, nil, errors.New(
			"Realtime-CU deployment material changed between independent hashes",
		)
	}
	files = verifiedFiles
	revision := material.Revision
	if revision == "" {
		revision = "content-" + strings.TrimPrefix(digest, "sha256:")[:20]
	}
	identity := inspect.ArtifactIdentity{ID: material.ArtifactID, Revision: revision, Digest: digest}
	if err := identity.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	seal := realtimeCUBackendSeal{
		Process: process,
		Material: realtimeCUBackendMaterial{
			ArtifactID: material.ArtifactID, Revision: material.Revision,
			LoadedDigest: material.LoadedDigest, DependencyDigest: material.DependencyDigest,
			ServicePath: material.ServicePath, ServiceDigest: material.ServiceDigest,
			Roots: append([]realtimeCUMaterialRoot(nil), material.Roots...),
		},
		Roots: append([]realtimeCUMaterialRoot(nil), material.Roots...),
		Files: append([]realtimeCUFileSeal(nil), files...),
	}
	if err := attestor.revalidateSeal(ctx, identity, seal); err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	return identity, seal, nil
}

func (attestor *localRealtimeCUBackendAttestor) Revalidate(
	ctx context.Context, expected inspect.ArtifactIdentity, opaque any,
) error {
	if ctx == nil {
		return errors.New("revalidate Realtime-CU backend: nil context")
	}
	seal, ok := opaque.(realtimeCUBackendSeal)
	if !ok {
		return errors.New("Realtime-CU backend attestation seal has the wrong type")
	}
	return attestor.revalidateSeal(ctx, expected, seal)
}

func (attestor *localRealtimeCUBackendAttestor) revalidateSeal(
	ctx context.Context, expected inspect.ArtifactIdentity, seal realtimeCUBackendSeal,
) error {
	process, err := attestor.config.ProcessSource.Listener(ctx, attestor.config.Port)
	if err != nil {
		return err
	}
	if !sameRealtimeCUProcess(process, seal.Process) {
		return errors.New("Realtime-CU backend listener process changed after attestation")
	}
	if err := attestor.config.ValidateProcess(process); err != nil {
		return err
	}
	if err := attestor.config.Probe(ctx, process, seal.Material); err != nil {
		return err
	}
	if err := revalidateRealtimeCUFiles(ctx, seal.Roots, seal.Files); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	return nil
}

func sameRealtimeCUProcess(left, right realtimeCUProcessSnapshot) bool {
	return left.PID == right.PID && left.StartTime == right.StartTime &&
		left.Executable == right.Executable && left.ExecutableHandle == right.ExecutableHandle &&
		left.WorkingDir == right.WorkingDir && left.EnvironmentDigest == right.EnvironmentDigest &&
		left.opaqueEnvironmentSeal == right.opaqueEnvironmentSeal &&
		reflect.DeepEqual(left.Arguments, right.Arguments) &&
		reflect.DeepEqual(left.Environment, right.Environment)
}

func hashRealtimeCUDeploymentMaterial(
	ctx context.Context, name string, process realtimeCUProcessSnapshot,
	material realtimeCUBackendMaterial,
) ([]realtimeCUFileSeal, string, error) {
	if strings.TrimSpace(material.ArtifactID) == "" || len(material.Roots) == 0 {
		return nil, "", errors.New("Realtime-CU backend material is incomplete")
	}
	roots := append([]realtimeCUMaterialRoot(nil), material.Roots...)
	sort.Slice(roots, func(left, right int) bool { return roots[left].Label < roots[right].Label })
	for index, root := range roots {
		if err := validateRealtimeCUMaterialRoot(root); err != nil {
			return nil, "", err
		}
		if index > 0 && roots[index-1].Label == root.Label {
			return nil, "", fmt.Errorf("Realtime-CU deployment material label %q is duplicated", root.Label)
		}
	}
	hasher := sha256.New()
	writeDeploymentDigestField(hasher, "format", realtimeCUDeploymentAttestationV1)
	writeDeploymentDigestField(hasher, "name", name)
	writeDeploymentDigestField(hasher, "artifact", material.ArtifactID)
	writeDeploymentDigestField(hasher, "revision", material.Revision)
	writeDeploymentDigestField(hasher, "loaded_digest", material.LoadedDigest)
	writeDeploymentDigestField(hasher, "dependency_digest", material.DependencyDigest)
	writeDeploymentDigestField(hasher, "service_path", material.ServicePath)
	writeDeploymentDigestField(hasher, "service_digest", material.ServiceDigest)
	writeDeploymentDigestField(hasher, "arguments", strings.Join(process.Arguments, "\x00"))
	writeDeploymentDigestField(hasher, "executable", process.Executable)
	writeDeploymentDigestField(hasher, "working_directory", process.WorkingDir)
	writeDeploymentDigestField(hasher, "environment_digest", process.EnvironmentDigest)
	environmentNames := make([]string, 0, len(process.Environment))
	for key := range process.Environment {
		environmentNames = append(environmentNames, key)
	}
	sort.Strings(environmentNames)
	for _, key := range environmentNames {
		writeDeploymentDigestField(hasher, "environment:"+key, process.Environment[key])
	}
	var files []realtimeCUFileSeal
	var totalBytes int64
	for _, root := range roots {
		current, err := enumerateRealtimeCUMaterialRoot(ctx, root)
		if err != nil {
			return nil, "", err
		}
		if len(files)+len(current) > maximumRealtimeCUDeploymentFiles {
			return nil, "", errors.New("Realtime-CU deployment material has too many files")
		}
		for _, file := range current {
			if file.Size > maximumRealtimeCUDeploymentBytes-totalBytes {
				return nil, "", errors.New("Realtime-CU deployment material exceeds the byte limit")
			}
			totalBytes += file.Size
			digest, err := hashRealtimeCUFile(ctx, file)
			if err != nil {
				return nil, "", err
			}
			writeDeploymentDigestField(hasher, "file", file.Key)
			writeDeploymentDigestField(hasher, "size", strconv.FormatInt(file.Size, 10))
			writeDeploymentDigestField(hasher, "sha256", digest)
			files = append(files, file)
		}
	}
	return files, "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateRealtimeCUMaterialRoot(root realtimeCUMaterialRoot) error {
	if strings.TrimSpace(root.Label) == "" || root.Label != strings.TrimSpace(root.Label) ||
		strings.ContainsAny(root.Label, "/\\\x00\r\n") ||
		!filepath.IsAbs(root.Path) || filepath.Clean(root.Path) != root.Path {
		return errors.New("Realtime-CU deployment material root is invalid")
	}
	return nil
}

func writeDeploymentDigestField(writer io.Writer, name, value string) {
	_, _ = io.WriteString(writer, strconv.Itoa(len(name))+":"+name+strconv.Itoa(len(value))+":"+value)
}

func enumerateRealtimeCUMaterialRoot(
	ctx context.Context, root realtimeCUMaterialRoot,
) ([]realtimeCUFileSeal, error) {
	info, err := os.Lstat(root.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect Realtime-CU deployment material %s: %w", root.Label, err)
	}
	var paths []string
	if info.IsDir() {
		err = filepath.WalkDir(root.Path, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if entry.IsDir() {
				if entry.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".pyc") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("enumerate Realtime-CU deployment material %s: %w", root.Label, err)
		}
	} else {
		paths = []string{root.Path}
	}
	sort.Strings(paths)
	files := make([]realtimeCUFileSeal, 0, len(paths))
	for _, path := range paths {
		relative := filepath.Base(path)
		if info.IsDir() {
			relative, err = filepath.Rel(root.Path, path)
			if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, errors.New("Realtime-CU deployment file escaped its material root")
			}
		}
		seal, err := snapshotRealtimeCUFile(root.Label+"/"+filepath.ToSlash(relative), path)
		if err != nil {
			return nil, err
		}
		files = append(files, seal)
	}
	if len(files) == 0 {
		return nil, errors.New("Realtime-CU deployment material root is empty")
	}
	return files, nil
}

func snapshotRealtimeCUFile(key, path string) (realtimeCUFileSeal, error) {
	before, err := os.Lstat(path)
	if err != nil || before.IsDir() {
		return realtimeCUFileSeal{}, errors.New("Realtime-CU deployment material is not a file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(resolved) {
		return realtimeCUFileSeal{}, errors.New("resolve Realtime-CU deployment material")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return realtimeCUFileSeal{}, errors.New("Realtime-CU deployment material is not regular")
	}
	identity, err := realtimeCUFileChangeIdentity(info)
	if err != nil {
		return realtimeCUFileSeal{}, err
	}
	return realtimeCUFileSeal{
		Key: key, Path: path, ResolvedPath: resolved, Size: info.Size(), Mode: info.Mode(),
		ModifiedNS: info.ModTime().UnixNano(), ChangeIdentity: identity, identity: info,
	}, nil
}

func hashRealtimeCUFile(ctx context.Context, expected realtimeCUFileSeal) (string, error) {
	return hashRealtimeCUFileWithOpen(ctx, expected, os.Open)
}

func digestRealtimeCUOpenRegularFile(
	ctx context.Context, path string, maximumBytes int64,
) (string, error) {
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || maximumBytes <= 0 {
		return "", errors.New("digest Realtime-CU open file: invalid input")
	}
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maximumBytes {
		return "", errors.New("digest Realtime-CU open file: invalid file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("digest Realtime-CU open file: open")
	}
	hasher := sha256.New()
	copied, copyErr := io.Copy(hasher, &contextReader{ctx: ctx, reader: io.LimitReader(file, maximumBytes+1)})
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := os.Stat(path)
	if copyErr != nil || statErr != nil || closeErr != nil || afterErr != nil ||
		copied != before.Size() || opened.Size() != before.Size() || after.Size() != before.Size() ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) {
		return "", errors.New("digest Realtime-CU open file: changed while hashing")
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func hashRealtimeCUFileWithOpen(
	ctx context.Context, expected realtimeCUFileSeal,
	open func(string) (*os.File, error),
) (string, error) {
	if open == nil {
		return "", errors.New("open Realtime-CU deployment material: nil opener")
	}
	file, err := open(expected.ResolvedPath)
	if err != nil {
		return "", errors.New("open Realtime-CU deployment material")
	}
	hasher := sha256.New()
	copied, copyErr := io.Copy(hasher, &contextReader{ctx: ctx, reader: io.LimitReader(file, expected.Size+1)})
	opened, statErr := file.Stat()
	openedSeal, openedSealErr := realtimeCUFileSealFromInfo(expected, opened)
	closeErr := file.Close()
	after, afterErr := snapshotRealtimeCUFile(expected.Key, expected.Path)
	if copyErr != nil || statErr != nil || openedSealErr != nil || closeErr != nil || afterErr != nil ||
		copied != expected.Size || opened.Size() != expected.Size || !sameRealtimeCUFileSeal(expected, after) {
		return "", errors.New("Realtime-CU deployment material changed while hashing")
	}
	if !sameRealtimeCUFileSeal(expected, openedSeal) {
		return "", errors.New("Realtime-CU deployment hash handle differs from the sealed file")
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func realtimeCUFileSealFromInfo(
	expected realtimeCUFileSeal, info os.FileInfo,
) (realtimeCUFileSeal, error) {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return realtimeCUFileSeal{}, errors.New("Realtime-CU deployment hash handle is not regular")
	}
	identity, err := realtimeCUFileChangeIdentity(info)
	if err != nil {
		return realtimeCUFileSeal{}, err
	}
	return realtimeCUFileSeal{
		Key: expected.Key, Path: expected.Path, ResolvedPath: expected.ResolvedPath,
		Size: info.Size(), Mode: info.Mode(), ModifiedNS: info.ModTime().UnixNano(),
		ChangeIdentity: identity, identity: info,
	}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if cause := context.Cause(reader.ctx); cause != nil {
		return 0, cause
	}
	return reader.reader.Read(buffer)
}

func revalidateRealtimeCUFiles(
	ctx context.Context, roots []realtimeCUMaterialRoot, expected []realtimeCUFileSeal,
) error {
	canonicalRoots := append([]realtimeCUMaterialRoot(nil), roots...)
	sort.Slice(canonicalRoots, func(left, right int) bool {
		return canonicalRoots[left].Label < canonicalRoots[right].Label
	})
	var actual []realtimeCUFileSeal
	for index, root := range canonicalRoots {
		if err := validateRealtimeCUMaterialRoot(root); err != nil {
			return err
		}
		if index > 0 && canonicalRoots[index-1].Label == root.Label {
			return errors.New("Realtime-CU deployment material labels changed after attestation")
		}
		files, err := enumerateRealtimeCUMaterialRoot(ctx, root)
		if err != nil {
			return err
		}
		if len(actual)+len(files) > maximumRealtimeCUDeploymentFiles {
			return errors.New("Realtime-CU deployment material has too many files")
		}
		actual = append(actual, files...)
	}
	if len(actual) != len(expected) {
		return errors.New("Realtime-CU deployment material file set changed after attestation")
	}
	for index, file := range expected {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if !sameRealtimeCUFileSeal(file, actual[index]) {
			return errors.New("Realtime-CU deployment material changed after attestation")
		}
		second, err := snapshotRealtimeCUFile(file.Key, file.Path)
		if err != nil || !sameRealtimeCUFileSeal(file, second) {
			return errors.New("Realtime-CU deployment material changed after attestation")
		}
	}
	return nil
}

func sameRealtimeCUFileSeal(left, right realtimeCUFileSeal) bool {
	return left.Key == right.Key && left.Path == right.Path && left.ResolvedPath == right.ResolvedPath &&
		left.Size == right.Size && left.Mode == right.Mode && left.ModifiedNS == right.ModifiedNS &&
		left.ChangeIdentity == right.ChangeIdentity && left.identity != nil && right.identity != nil &&
		os.SameFile(left.identity, right.identity)
}

func sameRealtimeCUFileSeals(left, right []realtimeCUFileSeal) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameRealtimeCUFileSeal(left[index], right[index]) {
			return false
		}
	}
	return true
}

type procRealtimeCUProcessSource struct{ root string }

func (source procRealtimeCUProcessSource) Listener(
	ctx context.Context, port int,
) (realtimeCUProcessSnapshot, error) {
	root := source.root
	if root == "" {
		root = "/proc"
	}
	inode, err := realtimeCUListenerInode(filepath.Join(root, "net", "tcp"), port)
	if err != nil {
		return realtimeCUProcessSnapshot{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return realtimeCUProcessSnapshot{}, errors.New("list procfs processes")
	}
	var matched []int
	for _, entry := range entries {
		if cause := context.Cause(ctx); cause != nil {
			return realtimeCUProcessSnapshot{}, cause
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(root, entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(root, entry.Name(), "fd", fd.Name()))
			if err == nil && target == "socket:["+inode+"]" {
				matched = append(matched, pid)
				break
			}
		}
	}
	if len(matched) != 1 {
		return realtimeCUProcessSnapshot{}, fmt.Errorf(
			"Realtime-CU port %d belongs to %d listener processes", port, len(matched),
		)
	}
	return snapshotRealtimeCUProcess(root, matched[0])
}

func realtimeCUListenerInode(path string, port int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open procfs TCP listeners")
	}
	defer file.Close()
	wantPort := strings.ToUpper(fmt.Sprintf("%04X", port))
	var inodes []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		address, found := strings.CutSuffix(fields[1], ":"+wantPort)
		if !found || address != "0100007F" {
			continue
		}
		inodes = append(inodes, fields[9])
	}
	if err := scanner.Err(); err != nil || len(inodes) != 1 {
		return "", fmt.Errorf("Realtime-CU port %d has %d exact loopback listeners", port, len(inodes))
	}
	return inodes[0], nil
}

func snapshotRealtimeCUProcess(root string, pid int) (realtimeCUProcessSnapshot, error) {
	prefix := filepath.Join(root, strconv.Itoa(pid))
	cmdline, err := os.ReadFile(filepath.Join(prefix, "cmdline"))
	if err != nil || len(cmdline) == 0 || len(cmdline) > 1<<20 {
		return realtimeCUProcessSnapshot{}, errors.New("read Realtime-CU listener command line")
	}
	arguments := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	for _, argument := range arguments {
		if argument == "" || strings.ContainsAny(argument, "\r\n") {
			return realtimeCUProcessSnapshot{}, errors.New("Realtime-CU listener command line is invalid")
		}
	}
	stat, err := os.ReadFile(filepath.Join(prefix, "stat"))
	if err != nil || len(stat) > 1<<20 {
		return realtimeCUProcessSnapshot{}, errors.New("read Realtime-CU listener start identity")
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return realtimeCUProcessSnapshot{}, errors.New("parse Realtime-CU listener start identity")
	}
	remaining := strings.Fields(strings.TrimSpace(string(stat)[closeParen+1:]))
	if len(remaining) <= 19 {
		return realtimeCUProcessSnapshot{}, errors.New("parse Realtime-CU listener start identity")
	}
	executable, err := os.Readlink(filepath.Join(prefix, "exe"))
	if err != nil {
		return realtimeCUProcessSnapshot{}, errors.New("read Realtime-CU listener executable")
	}
	workingDir, err := os.Readlink(filepath.Join(prefix, "cwd"))
	if err != nil {
		return realtimeCUProcessSnapshot{}, errors.New("read Realtime-CU listener working directory")
	}
	environmentPayload, err := os.ReadFile(filepath.Join(prefix, "environ"))
	if err != nil || len(environmentPayload) > 4<<20 {
		return realtimeCUProcessSnapshot{}, errors.New("read Realtime-CU listener environment")
	}
	environmentDigest, err := publicRealtimeCUEnvironmentDigest(environmentPayload)
	if err != nil {
		return realtimeCUProcessSnapshot{}, err
	}
	opaqueEnvironmentSum := sha256.Sum256(environmentPayload)
	opaqueEnvironmentSeal := "sha256:" + hex.EncodeToString(opaqueEnvironmentSum[:])
	environment := make(map[string]string)
	environmentBytes := 0
	for _, entry := range strings.Split(string(environmentPayload), "\x00") {
		name, value, found := strings.Cut(entry, "=")
		if !found || !retainRealtimeCUBehaviorEnvironment(name) {
			continue
		}
		if len(name) > 128 || len(value) > 4096 || len(environment) >= maximumRealtimeCUBehaviorEnvironment ||
			environmentBytes > maximumRealtimeCUBehaviorEnvBytes-len(name)-len(value) {
			return realtimeCUProcessSnapshot{}, errors.New("Realtime-CU listener behavior environment exceeds bounds")
		}
		environment[name] = value
		environmentBytes += len(name) + len(value)
	}
	return realtimeCUProcessSnapshot{
		PID: pid, StartTime: remaining[19], Arguments: arguments,
		Executable: executable, ExecutableHandle: filepath.Join(prefix, "exe"),
		WorkingDir: workingDir, Environment: environment, EnvironmentDigest: environmentDigest,
		opaqueEnvironmentSeal: opaqueEnvironmentSeal,
	}, nil
}

func publicRealtimeCUEnvironmentDigest(payload []byte) (string, error) {
	values := make(map[string]string)
	for _, entry := range strings.Split(string(payload), "\x00") {
		if entry == "" {
			continue
		}
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.ContainsAny(name, "=\x00\r\n") {
			return "", errors.New("Realtime-CU listener environment is malformed")
		}
		if _, duplicate := values[name]; duplicate {
			return "", errors.New("Realtime-CU listener environment repeats a variable")
		}
		if realtimeCUEnvironmentNameSensitive(name) {
			value = "<sensitive-value-elided>"
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	hasher := sha256.New()
	writeDeploymentDigestField(hasher, "format", "openrealtime.public-environment.v1")
	for _, name := range names {
		writeDeploymentDigestField(hasher, name, values[name])
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func realtimeCUEnvironmentNameSensitive(name string) bool {
	upper := strings.ToUpper(name)
	for _, sensitive := range []string{
		"TOKEN", "KEY", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH", "COOKIE",
	} {
		if strings.Contains(upper, sensitive) {
			return true
		}
	}
	return false
}

func retainRealtimeCUBehaviorEnvironment(name string) bool {
	if realtimeCUEnvironmentNameSensitive(name) {
		return false
	}
	if name == "HOME" || name == "PYTHONHOME" || name == "PYTHONPATH" ||
		name == "PYTHONNOUSERSITE" || name == "VIRTUAL_ENV" ||
		name == "HF_HOME" || name == "HUGGINGFACE_HUB_CACHE" ||
		name == "MODELSCOPE_CACHE" || strings.HasPrefix(name, "SENSEVOICE_") ||
		name == "CUDA_VISIBLE_DEVICES" || name == "CUDA_DEVICE_ORDER" ||
		name == "TOKENIZERS_PARALLELISM" || name == "OMP_NUM_THREADS" ||
		name == "MKL_NUM_THREADS" {
		return true
	}
	for _, prefix := range []string{
		"VLLM_", "NCCL_", "TORCH_", "PYTORCH_", "TRANSFORMERS_", "FLASHINFER_", "XFORMERS_", "RAY_",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func newLocalRealtimeCUDeploymentVerifier() (realtimeCUDeploymentVerifier, error) {
	processes := procRealtimeCUProcessSource{}
	client := &http.Client{Timeout: 15 * time.Second}
	whisperService, err := configuredRealtimeCUWhisperService(context.Background())
	if err != nil {
		return nil, err
	}
	whisperDependencyRoots, err := configuredRealtimeCUWhisperDependencyRoots()
	if err != nil {
		return nil, err
	}
	model, err := newLocalRealtimeCUBackendAttestor(realtimeCUBackendAttestorConfig{
		Name: "qwen-fast-model-and-vision", Port: 8000, ProcessSource: processes,
		ValidateProcess: validateRealtimeCUQwenProcess,
		Probe: func(ctx context.Context, _ realtimeCUProcessSnapshot, material realtimeCUBackendMaterial) error {
			return probeRealtimeCUQwen(ctx, client, material)
		},
		Material: realtimeCUQwenMaterial,
	})
	if err != nil {
		return nil, err
	}
	asr, err := newLocalRealtimeCUBackendAttestor(realtimeCUBackendAttestorConfig{
		Name: "whisper-large-v3-turbo-asr", Port: 8003, ProcessSource: processes,
		ValidateProcess: func(process realtimeCUProcessSnapshot) error {
			return validateRealtimeCUWhisperProcess(process, whisperService, whisperDependencyRoots)
		},
		Probe: func(ctx context.Context, process realtimeCUProcessSnapshot, material realtimeCUBackendMaterial) error {
			return probeRealtimeCUWhisper(ctx, client, process, material)
		},
		Material: func(ctx context.Context, process realtimeCUProcessSnapshot) (realtimeCUBackendMaterial, error) {
			return realtimeCUWhisperMaterial(ctx, process, whisperService, whisperDependencyRoots)
		},
	})
	if err != nil {
		return nil, err
	}
	return newComposedRealtimeCUDeploymentVerifier(model, asr)
}

func validateRealtimeCUQwenProcess(process realtimeCUProcessSnapshot) error {
	modelPath, err := exactRealtimeCUProcessArgument(process.Arguments, "--model")
	if err != nil {
		return errors.New("Qwen listener requires one exact immutable local model path")
	}
	revision, err := realtimeCUQwenSnapshotRevision(modelPath)
	if err != nil || !validRealtimeCURevision(revision) {
		return errors.New("Qwen listener requires one exact immutable local model snapshot")
	}
	if err := requireRealtimeCUProcessArguments(process.Arguments, map[string]string{
		"--model": modelPath, "--served-model-name": "qwen-fast",
		"--host": "127.0.0.1", "--port": "8000", "--max-model-len": "40960",
	}, []string{"-m", "vllm.entrypoints.openai.api_server"}); err != nil {
		return err
	}
	for _, argument := range process.Arguments {
		if argument == "--revision" || strings.HasPrefix(argument, "--revision=") {
			return errors.New("Qwen local snapshot selection must not include a separately mutable revision")
		}
	}
	return nil
}

func validateRealtimeCUSenseVoiceProcess(process realtimeCUProcessSnapshot) error {
	if value := process.Environment["SENSEVOICE_MODEL"]; value != "" && value != realtimeCULocalASRModel {
		return errors.New("SenseVoice listener selects a different model")
	}
	if _, err := canonicalRealtimeCUDeploymentPath(process.Environment["SENSEVOICE_MODEL_PATH"]); err != nil {
		return errors.New("SenseVoice listener requires an exact local model path")
	}
	for _, argument := range process.Arguments {
		if argument == "--app-dir" || strings.HasPrefix(argument, "--app-dir=") ||
			argument == "--factory" || argument == "--reload" {
			return errors.New("SenseVoice listener uses an unreviewed module-loading mode")
		}
	}
	return requireRealtimeCUProcessArguments(process.Arguments, map[string]string{
		"--host": "127.0.0.1", "--port": "8002", "--workers": "1",
	}, []string{"-m", "uvicorn", "server:app"})
}

func configuredRealtimeCUWhisperService(ctx context.Context) (string, error) {
	path, err := canonicalRealtimeCUDeploymentPath(
		strings.TrimSpace(os.Getenv(realtimeCUWhisperServicePathEnv)),
	)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", realtimeCUWhisperServicePathEnv, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || filepath.Base(path) != "server.py" ||
		filepath.Base(filepath.Dir(path)) != "whisper" {
		return "", errors.New("Realtime-CU Whisper service path is not the exact reviewed module")
	}
	seal, err := snapshotRealtimeCUFile("service/server.py", path)
	if err != nil {
		return "", err
	}
	digest, err := hashRealtimeCUFile(ctx, seal)
	if err != nil {
		return "", err
	}
	if digest != realtimeCUWhisperServiceDigestV2 {
		return "", errors.New("Realtime-CU Whisper service differs from the pinned reviewed implementation")
	}
	return path, nil
}

func configuredRealtimeCUWhisperDependencyRoots() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(realtimeCUWhisperDependencyRootsEnv))
	if raw == "" {
		return nil, fmt.Errorf("%s is required", realtimeCUWhisperDependencyRootsEnv)
	}
	values := filepath.SplitList(raw)
	if len(values) == 0 || len(values) > 16 {
		return nil, errors.New("Realtime-CU Whisper dependency root selection is invalid")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		canonical, err := canonicalRealtimeCUDeploymentPath(value)
		if err != nil {
			return nil, errors.New("Realtime-CU Whisper dependency root is not canonical and absolute")
		}
		resolved, err := filepath.EvalSymlinks(canonical)
		info, statErr := os.Stat(canonical)
		if err != nil || statErr != nil || resolved != canonical || !info.IsDir() {
			return nil, errors.New("Realtime-CU Whisper dependency root is not an exact directory")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, errors.New("Realtime-CU Whisper dependency root is repeated")
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	return result, nil
}

func validateRealtimeCUWhisperProcess(
	process realtimeCUProcessSnapshot, expectedService string, expectedDependencyRoots []string,
) error {
	if len(process.Arguments) < 5 {
		return errors.New("Whisper listener command is incomplete")
	}
	if !reflect.DeepEqual(process.Arguments[1:5], []string{"-I", "-S", "-B", realtimeCUWhisperServiceArgument}) ||
		expectedService == "" || len(expectedDependencyRoots) == 0 {
		return errors.New("Whisper listener requires the exact sealed service handle")
	}
	if process.Environment["PYTHONPATH"] != "" || process.Environment["PYTHONHOME"] != "" {
		return errors.New("Whisper listener forbids alternate Python module roots")
	}
	modelPath, err := exactRealtimeCUProcessArgument(process.Arguments, "--model")
	if err != nil {
		return errors.New("Whisper listener requires one exact immutable local model path")
	}
	if _, err := realtimeCUWhisperSnapshotRevision(modelPath); err != nil {
		return errors.New("Whisper listener requires one exact immutable local model snapshot")
	}
	want := []string{
		process.Arguments[0], "-I", "-S", "-B", realtimeCUWhisperServiceArgument,
		"--model", modelPath, "--device", "cuda", "--compute-type", "int8",
		"--language", "en",
	}
	for _, root := range expectedDependencyRoots {
		want = append(want, "--dependency-root", root)
	}
	want = append(want, "--port", "8003")
	if !reflect.DeepEqual(process.Arguments, want) {
		return errors.New("Whisper listener command differs from the strict local profile")
	}
	return nil
}

func requireRealtimeCUProcessArguments(
	arguments []string, pairs map[string]string, sequence []string,
) error {
	for flag, want := range pairs {
		matches := 0
		for index := 0; index < len(arguments); index++ {
			if arguments[index] == flag {
				if index+1 >= len(arguments) || arguments[index+1] != want {
					return fmt.Errorf("Realtime-CU listener selects a different %s value", flag)
				}
				matches++
			}
			if strings.HasPrefix(arguments[index], flag+"=") {
				return fmt.Errorf("Realtime-CU listener uses a noncanonical %s argument", flag)
			}
		}
		if matches != 1 {
			return fmt.Errorf("Realtime-CU listener lacks exact %s %s", flag, want)
		}
	}
	sequenceMatches := 0
	for offset := 0; offset+len(sequence) <= len(arguments); offset++ {
		if reflect.DeepEqual(arguments[offset:offset+len(sequence)], sequence) {
			sequenceMatches++
		}
	}
	if sequenceMatches != 1 {
		return errors.New("Realtime-CU listener command does not uniquely match the selected service")
	}
	return nil
}

func exactRealtimeCUProcessArgument(arguments []string, flag string) (string, error) {
	var result string
	for index, argument := range arguments {
		if argument == flag {
			if index+1 >= len(arguments) || result != "" {
				return "", fmt.Errorf("Realtime-CU listener has an ambiguous %s argument", flag)
			}
			result = arguments[index+1]
		} else if strings.HasPrefix(argument, flag+"=") {
			return "", fmt.Errorf("Realtime-CU listener has a noncanonical %s argument", flag)
		}
	}
	if result == "" {
		return "", fmt.Errorf("Realtime-CU listener lacks %s", flag)
	}
	return result, nil
}

func exactRealtimeCUProcessArguments(arguments []string, flag string) ([]string, error) {
	if flag == "" {
		return nil, errors.New("Realtime-CU listener argument flag is empty")
	}
	var result []string
	for index, argument := range arguments {
		if argument == flag {
			if index+1 >= len(arguments) || arguments[index+1] == "" {
				return nil, fmt.Errorf("Realtime-CU listener has an incomplete %s argument", flag)
			}
			result = append(result, arguments[index+1])
		} else if strings.HasPrefix(argument, flag+"=") {
			return nil, fmt.Errorf("Realtime-CU listener has a noncanonical %s argument", flag)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Realtime-CU listener lacks %s", flag)
	}
	return result, nil
}

func validRealtimeCURevision(revision string) bool {
	return len(revision) == 40 && strings.Trim(revision, "0123456789abcdef") == ""
}

func realtimeCUQwenSnapshotRevision(path string) (string, error) {
	canonical, err := canonicalRealtimeCUDeploymentPath(path)
	if err != nil {
		return "", err
	}
	revision := filepath.Base(canonical)
	snapshots := filepath.Dir(canonical)
	repository := filepath.Dir(snapshots)
	if filepath.Base(snapshots) != "snapshots" ||
		filepath.Base(repository) != "models--Qwen--Qwen3-VL-30B-A3B-Instruct-FP8" ||
		!validRealtimeCURevision(revision) {
		return "", errors.New("Qwen local model path is not the immutable selected snapshot")
	}
	return revision, nil
}

func realtimeCUWhisperSnapshotRevision(path string) (string, error) {
	canonical, err := canonicalRealtimeCUDeploymentPath(path)
	if err != nil {
		return "", err
	}
	revision := filepath.Base(canonical)
	snapshots := filepath.Dir(canonical)
	repository := filepath.Dir(snapshots)
	if filepath.Base(snapshots) != "snapshots" ||
		filepath.Base(repository) != "models--mobiuslabsgmbh--faster-whisper-large-v3-turbo" ||
		!validRealtimeCURevision(revision) {
		return "", errors.New("Whisper local model path is not the immutable selected snapshot")
	}
	return revision, nil
}

func realtimeCUQwenMaterial(
	ctx context.Context, process realtimeCUProcessSnapshot,
) (realtimeCUBackendMaterial, error) {
	modelPath, err := exactRealtimeCUProcessArgument(process.Arguments, "--model")
	if err != nil {
		return realtimeCUBackendMaterial{}, errors.New("resolve immutable Qwen listener model path")
	}
	revision, err := realtimeCUQwenSnapshotRevision(modelPath)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	packages, err := realtimeCURuntimePackageRoots(
		ctx, process, []string{"torch", "transformers", "vllm"},
	)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	roots := []realtimeCUMaterialRoot{{Label: "model", Path: modelPath}}
	roots = append(roots, packages...)
	return realtimeCUBackendMaterial{
		ArtifactID: realtimeCUQwenArtifactID, Revision: revision, Roots: roots,
	}, nil
}

func realtimeCUSenseVoiceMaterial(
	ctx context.Context, process realtimeCUProcessSnapshot,
) (realtimeCUBackendMaterial, error) {
	modelPath, err := canonicalRealtimeCUDeploymentPath(process.Environment["SENSEVOICE_MODEL_PATH"])
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	loadedDigest, err := digestRealtimeCULoadedModel(ctx, modelPath)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	revision := "content-" + strings.TrimPrefix(loadedDigest, "sha256:")[:20]
	packages, err := realtimeCURuntimePackageRoots(
		ctx, process, []string{"funasr", "modelscope", "server", "torch"},
	)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	servicePath := ""
	filteredPackages := make([]realtimeCUMaterialRoot, 0, len(packages))
	for _, root := range packages {
		if root.Label == "runtime-server" {
			if servicePath != "" || filepath.Base(root.Path) != "server.py" {
				return realtimeCUBackendMaterial{}, errors.New("SenseVoice listener resolves an ambiguous service module")
			}
			servicePath = root.Path
			continue
		}
		filteredPackages = append(filteredPackages, root)
	}
	if servicePath == "" {
		return realtimeCUBackendMaterial{}, errors.New("SenseVoice listener service module is unresolved")
	}
	serviceSeal, err := snapshotRealtimeCUFile("service/server.py", servicePath)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	serviceDigest, err := hashRealtimeCUFile(ctx, serviceSeal)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	roots := []realtimeCUMaterialRoot{
		{Label: "model", Path: modelPath},
		{Label: "service", Path: servicePath},
	}
	roots = append(roots, filteredPackages...)
	return realtimeCUBackendMaterial{
		ArtifactID: realtimeCUSenseVoiceArtifactID, Revision: revision,
		LoadedDigest: loadedDigest, ServicePath: servicePath, ServiceDigest: serviceDigest,
		Roots: roots,
	}, nil
}

func realtimeCUWhisperMaterial(
	ctx context.Context, process realtimeCUProcessSnapshot, expectedService string,
	expectedDependencyRoots []string,
) (realtimeCUBackendMaterial, error) {
	dependencyRoots, err := exactRealtimeCUProcessArguments(process.Arguments, "--dependency-root")
	if err != nil || !reflect.DeepEqual(dependencyRoots, expectedDependencyRoots) {
		return realtimeCUBackendMaterial{}, errors.New("resolve exact Whisper dependency roots")
	}
	modelPath, err := exactRealtimeCUProcessArgument(process.Arguments, "--model")
	if err != nil {
		return realtimeCUBackendMaterial{}, errors.New("resolve immutable Whisper listener model path")
	}
	revision, err := realtimeCUWhisperSnapshotRevision(modelPath)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	sourceDigest, err := digestRealtimeCULoadedModel(ctx, modelPath)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	materializedPath := filepath.Join(
		"/tmp", fmt.Sprintf("openrealtime-whisper-sealed-%d", process.PID), "model",
	)
	loadedDigest, err := digestRealtimeCULoadedModel(ctx, materializedPath)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	if sourceDigest != loadedDigest {
		return realtimeCUBackendMaterial{}, errors.New(
			"Whisper loaded model material differs from the selected immutable snapshot",
		)
	}
	if len(process.Arguments) < 2 {
		return realtimeCUBackendMaterial{}, errors.New("resolve Whisper listener service module")
	}
	serviceHandle := filepath.Join("/proc", strconv.Itoa(process.PID), "fd", "3")
	if err := verifyRealtimeCUWhisperServiceHandle(serviceHandle); err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	serviceDigest, err := digestRealtimeCUOpenRegularFile(ctx, serviceHandle, 4<<20)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	if serviceDigest != realtimeCUWhisperServiceDigestV2 {
		return realtimeCUBackendMaterial{}, errors.New("Whisper listener service differs from the pinned reviewed implementation")
	}
	packages, err := realtimeCURuntimePackageRoots(
		ctx, process, realtimeCUWhisperRuntimePackages,
	)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	dependencyDigest, err := digestRealtimeCUWhisperDependencies(ctx, packages, expectedDependencyRoots)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	roots := []realtimeCUMaterialRoot{
		{Label: "loaded-model", Path: materializedPath},
		{Label: "model-source", Path: modelPath},
		{Label: "service-source", Path: expectedService},
	}
	roots = append(roots, packages...)
	return realtimeCUBackendMaterial{
		ArtifactID: realtimeCUWhisperArtifactID, Revision: revision,
		LoadedDigest: loadedDigest, DependencyDigest: dependencyDigest,
		ServicePath: realtimeCUWhisperServiceLogicalID, ServiceDigest: serviceDigest,
		Roots: roots,
	}, nil
}

func digestRealtimeCUWhisperDependencies(
	ctx context.Context, roots []realtimeCUMaterialRoot, dependencyRoots []string,
) (string, error) {
	if len(dependencyRoots) == 0 {
		return "", errors.New("Whisper dependency roots are empty")
	}
	for _, root := range dependencyRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return "", errors.New("Whisper dependency root is not canonical and absolute")
		}
		resolved, err := filepath.EvalSymlinks(root)
		info, statErr := os.Stat(root)
		if err != nil || statErr != nil || resolved != root || !info.IsDir() {
			return "", errors.New("Whisper dependency root is not an exact directory")
		}
	}
	byLabel := make(map[string]realtimeCUMaterialRoot, len(roots))
	for _, root := range roots {
		if _, duplicate := byLabel[root.Label]; duplicate {
			return "", errors.New("Whisper dependency material root is duplicated")
		}
		byLabel[root.Label] = root
	}
	hasher := sha256.New()
	writeDeploymentDigestField(hasher, "format", "openrealtime.whisper-dependencies.v1")
	for _, name := range realtimeCUWhisperRuntimePackages {
		label := "runtime-" + name
		root, found := byLabel[label]
		if !found {
			return "", fmt.Errorf("Whisper dependency material %s is unresolved", name)
		}
		if !pathWithinAnyRealtimeCURoot(root.Path, dependencyRoots) {
			return "", errors.New("Whisper dependency module escaped its selected roots")
		}
		files, err := enumerateRealtimeCUMaterialRoot(ctx, root)
		if err != nil {
			return "", err
		}
		writeDeploymentDigestField(hasher, "module", name)
		for _, file := range files {
			if err := validateRealtimeCUWhisperDependencyFile(file, dependencyRoots); err != nil {
				return "", err
			}
			relative, found := strings.CutPrefix(file.Key, label+"/")
			if !found || relative == "" {
				return "", errors.New("Whisper dependency material has an invalid relative path")
			}
			digest, err := hashRealtimeCUFile(ctx, file)
			if err != nil {
				return "", err
			}
			writeDeploymentDigestField(hasher, "file", relative)
			writeDeploymentDigestField(hasher, "size", strconv.FormatInt(file.Size, 10))
			writeDeploymentDigestField(hasher, "sha256", digest)
		}
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateRealtimeCUWhisperDependencyFile(
	expected realtimeCUFileSeal, dependencyRoots []string,
) error {
	if expected.Path != expected.ResolvedPath ||
		!pathWithinAnyRealtimeCURoot(expected.ResolvedPath, dependencyRoots) {
		return errors.New("Whisper dependency file is an alias or escaped its selected roots")
	}
	file, err := os.Open(expected.Path)
	if err != nil {
		return errors.New("open Whisper dependency file")
	}
	linkErr := fileidentity.RequireSingleLink(file)
	info, statErr := file.Stat()
	opened, sealErr := realtimeCUFileSealFromInfo(expected, info)
	closeErr := file.Close()
	after, afterErr := snapshotRealtimeCUFile(expected.Key, expected.Path)
	if linkErr != nil || statErr != nil || sealErr != nil || closeErr != nil || afterErr != nil ||
		!sameRealtimeCUFileSeal(expected, opened) || !sameRealtimeCUFileSeal(expected, after) {
		return errors.New("Whisper dependency file is aliased or changed during validation")
	}
	return nil
}

func pathWithinAnyRealtimeCURoot(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func digestRealtimeCULoadedModel(ctx context.Context, path string) (string, error) {
	root := realtimeCUMaterialRoot{Label: "loaded-model", Path: path}
	if err := validateRealtimeCUMaterialRoot(root); err != nil {
		return "", err
	}
	files, err := enumerateRealtimeCUMaterialRoot(ctx, root)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	writeDeploymentDigestField(hasher, "format", "openrealtime.loaded-model.v1")
	var total int64
	for _, file := range files {
		if file.Size > maximumRealtimeCUDeploymentBytes-total {
			return "", errors.New("Realtime-CU loaded model exceeds the byte limit")
		}
		total += file.Size
		digest, err := hashRealtimeCUFile(ctx, file)
		if err != nil {
			return "", err
		}
		writeDeploymentDigestField(hasher, "file", strings.TrimPrefix(file.Key, "loaded-model/"))
		writeDeploymentDigestField(hasher, "size", strconv.FormatInt(file.Size, 10))
		writeDeploymentDigestField(hasher, "sha256", digest)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func realtimeCUHuggingFaceCache(environment map[string]string) (string, error) {
	if path := strings.TrimSpace(environment["HUGGINGFACE_HUB_CACHE"]); path != "" {
		return canonicalRealtimeCUDeploymentPath(path)
	}
	if path := strings.TrimSpace(environment["HF_HOME"]); path != "" {
		base, err := canonicalRealtimeCUDeploymentPath(path)
		return filepath.Join(base, "hub"), err
	}
	home, err := realtimeCUDeploymentHome(environment)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "huggingface", "hub"), nil
}

func realtimeCUModelScopeCache(environment map[string]string) (string, error) {
	if path := strings.TrimSpace(environment["MODELSCOPE_CACHE"]); path != "" {
		return canonicalRealtimeCUDeploymentPath(path)
	}
	home, err := realtimeCUDeploymentHome(environment)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "modelscope"), nil
}

func realtimeCUDeploymentHome(environment map[string]string) (string, error) {
	home := strings.TrimSpace(environment["HOME"])
	if home == "" {
		return "", errors.New("Realtime-CU listener does not expose its canonical home directory")
	}
	return canonicalRealtimeCUDeploymentPath(home)
}

func canonicalRealtimeCUDeploymentPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("Realtime-CU deployment cache path is not canonical and absolute")
	}
	return path, nil
}

func realtimeCURuntimePackageRoots(
	ctx context.Context, process realtimeCUProcessSnapshot, packages []string,
) ([]realtimeCUMaterialRoot, error) {
	if ctx == nil {
		return nil, errors.New("resolve Realtime-CU backend Python packages: nil context")
	}
	if len(process.Arguments) == 0 {
		return nil, errors.New("Realtime-CU backend Python launcher is missing")
	}
	python := process.Arguments[0]
	if !filepath.IsAbs(python) || filepath.Clean(python) != python {
		return nil, errors.New("Realtime-CU backend Python launcher is not canonical and absolute")
	}
	for _, name := range packages {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) ||
			strings.ContainsAny(name, "/\\.\x00\r\n") {
			return nil, errors.New("Realtime-CU backend Python package name is invalid")
		}
	}
	request, err := json.Marshal(packages)
	if err != nil {
		return nil, err
	}
	searchRoots, err := realtimeCUPythonSearchRoots(process)
	if err != nil {
		return nil, err
	}
	searchRequest, err := json.Marshal(searchRoots)
	if err != nil {
		return nil, err
	}
	program := `
import importlib.machinery, json, pathlib, sys, sysconfig
names = json.loads(sys.argv[1])
roots = json.loads(sys.argv[2])
if not roots:
    raise SystemExit("empty package search roots")
for root in roots:
    path = pathlib.Path(root)
    if not path.is_absolute() or str(path) != str(path.resolve(strict=True)) or not path.is_dir():
        raise SystemExit("invalid package search root")
paths = {}
for name in names:
    spec = importlib.machinery.PathFinder.find_spec(name, roots)
    if spec is None:
        raise SystemExit("missing package: " + name)
    if spec.submodule_search_locations:
        values = list(spec.submodule_search_locations)
        if len(values) != 1:
            raise SystemExit("ambiguous package: " + name)
        value = values[0]
    else:
        value = spec.origin
    paths[name] = str(pathlib.Path(value).resolve(strict=True))
paths["__stdlib__"] = str(pathlib.Path(sysconfig.get_path("stdlib")).resolve(strict=True))
print(json.dumps(paths, sort_keys=True, separators=(",", ":")))
`
	if process.ExecutableHandle == "" || !filepath.IsAbs(process.ExecutableHandle) ||
		filepath.Clean(process.ExecutableHandle) != process.ExecutableHandle {
		return nil, errors.New("Realtime-CU backend procfs executable handle is invalid")
	}
	listenerExecutable, err := os.Stat(process.ExecutableHandle)
	if err != nil || !listenerExecutable.Mode().IsRegular() {
		return nil, errors.New("inspect Realtime-CU backend listener executable handle")
	}
	launcherExecutable, err := os.Stat(python)
	if err != nil || !launcherExecutable.Mode().IsRegular() ||
		!os.SameFile(listenerExecutable, launcherExecutable) {
		return nil, errors.New("Realtime-CU backend launcher path differs from the listener executable")
	}
	command := exec.CommandContext(
		ctx, process.ExecutableHandle, "-I", "-S", "-B", "-c", program,
		string(request), string(searchRequest),
	)
	// Python uses argv[0] to discover the exact virtual environment while Path
	// remains the already-open procfs executable identity of the listener.
	command.Args[0] = python
	command.Dir = process.WorkingDir
	// -I -S prevents PYTHONPATH, user-site, sitecustomize, .pth, and the
	// listener's HOME from influencing this independent resolution. Only the
	// explicit, canonical roots supplied above participate.
	command.Env = []string{"LC_ALL=C.UTF-8"}
	output, err := command.Output()
	if err != nil || len(output) == 0 || len(output) > 1<<20 {
		return nil, errors.New("resolve Realtime-CU backend Python package roots")
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	decoder.DisallowUnknownFields()
	resolved := make(map[string]string)
	if err := decoder.Decode(&resolved); err != nil {
		return nil, errors.New("decode Realtime-CU backend Python package roots")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("Realtime-CU backend Python package roots have trailing output")
	}
	if len(resolved) != len(packages)+1 {
		return nil, errors.New("Realtime-CU backend Python package roots are incomplete")
	}
	// Hash through /proc/PID/exe, not argv[0]. The SameFile check above binds
	// the human-readable launcher path to this live executable inode while the
	// procfs handle remains valid even if that path is replaced after startup.
	result := []realtimeCUMaterialRoot{{Label: "python-executable", Path: process.ExecutableHandle}}
	venvConfig := filepath.Join(filepath.Dir(filepath.Dir(python)), "pyvenv.cfg")
	if info, statErr := os.Lstat(venvConfig); statErr == nil && !info.IsDir() {
		result = append(result, realtimeCUMaterialRoot{Label: "python-venv-config", Path: venvConfig})
	}
	stdlib, found := resolved["__stdlib__"]
	if !found || !filepath.IsAbs(stdlib) || filepath.Clean(stdlib) != stdlib {
		return nil, errors.New("Realtime-CU backend Python standard library is not canonical")
	}
	result = append(result, realtimeCUMaterialRoot{Label: "python-stdlib", Path: stdlib})
	delete(resolved, "__stdlib__")
	for _, name := range packages {
		path, found := resolved[name]
		if !found || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("resolve Realtime-CU runtime package %s", name)
		}
		result = append(result, realtimeCUMaterialRoot{Label: "runtime-" + name, Path: path})
	}
	return result, nil
}

func realtimeCUPythonSearchRoots(process realtimeCUProcessSnapshot) ([]string, error) {
	if len(process.Arguments) == 0 || process.WorkingDir == "" {
		return nil, errors.New("Realtime-CU backend Python search context is incomplete")
	}
	var candidates []string
	if len(process.Arguments) >= 5 &&
		reflect.DeepEqual(process.Arguments[1:4], []string{"-I", "-S", "-B"}) {
		roots, err := exactRealtimeCUProcessArguments(process.Arguments, "--dependency-root")
		if err != nil {
			return nil, errors.New("Realtime-CU isolated Python listener lacks dependency roots")
		}
		candidates = append(candidates, roots...)
	} else {
		if process.Environment["PYTHONHOME"] != "" {
			return nil, errors.New("Realtime-CU backend uses an unsupported Python home override")
		}
		if len(process.Arguments) > 1 && filepath.IsAbs(process.Arguments[1]) &&
			strings.HasSuffix(process.Arguments[1], ".py") {
			candidates = append(candidates, filepath.Dir(process.Arguments[1]))
		}
		candidates = append(candidates, process.WorkingDir)
		if value := process.Environment["PYTHONPATH"]; value != "" {
			for _, root := range filepath.SplitList(value) {
				if !filepath.IsAbs(root) || filepath.Clean(root) != root {
					return nil, errors.New("Realtime-CU backend Python path is not canonical and absolute")
				}
				candidates = append(candidates, root)
			}
		}
		pythonRoot := filepath.Dir(filepath.Dir(process.Arguments[0]))
		patterns := []string{filepath.Join(pythonRoot, "lib", "python*", "site-packages")}
		if home := process.Environment["HOME"]; home != "" {
			if !filepath.IsAbs(home) || filepath.Clean(home) != home {
				return nil, errors.New("Realtime-CU backend home is not canonical and absolute")
			}
			patterns = append(patterns, filepath.Join(home, ".local", "lib", "python*", "site-packages"))
		}
		patterns = append(patterns,
			"/usr/local/lib/python*/dist-packages", "/usr/local/lib/python*/site-packages",
			"/usr/lib/python*/dist-packages", "/usr/lib/python*/site-packages",
		)
		for _, pattern := range patterns {
			matches, globErr := filepath.Glob(pattern)
			if globErr != nil {
				return nil, errors.New("resolve Realtime-CU backend Python search roots")
			}
			sort.Strings(matches)
			candidates = append(candidates, matches...)
		}
	}
	if len(candidates) == 0 || len(candidates) > 64 {
		return nil, errors.New("Realtime-CU backend Python search roots are incomplete")
	}
	result := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
			return nil, errors.New("Realtime-CU backend Python search root is not canonical and absolute")
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		info, statErr := os.Stat(candidate)
		if err != nil || statErr != nil || !info.IsDir() || !filepath.IsAbs(resolved) {
			return nil, errors.New("Realtime-CU backend Python search root is unavailable")
		}
		if _, duplicate := seen[resolved]; duplicate {
			continue
		}
		seen[resolved] = struct{}{}
		result = append(result, resolved)
	}
	if len(result) == 0 {
		return nil, errors.New("Realtime-CU backend Python search roots are empty")
	}
	return result, nil
}

func probeRealtimeCUQwen(
	ctx context.Context, client *http.Client, material realtimeCUBackendMaterial,
) error {
	if !validRealtimeCURevision(material.Revision) {
		return errors.New("Qwen deployment probe lacks the exact loaded revision")
	}
	var modelPath string
	for _, root := range material.Roots {
		if root.Label == "model" {
			if modelPath != "" {
				return errors.New("Qwen deployment probe has an ambiguous loaded model path")
			}
			modelPath = root.Path
		}
	}
	if revision, err := realtimeCUQwenSnapshotRevision(modelPath); err != nil || revision != material.Revision {
		return errors.New("Qwen deployment probe lacks the exact loaded model path")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8000/v1/models", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("query Qwen deployment identity")
	}
	defer response.Body.Close()
	var payload struct {
		Data []struct {
			ID          string `json:"id"`
			OwnedBy     string `json:"owned_by"`
			Root        string `json:"root"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decodeErr := decoder.Decode(&payload)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if response.StatusCode != http.StatusOK || decodeErr != nil || !errors.Is(trailingErr, io.EOF) ||
		len(payload.Data) != 1 ||
		payload.Data[0].ID != realtimeCULocalModelName || payload.Data[0].OwnedBy != "vllm" ||
		payload.Data[0].Root != modelPath ||
		payload.Data[0].MaxModelLen != 40960 {
		return errors.New("Qwen deployment probe differs from the strict local profile")
	}
	return nil
}

func probeRealtimeCUSenseVoice(
	ctx context.Context, client *http.Client, material realtimeCUBackendMaterial,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8002/health", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("query SenseVoice deployment identity")
	}
	defer response.Body.Close()
	var payload struct {
		Status        string `json:"status"`
		Model         string `json:"model"`
		Device        string `json:"device"`
		Revision      string `json:"revision"`
		Digest        string `json:"digest"`
		ServicePath   string `json:"service_path"`
		ServiceDigest string `json:"service_digest"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decodeErr := decoder.Decode(&payload)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if response.StatusCode != http.StatusOK || decodeErr != nil || !errors.Is(trailingErr, io.EOF) ||
		payload.Status != "ok" || payload.Model != realtimeCULocalASRModel || payload.Device != "cuda:0" ||
		payload.Revision != material.Revision || payload.Digest != material.LoadedDigest ||
		payload.ServicePath != material.ServicePath || payload.ServiceDigest != material.ServiceDigest {
		return errors.New("SenseVoice deployment probe differs from the strict local profile")
	}
	return nil
}

func probeRealtimeCUWhisper(
	ctx context.Context, client *http.Client, process realtimeCUProcessSnapshot,
	material realtimeCUBackendMaterial,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8003/health", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("query Whisper deployment identity")
	}
	defer response.Body.Close()
	var payload struct {
		Status                string   `json:"status"`
		Model                 string   `json:"model"`
		ModelPath             string   `json:"model_path"`
		MaterializedModelPath string   `json:"materialized_model_path"`
		Device                string   `json:"device"`
		ComputeType           string   `json:"compute_type"`
		Language              string   `json:"language"`
		Revision              string   `json:"revision"`
		Digest                string   `json:"digest"`
		ServicePath           string   `json:"service_path"`
		ServiceDigest         string   `json:"service_digest"`
		DependencyRoots       []string `json:"dependency_roots"`
		DependencyDigest      string   `json:"dependency_digest"`
		ProbeSeconds          float64  `json:"probe_seconds"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&payload)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	modelPath := ""
	materializedPath := ""
	dependencyRoots, dependencyErr := exactRealtimeCUProcessArguments(
		process.Arguments, "--dependency-root",
	)
	for _, root := range material.Roots {
		switch root.Label {
		case "model-source":
			if modelPath != "" {
				return errors.New("Whisper deployment probe has an ambiguous source model path")
			}
			modelPath = root.Path
		case "loaded-model":
			if materializedPath != "" {
				return errors.New("Whisper deployment probe has an ambiguous loaded model path")
			}
			materializedPath = root.Path
		}
	}
	if response.StatusCode != http.StatusOK || decodeErr != nil || dependencyErr != nil ||
		!errors.Is(trailingErr, io.EOF) ||
		payload.Status != "ok" || payload.Model != realtimeCULocalASRModel ||
		payload.ModelPath != modelPath || payload.MaterializedModelPath != materializedPath ||
		payload.Device != "cuda" ||
		payload.ComputeType != "int8" || payload.Language != "en" ||
		!reflect.DeepEqual(payload.DependencyRoots, dependencyRoots) ||
		payload.DependencyDigest != material.DependencyDigest ||
		payload.Revision != material.Revision || payload.Digest != material.LoadedDigest ||
		payload.ServicePath != material.ServicePath || payload.ServiceDigest != material.ServiceDigest ||
		payload.ProbeSeconds < 0 {
		return errors.New("Whisper deployment probe differs from the strict local profile")
	}
	return nil
}
