package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/callum/k6-autobench/internal/bench"
	"github.com/callum/k6-autobench/internal/config"
	"github.com/callum/k6-autobench/internal/report"
	"github.com/callum/k6-autobench/internal/results"
	"github.com/callum/k6-autobench/internal/runner"
	"github.com/callum/k6-autobench/internal/search"
)

// capacityReport is the machine-readable form of a capacity search, written by
// --json and --out-dir.
type capacityReport struct {
	Name      string          `json:"name"`
	Target    string          `json:"target"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at"`
	ElapsedMs float64         `json:"elapsed_ms"`
	Search    search.Config   `json:"search"`
	Outcome   *search.Outcome `json:"outcome,omitempty"`
	Verdict   results.Verdict `json:"verdict"`
	Error     string          `json:"error,omitempty"`
}

func capacityCommand(args []string) int {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
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
	fs.IntVar(&ov.startRate, "start-rate", 0, "first rate to probe, iterations/s (overrides config search.start_rate)")
	fs.Float64Var(&ov.increaseFactor, "increase-factor", 0, "rate multiplier applied after each passing probe (overrides config)")
	fs.IntVar(&ov.resolution, "resolution", 0, "stop bisecting once the bracket is this narrow, iterations/s (overrides config)")
	fs.StringVar(&ov.probeDuration, "probe-duration", "", "duration of each measurement, e.g. 1m (overrides config)")
	fs.StringVar(&ov.warmup, "warmup", "", "discarded warmup run before each measurement, e.g. 10s (overrides config)")
	fs.IntVar(&ov.confirmRuns, "confirmation-runs", 0, "consecutive passes needed to accept a rate (overrides config)")
	fs.IntVar(&ov.failConfirm, "failure-confirmation", 0, "failures needed to reject a rate (overrides config)")
	fs.IntVar(&ov.maxRate, "max-rate", 0, "stop growing above this rate, iterations/s (overrides config)")
	fs.IntVar(&ov.maxProbes, "max-probes", 0, "cap on the number of measurements (overrides config)")
	fs.IntVar(&ov.maxVUGrowth, "max-vu-growth", 0, "times a saturated probe may double the VU pool (overrides config)")
	fs.StringVar(&ov.validationDur, "validation-duration", "", "length of the confirming run, 0s to skip it (overrides config)")
	fs.StringVar(&ov.validationRate, "validation-rate", "", "\"discovered\" or an explicit rate for the confirming run (overrides config)")
	fs.Float64Var(&ov.safetyMargin, "safety-margin", 0, "fraction of the discovered capacity to validate at, e.g. 0.9 (overrides config)")
	fs.IntVar(&ov.vus, "vus", 0, "pin the pre-allocated VU pool (overrides config)")
	fs.IntVar(&ov.maxVUs, "max-vus", 0, "pin the maximum VU pool (overrides config)")
	fs.StringVar(&ov.k6Timeout, "k6-timeout", "", "kill k6 if a probe runs longer than this (default: the probe duration plus a grace period)")
	fs.StringVar(&k6Path, "k6", "", "path to the k6 binary (default: $AUTOBENCH_K6 or \"k6\")")
	fs.StringVar(&scriptPath, "script", "", "override the embedded k6 workload script")
	fs.StringVar(&jsonOut, "json", "", "write the search result as JSON to this path")
	fs.StringVar(&htmlOut, "html", "", "write a self-contained HTML report to this path")
	fs.StringVar(&outDir, "out-dir", "", "write the config, manifest, per-probe results and reports to this directory")
	fs.BoolVar(&noColor, "no-color", false, "disable coloured output")
	registerAnalysisFlags(fs, &ov)
	fs.Usage = func() { capacityUsage(fs) }

	if code := parseFlags(fs, args, &ov.configPath); code != exitPass {
		return code
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, code := buildConfig(ov.configPath, config.ModeCapacity, set, ov)
	if cfg == nil {
		return code
	}
	return runCapacity(cfg, ov, k6Path, scriptPath, jsonOut, htmlOut, outDir, noColor)
}

// runCapacity drives the search for an already-validated config.
func runCapacity(cfg *config.Config, ov overrides, k6Path, scriptPath, jsonOut, htmlOut, outDir string, noColor bool) int {
	validationRate, err := cfg.ValidationRateOverride()
	if err != nil {
		fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
		return exitError
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := &bench.Measurer{
		Config:         cfg,
		Runner:         &runner.Runner{K6Path: k6Path, ScriptPath: scriptPath},
		Warmup:         cfg.Search.Warmup.Duration,
		MaxVUGrowth:    cfg.Search.MaxVUGrowth,
		Window:         cfg.Analysis.Window.Duration,
		Timeout:        cfg.Run.K6Timeout.Duration,
		InsecureTLS:    cfg.Target.TLS.Insecure,
		CAFile:         cfg.Target.TLS.CAFile,
		RequestTimeout: cfg.Target.RequestTimeout.Duration,
		Collectors:     buildCollectors(cfg),
		OnProgress:     func(msg string) { fmt.Fprintln(os.Stderr, "autobench: "+msg) },
	}
	if notice := collectorNotice(cfg); notice != "" {
		fmt.Fprintf(os.Stderr, "autobench: sampling %s per probe\n", notice)
	}

	scfg := search.Config{
		StartRate:           cfg.Search.StartRate,
		IncreaseFactor:      cfg.Search.IncreaseFactor,
		Resolution:          cfg.Search.Resolution,
		ConfirmationRuns:    cfg.Search.ConfirmationRuns,
		FailureConfirmation: cfg.Search.FailureConfirmation,
		MaxRate:             cfg.Search.MaxRate,
		MaxProbes:           cfg.Search.MaxProbes,
		ProbeDuration:       cfg.Search.ProbeDuration.Duration,
		ValidationDuration:  cfg.Validation.Duration.Duration,
		ValidationRate:      validationRate,
		SafetyMargin:        cfg.Validation.SafetyMargin,
	}

	ctrl := search.New(scfg, m.ProbeFunc())
	ctrl.OnProbe(func(p search.Probe) {
		fmt.Fprintf(os.Stderr, "autobench: probe %d/%s %d/s → %s\n",
			p.Index, p.Phase, p.Rate, p.Result.Verdict)
	})

	fmt.Fprintf(os.Stderr, "autobench: searching for capacity against %s (start %d/s, probe %s)\n",
		cfg.Target.BaseURL, cfg.Search.StartRate, cfg.Search.ProbeDuration)
	if notice := windowNotice(cfg, cfg.Search.StartRate, cfg.Search.ProbeDuration.Duration,
		cfg.Search.MaxProbes); notice != "" {
		fmt.Fprintf(os.Stderr, "autobench: %s\n", notice)
	}

	started := time.Now()
	out, runErr := ctrl.Run(ctx)
	ended := time.Now()

	color := !noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	if out != nil {
		report.RenderCapacity(os.Stdout, cfg, out, version, ended.Sub(started), color)
	}

	rep := &capacityReport{
		Name:      cfg.Name,
		Target:    cfg.Target.BaseURL,
		StartedAt: started,
		EndedAt:   ended,
		ElapsedMs: float64(ended.Sub(started)) / float64(time.Millisecond),
		Search:    scfg,
		Outcome:   out,
		Verdict:   out.Verdict(),
	}
	if runErr != nil {
		rep.Error = runErr.Error()
	}

	if jsonOut != "" {
		if err := writeJSONFile(jsonOut, rep); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}
	if htmlOut != "" {
		if err := writeHTMLFile(htmlOut, func(w io.Writer) error {
			return report.RenderCapacityHTML(w, cfg, out, version, ended.Sub(started), ended)
		}); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}
	if outDir != "" {
		if err := writeArtifacts(outDir, ov.configPath, rep, cfg, out, ended.Sub(started)); err != nil {
			fmt.Fprintln(os.Stderr, "autobench: "+err.Error())
			return exitError
		}
	}

	switch {
	case runErr == nil:
	case errors.Is(runErr, search.ErrNoPassingRate),
		errors.Is(runErr, search.ErrNoResponses),
		errors.Is(runErr, search.ErrGeneratorSaturated):
		// A search outcome rather than a crash: report it and fail the gate.
		fmt.Fprintln(os.Stderr, "autobench: "+runErr.Error())
	case ctx.Err() != nil:
		fmt.Fprintln(os.Stderr, "autobench: interrupted")
		return exitSigint
	default:
		fmt.Fprintln(os.Stderr, "autobench: "+runErr.Error())
		return exitError
	}

	if out.Pass() {
		return exitPass
	}
	return exitFail
}

// probeFileRE matches the names writeArtifacts gives per-probe files. Anything
// not matching is not autobench's to delete.
var probeFileRE = regexp.MustCompile(`^\d{3}-[a-z]+-\d+\.json$`)

// artifactNames are the files autobench owns at the top of an --out-dir.
var artifactNames = []string{"config.yaml", "manifest.json", "summary.json", "report.html"}

// clearStaleArtifacts removes what an earlier run into the same directory left
// behind.
//
// --out-dir is where one run's artefacts go, and a directory holding some of this
// run and some of a previous one is worse than either: the manifest and summary
// are overwritten, so nothing on disk says the extras do not belong. A run with
// fewer probes than its predecessor used to leave the surplus behind, and a run
// with no config path used to leave the previous config.yaml next to a manifest
// that did not come from it.
//
// Only the names autobench writes are removed. Anything else the directory holds
// is left alone, because pointing --out-dir at a directory that already has
// something in it should not be destructive.
func clearStaleArtifacts(dir string) error {
	for _, name := range artifactNames {
		if err := removeIfPresent(filepath.Join(dir, name)); err != nil {
			return err
		}
	}

	probeDir := filepath.Join(dir, "probes")
	entries, err := os.ReadDir(probeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !probeFileRE.MatchString(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(probeDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// removeIfPresent deletes a file, treating absence as success.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeArtifacts persists a search so it can be re-read and diffed later
// (PLAN-OUTLINE.md §26): the source config, a manifest, one file per probe, a
// compact summary and the HTML report.
func writeArtifacts(dir, configPath string, rep *capacityReport, cfg *config.Config, out *search.Outcome, elapsed time.Duration) error {
	probeDir := filepath.Join(dir, "probes")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", probeDir, err)
	}
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

	if err := writeJSONFile(filepath.Join(dir, "manifest.json"), rep); err != nil {
		return err
	}
	if out != nil {
		for _, p := range out.Probes {
			name := fmt.Sprintf("%03d-%s-%d.json", p.Index, p.Phase, p.Rate)
			if err := writeJSONFile(filepath.Join(probeDir, name), p); err != nil {
				return err
			}
		}
	}

	if err := writeJSONFile(filepath.Join(dir, "summary.json"), searchSummaryReport(rep)); err != nil {
		return err
	}
	return writeHTMLFile(filepath.Join(dir, "report.html"), func(w io.Writer) error {
		return report.RenderCapacityHTML(w, cfg, out, version, elapsed, rep.EndedAt)
	})
}

// capacitySummary is the compact, CI-friendly form of a search.
type capacitySummary struct {
	Name           string          `json:"name"`
	Target         string          `json:"target"`
	Verdict        results.Verdict `json:"verdict"`
	Capacity       int             `json:"capacity"`
	LowerBoundOnly bool            `json:"lower_bound_only"`
	PassingRate    int             `json:"passing_rate"`
	FailingRate    int             `json:"failing_rate"`
	ValidationRate int             `json:"validation_rate,omitempty"`
	Probes         int             `json:"probes"`
	ElapsedMs      float64         `json:"elapsed_ms"`
	Error          string          `json:"error,omitempty"`
}

func searchSummaryReport(rep *capacityReport) capacitySummary {
	s := capacitySummary{
		Name:      rep.Name,
		Target:    rep.Target,
		Verdict:   rep.Verdict,
		ElapsedMs: rep.ElapsedMs,
		Error:     rep.Error,
	}
	if rep.Outcome != nil {
		s.Capacity = rep.Outcome.Capacity
		s.LowerBoundOnly = rep.Outcome.LowerBoundOnly
		s.PassingRate = rep.Outcome.PassingRate
		s.FailingRate = rep.Outcome.FailingRate
		s.ValidationRate = rep.Outcome.ValidationRate
		s.Probes = len(rep.Outcome.Probes)
	}
	return s
}

func capacityUsage(fs *flag.FlagSet) {
	fmt.Fprint(fs.Output(), `Usage:
  autobench capacity [flags] [config.yaml]

Finds the maximum sustainable iteration rate: it grows the rate until a
threshold fails, bisects that bracket down to the configured resolution, then
optionally validates the result with a longer confirming run.

Flags:
`)
	fs.PrintDefaults()
	fmt.Fprint(fs.Output(), `
Examples:
  autobench capacity examples/api-capacity.yaml
  autobench capacity bench.yaml --start-rate 500 --resolution 50
  autobench capacity bench.yaml --probe-duration 30s --validation-duration 2m
  autobench capacity --target http://localhost:3000 --start-rate 100 --max-probes 6
`)
}
