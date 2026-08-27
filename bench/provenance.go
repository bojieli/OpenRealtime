// Package bench is the measurement program's harness.
//
// The claim this project wants to make is not that four voice stacks are
// supported - that is a release gate - but what each one is worth, measured the
// same way. This package is the "same way".
//
// Its rules are not conveniences. Every one of them exists because a
// measurement program without it produces numbers that look like evidence and
// are not:
//
//   - No incomplete cell is reported. A partially executed cell is not a
//     smaller result, it is a different one.
//   - Every cell declares its source revision and executable hash. A number
//     that cannot be traced to a build is a number that will eventually be
//     wrong about which build it describes.
//   - Latency claims carry distributions. A mean without its tail describes a
//     system nobody is using.
//   - Negative results publish. Suppressing them is how a measured claim
//     becomes a marketing claim.
//   - No cross-suite synthesis. Two suites measuring different things do not
//     average into one number.
package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Provenance identifies what produced a result.
type Provenance struct {
	// Revision is the source revision the executable was built from.
	Revision string `json:"revision"`
	// Modified reports that the working tree had uncommitted changes. A result
	// from a modified tree is not reproducible and says so rather than
	// pretending its revision describes it.
	Modified bool `json:"modified"`
	// ExecutableSHA256 identifies the binary itself, which is what actually
	// ran. Two builds of one revision can differ.
	ExecutableSHA256 string `json:"executable_sha256"`
	// Machine is where it ran. An efficiency or latency number without one is
	// not a number.
	Machine Machine `json:"machine"`
	// StartedAt and FinishedAt bound the run.
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// Machine describes the host.
type Machine struct {
	CPU       string `json:"cpu,omitempty"`
	Cores     int    `json:"cores"`
	GPU       string `json:"gpu,omitempty"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	GoVersion string `json:"go_version"`
	Hostname  string `json:"hostname,omitempty"`
}

// Capture reads the provenance of the running process.
func Capture() Provenance {
	provenance := Provenance{
		Machine:   describeMachine(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	provenance.Revision, provenance.Modified = readRevision()
	if executable, err := os.Executable(); err == nil {
		if digest, err := hashFile(executable); err == nil {
			provenance.ExecutableSHA256 = digest
		}
	}
	return provenance
}

// Complete stamps the finish time.
func (provenance Provenance) Complete() Provenance {
	provenance.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	return provenance
}

// Reproducible reports whether a result can be traced to a build.
//
// A run from a modified tree, or one whose executable could not be hashed, is
// still useful while developing and must not be published as evidence. Saying
// so here is cheaper than discovering it later from a number that will not
// reproduce.
func (provenance Provenance) Reproducible() error {
	if strings.TrimSpace(provenance.Revision) == "" {
		return errors.New("no source revision: this build cannot be traced")
	}
	if provenance.Modified {
		return errors.New("the working tree was modified: this run is not reproducible")
	}
	if strings.TrimSpace(provenance.ExecutableSHA256) == "" {
		return errors.New("the executable could not be hashed")
	}
	return nil
}

func readRevision() (string, bool) {
	command := exec.Command("git", "rev-parse", "HEAD")
	revision, err := command.Output()
	if err != nil {
		return "", false
	}
	status, err := exec.Command("git", "status", "--porcelain").Output()
	return strings.TrimSpace(string(revision)), len(strings.TrimSpace(string(status))) > 0
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func describeMachine() Machine {
	result := Machine{
		Cores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		GoVersion: runtime.Version(),
	}
	result.Hostname, _ = os.Hostname()
	if payload, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(payload), "\n") {
			if name, value, found := strings.Cut(line, ":"); found &&
				strings.TrimSpace(name) == "model name" {
				result.CPU = strings.TrimSpace(value)
				break
			}
		}
	}
	// A machine description must not make a benchmark depend on the health of
	// the accelerator control plane. nvidia-smi can enter uninterruptible kernel
	// wait after a driver fault, which used to stop a run before its first task
	// merely because provenance wanted the card's name. Linux exposes that
	// immutable identity directly; an absent file means the GPU is unknown, not
	// that measurement should execute another program and wait indefinitely.
	result.GPU = nvidiaGPUFromProc("/proc/driver/nvidia/gpus")
	return result
}

func nvidiaGPUFromProc(root string) string {
	paths, err := filepath.Glob(filepath.Join(root, "*", "information"))
	if err != nil {
		return ""
	}
	for _, path := range paths {
		payload, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(payload), "\n") {
			name, value, found := strings.Cut(line, ":")
			if found && strings.TrimSpace(name) == "Model" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

// Fingerprint is a short, stable identity for a configuration, so two runs of
// the same cell can be recognised as the same cell.
func Fingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:8])
}

// String renders provenance for a log line.
func (provenance Provenance) String() string {
	revision := provenance.Revision
	if len(revision) > 12 {
		revision = revision[:12]
	}
	modified := ""
	if provenance.Modified {
		modified = " (modified)"
	}
	return fmt.Sprintf("%s%s on %s", revision, modified, provenance.Machine.Hostname)
}
