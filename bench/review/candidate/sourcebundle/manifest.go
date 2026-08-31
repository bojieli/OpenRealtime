package sourcebundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	ManifestFormat        = "openrealtime.candidate-source-bundle"
	ManifestFormatVersion = 1
	ReceiptFormat         = "openrealtime.candidate-source-receipt"
	ReceiptFormatVersion  = 1
	manifestName          = "manifest.json"
	resultName            = "result.json"
	reviewName            = "REVIEW.md"
	maximumMetadataBytes  = 64 << 20
)

type SourceFile struct {
	Path      string `json:"path"`
	Purpose   string `json:"purpose,omitempty"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type Artifact struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Role        string `json:"role"`
	ContentType string `json:"content_type"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
}

type AttemptEntry struct {
	AttemptID        string            `json:"attempt_id"`
	Suite            string            `json:"suite"`
	Case             string            `json:"case"`
	Trial            int               `json:"trial"`
	Directory        string            `json:"directory"`
	MediaSource      string            `json:"media_source"`
	AttemptPath      string            `json:"attempt_path"`
	AttemptSHA256    string            `json:"attempt_sha256"`
	CompletionPath   string            `json:"completion_path,omitempty"`
	CompletionSHA256 string            `json:"completion_sha256,omitempty"`
	ContextPath      string            `json:"review_context_path,omitempty"`
	ContextSHA256    string            `json:"review_context_sha256,omitempty"`
	Media            *review.Media     `json:"review_media,omitempty"`
	Artifacts        []Artifact        `json:"artifacts"`
	Deterministic    bench.TaskOutcome `json:"deterministic_outcome,omitempty"`
	EvidenceComplete bool              `json:"evidence_complete"`
	Terminal         string            `json:"terminal"`
}

type Manifest struct {
	Format        string              `json:"format"`
	FormatVersion int                 `json:"format_version"`
	Complete      bool                `json:"complete"`
	Suite         string              `json:"suite"`
	Cell          bench.Cell          `json:"cell"`
	Provenance    bench.Provenance    `json:"provenance"`
	Origin        candidate.RunOrigin `json:"run_origin"`
	AttemptCount  int                 `json:"attempt_count"`
	ResultPath    string              `json:"result_path"`
	ResultSHA256  string              `json:"result_sha256"`
	ReviewPath    string              `json:"review_path"`
	ReviewSHA256  string              `json:"review_sha256"`
	FileSetSHA256 string              `json:"file_set_sha256"`
	Files         []SourceFile        `json:"files"`
	Attempts      []AttemptEntry      `json:"attempts"`
	Advisory      string              `json:"advisory_review_status"`
}

type Receipt struct {
	Format         string `json:"format"`
	FormatVersion  int    `json:"format_version"`
	Directory      string `json:"directory"`
	ManifestSHA256 string `json:"manifest_sha256"`
	FileSetSHA256  string `json:"file_set_sha256"`
	AttemptCount   int    `json:"attempt_count"`
	ReceiptSHA256  string `json:"receipt_sha256"`
}

func canonicalIndented(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) > maximumMetadataBytes {
		return nil, errors.New("candidate source metadata is not bounded JSON")
	}
	payload = append(payload, '\n')
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumMetadataBytes, MaxDepth: 128, MaxTokens: 5_000_000,
		MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: maximumMetadataBytes,
		MaxWorkBytes: 256 << 20,
	}); err != nil {
		return nil, fmt.Errorf("candidate source metadata is not strict JSON: %w", err)
	}
	return payload, nil
}

func canonicalCompact(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil || len(payload) == 0 || len(payload) > maximumMetadataBytes {
		return nil, errors.New("candidate source identity is not bounded JSON")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumMetadataBytes, MaxDepth: 128, MaxTokens: 5_000_000,
		MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: maximumMetadataBytes,
		MaxWorkBytes: 256 << 20,
	}); err != nil {
		return nil, errors.New("candidate source identity is not strict JSON")
	}
	return payload, nil
}

func fileSetDigest(files []SourceFile) (string, error) {
	payload, err := canonicalCompact(files)
	if err != nil {
		return "", err
	}
	return digest(payload), nil
}

func receiptPayload(receipt Receipt) ([]byte, error) {
	identity := receipt
	identity.Directory = ""
	identity.ReceiptSHA256 = ""
	payload, err := canonicalCompact(identity)
	if err != nil {
		return nil, err
	}
	receipt.ReceiptSHA256 = digest(payload)
	return canonicalIndented(receipt)
}

func sourceReview(result bench.Result, attempts []AttemptEntry, guard sensitiveGuard) []byte {
	var output strings.Builder
	output.WriteString("# Candidate multimodal review bundle\n\n")
	output.WriteString("Deterministic benchmark outcomes remain authoritative. ")
	output.WriteString("Multimodal model review is advisory and has not yet been attached to this source bundle.\n\n")
	output.WriteString("| Case | Trial | Deterministic | Evidence | Recording | Advisory review |\n")
	output.WriteString("| --- | ---: | --- | --- | --- | --- |\n")
	for _, attempt := range attempts {
		deterministic := "incomplete"
		if attempt.Deterministic.Completed {
			if attempt.Deterministic.Passed {
				deterministic = "pass"
			} else {
				deterministic = "fail"
			}
		}
		evidence := "incomplete"
		if attempt.EvidenceComplete {
			evidence = "complete"
		}
		recording := "missing"
		if attempt.Media != nil {
			recording = "[" + markdownEscape(attempt.Media.Kind) + "](" +
				markdownEscape(attempt.Directory+"/"+attempt.Media.Path) + ")"
		}
		output.WriteString("| " + markdownEscape(attempt.Case) + " | ")
		output.WriteString(fmt.Sprint(attempt.Trial) + " | " + deterministic + " | " + evidence + " | ")
		output.WriteString(recording + " | pending |\n")
	}
	output.WriteString("\nSummary: ")
	output.WriteString(fmt.Sprintf("%d/%d deterministic passes; %d completed; bundle population %d.\n",
		result.Summary.Passed, result.Expected, result.Summary.Completed, len(attempts)))
	payload := []byte(output.String())
	if guard.rejects(payload) {
		return []byte("# Candidate multimodal review bundle\n\nReview text was withheld because it contained a declared sensitive value. Deterministic result and retained source manifests remain available.\n")
	}
	return payload
}

func markdownEscape(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "|", "\\|", "[", "\\[", "]", "\\]", "\n", " ", "\r", " ")
	return replacer.Replace(value)
}

func equalCanonical(left, right []byte) bool {
	return bytes.Equal(bytes.TrimSpace(left), bytes.TrimSpace(right))
}
