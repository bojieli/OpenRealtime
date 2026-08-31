package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

type fixtureMeetingBackendAttestor struct {
	identity        inspect.ArtifactIdentity
	seal            string
	attestErr       error
	revalidateErr   error
	attestCalls     int
	revalidateCalls int
}

func (attestor *fixtureMeetingBackendAttestor) Attest(
	ctx context.Context,
) (inspect.ArtifactIdentity, any, error) {
	attestor.attestCalls++
	if err := context.Cause(ctx); err != nil {
		return inspect.ArtifactIdentity{}, nil, err
	}
	if attestor.attestErr != nil {
		return inspect.ArtifactIdentity{}, nil, attestor.attestErr
	}
	return attestor.identity, attestor.seal, nil
}

func (attestor *fixtureMeetingBackendAttestor) Revalidate(
	ctx context.Context, expected inspect.ArtifactIdentity, seal any,
) error {
	attestor.revalidateCalls++
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if attestor.revalidateErr != nil {
		return attestor.revalidateErr
	}
	if expected != attestor.identity || seal != attestor.seal {
		return errors.New("fixture Meeting backend proof drifted")
	}
	return nil
}

func TestComposedMeetingDeploymentVerifierBindsOpaqueLiveProofs(t *testing.T) {
	want := meetingProfileDeployments()
	model := &fixtureMeetingBackendAttestor{identity: want.Model, seal: "model-seal"}
	asr := &fixtureMeetingBackendAttestor{identity: want.ASR, seal: "asr-seal"}
	tts := &fixtureMeetingBackendAttestor{identity: want.TTS, seal: "tts-seal"}
	background := &fixtureMeetingBackendAttestor{identity: want.Background, seal: "remote-seal"}
	verifier, err := newComposedMeetingDeploymentVerifier(model, asr, tts, background)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifier.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want.Vision = want.Model
	if got != want {
		t.Fatalf("resolved Meeting deployments = %+v, want %+v", got, want)
	}
	if err := verifier.Verify(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	for name, attestor := range map[string]*fixtureMeetingBackendAttestor{
		"model": model, "ASR": asr, "TTS": tts, "background": background,
	} {
		if attestor.attestCalls != 1 || attestor.revalidateCalls != 1 {
			t.Fatalf("%s proof calls attest=%d revalidate=%d", name,
				attestor.attestCalls, attestor.revalidateCalls)
		}
	}
	if err := verifier.VerifyVision(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if model.revalidateCalls != 2 || asr.revalidateCalls != 1 ||
		tts.revalidateCalls != 1 || background.revalidateCalls != 1 {
		t.Fatalf("Meeting visual verification touched unrelated deployments: model=%d ASR=%d TTS=%d background=%d",
			model.revalidateCalls, asr.revalidateCalls, tts.revalidateCalls, background.revalidateCalls)
	}
	if err := verifier.VerifyBackground(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if model.revalidateCalls != 2 || asr.revalidateCalls != 1 ||
		tts.revalidateCalls != 1 || background.revalidateCalls != 2 {
		t.Fatalf("Meeting background verification touched unrelated deployments: model=%d ASR=%d TTS=%d background=%d",
			model.revalidateCalls, asr.revalidateCalls, tts.revalidateCalls, background.revalidateCalls)
	}
	drifted := got
	drifted.TTS.Digest = "sha256:" + strings.Repeat("e", 64)
	if err := verifier.Verify(context.Background(), drifted); err == nil ||
		!strings.Contains(err.Error(), "differ") {
		t.Fatalf("drifted Meeting deployment error = %v", err)
	}
	model.revalidateErr = errors.New("model files changed")
	if err := verifier.Verify(context.Background(), got); err == nil ||
		!strings.Contains(err.Error(), "model files changed") {
		t.Fatalf("changed Meeting model proof error = %v", err)
	}
}

func TestMeetingBackgroundSelectionAttestsExactGeminiWithoutRetainingCredential(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	attestor, err := newMeetingBackgroundSelectionAttestor()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := attestor.Attest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "credential") {
		t.Fatalf("Meeting remote selection without credential error = %v", err)
	}
	const secret = "fixture-gemini-credential-not-evidence"
	t.Setenv("GEMINI_API_KEY", secret)
	identity, seal, err := attestor.Attest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != meetingBackgroundSelectionID || identity.Revision != "gemini-3.7-flash" ||
		identity.Digest == "" {
		t.Fatalf("Meeting remote selection identity = %+v", identity)
	}
	if err := attestor.Revalidate(context.Background(), identity, seal); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Identity inspect.ArtifactIdentity `json:"identity"`
		Seal     any                      `json:"seal"`
	}{Identity: identity, Seal: seal})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(secret)) {
		t.Fatal("Meeting background selection evidence retained the credential")
	}
}

