package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const testSourceRootIdentity = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestRootedSourcePublisherCreatesUpdatesAndReceiptsExactMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "graphs"), 0o755); err != nil {
		t.Fatal(err)
	}
	publisher := testRootedSourcePublisher(t, root)
	create := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion,
		RootIdentity:  testSourceRootIdentity,
		Mode:          SourceCreate,
		Path:          "graphs/agent.ortg",
		Source:        "graph agent {\n}\n",
	}
	created, err := publisher.Publish(t.Context(), create)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceWriteReceipt(create, created); err != nil {
		t.Fatal(err)
	}
	if created.PreviousSourceDigest != "" || created.CleanupPending ||
		created.SourceDigest != digestSource(create.Source) {
		t.Fatalf("create receipt = %+v", created)
	}
	assertSourceFile(t, root, create.Path, create.Source, 0o644)
	payload, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(root)) || bytes.Contains(payload, []byte(create.Source)) {
		t.Fatalf("receipt exposed root or source payload: %s", payload)
	}

	if _, err := publisher.Publish(t.Context(), create); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create = %v", err)
	}
	assertSourceFile(t, root, create.Path, create.Source, 0o644)

	update := SourceWriteRequest{
		FormatVersion:        SourceWriteFormatVersion,
		RootIdentity:         testSourceRootIdentity,
		Mode:                 SourceUpdate,
		Path:                 create.Path,
		Source:               "graph agent {\n  boundary {\n  }\n}\n",
		ExpectedSourceDigest: created.SourceDigest,
	}
	updated, err := publisher.Publish(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceWriteReceipt(update, updated); err != nil {
		t.Fatal(err)
	}
	if updated.PreviousSourceDigest != created.SourceDigest || updated.CleanupPending ||
		updated.SourceDigest != digestSource(update.Source) {
		t.Fatalf("update receipt = %+v", updated)
	}
	assertSourceFile(t, root, update.Path, update.Source, 0o644)

	stale := update
	stale.Source = "graph stale {\n}\n"
	if _, err := publisher.Publish(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
	assertSourceFile(t, root, update.Path, update.Source, 0o644)
	assertNoSourceStages(t, filepath.Join(root, "graphs"))

	wrongRoot := create
	wrongRoot.Path = "other.ortg"
	wrongRoot.RootIdentity = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if _, err := publisher.Publish(t.Context(), wrongRoot); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-root create = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "other.ortg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong-root create affected filesystem: %v", err)
	}

	if got := publisher.RootIdentity(); got != testSourceRootIdentity {
		t.Fatalf("root identity = %q", got)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if publisher.RootIdentity() != "" {
		t.Fatal("closed publisher retained its root identity projection")
	}
	if _, err := publisher.Publish(t.Context(), create); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed publication = %v", err)
	}
}

