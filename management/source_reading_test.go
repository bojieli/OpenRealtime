package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSourceReadRootIdentity = "sha256:abababababababababababababababababababababababababababababababab"

func TestRootedSourceReaderReturnsExactStableUTF8Source(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "graphs"), 0o700); err != nil {
		t.Fatal(err)
	}
	publisher := newTestSourceReader(t, root, 0)
	write := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceReadRootIdentity,
		Mode: SourceCreate, Path: "graphs/agent-β.ortg", Source: "graph agent_β {\n}\n",
	}
	if _, err := publisher.Publish(context.Background(), write); err != nil {
		t.Fatal(err)
	}
	request := SourceReadRequest{
		FormatVersion: SourceReadFormatVersion,
		RootIdentity:  testSourceReadRootIdentity,
		Path:          write.Path,
	}
	result, err := publisher.Read(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != write.Source || result.SourceBytes != uint64(len(write.Source)) ||
		ValidateSourceReadResult(request, result) != nil {
		t.Fatalf("source read result = %+v", result)
	}
	result.Source = "mutated"
	again, err := publisher.Read(context.Background(), request)
	if err != nil || again.Source != write.Source {
		t.Fatalf("caller mutation changed rooted source read = %+v, %v", again, err)
	}
}

func TestSourceReadRequestAndResultValidationFailClosed(t *testing.T) {
	request := SourceReadRequest{
		FormatVersion: SourceReadFormatVersion,
		RootIdentity:  testSourceReadRootIdentity,
		Path:          "nested/agent.yaml",
	}
	result, err := NewSourceReadResult(request, "graph source {\n}\n")
	if err != nil || ValidateSourceReadResult(request, result) != nil {
		t.Fatalf("valid source read result = %+v, %v", result, err)
	}
	invalidRequests := []SourceReadRequest{
		{},
		{FormatVersion: 2, RootIdentity: request.RootIdentity, Path: request.Path},
		{FormatVersion: 1, RootIdentity: "root", Path: request.Path},
		{FormatVersion: 1, RootIdentity: request.RootIdentity, Path: "../agent.ortg"},
		{FormatVersion: 1, RootIdentity: request.RootIdentity, Path: "agent.txt"},
		{FormatVersion: 1, RootIdentity: request.RootIdentity, Path: ".openrealtime-authoring-stage.ortg"},
	}
	for _, invalid := range invalidRequests {
		if err := ValidateSourceReadRequest(invalid); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid request %+v error = %v", invalid, err)
		}
	}
	mutations := []func(*SourceReadResult){
		func(value *SourceReadResult) { value.FormatVersion++ },
		func(value *SourceReadResult) { value.RootIdentity = "sha256:" + strings.Repeat("1", 64) },
		func(value *SourceReadResult) { value.Path = "other.ortg" },
		func(value *SourceReadResult) { value.Source += " " },
		func(value *SourceReadResult) { value.SourceBytes++ },
		func(value *SourceReadResult) { value.SourceDigest = "sha256:" + strings.Repeat("2", 64) },
		func(value *SourceReadResult) { value.ResultDigest = "sha256:" + strings.Repeat("3", 64) },
	}
	for index, mutate := range mutations {
		forged := result
		mutate(&forged)
		if err := ValidateSourceReadResult(request, forged); !errors.Is(err, ErrConflict) {
			t.Errorf("forged result %d error = %v", index, err)
		}
	}
	if _, err := NewSourceReadResult(request, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty result error = %v", err)
	}
	if _, err := NewSourceReadResult(request, string([]byte{0xff})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-UTF-8 result error = %v", err)
	}
}

