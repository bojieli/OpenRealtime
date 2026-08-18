package audiobench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Report can contain either or both speech stages. Runtime configuration is
// limited to non-secret deployment facts.
type Report struct {
	SchemaVersion string            `json:"schema_version"`
	CreatedAt     time.Time         `json:"created_at"`
	Runtime       map[string]string `json:"runtime,omitempty"`
	ASR           *ASRResult        `json:"asr,omitempty"`
	TTS           *TTSResult        `json:"tts,omitempty"`
}

// WriteReport publishes an indented JSON report atomically.
func WriteReport(filename string, report Report) error {
	if strings.TrimSpace(filename) == "" {
		return errors.New("audio benchmark report path is required")
	}
	if report.SchemaVersion == "" {
		report.SchemaVersion = SchemaVersion
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = time.Now().UTC()
	}
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create audio report directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".audio-report-*.json")
	if err != nil {
		return fmt.Errorf("create temporary audio report: %w", err)
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
		return fmt.Errorf("set audio report permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode audio report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync audio report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close audio report: %w", err)
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fmt.Errorf("publish audio report: %w", err)
	}
	remove = false
	return nil
}
