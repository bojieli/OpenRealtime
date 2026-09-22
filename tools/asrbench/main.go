// Command asrbench replays recorded utterances through an OpenRealtime
// recogniser adapter at wall-clock speed and records when each piece of
// evidence became available.
//
// It measures the adapter as the runtime uses it: the same provider factory,
// 100 ms frames, real-time pacing, and one Finalize at the end of the audio.
// For every utterance it reports the first non-empty hypothesis, the first
// committed (stable) text, every revision with its wall-clock offset from the
// start of the audio, the finalization delay after the last frame, whether a
// committed prefix was ever withdrawn, and the edit distance of the final text
// against the reference transcript.
//
// Fixtures are a JSONL manifest of {"id","pcm","text","language"} where pcm is
// raw little-endian PCM16 mono at 24 kHz (tools/duplexmodels/fixtures.py
// writes them).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/providers"
)

const (
	sampleRate  = 24_000
	frameMillis = 100
	frameBytes  = sampleRate * frameMillis / 1000 * 2
)

type fixture struct {
	ID       string `json:"id"`
	PCM      string `json:"pcm"`
	Text     string `json:"text"`
	Language string `json:"language"`
}

type revisionRecord struct {
	AtMS     float64 `json:"at_ms"`
	AudioMS  float64 `json:"audio_ms"`
	Stable   string  `json:"stable"`
	Unstable string  `json:"unstable"`
	Final    bool    `json:"final"`
}

type result struct {
	ID               string   `json:"id"`
	Language         string   `json:"language"`
	Reference        string   `json:"reference"`
	Hypothesis       string   `json:"hypothesis"`
	AudioMS          float64  `json:"audio_ms"`
	Errors           int      `json:"errors"`
	ReferenceUnits   int      `json:"reference_units"`
	Unit             string   `json:"unit"`
	FirstPartialMS   *float64 `json:"first_partial_ms,omitempty"`
	FirstCommittedMS *float64 `json:"first_committed_ms,omitempty"`
	FinalizeMS       float64  `json:"finalize_ms"`
	Revisions        int      `json:"revisions"`
	Withdrawals      int      `json:"committed_withdrawals"`
	// Rewrites counts hypotheses that did not extend the previous one: the
	// provisional text a consumer had already seen was replaced. RewriteDepth
	// sums how many characters each rewrite took back.
	Rewrites       int              `json:"provisional_rewrites"`
	RewriteDepth   int              `json:"rewrite_depth_chars"`
	CommittedCurve []revisionRecord `json:"revision_trace"`
	Error          string           `json:"error,omitempty"`
}

func main() {
	var (
		manifest    = flag.String("manifest", "", "JSONL fixture manifest")
		provider    = flag.String("provider", "qwen-asr", "recogniser catalogue name")
		model       = flag.String("model", "", "model override")
		baseURL     = flag.String("url", "", "endpoint override")
		language    = flag.String("language", "", "recognition language, where the provider takes one")
		out         = flag.String("out", "", "result JSON path")
		limit       = flag.Int("limit", 0, "maximum fixtures (0 = all)")
		concurrency = flag.Int("concurrency", 1, "utterances replayed at once")
		label       = flag.String("label", "", "cell label recorded in the result")
	)
	flag.Parse()
	if *manifest == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "asrbench: -manifest and -out are required")
		os.Exit(2)
	}
	fixtures, err := load(*manifest, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "asrbench:", err)
		os.Exit(1)
	}
	factory, err := providers.NewASRFactory(providers.ASRRequest{
		Provider: *provider, Model: *model, BaseURL: *baseURL, Language: *language,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "asrbench:", err)
		os.Exit(1)
	}
	results := make([]result, len(fixtures))
	work := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < max(1, *concurrency); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range work {
				results[index] = replay(factory, fixtures[index])
				current := results[index]
				fmt.Fprintf(os.Stderr, "%s errors=%d/%d finalize=%.0fms %q\n",
					current.ID, current.Errors, current.ReferenceUnits, current.FinalizeMS, current.Hypothesis)
			}
		}()
	}
	for index := range fixtures {
		work <- index
	}
	close(work)
	group.Wait()
	report := summarise(results)
	report["provider"], report["model"], report["url"], report["label"] = *provider, *model, *baseURL, *label
	report["manifest"], report["concurrency"] = *manifest, *concurrency
	report["utterances"] = results
	encoded, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "asrbench:", err)
		os.Exit(1)
	}
	summary, _ := json.MarshalIndent(report["summary"], "", "  ")
	fmt.Println(string(summary))
}

func load(path string, limit int) ([]fixture, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var fixtures []fixture
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry fixture
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("decode manifest line: %w", err)
		}
		fixtures = append(fixtures, entry)
		if limit > 0 && len(fixtures) == limit {
			break
		}
	}
	return fixtures, scanner.Err()
}

