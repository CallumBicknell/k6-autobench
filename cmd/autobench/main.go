// Command autobench is an adaptive load-testing orchestrator around k6.
//
// V0.1 provides a single fixed-rate run: it drives k6 at a constant arrival
// rate, parses the machine-readable summary, evaluates configured SLO
// thresholds and renders a terminal report.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/metrics"
	"github.com/callum/k6-autobench/internal/report"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/runner"
)

const version = "0.2.0"

// Exit codes.
const (
	exitPass   = 0
	exitFail   = 1
	exitError  = 2
	exitSigint = 130
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitError
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("autobench %s\n", version)
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

func runCommand(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		configPath string
		target     string
		rate       int
		duration   string
		vus        int
		maxVUs     int
		timeUnit   string
		k6Path     string
		scriptPath string
		jsonOut    string
		noColor    bool
	)
	fs.StringVar(&configPath, "config", "", "path to a YAML config file")
	fs.StringVar(&configPath, "c", "", "path to a YAML config file (shorthand)")
	fs.StringVar(&target, "target", "", "target base URL (overrides config)")
	fs.IntVar(&rate, "rate", 0, "iteration rate (overrides config run.rate)")
	fs.StringVar(&duration, "duration", "", "run duration, e.g. 30s or 2m (overrides config)")
	fs.IntVar(&vus, "vus", 0, "pre-allocated VUs (overrides config)")
	fs.IntVar(&maxVUs, "max-vus", 0, "maximum VUs (overrides config)")
	fs.StringVar(&timeUnit, "time-unit", "", "arrival-rate time unit, e.g. 1s (overrides config)")
	fs.StringVar(&k6Path, "k6", "", "path to the k6 binary (default: $AUTOBENCH_K6 or \"k6\")")
	fs.StringVar(&scriptPath, "script", "", "override the embedded k6 workload script")
	fs.StringVar(&jsonOut, "json", "", "write the full result as JSON to this path")
	fs.BoolVar(&noColor, "no-color", false, "disable coloured output")
	fs.Usage = func() { runUsage(fs) }

	if code := parseFlags(fs, args, &configPath); code != exitPass {
		return code
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, code := buildConfig(configPath, set, target, rate, duration, vus, maxVUs, timeUnit)
	if cfg == nil {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "autobench: %d iterations/%s for %s against %s\n",
		cfg.Run.Rate, cfg.Run.TimeUnit, cfg.Run.Duration, cfg.Target.BaseURL)

	r := &runner.Runner{K6Path: k6Path, ScriptPath: scriptPath}
	spec := runner.Spec{
		BaseURL:         cfg.Target.BaseURL,
		Requests:        cfg.Workload.Requests,
		Rate:            cfg.Run.Rate,
		TimeUnit:        cfg.Run.TimeUnit,
		Duration:        cfg.Run.Duration.Duration,
		PreAllocatedVUs: cfg.LoadGenerator.PreAllocatedVUs,
		MaxVUs:          cfg.LoadGenerator.MaxVUs,
		Percentiles:     cfg.Percentiles,
	}

	started := time.Now()
	summary, err := r.Run(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "autobench: interrupted")
			return exitSigint
		}
		fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
		return exitError
	}
	ended := time.Now()

	m := metrics.Extract(summary, cfg.Percentiles)
	verdict, failures, gen := results.Evaluate(cfg, m)

	res := &results.Result{
		Name:      cfg.Name,
		Target:    cfg.Target.BaseURL,
		Rate:      cfg.Run.Rate,
		TimeUnit:  cfg.Run.TimeUnit,
		Duration:  cfg.Run.Duration,
		VUs:       results.VUPlan{PreAllocated: cfg.LoadGenerator.PreAllocatedVUs, Max: cfg.LoadGenerator.MaxVUs},
		StartedAt: started,
		EndedAt:   ended,
		ElapsedMs: float64(ended.Sub(started)) / float64(time.Millisecond),
		Metrics:   m,
		Verdict:   verdict,
		Failures:  failures,
		Generator: gen,
		Raw:       summary.Metrics,
	}

	color := !noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	report.Render(os.Stdout, cfg, res, version, color)

	if jsonOut != "" {
		if err := writeJSON(jsonOut, res); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}

	if verdict.OK() {
		return exitPass
	}
	return exitFail
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
				return exitPass
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
// applies defaults and validates. It returns a nil config and an exit code on
// failure.
func buildConfig(path string, set map[string]bool, target string, rate int, duration string, vus, maxVUs int, timeUnit string) (*config.Config, int) {
	var cfg *config.Config
	if path != "" {
		loaded, err := config.Load(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return nil, exitError
		}
		cfg = loaded
	} else {
		cfg = &config.Config{}
	}

	if set["target"] {
		cfg.Target.BaseURL = target
	}
	if set["rate"] {
		cfg.Run.Rate = rate
	}
	if set["time-unit"] {
		cfg.Run.TimeUnit = timeUnit
	}
	if set["vus"] {
		cfg.LoadGenerator.PreAllocatedVUs = vus
	}
	if set["max-vus"] {
		cfg.LoadGenerator.MaxVUs = maxVUs
	}
	if set["duration"] {
		d, err := time.ParseDuration(duration)
		if err != nil {
			fmt.Fprintf(os.Stderr, "autobench: invalid --duration %q: %v\n", duration, err)
			return nil, exitError
		}
		cfg.Run.Duration = config.Duration{Duration: d}
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
		return nil, exitError
	}
	return cfg, exitPass
}

func writeJSON(path string, res *results.Result) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
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
  autobench version

Commands:
  run        Run a fixed-rate load test and report the result
  version    Print the autobench version
  help       Show this help

Exit codes:
  0  all thresholds passed
  1  a threshold failed or the load generator saturated
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
