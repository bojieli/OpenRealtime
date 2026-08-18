package interleavebench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteReport atomically writes an indented benchmark report. The destination
// directory is created when necessary; API keys and provider-native state are
// absent from Report by construction.
func WriteReport(filename string, report Report) error {
	if filename == "" {
		return errors.New("interleave report path is required")
	}
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create interleave report directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".interleave-report-*.json")
	if err != nil {
		return fmt.Errorf("create temporary interleave report: %w", err)
	}
	temporaryName := temporary.Name()
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("set interleave report permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode interleave report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync interleave report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close interleave report: %w", err)
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("publish interleave report: %w", err)
	}
	remove = false
	return nil
}
