package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/CallumBicknell/k6-autobench/internal/collect"
	"github.com/CallumBicknell/k6-autobench/internal/config"
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

	// The order is fixed rather than map-driven, so a report lists its sources
	// the same way every run.
	var out []collect.Collector

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

	for _, src := range cfg.Collectors.Docker {
		out = append(out, &collect.Docker{
			Source:    src.Name,
			Container: src.Container,
			Socket:    src.Socket,
			Interval:  interval,
		})
	}

	for _, src := range cfg.Collectors.System {
		out = append(out, &collect.System{
			Source:   src.Name,
			Proc:     src.Proc,
			Interval: interval,
		})
	}

	for _, src := range cfg.Collectors.Kubernetes {
		out = append(out, &collect.Kubernetes{
			Source:    src.Name,
			URL:       src.URL,
			Token:     src.Token,
			Namespace: src.Namespace,
			Selector:  src.Selector,
			CAFile:    src.CAFile,
			Insecure:  src.Insecure,
			Interval:  interval,
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

	var names []string
	for _, src := range cfg.Collectors.Prometheus {
		names = append(names, src.Name)
	}
	for _, src := range cfg.Collectors.Docker {
		names = append(names, src.Name)
	}
	for _, src := range cfg.Collectors.System {
		names = append(names, src.Name)
	}
	for _, src := range cfg.Collectors.Kubernetes {
		names = append(names, src.Name)
	}

	return fmtCollectors(names, cfg.Collectors.Interval.Duration)
}

// fmtCollectors is split out so the notice can be tested without a config.
func fmtCollectors(names []string, interval time.Duration) string {
	return fmt.Sprintf("%s every %s", strings.Join(names, ", "), interval)
}
