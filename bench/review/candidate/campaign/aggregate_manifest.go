package campaign

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	AggregateManifestFormat        = "openrealtime.candidate-review-campaign"
	AggregateManifestFormatVersion = 1
	AggregateReceiptFormat         = "openrealtime.candidate-review-campaign-receipt"
	AggregateReceiptFormatVersion  = 1

	aggregateManifestName = "manifest.json"
	aggregateResultName   = "campaign.json"
	aggregateReviewName   = "REVIEW.md"
	maximumAggregateJSON  = 64 << 20
)

type AggregateOptions struct {
	Directory           string
	ReceiptPath         string
	SourceReceiptPath   string
	QuarantineDirectory string
	SensitiveValues     []string
}

type AggregateFile struct {
	Path      string `json:"path"`
	Purpose   string `json:"purpose"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type AggregateEvaluation struct {
	AttemptID     string                         `json:"attempt_id"`
	Suite         string                         `json:"suite"`
	Case          string                         `json:"case"`
	Trial         int                            `json:"trial"`
	ReceiptPath   string                         `json:"receipt_path"`
	Receipt       review.EvaluationBundleReceipt `json:"receipt"`
	RecordSHA256  string                         `json:"record_sha256"`
	Media         []AggregateMedia               `json:"media"`
	Deterministic string                         `json:"deterministic"`
	Observed      string                         `json:"observed"`
	Agrees        bool                           `json:"agrees"`
	MediaUsable   bool                           `json:"media_usable"`
	Confidence    float64                        `json:"confidence"`
	ProblemCount  int                            `json:"significant_problem_count"`
	Assessment    review.Assessment              `json:"assessment"`
}

type AggregateMedia struct {
	Ordinal   int    `json:"ordinal"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type AggregateManifest struct {
	Format              string                    `json:"format"`
	FormatVersion       int                       `json:"format_version"`
	Complete            bool                      `json:"complete"`
	Source              sourcebundle.Receipt      `json:"source_receipt"`
	Provider            review.ProviderDescriptor `json:"provider"`
	Expected            int                       `json:"expected_evaluations"`
	EvaluationCount     int                       `json:"evaluation_count"`
	EvaluationSetSHA256 string                    `json:"evaluation_set_sha256"`
	ResultPath          string                    `json:"result_path"`
	ResultSHA256        string                    `json:"result_sha256"`
	ReviewPath          string                    `json:"review_path"`
	ReviewSHA256        string                    `json:"review_sha256"`
	FileSetSHA256       string                    `json:"file_set_sha256"`
	Files               []AggregateFile           `json:"files"`
	Evaluations         []AggregateEvaluation     `json:"evaluations"`
}

type AggregateReceipt struct {
	Format               string `json:"format"`
	FormatVersion        int    `json:"format_version"`
	Directory            string `json:"directory"`
	ManifestSHA256       string `json:"manifest_sha256"`
	FileSetSHA256        string `json:"file_set_sha256"`
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	EvaluationSetSHA256  string `json:"evaluation_set_sha256"`
	EvaluationCount      int    `json:"evaluation_count"`
	ReceiptSHA256        string `json:"receipt_sha256"`
}

type AggregateBundle struct {
	Manifest AggregateManifest
	Result   Result
	Receipt  AggregateReceipt
}

func aggregateCanonical(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil || len(payload) == 0 || len(payload) > maximumAggregateJSON {
		return nil, errors.New("candidate review aggregate metadata is empty or oversized")
	}
	payload = append(payload, '\n')
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumAggregateJSON, MaxDepth: 128, MaxTokens: 5_000_000,
		MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: maximumAggregateJSON,
		MaxWorkBytes: 256 << 20,
	}); err != nil {
		return nil, errors.New("candidate review aggregate metadata is not strict JSON")
	}
	return payload, nil
}

