package report

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/callum/k6-autobench/internal/collect"
	"github.com/callum/k6-autobench/internal/config"
)

// systemRows renders the collector readings as report rows: min, max, average
// and when the peak happened, in the order the queries were configured.
//
// The peak offset is the column that earns its place. The windowed latency table
// says a spike began at 35s; this says the database hit 97% CPU at 35s, which is
// the difference between noticing and diagnosing (PLAN-OUTLINE.md §33).
func systemRows(r collect.Report) []systemRow {
	rows := make([]systemRow, 0, len(r.Series))
	for _, s := range r.Series {
		row := systemRow{Series: seriesLabel(s)}
		if st, ok := s.Summary(); ok {
			row.Summarised = true
			row.Min = seriesValue(st.Min, s.Unit)
			row.Max = seriesValue(st.Max, s.Unit)
			row.Avg = seriesValue(st.Avg, s.Unit)
			row.PeakAt = peakAt(st.MaxAtMs)
		}
		rows = append(rows, row)
	}
	return rows
}

// systemRow is one series' summary, formatted for a report.
type systemRow struct {
	Series string
	Min    string
	Max    string
	Avg    string
	PeakAt string
	// Summarised is false for a series whose readings were all NaN or Inf, which
	// has nothing to summarise; the report says so rather than printing zeros.
	Summarised bool
}

// systemNote describes how often the readings were taken, so a reader can tell
// whether a spike could have fallen between two samples.
func systemNote(cfg *config.Config) string {
	if cfg == nil || !cfg.Collectors.Enabled() {
		return ""
	}
	return "sampled every " + cfg.Collectors.Interval.String()
}

// systemIssues flattens the collectors' own failures into report lines.
func systemIssues(r collect.Report) []string {
	out := make([]string, 0, len(r.Failures))
	for _, f := range r.Failures {
		out = append(out, f.Source+": "+f.Error)
	}
	return out
}

// seriesValue renders a collector reading using the unit the configuration gave
// it. Prometheus values are unitless numbers, so only the configuration can say
// what one means.
func seriesValue(v float64, unit string) string {
	switch classifyUnit(unit) {
	case unitBytes:
		return byteCount(v)
	case unitPercent:
		return strconv.FormatFloat(v, 'f', 1, 64) + "%"
	default:
		return number(v)
	}
}

// seriesLabel names a series, attaching the configured unit when the rendered
// value does not already carry it. Keeping units out of the value columns is
// what keeps a table of mixed units aligned: "498 connections" is wider than the
// column, "498" is not.
func seriesLabel(s collect.Series) string {
	if classifyUnit(s.Unit) == unitPlain {
		if unit := strings.TrimSpace(s.Unit); unit != "" {
			return s.Label() + " (" + unit + ")"
		}
	}
	return s.Label()
}

// unitKind classifies a configured unit. Prometheus values are unitless, so this
// is the only thing that can say whether 480000000 is bytes or a request count.
type unitKind int

const (
	// unitPlain is a bare number whose unit the series name carries.
	unitPlain unitKind = iota
	// unitBytes is rendered in binary units, which is how memory and traffic are
	// actually read: 480000000 is not a number anyone reasons about.
	unitBytes
	// unitPercent is rendered with a percent sign.
	unitPercent
)

func classifyUnit(unit string) unitKind {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "bytes", "byte", "b":
		return unitBytes
	case "percent", "percentage", "%":
		return unitPercent
	default:
		return unitPlain
	}
}

// number renders a unitless reading. Prometheus values span a wide range -- a
// per-second rate of 0.0035, a counter of 9,000,000 -- so four significant
// digits, with thousands separators once the magnitude warrants them.
func number(v float64) string {
	switch abs := math.Abs(v); {
	case abs == 0:
		return "0"
	case abs >= 1000:
		return commafyFloat(v, 1)
	default:
		return strconv.FormatFloat(v, 'g', 4, 64)
	}
}

// byteCount renders a byte count in binary units.
func byteCount(v float64) string {
	abs := math.Abs(v)
	if abs < 1024 {
		return strconv.FormatFloat(v, 'f', 0, 64) + "B"
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := -1
	for abs >= 1024 && i < len(units)-1 {
		abs /= 1024
		v /= 1024
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + units[i]
}

// peakAt renders when a series peaked, on the measurement's own timeline. It is
// deliberately the same timeline the window labels use, so a resource peak can
// be read against the latency window it landed in.
func peakAt(offsetMs float64) string {
	d := time.Duration(offsetMs * float64(time.Millisecond))
	if d < time.Second {
		return ms(offsetMs)
	}
	return d.Round(time.Second).String()
}
