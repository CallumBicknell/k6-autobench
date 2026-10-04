// Command autobench is an adaptive load-testing orchestrator around k6.
//
// V0.1 provides a single fixed-rate run: it drives k6 at a constant arrival
// rate, parses the machine-readable summary, evaluates configured SLO
// thresholds and renders a terminal report.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/report"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/runner"
)

// version is the release this build is. It is a variable so a release build can
// set it from the tag; a plain `go build` leaves it at the constant the README
// documents, and the version guard checks that constant.
var version = "1.0.0"

// commit and builtAt are recorded by a release build through -ldflags. They are
// empty in a plain `go build`, and `autobench version` says so rather than
// inventing a build identity it does not have.
var (
	commit  = ""
	builtAt = ""
)

// versionLine is what `autobench version` prints: the release, and which build of
// it when one was recorded.
func versionLine() string {
	out := "autobench " + version
	switch {
	case commit != "" && builtAt != "":
		out += " (" + commit + ", built " + builtAt + ")"
	case commit != "":
		out += " (" + commit + ")"
	case builtAt != "":
		out += " (built " + builtAt + ")"
	}
	return out
}

// Exit codes.
const (
	exitPass   = 0
	exitFail   = 1
	exitError  = 2
	exitSigint = 130
)

// exitHelp is what a command returns after printing its own usage. It is not an
// exit code: run translates it to a success, because asking for help is not a
// failure.
//
// It exists because "printed the usage" and "did the work" are different
// outcomes that both have nothing to report, and conflating them either crashed
// (compare indexed an empty argument list) or produced a spurious error after
// the usage (run and capacity reported a missing config).
const exitHelp = -1

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitError
	}
	code := dispatch(args)
	if code == exitHelp {
		return exitPass
	}
	return code
}

