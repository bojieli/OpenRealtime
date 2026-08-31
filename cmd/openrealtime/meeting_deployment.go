package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/providers"
)

const (
	meetingLocalDeploymentEnvironment = "OPENREALTIME_MEETING_LOCAL_DEPLOYMENT"
	meetingFishArtifactID             = "model://fishaudio/fish-speech-1.5"
	meetingBackgroundSelectionID      = "provider://google/gemini-3.7-flash/configuration"
)

type meetingBackendAttestor interface {
	Attest(context.Context) (inspect.ArtifactIdentity, any, error)
	Revalidate(context.Context, inspect.ArtifactIdentity, any) error
}

type composedMeetingDeploymentVerifier struct {
	mu         sync.Mutex
	model      meetingBackendAttestor
	asr        meetingBackendAttestor
	tts        meetingBackendAttestor
	background meetingBackendAttestor
	resolved   bool
	identity   meetingDeploymentIdentities
	modelSeal  any
	asrSeal    any
	ttsSeal    any
	remoteSeal any
}

func newComposedMeetingDeploymentVerifier(
	model, asr, tts, background meetingBackendAttestor,
) (*composedMeetingDeploymentVerifier, error) {
	if nilMeetingDeploymentInterface(model) || nilMeetingDeploymentInterface(asr) ||
		nilMeetingDeploymentInterface(tts) || nilMeetingDeploymentInterface(background) {
		return nil, errors.New(
			"Meeting deployment verifier requires model, ASR, TTS, and background-selection attestors",
		)
	}
	return &composedMeetingDeploymentVerifier{
		model: model, asr: asr, tts: tts, background: background,
	}, nil
}

