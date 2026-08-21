package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Distribution summarises a sample.
//
// It exists because a mean is not a latency claim. A system with a 200 ms mean
// and a 4 s ninety-ninth percentile is a system people notice being slow, and
// reporting the mean alone describes something nobody is using.
//
// The unit is carried rather than assumed. Suites report counts alongside
// durations - turns taken, turns missed, calls made - and a percentile of a
// count rendered as "2 ms" is not a smaller mistake than a wrong number, it is
// a number that means nothing and looks like it means something.
type Distribution struct {
	Count int     `json:"count"`
	Unit  string  `json:"unit"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

// UnitOf infers a metric's unit from its name.
//
// Naming is the convention the suites already follow - a duration ends in _ms
// - so the unit is derived from it rather than declared twice and allowed to
// disagree with itself.
func UnitOf(metric string) string {
	switch {
	case strings.HasSuffix(metric, "_ms"):
		return "ms"
	case strings.HasSuffix(metric, "_s"):
		return "s"
	case strings.HasSuffix(metric, "_usd"):
		return "usd"
	case strings.HasSuffix(metric, "_ratio"), strings.HasSuffix(metric, "_rate"):
		return "ratio"
	default:
		return "count"
	}
}

// Format renders one value with its unit, for a report a person reads.
func (distribution Distribution) Format(value float64) string {
	switch distribution.Unit {
	case "ms":
		return fmt.Sprintf("%.0f ms", value)
	case "s":
		return fmt.Sprintf("%.2f s", value)
	case "usd":
		return fmt.Sprintf("$%.4f", value)
	case "ratio":
		return fmt.Sprintf("%.1f%%", value*100)
	default:
		return fmt.Sprintf("%.3g", value)
	}
}

// Summarise builds a distribution from milliseconds.
func Summarise(values []float64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	total := 0.0
	for _, value := range sorted {
		total += value
	}
	return Distribution{
		Count: len(sorted), Min: sorted[0], Max: sorted[len(sorted)-1],
		P50:  percentile(sorted, 0.50),
		P90:  percentile(sorted, 0.90),
		P95:  percentile(sorted, 0.95),
		P99:  percentile(sorted, 0.99),
		Mean: total / float64(len(sorted)),
	}
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	position := fraction * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

// TaskOutcome is one unit of work in a suite.
type TaskOutcome struct {
	ID string `json:"id"`
	// Completed reports whether the task ran to a conclusion. A task that
	// errored is recorded, not dropped: an incomplete cell must be visible.
	Completed bool `json:"completed"`
	// Passed is the suite's own judgement. It is meaningful only when the task
	// completed.
	Passed bool `json:"passed"`
	// Error is why an incomplete task did not finish.
	Error string `json:"error,omitempty"`
	// Metrics are suite-specific numbers, in milliseconds where they are
	// durations.
	Metrics map[string]float64 `json:"metrics,omitempty"`
	// Notes carry anything a person would want when reading a surprising row.
	Notes map[string]string `json:"notes,omitempty"`
}

// Result is one completed cell of one suite.
type Result struct {
	Suite      string     `json:"suite"`
	Cell       Cell       `json:"cell"`
	Provenance Provenance `json:"provenance"`

	// Expected is how many tasks the suite declared. A cell is complete only
	// when every one of them ran.
	Expected int `json:"expected_tasks"`

	Tasks []TaskOutcome `json:"tasks"`

	// Summary is derived. It is populated by Finish so a report cannot carry a
	// summary that disagrees with its rows.
	Summary Summary `json:"summary"`
}

// Summary is the derived view of a cell.
type Summary struct {
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Passed    int `json:"passed"`
	// PassRate is meaningful only when the cell is complete.
	PassRate       float64                 `json:"pass_rate"`
	Complete       bool                    `json:"complete"`
	Incompleteness string                  `json:"incompleteness,omitempty"`
	Distributions  map[string]Distribution `json:"distributions,omitempty"`
}

// Finish derives the summary and states whether the cell is reportable.
//
// This is where the first reporting rule is enforced rather than remembered. A
// cell that did not run every task is marked incomplete and says why; nothing
// downstream has to decide whether a partial result counts.
func (result *Result) Finish() {
	summary := Summary{Distributions: map[string]Distribution{}}
	samples := map[string][]float64{}
	for _, task := range result.Tasks {
		if !task.Completed {
			summary.Failed++
			continue
		}
		summary.Completed++
		if task.Passed {
			summary.Passed++
		}
		for name, value := range task.Metrics {
			samples[name] = append(samples[name], value)
		}
	}
	for name, values := range samples {
		distribution := Summarise(values)
		distribution.Unit = UnitOf(name)
		summary.Distributions[name] = distribution
	}
	if summary.Completed > 0 {
		summary.PassRate = float64(summary.Passed) / float64(summary.Completed)
	}
	switch {
	case result.Expected <= 0:
		summary.Incompleteness = "the suite declared no tasks"
	case len(result.Tasks) < result.Expected:
		summary.Incompleteness = fmt.Sprintf(
			"%d of %d tasks were not attempted", result.Expected-len(result.Tasks), result.Expected)
	case summary.Failed > 0:
		summary.Incompleteness = fmt.Sprintf("%d of %d tasks did not complete", summary.Failed, result.Expected)
	default:
		summary.Complete = true
	}
	result.Provenance = result.Provenance.Complete()
	result.Summary = summary
}

// Reportable reports whether this cell may be published.
//
// Two independent conditions, and both are refusals rather than warnings: the
// cell ran completely, and the build it ran on can be traced.
func (result Result) Reportable() error {
	var failures []string
	if !result.Summary.Complete {
		failures = append(failures, result.Summary.Incompleteness)
	}
	if err := result.Provenance.Reproducible(); err != nil {
		failures = append(failures, err.Error())
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("cell %q is not reportable: %s", result.Cell.Name, strings.Join(failures, "; "))
}

// Write saves a result as JSON.
func (result Result) Write(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("a result needs a path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// Comparison is a paired reading of two cells.
type Comparison struct {
	Suite        string  `json:"suite"`
	Factor       Factor  `json:"factor"`
	Baseline     string  `json:"baseline"`
	Variant      string  `json:"variant"`
	BaselineRate float64 `json:"baseline_pass_rate"`
	VariantRate  float64 `json:"variant_pass_rate"`
	Difference   float64 `json:"difference"`
	// Reportable is false when either cell was incomplete or unreproducible.
	Reportable bool   `json:"reportable"`
	Refusal    string `json:"refusal,omitempty"`
}

// Pair compares two cells, refusing when the comparison would not mean
// anything.
//
// Refusing is the point. Two cells that differ in three factors do not measure
// any one of them, an incomplete cell is a different experiment rather than a
// smaller one, and a comparison across suites is two things that do not
// average. All three are refused here rather than left to whoever reads the
// numbers.
func Pair(baseline, variant Result) Comparison {
	comparison := Comparison{
		Suite: baseline.Suite, Baseline: baseline.Cell.Name, Variant: variant.Cell.Name,
		BaselineRate: baseline.Summary.PassRate, VariantRate: variant.Summary.PassRate,
		Difference: variant.Summary.PassRate - baseline.Summary.PassRate,
	}
	var refusals []string
	if baseline.Suite != variant.Suite {
		refusals = append(refusals, fmt.Sprintf(
			"suites %q and %q measure different things and do not compare", baseline.Suite, variant.Suite))
	}
	factor, paired := Paired(baseline.Cell, variant.Cell)
	if !paired {
		refusals = append(refusals, fmt.Sprintf(
			"the cells differ in %d factors, so a difference cannot be attributed to any one of them",
			len(Compare(baseline.Cell, variant.Cell))))
	}
	comparison.Factor = factor
	if err := baseline.Reportable(); err != nil {
		refusals = append(refusals, err.Error())
	}
	if err := variant.Reportable(); err != nil {
		refusals = append(refusals, err.Error())
	}
	if len(refusals) > 0 {
		comparison.Refusal = strings.Join(refusals, "; ")
		return comparison
	}
	comparison.Reportable = true
	return comparison
}