func TestRootedSourceReaderRejectsUnstableOrEscapingTargets(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "valid.ortg"), []byte("graph valid {\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "outside.ortg"), []byte("graph outside {\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher := newTestSourceReader(t, root, 64)
	read := func(path string) error {
		_, err := publisher.Read(context.Background(), SourceReadRequest{
			FormatVersion: 1, RootIdentity: testSourceReadRootIdentity, Path: path,
		})
		return err
	}
	for _, fixture := range []struct {
		name, path string
		prepare    func() error
	}{
		{name: "missing", path: "missing.ortg", prepare: func() error { return nil }},
		{name: "directory", path: "directory.ortg", prepare: func() error {
			return os.Mkdir(filepath.Join(root, "directory.ortg"), 0o700)
		}},
		{name: "invalid UTF-8", path: "invalid.ortg", prepare: func() error {
			return os.WriteFile(filepath.Join(root, "invalid.ortg"), []byte{0xff}, 0o600)
		}},
		{name: "empty", path: "empty.ortg", prepare: func() error {
			return os.WriteFile(filepath.Join(root, "empty.ortg"), nil, 0o600)
		}},
		{name: "oversized", path: "oversized.ortg", prepare: func() error {
			return os.WriteFile(filepath.Join(root, "oversized.ortg"), []byte(strings.Repeat("x", 65)), 0o600)
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := fixture.prepare(); err != nil {
				t.Fatal(err)
			}
			if err := read(fixture.path); !errors.Is(err, ErrConflict) {
				t.Fatalf("read error = %v", err)
			}
		})
	}
	t.Run("external symlink", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(external, "outside.ortg"), filepath.Join(root, "linked.ortg")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := read("linked.ortg"); !errors.Is(err, ErrConflict) {
			t.Fatalf("symlink read error = %v", err)
		}
	})
	t.Run("hard link", func(t *testing.T) {
		if err := os.Link(filepath.Join(root, "valid.ortg"), filepath.Join(root, "hard.ortg")); err != nil {
			t.Skipf("hard link unavailable: %v", err)
		}
		if err := read("hard.ortg"); !errors.Is(err, ErrConflict) {
			t.Fatalf("hard-link read error = %v", err)
		}
	})
	if err := read("../escape.ortg"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("traversal read error = %v", err)
	}
	if _, err := publisher.Read(context.Background(), SourceReadRequest{
		FormatVersion: 1, RootIdentity: "sha256:" + strings.Repeat("4", 64), Path: "valid.ortg",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-root read error = %v", err)
	}
}

func TestRootedSourceReaderDetectsLastMomentTargetAndParentSwap(t *testing.T) {
	t.Run("target", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "agent.ortg"), []byte("graph first {\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "replacement.ortg"), []byte("graph second {\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		publisher := newTestSourceReader(t, root, 0)
		_, err := publisher.read(context.Background(), SourceReadRequest{
			FormatVersion: 1, RootIdentity: testSourceReadRootIdentity, Path: "agent.ortg",
		}, sourceReadingOperations{afterInitialSnapshot: func() error {
			if err := os.Rename(filepath.Join(root, "agent.ortg"), filepath.Join(root, "former.ortg")); err != nil {
				return err
			}
			return os.Rename(filepath.Join(root, "replacement.ortg"), filepath.Join(root, "agent.ortg"))
		}})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("target-swap read error = %v", err)
		}
	})
	t.Run("parent", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "graphs")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, "agent.ortg"), []byte("graph stable {\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		publisher := newTestSourceReader(t, root, 0)
		_, err := publisher.read(context.Background(), SourceReadRequest{
			FormatVersion: 1, RootIdentity: testSourceReadRootIdentity, Path: "graphs/agent.ortg",
		}, sourceReadingOperations{afterInitialSnapshot: func() error {
			if err := os.Rename(parent, filepath.Join(root, "former")); err != nil {
				return err
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(parent, "agent.ortg"), []byte("graph stable {\n}\n"), 0o600)
		}})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("parent-swap read error = %v", err)
		}
	})
}

func TestRootedSourceReaderHonorsCancellationAndClose(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agent.ortg"), []byte("graph source {\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher := newTestSourceReader(t, root, 0)
	request := SourceReadRequest{FormatVersion: 1, RootIdentity: testSourceReadRootIdentity, Path: "agent.ortg"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := publisher.Read(ctx, request); !errors.Is(err, ErrUnavailable) ||
		!strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("canceled read error = %v", err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Read(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed read error = %v", err)
	}
}

func BenchmarkValidateSourceReadResult(b *testing.B) {
	request := SourceReadRequest{
		FormatVersion: 1, RootIdentity: testSourceReadRootIdentity, Path: "bench/agent.ortg",
	}
	result, err := NewSourceReadResult(request, "graph benchmark {\n}\n")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := ValidateSourceReadResult(request, result); err != nil {
			b.Fatal(err)
		}
	}
}

func newTestSourceReader(t testing.TB, root string, maximum int) *RootedSourcePublisher {
	t.Helper()
	publisher, err := NewRootedSourcePublisher(RootedSourcePublisherOptions{
		Root: root, RootIdentity: testSourceReadRootIdentity, MaxBytes: maximum,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publisher.Close(); err != nil {
			t.Errorf("close rooted source reader: %v", err)
		}
	})
	return publisher
}