func TestRootedSourcePublisherRejectsEscapesLinksAndNoncanonicalPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	publisher := testRootedSourcePublisher(t, root)
	base := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion,
		RootIdentity:  testSourceRootIdentity,
		Mode:          SourceCreate,
		Path:          "agent.ortg",
		Source:        "graph safe {\n}\n",
	}
	invalidPaths := []string{
		"", ".", "/absolute.ortg", "../escape.ortg", "a/../escape.ortg",
		"a//escape.ortg", "a\\escape.ortg", "a/./escape.ortg", "escape.txt",
		".openrealtime-authoring-deadbeef.ortg", "line\nbreak.ortg",
	}
	for _, name := range invalidPaths {
		request := base
		request.Path = name
		if err := ValidateSourceWriteRequest(request); !errors.Is(err, ErrInvalid) {
			t.Errorf("path %q validation = %v", name, err)
		}
	}
	invalidUTF8 := base
	invalidUTF8.Source = string([]byte{0xff})
	if err := ValidateSourceWriteRequest(invalidUTF8); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 source = %v", err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	escape := base
	escape.Path = "outside/escaped.ortg"
	if _, err := publisher.Publish(t.Context(), escape); !errors.Is(err, ErrInvalid) {
		t.Fatalf("external parent symlink = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.ortg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external parent received source: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	internalAlias := base
	internalAlias.Path = "inside/aliased.ortg"
	if _, err := publisher.Publish(t.Context(), internalAlias); !errors.Is(err, ErrInvalid) {
		t.Fatalf("internal parent symlink = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "real", "aliased.ortg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("internal parent alias received source: %v", err)
	}

	externalSource := filepath.Join(outside, "external.ortg")
	if err := os.WriteFile(externalSource, []byte(base.Source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalSource, filepath.Join(root, "linked.ortg")); err != nil {
		t.Fatal(err)
	}
	linkedUpdate := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceUpdate, Path: "linked.ortg", Source: "graph changed {\n}\n",
		ExpectedSourceDigest: digestSource(base.Source),
	}
	if _, err := publisher.Publish(t.Context(), linkedUpdate); !errors.Is(err, ErrConflict) {
		t.Fatalf("external target symlink = %v", err)
	}
	assertSourceFile(t, outside, "external.ortg", base.Source, 0o644)

	if err := os.WriteFile(filepath.Join(root, "original.ortg"), []byte(base.Source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "original.ortg"), filepath.Join(root, "alias.ortg")); err != nil {
		t.Fatal(err)
	}
	hardLinked := linkedUpdate
	hardLinked.Path = "alias.ortg"
	if _, err := publisher.Publish(t.Context(), hardLinked); !errors.Is(err, ErrConflict) {
		t.Fatalf("hard-linked target = %v", err)
	}
	assertSourceFile(t, root, "original.ortg", base.Source, 0o644)

	if err := os.Mkdir(filepath.Join(root, "directory.ortg"), 0o755); err != nil {
		t.Fatal(err)
	}
	directory := linkedUpdate
	directory.Path = "directory.ortg"
	if _, err := publisher.Publish(t.Context(), directory); !errors.Is(err, ErrConflict) {
		t.Fatalf("directory target = %v", err)
	}
	missingParent := base
	missingParent.Path = "missing/agent.ortg"
	if _, err := publisher.Publish(t.Context(), missingParent); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing parent = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publisher created a directory: %v", err)
	}
}

func TestRootedSourcePublisherRechecksStaleStateAndCancellation(t *testing.T) {
	root := t.TempDir()
	publisher := testRootedSourcePublisher(t, root)
	oldSource := "graph old {\n}\n"
	driftSource := "graph drift {\n}\n"
	path := filepath.Join(root, "agent.ortg")
	if err := os.WriteFile(path, []byte(oldSource), 0o640); err != nil {
		t.Fatal(err)
	}
	update := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceUpdate, Path: "agent.ortg", Source: "graph new {\n}\n",
		ExpectedSourceDigest: digestSource(oldSource),
	}
	_, err := publisher.publish(t.Context(), update, sourcePublicationOperations{
		afterInitialCheck: func() error {
			return os.WriteFile(path, []byte(driftSource), 0o640)
		},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("drifted update = %v", err)
	}
	assertSourceFile(t, root, update.Path, driftSource, 0o640)
	assertNoSourceStages(t, root)
	lastMoment := "graph last_moment {\n}\n"
	lastMomentUpdate := update
	lastMomentUpdate.ExpectedSourceDigest = digestSource(driftSource)
	lastMomentUpdate.Source = "graph should_not_publish {\n}\n"
	_, err = publisher.publish(t.Context(), lastMomentUpdate, sourcePublicationOperations{
		beforeAtomicUpdate: func() error {
			return os.WriteFile(path, []byte(lastMoment), 0o640)
		},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("last-moment drifted update = %v", err)
	}
	assertSourceFile(t, root, update.Path, lastMoment, 0o640)
	assertNoSourceStages(t, root)

	create := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceCreate, Path: "created.ortg", Source: "graph proposed {\n}\n",
	}
	intruder := "graph intruder {\n}\n"
	_, err = publisher.publish(t.Context(), create, sourcePublicationOperations{
		afterInitialCheck: func() error {
			return os.WriteFile(filepath.Join(root, create.Path), []byte(intruder), 0o644)
		},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("raced create = %v", err)
	}
	assertSourceFile(t, root, create.Path, intruder, 0o644)
	assertNoSourceStages(t, root)

	ctx, cancel := context.WithCancel(t.Context())
	canceled := create
	canceled.Path = "canceled.ortg"
	_, err = publisher.publish(ctx, canceled, sourcePublicationOperations{
		afterInitialCheck: func() error {
			cancel()
			return nil
		},
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled create = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, canceled.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled create published: %v", err)
	}

	parent := filepath.Join(root, "nested")
	displaced := filepath.Join(root, "displaced")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	parentSwap := create
	parentSwap.Path = "nested/swapped.ortg"
	_, err = publisher.publish(t.Context(), parentSwap, sourcePublicationOperations{
		afterInitialCheck: func() error {
			if err := os.Rename(parent, displaced); err != nil {
				return err
			}
			return os.Mkdir(parent, 0o755)
		},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("swapped parent publication = %v", err)
	}
	for _, candidate := range []string{
		filepath.Join(parent, "swapped.ortg"), filepath.Join(displaced, "swapped.ortg"),
	} {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("swapped parent exposed source at %s: %v", candidate, err)
		}
	}
	assertNoSourceStages(t, parent)
	assertNoSourceStages(t, displaced)
}

func TestRootedSourcePublisherSerializesConcurrentCreatesAndUpdates(t *testing.T) {
	root := t.TempDir()
	publisher := testRootedSourcePublisher(t, root)
	const contenders = 24
	createBase := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceCreate, Path: "winner.ortg",
	}
	var createWins atomic.Int64
	var createWinner atomic.Value
	var group sync.WaitGroup
	for index := range contenders {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := createBase
			request.Source = "graph create_" + string(rune('a'+index)) + " {\n}\n"
			receipt, err := publisher.Publish(t.Context(), request)
			switch {
			case err == nil:
				if ValidateSourceWriteReceipt(request, receipt) != nil {
					t.Errorf("winner receipt is invalid: %+v", receipt)
					return
				}
				createWins.Add(1)
				createWinner.Store(request.Source)
			case !errors.Is(err, ErrConflict):
				t.Errorf("concurrent create = %v", err)
			}
		}(index)
	}
	group.Wait()
	if createWins.Load() != 1 {
		t.Fatalf("concurrent create winners = %d", createWins.Load())
	}
	winner := createWinner.Load().(string)
	assertSourceFile(t, root, createBase.Path, winner, 0o644)

	expected := digestSource(winner)
	var updateWins atomic.Int64
	var updateWinner atomic.Value
	for index := range contenders {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := SourceWriteRequest{
				FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
				Mode: SourceUpdate, Path: createBase.Path,
				Source:               "graph update_" + string(rune('a'+index)) + " {\n}\n",
				ExpectedSourceDigest: expected,
			}
			receipt, err := publisher.Publish(t.Context(), request)
			switch {
			case err == nil:
				if ValidateSourceWriteReceipt(request, receipt) != nil {
					t.Errorf("update winner receipt is invalid: %+v", receipt)
					return
				}
				updateWins.Add(1)
				updateWinner.Store(request.Source)
			case !errors.Is(err, ErrConflict):
				t.Errorf("concurrent update = %v", err)
			}
		}(index)
	}
	group.Wait()
	if updateWins.Load() != 1 {
		t.Fatalf("concurrent update winners = %d", updateWins.Load())
	}
	assertSourceFile(t, root, createBase.Path, updateWinner.Load().(string), 0o644)
	assertNoSourceStages(t, root)
}