// dispatch routes a command to its handler.
func dispatch(args []string) int {
	switch args[0] {
	case "run", "fixed":
		return runCommand(args[1:])
	case "capacity", "search":
		return capacityCommand(args[1:])
	case "compare":
		return compareCommand(args[1:])
	case "version", "--version", "-v":
		fmt.Println(versionLine())
		return exitPass
	case "help", "--help", "-h":
		usage(os.Stdout)
		return exitPass
	default:
		if strings.HasSuffix(args[0], ".yaml") || strings.HasSuffix(args[0], ".yml") {
			return runCommand(args)
		}
		fmt.Fprintf(os.Stderr, "autobench: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return exitError
	}
}

// overrides carries the flag values that may replace configuration fields. The
// caller passes the set of flags the user actually typed, so an unset flag
// always leaves the config file's value alone.
type overrides struct {
	configPath string
	target     string
	timeUnit   string
	vus        int
	maxVUs     int

	// Fixed-rate run.
	rate      int
	duration  string
	k6Timeout string

	// Capacity search.
	startRate      int
	increaseFactor float64
	resolution     int
	probeDuration  string
	warmup         string
	confirmRuns    int
	failConfirm    int
	maxRate        int
	maxProbes      int
	maxVUGrowth    int

	// Windowed analysis.
	analysisWindow string
	allWindowsPass bool

	// Validation.
	validationDur  string
	validationRate string
	safetyMargin   float64
}

// registerAnalysisFlags adds the windowed-analysis overrides, which both
// commands accept because windowing applies to every measurement.
func registerAnalysisFlags(fs *flag.FlagSet, ov *overrides) {
	fs.StringVar(&ov.analysisWindow, "analysis-window", "",
		"slice every measurement into windows of this width, e.g. 10s (overrides config)")
	fs.BoolVar(&ov.allWindowsPass, "require-all-windows-pass", false,
		"fail a measurement when any single window breaches a threshold (overrides config)")
}

func runCommand(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		ov         overrides
		k6Path     string
		scriptPath string
		jsonOut    string
		htmlOut    string
		outDir     string
		noColor    bool
	)
	fs.StringVar(&ov.configPath, "config", "", "path to a YAML config file")
	fs.StringVar(&ov.configPath, "c", "", "path to a YAML config file (shorthand)")
	fs.StringVar(&ov.target, "target", "", "target base URL (overrides config)")
	fs.IntVar(&ov.rate, "rate", 0, "iteration rate (overrides config run.rate)")
	fs.StringVar(&ov.duration, "duration", "", "run duration, e.g. 30s or 2m (overrides config)")
	fs.StringVar(&ov.k6Timeout, "k6-timeout", "", "kill k6 if it runs longer than this (default: the duration plus a grace period)")
	fs.IntVar(&ov.vus, "vus", 0, "pre-allocated VUs (overrides config)")
	fs.IntVar(&ov.maxVUs, "max-vus", 0, "maximum VUs (overrides config)")
	fs.StringVar(&ov.timeUnit, "time-unit", "", "arrival-rate time unit, e.g. 1s (overrides config)")
	fs.StringVar(&k6Path, "k6", "", "path to the k6 binary (default: $AUTOBENCH_K6 or \"k6\")")
	fs.StringVar(&scriptPath, "script", "", "override the embedded k6 workload script")
	fs.StringVar(&jsonOut, "json", "", "write the full result as JSON to this path")
	fs.StringVar(&htmlOut, "html", "", "write a self-contained HTML report to this path")
	fs.StringVar(&outDir, "out-dir", "", "write the config, result, summary and HTML report to this directory")
	fs.BoolVar(&noColor, "no-color", false, "disable coloured output")
	registerAnalysisFlags(fs, &ov)
	fs.Usage = func() { runUsage(fs) }

	if code := parseFlags(fs, args, &ov.configPath); code != exitPass {
		return code
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, code := buildConfig(ov.configPath, config.ModeFixed, set, ov)
	if cfg == nil {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "autobench: %d iterations/%s for %s against %s\n",
		cfg.Run.Rate, cfg.Run.TimeUnit, cfg.Run.Duration, cfg.Target.BaseURL)
	if notice := collectorNotice(cfg); notice != "" {
		fmt.Fprintf(os.Stderr, "autobench: sampling %s\n", notice)
	}
	if notice := windowNotice(cfg, cfg.Run.Rate, cfg.Run.Duration.Duration, 1); notice != "" {
		fmt.Fprintf(os.Stderr, "autobench: %s\n", notice)
	}

	r := &runner.Runner{K6Path: k6Path, ScriptPath: scriptPath}
	spec := runner.Spec{
		BaseURL:         cfg.Target.BaseURL,
		Workload:        cfg.Workload,
		Rate:            cfg.Run.Rate,
		TimeUnit:        cfg.Run.TimeUnit,
		Duration:        cfg.Run.Duration.Duration,
		PreAllocatedVUs: cfg.LoadGenerator.PreAllocatedVUs,
		MaxVUs:          cfg.LoadGenerator.MaxVUs,
		Percentiles:     cfg.Percentiles,
		Window:          cfg.Analysis.Window.Duration,
		Timeout:         cfg.Run.K6Timeout.Duration,
		InsecureTLS:     cfg.Target.TLS.Insecure,
		CAFile:          cfg.Target.TLS.CAFile,
		ClientCertFile:  cfg.Target.TLS.CertFile,
		ClientKeyFile:   cfg.Target.TLS.KeyFile,
		RequestTimeout:  cfg.Target.RequestTimeout.Duration,
		MaxRedirects:    cfg.Target.MaxRedirects,
		Collectors:      buildCollectors(cfg),
	}

	started := time.Now()
	outcome, err := r.Run(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "autobench: interrupted")
			return exitSigint
		}
		fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
		return exitError
	}
	ended := time.Now()

	m := metrics.Extract(outcome.Summary, cfg.Percentiles)
	m.RateOverScenario(cfg.Run.Duration.Duration)
	verdict, failures, gen := results.Evaluate(cfg, m)
	windowFailures := results.WindowFailures(cfg, outcome.Windows)
	verdict = results.ApplyWindowPolicy(cfg, verdict, windowFailures)

	res := &results.Result{
		Name:           cfg.Name,
		Target:         cfg.Target.BaseURL,
		Rate:           cfg.Run.Rate,
		TimeUnit:       cfg.Run.TimeUnit,
		Duration:       cfg.Run.Duration,
		VUs:            results.VUPlan{PreAllocated: cfg.LoadGenerator.PreAllocatedVUs, Max: cfg.LoadGenerator.MaxVUs},
		StartedAt:      started,
		EndedAt:        ended,
		ElapsedMs:      float64(ended.Sub(started)) / float64(time.Millisecond),
		Metrics:        m,
		Verdict:        verdict,
		Failures:       failures,
		Generator:      gen,
		Raw:            outcome.Summary.Metrics,
		Windows:        outcome.Windows,
		WindowFailures: windowFailures,
		System:         outcome.System,
	}

	color := !noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	report.Render(os.Stdout, cfg, res, version, color)

	if jsonOut != "" {
		if err := writeJSONFile(jsonOut, res); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}
	if htmlOut != "" {
		if err := writeHTMLFile(htmlOut, func(w io.Writer) error {
			return report.RenderHTML(w, cfg, res, version)
		}); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}
	if outDir != "" {
		if err := writeRunArtifacts(outDir, ov.configPath, cfg, res); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}

	if verdict.OK() {
		return exitPass
	}
	return exitFail
}

