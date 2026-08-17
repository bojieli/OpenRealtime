// Package ablation renders self-contained cadence comparison reports.
package ablation

import (
	"fmt"
	"html/template"
	"io"

	"github.com/bojieli/OpenRealtime/experiments/m2"
)

type point struct {
	Name           string
	ObservedP50MS  float64
	ObservedP95MS  float64
	BaselineP50MS  float64
	DeltaP50MS     float64
	PreparedPreEnd uint64
	TrialCount     uint64
	X              int
	ObservedY      float64
	BaselineY      float64
}

type page struct {
	Points []point
	MaxMS  float64
}

func Render(output io.Writer, report m2.Report) error {
	if len(report.Conditions) == 0 {
		return fmt.Errorf("ablation visualization requires at least one condition")
	}
	points := make([]point, 0, len(report.Conditions))
	var maxMS float64
	for index, condition := range report.Conditions {
		observed, observedOK := condition.Distributions["observed_latency_ns"]
		baseline, baselineOK := condition.Distributions["baseline_latency_ns"]
		delta, deltaOK := condition.SignedDistributions["observed_minus_baseline_ns"]
		if !observedOK || !baselineOK || !deltaOK || observed.Count == 0 || baseline.Count != observed.Count || delta.Count != observed.Count {
			return fmt.Errorf("condition %q is missing consistent latency distributions", condition.Policy.Name)
		}
		point := point{
			Name: condition.Policy.Name, ObservedP50MS: nsToMS(observed.P50NS),
			ObservedP95MS: nsToMS(observed.P95NS), BaselineP50MS: nsToMS(baseline.P50NS),
			DeltaP50MS:     float64(delta.P50NS) / 1_000_000,
			PreparedPreEnd: condition.PreparedPreEndpointCount, TrialCount: observed.Count,
			X: 95 + index*165,
		}
		if point.BaselineP50MS > maxMS {
			maxMS = point.BaselineP50MS
		}
		if point.ObservedP95MS > maxMS {
			maxMS = point.ObservedP95MS
		}
		points = append(points, point)
	}
	if maxMS == 0 {
		maxMS = 1
	}
	for index := range points {
		points[index].ObservedY = 250 - points[index].ObservedP50MS/maxMS*190
		points[index].BaselineY = 250 - points[index].BaselineP50MS/maxMS*190
	}
	return pageTemplate.Execute(output, page{Points: points, MaxMS: maxMS})
}

func nsToMS(value uint64) float64 { return float64(value) / 1_000_000 }

var pageTemplate = template.Must(template.New("ablation").Funcs(template.FuncMap{
	"add": func(left, right int) int { return left + right },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>M2 cadence ablation</title>
<style>
:root{font-family:ui-sans-serif,system-ui,sans-serif;color:#172033;background:#f7f8fb}body{max-width:1160px;margin:0 auto;padding:32px 24px}
h1{margin-bottom:4px}p{color:#526078}.card{background:#fff;border:1px solid #dce1ea;border-radius:12px;padding:18px;overflow:auto}
svg{min-width:1080px;width:100%;height:330px}.axis{stroke:#98a2b3}.baseline{fill:#98a2b3}.observed{fill:#2563eb}.label{font-size:12px;fill:#344054}
table{border-collapse:collapse;width:100%;font-size:13px;margin-top:20px}th,td{text-align:right;padding:8px;border-bottom:1px solid #e8ebf0}th:first-child,td:first-child{text-align:left}
</style>
</head>
<body>
<h1>M2 cadence ablation</h1>
<p>Deterministic orchestration simulation. Lower response latency is better; this is not provider performance.</p>
<div class="card">
<svg viewBox="0 0 1100 330" role="img" aria-label="M2 response latency by scheduling policy">
<line class="axis" x1="50" y1="250" x2="1070" y2="250"/>
{{range .Points}}<line class="baseline" x1="{{.X}}" y1="250" x2="{{.X}}" y2="{{printf "%.2f" .BaselineY}}" stroke-width="8"><title>baseline P50 {{printf "%.3f" .BaselineP50MS}} ms</title></line>
<line class="observed" x1="{{add .X 18}}" y1="250" x2="{{add .X 18}}" y2="{{printf "%.2f" .ObservedY}}" stroke-width="8"><title>observed P50 {{printf "%.3f" .ObservedP50MS}} ms</title></line>
<text class="label" x="{{add .X -30}}" y="275" transform="rotate(20 {{add .X -30}} 275)">{{.Name}}</text>{{end}}
<text class="label" x="8" y="64">{{printf "%.1f" .MaxMS}} ms</text><text class="label" x="8" y="253">0 ms</text>
</svg>
<table><thead><tr><th>policy</th><th>observed P50 ms</th><th>observed P95 ms</th><th>paired delta P50 ms</th><th>prepared pre-endpoint</th></tr></thead><tbody>
{{range .Points}}<tr><td>{{.Name}}</td><td>{{printf "%.3f" .ObservedP50MS}}</td><td>{{printf "%.3f" .ObservedP95MS}}</td><td>{{printf "%.3f" .DeltaP50MS}}</td><td>{{.PreparedPreEnd}} / {{.TrialCount}}</td></tr>{{end}}
</tbody></table>
</div>
</body>
</html>
`))
