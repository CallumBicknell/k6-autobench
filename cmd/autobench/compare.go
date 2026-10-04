package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/callum/k6-autobench/internal/compare"
	"github.com/callum/k6-autobench/internal/report"
)

// compareReport is the machine-readable form of a comparison, written by --json.
// It wraps the comparison with the files it read, so a CI job can archive one
// artefact and still know what it was comparing.
type compareReport struct {
	Baseline  string          `json:"baseline"`
	Candidate string          `json:"candidate"`
	RanAt     time.Time       `json:"ran_at"`
	Verdict   string          `json:"verdict"`
	Regressed bool            `json:"regressed"`
	Tolerance compare.Config  `json:"tolerance"`
	Result    *compare.Report `json:"result"`
}

func compareCommand(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		latencyTolerance  float64
		capacityTolerance float64
		errorTolerance    float64
		jsonOut           string
		htmlOut           string
		noColor           bool
	)
	fs.Float64Var(&latencyTolerance, "tolerance-latency", compare.DefaultLatencyTolerance,
		"how much worse a latency percentile may get before it fails, as a fraction (0.10 is 10%)")
	fs.Float64Var(&capacityTolerance, "tolerance-capacity", compare.DefaultCapacityTolerance,
		"how much capacity may drop before it fails, as a fraction")
	fs.Float64Var(&errorTolerance, "tolerance-errors", compare.DefaultErrorTolerance,
		"how much the error rate may rise before it fails, in absolute terms (0.001 is 0.1pp)")
	fs.StringVar(&jsonOut, "json", "", "write the comparison as JSON to this path")
	fs.StringVar(&htmlOut, "html", "", "write a self-contained HTML report to this path")
	fs.BoolVar(&noColor, "no-color", false, "disable coloured output")
	fs.Usage = func() { compareUsage(fs) }

	positional, code := compareArgs(fs, args)
	if code != exitPass {
		return code
	}

	cfg := compare.Config{
		Latency:  latencyTolerance,
		Capacity: capacityTolerance,
		Errors:   errorTolerance,
	}
	if err := validateTolerances(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
		return exitError
	}

	before, err := compare.Load(positional[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "autobench: baseline: "+err.Error())
		return exitError
	}
	after, err := compare.Load(positional[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "autobench: candidate: "+err.Error())
		return exitError
	}

	rep := compare.Compare(before, after, cfg)

	color := !noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	report.RenderCompare(os.Stdout, rep, version, color)

	ranAt := time.Now()
	if jsonOut != "" {
		out := compareReport{
			Baseline: positional[0], Candidate: positional[1], RanAt: ranAt,
			Verdict: string(rep.Verdict), Regressed: rep.Regressed(),
			Tolerance: cfg, Result: rep,
		}
		if err := writeJSONFile(jsonOut, out); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}
	if htmlOut != "" {
		if err := writeHTMLFile(htmlOut, func(w io.Writer) error {
			return report.RenderCompareHTML(w, rep, version, ranAt)
		}); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}

	if rep.Regressed() {
		return exitFail
	}
	return exitPass
}

// compareArgs reads the two result paths, allowing them either side of the
// flags.
//
// Go's flag package stops at the first positional, so the loop takes one path
// and re-parses the rest. Anything after a path may therefore be a flag, which
// is what makes `compare a.json --html out.html b.json` work.
func compareArgs(fs *flag.FlagSet, args []string) ([]string, int) {
	var positional []string
	remaining := args
	for {
		if err := fs.Parse(remaining); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, exitPass
			}
			return nil, exitError
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Args()[0])
		remaining = fs.Args()[1:]
	}

	if len(positional) != 2 {
		fmt.Fprintf(os.Stderr,
			"autobench: compare needs exactly two result files, got %d\n\n", len(positional))
		compareUsage(fs)
		return nil, exitError
	}
	return positional, exitPass
}

// validateTolerances rejects a tolerance that could not mean anything.
func validateTolerances(cfg compare.Config) error {
	for _, t := range []struct {
		name  string
		value float64
	}{
		{"--tolerance-latency", cfg.Latency},
		{"--tolerance-capacity", cfg.Capacity},
		{"--tolerance-errors", cfg.Errors},
	} {
		if t.value < 0 {
			return fmt.Errorf("%s must not be negative", t.name)
		}
	}
	return nil
}

func compareUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `Usage:
  autobench compare [flags] baseline.json candidate.json

Compares two recorded runs and fails when the candidate regressed beyond the
configured tolerance. This is the gate a pipeline needs: a capacity search says
how much a system can take, and only a comparison says whether a change made it
worse.

Both files come from --json on either command:

  autobench capacity bench.yaml --json baseline.json   # on main
  autobench capacity bench.yaml --json candidate.json  # on the PR
  autobench compare baseline.json candidate.json

The two must be the same kind of run. Latency is compared percentile by
percentile, errors in absolute terms, and capacity only between two searches.

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(fs.Output(), `
Examples:
  autobench compare baseline.json candidate.json
  autobench compare baseline.json candidate.json --tolerance-latency 0.05
  autobench compare baseline.json candidate.json --html compare.html --json compare.json

Exit codes:
  0  nothing regressed beyond the tolerance
  1  a regression was found
  2  usage or runtime error
`)
}