// writeRunArtifacts persists a fixed run the way a search is persisted
// (PLAN-OUTLINE.md §26): the source config, the full result, a compact summary and
// the HTML report. There is no probes/ directory, because a fixed run takes one
// measurement.
func writeRunArtifacts(dir, configPath string, cfg *config.Config, res *results.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// A rerun into the same directory must not leave the previous one's files
	// beside a manifest that did not come from them.
	if err := clearStaleArtifacts(dir); err != nil {
		return fmt.Errorf("clear %s: %w", dir, err)
	}

	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", configPath, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), raw, 0o644); err != nil {
			return fmt.Errorf("write config.yaml: %w", err)
		}
	}

	if err := writeJSONFile(filepath.Join(dir, "manifest.json"), res); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "summary.json"), summariseRun(res)); err != nil {
		return err
	}
	return writeHTMLFile(filepath.Join(dir, "report.html"), func(w io.Writer) error {
		return report.RenderHTML(w, cfg, res, version)
	})
}

// runSummary is the compact, CI-friendly form of a fixed run.
type runSummary struct {
	Name           string               `json:"name"`
	Target         string               `json:"target"`
	Verdict        results.Verdict      `json:"verdict"`
	Rate           int                  `json:"rate"`
	TimeUnit       string               `json:"time_unit"`
	DurationMs     float64              `json:"duration_ms"`
	Requests       int64                `json:"requests"`
	FailedRequests int64                `json:"failed_requests"`
	ErrorRate      float64              `json:"error_rate"`
	AvgMs          float64              `json:"latency_avg_ms"`
	Percentiles    []metrics.Percentile `json:"latency_percentiles_ms,omitempty"`
	Failures       []results.Failure    `json:"failures,omitempty"`
	ElapsedMs      float64              `json:"elapsed_ms"`
}

func summariseRun(res *results.Result) runSummary {
	return runSummary{
		Name:           res.Name,
		Target:         res.Target,
		Verdict:        res.Verdict,
		Rate:           res.Rate,
		TimeUnit:       res.TimeUnit,
		DurationMs:     res.Duration.Duration.Seconds() * 1000,
		Requests:       res.Metrics.Requests,
		FailedRequests: res.Metrics.FailedRequests,
		ErrorRate:      res.Metrics.ErrorRate,
		AvgMs:          res.Metrics.AvgMs,
		Percentiles:    res.Metrics.Percentiles,
		Failures:       res.Failures,
		ElapsedMs:      res.ElapsedMs,
	}
}

// parseFlags parses args, allowing the config-path positional to appear before
// or after flags. Go's flag package stops parsing at the first positional, so
// any flags that follow it are re-parsed from the remainder.
func parseFlags(fs *flag.FlagSet, args []string, configPath *string) int {
	remaining := args
	firstPositional := true
	for {
		if err := fs.Parse(remaining); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitHelp
			}
			return exitError
		}
		if fs.NArg() == 0 {
			return exitPass
		}
		positional := fs.Args()
		if firstPositional && *configPath == "" {
			*configPath = positional[0]
			firstPositional = false
		} else {
			fmt.Fprintf(os.Stderr, "autobench: unexpected argument %q\n", positional[0])
			return exitError
		}
		remaining = positional[1:]
	}
}

