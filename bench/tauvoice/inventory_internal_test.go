package tauvoice

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/testgate"
)

func TestInventoryLoaderUsesCleanPinnedInputsAndExplicitBaseSplit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		testgate.Missing(t, "git")
	}
	root := t.TempDir()
	mustGit(t, root, "init", "--quiet")
	mustGit(t, root, "config", "user.email", "inventory-test@openrealtime.invalid")
	mustGit(t, root, "config", "user.name", "OpenRealtime inventory test")
	for _, source := range taskInventoryGitPaths {
		path := filepath.Join(root, filepath.FromSlash(source))
		if filepath.Ext(path) == "" {
			path = filepath.Join(path, "inventory-source.txt")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("pinned\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, root, "add", ".")
	mustGit(t, root, "commit", "--quiet", "-m", "pinned task universe")
	revision := strings.TrimSpace(mustGit(t, root, "rev-parse", "HEAD"))

	verified, err := verifyTaskInventoryCheckout(context.Background(), root, revision)
	if err != nil {
		t.Fatalf("verify clean checkout: %v", err)
	}
	if verified != root {
		t.Fatalf("verified root = %q, want %q", verified, root)
	}
	if _, err := verifyTaskInventoryCheckout(context.Background(), root, strings.Repeat("0", 40)); err == nil ||
		!strings.Contains(err.Error(), "pinned") {
		t.Fatalf("moved revision error = %v", err)
	}

	rawTasks := validInternalTaskIdentities()
	payload, err := json.Marshal(rawTasks)
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "inventory-command.txt")
	stub := filepath.Join(root, "fake-python")
	script := "#!/bin/sh\n" +
		"{ printf '%s\\n' \"$TAU2_DATA_DIR\"; printf '%s\\n' \"$PYTHONPATH\"; printf '%s\\n' \"$@\"; } > " + shellQuote(record) + "\n" +
		"printf '%s\\n' " + shellQuote(string(payload)) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAU2_DATA_DIR", "/wrong/data")
	t.Setenv("PYTHONPATH", "/wrong/src")
	tasks, err := exportTaskIdentities(context.Background(), root, stub)
	if err != nil {
		t.Fatalf("export task identities: %v", err)
	}
	if len(tasks) != TaskCount {
		t.Fatalf("exported %d tasks, want %d", len(tasks), TaskCount)
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	command := string(recorded)
	if !strings.Contains(command, filepath.Join(root, "data")+"\n") ||
		!strings.Contains(command, filepath.Join(root, "src")+"\n") {
		t.Fatalf("upstream paths were not pinned explicitly:\n%s", command)
	}
	if !strings.Contains(command, `load_tasks(domain, "base")`) {
		t.Fatalf("export did not explicitly select the base split:\n%s", command)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeStub, err := filepath.Rel(workingDirectory, stub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exportTaskIdentities(context.Background(), root, relativeStub); err != nil {
		t.Fatalf("export with caller-relative interpreter: %v", err)
	}

	// A local edit to any input that can select or interpret a task invalidates
	// the inventory before Python is allowed to run.
	dirty := filepath.Join(root, "data", "tau2", "domains", "airline", "tasks.json")
	if err := os.WriteFile(dirty, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyTaskInventoryCheckout(context.Background(), root, revision); err == nil ||
		!strings.Contains(err.Error(), "differ") {
		t.Fatalf("dirty task source error = %v", err)
	}
}

func TestInventoryCheckoutRequiresRootAndContext(t *testing.T) {
	if _, err := verifyTaskInventoryCheckout(nil, t.TempDir(), PinnedRevision); err == nil ||
		!strings.Contains(err.Error(), "context") {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := verifyTaskInventoryCheckout(context.Background(), "", PinnedRevision); err == nil ||
		!strings.Contains(err.Error(), "checkout") {
		t.Fatalf("empty checkout error = %v", err)
	}
}

func validInternalTaskIdentities() []TaskIdentity {
	var result []TaskIdentity
	for _, domain := range []struct {
		name  string
		count int
	}{
		{name: "airline", count: AirlineTaskCount},
		{name: "retail", count: RetailTaskCount},
		{name: "telecom", count: TelecomTaskCount},
	} {
		for index := 0; index < domain.count; index++ {
			result = append(result, TaskIdentity{
				Domain: domain.name, ID: fmt.Sprintf("task-%03d", index),
			})
		}
	}
	return result
}

func mustGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