func TestMeetingFishProcessRequiresExactMaterialPathsAndRuntime(t *testing.T) {
	checkpoint := filepath.Join(
		"/models/models--fishaudio--fish-speech-1.5/snapshots",
		strings.Repeat("a", 40),
	)
	valid := realtimeCUProcessSnapshot{
		Arguments: []string{
			"/runtime/fish/bin/python",
			"/workspace/OpenRealtime/tools/fish15/server.py",
			"--port", "8123",
			"--checkpoint", checkpoint,
			"--device", "cuda",
			"--voices", "/voices/fish",
		},
		WorkingDir: "/workspace/OpenRealtime/.runtime/fish/fish-speech",
		Environment: map[string]string{
			"PYTHONPATH": "/workspace/OpenRealtime/.runtime/fish/fish-speech",
		},
	}
	if err := validateMeetingFishProcess(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]string) []string{
		"missing checkpoint": func(arguments []string) []string { return arguments[:4] },
		"relative checkpoint": func(arguments []string) []string {
			arguments[5] = "models/fish"
			return arguments
		},
		"mutable checkpoint": func(arguments []string) []string {
			arguments[5] = "/models/checkpoints/fish-speech-1.5"
			return arguments
		},
		"unreviewed service": func(arguments []string) []string {
			arguments[1] = "/tmp/server.py"
			return arguments
		},
		"no compile": func(arguments []string) []string { return append(arguments, "--no-compile") },
	} {
		t.Run(name, func(t *testing.T) {
			arguments := append([]string(nil), valid.Arguments...)
			changed := valid
			changed.Arguments = mutate(arguments)
			if err := validateMeetingFishProcess(changed); err == nil {
				t.Fatal("invalid Fish Speech process was accepted")
			}
		})
	}
	wrongSource := valid
	wrongSource.Environment = map[string]string{"PYTHONPATH": "/tmp/fish-speech"}
	if err := validateMeetingFishProcess(wrongSource); err == nil {
		t.Fatal("Fish Speech process with a different import root was accepted")
	}
}

func TestMeetingFishHealthBindsLoadedSelectionRuntimeAndVoices(t *testing.T) {
	revision := strings.Repeat("b", 40)
	checkpoint := filepath.Join(
		t.TempDir(), "models--fishaudio--fish-speech-1.5", "snapshots", revision,
	)
	voices := filepath.Join(t.TempDir(), "voices")
	service := filepath.Join(t.TempDir(), "tools", "fish15", "server.py")
	for _, directory := range []string{checkpoint, voices, filepath.Dir(service)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(service, []byte("reviewed service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(voices, "default.wav"), []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	process := realtimeCUProcessSnapshot{
		Arguments: []string{
			"/runtime/fish/bin/python", service, "--port", "8123",
			"--checkpoint", checkpoint, "--device", "cuda", "--voices", voices,
		},
		WorkingDir:  "/runtime/fish/fish-speech",
		Environment: map[string]string{"PYTHONPATH": "/runtime/fish/fish-speech"},
	}
	material := realtimeCUBackendMaterial{
		ArtifactID: meetingFishArtifactID, Revision: revision,
		Roots: []realtimeCUMaterialRoot{
			{Label: "model", Path: checkpoint},
			{Label: "service", Path: service},
			{Label: "voices", Path: voices},
			{Label: "runtime-fish_speech", Path: "/runtime/fish/fish_speech"},
			{Label: "runtime-soundfile", Path: "/runtime/fish/soundfile.py"},
			{Label: "runtime-torch", Path: "/runtime/fish/torch"},
			{Label: "runtime-torchaudio", Path: "/runtime/fish/torchaudio"},
		},
	}
	serviceSeal, err := snapshotRealtimeCUFile("service/server.py", service)
	if err != nil {
		t.Fatal(err)
	}
	serviceDigest, err := hashRealtimeCUFile(context.Background(), serviceSeal)
	if err != nil {
		t.Fatal(err)
	}
	voicesDigest, err := meetingFishVoiceDigests(context.Background(), voices)
	if err != nil {
		t.Fatal(err)
	}
	health := meetingFishHealth{
		Status: "ready", CheckpointRoot: checkpoint, CheckpointRevision: revision,
		VoicesRoot: voices, Voices: voicesDigest, Device: "cuda", CompileGraphs: true,
		ServicePath: service, ServiceSHA256: serviceDigest,
		RuntimeExecutable: process.Arguments[0], WorkingDirectory: process.WorkingDir,
		RuntimeModules: map[string]string{
			"fish_speech": "/runtime/fish/fish_speech",
			"soundfile":   "/runtime/fish/soundfile.py",
			"torch":       "/runtime/fish/torch",
			"torchaudio":  "/runtime/fish/torchaudio",
		},
	}
	if err := validateMeetingFishHealth(context.Background(), process, material, health); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*meetingFishHealth){
		"revision": func(value *meetingFishHealth) { value.CheckpointRevision = strings.Repeat("c", 40) },
		"voice":    func(value *meetingFishHealth) { value.Voices["default.wav"] = "sha256:" + strings.Repeat("d", 64) },
		"service":  func(value *meetingFishHealth) { value.ServiceSHA256 = "sha256:" + strings.Repeat("e", 64) },
		"runtime":  func(value *meetingFishHealth) { value.RuntimeModules["torch"] = "/tmp/torch" },
		"compile":  func(value *meetingFishHealth) { value.CompileGraphs = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := health
			changed.Voices = map[string]string{"default.wav": health.Voices["default.wav"]}
			changed.RuntimeModules = make(map[string]string, len(health.RuntimeModules))
			for key, value := range health.RuntimeModules {
				changed.RuntimeModules[key] = value
			}
			mutate(&changed)
			if err := validateMeetingFishHealth(
				context.Background(), process, material, changed,
			); err == nil {
				t.Fatal("drifted Fish Speech health was accepted")
			}
		})
	}
}

