package report

import (
	"fmt"
	"html/template"
	"io"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/compare"
)

// comparePage is the view model for the comparison report. Like the others every
// field is pre-formatted, so the template holds no formatting logic.
type comparePage struct {
	Version   string
	Title     string
	Baseline  string
	Candidate string
	RanAt     time.Time
	Tolerance string

	Verdict string
	Passed  bool

	Regressions []string
	Notes       []string

	Capacity  []htmlChange
	Latency   []htmlChange
	Errors    []htmlChange
	Endpoints []htmlEndpointChange
}

// htmlChange is one row of a before/after table.
type htmlChange struct {
	Metric    string
	Baseline  string
	Candidate string
	Change    string
	// Mark is "REGRESSION", "worse" or "" -- the trailing word on the row.
	Mark       string
	Regression bool
}

// htmlEndpointChange is one row of the per-request comparison.
type htmlEndpointChange struct {
	Name       string
	Baseline   string
	Candidate  string
	Change     string
	Mark       string
	Regression bool
}

// RenderCompareHTML writes a self-contained HTML comparison report.
func RenderCompareHTML(w io.Writer, rep *compare.Report, version string, ranAt time.Time) error {
	page := comparePage{
		Version:   version,
		Title:     "regression comparison",
		Baseline:  rep.Before.Path,
		Candidate: rep.After.Path,
		RanAt:     ranAt,
		Tolerance: fmt.Sprintf("latency %s, capacity %s, errors %s",
			pctTrimmed(rep.Config.Latency), pctTrimmed(rep.Config.Capacity), pctTrimmed(rep.Config.Errors)),
		Verdict:     string(rep.Verdict),
		Passed:      !rep.Regressed(),
		Regressions: rep.Regressions,
		Notes:       rep.Notes,
	}

	if rep.Capacity != nil {
		page.Capacity = changeRows([]compare.Change{*rep.Capacity}, "iterations/s")
	}
	page.Latency = changeRows(rep.Latency, "ms")
	page.Errors = changeRows([]compare.Change{rep.Errors}, "ratio")
	page.Endpoints = endpointChangeRows(rep.Endpoints)

	if err := compareTemplate.Execute(w, page); err != nil {
		return fmt.Errorf("render compare html: %w", err)
	}
	return nil
}

func changeRows(changes []compare.Change, unit string) []htmlChange {
	out := make([]htmlChange, 0, len(changes))
	for _, c := range changes {
		out = append(out, htmlChange{
			Metric:     c.Metric,
			Baseline:   formatChangeValue(c.Before, unit),
			Candidate:  formatChangeValue(c.After, unit),
			Change:     formatChange(c),
			Mark:       changeMark(c),
			Regression: c.Regression,
		})
	}
	return out
}

func endpointChangeRows(changes []compare.EndpointChange) []htmlEndpointChange {
	out := make([]htmlEndpointChange, 0, len(changes))
	for _, e := range changes {
		row := htmlEndpointChange{Name: e.Name, Baseline: "-", Candidate: "-", Change: "-"}
		switch {
		case e.Added:
			row.Mark = "added"
		case e.Removed:
			row.Mark = "removed"
		default:
			row.Baseline = ms(e.Before)
			row.Candidate = ms(e.After)
			row.Change = formatSignedPercent(e.Percent, e.PercentMeaningful)
			row.Regression = e.Regression
			if e.Regression {
				row.Mark = "REGRESSION"
			} else if e.Worse {
				row.Mark = "worse"
			}
		}
		out = append(out, row)
	}
	return out
}

// changeMark is the plain-text version of the terminal's trailing word.
func changeMark(c compare.Change) string {
	switch {
	case c.Regression:
		return "REGRESSION"
	case c.Worse:
		return "worse"
	default:
		return ""
	}
}

// compareTemplate is the comparison report. It reuses the styling of the
// measurement report, since the two are read side by side.
//
// Both templates are parsed in one call so the shared table definition is
// available to the body that invokes it.
var compareTemplate = template.Must(
	template.New("compare").Parse(htmlHead + compareBody + changeTable))

// changeTable is the before/after table every metric block uses.
const changeTable = `{{define "changetable"}}<table>
<thead><tr><th>Metric</th><th class="num">Baseline</th><th class="num">Candidate</th><th class="num">Change</th><th></th></tr></thead>
<tbody>
{{range .}}  <tr{{if .Regression}} class="worst"{{end}}><td>{{.Metric}}</td><td class="num">{{.Baseline}}</td><td class="num">{{.Candidate}}</td><td class="num">{{.Change}}</td><td class="{{if .Regression}}fail{{else}}note{{end}}">{{.Mark}}</td></tr>
{{end}}</tbody>
</table>
{{end}}`

const compareBody = `<main>
<header>
  <h1>Regression comparison</h1>
  <div class="sub">{{.Baseline}} → {{.Candidate}}</div>
  <div class="verdict {{if .Passed}}ok{{else}}bad{{end}}">{{.Verdict}}</div>
</header>

<dl>
  <dt>Tolerance</dt><dd>{{.Tolerance}}</dd>
</dl>

{{if .Regressions}}
<div class="warn"><strong>Regressions</strong> — these moved past their tolerance: {{range $i, $r := .Regressions}}{{if $i}}, {{end}}{{$r}}{{end}}</div>
{{end}}

{{if .Capacity}}
<h2>Capacity</h2>
{{template "changetable" .Capacity}}
{{end}}

<h2>Latency</h2>
{{if .Latency}}{{template "changetable" .Latency}}{{else}}<p class="note">Nothing comparable was recorded by both runs.</p>{{end}}

<h2>Errors</h2>
{{template "changetable" .Errors}}

{{if .Endpoints}}
<h2>Requests</h2>
<table>
<thead><tr><th>Request</th><th class="num">Baseline p95</th><th class="num">Candidate p95</th><th class="num">Change</th><th></th></tr></thead>
<tbody>
{{range .Endpoints}}  <tr{{if .Regression}} class="worst"{{end}}><td>{{.Name}}</td><td class="num">{{.Baseline}}</td><td class="num">{{.Candidate}}</td><td class="num">{{.Change}}</td><td class="{{if .Regression}}fail{{else}}note{{end}}">{{.Mark}}</td></tr>
{{end}}</tbody>
</table>
{{end}}

{{if .Notes}}
<ul class="notes">
{{range .Notes}}  <li>{{.}}</li>
{{end}}</ul>
{{end}}

<p class="note">A single run against a single run is a noisy measurement. Narrow the tolerance only once you know your noise floor.</p>

<footer>
  autobench v{{.Version}} · generated {{.RanAt.Format "2006-01-02 15:04:05"}}
</footer>
</main>
</body>
</html>
`
