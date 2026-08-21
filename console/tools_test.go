package console_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/console"
)

func newHost(t *testing.T, tools ...string) (*console.Host, string) {
	t.Helper()
	root := t.TempDir()
	// t.TempDir can hand back a path through a symlink, and the host resolves
	// its root, so the test compares against the resolved form or every
	// containment assertion is about the wrong string.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve the temporary root: %v", err)
	}
	if len(tools) == 0 {
		tools = []string{"read_file", "list_directory", "search_files", "write_file", "run_command"}
	}
	host, err := console.NewHost(console.HostConfig{
		Root: resolved, Enabled: tools, CommandTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	return host, resolved
}

func run(t *testing.T, host *console.Host, name string, arguments any) (string, error) {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return host.Run(context.Background(), name, encoded)
}

func TestToolsReadWriteAndSearchInsideTheRoot(t *testing.T) {
	t.Parallel()
	host, root := newHost(t)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	content, err := run(t, host, "read_file", map[string]any{"path": "notes.txt"})
	if err != nil || !strings.Contains(content, "beta") {
		t.Fatalf("read_file: %q %v", content, err)
	}

	listing, err := run(t, host, "list_directory", map[string]any{})
	if err != nil || !strings.Contains(listing, "notes.txt") {
		t.Fatalf("list_directory: %q %v", listing, err)
	}

	matches, err := run(t, host, "search_files", map[string]any{"pattern": "^be"})
	if err != nil || !strings.Contains(matches, "notes.txt:2") {
		t.Fatalf("search_files: %q %v", matches, err)
	}

	written, err := run(t, host, "write_file",
		map[string]any{"path": "nested/new.txt", "content": "hello"})
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if !strings.Contains(written, "nested/new.txt") {
		t.Fatalf("write_file should report where it wrote: %q", written)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "nested", "new.txt"))
	if err != nil || string(onDisk) != "hello" {
		t.Fatalf("the file was not written: %q %v", onDisk, err)
	}
}

// The root is the whole of the blast radius, so every way out of it is a
// defect rather than an inconvenience.
func TestEveryPathResolvesInsideTheRoot(t *testing.T) {
	t.Parallel()
	host, root := newHost(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A symlink inside the root pointing out of it is the case a prefix test
	// on the unresolved path misses entirely.
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "escape-dir")); err != nil {
		t.Fatalf("link a directory: %v", err)
	}

	for name, path := range map[string]string{
		"a parent traversal":       "../secret.txt",
		"a deep traversal":         "nested/../../secret.txt",
		"an absolute path":         outside,
		"a symlink to a file":      "escape.txt",
		"a symlink to a directory": "escape-dir/secret.txt",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, host, "read_file", map[string]any{"path": path}); err == nil {
				t.Fatalf("reading %q escaped the root", path)
			}
			// Writing has to be refused through the same check, and a path
			// that does not exist yet is exactly where a containment test is
			// easy to get wrong.
			if _, err := run(t, host, "write_file",
				map[string]any{"path": path, "content": "x"}); err == nil {
				t.Fatalf("writing %q escaped the root", path)
			}
		})
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "not yours" {
		t.Fatalf("a refused write still changed the file: %q %v", content, err)
	}
}

