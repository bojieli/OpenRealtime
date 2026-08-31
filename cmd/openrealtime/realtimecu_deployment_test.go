package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mutableRealtimeCUProcessSource struct {
	mu       sync.Mutex
	snapshot realtimeCUProcessSnapshot
	err      error
}

func (source *mutableRealtimeCUProcessSource) Listener(
	ctx context.Context, _ int,
) (realtimeCUProcessSnapshot, error) {
	if cause := context.Cause(ctx); cause != nil {
		return realtimeCUProcessSnapshot{}, cause
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.err != nil {
		return realtimeCUProcessSnapshot{}, source.err
	}
	return cloneRealtimeCUProcessSnapshot(source.snapshot), nil
}

func (source *mutableRealtimeCUProcessSource) mutate(
	mutation func(*realtimeCUProcessSnapshot),
) {
	source.mu.Lock()
	defer source.mu.Unlock()
	mutation(&source.snapshot)
}

func cloneRealtimeCUProcessSnapshot(source realtimeCUProcessSnapshot) realtimeCUProcessSnapshot {
	result := source
	result.Arguments = slices.Clone(source.Arguments)
	result.Environment = make(map[string]string, len(source.Environment))
	for name, value := range source.Environment {
		result.Environment[name] = value
	}
	return result
}

type fixtureRealtimeCUBackend struct {
	source *mutableRealtimeCUProcessSource
	root   string
	probe  atomic.Bool
	inner  *localRealtimeCUBackendAttestor
}

func newFixtureRealtimeCUBackend(
	t *testing.T, name, artifactID string, pid int,
) *fixtureRealtimeCUBackend {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "weights.bin"), []byte("immutable-"+name), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &mutableRealtimeCUProcessSource{snapshot: realtimeCUProcessSnapshot{
		PID: pid, StartTime: fmt.Sprintf("start-%d", pid),
		Arguments:  []string{"/runtime/python", "-m", name},
		Executable: "/runtime/python", WorkingDir: "/runtime/" + name,
		Environment: map[string]string{"MODEL_VARIANT": name},
	}}
	result := &fixtureRealtimeCUBackend{source: source, root: root}
	result.probe.Store(true)
	inner, err := newLocalRealtimeCUBackendAttestor(realtimeCUBackendAttestorConfig{
		Name: name, Port: 10_000 + pid, ProcessSource: source,
		ValidateProcess: func(snapshot realtimeCUProcessSnapshot) error {
			if !slices.Contains(snapshot.Arguments, name) {
				return errors.New("wrong listener process")
			}
			return nil
		},
		Probe: func(context.Context, realtimeCUProcessSnapshot, realtimeCUBackendMaterial) error {
			if !result.probe.Load() {
				return errors.New("wrong live backend")
			}
			return nil
		},
		Material: func(context.Context, realtimeCUProcessSnapshot) (realtimeCUBackendMaterial, error) {
			return realtimeCUBackendMaterial{
				ArtifactID: artifactID, Revision: "fixture-revision",
				Roots: []realtimeCUMaterialRoot{{Label: "deployment", Path: root}},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result.inner = inner
	return result
}

func newFixtureRealtimeCUDeploymentVerifier(
	t *testing.T,
) (*composedRealtimeCUDeploymentVerifier, *fixtureRealtimeCUBackend, *fixtureRealtimeCUBackend) {
	t.Helper()
	model := newFixtureRealtimeCUBackend(t, "model", "fixture://realtime-cu/model", 101)
	asr := newFixtureRealtimeCUBackend(t, "asr", "fixture://realtime-cu/asr", 102)
	verifier, err := newComposedRealtimeCUDeploymentVerifier(model.inner, asr.inner)
	if err != nil {
		t.Fatal(err)
	}
	return verifier, model, asr
}

func TestComposedRealtimeCUDeploymentVerifierBindsLiveProcessProbeAndMaterial(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *fixtureRealtimeCUBackend, *fixtureRealtimeCUBackend)
	}{
		{
			name: "listener PID replacement",
			mutate: func(_ *testing.T, model, _ *fixtureRealtimeCUBackend) {
				model.source.mutate(func(snapshot *realtimeCUProcessSnapshot) {
					snapshot.PID++
					snapshot.StartTime = "replacement"
				})
			},
		},
		{
			name: "model bytes drift",
			mutate: func(t *testing.T, model, _ *fixtureRealtimeCUBackend) {
				if err := os.WriteFile(filepath.Join(model.root, "weights.bin"), []byte("mutated-model"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "model file set drift",
			mutate: func(t *testing.T, model, _ *fixtureRealtimeCUBackend) {
				if err := os.WriteFile(filepath.Join(model.root, "injected.bin"), []byte("injected"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "ASR health drift",
			mutate: func(_ *testing.T, _, asr *fixtureRealtimeCUBackend) {
				asr.probe.Store(false)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			verifier, model, asr := newFixtureRealtimeCUDeploymentVerifier(t)
			identity, err := verifier.Resolve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if identity.Model.Digest == "" || identity.ASR.Digest == "" || identity.Vision != identity.Model {
				t.Fatalf("resolved deployment identity = %+v", identity)
			}
			if err := verifier.Verify(context.Background(), identity); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, model, asr)
			if err := verifier.Verify(context.Background(), identity); err == nil {
				t.Fatal("deployment verifier accepted drift after attestation")
			}
		})
	}
}

func TestComposedRealtimeCUDeploymentVerifierRejectsUnattestedOrWrongExpectedIdentity(t *testing.T) {
	verifier, model, _ := newFixtureRealtimeCUDeploymentVerifier(t)
	if err := verifier.Verify(context.Background(), realtimeCUDeploymentIdentities{}); err == nil {
		t.Fatal("unresolved verifier accepted an expected identity")
	}
	model.probe.Store(false)
	if _, err := verifier.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "wrong live backend") {
		t.Fatalf("wrong-backend resolution error = %v", err)
	}

	verifier, _, _ = newFixtureRealtimeCUDeploymentVerifier(t)
	identity, err := verifier.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.Model.Revision = "forged-revision"
	if err := verifier.Verify(context.Background(), wrong); err == nil {
		t.Fatal("verifier accepted a caller-substituted deployment identity")
	}
}

func TestProcRealtimeCUProcessSourceBindsExactLoopbackListenerAndStartIdentity(t *testing.T) {
	root := t.TempDir()
	const originalEnvironment = "HF_HOME=/models\x00VLLM_WORKER_MULTIPROC_METHOD=spawn\x00" +
		"CUDA_VISIBLE_DEVICES=0\x00LD_PRELOAD=/runtime/inject.so\x00" +
		"NVIDIA_TF32_OVERRIDE=0\x00VLLM_API_KEY=guessable-one\x00" +
		"SENSEVOICE_API_KEY=guessable-two\x00SECRET=guessable-three\x00"
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeProcTCP := func(inode string) {
		t.Helper()
		payload := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n" +
			"   0: 0100007F:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 " + inode + "\n"
		if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProcess := func(pid int, inode, start string) {
		t.Helper()
		prefix := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(filepath.Join(prefix, "fd"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(prefix, "fd", "3")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "cmdline"), []byte("/runtime/python\x00-m\x00fixture\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		fields := []string{"S"}
		for index := 1; index <= 20; index++ {
			value := fmt.Sprint(index)
			if index == 19 {
				value = start
			}
			fields = append(fields, value)
		}
		if err := os.WriteFile(filepath.Join(prefix, "stat"), []byte(fmt.Sprintf("%d (fixture) %s\n", pid, strings.Join(fields, " "))), 0o600); err != nil {
			t.Fatal(err)
		}
		executable := filepath.Join(root, "python")
		if err := os.WriteFile(executable, []byte("runtime"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(executable, filepath.Join(prefix, "exe")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(root, filepath.Join(prefix, "cwd")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "environ"), []byte(originalEnvironment), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeProcTCP("4242")
	writeProcess(321, "4242", "777")
	source := procRealtimeCUProcessSource{root: root}
	snapshot, err := source.Listener(context.Background(), 8000)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PID != 321 || snapshot.StartTime != "777" || snapshot.Environment["HF_HOME"] != "/models" {
		t.Fatalf("listener snapshot = %+v", snapshot)
	}
	if snapshot.ExecutableHandle != filepath.Join(root, "321", "exe") ||
		snapshot.EnvironmentDigest == "" ||
		snapshot.Environment["VLLM_WORKER_MULTIPROC_METHOD"] != "spawn" ||
		snapshot.Environment["CUDA_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("listener execution/behavior identity = %+v", snapshot)
	}
	for _, name := range []string{
		"SECRET", "VLLM_API_KEY", "SENSEVOICE_API_KEY", "LD_PRELOAD", "NVIDIA_TF32_OVERRIDE",
	} {
		if _, retained := snapshot.Environment[name]; retained {
			t.Fatalf("procfs snapshot retained sensitive environment %s", name)
		}
	}
	secretChangedPayload := []byte(
		"HF_HOME=/models\x00VLLM_WORKER_MULTIPROC_METHOD=spawn\x00" +
			"CUDA_VISIBLE_DEVICES=0\x00LD_PRELOAD=/runtime/inject.so\x00" +
			"NVIDIA_TF32_OVERRIDE=0\x00VLLM_API_KEY=a\x00" +
			"SENSEVOICE_API_KEY=b\x00SECRET=c\x00",
	)
	if err := os.WriteFile(filepath.Join(root, "321", "environ"), secretChangedPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	secretChanged, err := source.Listener(context.Background(), 8000)
	if err != nil {
		t.Fatal(err)
	}
	if secretChanged.EnvironmentDigest != snapshot.EnvironmentDigest ||
		secretChanged.opaqueEnvironmentSeal == snapshot.opaqueEnvironmentSeal ||
		sameRealtimeCUProcess(snapshot, secretChanged) {
		t.Fatal("secret value was exposed publicly or omitted from the opaque process seal")
	}
	publicMaterial := realtimeCUBackendMaterial{
		ArtifactID: "fixture://environment-digest", Revision: "v1",
		Roots: []realtimeCUMaterialRoot{{Label: "runtime", Path: filepath.Join(root, "python")}},
	}
	_, publicBefore, err := hashRealtimeCUDeploymentMaterial(
		context.Background(), "environment-digest", snapshot, publicMaterial,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, publicAfterSecretChange, err := hashRealtimeCUDeploymentMaterial(
		context.Background(), "environment-digest", secretChanged, publicMaterial,
	)
	if err != nil {
		t.Fatal(err)
	}
	if publicBefore != publicAfterSecretChange {
		t.Fatal("public deployment identity exposed a secret environment value")
	}
	loaderChangedPayload := []byte(
		"HF_HOME=/models\x00VLLM_WORKER_MULTIPROC_METHOD=spawn\x00" +
			"CUDA_VISIBLE_DEVICES=0\x00LD_PRELOAD=/runtime/different.so\x00" +
			"NVIDIA_TF32_OVERRIDE=0\x00VLLM_API_KEY=a\x00" +
			"SENSEVOICE_API_KEY=b\x00SECRET=c\x00",
	)
	if err := os.WriteFile(filepath.Join(root, "321", "environ"), loaderChangedPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	environmentChanged, err := source.Listener(context.Background(), 8000)
	if err != nil {
		t.Fatal(err)
	}
	if environmentChanged.EnvironmentDigest == snapshot.EnvironmentDigest ||
		sameRealtimeCUProcess(snapshot, environmentChanged) {
		t.Fatal("complete behavior environment drift did not change the process identity")
	}
	_, publicAfterLoaderChange, err := hashRealtimeCUDeploymentMaterial(
		context.Background(), "environment-digest", environmentChanged, publicMaterial,
	)
	if err != nil {
		t.Fatal(err)
	}
	if publicAfterLoaderChange == publicBefore {
		t.Fatal("public deployment identity omitted a behavior-changing loader variable")
	}

	writeProcTCP("5252")
	writeProcess(654, "5252", "888")
	replacement, err := source.Listener(context.Background(), 8000)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.PID != 654 || replacement.StartTime != "888" || sameRealtimeCUProcess(snapshot, replacement) {
		t.Fatalf("listener replacement = %+v", replacement)
	}
}

func TestRealtimeCUQwenProcessRequiresOneImmutableLoadedRevision(t *testing.T) {
	revision := strings.Repeat("a", 40)
	modelPath := filepath.Join(
		t.TempDir(), "models--Qwen--Qwen3-VL-30B-A3B-Instruct-FP8", "snapshots", revision,
	)
	valid := realtimeCUProcessSnapshot{Arguments: []string{
		"/runtime/python", "-m", "vllm.entrypoints.openai.api_server",
		"--model", modelPath, "--served-model-name", "qwen-fast",
		"--host", "127.0.0.1", "--port", "8000", "--max-model-len", "40960",
	}}
	if err := validateRealtimeCUQwenProcess(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]string) []string{
		func(arguments []string) []string {
			index := slices.Index(arguments, "--model")
			return append(arguments[:index], arguments[index+2:]...)
		},
		func(arguments []string) []string {
			arguments[slices.Index(arguments, "--model")+1] = filepath.Join(filepath.Dir(modelPath), "main")
			return arguments
		},
		func(arguments []string) []string {
			return append(arguments, "--revision", strings.Repeat("b", 40))
		},
	} {
		arguments := mutate(slices.Clone(valid.Arguments))
		if err := validateRealtimeCUQwenProcess(realtimeCUProcessSnapshot{Arguments: arguments}); err == nil {
			t.Fatalf("Qwen process accepted mutable/ambiguous revision: %q", arguments)
		}
	}
}

func TestMeetingSenseVoiceProcessRequiresItsExactModelAndModuleLoading(t *testing.T) {
	valid := realtimeCUProcessSnapshot{
		Arguments: []string{
			"/runtime/python", "-m", "uvicorn", "server:app",
			"--host", "127.0.0.1", "--port", "8002", "--workers", "1",
		},
		Environment: map[string]string{
			"SENSEVOICE_MODEL":      meetingLocalASRModel,
			"SENSEVOICE_MODEL_PATH": "/models/sensevoice/immutable",
		},
	}
	if err := validateMeetingSenseVoiceProcess(valid); err != nil {
		t.Fatal(err)
	}
	differentModel := cloneRealtimeCUProcessSnapshot(valid)
	differentModel.Environment["SENSEVOICE_MODEL"] = realtimeCULocalASRModel
	if err := validateMeetingSenseVoiceProcess(differentModel); err == nil {
		t.Fatalf("Meeting SenseVoice process accepted Realtime-CU model %q", realtimeCULocalASRModel)
	}
	for _, extra := range [][]string{
		{"--app-dir", "/different/service"}, {"--app-dir=/different/service"},
		{"--factory"}, {"--reload"},
	} {
		changed := cloneRealtimeCUProcessSnapshot(valid)
		changed.Arguments = append(changed.Arguments, extra...)
		if err := validateMeetingSenseVoiceProcess(changed); err == nil {
			t.Fatalf("SenseVoice process accepted alternate module loading: %q", extra)
		}
	}
}

func TestRealtimeCUWhisperProcessRequiresExactImmutableSnapshotAndCommand(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "models--mobiuslabsgmbh--faster-whisper-large-v3-turbo")
	revision := strings.Repeat("a", 40)
	model := filepath.Join(repository, "snapshots", revision)
	if err := os.MkdirAll(model, 0o700); err != nil {
		t.Fatal(err)
	}
	serviceDirectory := filepath.Join(t.TempDir(), "whisper")
	if err := os.MkdirAll(serviceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(serviceDirectory, "server.py")
	if err := os.WriteFile(service, []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencyRoot := filepath.Join(t.TempDir(), "site-packages")
	if err := os.MkdirAll(dependencyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := realtimeCUProcessSnapshot{Arguments: []string{
		"/runtime/python", "-I", "-S", "-B", realtimeCUWhisperServiceArgument,
		"--model", model, "--device", "cuda", "--compute-type", "int8",
		"--language", "en", "--dependency-root", dependencyRoot, "--port", "8003",
	}}
	if err := validateRealtimeCUWhisperProcess(valid, service, []string{dependencyRoot}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func([]string) []string{
		func(arguments []string) []string { return append(arguments, "--beam-size", "5") },
		func(arguments []string) []string {
			copy := slices.Clone(arguments)
			copy[slices.Index(copy, "--language")+1] = "auto"
			return copy
		},
		func(arguments []string) []string {
			copy := slices.Clone(arguments)
			copy[slices.Index(copy, "--compute-type")+1] = "float16"
			return copy
		},
		func(arguments []string) []string {
			copy := slices.Clone(arguments)
			copy[slices.Index(copy, "--model")+1] = filepath.Join(repository, "refs", "main")
			return copy
		},
		func(arguments []string) []string {
			copy := slices.Clone(arguments)
			copy[4] = "tools/whisper/server.py"
			return copy
		},
		func(arguments []string) []string {
			copy := slices.Clone(arguments)
			copy[slices.Index(copy, "--dependency-root")+1] = "/different/dependencies"
			return copy
		},
	} {
		changed := valid
		changed.Arguments = mutation(slices.Clone(valid.Arguments))
		if err := validateRealtimeCUWhisperProcess(changed, service, []string{dependencyRoot}); err == nil {
			t.Fatalf("Whisper process accepted drifted command: %q", changed.Arguments)
		}
	}
	alternateDirectory := filepath.Join(t.TempDir(), "whisper")
	if err := os.MkdirAll(alternateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(alternateDirectory, "server.py")
	if err := os.WriteFile(alternate, []byte("# alternate fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := valid
	changed.Arguments = slices.Clone(valid.Arguments)
	changed.Arguments[4] = alternate
	if err := validateRealtimeCUWhisperProcess(changed, service, []string{dependencyRoot}); err == nil {
		t.Fatal("Whisper process accepted an alternate same-named service module")
	}
	changed = valid
	changed.Environment = map[string]string{"PYTHONPATH": "/tmp/shadow-modules"}
	if err := validateRealtimeCUWhisperProcess(changed, service, []string{dependencyRoot}); err == nil {
		t.Fatal("Whisper process accepted an alternate Python module root")
	}
}

func TestConfiguredRealtimeCUWhisperServiceRequiresPinnedReviewedBytes(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	t.Setenv(realtimeCUWhisperServicePathEnv, service)
	got, err := configuredRealtimeCUWhisperService(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != service {
		t.Fatalf("configured Whisper service = %q, want %q", got, service)
	}
	launcher, err := os.ReadFile(filepath.Join(repository, "tools", "services", "up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(launcher), realtimeCUWhisperServiceDigestV2) != 1 {
		t.Fatal("Whisper launcher does not exact-pin the reviewed service digest")
	}
	if strings.Count(string(launcher), `"${whisper_python}" -I -S -B -c`) != 2 ||
		!strings.Contains(string(launcher), `os.execv(python, [python, "-I", "-S", "-B", "/proc/self/fd/3"`) {
		t.Fatal("Whisper launcher does not isolate both its sealer and sealed service")
	}
	content, err := os.ReadFile(service)
	if err != nil {
		t.Fatal(err)
	}
	alternateDirectory := filepath.Join(t.TempDir(), "whisper")
	if err := os.MkdirAll(alternateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(alternateDirectory, "server.py")
	if err := os.WriteFile(alternate, append(content, []byte("\n# mutation\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realtimeCUWhisperServicePathEnv, alternate)
	if _, err := configuredRealtimeCUWhisperService(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "pinned reviewed implementation") {
		t.Fatalf("alternate Whisper service error = %v", err)
	}
}

func TestConfiguredRealtimeCUWhisperDependencyRootsRequireExactCanonicalDirectories(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	for _, root := range []string{first, second} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(realtimeCUWhisperDependencyRootsEnv, strings.Join([]string{first, second}, string(os.PathListSeparator)))
	got, err := configuredRealtimeCUWhisperDependencyRoots()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{first, second}) {
		t.Fatalf("Whisper dependency roots = %q", got)
	}
	for name, value := range map[string]string{
		"empty":     "",
		"relative":  "relative",
		"duplicate": strings.Join([]string{first, first}, string(os.PathListSeparator)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(realtimeCUWhisperDependencyRootsEnv, value)
			if _, err := configuredRealtimeCUWhisperDependencyRoots(); err == nil {
				t.Fatal("invalid Whisper dependency roots were accepted")
			}
		})
	}
	symlink := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(first, symlink); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realtimeCUWhisperDependencyRootsEnv, symlink)
	if _, err := configuredRealtimeCUWhisperDependencyRoots(); err == nil {
		t.Fatal("symlinked Whisper dependency root was accepted")
	}
}

func TestHashRealtimeCUFileRejectsPathSwapEvenWhenMetadataMatches(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.bin")
	replacement := filepath.Join(directory, "replacement.bin")
	if err := os.WriteFile(target, []byte("aaaaaaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("bbbbbbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := snapshotRealtimeCUFile("model/weights", target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, expected.identity.ModTime(), expected.identity.ModTime()); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(directory, "saved.bin")
	_, err = hashRealtimeCUFileWithOpen(context.Background(), expected, func(path string) (*os.File, error) {
		if err := os.Rename(target, saved); err != nil {
			return nil, err
		}
		if err := os.Rename(replacement, target); err != nil {
			return nil, err
		}
		file, openErr := os.Open(path)
		if err := os.Rename(target, replacement); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := os.Rename(saved, target); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, openErr
	})
	if err == nil || (!strings.Contains(err.Error(), "handle differs") &&
		!strings.Contains(err.Error(), "changed while hashing")) {
		t.Fatalf("path-swap hash error = %v", err)
	}
}

func TestLocalRealtimeCUBackendAttestationRejectsInterpassMaterialDrift(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deployment")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	weights := filepath.Join(root, "weights.bin")
	if err := os.WriteFile(weights, []byte("first-pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &mutableRealtimeCUProcessSource{snapshot: realtimeCUProcessSnapshot{
		PID: 321, StartTime: "start", Arguments: []string{"/runtime/python", "-m", "fixture"},
		Executable: "/runtime/python", WorkingDir: "/runtime", Environment: map[string]string{},
	}}
	attestor, err := newLocalRealtimeCUBackendAttestor(realtimeCUBackendAttestorConfig{
		Name: "interpass-drift", Port: 12345, ProcessSource: source,
		ValidateProcess: func(realtimeCUProcessSnapshot) error { return nil },
		Probe: func(context.Context, realtimeCUProcessSnapshot, realtimeCUBackendMaterial) error {
			return nil
		},
		Material: func(context.Context, realtimeCUProcessSnapshot) (realtimeCUBackendMaterial, error) {
			return realtimeCUBackendMaterial{
				ArtifactID: "fixture://interpass-drift", Revision: "v1",
				Roots: []realtimeCUMaterialRoot{{Label: "model", Path: root}},
			}, nil
		},
		afterInitialHash: func() error {
			return os.WriteFile(weights, []byte("second-pass"), 0o600)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := attestor.Attest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "changed between independent hashes") {
		t.Fatalf("interpass deployment drift error = %v", err)
	}
}

func TestRealtimeCURuntimePackageRootsUseListenerExecutableAndWorkingDirectory(t *testing.T) {
	working := t.TempDir()
	marker := filepath.Join(t.TempDir(), "sitecustomize-executed")
	if err := os.WriteFile(
		filepath.Join(working, "sitecustomize.py"),
		[]byte(fmt.Sprintf("open(%q, 'w').write('executed')\n", marker)), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(working, "attestedfixture")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "__init__.py"), []byte("VALUE = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join("..", "..", ".runtime", "sensevoice", "bin", "python")
	python, err := filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(python); err != nil || !info.Mode().IsRegular() {
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Skip("Python is unavailable")
		}
		python, err = filepath.Abs(python)
		if err != nil {
			t.Fatal(err)
		}
	}
	process := realtimeCUProcessSnapshot{
		Arguments: []string{python}, Executable: python, ExecutableHandle: python,
		WorkingDir: working, Environment: map[string]string{"HOME": working, "PYTHONPATH": working},
	}
	roots, err := realtimeCURuntimePackageRoots(context.Background(), process, []string{"attestedfixture"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, root := range roots {
		if root.Label == "runtime-attestedfixture" && root.Path == module {
			found = true
		}
	}
	if !found {
		t.Fatalf("runtime package roots = %+v", roots)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated package resolver executed sitecustomize: %v", err)
	}
	process.ExecutableHandle = "/bin/false"
	if _, err := realtimeCURuntimePackageRoots(context.Background(), process, []string{"attestedfixture"}); err == nil {
		t.Fatal("runtime package resolver executed argv[0] instead of the listener executable handle")
	}
	launcher := filepath.Join(t.TempDir(), "python")
	if err := os.Symlink(python, launcher); err != nil {
		t.Fatal(err)
	}
	process.Arguments[0] = launcher
	process.ExecutableHandle = python
	if _, err := realtimeCURuntimePackageRoots(context.Background(), process, []string{"attestedfixture"}); err != nil {
		t.Fatalf("same-inode listener launcher refusal = %v", err)
	}
	if err := os.Remove(launcher); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/false", launcher); err != nil {
		t.Fatal(err)
	}
	if _, err := realtimeCURuntimePackageRoots(context.Background(), process, []string{"attestedfixture"}); err == nil ||
		!strings.Contains(err.Error(), "differs from the listener executable") {
		t.Fatalf("replaced launcher path error = %v", err)
	}
}

func TestLocalRealtimeCUDeploymentSessionRevalidationLatencyOptIn(t *testing.T) {
	if os.Getenv("OPENREALTIME_CU_LOCAL_DEPLOYMENT_E2E") != "1" {
		t.Skip("set OPENREALTIME_CU_LOCAL_DEPLOYMENT_E2E=1 with strict local backends running")
	}
	verifier, err := newLocalRealtimeCUDeploymentVerifier()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scoped := verifier.(realtimeCUScopedDeploymentVerifier)
	started := time.Now()
	if err := scoped.VerifyModel(context.Background(), identity.Model); err != nil {
		t.Fatal(err)
	}
	if err := scoped.VerifyObserver(context.Background(), identity.ASR, identity.Vision); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("one session deployment revalidation took %s, limit 5s", elapsed)
	}
}

func BenchmarkLocalRealtimeCUDeploymentSessionRevalidation(b *testing.B) {
	if os.Getenv("OPENREALTIME_CU_LOCAL_DEPLOYMENT_E2E") != "1" {
		b.Skip("set OPENREALTIME_CU_LOCAL_DEPLOYMENT_E2E=1 with strict local backends running")
	}
	verifier, err := newLocalRealtimeCUDeploymentVerifier()
	if err != nil {
		b.Fatal(err)
	}
	identity, err := verifier.Resolve(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	scoped := verifier.(realtimeCUScopedDeploymentVerifier)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := scoped.VerifyModel(context.Background(), identity.Model); err != nil {
			b.Fatal(err)
		}
		if err := scoped.VerifyObserver(context.Background(), identity.ASR, identity.Vision); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSenseVoiceServiceLoadedModelDigestMatchesIndependentGoVerifier(t *testing.T) {
	root := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string][]byte{
		filepath.Join(root, "weights.bin"):      []byte("weights"),
		filepath.Join(root, "nested", "config"): []byte("configuration"),
	} {
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := digestRealtimeCULoadedModel(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	python := filepath.Join(repository, ".runtime", "sensevoice", "bin", "python")
	if info, err := os.Stat(python); err != nil || !info.Mode().IsRegular() {
		t.Skip("local SenseVoice Python runtime is unavailable")
	}
	command := exec.Command(python, "-B", "-c",
		"import server,sys; print(server.loaded_model_digest(sys.argv[1])); print('|'.join(server.service_module_identity()))", root)
	command.Dir = filepath.Join(repository, "deploy", "sensevoice")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 || lines[0] != want {
		t.Fatalf("SenseVoice service output = %q, independent model digest = %s", lines, want)
	}
	servicePath := filepath.Join(repository, "deploy", "sensevoice", "server.py")
	serviceSeal, err := snapshotRealtimeCUFile("service/server.py", servicePath)
	if err != nil {
		t.Fatal(err)
	}
	serviceDigest, err := hashRealtimeCUFile(context.Background(), serviceSeal)
	if err != nil {
		t.Fatal(err)
	}
	if lines[1] != servicePath+"|"+serviceDigest {
		t.Fatalf("SenseVoice boot service identity = %q, want %q|%q",
			lines[1], servicePath, serviceDigest)
	}
}

func TestWhisperServiceLoadedModelDigestMatchesIndependentGoVerifier(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "snapshot")
	blobs := filepath.Join(directory, "blobs")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "weights"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(blobs, "weights"), filepath.Join(root, "model.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "config"), []byte("configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := digestRealtimeCULoadedModel(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	python := filepath.Join(repository, ".runtime", "sensevoice", "bin", "python")
	if info, err := os.Stat(python); err != nil || !info.Mode().IsRegular() {
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Skip("Python is unavailable")
		}
	}
	command := exec.Command(python, "-B", "-c",
		"import server,sys; print(server.loaded_model_digest(sys.argv[1])); print('|'.join(server.service_module_identity()))", root)
	command.Dir = filepath.Join(repository, "tools", "whisper")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 || lines[0] != want {
		t.Fatalf("Whisper service output = %q, independent model digest = %s", lines, want)
	}
	servicePath := filepath.Join(repository, "tools", "whisper", "server.py")
	serviceSeal, err := snapshotRealtimeCUFile("service/server.py", servicePath)
	if err != nil {
		t.Fatal(err)
	}
	serviceDigest, err := hashRealtimeCUFile(context.Background(), serviceSeal)
	if err != nil {
		t.Fatal(err)
	}
	if lines[1] != servicePath+"|"+serviceDigest {
		t.Fatalf("Whisper boot service identity = %q, want %q|%q",
			lines[1], servicePath, serviceDigest)
	}
}

func TestWhisperServiceDependencyDigestMatchesIndependentGoVerifier(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	root := t.TempDir()
	module := filepath.Join(root, "fixture_dependency")
	if err := os.MkdirAll(filepath.Join(module, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, payload := range map[string][]byte{
		filepath.Join(module, "__init__.py"):        []byte("VALUE = 1\n"),
		filepath.Join(module, "nested", "data.bin"): []byte("dependency data"),
	} {
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original := realtimeCUWhisperRuntimePackages
	realtimeCUWhisperRuntimePackages = []string{"fixture_dependency"}
	t.Cleanup(func() { realtimeCUWhisperRuntimePackages = original })
	want, err := digestRealtimeCUWhisperDependencies(context.Background(), []realtimeCUMaterialRoot{
		{Label: "runtime-fixture_dependency", Path: module},
	}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	program := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
if list(server.WHISPER_DEPENDENCY_MODULES) != json.loads(sys.argv[3]):
    raise SystemExit("Go and Python dependency selections differ")
server.WHISPER_DEPENDENCY_MODULES = ("fixture_dependency",)
print(json.dumps(server.WHISPER_DEPENDENCY_MODULES))
print(server.dependency_material_digest([sys.argv[2]]))
`
	selection, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-I", "-S", "-B", "-c", program, service, root, string(selection))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 2 || lines[0] != `["fixture_dependency"]` {
		t.Fatalf("Whisper dependency selection output = %q", lines)
	}
	if got := lines[1]; got != want {
		t.Fatalf("Whisper dependency digest = %q, independent Go digest = %q", got, want)
	}
}

func TestWhisperDependencyImportGuardNeverConsumesPreexistingBytecode(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	program := `
import importlib, importlib.util, os, pathlib, py_compile, sys, types
root = pathlib.Path(sys.argv[2])
source = root / "cached_dependency.py"
source.write_text("VALUE = 'EVIL'\n")
before = source.stat()
py_compile.compile(str(source), doraise=True)
source.write_text("VALUE = 'GOOD'\n")
os.utime(source, ns=(before.st_atime_ns, before.st_mtime_ns))
sys.path.insert(0, str(root))
cached = importlib.import_module("cached_dependency")
if cached.VALUE != "EVIL":
    raise SystemExit("stale-bytecode fixture was not valid")
del sys.modules["cached_dependency"]
sys.path.remove(str(root))

spec = importlib.util.spec_from_file_location("reviewed_whisper_server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
server.WHISPER_DEPENDENCY_MODULES = ("cached_dependency",)
outside = types.ModuleType("outside_deleted_service")
outside.__file__ = "/memfd:outside-deleted-service (deleted)"
sys.modules[outside.__name__] = outside
server.verify_loaded_dependency_modules([str(root)])
escape_path = root.parent / (root.name + "-escape.py")
escape_path.write_text("VALUE = 'OUTSIDE'\n")
escaped = types.ModuleType("cached_dependency.escaped")
escaped.__file__ = str(root / ".." / escape_path.name)
sys.modules[escaped.__name__] = escaped
try:
    server.verify_loaded_dependency_modules([str(root)])
except RuntimeError as error:
    if "escaped its selected roots" not in str(error):
        raise
else:
    raise SystemExit("lexical dependency escape was accepted")
del sys.modules[escaped.__name__]
server.install_dependency_import_guard([str(root)])
sys.path.insert(0, str(root))
guarded = importlib.import_module("cached_dependency")
if guarded.VALUE != "GOOD":
    raise SystemExit("Whisper dependency guard consumed stale bytecode")
`
	root := t.TempDir()
	command := exec.Command(python, "-I", "-S", "-B", "-c", program, service, root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("source-only dependency import guard failed: %v\n%s", err, output)
	}
}

func TestWhisperDependencyDigestRejectsSymlinkAndExternalHardLinkAliases(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	original := realtimeCUWhisperRuntimePackages
	realtimeCUWhisperRuntimePackages = []string{"fixture_dependency"}
	t.Cleanup(func() { realtimeCUWhisperRuntimePackages = original })

	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			module := filepath.Join(root, "fixture_dependency")
			if err := os.Mkdir(module, 0o700); err != nil {
				t.Fatal(err)
			}
			externalDirectory := t.TempDir()
			external := filepath.Join(externalDirectory, "external.py")
			if err := os.WriteFile(external, []byte("VALUE = 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(module, "__init__.py")
			var aliasErr error
			if kind == "symlink" {
				aliasErr = os.Symlink(external, alias)
			} else {
				aliasErr = os.Link(external, alias)
			}
			if aliasErr != nil {
				t.Skipf("create %s fixture: %v", kind, aliasErr)
			}
			if _, err := digestRealtimeCUWhisperDependencies(
				context.Background(),
				[]realtimeCUMaterialRoot{{Label: "runtime-fixture_dependency", Path: module}},
				[]string{root},
			); err == nil {
				t.Fatalf("Go dependency verifier accepted an external %s alias", kind)
			}
			program := `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("reviewed_whisper_server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
server.WHISPER_DEPENDENCY_MODULES = ("fixture_dependency",)
try:
    server.dependency_material_digest([sys.argv[2]])
except RuntimeError:
    pass
else:
    raise SystemExit("Python dependency verifier accepted an external alias")
`
			command := exec.Command(python, "-I", "-S", "-B", "-c", program, service, root)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("Python %s alias guard failed: %v\n%s", kind, err, output)
			}
		})
	}
}

func TestWhisperServiceRejectsLoaderWindowModelSwap(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	revision := strings.Repeat("b", 40)
	model := filepath.Join(t.TempDir(), revision)
	if err := os.MkdirAll(model, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "model.bin"), []byte("GOOD"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakePackage := t.TempDir()
	siteMarker := filepath.Join(t.TempDir(), "sitecustomize-executed")
	fakeModule := `
import pathlib

class _Info:
    language = "en"

class WhisperModel:
    def __init__(self, path, **_kwargs):
        target = pathlib.Path(path) / "model.bin"
        good = target.read_bytes()
        target.chmod(0o600)
        target.write_bytes(b"EVIL")
        self.loaded = target.read_bytes()
        target.write_bytes(good)
        target.chmod(0o400)

    def transcribe(self, *_args, **_kwargs):
        return iter(()), _Info()
`
	if err := os.WriteFile(filepath.Join(fakePackage, "faster_whisper.py"), []byte(fakeModule), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(fakePackage, "sitecustomize.py"),
		[]byte(fmt.Sprintf("open(%q, 'w').write('executed')\n", siteMarker)), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	program := `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
server.WHISPER_DEPENDENCY_MODULES = ("faster_whisper",)
try:
    server.Recogniser(sys.argv[2], "cuda", "float16", "en", [sys.argv[3]])
except RuntimeError as error:
    if "changed after sealing" not in str(error):
        raise
else:
    raise SystemExit("loader-window replacement was accepted")
`
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	command := exec.Command(python, "-I", "-S", "-B", "-c", program, service, model, fakePackage)
	command.Dir = repository
	command.Env = []string{"HOME=" + t.TempDir(), "PYTHONPATH=" + fakePackage}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("loader-window guard failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(siteMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated Whisper load executed sitecustomize: %v", err)
	}
}

func TestWhisperServiceRejectsLoaderWindowCampaignAncestorSwap(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	revision := strings.Repeat("d", 40)
	model := filepath.Join(t.TempDir(), revision)
	if err := os.MkdirAll(model, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "model.bin"), []byte("GOOD"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakePackage := t.TempDir()
	fakeModule := `
import pathlib, shutil

class _Info:
    language = "en"

class WhisperModel:
    def __init__(self, path, **_kwargs):
        model = pathlib.Path(path)
        campaign = model.parent
        moved = campaign.with_name(campaign.name + ".moved")
        campaign.rename(moved)
        replacement = campaign / "model"
        replacement.mkdir(parents=True)
        (replacement / "model.bin").write_bytes(b"EVIL")
        self.loaded = (model / "model.bin").read_bytes()
        shutil.rmtree(campaign)
        moved.rename(campaign)

    def transcribe(self, *_args, **_kwargs):
        return iter(()), _Info()
`
	if err := os.WriteFile(filepath.Join(fakePackage, "faster_whisper.py"), []byte(fakeModule), 0o600); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	program := `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
server.WHISPER_DEPENDENCY_MODULES = ("faster_whisper",)
try:
    server.Recogniser(sys.argv[2], "cuda", "float16", "en", [sys.argv[3]])
except RuntimeError as error:
    if "changed after sealing" not in str(error):
        raise
else:
    raise SystemExit("campaign ancestor replacement was accepted")
`
	command := exec.Command(python, "-I", "-S", "-B", "-c", program, service, model, fakePackage)
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("campaign ancestor guard failed: %v\n%s", err, output)
	}
}

func TestWhisperServiceRejectsDependencyModuleSwapAndRestoreDuringImport(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	revision := strings.Repeat("e", 40)
	model := filepath.Join(t.TempDir(), revision)
	if err := os.MkdirAll(model, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "model.bin"), []byte("GOOD"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakePackage := t.TempDir()
	fakeModule := `
import pathlib
_source = pathlib.Path(__file__)
_source.write_text("# restored GOOD dependency bytes\n")

class _Info:
    language = "en"

class WhisperModel:
    behavior = "EVIL"

    def __init__(self, *_args, **_kwargs):
        pass

    def transcribe(self, *_args, **_kwargs):
        return iter(()), _Info()
`
	modulePath := filepath.Join(fakePackage, "faster_whisper.py")
	if err := os.WriteFile(modulePath, []byte(fakeModule), 0o600); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	service := filepath.Join(repository, "tools", "whisper", "server.py")
	program := `
import importlib.util, pathlib, sys
spec = importlib.util.spec_from_file_location("server", sys.argv[1])
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
server.WHISPER_DEPENDENCY_MODULES = ("faster_whisper",)
try:
    server.Recogniser(sys.argv[2], "cuda", "float16", "en", [sys.argv[3]])
except RuntimeError as error:
    if "dependency material changed after sealing" not in str(error):
        raise
else:
    raise SystemExit("dependency swap and restore was accepted")
if pathlib.Path(sys.argv[3], "faster_whisper.py").read_text() != "# restored GOOD dependency bytes\n":
    raise SystemExit("dependency fixture did not restore its benign bytes")
`
	command := exec.Command(python, "-I", "-S", "-B", "-c", program, service, model, fakePackage)
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dependency load-window guard failed: %v\n%s", err, output)
	}
}

func TestWhisperExistingListenerVerifierRejectsStaleProcessAndHTTPFailure(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is unavailable")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not ready", http.StatusServiceUnavailable)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	serviceDirectory := filepath.Join(repository, "tools", "whisper")
	for name, program := range map[string]string{
		"HTTP 503": `
import server, sys
try:
    server.read_health(sys.argv[1])
except Exception:
    pass
else:
    raise SystemExit("HTTP 503 was accepted as healthy")
`,
		"stale listener process": `
import server, types, sys
server.WHISPER_DEPENDENCY_MODULES = ("faster_whisper",)
arguments = types.SimpleNamespace(
    port=int(sys.argv[1]), model=sys.argv[2], device="cuda", compute_type="int8",
    language="en", dependency_root=[sys.argv[2]], working_directory=sys.argv[3],
)
try:
    server.verify_existing_listener(arguments)
except RuntimeError as error:
    if "command differs" not in str(error):
        raise
else:
    raise SystemExit("stale listener was adopted")
`,
	} {
		t.Run(name, func(t *testing.T) {
			model := filepath.Join(t.TempDir(), strings.Repeat("c", 40))
			if err := os.MkdirAll(model, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(model, "model.bin"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(model, "faster_whisper.py"), []byte("# fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			arguments := []string{"-B", "-c", program}
			if name == "HTTP 503" {
				arguments = append(arguments, fmt.Sprintf("http://127.0.0.1:%d/health", port))
			} else {
				arguments = append(arguments, strconv.Itoa(port), model, repository)
			}
			command := exec.Command(python, arguments...)
			command.Dir = serviceDirectory
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("existing-listener guard failed: %v\n%s", err, output)
			}
		})
	}
}

func TestRequireRealtimeCUProcessArgumentsRejectsAmbiguousSelections(t *testing.T) {
	pairs := map[string]string{"--model": "selected", "--port": "8000"}
	sequence := []string{"-m", "service"}
	valid := []string{"/python", "-m", "service", "--model", "selected", "--port", "8000"}
	if err := requireRealtimeCUProcessArguments(valid, pairs, sequence); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"/python", "-m", "service", "--model", "selected", "--model", "other", "--port", "8000"},
		{"/python", "-m", "service", "--model=selected", "--port", "8000"},
		{"/python", "-m", "service", "-m", "service", "--model", "selected", "--port", "8000"},
	} {
		if err := requireRealtimeCUProcessArguments(arguments, pairs, sequence); err == nil {
			t.Fatalf("ambiguous process arguments accepted: %q", arguments)
		}
	}
}
