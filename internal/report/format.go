package report

import (
	"regexp"
	"strconv"
	"strings"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleLen returns the printable width of s, ignoring ANSI escape sequences.
func visibleLen(s string) int {
	return len([]rune(ansiRE.ReplaceAllString(s, "")))
}

// commafy groups an integer with thousands separators.
func commafy(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// commafyFloat formats f with dp decimals and thousands separators.
func commafyFloat(f float64, dp int) string {
	s := strconv.FormatFloat(f, 'f', dp, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return s
	}
	out := commafy(n)
	if frac != "" {
		out += "." + frac
	}
	return out
}

// ms formats a millisecond value with one decimal, e.g. "42.8ms".
func ms(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) + "ms" }

// duration renders a millisecond magnitude with the most readable unit, so a
// sub-millisecond limit does not collapse to "0.0ms". Used for threshold limits.
func duration(f float64) string {
	switch {
	case f >= 1000:
		return strconv.FormatFloat(f/1000, 'f', 2, 64) + "s"
	case f < 1:
		return strconv.FormatFloat(f*1000, 'f', 1, 64) + "us"
	default:
		return strconv.FormatFloat(f, 'f', 1, 64) + "ms"
	}
}

// pct formats a ratio as a percentage with three decimals, e.g. "0.012%".
func pct(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 3, 64) + "%"
}

// pctTrimmed renders a ratio as a percentage without trailing zeros, which is how
// a tolerance reads best: "10%", not "10.000%".
func pctTrimmed(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', -1, 64) + "%"
}

// formatP renders a percentile label, e.g. 99.9 -> "p99.9".
func formatP(p float64) string { return "p" + strconv.FormatFloat(p, 'f', -1, 64) }

// truncate shortens s to at most n runes, marking the cut with an ellipsis.
// Endpoint names come from user config and can be arbitrarily long; the report
// columns are fixed width.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
