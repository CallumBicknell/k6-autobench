# autobench

An adaptive load-testing orchestrator around [k6](https://k6.io/).

autobench is the **controller**; k6 is the **traffic generator**. The controller
decides the load, spawns k6, parses the machine-readable summary, evaluates your
SLO thresholds and renders the report. k6 stays a dumb, precisely controlled
arrival-rate generator.

> **Status: V1.0** — `autobench run` (fixed rate) and `autobench capacity`
> (adaptive search) over weighted request pools or multi-step journeys, with
> per-endpoint and response-class breakdowns, windowed latency analysis that
> attributes a spike to the request that caused it, resource collectors
> (Prometheus, Docker, system, Kubernetes) that sample a system while it is
> loaded and rank what moved with the latency, and `autobench compare`, which
> turns two recorded runs into a regression gate for CI. JSON and a
> self-contained HTML report throughout. See `PLAN-OUTLINE.md`.

## Install

Requires Go 1.24+ and `k6` on your `PATH` (or set `AUTOBENCH_K6`).

Tested against k6 v0.49.0 and v2.1.0 — a fixed run, a capacity search and
windowed analysis all work on both, so anything in between should be fine.

```bash
go build -o bin/autobench ./cmd/autobench
# or
make build

# install into $(go env GOBIN) or $(go env GOPATH)/bin
go install ./cmd/autobench
```

A release build — `make release`, or the workflow on a `v*` tag — cross-compiles
linux, darwin and windows on amd64 and arm64 into `dist/`, with a `checksums.txt`.
The binaries record their commit and build time:

```bash
autobench version
autobench 1.0.0 (e6ca963, built 2026-10-04T16:12:40Z)
```

Or build the container, which ships the orchestrator and the generator together:

```bash
docker build -t autobench .
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/bench" \
  autobench capacity bench.yaml --json /bench/baseline.json
```

`--user` is needed whenever a result has to land in a mounted directory: the k6
image runs as uid 12345, so without it the container cannot write to a
host-owned mount.

### Platforms

autobench builds and runs on Linux, macOS and Windows (`linux`/`darwin`/`windows`
× `amd64`/`arm64`). Two collectors are narrower than the rest:

| Collector | Linux | macOS | Windows |
| --- | --- | --- | --- |
| `prometheus` | ✓ | ✓ | ✓ |
| `kubernetes` | ✓ | ✓ | ✓ |
| `system` | ✓ | — | — |
| `docker` | ✓ (socket or tcp) | ✓ (socket or tcp) | tcp only |

`system` reads the kernel's own counters from `/proc`, which only Linux has. On
another platform it reports that once, by name, rather than failing three files on
every tick.

`docker` reaches the daemon over a unix socket or a `tcp://` daemon. A Windows
named pipe (`npipe://`) is not a unix socket, and autobench has no named-pipe
dialer, so it refuses it by name instead of failing with a confusing path error.
Point `DOCKER_HOST` at a `tcp://` daemon to use it on Windows.

Everything else — the fixed run, the capacity search, the comparison, the
windowed analysis and the reports — is platform-independent.

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

# Regression gate: did this change make it worse?
autobench compare baseline.json candidate.json
autobench compare baseline.json candidate.json --html compare.html
```

The config path is a positional argument and flags may appear before or after it.
`run` is also available as `fixed`, and `capacity` as `search`.

Exit codes: `0` all thresholds passed · `1` a threshold failed, no capacity was
found, the generator saturated, or a regression was found · `2` usage/runtime
error.

Verdicts: `PASS`, `FAIL_LATENCY`, `FAIL_ERRORS`, `FAIL_GENERATOR`,
`FAIL_NO_CAPACITY`, `FAIL_NO_RESPONSE`, `FAIL_REGRESSION`. A saturated generator
outranks threshold breaches, because a probe that never reached the target rate
says nothing about the system under test.

`FAIL_NO_RESPONSE` outranks everything, including saturation. It means not one
request received an HTTP response: the benchmark never reached the target. That is
not a verdict about the system, and reporting it as a pass would claim a
measurement that did not happen — which is how a benchmark pointed at a dead
service used to go green. The report drops the latency columns rather than print
`0.0ms`, which would read as instant.

**A run with no thresholds configured checks nothing.** autobench does not invent
an SLO, so if you do not say what passing means, nothing can breach and the
verdict is `PASS` — except for `FAIL_NO_RESPONSE` and `FAIL_GENERATOR`, which are
statements about whether the measurement happened at all rather than judgements
about the system. Set at least `thresholds.error_rate`.

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
| `--k6-timeout` | Kill k6 if it runs longer than this (default: the duration plus a grace period) |
| `--json` | Write the full result as JSON |
| `--html` | Write a self-contained HTML report |
| `--out-dir` | Write the config, result, summary and HTML report to a directory |
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

`autobench compare` takes two result files and no config:

| Flag | Description |
| --- | --- |
| `--tolerance-latency` | How much worse a percentile may get before it fails (default `0.10`) |
| `--tolerance-capacity` | How much capacity may drop before it fails (default `0.05`) |
| `--tolerance-errors` | How much the error rate may rise, in absolute terms (default `0.001`) |
| `--json` | Write the comparison as JSON |
| `--html` | Write a self-contained HTML report |
| `--no-color` | Disable coloured output |

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

## Gating a change

A capacity search answers *how much can this system take*. It cannot answer *did
this change make it worse*, because that is a question about two runs.
`autobench compare` is that question, and it is the reason to run autobench in a
pipeline at all.

```bash
autobench capacity bench.yaml --json baseline.json   # on main
autobench capacity bench.yaml --json candidate.json  # on the PR
autobench compare baseline.json candidate.json
```

```text
  CAPACITY

  METRIC            BASELINE   CANDIDATE      CHANGE
  capacity               675         621       -8.0%   REGRESSION

  LATENCY

  METRIC            BASELINE   CANDIDATE      CHANGE
  p95                  3.0ms       3.9ms      +30.1%   REGRESSION

  ERRORS

  METRIC            BASELINE   CANDIDATE      CHANGE
  error rate          0.000%      0.000%    +0.000pp   ok

  REQUESTS

  REQUEST               BASELINE p95 CANDIDATE p95      CHANGE
  root                         3.0ms         3.9ms      +30.1%   REGRESSION

  RESULT: FAIL_REGRESSION

  2 metrics regressed: capacity, p95

  a single run against a single run is a noisy measurement;
  narrow the tolerance only once you know your noise floor
```

(The `REQUESTS` block appears when both runs recorded a per-request breakdown,
and leads with the request that regressed.)

Both files come from `--json` on either command, and must be the same kind of
run. Latency is compared percentile by percentile, errors in absolute terms, and
capacity only between two searches — one measurement is not a capacity.

### Direction is per metric

A latency rise is bad, a capacity drop is bad, an error rise is bad. autobench
reports the signed change *and* the verdict, because a bare `+56.8%` invites
reading a regression as good news.

### What counts as a regression

| Metric | Judged on | Default tolerance |
| --- | --- | --- |
| Latency percentiles | the relative change | 10% worse |
| Capacity | the relative change | 5% lower |
| Error rate | the **absolute** rise | +0.1 percentage points |
| Per-request p95 | the relative change | 10% worse |

The error rate is the exception, and deliberately so. Going from 0.1% to 0.2% has
doubled, but the doubling is not the alarming part, and a relative tolerance
would treat a trivial rate as a crisis.

The defaults are loose on purpose: a load test is a noisy measurement, and a gate
that fires on run-to-run variation is a gate people turn off. Narrow them once
you know your system's noise floor.

### A comparison says when it is not like for like

Different targets, different rates, a fixed run against a search, a run whose
generator saturated — each produces a note rather than a quiet answer. A
comparison of two different things is worse than no comparison, because it looks
like one.

A request only one of the runs exercised is reported as added or removed, not as
a regression: that is a change in the workload, not in the system.

### In CI

`examples/github-actions.yml` is a starting point. The shape:

1. Search on the PR branch, writing `--json candidate.json`.
2. Compare against `benchmarks/baseline.json`, committed on the default branch.
3. Fail the job on a regression, and upload the HTML report either way.
4. On the default branch, refresh the baseline.

Committing the baseline is the point: the number a PR is judged against is a
reviewed change, not whatever the last pipeline happened to leave behind.

If the baseline is missing the job warns rather than passing silently, so a fresh
repository is not broken and a missing gate is not mistaken for a passing one.

`--json` on `compare` writes the comparison itself, so a CI job can archive one
artefact and still know which two files it compared.

### The latency knee and the operating range

A capacity is where the system breaks. It is usually not where you want to run:
by the time a curve reaches its breaking point the latency has already collapsed.
`operating_policy` turns the measurement into a rate to run at.

```yaml
operating_policy:
  max_utilization_of_capacity: 0.8   # 80% of the measured capacity
  knee_percentile: 99                # optional, default 99
  knee_latency_multiple: 2           # optional, default 2
```

```text
  Sustained     12,000 iterations/s
  Capacity      14,000 iterations/s

  Latency knee  10,000/s  (p99 still within 2× the 20.0ms seen at 2,000/s)
  Policy limit  11,200/s  (80% of measured capacity)
  Operating range < 10,000/s
    set by the latency knee
```

**The knee is the highest passing rate whose latency was still within
`knee_latency_multiple` times the latency at the lowest passing rate.** It is
measured against the lowest *rate's* reading rather than the lowest reading
anywhere on the curve, so a dip partway up cannot move the line.

Both halves of that are choices, which is why both are configurable. The outline
is explicit that a recommendation must not be a hidden judgement
(§24), so the report shows the arithmetic — you can see which rate the floor came
from and check it against the curve above.

**The limit is the lower of the two.** Running at 80% of a capacity the system
reached with latency already collapsed is not a recommendation worth making, so a
knee below the policy ceiling wins, and the report says which one bound it.

The knee is reported even when there is none. A flat curve is a good result, not
a missing section:

```text
  Latency knee  none found (no passing rate reached 2× the p99 at the bottom)
```

A capacity that is only a lower bound cannot bound anything from below, so no
policy limit is computed from it, and the report says so rather than staying
silent about a block you configured.

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
to the number of requests — k6 has to stream every sample to disk for it. Measured
against this project's own workload script, that is **about 3 KB per request**: a
ten-minute run at 10,000 iterations/s writes roughly 16 GB. autobench prints an
estimate before a windowed run starts, because the cost is invisible until the
disk fills:

```text
autobench: windowed analysis (analysis.window 10s) streams every sample to disk:
  about 1.2MiB per probe, up to 5 probes
```

Windows with no traffic are reported as empty rather than skipped, and if a window
holds more samples than are retained its percentiles come from a prefix and the
report says so.

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

Collectors sample a system's own metrics while it is loaded, from outside it, and
the report puts them next to the latency they caused. There are four kinds, and
one run can use any of them:

| Kind | Reads | Needs |
| --- | --- | --- |
| `prometheus` | whatever PromQL can express | a Prometheus server |
| `docker` | a container's CPU and memory | the Docker socket |
| `system` | the host autobench runs on | Linux `/proc` |
| `kubernetes` | a workload's pod CPU and memory | cluster API access |

```yaml
collectors:
  interval: 5s

  prometheus:
    - name: pg
      url: http://localhost:9090
      queries:
        - name: connections
          query: pg_stat_activity_count{job="postgres"}

  docker:
    - name: worker
      container: my-worker
      socket: /var/run/docker.sock   # defaults to $DOCKER_HOST, then /var/run/docker.sock

  system:
    - name: generator

  kubernetes:
    - name: cluster-api
      url: https://127.0.0.1:6443
      token: ""                      # defaults to $AUTOBENCH_K8S_TOKEN, then a mounted service account token
      namespace: bench
      selector: app=api
      insecure: true                 # or ca_file: /path/to/ca.pem, not both
```

```
  SYSTEM (sampled every 3s)

  SERIES                          MIN        MAX        AVG   PEAK AT
  pg connections                  100      167.5      133.8       45s
  worker cpu                    38.9%      40.0%      39.9%        3s
  worker memory               61.2MiB   128.7MiB    94.9MiB       45s
  generator cpu                 38.8%      66.9%      55.9%   101.2ms
  generator memory            16.5GiB    16.9GiB    16.7GiB       15s
  generator load                 3.22       5.15      3.991       45s
  cluster-api cpu (cores)      0.2466     0.6067     0.4267       45s
  cluster-api memory         203.3MiB   383.3MiB   293.3MiB       45s
```

Every series records the expression or endpoint it came from — a PromQL string, a
Docker URL, a `/proc` file, a metrics API URL — so a report can be reproduced by
hand.

`PEAK AT` is measured from the start of the measurement, on the same timeline the
window labels use. A series that never moved shows a dash: a container pinned at
40% CPU has no peak to point at.

### What each kind is actually for

- **`prometheus`** covers the most ground: whatever the system under test already
  exports is reachable through it. Use it when there is monitoring in place.
- **`docker`** is for the case Prometheus cannot cover — a target with no
  monitoring installed at all. Everything Docker runs already exports CPU and
  memory. CPU uses the daemon's own documented formula, and memory is reported
  the way the `docker` CLI reports it: usage minus the reclaimable page cache the
  kernel counts but the container does not really hold.
- **`kubernetes`** is for a cluster with no Prometheus: the metrics API is what
  `kubectl top` reads. Usage is summed across the selected pods, because pod names
  are generated and mean nothing in a report. Per-pod detail is what the
  Prometheus collector is for.
- **`system`** is different from the other three and worth being clear about: it
  reads the **host autobench itself runs on**. If the system under test runs
  elsewhere these readings say nothing about it; if it runs on this machine the
  two are the same host and the report cannot separate them. What they are
  unambiguously good for is the question a saturation verdict raises — was the
  *generator*, rather than the target, the thing that ran out?

### Units

Prometheus and Kubernetes values are unitless or bare numbers, so the
configuration says what they mean where it can: `bytes` is rendered in binary
units and `percent` with a sign, and any other unit is attached to the series
name (`cluster-api cpu (cores)`). Keeping units out of the value columns is what
keeps a table of mixed units aligned.

### Collectors never fail a run

A collector is advisory. If Prometheus is unreachable, a query has a typo, a
container is not running, or the cluster refuses the token, the report says so and
the run carries on:

```
  ⚠ COLLECTOR PROBLEMS — readings missing, the run is unaffected
    down: cpu: Get "http://127.0.0.1:19999/api/v1/query?query=up": dial tcp 127.0.0.1:19999: connect: connection refused
    worker: my-worker: 404 Not Found: {"message":"No such container: my-worker"}
```

The verdict is untouched. Your monitoring being down says nothing about your
capacity, and a load test that errors because Prometheus is unavailable would be
worse than useless. Each failure is reported once, however many polls hit it, and
one broken reading never costs the others: a container whose CPU the daemon
cannot compute still reports its memory.

Mistakes that are visible without asking the server — a URL with no host, a
duplicate name, a container with no name, a cluster setting both `insecure` and
`ca_file` — are rejected at configuration time instead.

Source names must be unique across every kind: a name labels every series it
produces, so two sources sharing one would produce report rows that cannot be
told apart.

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
  request_timeout: 10s   # optional; k6's own default is 60s
  # max_redirects: 0     # optional; k6's own default is 10
  tls:                   # optional; for a target the system roots do not cover
    insecure: true       # accept any certificate
    # ca_file: /etc/ssl/private-ca.pem   # or verify against a bundle
    # cert_file: /etc/ssl/client.pem    # or present a client certificate

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

collectors:          # optional: sample a system's own metrics while it is loaded
  interval: 5s
  prometheus:
    - name: pg
      url: http://localhost:9090
      queries:
        - name: connections
          query: pg_stat_activity_count{job="postgres"}
  docker:
    - name: worker
      container: my-worker
  system:
    - name: generator

operating_policy:    # optional: turn the capacity into a rate to run at
  max_utilization_of_capacity: 0.8
  knee_percentile: 99          # optional, default 99
  knee_latency_multiple: 2     # optional, default 2
```

Unknown config keys are rejected, so typos fail fast.

### Benchmarking a staging target

Staging environments very commonly present a certificate the system roots do not
know, and k6 verifies by default — so without these two settings every request
fails at the handshake and the report says the target never answered.

```yaml
target:
  base_url: https://staging.internal
  tls:
    ca_file: /etc/ssl/private-ca.pem   # verify against a bundle
    # insecure: true                   # or turn verification off entirely
    # cert_file: /etc/ssl/client.pem   # if the target authenticates the caller
    # key_file:  /etc/ssl/client.key
```

`ca_file` is the better option where the CA is known: it tells autobench what to
trust rather than turning verification off. It is passed to k6 as `SSL_CERT_FILE`,
which is how k6 itself reads a custom trust store, so it *replaces* the system
roots for the run. A path that does not exist is caught before the run starts,
because the alternative is an empty trust store and a failure that names the
certificate rather than the typo.

`cert_file` and `key_file` are a client certificate, for a target that
authenticates *the caller* — mTLS. Both must be given together, and both are
checked before the run. The certificate is mapped to the target's host, which
autobench takes from `base_url`: k6 keys a client certificate by domain, so a
target reached by a different name would not present it.

`request_timeout` bounds one HTTP request. k6's own default is 60s, which is far
longer than a probe: a target that hangs makes the probe last a minute rather than
the duration it was configured for, so the run measures the hang instead of the
load. Set it below `probe_duration`.

`max_redirects` is how many redirects one request may follow. k6's own default is
10, and following a chain measures the whole chain; setting it to `0` measures the
redirect itself, reporting the 3xx as the response. Unset means k6's default, so
zero and absent are different settings:

```yaml
target:
  base_url: http://service-with-a-301
  max_redirects: 0
```

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
make test     # go test ./...
make vet      # go vet ./...
make build    # build ./bin/autobench
make image    # build the container
make release  # cross-compile the six release binaries into dist/
```

`make test` runs the suite `-race`-clean. The suite drives real subprocesses for
the parts that are about subprocesses — the k6 timeout and mTLS tests run the real
k6 against a local server, because the bugs they exist for lived in the
interaction between the embedded script and the environment autobench builds, and
a unit test of either half would have passed.

`make release` writes `autobench_<version>_<os>_<arch>` for linux, darwin and
windows on amd64 and arm64, plus `checksums.txt`. The version in the filename is
read from the source constant, so a filename cannot disagree with what the binary
reports. The build records its commit and time:

```text
$ ./dist/autobench_1.0.0_linux_amd64 version
autobench 1.0.0 (e6ca963, built 2026-10-04T16:12:40Z)
```

A plain `go build` leaves those out and reports just `autobench 1.0.0`, rather
than inventing a build identity it does not have.

Pushing a `v*` tag runs `.github/workflows/release.yml`, which tests, runs
`make release` and attaches the artefacts to the release.