func (verifier *composedMeetingDeploymentVerifier) Resolve(
	ctx context.Context,
) (meetingDeploymentIdentities, error) {
	if ctx == nil {
		return meetingDeploymentIdentities{}, errors.New("resolve Meeting deployments: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return meetingDeploymentIdentities{}, cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if verifier.resolved {
		if err := verifier.verifyLocked(ctx, verifier.identity); err != nil {
			return meetingDeploymentIdentities{}, err
		}
		return verifier.identity, nil
	}
	model, modelSeal, err := verifier.model.Attest(ctx)
	if err != nil {
		return meetingDeploymentIdentities{}, fmt.Errorf("attest Meeting model deployment: %w", err)
	}
	asr, asrSeal, err := verifier.asr.Attest(ctx)
	if err != nil {
		return meetingDeploymentIdentities{}, fmt.Errorf("attest Meeting ASR deployment: %w", err)
	}
	tts, ttsSeal, err := verifier.tts.Attest(ctx)
	if err != nil {
		return meetingDeploymentIdentities{}, fmt.Errorf("attest Meeting TTS deployment: %w", err)
	}
	background, remoteSeal, err := verifier.background.Attest(ctx)
	if err != nil {
		return meetingDeploymentIdentities{}, fmt.Errorf(
			"attest Meeting background provider selection: %w", err,
		)
	}
	identity := meetingDeploymentIdentities{
		Model: model, ASR: asr, TTS: tts, Vision: model, Background: background,
	}
	if err := identity.validate(); err != nil {
		return meetingDeploymentIdentities{}, err
	}
	verifier.identity = identity
	verifier.modelSeal = modelSeal
	verifier.asrSeal = asrSeal
	verifier.ttsSeal = ttsSeal
	verifier.remoteSeal = remoteSeal
	verifier.resolved = true
	return identity, nil
}

func (verifier *composedMeetingDeploymentVerifier) Verify(
	ctx context.Context, expected meetingDeploymentIdentities,
) error {
	if ctx == nil {
		return errors.New("verify Meeting deployments: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if !verifier.resolved {
		return errors.New("Meeting deployments were not independently resolved")
	}
	return verifier.verifyLocked(ctx, expected)
}

func (verifier *composedMeetingDeploymentVerifier) VerifyForeground(
	ctx context.Context, expected meetingDeploymentIdentities,
) error {
	return verifier.verifyComponents(ctx, expected, true, true, true, false)
}

func (verifier *composedMeetingDeploymentVerifier) VerifyVision(
	ctx context.Context, expected meetingDeploymentIdentities,
) error {
	return verifier.verifyComponents(ctx, expected, true, false, false, false)
}

func (verifier *composedMeetingDeploymentVerifier) VerifyBackground(
	ctx context.Context, expected meetingDeploymentIdentities,
) error {
	return verifier.verifyComponents(ctx, expected, false, false, false, true)
}

func (verifier *composedMeetingDeploymentVerifier) verifyComponents(
	ctx context.Context, expected meetingDeploymentIdentities,
	model, asr, tts, background bool,
) error {
	if ctx == nil {
		return errors.New("verify Meeting deployment component: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if !verifier.resolved {
		return errors.New("Meeting deployments were not independently resolved")
	}
	if err := verifier.verifyExpectedLocked(expected); err != nil {
		return err
	}
	if model {
		if err := verifier.model.Revalidate(ctx, expected.Model, verifier.modelSeal); err != nil {
			return fmt.Errorf("revalidate Meeting model deployment: %w", err)
		}
	}
	if asr {
		if err := verifier.asr.Revalidate(ctx, expected.ASR, verifier.asrSeal); err != nil {
			return fmt.Errorf("revalidate Meeting ASR deployment: %w", err)
		}
	}
	if tts {
		if err := verifier.tts.Revalidate(ctx, expected.TTS, verifier.ttsSeal); err != nil {
			return fmt.Errorf("revalidate Meeting TTS deployment: %w", err)
		}
	}
	if background {
		if err := verifier.background.Revalidate(
			ctx, expected.Background, verifier.remoteSeal,
		); err != nil {
			return fmt.Errorf("revalidate Meeting background provider selection: %w", err)
		}
	}
	return nil
}

func (verifier *composedMeetingDeploymentVerifier) verifyLocked(
	ctx context.Context, expected meetingDeploymentIdentities,
) error {
	if err := verifier.verifyExpectedLocked(expected); err != nil {
		return err
	}
	if err := verifier.model.Revalidate(ctx, expected.Model, verifier.modelSeal); err != nil {
		return fmt.Errorf("revalidate Meeting model deployment: %w", err)
	}
	if err := verifier.asr.Revalidate(ctx, expected.ASR, verifier.asrSeal); err != nil {
		return fmt.Errorf("revalidate Meeting ASR deployment: %w", err)
	}
	if err := verifier.tts.Revalidate(ctx, expected.TTS, verifier.ttsSeal); err != nil {
		return fmt.Errorf("revalidate Meeting TTS deployment: %w", err)
	}
	if err := verifier.background.Revalidate(
		ctx, expected.Background, verifier.remoteSeal,
	); err != nil {
		return fmt.Errorf("revalidate Meeting background provider selection: %w", err)
	}
	return nil
}

func (verifier *composedMeetingDeploymentVerifier) verifyExpectedLocked(
	expected meetingDeploymentIdentities,
) error {
	if expected != verifier.identity {
		return errors.New("Meeting expected deployment identities differ from live attestation")
	}
	if expected.Vision != expected.Model {
		return errors.New("Meeting local vision deployment must be the attested model deployment")
	}
	return nil
}

type meetingBackgroundSelectionAttestor struct {
	identity inspect.ArtifactIdentity
}

func newMeetingBackgroundSelectionAttestor() (*meetingBackgroundSelectionAttestor, error) {
	config := defaultMeetingLocalConfiguration(
		inspect.ArtifactIdentity{ID: "configuration://unlinked", Digest: "sha256:" + strings.Repeat("0", 64)},
		meetingDeploymentIdentities{},
	).Background
	payload, err := json.Marshal(struct {
		FormatVersion uint64 `json:"format_version"`
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		BaseURL       string `json:"base_url"`
		Effort        string `json:"effort"`
		RetainReason  bool   `json:"retain_reasoning"`
		TimeoutMS     int64  `json:"request_timeout_ms"`
		Caveat        string `json:"attestation_caveat"`
	}{
		FormatVersion: 1, Provider: config.Provider, Model: config.Model,
		BaseURL: config.BaseURL, Effort: config.Effort, RetainReason: config.RetainReasoning,
		TimeoutMS: config.RequestTimeoutMS,
		Caveat:    "remote provider selection and credential readiness; provider weights are not locally attestable",
	})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	identity := inspect.ArtifactIdentity{
		ID: meetingBackgroundSelectionID, Revision: meetingBackgroundModel,
		Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	return &meetingBackgroundSelectionAttestor{identity: identity}, nil
}

func (attestor *meetingBackgroundSelectionAttestor) Attest(
	ctx context.Context,
) (inspect.ArtifactIdentity, any, error) {
	if err := attestor.revalidate(ctx, attestor.identity); err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	return attestor.identity, attestor.identity.Digest, nil
}

func (attestor *meetingBackgroundSelectionAttestor) Revalidate(
	ctx context.Context, expected inspect.ArtifactIdentity, seal any,
) error {
	digest, ok := seal.(string)
	if !ok || digest != attestor.identity.Digest {
		return errors.New("Meeting background selection seal is invalid")
	}
	return attestor.revalidate(ctx, expected)
}

func (attestor *meetingBackgroundSelectionAttestor) revalidate(
	ctx context.Context, expected inspect.ArtifactIdentity,
) error {
	if ctx == nil {
		return errors.New("verify Meeting background selection: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if expected != attestor.identity {
		return errors.New("Meeting background selection differs from the exact Gemini 3.7 Flash configuration")
	}
	provider, err := providers.NewLLM(meetingBackgroundLLMRequest(defaultMeetingLocalConfiguration(
		inspect.ArtifactIdentity{ID: "configuration://unlinked", Digest: "sha256:" + strings.Repeat("0", 64)},
		meetingDeploymentIdentities{},
	).Background))
	if err != nil {
		return fmt.Errorf("open exact Gemini 3.7 Flash provider selection: %w", err)
	}
	return closeReadinessResource(provider)
}

func newLocalMeetingDeploymentVerifier() (meetingDeploymentVerifier, error) {
	processes := procRealtimeCUProcessSource{}
	client := &http.Client{Timeout: 15 * time.Second}
	model, err := newLocalRealtimeCUBackendAttestor(realtimeCUBackendAttestorConfig{
		Name: "meeting-qwen-fast-model-and-vision", Port: 8000, ProcessSource: processes,
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
		Name: "meeting-sensevoice-asr", Port: 8002, ProcessSource: processes,
		ValidateProcess: validateMeetingSenseVoiceProcess,
		Probe: func(ctx context.Context, _ realtimeCUProcessSnapshot, material realtimeCUBackendMaterial) error {
			return probeRealtimeCUSenseVoice(ctx, client, material)
		},
		Material: realtimeCUSenseVoiceMaterial,
	})
	if err != nil {
		return nil, err
	}
	tts, err := newLocalRealtimeCUBackendAttestor(realtimeCUBackendAttestorConfig{
		Name: "meeting-fish-speech-tts", Port: 8123, ProcessSource: processes,
		ValidateProcess: validateMeetingFishProcess,
		Probe: func(ctx context.Context, process realtimeCUProcessSnapshot, material realtimeCUBackendMaterial) error {
			return probeMeetingFish(ctx, client, process, material)
		},
		Material: meetingFishMaterial,
	})
	if err != nil {
		return nil, err
	}
	background, err := newMeetingBackgroundSelectionAttestor()
	if err != nil {
		return nil, err
	}
	return newComposedMeetingDeploymentVerifier(model, asr, tts, background)
}

func validateMeetingSenseVoiceProcess(process realtimeCUProcessSnapshot) error {
	return validateSenseVoiceProcess(process, meetingLocalASRModel)
}

func validateMeetingFishProcess(process realtimeCUProcessSnapshot) error {
	working, err := canonicalRealtimeCUDeploymentPath(process.WorkingDir)
	parent := filepath.Dir(working)
	grandparent := filepath.Dir(parent)
	validSourceRoot := filepath.Base(working) == "fish-speech" &&
		(filepath.Base(parent) == ".runtime" ||
			(filepath.Base(parent) == "fish" && filepath.Base(grandparent) == ".runtime"))
	if err != nil || !validSourceRoot ||
		process.Environment["PYTHONPATH"] != working {
		return errors.New("Fish Speech listener requires one exact reviewed source root")
	}
	checkpoint, err := exactRealtimeCUProcessArgument(process.Arguments, "--checkpoint")
	if err != nil {
		return errors.New("Fish Speech listener requires one exact --checkpoint path")
	}
	if _, err := meetingFishSnapshotRevision(checkpoint); err != nil {
		return errors.New("Fish Speech listener checkpoint is not the immutable selected snapshot")
	}
	voices, err := exactRealtimeCUProcessArgument(process.Arguments, "--voices")
	if err != nil {
		return errors.New("Fish Speech listener requires one exact --voices path")
	}
	if _, err := canonicalRealtimeCUDeploymentPath(voices); err != nil {
		return errors.New("Fish Speech listener voices path is not canonical and absolute")
	}
	service, err := meetingFishServicePath(process)
	if err != nil {
		return err
	}
	if err := requireRealtimeCUProcessArguments(process.Arguments, map[string]string{
		"--port": "8123", "--device": "cuda",
	}, []string{service}); err != nil {
		return err
	}
	for _, argument := range process.Arguments {
		if argument == "--no-compile" {
			return errors.New("Fish Speech listener disables the selected compiled runtime")
		}
	}
	return nil
}

func meetingFishServicePath(process realtimeCUProcessSnapshot) (string, error) {
	if len(process.Arguments) < 2 {
		return "", errors.New("Fish Speech listener service path is missing")
	}
	service, err := canonicalRealtimeCUDeploymentPath(process.Arguments[1])
	if err != nil || filepath.Base(service) != "server.py" ||
		filepath.Base(filepath.Dir(service)) != "fish15" ||
		filepath.Base(filepath.Dir(filepath.Dir(service))) != "tools" {
		return "", errors.New("Fish Speech listener service path is not the reviewed implementation")
	}
	return service, nil
}

func meetingFishSnapshotRevision(path string) (string, error) {
	canonical, err := canonicalRealtimeCUDeploymentPath(path)
	if err != nil {
		return "", err
	}
	revision := filepath.Base(canonical)
	snapshots := filepath.Dir(canonical)
	repository := filepath.Dir(snapshots)
	if filepath.Base(snapshots) != "snapshots" ||
		filepath.Base(repository) != "models--fishaudio--fish-speech-1.5" ||
		!validRealtimeCURevision(revision) {
		return "", errors.New("Fish Speech local model path is not the immutable selected snapshot")
	}
	return revision, nil
}

func meetingFishMaterial(
	ctx context.Context, process realtimeCUProcessSnapshot,
) (realtimeCUBackendMaterial, error) {
	checkpoint, err := exactRealtimeCUProcessArgument(process.Arguments, "--checkpoint")
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	checkpoint, err = canonicalRealtimeCUDeploymentPath(checkpoint)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	voices, err := exactRealtimeCUProcessArgument(process.Arguments, "--voices")
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	voices, err = canonicalRealtimeCUDeploymentPath(voices)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	revision, err := meetingFishSnapshotRevision(checkpoint)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	service, err := meetingFishServicePath(process)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	packages, err := realtimeCURuntimePackageRoots(
		ctx, process, []string{"soundfile", "torch", "torchaudio"},
	)
	if err != nil {
		return realtimeCUBackendMaterial{}, err
	}
	roots := []realtimeCUMaterialRoot{
		{Label: "model", Path: checkpoint},
		{Label: "service", Path: service},
		{Label: "voices", Path: voices},
		{Label: "runtime-fish_speech", Path: filepath.Join(process.WorkingDir, "fish_speech")},
	}
	roots = append(roots, packages...)
	return realtimeCUBackendMaterial{
		ArtifactID: meetingFishArtifactID, Revision: revision, Roots: roots,
	}, nil
}

type meetingFishHealth struct {
	Status             string            `json:"status"`
	CheckpointRoot     string            `json:"checkpoint_root"`
	CheckpointRevision string            `json:"checkpoint_revision"`
	VoicesRoot         string            `json:"voices_root"`
	Voices             map[string]string `json:"voices"`
	Device             string            `json:"device"`
	CompileGraphs      bool              `json:"compile_graphs"`
	ServicePath        string            `json:"service_path"`
	ServiceSHA256      string            `json:"service_sha256"`
	RuntimeExecutable  string            `json:"runtime_executable"`
	WorkingDirectory   string            `json:"working_directory"`
	RuntimeModules     map[string]string `json:"runtime_modules"`
}

func probeMeetingFish(
	ctx context.Context, client *http.Client, process realtimeCUProcessSnapshot,
	material realtimeCUBackendMaterial,
) error {
	if material.ArtifactID != meetingFishArtifactID || len(material.Roots) == 0 {
		return errors.New("Fish Speech deployment probe lacks exact retained material")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8123/health", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("query Fish Speech deployment readiness")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Fish Speech deployment probe differs from the strict local profile")
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32<<10))
	decoder.DisallowUnknownFields()
	var health meetingFishHealth
	if err := decoder.Decode(&health); err != nil {
		return errors.New("decode Fish Speech deployment identity")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("Fish Speech deployment identity has trailing data")
	}
	if err := validateMeetingFishHealth(ctx, process, material, health); err != nil {
		return err
	}
	return nil
}

func validateMeetingFishHealth(
	ctx context.Context, process realtimeCUProcessSnapshot,
	material realtimeCUBackendMaterial, health meetingFishHealth,
) error {
	checkpoint, err := exactRealtimeCUProcessArgument(process.Arguments, "--checkpoint")
	if err != nil {
		return err
	}
	voices, err := exactRealtimeCUProcessArgument(process.Arguments, "--voices")
	if err != nil {
		return err
	}
	service, err := meetingFishServicePath(process)
	if err != nil {
		return err
	}
	revision, err := meetingFishSnapshotRevision(checkpoint)
	if err != nil {
		return err
	}
	serviceSeal, err := snapshotRealtimeCUFile("service/server.py", service)
	if err != nil {
		return err
	}
	serviceDigest, err := hashRealtimeCUFile(ctx, serviceSeal)
	if err != nil {
		return err
	}
	voiceDigests, err := meetingFishVoiceDigests(ctx, voices)
	if err != nil {
		return err
	}
	wantRuntime := make(map[string]string)
	for _, root := range material.Roots {
		if name, found := strings.CutPrefix(root.Label, "runtime-"); found {
			wantRuntime[name] = root.Path
		}
	}
	if health.Status != "ready" || health.CheckpointRoot != checkpoint ||
		health.CheckpointRevision != revision || health.VoicesRoot != voices ||
		health.Device != "cuda" || !health.CompileGraphs ||
		health.ServicePath != service || health.ServiceSHA256 != serviceDigest ||
		health.RuntimeExecutable != process.Arguments[0] ||
		health.WorkingDirectory != process.WorkingDir ||
		!reflect.DeepEqual(health.Voices, voiceDigests) ||
		!reflect.DeepEqual(health.RuntimeModules, wantRuntime) {
		return errors.New("Fish Speech deployment probe differs from the strict local profile")
	}
	return nil
}

func meetingFishVoiceDigests(ctx context.Context, directory string) (map[string]string, error) {
	canonical, err := canonicalRealtimeCUDeploymentPath(directory)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, errors.New("open Fish Speech voice directory")
	}
	defer root.Close()
	opened, err := root.Open(".")
	if err != nil {
		return nil, errors.New("open Fish Speech voice directory handle")
	}
	entries, readErr := opened.ReadDir(-1)
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || len(entries) == 0 {
		return nil, errors.New("read Fish Speech voice directory")
	}
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			strings.ContainsAny(entry.Name(), "/\\\x00\r\n") {
			return nil, errors.New("Fish Speech voice directory has an unsupported entry")
		}
		file, err := root.Open(entry.Name())
		if err != nil {
			return nil, errors.New("open Fish Speech voice file")
		}
		info, statErr := file.Stat()
		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, &contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if statErr != nil || copyErr != nil || closeErr != nil || !info.Mode().IsRegular() {
			return nil, errors.New("hash Fish Speech voice file")
		}
		result[entry.Name()] = "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	}
	return result, nil
}
