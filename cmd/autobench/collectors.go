package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/callum/k6-autobench/internal/collect"
	"github.com/callum/k6-autobench/internal/config"
)

// buildCollectors turns the configured collectors into the objects that observe
// the system under test during a measurement (PLAN-OUTLINE.md §33).
//
// It returns nil when nothing is configured, which is the signal the runner uses
// to skip the bookkeeping entirely.
func buildCollectors(cfg *config.Config) []collect.Collector {
	if !cfg.Collectors.Enabled() {
		return nil
	}

	interval := cfg.Collectors.Interval.Duration
	if interval <= 0 {
		interval = config.DefaultCollectInterval
	}

	out := make([]collect.Collector, 0, len(cfg.Collectors.Prometheus))
	for _, src := range cfg.Collectors.Prometheus {
		queries := make([]collect.Query, 0, len(src.Queries))
		for _, q := range src.Queries {
			queries = append(queries, collect.Query{Name: q.Name, Expr: q.Query, Unit: q.Unit})
		}
		out = append(out, &collect.Prometheus{
			Source:   src.Name,
			URL:      src.URL,
			Queries:  queries,
			Interval: interval,
		})
	}
	return out
}

// collectorNotice describes what will be sampled, so a long run says up front
// which sources it is watching. It returns "" when nothing is configured.
func collectorNotice(cfg *config.Config) string {
	if !cfg.Collectors.Enabled() {
		return ""
	}
	names := make([]string, 0, len(cfg.Collectors.Prometheus))
	total := 0
	for _, src := range cfg.Collectors.Prometheus {
		names = append(names, src.Name)
		total += len(src.Queries)
	}
	return fmtCollectors(names, total, cfg.Collectors.Interval.Duration)
}

// fmtCollectors is split out so the notice can be tested without a config.
func fmtCollectors(names []string, queries int, interval time.Duration) string {
	return fmt.Sprintf("%s (%d queries every %s)", strings.Join(names, ", "), queries, interval)
}
