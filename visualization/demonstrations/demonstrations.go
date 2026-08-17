// Package demonstrations renders the M5 translation and rapid-game results.
package demonstrations

import (
	"fmt"
	"html/template"
	"io"

	m5experiment "github.com/bojieli/OpenRealtime/experiments/m5"
)

type translationRow struct {
	Condition    string
	MeanLagMS    float64
	CompletionMS float64
	Quality      uint64
	Failures     uint64
	Compute      uint64
	Trials       uint64
}

type gameRow struct {
	Condition  string
	ReactionMS float64
	Quality    uint64
	Failures   uint64
	Compute    uint64
	Trials     uint64
}

type page struct {
	Translation []translationRow
	GameName    string
	Game        []gameRow
}

func Render(output io.Writer, report m5experiment.Report) error {
	if len(report.Translation.Conditions) == 0 || len(report.Game.Conditions) == 0 {
		return fmt.Errorf("M5 visualization requires translation and game conditions")
	}
	view := page{GameName: report.Game.Name}
	for _, condition := range report.Translation.Conditions {
		trials := condition.MeanLag.Count
		if trials == 0 || condition.CompletionLag.Count != trials || condition.Quality.Count != trials || condition.Compute.Count != trials {
			return fmt.Errorf("translation condition %q has inconsistent distributions", condition.Policy)
		}
		view.Translation = append(view.Translation, translationRow{
			Condition: string(condition.Policy), MeanLagMS: float64(condition.MeanLag.P50NS) / 1_000_000,
			CompletionMS: float64(condition.CompletionLag.P50NS) / 1_000_000,
			Quality:      condition.Quality.P50, Failures: condition.FailureCount,
			Compute: condition.Compute.P50, Trials: trials,
		})
	}
	for _, condition := range report.Game.Conditions {
		trials := condition.Quality.Count
		if trials == 0 || condition.Compute.Count != trials || condition.Failures.Count != trials {
			return fmt.Errorf("game condition %q has inconsistent distributions", condition.Condition)
		}
		view.Game = append(view.Game, gameRow{
			Condition: string(condition.Condition), ReactionMS: float64(condition.ReactionLatency.P50NS) / 1_000_000,
			Quality: condition.Quality.P50, Failures: condition.FailureCount,
			Compute: condition.Compute.P50, Trials: trials,
		})
	}
	return pageTemplate.Execute(output, view)
}

var pageTemplate = template.Must(template.New("demonstrations").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>M5 translation and rapid interaction</title><style>
:root{font-family:ui-sans-serif,system-ui,sans-serif;color:#172033;background:#f7f8fb}body{max-width:1050px;margin:0 auto;padding:32px 24px}
.card{background:#fff;border:1px solid #dce1ea;border-radius:12px;padding:18px;margin:18px 0}p{color:#526078}table{border-collapse:collapse;width:100%}
th,td{text-align:right;padding:10px;border-bottom:1px solid #e8ebf0}th:first-child,td:first-child{text-align:left}
</style></head><body><h1>M5 translation and rapid interaction</h1>
<p>Deterministic symbolic exact-match and deadline instrumentation. These values are not model or natural-language quality benchmarks.</p>
<div class="card"><h2>Simultaneous translation</h2><table><thead><tr><th>policy</th><th>mean lag P50 ms</th><th>completion lag P50 ms</th><th>quality P50</th><th>failures</th><th>compute P50</th></tr></thead><tbody>
{{range .Translation}}<tr><td>{{.Condition}}</td><td>{{printf "%.3f" .MeanLagMS}}</td><td>{{printf "%.3f" .CompletionMS}}</td><td>{{.Quality}}</td><td>{{.Failures}}</td><td>{{.Compute}}</td></tr>{{end}}
</tbody></table></div><div class="card"><h2>Rapid audio game: {{.GameName}}</h2><table><thead><tr><th>condition</th><th>reaction P50 ms</th><th>quality P50</th><th>deadline failures</th><th>compute P50</th></tr></thead><tbody>
{{range .Game}}<tr><td>{{.Condition}}</td><td>{{printf "%.3f" .ReactionMS}}</td><td>{{.Quality}}</td><td>{{.Failures}}</td><td>{{.Compute}}</td></tr>{{end}}
</tbody></table></div></body></html>`))
