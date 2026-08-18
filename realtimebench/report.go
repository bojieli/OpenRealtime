package realtimebench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WriteReport atomically publishes an indented, secret-free report. PCM bytes
// remain available to the caller in memory but are excluded from JSON.
func WriteReport(filename string, report Report) error {
	if strings.TrimSpace(filename) == "" {
		return errors.New("realtime benchmark report path is required")
	}
	if report.SchemaVersion == "" {
		report.SchemaVersion = SchemaVersion
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = time.Now().UTC()
	}
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create realtime report directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".realtime-report-*.json")
	if err != nil {
		return fmt.Errorf("create temporary realtime report: %w", err)
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
		return fmt.Errorf("set realtime report permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode realtime report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync realtime report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close realtime report: %w", err)
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("publish realtime report: %w", err)
	}
	remove = false
	return nil
}
