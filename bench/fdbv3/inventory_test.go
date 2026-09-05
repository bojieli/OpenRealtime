package fdbv3

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/testgate"
)

func TestPinnedReleasedInventoryLoadsExactHundredTasks(t *testing.T) {
	root := filepath.Join("..", "..", ".runtime", "full-duplex-bench-v3", "dataset", "fdb_v3_data_released")
	if _, err := os.Stat(root); err != nil {
		testgate.Unprepared(t, "the prepared pinned FDB v3 dataset", err)
	}
	tasks, err := loadDataset(root, 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 100 {
		t.Fatalf("released tasks = %d, want 100", len(tasks))
	}
	if tasks[0].ID != pinnedReleasedInventory.TaskNames[0] ||
		tasks[len(tasks)-1].ID != pinnedReleasedInventory.TaskNames[len(pinnedReleasedInventory.TaskNames)-1] {
		t.Fatalf("released ordering changed: first=%q last=%q", tasks[0].ID, tasks[len(tasks)-1].ID)
	}
}

func TestStrictReleasePlaybackOwnsValidatedAudioAcrossPathMutationAndRemoval(t *testing.T) {
	originalSamples := []int16{101, -202, 303, -404}
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "mutated",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, pcmWAV24k([]int16{9, 8, 7}), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "removed",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, path, originalWAV, inventory := audioPlaybackFixture(t, originalSamples)
			tasks, err := loadDataset(root, 0, &inventory)
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 || tasks[0].AudioWAV == nil || tasks[0].AudioPath != "" ||
				!bytes.Equal(tasks[0].AudioWAV, originalWAV) {
				t.Fatalf("strict task audio was not retained exactly: %+v", tasks)
			}

			testCase.mutate(t, path)
			capture, playErr := captureTaskInput(t, tasks[0])
			if playErr == nil || !strings.Contains(playErr.Error(), "session needs an endpoint") {
				t.Fatalf("release playback error = %v, want post-decode endpoint refusal", playErr)
			}
			if capture.SampleRateHz != 24_000 || !slices.Equal(capture.RoomPCM16, originalSamples) {
				t.Fatalf("release playback captured %+v, want retained %v", capture, originalSamples)
			}
		})
	}
}