// buildConfig loads an optional config file, applies flag overrides, then
// applies defaults and validates for the given mode. It returns a nil config
// and an exit code on failure.
func buildConfig(path string, mode config.Mode, set map[string]bool, ov overrides) (*config.Config, int) {
	var cfg *config.Config
	if path != "" {
		// Decode without validating: a flag may supply something the file omits
		// (e.g. --start-rate for a config with no search block).
		loaded, err := config.Decode(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return nil, exitError
		}
		cfg = loaded
	} else {
		cfg = &config.Config{}
	}

	if set["target"] {
		cfg.Target.BaseURL = ov.target
	}
	if set["time-unit"] {
		cfg.Run.TimeUnit = ov.timeUnit
	}
	if set["rate"] {
		cfg.Run.Rate = ov.rate
	}
	if set["vus"] {
		cfg.LoadGenerator.PreAllocatedVUs = ov.vus
		cfg.PinPreAllocatedVUs()
	}
	if set["max-vus"] {
		cfg.LoadGenerator.MaxVUs = ov.maxVUs
		cfg.PinMaxVUs()
	}

	durations := []struct {
		flag  string
		value string
		dst   *config.Duration
	}{
		{"duration", ov.duration, &cfg.Run.Duration},
		{"k6-timeout", ov.k6Timeout, &cfg.Run.K6Timeout},
		{"probe-duration", ov.probeDuration, &cfg.Search.ProbeDuration},
		{"warmup", ov.warmup, &cfg.Search.Warmup},
		{"validation-duration", ov.validationDur, &cfg.Validation.Duration},
		{"analysis-window", ov.analysisWindow, &cfg.Analysis.Window},
	}
	for _, d := range durations {
		if !set[d.flag] {
			continue
		}
		parsed, ok := parseDurationFlag("--"+d.flag, d.value)
		if !ok {
			return nil, exitError
		}
		d.dst.Duration = parsed
	}

	if set["require-all-windows-pass"] {
		cfg.Analysis.RequireAllWindowsPass = ov.allWindowsPass
	}

	if set["start-rate"] {
		cfg.Search.StartRate = ov.startRate
	}
	if set["increase-factor"] {
		cfg.Search.IncreaseFactor = ov.increaseFactor
	}
	if set["resolution"] {
		cfg.Search.Resolution = ov.resolution
	}
	if set["confirmation-runs"] {
		cfg.Search.ConfirmationRuns = ov.confirmRuns
	}
	if set["failure-confirmation"] {
		cfg.Search.FailureConfirmation = ov.failConfirm
	}
	if set["max-rate"] {
		cfg.Search.MaxRate = ov.maxRate
	}
	if set["max-probes"] {
		cfg.Search.MaxProbes = ov.maxProbes
	}
	if set["max-vu-growth"] {
		cfg.Search.MaxVUGrowth = ov.maxVUGrowth
	}
	if set["validation-rate"] {
		cfg.Validation.Rate = ov.validationRate
	}
	if set["safety-margin"] {
		cfg.Validation.SafetyMargin = ov.safetyMargin
	}

	cfg.ApplyDefaults()
	if err := cfg.ValidateMode(mode); err != nil {
		fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
		return nil, exitError
	}
	return cfg, exitPass
}

// parseDurationFlag parses a duration flag, reporting the error to stderr.
func parseDurationFlag(name, value string) (time.Duration, bool) {
	d, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "autobench: invalid %s %q: %v\n", name, value, err)
		return 0, false
	}
	return d, true
}

// writeJSONFile writes v to path as indented JSON.
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeHTMLFile renders a report to path. It buffers first so a template error
// cannot leave a truncated file behind.
func writeHTMLFile(path string, render func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		return err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func usage(w *os.File) {
	fmt.Fprintf(w, `autobench %s — adaptive load testing around k6

Usage:
  autobench run [flags] [config.yaml]
  autobench run bench.yaml --rate 5000 --duration 2m
  autobench capacity [flags] [config.yaml]
  autobench compare [flags] baseline.json candidate.json
  autobench version

Commands:
  run        Run a fixed-rate load test and report the result (alias: fixed)
  capacity   Search for the maximum sustainable iteration rate (alias: search)
  compare    Compare two recorded runs and fail on a regression
  version    Print the autobench version
  help       Show this help

Exit codes:
  0  all thresholds passed
  1  a threshold failed, no capacity was found, a regression was found, or the
     load generator saturated
  2  usage or runtime error

`, version)
}

func runUsage(fs *flag.FlagSet) {
	fmt.Fprintf(fs.Output(), `Usage:
  autobench run [flags] [config.yaml]

Runs k6 at a constant arrival rate and reports latency percentiles, error
rate and an SLO verdict. Any config value can be overridden by a flag.

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(fs.Output(), `
Examples:
  autobench run --target http://localhost:3000 --rate 500 --duration 30s
  autobench run examples/api.yaml
  autobench run examples/api.yaml --rate 2000 --json result.json
`)
}