func aggregateCompact(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil || len(payload) == 0 || len(payload) > maximumAggregateJSON {
		return nil, errors.New("candidate review aggregate identity is empty or oversized")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumAggregateJSON, MaxDepth: 128, MaxTokens: 5_000_000,
		MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: maximumAggregateJSON,
		MaxWorkBytes: 256 << 20,
	}); err != nil {
		return nil, errors.New("candidate review aggregate identity is not strict JSON")
	}
	return payload, nil
}

func aggregateDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func aggregateFileSetDigest(files []AggregateFile) (string, error) {
	payload, err := aggregateCompact(files)
	if err != nil {
		return "", err
	}
	return aggregateDigest(payload), nil
}

func aggregateEvaluationSetDigest(evaluations []AggregateEvaluation) (string, error) {
	payload, err := aggregateCompact(evaluations)
	if err != nil {
		return "", err
	}
	return aggregateDigest(payload), nil
}

func aggregateReceiptPayload(receipt AggregateReceipt) ([]byte, error) {
	identity := receipt
	identity.Directory = ""
	identity.ReceiptSHA256 = ""
	payload, err := aggregateCompact(identity)
	if err != nil {
		return nil, err
	}
	receipt.ReceiptSHA256 = aggregateDigest(payload)
	return aggregateCanonical(receipt)
}

func aggregateReview(
	result Result, evaluations []AggregateEvaluation, aggregateDirectory string,
) []byte {
	var output strings.Builder
	output.WriteString("# Candidate case-by-case media review\n\n")
	output.WriteString("The deterministic benchmark scorer is authoritative. ")
	output.WriteString("The selected multimodal provider is an advisory reviewer.\n\n")
	passes, failures, infrastructure, agreements, usable := 0, 0, 0, 0, 0
	for _, evaluation := range evaluations {
		switch evaluation.Deterministic {
		case "pass":
			passes++
		case "fail":
			failures++
		default:
			infrastructure++
		}
		if evaluation.Agrees {
			agreements++
		}
		if evaluation.MediaUsable {
			usable++
		}
	}
	output.WriteString(fmt.Sprintf(
		"- Source manifest: `%s`\n- Provider: `%s/%s` (`%s`, revision `%s`)\n"+
			"- Deterministic: %d pass, %d fail, %d infrastructure failure\n"+
			"- Advisory agreement: %d/%d; usable media: %d/%d\n\n",
		result.Source.ManifestSHA256, markdownText(result.Provider.Provider),
		markdownText(result.Provider.Model), markdownText(result.Provider.API),
		markdownText(result.Provider.APIRevision), passes, failures, infrastructure,
		agreements, len(evaluations), usable, len(evaluations),
	))
	output.WriteString("| Case | Trial | Deterministic | Advisory | Agreement | Media | Confidence | Recording |\n")
	output.WriteString("| --- | ---: | --- | --- | --- | --- | ---: | --- |\n")
	for _, evaluation := range evaluations {
		agreement := "disagrees"
		if evaluation.Agrees {
			agreement = "agrees"
		}
		usable := "unusable"
		if evaluation.MediaUsable {
			usable = "usable"
		}
		recording := "missing"
		if len(evaluation.Media) > 0 {
			links := make([]string, 0, len(evaluation.Media))
			for _, media := range evaluation.Media {
				relative, err := filepath.Rel(aggregateDirectory, media.Path)
				if err != nil || relative == "" {
					relative = media.Path
				}
				links = append(links, fmt.Sprintf(
					"[media %d (%s)](%s)", media.Ordinal, markdownText(media.MediaType),
					markdownPath(relative),
				))
			}
			recording = strings.Join(links, " ")
		}
		output.WriteString("| " + markdownText(evaluation.Case) + " | ")
		output.WriteString(fmt.Sprintf("%d | %s | %s | %s | %s | %.3f | %s |\n",
			evaluation.Trial, evaluation.Deterministic, evaluation.Observed,
			agreement, usable, evaluation.Confidence, recording))
	}
	output.WriteString("\n## Per-case findings\n\n")
	for _, evaluation := range evaluations {
		output.WriteString("### " + markdownText(evaluation.Case) +
			fmt.Sprintf(" (trial %d)\n\n", evaluation.Trial))
		output.WriteString("Deterministic: " + evaluation.Deterministic +
			". Advisory: " + evaluation.Observed + ".\n\n")
		receiptRelative, err := filepath.Rel(aggregateDirectory, evaluation.ReceiptPath)
		if err != nil || receiptRelative == "" {
			receiptRelative = evaluation.ReceiptPath
		}
		output.WriteString(fmt.Sprintf(
			"Evaluation receipt: [open](%s) (`%s`).\n\n",
			markdownPath(receiptRelative), evaluation.Receipt.ReceiptSHA256,
		))
		output.WriteString(markdownText(evaluation.Assessment.Summary) + "\n\n")
		if len(evaluation.Assessment.SignificantProblems) > 0 {
			output.WriteString("Significant problems:\n\n")
			for _, finding := range evaluation.Assessment.SignificantProblems {
				output.WriteString("- " + findingWindow(finding) + markdownText(finding.Category) + ": " +
					markdownText(finding.Evidence) + " Impact: " + markdownText(finding.Impact) + "\n")
			}
			output.WriteString("\n")
		}
		if len(evaluation.Assessment.MinorObservations) > 0 {
			output.WriteString("Minor observations:\n\n")
			for _, finding := range evaluation.Assessment.MinorObservations {
				output.WriteString("- " + findingWindow(finding) + markdownText(finding.Category) + ": " +
					markdownText(finding.Evidence) + "\n")
			}
			output.WriteString("\n")
		}
		if len(evaluation.Assessment.Limitations) > 0 {
			output.WriteString("Limitations:\n\n")
			for _, limitation := range evaluation.Assessment.Limitations {
				output.WriteString("- " + markdownText(limitation) + "\n")
			}
			output.WriteString("\n")
		}
	}
	output.WriteString(fmt.Sprintf(
		"Reviewed %d/%d new candidate attempts with %s/%s.\n",
		len(evaluations), result.Expected, result.Provider.Provider, result.Provider.Model,
	))
	return []byte(output.String())
}