func TestWritingThroughANewDirectoryStaysInsideTheRoot(t *testing.T) {
	t.Parallel()
	host, root := newHost(t)
	if _, err := run(t, host, "write_file",
		map[string]any{"path": "a/b/c/deep.txt", "content": "fine"}); err != nil {
		t.Fatalf("write into new directories: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "b", "c", "deep.txt")); err != nil {
		t.Fatalf("the file was not created: %v", err)
	}
}

func TestRunCommandReportsWhatHappened(t *testing.T) {
	t.Parallel()
	host, root := newHost(t)

	output, err := run(t, host, "run_command", map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if strings.TrimSpace(output) != root {
		t.Fatalf("a command runs in the root; got %q want %q", strings.TrimSpace(output), root)
	}

	// A non-zero exit is a result, not a tool failure: the model asked what
	// happens when this runs, and this is what happened.
	failed, err := run(t, host, "run_command", map[string]any{"command": "echo nope >&2; exit 3"})
	if err != nil {
		t.Fatalf("a failing command is still a result: %v", err)
	}
	if !strings.Contains(failed, "exit status") || !strings.Contains(failed, "nope") {
		t.Fatalf("a failing command must report its status and output: %q", failed)
	}
}

func TestRunCommandIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(root)
	host, err := console.NewHost(console.HostConfig{
		Root: resolved, Enabled: []string{"run_command"}, CommandTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	if _, err := run(t, host, "run_command", map[string]any{"command": "sleep 10"}); err == nil {
		t.Fatal("a command past the timeout must be reported rather than waited on")
	} else if !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOutputIsTruncatedRatherThanUnbounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(root)
	host, err := console.NewHost(console.HostConfig{
		Root: resolved, Enabled: []string{"read_file"}, MaxOutputBytes: 128,
	})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "big.txt"), []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	output, err := run(t, host, "read_file", map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(output, "truncated at 128 bytes") {
		t.Fatalf("expected a truncation notice: %q", output)
	}
}

func TestOnlySelectedToolsExist(t *testing.T) {
	t.Parallel()
	host, _ := newHost(t, "read_file")
	if _, declared := host.Lookup("run_command"); declared {
		t.Fatal("a tool that was not selected must not be declared")
	}
	if _, err := run(t, host, "run_command", map[string]any{"command": "echo hi"}); err == nil {
		t.Fatal("a tool that was not selected must not run")
	}
	if _, err := console.NewHost(console.HostConfig{Root: t.TempDir(), Enabled: []string{"rm_rf"}}); err == nil {
		t.Fatal("an unknown tool name must be refused at startup, not at call time")
	}
}

func TestTheDefaultToolSetIsReadOnly(t *testing.T) {
	t.Parallel()
	host, err := console.NewHost(console.HostConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	for _, tool := range host.Tools() {
		if tool.Mutating {
			t.Fatalf("the default set must not be able to change anything: %s", tool.Name)
		}
	}
}

// Every mutating tool declares that it needs confirmation. The declaration is
// what the session is told and what the console enforces, so a tool that could
// change something without one would be a hole in both at once.
func TestEveryMutatingToolRequiresConfirmation(t *testing.T) {
	t.Parallel()
	host, _ := newHost(t, "all")
	for _, tool := range host.Tools() {
		if tool.Mutating && tool.Confirm == "never" {
			t.Fatalf("%s changes things and declares no confirmation", tool.Name)
		}
		if !tool.Mutating && tool.Confirm != "never" {
			t.Fatalf("%s cannot change anything but asks for confirmation", tool.Name)
		}
		if !json.Valid(tool.Parameters) {
			t.Fatalf("%s has an invalid parameter schema", tool.Name)
		}
	}
}

// The Realtime protocol carries function call arguments as a JSON-encoded
// string, not as an object. A client that forwards them verbatim is doing the
// obvious thing, and the tool host has to understand what it is given.
func TestArgumentsAreAcceptedInBothShapesTheyArriveIn(t *testing.T) {
	t.Parallel()
	host, root := newHost(t, "read_file")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("contents"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for name, arguments := range map[string]string{
		"an object":          `{"path":"notes.txt"}`,
		"the protocol's own": `"{\"path\":\"notes.txt\"}"`,
	} {
		t.Run(name, func(t *testing.T) {
			output, err := host.Run(context.Background(), "read_file", []byte(arguments))
			if err != nil || output != "contents" {
				t.Fatalf("%s: %q %v", name, output, err)
			}
		})
	}
	if _, err := host.Run(context.Background(), "read_file", []byte(`"not json at all"`)); err == nil {
		t.Fatal("arguments that are neither shape must be reported rather than guessed at")
	}
}