func replay(factory func() (v1.PerceptionProvider, error), entry fixture) result {
	output := result{ID: entry.ID, Language: entry.Language, Reference: entry.Text}
	audio, err := os.ReadFile(entry.PCM)
	if err != nil {
		output.Error = err.Error()
		return output
	}
	audio = audio[:len(audio)/2*2]
	output.AudioMS = float64(len(audio)/2) * 1000 / sampleRate
	recogniser, err := factory()
	if err != nil {
		output.Error = err.Error()
		return output
	}
	if closer, ok := recogniser.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(output.AudioMS)*time.Millisecond+60*time.Second)
	defer cancel()
	start := time.Now()
	lastStable, lastWhole := "", ""
	observe := func(revisions []v1.PerceptionRevision, audioMS float64) {
		for _, revision := range revisions {
			at := float64(time.Since(start).Microseconds()) / 1000
			output.Revisions++
			output.CommittedCurve = append(output.CommittedCurve, revisionRecord{
				AtMS: at, AudioMS: audioMS, Stable: revision.StableText,
				Unstable: revision.UnstableText, Final: revision.Final,
			})
			if output.FirstPartialMS == nil && v1.CarriesSpeech(revision.StableText+revision.UnstableText) {
				value := at
				output.FirstPartialMS = &value
			}
			if output.FirstCommittedMS == nil && v1.CarriesSpeech(revision.StableText) {
				value := at
				output.FirstCommittedMS = &value
			}
			if !strings.HasPrefix(strings.TrimSpace(revision.StableText), strings.TrimSpace(lastStable)) {
				output.Withdrawals++
			}
			lastStable = revision.StableText
			whole := []rune(strings.TrimSpace(revision.StableText + revision.UnstableText))
			previous := []rune(lastWhole)
			common := 0
			for common < len(whole) && common < len(previous) && whole[common] == previous[common] {
				common++
			}
			if common < len(previous) {
				output.Rewrites++
				output.RewriteDepth += len(previous) - common
			}
			lastWhole = string(whole)
		}
	}
	var offset uint64
	index := uint64(0)
	for position := 0; position < len(audio); position += frameBytes {
		end := min(position+frameBytes, len(audio))
		// Pace to the wall clock: a frame is sent when its last sample has
		// been "spoken".
		due := start.Add(time.Duration(float64(end/2) * float64(time.Second) / sampleRate))
		time.Sleep(time.Until(due))
		chunk := audio[position:end]
		revisions, err := recogniser.PushFrame(ctx, v1.AudioFrame{
			Index: index, SampleOffset: offset, SampleRateHz: sampleRate, PCM16LE: chunk,
		})
		if err != nil {
			output.Error = err.Error()
			return output
		}
		observe(revisions, float64(end/2)*1000/sampleRate)
		index++
		offset += uint64(len(chunk) / 2)
	}
	finalizeStart := time.Now()
	final, err := recogniser.Finalize(ctx, offset)
	output.FinalizeMS = float64(time.Since(finalizeStart).Microseconds()) / 1000
	if err != nil {
		output.Error = err.Error()
		return output
	}
	observe([]v1.PerceptionRevision{final}, output.AudioMS)
	output.Hypothesis = strings.TrimSpace(final.StableText + final.UnstableText)
	output.Errors, output.ReferenceUnits, output.Unit = score(entry.Text, output.Hypothesis, entry.Language)
	return output
}

// score returns the edit distance in words, or in characters for languages
// written without spaces.
func score(reference, hypothesis, language string) (int, int, string) {
	characters := strings.HasPrefix(language, "zh") || strings.HasPrefix(language, "ja")
	tokenize := func(text string) []string {
		text = strings.ToLower(text)
		var cleaned strings.Builder
		for _, r := range text {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r):
				cleaned.WriteRune(r)
			case r == '\'':
				// keep contractions whole
				cleaned.WriteRune(r)
			default:
				cleaned.WriteRune(' ')
			}
		}
		if characters {
			var units []string
			for _, r := range cleaned.String() {
				if !unicode.IsSpace(r) {
					units = append(units, string(r))
				}
			}
			return units
		}
		return strings.Fields(cleaned.String())
	}
	ref, hyp := tokenize(reference), tokenize(hypothesis)
	unit := "word"
	if characters {
		unit = "character"
	}
	return distance(ref, hyp), len(ref), unit
}

func distance(a, b []string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func summarise(results []result) map[string]any {
	errorsByLanguage := map[string][2]int{}
	var firstPartial, firstCommitted, finalize []float64
	failed, withdrawals, revisions, rewrites, depth := 0, 0, 0, 0, 0
	for _, current := range results {
		if current.Error != "" {
			failed++
			continue
		}
		totals := errorsByLanguage[current.Language]
		totals[0] += current.Errors
		totals[1] += current.ReferenceUnits
		errorsByLanguage[current.Language] = totals
		if current.FirstPartialMS != nil {
			firstPartial = append(firstPartial, *current.FirstPartialMS)
		}
		if current.FirstCommittedMS != nil {
			firstCommitted = append(firstCommitted, *current.FirstCommittedMS)
		}
		finalize = append(finalize, current.FinalizeMS)
		withdrawals += current.Withdrawals
		revisions += current.Revisions
		rewrites += current.Rewrites
		depth += current.RewriteDepth
	}
	rates := map[string]float64{}
	for language, totals := range errorsByLanguage {
		if totals[1] > 0 {
			rates[language] = float64(totals[0]) / float64(totals[1])
		}
	}
	return map[string]any{"summary": map[string]any{
		"utterances": len(results), "failed": failed, "error_rate": rates,
		"first_partial_ms": distribution(firstPartial), "first_committed_ms": distribution(firstCommitted),
		"finalize_ms": distribution(finalize), "committed_withdrawals": withdrawals, "revisions": revisions,
		"provisional_rewrites": rewrites, "rewrite_depth_chars": depth,
	}}
}

func distribution(values []float64) map[string]float64 {
	if len(values) == 0 {
		return map[string]float64{"count": 0}
	}
	sort.Float64s(values)
	at := func(q float64) float64 { return values[min(len(values)-1, int(q*float64(len(values))))] }
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return map[string]float64{
		"count": float64(len(values)), "p50": at(0.5), "p90": at(0.9), "max": values[len(values)-1],
		"mean": sum / float64(len(values)),
	}
}