func markdownText(value string) string {
	value = strings.Map(func(symbol rune) rune {
		if symbol == '\n' || symbol == '\r' || symbol == 0 {
			return ' '
		}
		return symbol
	}, value)
	replacer := strings.NewReplacer(
		"\\", "\\\\", "|", "\\|", "[", "\\[", "]", "\\]",
		"<", "&lt;", ">", "&gt;", "`", "\\`",
	)
	return replacer.Replace(value)
}

func markdownPath(value string) string {
	return (&url.URL{Path: filepath.ToSlash(value)}).EscapedPath()
}

func findingWindow(finding review.Finding) string {
	if finding.StartMS == nil && finding.EndMS == nil {
		return ""
	}
	start, end := "?", "?"
	if finding.StartMS != nil {
		start = fmt.Sprint(*finding.StartMS)
	}
	if finding.EndMS != nil {
		end = fmt.Sprint(*finding.EndMS)
	}
	return "[" + start + "–" + end + " ms] "
}

func canonicalAggregateEvaluations(source []AggregateEvaluation) []AggregateEvaluation {
	result := make([]AggregateEvaluation, len(source))
	copy(result, source)
	for index := range result {
		result[index].Media = append([]AggregateMedia(nil), result[index].Media...)
		sort.Slice(result[index].Media, func(left, right int) bool {
			if result[index].Media[left].Ordinal == result[index].Media[right].Ordinal {
				return result[index].Media[left].Path < result[index].Media[right].Path
			}
			return result[index].Media[left].Ordinal < result[index].Media[right].Ordinal
		})
	}
	return result
}