func TestRootedSourcePublisherAtomicReadersSeeWholeSources(t *testing.T) {
	root := t.TempDir()
	publisher := testRootedSourcePublisher(t, root)
	first := "graph a {\n  # " + strings.Repeat("a", 128<<10) + "\n}\n"
	second := "graph b {\n  # " + strings.Repeat("b", 128<<10) + "\n}\n"
	request := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceCreate, Path: "atomic.ortg", Source: first,
	}
	receipt, err := publisher.Publish(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	readerErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			value, err := os.ReadFile(filepath.Join(root, request.Path))
			if err != nil {
				select {
				case readerErr <- err:
				default:
				}
				return
			}
			if !bytes.Equal(value, []byte(first)) && !bytes.Equal(value, []byte(second)) {
				select {
				case readerErr <- errors.New("reader observed a partial source"):
				default:
				}
				return
			}
		}
	}()
	for iteration := range 20 {
		next := second
		if iteration%2 != 0 {
			next = first
		}
		request.Mode = SourceUpdate
		request.ExpectedSourceDigest = receipt.SourceDigest
		request.Source = next
		receipt, err = publisher.Publish(t.Context(), request)
		if err != nil {
			close(stop)
			t.Fatal(err)
		}
	}
	close(stop)
	select {
	case err := <-readerErr:
		t.Fatal(err)
	default:
	}
}

