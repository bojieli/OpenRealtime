// Package frontier renders the M4 latency/quality/compute comparison.
package frontier

import (
	"fmt"
	"html/template"
	"io"

	m4experiment "github.com/bojieli/OpenRealtime/experiments/m4"
)

type row struct {
	Condition       string
	FirstProgressMS float64
	FinalAnswerMS   float64
	Quality         uint64
	Compute         uint64
	Successes       uint64
	Trials          uint64
}

func Render(output io.Writer, report m4experiment.Report) error {
	if len(report.Conditions) == 0 {
		return fmt.Errorf("frontier visualization requires conditions")
	}
	rows := make([]row, 0, len(report.Conditions))
	for _, condition := range report.Conditions {
		count := condition.FirstTruthfulProgress.Count
		if count == 0 || condition.FinalAnswer.Count != count || condition.Quality.Count != count || condition.Compute.Count != count {
			return fmt.Errorf("condition %q has inconsistent distributions", condition.Kind)
		}
		rows = append(rows, row{
			Condition: string(condition.Kind), FirstProgressMS: float64(condition.FirstTruthfulProgress.P50NS) / 1_000_000,
			FinalAnswerMS: float64(condition.FinalAnswer.P50NS) / 1_000_000,
			Quality:       condition.Quality.P50, Compute: condition.Compute.P50,
			Successes: condition.TaskSuccessCount, Trials: count,
		})
	}
	return pageTemplate.Execute(output, rows)
}

var pageTemplate = template.Must(template.New("frontier").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>M4 fast/slow frontier</title><style>
:root{font-family:ui-sans-serif,system-ui,sans-serif;color:#172033;background:#f7f8fb}body{max-width:980px;margin:0 auto;padding:32px 24px}
.card{background:#fff;border:1px solid #dce1ea;border-radius:12px;padding:18px}p{color:#526078}table{border-collapse:collapse;width:100%}
th,td{text-align:right;padding:10px;border-bottom:1px solid #e8ebf0}th:first-child,td:first-child{text-align:left}
</style></head><body><h1>M4 fast/slow cognition frontier</h1>
<p>Deterministic symbolic scores and compute units. Values are instrumentation, not model benchmarks.</p><div class="card">
<table><thead><tr><th>condition</th><th>truthful progress P50 ms</th><th>final answer P50 ms</th><th>quality P50</th><th>compute P50</th><th>success</th></tr></thead><tbody>
{{range .}}<tr><td>{{.Condition}}</td><td>{{printf "%.3f" .FirstProgressMS}}</td><td>{{printf "%.3f" .FinalAnswerMS}}</td><td>{{.Quality}}</td><td>{{.Compute}}</td><td>{{.Successes}} / {{.Trials}}</td></tr>{{end}}
</tbody></table></div></body></html>`))
