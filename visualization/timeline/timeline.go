// Package timeline renders self-contained HTML from an OpenRealtime causal trace.
package timeline

import (
	"fmt"
	"html/template"
	"io"
	"strings"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trace"
)

type point struct {
	X         float64
	Y         int
	Color     string
	Type      string
	TimeMS    float64
	Direction string
	TraceID   string
	Label     bool
}

type page struct {
	Title      string
	DurationMS float64
	Points     []point
}

func Render(output io.Writer, title string, records []trace.Record) error {
	if len(records) == 0 {
		return fmt.Errorf("timeline requires at least one record")
	}
	firstNS := records[0].MonotonicNS
	lastNS := records[len(records)-1].MonotonicNS
	durationNS := lastNS - firstNS
	if durationNS == 0 {
		durationNS = 1
	}
	points := make([]point, 0, len(records))
	for _, record := range records {
		message, err := openaiwire.Decode(record.Message)
		if err != nil {
			return fmt.Errorf("trace %s: %w", record.TraceID, err)
		}
		x := 70 + float64(record.MonotonicNS-firstNS)/float64(durationNS)*1080
		y, color := 82, "#2563eb"
		if record.Direction == openaiwire.DirectionServer {
			y, color = 178, "#dc2626"
		}
		points = append(points, point{
			X: x, Y: y, Color: color, Type: string(message.Type()),
			TimeMS:    float64(record.MonotonicNS) / 1_000_000,
			Direction: string(record.Direction), TraceID: record.TraceID,
			Label: message.Type() != openaiwire.EventInputAudioBufferAppend,
		})
	}
	if strings.TrimSpace(title) == "" {
		title = "OpenRealtime trace"
	}
	return pageTemplate.Execute(output, page{
		Title: title, DurationMS: float64(lastNS-firstNS) / 1_000_000, Points: points,
	})
}

var pageTemplate = template.Must(template.New("timeline").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light;font-family:ui-sans-serif,system-ui,sans-serif;color:#172033;background:#f7f8fb}
body{max-width:1240px;margin:0 auto;padding:32px 24px}h1{margin:0 0 4px;font-size:28px}p{color:#526078}
.card{background:white;border:1px solid #dce1ea;border-radius:12px;padding:18px;box-shadow:0 4px 18px #1720330a;overflow:auto}
svg{min-width:1160px;width:100%;height:260px}.axis{stroke:#aab3c2;stroke-width:1}.lane{font-weight:650;fill:#344158}.tick{font-size:11px;fill:#667085}.label{font-size:10px;fill:#172033}
table{border-collapse:collapse;width:100%;font-size:13px;margin-top:20px}th,td{text-align:left;padding:7px 9px;border-bottom:1px solid #e8ebf0}th{color:#526078}
.client{color:#2563eb}.server{color:#dc2626}code{font-size:12px}
</style>
</head>
<body>
<h1>{{.Title}}</h1>
<p>Duration: {{printf "%.3f" .DurationMS}} ms. Blue is client → server; red is server → client.</p>
<div class="card">
<svg viewBox="0 0 1200 260" role="img" aria-label="Realtime event timeline">
<text class="lane" x="4" y="87">client</text><text class="lane" x="4" y="183">server</text>
<line class="axis" x1="70" y1="82" x2="1150" y2="82"/><line class="axis" x1="70" y1="178" x2="1150" y2="178"/>
{{range .Points}}<circle cx="{{printf "%.2f" .X}}" cy="{{.Y}}" r="4" fill="{{.Color}}"><title>{{.Type}} at {{printf "%.3f" .TimeMS}} ms</title></circle>{{if .Label}}<text class="label" x="{{printf "%.2f" .X}}" y="{{if eq .Direction "client"}}68{{else}}198{{end}}" transform="rotate(-35 {{printf "%.2f" .X}} {{if eq .Direction "client"}}68{{else}}198{{end}})">{{.Type}}</text>{{end}}{{end}}
</svg>
<table><thead><tr><th>time (ms)</th><th>direction</th><th>event</th><th>trace ID</th></tr></thead><tbody>
{{range .Points}}<tr><td>{{printf "%.3f" .TimeMS}}</td><td class="{{.Direction}}">{{.Direction}}</td><td><code>{{.Type}}</code></td><td><code>{{.TraceID}}</code></td></tr>{{end}}
</tbody></table>
</div>
</body>
</html>
`))