func TestRootedSourcePublisherHeldRootSurvivesConfiguredDirectoryRename(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	moved := filepath.Join(parent, "moved")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	publisher := testRootedSourcePublisher(t, root)
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	request := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceCreate, Path: "anchored.ortg", Source: "graph anchored {\n}\n",
	}
	if _, err := publisher.Publish(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	assertSourceFile(t, moved, request.Path, request.Source, 0o644)
	if _, err := os.Stat(filepath.Join(root, request.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publisher followed the displaced root name: %v", err)
	}
}

func TestSourceWriteValidationRejectsForgedReceiptsAndInvalidConstruction(t *testing.T) {
	request := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceCreate, Path: "agent.ortg", Source: "graph receipt {\n}\n",
	}
	receipt, err := NewSourceWriteReceipt(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceWriteReceipt(request, receipt); err != nil {
		t.Fatal(err)
	}
	invalidRequest := request
	invalidRequest.Path = "../escape.ortg"
	if receipt, err := NewSourceWriteReceipt(invalidRequest, false); !errors.Is(err, ErrInvalid) ||
		receipt != (SourceWriteReceipt{}) {
		t.Fatalf("invalid receipt construction = %+v, %v", receipt, err)
	}
	mutations := map[string]func(*SourceWriteReceipt){
		"version": func(value *SourceWriteReceipt) { value.FormatVersion++ },
		"root":    func(value *SourceWriteReceipt) { value.RootIdentity = strings.Replace(value.RootIdentity, "1", "2", 1) },
		"mode":    func(value *SourceWriteReceipt) { value.Mode = SourceUpdate },
		"path":    func(value *SourceWriteReceipt) { value.Path = "other.ortg" },
		"previous": func(value *SourceWriteReceipt) {
			value.PreviousSourceDigest = digestSource("old")
		},
		"source":  func(value *SourceWriteReceipt) { value.SourceDigest = digestSource("other") },
		"bytes":   func(value *SourceWriteReceipt) { value.SourceBytes++ },
		"cleanup": func(value *SourceWriteReceipt) { value.CleanupPending = true },
		"receipt": func(value *SourceWriteReceipt) { value.ReceiptDigest = digestSource("forged") },
	}
	for name, mutate := range mutations {
		forged := receipt
		mutate(&forged)
		if err := ValidateSourceWriteReceipt(request, forged); !errors.Is(err, ErrConflict) {
			t.Errorf("%s forged receipt = %v", name, err)
		}
	}

	if !atomicSourcePublicationSupported() {
		t.Skip("atomic rooted publication is unsupported on this platform")
	}
	root := t.TempDir()
	invalid := []RootedSourcePublisherOptions{
		{Root: root, RootIdentity: "bad"},
		{Root: ".", RootIdentity: testSourceRootIdentity},
		{Root: filepath.VolumeName(root) + string(os.PathSeparator), RootIdentity: testSourceRootIdentity},
		{Root: root, RootIdentity: testSourceRootIdentity, MaxBytes: maxManagedSourceBytes + 1},
		{Root: root, RootIdentity: testSourceRootIdentity, CreateMode: 0o755},
	}
	for index, options := range invalid {
		publisher, err := NewRootedSourcePublisher(options)
		if err == nil {
			_ = publisher.Close()
			t.Errorf("invalid publisher %d succeeded", index)
		}
	}
	missing := filepath.Join(root, "missing")
	if publisher, err := NewRootedSourcePublisher(RootedSourcePublisherOptions{
		Root: missing, RootIdentity: testSourceRootIdentity,
	}); !errors.Is(err, ErrUnavailable) {
		if publisher != nil {
			_ = publisher.Close()
		}
		t.Fatalf("missing publisher root = %v", err)
	}
	limitedRoot := t.TempDir()
	limited, err := NewRootedSourcePublisher(RootedSourcePublisherOptions{
		Root: limitedRoot, RootIdentity: testSourceRootIdentity, MaxBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limited.Close() }()
	tooLarge := request
	if _, err := limited.Publish(t.Context(), tooLarge); !errors.Is(err, ErrInvalid) {
		t.Fatalf("configured source limit = %v", err)
	}
	if _, err := os.Stat(filepath.Join(limitedRoot, request.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("over-limit request touched target: %v", err)
	}
}

func BenchmarkSourceWriteReceiptValidation(b *testing.B) {
	request := SourceWriteRequest{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: testSourceRootIdentity,
		Mode: SourceUpdate, Path: "graphs/agent.ortg", Source: "graph benchmark {\n}\n",
		ExpectedSourceDigest: digestSource("graph old {\n}\n"),
	}
	receipt := SourceWriteReceipt{
		FormatVersion: SourceWriteFormatVersion, RootIdentity: request.RootIdentity,
		Mode: request.Mode, Path: request.Path,
		PreviousSourceDigest: request.ExpectedSourceDigest,
		SourceDigest:         digestSource(request.Source), SourceBytes: uint64(len(request.Source)),
	}
	receipt.ReceiptDigest = sourceWriteReceiptDigest(receipt)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := ValidateSourceWriteReceipt(request, receipt); err != nil {
			b.Fatal(err)
		}
	}
}

func testRootedSourcePublisher(t *testing.T, root string) *RootedSourcePublisher {
	t.Helper()
	if !atomicSourcePublicationSupported() {
		t.Skip("atomic rooted publication is unsupported on this platform")
	}
	publisher, err := NewRootedSourcePublisher(RootedSourcePublisherOptions{
		Root: root, RootIdentity: testSourceRootIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publisher.Close(); err != nil {
			t.Errorf("close rooted source publisher: %v", err)
		}
	})
	return publisher
}

func assertSourceFile(
	t *testing.T, root, relative, source string, mode os.FileMode,
) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != source {
		t.Fatalf("source %s = %q, want %q", relative, value, source)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("source %s mode = %o, want %o", relative, info.Mode().Perm(), mode)
	}
}

func assertNoSourceStages(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), sourceStagePrefix) {
			t.Fatalf("source publication left private stage %s", entry.Name())
		}
	}
}
