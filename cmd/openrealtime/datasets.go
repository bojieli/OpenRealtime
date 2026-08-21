package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runDatasets inventories a prepared benchmark dataset.
//
// The preparation scripts fetch archives from where their owners host them and
// verify the digests a manifest pins. This is the step after: counting what
// actually landed, so a run against a half-extracted dataset fails at
// preparation rather than as a mysteriously low score three hours later.
func runDatasets(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime datasets inspect -root <path> [-pattern <glob>]")
	}
	switch strings.ToLower(strings.TrimSpace(arguments[0])) {
	case "inspect":
		return inspectDataset(arguments[1:], output)
	default:
		return fmt.Errorf("unknown datasets command %q", arguments[0])
	}
}

type datasetInventory struct {
	Root string `json:"root"`
	// Count is how many complete samples were found, which is what a manifest
	// pins and what a run's expected task count must match.
	Count int `json:"count"`
	// Groups are the dataset's own partitions, listed because they are not
	// interchangeable: a result from a clean partition says nothing about a
	// noisy one.
	Groups []datasetGroup `json:"groups"`
	// SHA256 is a digest over every sample's path and size, so two
	// preparations of the same manifest can be compared without re-hashing
	// gigabytes of audio.
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type datasetGroup struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
}

func inspectDataset(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime datasets inspect", flag.ContinueOnError)
	var (
		root    string
		pattern string
	)
	flags.StringVar(&root, "root", "", "dataset root to inspect")
	flags.StringVar(&pattern, "pattern", "*.wav", "glob that identifies one sample")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(root) == "" {
		return errors.New("-root is required")
	}

	groups := map[string]*datasetGroup{}
	var paths []string
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Extraction leaves resource forks behind on some archives, and
			// counting them as samples would make the inventory disagree with
			// the manifest for a reason nobody would guess.
			if strings.HasPrefix(entry.Name(), "__MACOSX") || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		matched, matchErr := filepath.Match(pattern, entry.Name())
		if matchErr != nil || !matched {
			return matchErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		relative, _ := filepath.Rel(root, path)
		name := filepath.Dir(relative)
		if parent := strings.Split(filepath.ToSlash(name), "/"); len(parent) > 0 {
			name = parent[0]
		}
		group, exists := groups[name]
		if !exists {
			group = &datasetGroup{Name: name}
			groups[name] = group
		}
		group.Count++
		group.Bytes += info.Size()
		total += info.Size()
		paths = append(paths, fmt.Sprintf("%s:%d", filepath.ToSlash(relative), info.Size()))
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect %s: %w", root, err)
	}

	sort.Strings(paths)
	digest := sha256.Sum256([]byte(strings.Join(paths, "\n")))
	inventory := datasetInventory{
		Root: root, Count: len(paths), SHA256: hex.EncodeToString(digest[:]), Bytes: total,
	}
	for _, group := range groups {
		inventory.Groups = append(inventory.Groups, *group)
	}
	sort.Slice(inventory.Groups, func(left, right int) bool {
		return inventory.Groups[left].Name < inventory.Groups[right].Name
	})
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}
