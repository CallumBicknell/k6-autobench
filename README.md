# autobench

An adaptive load-testing orchestrator around [k6](https://k6.io/).

autobench is the **controller**; k6 is the **traffic generator**. The controller
decides the load, spawns k6, parses the machine-readable summary, evaluates your
SLO thresholds and renders the report. k6 stays a dumb, precisely controlled
arrival-rate generator.

> **Status: V0.3** — `autobench run` (fixed rate) and `autobench capacity`
> (adaptive search) both report latency, per-endpoint breakdowns, JSON and a
> self-contained HTML report with a rate/latency chart. See `PLAN-OUTLINE.md`.

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
```

Unknown config keys are rejected, so typos fail fast.

### Iterations vs requests

k6's arrival-rate executors control **iterations**, not HTTP requests. In V0.1 an
iteration performs exactly one HTTP call, so `rate` is also the request rate. A
future release will support multi-request user journeys and report the two rates
separately.

### VU sizing

Omit `load_generator` and `autobench run` sizes the pool from `run.rate`
(assuming ~100ms per iteration); `autobench capacity` re-sizes it per probe from
the latency it observes. If k6 runs out of VUs or drops iterations, autobench
detects it and reports `FAIL_GENERATOR` rather than misreading the load
generator as the bottleneck.

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