func TestLiveMeetingDeploymentVerifier(t *testing.T) {
	if os.Getenv("OPENREALTIME_LIVE_MEETING_DEPLOYMENTS") != "1" {
		t.Skip("set OPENREALTIME_LIVE_MEETING_DEPLOYMENTS=1 for local deployment attestation")
	}
	verifier, err := newLocalMeetingDeploymentVerifier()
	if err != nil {
		t.Fatal(err)
	}
	identities, err := verifier.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identities.Model != identities.Vision || identities.Background.ID != meetingBackgroundSelectionID {
		t.Fatalf("live Meeting deployments = %+v", identities)
	}
	if err := verifier.Verify(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
}

func TestLiveMeetingDeploymentSessionRevalidationLatency(t *testing.T) {
	if os.Getenv("OPENREALTIME_LIVE_MEETING_DEPLOYMENTS") != "1" {
		t.Skip("set OPENREALTIME_LIVE_MEETING_DEPLOYMENTS=1 for local deployment attestation")
	}
	verifier, err := newLocalMeetingDeploymentVerifier()
	if err != nil {
		t.Fatal(err)
	}
	identities, err := verifier.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	components, ok := verifier.(meetingDeploymentComponentVerifier)
	if !ok {
		t.Fatal("local Meeting verifier lacks scoped session revalidation")
	}
	started := time.Now()
	if err := components.VerifyForeground(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	if err := components.VerifyVision(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	if err := components.VerifyBackground(context.Background(), identities); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("one Meeting session deployment revalidation took %s, limit 5s", elapsed)
	}
}

func BenchmarkLiveMeetingDeploymentSessionRevalidation(b *testing.B) {
	if os.Getenv("OPENREALTIME_LIVE_MEETING_DEPLOYMENTS") != "1" {
		b.Skip("set OPENREALTIME_LIVE_MEETING_DEPLOYMENTS=1 for local deployment attestation")
	}
	verifier, err := newLocalMeetingDeploymentVerifier()
	if err != nil {
		b.Fatal(err)
	}
	identities, err := verifier.Resolve(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	components := verifier.(meetingDeploymentComponentVerifier)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := components.VerifyForeground(context.Background(), identities); err != nil {
			b.Fatal(err)
		}
		if err := components.VerifyVision(context.Background(), identities); err != nil {
			b.Fatal(err)
		}
		if err := components.VerifyBackground(context.Background(), identities); err != nil {
			b.Fatal(err)
		}
	}
}
