package sidecar_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/internal/testgate"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// The reference sidecars are the documented way to run Qwen3-Omni,
// MiniCPM-o, and Moshi, and each accepts --mock so its plumbing can be
// verified without a model. Nothing ran them: the Python suite covers the
// framing library and the Qwen adapter's parsing, and the Go suite drove a Go
// echo sidecar, so a reference sidecar could stop speaking the protocol it is
// shipped for and every gate would stay green. This runs each as the engine
// would, at version 1 and at the highest version it declares.
func TestReferenceSidecarsPassConformanceInMockMode(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		testgate.Missing(t, "python3")
	}
	if out, err := exec.Command(python, "-c", "import numpy").CombinedOutput(); err != nil {
		t.Logf("numpy import: %s", out)
		testgate.Missing(t, "python3 numpy (pip install -r sidecars/requirements-test.txt)")
	}
	root, err := filepath.Abs(filepath.Join("..", "sidecars"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		script   string
		versions []int
		required int
	}{
		{script: "moshi_sidecar.py", versions: []int{1, sidecar.VersionInteraction}},
		{script: "personaplex_sidecar.py", versions: []int{1, sidecar.VersionInteraction}},
		{script: "minicpm_o_sidecar.py", versions: []int{1, sidecar.VersionInteraction}},
		{script: "minicpm_o_duplex_sidecar.py", versions: []int{1, sidecar.VersionInteraction}},
		{script: "voicechat_sidecar.py", versions: []int{1, sidecar.VersionMultimodal}},
		{script: "qwen3_omni_sidecar.py", versions: []int{1, sidecar.VersionMultimodal}},
	}
	for _, testCase := range cases {
		for _, version := range testCase.versions {
			name := testCase.script + "/v" + string(rune('0'+version))
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				report := sidecar.RunConformance(ctx, sidecar.ConformanceOptions{
					Config: sidecar.Config{
						Command:         []string{python, filepath.Join(root, testCase.script), "--mock"},
						Environment:     []string{"PYTHONPATH=" + root, "PYTHONDONTWRITEBYTECODE=1"},
						ProtocolVersion: version,
						Logf:            t.Logf,
					},
					SpeechSeconds: 0.5, TurnTimeout: 30 * time.Second,
				})
				if !report.Passed {
					t.Fatalf("%s must pass protocol v%d conformance in mock mode: %v",
						testCase.script, version, report.Failures)
				}
				if report.Version != version {
					t.Fatalf("%s negotiated v%d when v%d was selected", testCase.script, report.Version, version)
				}
				if len(report.Checks) < 15 {
					t.Fatalf("%s v%d ran only %d checks; the suite is larger than that",
						testCase.script, version, len(report.Checks))
				}
			})
		}
	}
}
