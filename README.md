# autobench

An adaptive load-testing orchestrator around [k6](https://k6.io/).

autobench is the **controller**; k6 is the **traffic generator**. The controller
decides the load, spawns k6, parses the machine-readable summary, evaluates your
SLO thresholds and renders the report. k6 stays a dumb, precisely controlled
arrival-rate generator.

> **Status: V0.9** — `autobench run` (fixed rate) and `autobench capacity`
> (adaptive search) over weighted request pools or multi-step journeys, with
> per-endpoint and response-class breakdowns, windowed latency analysis that
> attributes a spike to the request that caused it, resource collectors that
> sample the system under test while it is loaded, JSON and a self-contained
> HTML report. See `PLAN-OUTLINE.md`.

## Install

Requires Go 1.24+ and `k6` on your `PATH` (or set `AUTOBENCH_K6`).

```bash
go build -o bin/autobench ./cmd/autobench
# or
make build
```

## Usage

```bash
# Fixed rate: one measurement at a known load
autobench run --target http://localhost:3000 --rate 500 --duration 30s
autobench run examples/api.yaml
autobench run examples/api.yaml --rate 2000 --json result.json

# Capacity search: find the maximum sustainable rate
autobench capacity examples/api-capacity.yaml
autobench capacity bench.yaml --start-rate 500 --probe-duration 30s
autobench capacity --target http://localhost:3000 --start-rate 100 --max-probes 6
```

The config path is a positional argument and flags may appear before or after it.
`run` is also available as `fixed`, and `capacity` as `search`.

Exit codes: `0` all thresholds passed · `1` a threshold failed, no capacity was
found, or the generator saturated · `2` usage/runtime error.

Verdicts: `PASS`, `FAIL_LATENCY`, `FAIL_ERRORS`, `FAIL_GENERATOR`,
`FAIL_NO_CAPACITY`. A saturated generator outranks threshold breaches, because a
probe that never reached the target rate says nothing about the system under
test.

### Flags

| Flag | Description |
| --- | --- |
| `-c, --config` | YAML config path (also accepted as a positional arg) |
| `--target` | Target base URL (overrides `target.base_url`) |
| `--rate` | Iteration rate (overrides `run.rate`) |
| `--duration` | Run duration, e.g. `30s`, `2m` (a bare number means seconds) |
| `--time-unit` | Arrival-rate time unit, e.g. `1s`, `1m` |
| `--vus` | Pre-allocated VUs |
| `--max-vus` | Maximum VUs |
| `--k6` | Path to the k6 binary |
| `--script` | Override the embedded k6 workload script |
| `--json` | Write the full result as JSON |
| `--html` | Write a self-contained HTML report |
| `--no-color` | Disable coloured output |

`autobench capacity` additionally accepts:

| Flag | Description |
| --- | --- |
| `--start-rate` | First rate probed (overrides `search.start_rate`) |
| `--increase-factor` | Rate multiplier per passing probe |
| `--resolution` | Stop bisecting once the bracket is this narrow |
| `--probe-duration` | Duration of each measurement |
| `--warmup` | Discarded warmup run before each measurement |
| `--confirmation-runs` | Passes required to accept a rate |
| `--failure-confirmation` | Failures required to reject a rate |
| `--max-rate` | Ceiling for the exponential phase |
| `--max-probes` | Cap on the number of measurements |
| `--max-vu-growth` | Times a saturated probe may double the VU pool |
| `--validation-duration` | Length of the confirming run (`0s` skips it) |
| `--validation-rate` | `discovered` or an explicit rate |
| `--safety-margin` | Fraction of capacity to validate at, e.g. `0.9` |
| `--html` | Write a self-contained HTML report |
| `--out-dir` | Write the config, manifest, per-probe results, summary and HTML report |

## The capacity search

`autobench capacity` answers "how much load can this system take?" in three
phases:

1. **Exponential bracket.** Probe `start_rate`, then multiply by
   `increase_factor` after each pass until something fails. That brackets the
   boundary between the last rate that passed and the first that failed.
2. **Binary search.** Bisect the bracket until it is no wider than `resolution`,
   so you get a number rather than an order of magnitude.
3. **Validation.** Re-run at `safety_margin` × the discovered capacity for
   `validation.duration`, long enough to trust the result.

```yaml
search:
  start_rate: 200        # defaults to run.rate when run is present
  increase_factor: 1.5
  resolution: 50         # iterations/s
  probe_duration: 1m
  warmup: 10s            # discarded run so the target is warm before measuring
  confirmation_runs: 1   # passes needed to accept a rate
  failure_confirmation: 1
  max_probes: 32
  # max_rate: 20000      # optional ceiling for phase 1
  # max_vu_growth: 3     # off by default; see "Generator saturation" below

validation:
  rate: discovered       # or an explicit rate
  duration: 5m           # omit the block to skip validation
  safety_margin: 0.9
```

Only the zero value counts as "unset", so a negative or invalid value is
rejected rather than silently replaced with a default.

### Reading the report

Every probe is listed, which is the rate/latency curve in tabular form:

```
  #   PHASE             RATE       P95       P99  ACHIEVED  VERDICT
  1   exponential        100     1.2ms     1.4ms       100  PASS
  2   exponential        200     1.2ms     1.5ms       200  PASS
  3   exponential        400     1.2ms     1.7ms       400  PASS
  4   exponential        800     1.3ms     2.9ms       800  FAIL_ERRORS
      error_rate     93.750%  >  1.000%   (limit)
  5   binary             600     1.1ms     2.1ms       600  FAIL_ERRORS
  ...
  9   binary             487     1.4ms     3.6ms       487  PASS
  10  validation         438     1.2ms     1.8ms       438  PASS

  Sustained     487 iterations/s
  Breached      500 iterations/s
  Resolution    20/s (bracket width 13)
  Capacity      487 iterations/s
  Validated     438 iterations/s for 5s (margin 0.9)

  RESULT: PASS
```

If the search never finds a failure it reports a lower bound (`Capacity ≥`) and
says so, rather than inventing a number. If even `start_rate` fails it reports
`FAIL_NO_CAPACITY` and tells you to lower `start_rate`.

### Generator saturation

If k6 cannot reach the target rate, the probe is **discarded** — the load
generator was the bottleneck, so the rate says nothing about the target. The
search stops and reports `FAIL_GENERATOR` with advice.

By default autobench does not grow the VU pool on its own, because silently
spawning thousands of VUs is a poor surprise on a shared machine. Set
`search.max_vu_growth: 3` to let it double the pool and retry the same rate
(useful for long searches against a slow target). Pinning `load_generator`
disables growth, since the pool then has a hard ceiling.

### Per-probe VU sizing

`capacity` re-sizes the VU pool for every probe rate using Little's law
(`concurrency = rate × mean latency`), starting from a 100ms assumption and
using the latency the previous probe actually observed. The pool is monotone: it
never shrinks below what the search has already needed, so a retry always gives
k6 more VUs than the attempt that saturated. Anything you pin in
`load_generator` is left alone.

## Reporting

### Response classes

`FAIL_ERRORS` says requests failed; it does not say how, and the difference
changes the conclusion. The workload script counts each response by class, so the
report can:

```
  Errors        3,351  (93.057%)
    2xx               250  (6.943% of responses)
    5xx               3,351  (93.057% of responses)
  ...
  RESULT: FAIL_ERRORS
  ⚠ 93.057% of responses were 5xx: the system under test is failing.
```

A 4xx storm is the benchmark's fault, and the report says so:

```
  ⚠ 100.000% of responses were 4xx: the target is rejecting these requests —
    check the workload (paths, methods, bodies) before reading the rest as capacity.
```

3xx and status-less (transport) responses get their own explanation. The
breakdown is printed only when something other than 2xx was seen, so a healthy
run stays quiet.

The classes come from four counters the workload script declares up front
(`ab_status_2xx` … `ab_status_5xx`, plus `ab_status_other`), because k6 metrics
must exist before a test starts.

### Endpoint breakdown

Every configured request carries its `name` as a k6 tag, so the report breaks
latency and errors down per endpoint, not just for the run as a whole:

```
  REQUEST                REQS   ERRORS       avg       p95       p99
  feed                    384        0     0.7ms     1.0ms     1.2ms
  post                    200        0     0.7ms     1.0ms     1.1ms
  like                    127        0     0.7ms     0.9ms     1.1ms
  create_post              89        0     0.8ms     1.0ms     1.3ms
```

Rows are ordered busiest-first, so the endpoint carrying the most load leads.
This is what turns "the p99 is 200ms" into "the p99 is 200ms *because of
`/feed`".

k6 only includes a tagged submetric in its summary when a threshold selects it,
so the workload script declares always-satisfied thresholds (`p(95)>=0`, with
`abortOnFail` disabled) for each request name. They cannot change the outcome of
a run; the controller still owns every pass/fail decision.

### HTML report

`--html report.html` writes a single self-contained file: inline CSS, inline SVG,
and no JavaScript or external assets, so it renders identically offline and can
be archived as a CI artefact.

It contains the verdict, run facts, the per-endpoint table, and — for a capacity
search — a rate-vs-latency chart with the p95 SLO line and the discovered
capacity marked, plus the full probe trace:

```
  Rate vs latency
     2s │                                    ●
        │                              ●
   500ms│                        ●
        │                  ●
   100ms│ ●──●──●──●──●
        └──────────────────────────────────
          100   200   400   600   800
```

### Result storage

`capacity --out-dir results/` writes everything needed to reproduce and re-read a
search:

```text
results/
├── config.yaml      # the config as it was read
├── manifest.json    # search parameters, every probe, the verdict
├── summary.json     # compact, CI-friendly: capacity, rates, verdict
├── report.html      # the HTML report
└── probes/
    ├── 001-exponential-100.json
    ├── 002-exponential-200.json
    └── ...
```

## Latency over time

An overall p99 can hide a collapse. A system that is fine for four minutes and
then falls over still averages out:

```
  Latency
    p95       1.3ms        <- the run as a whole looks healthy
    max     301.5ms

  LATENCY OVER TIME (5s windows)

  WINDOW          REQUESTS   ERRORS        p95        p99        max
  0s-5s              1,003        0      1.4ms          -     10.4ms
  5s-10s             1,000        0    300.9ms          -    301.5ms ← worst
  10s-15s            1,000        0      1.0ms          -      2.8ms
  15s-20s              997        0      1.0ms          -      9.9ms

  Worst window  5s-10s: p95 300.9ms (249.5x the median 1.2ms)

  RESULT: FAIL_LATENCY
```

That run is a **PASS** without windowing. Enable it with:

```yaml
analysis:
  window: 5s
  require_all_windows_pass: true
```

`window` slices every measurement — fixed runs, every search probe and the
validation run — into buckets of that width. `require_all_windows_pass` makes a
measurement fail when *any* window breaches a threshold, rather than only when
the run as a whole does. Without it the window table is still reported, so the
spike is visible even when the verdict is lenient.

Windowed analysis is off by default because it costs a sample file proportional
to `rate × duration` — k6 has to stream every sample to disk for it. Windows
with no traffic are reported as empty rather than skipped, and if a window holds
more samples than are retained its percentiles come from a prefix and the report
says so.

Both the terminal and HTML reports gain a latency-over-time chart with the same
SLO line as the rate/latency chart.

### Which endpoint caused the spike

A window tells you latency moved; it does not tell you what moved it. Each
window therefore carries the same per-request breakdown as the run, and the
report compares each request's worst window against its own run-wide latency:

```
  Worst window  5s-10s: p95 300.8ms (248.3x the median 1.2ms)

  ENDPOINT DRIFT (worst window vs the run, p95)

  REQUEST                 RUN p95  WORST WINDOW        p95     RATIO
  slow                      1.3ms        5s-10s    301.1ms    227.6x
  fast                      1.1ms         0s-5s      1.4ms      1.2x
```

Worst drift first, so the request to look at leads. This is a diagnostic rather
than a gate: the run above passed its 500ms p95 SLO, and the table still points
at `/slow`.

## What the system was doing

Everything above is the view from the client: how slow the response was, how
many failed. It cannot say *why*. k6 does not know that the database was out of
connections when the request arrived.

Collectors sample the system under test's own metrics while it is loaded, from
outside it, and the report puts them next to the latency they caused:

```yaml
collectors:
  interval: 5s
  prometheus:
    - name: api
      url: http://localhost:9090
      queries:
        - name: cpu
          query: rate(process_cpu_seconds_total{job="api"}[1m])
          unit: cores
        - name: memory
          query: process_resident_memory_bytes{job="api"}
          unit: bytes

    - name: postgres
      url: http://localhost:9090
      queries:
        - name: connections
          query: pg_stat_activity_count{job="postgres"}
        - name: cpu
          query: 100 * (1 - rate(node_cpu_seconds_total{mode="idle"}[1m]))
          unit: percent
```

```
  SYSTEM (sampled every 5s)

  SERIES                          MIN        MAX        AVG   PEAK AT
  api cpu (cores)                0.31       0.42       0.37       10s
  api memory                 204.1MiB   457.8MiB   331.9MiB       10s
  postgres connections            120        498      342.7       45s
  postgres cpu                  22.0%      99.0%      61.4%       45s
```

The API never worked hard. The database ran out of connections at 45s — and if
you have windowed analysis on, you can read that against the window table to see
whether the latency spike started then.

`PEAK AT` is measured from the start of the measurement, on the same timeline the
window labels use. A series that never moved shows a dash: a connection pool
pinned at its ceiling has no peak to point at.

### Units

Prometheus values are unitless, so the configuration has to say what they mean.
`bytes` is rendered in binary units and `percent` with a sign; any other unit is
attached to the series name (`api cpu (cores)`). Keeping units out of the value
columns is what keeps a table of mixed units aligned.

### Collectors never fail a run

A collector is advisory. If Prometheus is unreachable, or a query has a typo, or
the expression matches nothing, the report says so and the run carries on:

```
  ⚠ COLLECTOR PROBLEMS — readings missing, the run is unaffected
    down: cpu: Get "http://127.0.0.1:19999/api/v1/query?query=up": dial tcp 127.0.0.1:19999: connect: connection refused
    api: typo: prometheus error: unknown expression 'rate(nosuch_metric_total[1m])'
```

The verdict is untouched. Your monitoring being down says nothing about your
capacity, and a load test that errors because Prometheus is unavailable would be
worse than useless. Each failure is reported once, however many polls hit it.

Mistakes that are visible without asking the server — a URL with no host, a
duplicate name, an empty expression — are rejected at configuration time
instead.

Collectors run for **every** measurement: the fixed run, each search probe and
the validation run. The capacity report shows the deciding measurement's
readings (the validation run, or the last passing probe).

### Which resource is the bottleneck

The SYSTEM table says what the system was doing. With windowed analysis on, the
report also says which of it moved with the latency:

```
  LIKELY CORRELATED (resource vs p95, strongest first)

  SERIES                          r   WINDOWS
  postgres connections        +0.97         9
```

A live run: the target slowed from 7.5ms to 24.7ms per window, and Postgres
connections climbed from 104 to 456 alongside it. Two other series were
collected and left out — one oscillating on a period unrelated to the ramp, one
pinned flat.

Three things make that worth reading rather than noise:

- **Rank correlation, not raw.** A resource that climbs 20% while latency
  triples is still moving together, and one bad window would otherwise drag a
  product-moment correlation down on its own.
- **A permutation test, not a formula.** A handful of windows is exactly where
  the normal approximation is worst, and shuffling assumes nothing about the
  shape of either series. It also handles the ties a saturated pool produces.
  The shuffle is seeded from the data, so a report is reproducible.
- **The bar rises with the number of series compared.** Testing a dozen
  resources is a dozen chances of a coincidence, so a series has to clear
  `0.05/n`.

The comparison is made **within one measurement**, where the arrival rate is
constant. Across rates, every resource rises with load and would correlate with
latency for that reason alone.

`r` is negative when a resource falls as latency rises, which is as worth seeing
as a rise — a cache hit rate collapsing, say.

**It is deliberately hard to appear in.** A series needs at least five windows
holding both a latency reading and one of its own, and the section is absent
rather than padded when nothing qualifies. That has a real cost: six windows
carries little statistical power, so a wide config may show nothing. It is the
honest answer for six data points, and a narrower `analysis.window` buys more of
them — a 60s probe at a 5s window gives twelve pairings instead of six.

The caveat is printed with the table rather than left to the reader to remember:
correlation is not cause. What it offers is a shortlist, not a verdict.

## Configuration

```yaml
name: social-api

target:
  base_url: http://localhost:3000

workload:
  type: http
  requests:
    - name: feed
      method: GET
      path: /feed
      weight: 50
    - name: create_post
      method: POST
      path: /posts
      weight: 10
      body: '{"title":"hello"}'
      headers:
        Content-Type: application/json

run:
  rate: 500          # iterations per time_unit
  duration: 30s
  time_unit: 1s

percentiles: [50, 75, 90, 95, 99, 99.9]

thresholds:
  error_rate:
    max: 0.01        # fraction, 0.01 == 1%
  latency:
    p50: { max: 100ms }
    p95: { max: 500ms }
    p99: { max: 1s }

collectors:          # optional: sample the system under test while it is loaded
  interval: 5s
  prometheus:
    - name: api
      url: http://localhost:9090
      queries:
        - name: cpu
          query: rate(process_cpu_seconds_total{job="api"}[1m])
          unit: cores
```

Unknown config keys are rejected, so typos fail fast.

### Iterations vs requests

k6's arrival-rate executors control **iterations**, not HTTP requests, so
autobench always reports both rates.

A **weighted pool** is the default: each iteration performs exactly one of the
configured requests, chosen at random by weight. One iteration is one HTTP call,
so 500 iterations/s is 500 requests/s.

```yaml
workload:
  requests:
    - name: feed
      method: GET
      path: /feed
      weight: 50
    - name: like
      method: POST
      path: /posts/1/like
      weight: 15
```

A **journey** is an ordered sequence: each iteration performs every step.

```yaml
workload:
  journey:
    - name: browse
      method: GET
      path: /feed
    - name: read
      method: GET
      path: /posts/1
    - name: like
      method: POST
      path: /posts/1/like
```

One iteration is now one call *per step*, so the report shows both rates and the
end-to-end journey latency:

```
  Workload      checkout-journey (4-step journey)
  Rate          100 iterations/1s  (= 400 requests/1s)
  ...
  Requests      2,400  (392.4/s)
  Iterations    600  (98.1/s)

  Latency
    p95       42.3ms

  Journey latency (end to end, all steps)
    avg       125.7ms
    p95       127.8ms

  REQUEST                REQS   ERRORS       avg       p95       p99
  browse                  600        0     0.7ms     1.0ms         -
  read                    600        0    41.4ms    42.5ms         -
  like                    600        0    41.2ms    41.9ms         -
  create                  600        0    41.4ms    42.7ms         -
```

The per-step table is what makes a journey actionable: the aggregate p95 above
says 42ms, but `browse` is 60x faster than the other three steps.

`requests` and `journey` are mutually exclusive — allowing both would make "what
does one iteration do?" ambiguous. Step names tag the metrics, so they must be
unique. `weight` is ignored for journey steps.

`autobench capacity` searches in iterations per second too, and reports the
request rate alongside:

```
  Capacity      487 iterations/s  (= 1,948 requests/s)
```

### VU sizing

Omit `load_generator` and `autobench run` sizes the pool from `run.rate`
(assuming ~100ms per HTTP call, so a 4-step journey gets 4x the pool);
`autobench capacity` re-sizes it per probe from the latency it observes — for a
journey, the end-to-end iteration time, which is what occupies a VU. If k6 runs
out of VUs or drops iterations, autobench detects it and reports
`FAIL_GENERATOR` rather than misreading the load generator as the bottleneck.

## Output

```
╭────────────────────────────────────────────────────────────╮
│  AUTOBENCH v0.1  — fixed-rate run                          │
╰────────────────────────────────────────────────────────────╯

  Target        http://localhost:3000
  Workload      social-api (4 weighted requests)
  Rate          500 iterations/1s
  Duration      30s
  VUs           50 pre-allocated / 200 max

  ──────────────────────────────────────────────────────────
  Requests      14,932  (497.7/s)
  Iterations    14,932  (497.7/s)
  Errors        0  (0.000%)

  Latency
    min        1.2ms
    avg        3.4ms
    p50        3.1ms
    p95        6.8ms
    p99        9.2ms
    p99.9      18.4ms
    max        41.0ms

  ──────────────────────────────────────────────────────────
  RESULT: PASS
```

## Development

```bash
make test    # go test ./...
make vet     # go vet ./...
make build   # build ./bin/autobench
```