func TestDiagnosticPlaybackExplicitlyReopensAudioPath(t *testing.T) {
	originalSamples := []int16{1, 2, 3}

	t.Run("mutation is observed", func(t *testing.T) {
		root, path, _, _ := audioPlaybackFixture(t, originalSamples)
		tasks, err := Load(root, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 || tasks[0].AudioWAV != nil || tasks[0].AudioPath != path {
			t.Fatalf("diagnostic task source = %+v", tasks)
		}
		replacement := []int16{44, -55, 66, -77}
		if err := os.WriteFile(path, pcmWAV24k(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
		capture, playErr := captureTaskInput(t, tasks[0])
		if playErr == nil || !strings.Contains(playErr.Error(), "session needs an endpoint") {
			t.Fatalf("diagnostic playback error = %v, want post-decode endpoint refusal", playErr)
		}
		if !slices.Equal(capture.RoomPCM16, replacement) {
			t.Fatalf("diagnostic playback captured %v, want changed path bytes %v", capture.RoomPCM16, replacement)
		}
	})

	t.Run("removal is observed", func(t *testing.T) {
		root, path, _, _ := audioPlaybackFixture(t, originalSamples)
		tasks, err := Load(root, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		capture, playErr := captureTaskInput(t, tasks[0])
		if playErr == nil || !os.IsNotExist(playErr) {
			t.Fatalf("diagnostic playback after removal error = %v, want missing path", playErr)
		}
		if len(capture.RoomPCM16) != 0 {
			t.Fatalf("missing diagnostic path unexpectedly captured audio: %+v", capture)
		}
	})
}

func TestLoadFailsClosedForMissingUnreadableOrCorruptTaskArtifacts(t *testing.T) {
	metadata := `{"id":"recording","domain":"test","title":"fixture","difficulty":"easy","expected_tool_calls":[{"function":"track_order","args":{"order_id":"ABC1"}}]}`
	for _, testCase := range []struct {
		name string
		edit func(*testing.T, string)
		want string
	}{
		{
			name: "missing metadata",
			edit: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.Remove(filepath.Join(directory, "metadata.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "metadata",
		},
		{
			name: "corrupt metadata",
			edit: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(`{"id":`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "validate",
		},
		{
			name: "missing audio",
			edit: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.Remove(filepath.Join(directory, "input.wav")); err != nil {
					t.Fatal(err)
				}
			},
			want: "audio",
		},
		{
			name: "empty audio",
			edit: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "input.wav"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "empty",
		},
		{
			name: "audio directory",
			edit: func(t *testing.T, directory string) {
				t.Helper()
				path := filepath.Join(directory, "input.wav")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "regular file",
		},
		{
			name: "unreadable dangling audio link",
			edit: func(t *testing.T, directory string) {
				t.Helper()
				path := filepath.Join(directory, "input.wav")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(directory, "absent.wav"), path); err != nil {
					t.Fatal(err)
				}
			},
			want: "regular file",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "recording_speaker")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			testCase.edit(t, directory)
			if _, err := Load(root, 0); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Load error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestPinnedInventoryDeclaresExactlyOneHundredStrictlyOrderedTasks(t *testing.T) {
	if err := pinnedReleasedInventory.validate(); err != nil {
		t.Fatal(err)
	}
	if len(pinnedReleasedInventory.TaskNames) != 100 {
		t.Fatalf("pinned FDB v3 task identities = %d, want 100", len(pinnedReleasedInventory.TaskNames))
	}
}

func TestReleasedInventoryRejectsMissingUnexpectedAndChangedBytes(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory := releasedDatasetInventory{Revision: "fixture", ArtifactDigest: digest, TaskNames: names}
	if _, err := loadDataset(root, 0, &inventory); err != nil {
		t.Fatal(err)
	}

	t.Run("changed bytes", func(t *testing.T) {
		changed := cloneDatasetFixture(t, root)
		path := filepath.Join(changed, "one_speaker", "input.wav")
		if err := os.WriteFile(path, []byte("changed audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadDataset(changed, 0, &inventory); err == nil || !strings.Contains(err.Error(), "artifact digest") {
			t.Fatalf("changed-byte error = %v", err)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		missing := t.TempDir()
		if _, err := loadDataset(missing, 0, &inventory); err == nil || !strings.Contains(err.Error(), "missing=[one_speaker]") {
			t.Fatalf("missing-directory error = %v", err)
		}
	})
	t.Run("unexpected directory", func(t *testing.T) {
		unexpected := cloneDatasetFixture(t, root)
		writeInventoryFixture(t, unexpected, "two_speaker", "two")
		if _, err := loadDataset(unexpected, 0, &inventory); err == nil || !strings.Contains(err.Error(), "unexpected=[two_speaker]") {
			t.Fatalf("unexpected-directory error = %v", err)
		}
	})
}

func TestReleasedInventoryValidatesEveryTopLevelEntry(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory := releasedDatasetInventory{Revision: "fixture", ArtifactDigest: digest, TaskNames: names}

	for _, testCase := range []struct {
		name string
		add  func(*testing.T, string)
		want string
	}{
		{
			name: "unknown regular file",
			add: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("unexpected"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected=[notes.txt]",
		},
		{
			name: "unknown symlink",
			add: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink("one_speaker", filepath.Join(root, "task-link")); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected=[task-link]",
		},
		{
			name: "hidden directory",
			add: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected=[.hidden]",
		},
		{
			name: "double underscore directory",
			add: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, "__MACOSX"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected=[__MACOSX]",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := cloneDatasetFixture(t, root)
			testCase.add(t, fixture)
			if _, err := loadDataset(fixture, 0, &inventory); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("loadDataset error = %v, want %q", err, testCase.want)
			}
		})
	}

	t.Run("regular DS Store is allowed", func(t *testing.T) {
		fixture := cloneDatasetFixture(t, root)
		if err := os.WriteFile(filepath.Join(fixture, releasedRootMetadata), []byte("finder metadata"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadDataset(fixture, 0, &inventory); err != nil {
			t.Fatal(err)
		}
	})

	for _, testCase := range []struct {
		name string
		add  func(*testing.T, string)
	}{
		{
			name: "DS Store directory",
			add: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, releasedRootMetadata), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "DS Store symlink",
			add: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink("one_speaker", filepath.Join(root, releasedRootMetadata)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := cloneDatasetFixture(t, root)
			testCase.add(t, fixture)
			if _, err := loadDataset(fixture, 0, &inventory); err == nil ||
				!strings.Contains(err.Error(), releasedRootMetadata) || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("loadDataset error = %v, want non-regular %s rejection", err, releasedRootMetadata)
			}
		})
	}
}

func TestReleasedInventoryAcceptsExactlyTwoRegularTaskArtifacts(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory := releasedDatasetInventory{Revision: "fixture", ArtifactDigest: digest, TaskNames: names}

	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "extra regular file",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: `unexpected task entry "notes.txt"`,
		},
		{
			name: "nested extra file",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				nested := filepath.Join(directory, "nested")
				if err := os.Mkdir(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nested, "extra.bin"), []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: `unexpected task entry "nested"`,
		},
		{
			name: "extra symlink",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				if err := os.Symlink(metadataArtifactName, filepath.Join(directory, "metadata-link.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: `unexpected task entry "metadata-link.json"`,
		},
		{
			name: "metadata replaced by directory",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				path := filepath.Join(directory, metadataArtifactName)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: `task artifact "metadata.json" is not a regular file`,
		},
		{
			name: "audio replaced by symlink",
			mutate: func(t *testing.T, directory string) {
				t.Helper()
				path := filepath.Join(directory, audioArtifactName)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(metadataArtifactName, path); err != nil {
					t.Fatal(err)
				}
			},
			want: `task artifact "input.wav" is not a regular file`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := cloneDatasetFixture(t, root)
			directory := filepath.Join(fixture, "one_speaker")
			testCase.mutate(t, directory)
			if _, err := loadDataset(fixture, 0, &inventory); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("loadDataset error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestReleasedInventoryEnumeratesEveryTaskDirectory(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	writeInventoryFixture(t, root, "two_speaker", "two")
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory := releasedDatasetInventory{Revision: "fixture", ArtifactDigest: digest, TaskNames: names}
	if err := os.WriteFile(
		filepath.Join(root, "two_speaker", "unlisted.bin"), []byte("extra"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDataset(root, 0, &inventory); err == nil ||
		!strings.Contains(err.Error(), `unexpected task entry "unlisted.bin"`) {
		t.Fatalf("second task directory error = %v", err)
	}
}

func TestReleasedInventoryRevalidatesRootAfterTaskReads(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "added task",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeInventoryFixture(t, root, "two_speaker", "two")
			},
			want: "unexpected=[two_speaker]",
		},
		{
			name: "removed task",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.RemoveAll(filepath.Join(root, "one_speaker")); err != nil {
					t.Fatal(err)
				}
			},
			want: "missing=[one_speaker]",
		},
		{
			name: "extra root entry",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("late extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected=[notes.txt]",
		},
		{
			name: "replaced task directory",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				path := filepath.Join(root, "one_speaker")
				if err := os.Rename(path, filepath.Join(filepath.Dir(root), "original-task")); err != nil {
					t.Fatal(err)
				}
				writeInventoryFixture(t, root, "one_speaker", "one")
			},
			want: "identity changed after its artifacts were read",
		},
		{
			name: "replaced root directory",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Rename(root, filepath.Join(filepath.Dir(root), "original-root")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				writeInventoryFixture(t, root, "one_speaker", "one")
			},
			want: "directory identity changed between checks",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "dataset")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			writeInventoryFixture(t, root, "one_speaker", "one")
			names, digest, err := releasedArtifactIdentity(root)
			if err != nil {
				t.Fatal(err)
			}
			inventory := releasedDatasetInventory{
				Revision: "fixture", ArtifactDigest: digest, TaskNames: names,
			}
			_, err = loadDatasetWithTestHooks(root, 0, &inventory, datasetLoadTestHooks{
				beforeFinalReleasedRootCheck: func() error {
					testCase.mutate(t, root)
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("late root mutation error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestDiagnosticInventoryIntentionallyAllowsTaskLocalSidecars(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	directory := filepath.Join(root, "one_speaker")
	if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("diagnostic note"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(directory, "diagnostic")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "trace.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(directory, "latest-note")); err != nil {
		t.Fatal(err)
	}
	tasks, err := Load(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].AudioPath != filepath.Join(directory, audioArtifactName) {
		t.Fatalf("diagnostic tasks = %+v", tasks)
	}
}

func TestArtifactLoadsRejectOversizedMetadataAndAudioBeforeReading(t *testing.T) {
	root := t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory := releasedDatasetInventory{Revision: "fixture", ArtifactDigest: digest, TaskNames: names}
	for _, testCase := range []struct {
		name     string
		artifact string
		size     int64
	}{
		{name: "metadata", artifact: metadataArtifactName, size: maximumMetadataArtifactBytes + 1},
		{name: "audio", artifact: audioArtifactName, size: maximumAudioArtifactBytes + 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := cloneDatasetFixture(t, root)
			path := filepath.Join(fixture, "one_speaker", testCase.artifact)
			if err := os.Truncate(path, testCase.size); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDataset(fixture, 0, &inventory); err == nil ||
				!strings.Contains(err.Error(), testCase.name) ||
				!strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("loadDataset error = %v, want bounded %s rejection", err, testCase.name)
			}
		})
	}
}

func TestRetainedArtifactRejectsPostStatShortRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("opened-file-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRetainedArtifact(file, opened.Size(), maximumMetadataArtifactBytes); err == nil ||
		!strings.Contains(err.Error(), "retained byte count") {
		t.Fatalf("short retained read error = %v", err)
	}
}

func writeInventoryFixture(t *testing.T, root, directoryName, metadataID string) {
	t.Helper()
	directory := filepath.Join(root, directoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := `{"id":"` + metadataID + `","domain":"test","title":"fixture","difficulty":"easy","expected_tool_calls":[{"function":"track_order","args":{"order_id":"ABC1"}}]}`
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("audio-"+directoryName), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cloneDatasetFixture(t *testing.T, source string) string {
	t.Helper()
	destination := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		sourceDirectory := filepath.Join(source, entry.Name())
		destinationDirectory := filepath.Join(destination, entry.Name())
		if err := os.Mkdir(destinationDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"metadata.json", "input.wav"} {
			payload, err := os.ReadFile(filepath.Join(sourceDirectory, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(destinationDirectory, name), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return destination
}

func audioPlaybackFixture(
	t *testing.T, samples []int16,
) (root, path string, wav []byte, inventory releasedDatasetInventory) {
	t.Helper()
	root = t.TempDir()
	writeInventoryFixture(t, root, "one_speaker", "one")
	path = filepath.Join(root, "one_speaker", "input.wav")
	wav = pcmWAV24k(samples)
	if err := os.WriteFile(path, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	inventory = releasedDatasetInventory{
		Revision: "fixture", ArtifactDigest: digest, TaskNames: names,
	}
	return root, path, wav, inventory
}

func captureTaskInput(t *testing.T, task Task) (bench.SessionAudioCapture, error) {
	t.Helper()
	var capture bench.SessionAudioCapture
	_, err := playTaskAudio(context.Background(), bench.SessionConfig{
		CaptureAudio: func(got bench.SessionAudioCapture) error {
			capture = got
			return nil
		},
	}, task)
	return capture, err
}

func pcmWAV24k(samples []int16) []byte {
	const headerBytes = 44
	dataBytes := len(samples) * 2
	payload := make([]byte, headerBytes+dataBytes)
	copy(payload[0:4], "RIFF")
	binary.LittleEndian.PutUint32(payload[4:8], uint32(36+dataBytes))
	copy(payload[8:12], "WAVE")
	copy(payload[12:16], "fmt ")
	binary.LittleEndian.PutUint32(payload[16:20], 16)
	binary.LittleEndian.PutUint16(payload[20:22], 1)
	binary.LittleEndian.PutUint16(payload[22:24], 1)
	binary.LittleEndian.PutUint32(payload[24:28], 24_000)
	binary.LittleEndian.PutUint32(payload[28:32], 48_000)
	binary.LittleEndian.PutUint16(payload[32:34], 2)
	binary.LittleEndian.PutUint16(payload[34:36], 16)
	copy(payload[36:40], "data")
	binary.LittleEndian.PutUint32(payload[40:44], uint32(dataBytes))
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(payload[headerBytes+index*2:], uint16(sample))
	}
	return payload
}
