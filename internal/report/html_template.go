package report

import "html/template"

// htmlTemplate is the whole report. It is a single self-contained file: styles
// are inline, the chart is inline SVG, and there is no JavaScript, so the report
// renders identically offline and can be archived as a CI artefact.
var htmlTemplate = template.Must(template.New("report").Parse(htmlHead + htmlBody))

const htmlHead = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — autobench</title>
<style>
:root{--fg:#111827;--muted:#6b7280;--line:#e5e7eb;--bg:#fff;--card:#f9fafb;--ok:#047857;--bad:#b91c1c;--warn:#b45309;--accent:#1d4ed8}
@media (prefers-color-scheme:dark){:root{--fg:#e5e7eb;--muted:#9ca3af;--line:#374151;--bg:#111827;--card:#1f2937;--ok:#34d399;--bad:#f87171;--warn:#fbbf24;--accent:#93c5fd}}
*{box-sizing:border-box}
body{margin:0;padding:2rem 1rem;background:var(--bg);color:var(--fg);font:15px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:820px;margin:0 auto}
header{border-bottom:1px solid var(--line);padding-bottom:1rem;margin-bottom:1.5rem}
h1{margin:0;font-size:1.4rem}
h2{font-size:.8rem;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);margin:2rem 0 .75rem;font-weight:600}
h3{font-size:.8rem;letter-spacing:.04em;color:var(--muted);margin:1.5rem 0 .5rem;font-weight:600}
.sub{color:var(--muted);margin-top:.35rem;font-size:.9rem}
.verdict{display:inline-block;margin-top:1rem;padding:.4rem .8rem;border-radius:.4rem;font-weight:700;letter-spacing:.03em;font-size:.9rem}
.verdict.ok{color:var(--ok);border:1px solid var(--ok)}
.verdict.bad{color:var(--bad);border:1px solid var(--bad)}
.headline{margin:1.5rem 0;padding:1.25rem 1.5rem;border:1px solid var(--line);border-radius:.6rem;background:var(--card)}
.headline .figure{font-size:2.4rem;font-weight:700;letter-spacing:-.02em}
.headline .caption{color:var(--muted);margin-top:.25rem}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.4rem 1.25rem;margin:0}
dt{color:var(--muted)}
dd{margin:0}
table{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}
th,td{text-align:left;padding:.45rem .6rem;border-bottom:1px solid var(--line);white-space:nowrap}
th{color:var(--muted);font-size:.78rem;letter-spacing:.04em;text-transform:uppercase;font-weight:600}
td.num,th.num{text-align:right}
tr.worst td{background:color-mix(in srgb,var(--warn) 12%,transparent)}
.pass{color:var(--ok)}
.fail{color:var(--bad)}
.note{color:var(--muted);font-size:.85rem}
ul.notes{margin:.5rem 0 0;padding-left:1.2rem;color:var(--warn)}
.warn{border-left:3px solid var(--warn);padding:.5rem .75rem;background:var(--card);margin:1rem 0}
.chart{width:100%;height:auto;display:block}
.chart .plot{fill:none;stroke:var(--line)}
.chart .grid{stroke:var(--line);stroke-width:1;opacity:.6}
.chart .series{fill:none;stroke:var(--accent);stroke-width:2}
.chart .limit{stroke:var(--warn);stroke-width:1.5;stroke-dasharray:5 4}
.chart .marker{stroke:var(--ok);stroke-width:1.5;stroke-dasharray:3 3}
.chart .axis{fill:var(--muted);font-size:11px;font-family:inherit}
.chart .axis.y{text-anchor:end}
.chart .axis.x{text-anchor:middle}
.chart .axis.title{text-anchor:middle}
.chart circle.pass{fill:var(--ok)}
.chart circle.fail{fill:var(--bad)}
footer{margin-top:3rem;padding-top:1rem;border-top:1px solid var(--line);color:var(--muted);font-size:.85rem}
</style>
</head>
<body>
`

const htmlBody = `<main>
<header>
  <h1>{{.Title}}</h1>
  <div class="sub">{{.Kind}} · {{.Target}} · {{.Workload}}</div>
  <div class="verdict {{if .Passed}}ok{{else}}bad{{end}}">{{.Verdict}}</div>
</header>

{{if .Headline}}
<div class="headline">
  <div class="figure">{{.Headline}}</div>
  <div class="caption">{{.Subline}}</div>
</div>
{{end}}

<h2>Run</h2>
<dl>
{{range .Facts}}  <dt>{{.Label}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>

{{if .Chart}}
<h2>Rate vs latency</h2>
{{.Chart}}
{{end}}

{{if .Traffic}}
<h2>Traffic</h2>
<dl>
{{range .Traffic}}  <dt>{{.Label}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>
{{end}}

{{if .Latency}}
<h2>Latency</h2>
<dl>
{{range .Latency}}  <dt>{{.Label}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>
{{end}}

{{if .Status}}
<h2>Responses</h2>
<dl>
{{range .Status}}  <dt>{{.Label}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>
{{end}}

{{if .IterationLatency}}
<h2>Journey latency <span class="note">end to end, all steps</span></h2>
<dl>
{{range .IterationLatency}}  <dt>{{.Label}}</dt><dd>{{.Value}}</dd>
{{end}}</dl>
{{end}}

{{if .Endpoints}}
<h2>Endpoints</h2>
<table>
<thead><tr><th>Request</th><th class="num">Requests</th><th class="num">Rate/s</th><th class="num">Errors</th><th class="num">avg</th><th class="num">p95</th><th class="num">p99</th><th class="num">max</th></tr></thead>
<tbody>
{{range .Endpoints}}  <tr><td>{{.Name}}</td><td class="num">{{.Requests}}</td><td class="num">{{.Rate}}</td><td class="num">{{.Errors}}</td><td class="num">{{.Avg}}</td><td class="num">{{.P95}}</td><td class="num">{{.P99}}</td><td class="num">{{.Max}}</td></tr>
{{end}}</tbody>
</table>
{{end}}

{{if .Windows}}
<h2>Latency over time</h2>
{{if .WindowNote}}<p class="note">{{.WindowNote}}</p>{{end}}
{{.WindowChart}}
<table>
<thead><tr><th>Window</th><th class="num">Requests</th><th class="num">Errors</th><th class="num">p95</th><th class="num">p99</th><th class="num">max</th></tr></thead>
<tbody>
{{range .Windows}}  <tr{{if .Worst}} class="worst"{{end}}><td>{{.Label}}{{if .Worst}} <span class="note">worst</span>{{end}}</td><td class="num">{{.Requests}}</td><td class="num">{{.Errors}}</td><td class="num">{{.P95}}</td><td class="num">{{.P99}}</td><td class="num">{{.Max}}</td></tr>
{{end}}</tbody>
</table>
{{if .Drift}}
<h3>Endpoint drift</h3>
<table>
<thead><tr><th>Request</th><th class="num">Run p95</th><th>Worst window</th><th class="num">p95</th><th class="num">Ratio</th></tr></thead>
<tbody>
{{range .Drift}}  <tr{{if .Flag}} class="worst"{{end}}><td>{{.Name}}</td><td class="num">{{.RunP95}}</td><td>{{.Window}}</td><td class="num">{{.Worst}}</td><td class="num">{{.Ratio}}</td></tr>
{{end}}</tbody>
</table>
{{end}}
{{end}}

{{if .Probes}}
<h2>Probes</h2>
<table>
<thead><tr><th class="num">#</th><th>Phase</th><th class="num">Rate</th><th class="num">p95</th><th class="num">p99</th><th class="num">Achieved</th><th>Verdict</th></tr></thead>
<tbody>
{{range .Probes}}  <tr><td class="num">{{.Index}}</td><td>{{.Phase}}</td><td class="num">{{.Rate}}</td><td class="num">{{.P95}}</td><td class="num">{{.P99}}</td><td class="num">{{.Achieved}}</td><td class="{{if .Passed}}pass{{else}}fail{{end}}">{{.Verdict}}</td></tr>
{{if .Note}}  <tr><td></td><td colspan="6" class="note">{{.Note}}</td></tr>{{end}}
{{range .Failures}}  <tr><td></td><td colspan="6" class="fail">{{.Metric}} {{.Observed}} &gt; {{.Limit}} ({{.Unit}})</td></tr>
{{end}}{{end}}</tbody>
</table>
{{end}}

{{if .Failures}}
<h2>Threshold breaches</h2>
<ul class="notes">
{{range .Failures}}  <li>{{.Metric}}: observed {{.Observed}} {{.Unit}}, limit {{.Limit}} {{.Unit}}</li>
{{end}}</ul>
{{end}}

{{if .StatusHint}}
<div class="warn">{{.StatusHint}}</div>
{{end}}

{{if .Generator}}
<div class="warn"><strong>Generator saturation</strong> — requested {{.Generator.TargetRate}}/s, achieved {{.Generator.AchievedRate}}/s{{if .Generator.Reason}} ({{.Generator.Reason}}){{end}}. This probe says nothing about the target; raise <code>load_generator.max_vus</code>.</div>
{{end}}

{{if .Notes}}
<ul class="notes">
{{range .Notes}}  <li>{{.}}</li>
{{end}}</ul>
{{end}}

<footer>
  autobench v{{.Version}} · generated {{.RanAt.Format "2006-01-02 15:04:05"}} · ran {{.Elapsed}}
</footer>
</main>
</body>
</html>
`
