> **Implementation status.** This document is the original design outline, kept
> as the reference for what to build. What actually exists:
>
> - **V0.1 — done.** `autobench run`: fixed-rate k6 runner, SLO evaluation,
>   terminal report, JSON output, CI exit codes. (`internal/{config,metrics,results,report,runner}`, `cmd/autobench`.)
> - **V0.2 — done.** `autobench capacity`: exponential bracket → binary search →
>   validation, with per-probe VU sizing, warmup runs, confirmation runs and
>   per-probe artefacts. (`internal/search`, `internal/bench`.)
> - **V0.3 — done.** Endpoint breakdown, `--html` report (self-contained, inline
>   SVG rate/latency chart), `report.html` in the results directory.
>   (`internal/metrics`, `internal/report`.)
> - **V0.4 — done.** Windowed analysis: per-window percentiles from k6's sample
>   stream, `analysis.require_all_windows_pass`, latency-over-time charts.
>   (`internal/metrics`, `internal/results`, `internal/report`.)
> - **V0.5 — done.** Multi-step journeys (`workload.journey`), with iteration rate
>   and request rate reported separately, and end-to-end journey latency.
> - **V0.6 — done.** Response-class breakdown (2xx/3xx/4xx/5xx/other) with a plain
>   reading of what the mix implies.
> - **V0.7 — done.** Per-endpoint windows: each window carries the request
>   breakdown, so a spike is attributed to the request that caused it.
> - **V0.8 — done.** Resource collectors (§33): a Prometheus collector behind a
>   `collectors:` block, sampling the system under test while it is loaded, with
>   the readings reported alongside the latency they caused and the resources
>   that moved with it ranked. (`internal/collect`.)
> - **V0.9 — done.** The rest of §33: Docker, system and Kubernetes collectors on
>   the same abstraction, and the host's own CPU, memory and load — which is
>   generator CPU/memory from §30 as well. (`internal/collect`.)
>
> Still open: nothing from the outline's collector list.
>
> Deviations worth knowing, and why:
>
> - `run` stays the fixed-rate command (V0.1 shipped it that way); the search is
>   `capacity`, with `fixed` and `search` as aliases. §28/§29 in this document
>   fold the two into one command.
> - Validation is opt-in rather than on by default, so a flag-driven search stays
>   fast and a config can express "skip it" unambiguously.
> - Generator-saturation retries (`search.max_vu_growth`, §18) are opt-in: the
>   default is to abort with advice rather than silently spawn thousands of VUs
>   on a shared machine.
> - `validation.window` / `require_all_windows_pass` (§14) are decoded but not yet
>   used; they belong with time-windowed percentiles (§31), which are still to do.
> - `validation.window` and `validation.require_all_windows_pass` (§32) moved to
>   an `analysis:` block. They apply to *every* measurement, not just the
>   validation run, and having one window size under two names would have been
>   worse than the rename.
> - Endpoint breakdowns rely on k6 emitting a tagged submetric per request name,
>   which it only does when a threshold selects it, so the workload script
>   declares always-satisfied thresholds (§17 in `k6/runner.js`).

---


The key idea:

> **Your tool controls k6 → k6 generates a precisely controlled arrival rate → your tool reads the results → decides pass/fail → changes the rate → repeats until it finds the sustainable ceiling → then runs a long validation test.**

k6 already gives you the primitives you need: arrival-rate executors, HTTP/error/duration metrics, arbitrary percentile summaries, thresholds, and machine-readable summaries. ([Grafana k6][1])

---

# 1. What I'd build

I'd call the project something like:

```text
k6-autobench
```

CLI:

```bash
autobench run bench.yaml
```

Output:

```text
╭──────────────────────────────────────────────────────────────╮
│                    AUTOBENCH v0.1                            │
│                    Adaptive Capacity Test                    │
╰──────────────────────────────────────────────────────────────╯

Target
  http://165.227.86.235

Search
  Starting rate       5,000 req/s
  Initial duration    2m
  Validation          5m
  Error threshold     1%
  p50 threshold       100ms
  p95 threshold       500ms
  p99 threshold       1s

───────────────────────────────────────────────────────────────

  Probe 1     5,000 req/s       PASS
  Probe 2     7,500 req/s       PASS
  Probe 3    11,250 req/s       FAIL
  Probe 4     9,375 req/s       PASS
  Probe 5    10,312 req/s       FAIL
  Probe 6     9,844 req/s       PASS
  Probe 7    10,078 req/s       PASS
  Probe 8    10,195 req/s       FAIL

───────────────────────────────────────────────────────────────

  CAPACITY SEARCH

  Passing rate     10,078 req/s
  Failing rate     10,195 req/s
  Resolution           117 req/s

  estimated capacity: ~10.1k req/s

───────────────────────────────────────────────────────────────

  Starting 5 minute validation...

  0:00 ████████████████████████████████████ 10,078 req/s
  1:00 ████████████████████████████████████ 10,078 req/s
  2:00 ████████████████████████████████████ 10,078 req/s
  ...
```

Then:

```text
╭──────────────────── FINAL RESULT ────────────────────╮

  Capacity
    sustainable RPS       10,078
    tested RPS            10,078
    validation duration   5m

  Requests
    total                 3,023,400
    errors                0
    error rate            0.000%

  Latency
    p50                   18.2ms
    p75                   24.1ms
    p90                   31.7ms
    p95                   42.8ms
    p99                   81.4ms
    p99.9                 164.2ms
    max                   812ms

  RESULT: PASS

╰──────────────────────────────────────────────────────╯
```

---

# 2. The important architectural decision

I'd make **your application the controller**.

Not:

```text
k6 script
  └── tries to figure out the next RPS
```

Instead:

```text
                    ┌──────────────────┐
                    │   autobench CLI  │
                    │                  │
                    │ search algorithm  │
                    │ thresholds        │
                    │ reporting         │
                    └────────┬─────────┘
                             │
                             │ spawn k6
                             ▼
                    ┌──────────────────┐
                    │       k6         │
                    │                  │
                    │ constant arrival │
                    │ HTTP workload    │
                    └────────┬─────────┘
                             │
                             ▼
                    ┌──────────────────┐
                    │       API        │
                    └──────────────────┘
```

That gives you a very clean separation.

**autobench**

* decides load
* starts/stops tests
* parses results
* determines pass/fail
* performs binary search
* stores results
* renders UI
* generates reports

**k6**

* generates traffic
* measures HTTP performance
* handles VUs
* collects metrics

This is also aligned with how k6 is designed: `constant-arrival-rate` lets you generate a fixed iteration rate independently of SUT response time. ([Grafana Labs][2])

---

# 3. The core algorithm

This is the most interesting part.

You don't actually want a simple binary search.

You have a problem where the upper limit is initially unknown.

Suppose:

```text
starting = 5,000 RPS
```

You don't know whether the system can handle:

```text
6k
10k
20k
50k
```

So I'd use **three phases**.

### Phase A — establish a bracket

### Phase B — binary search

### Phase C — validation

---

# 4. Phase A — find the failure boundary

Start:

```text
5,000
```

If it passes, increase aggressively.

For example:

```text
5,000
7,500
11,250
16,875
25,312
```

using:

```text
next = current × 1.5
```

until something fails.

Imagine:

```text
5,000    PASS
7,500    PASS
11,250   PASS
16,875   FAIL
```

Now you know:

```text
LOW  = 11,250
HIGH = 16,875
```

Your capacity lies somewhere in that interval.

This is much better than arbitrarily choosing:

```text
10k → 11k → 12k → ...
```

because you can discover wildly different systems.

---

# 5. Phase B — binary search

Now:

```text
LOW  = 11,250
HIGH = 16,875
```

Try:

```text
MID = (11250 + 16875) / 2
    = 14062
```

Suppose:

```text
14,062 FAIL
```

Then:

```text
LOW  = 11,250
HIGH = 14,062
```

Next:

```text
12,656
```

Suppose PASS.

Then:

```text
LOW  = 12,656
HIGH = 14,062
```

Continue:

```text
13,359
```

PASS

```text
13,710
```

FAIL

```text
13,534
```

PASS

etc.

Eventually:

```text
PASS: 13,534
FAIL: 13,548
```

Now your resolution is:

```text
14 RPS
```

You don't actually need to get this absurdly precise, though.

I'd make the user specify:

```yaml
search:
  resolution: 100
```

Then stop when:

```text
high - low <= 100
```

---

# 6. Don't call it "exact capacity"

This is important.

You can't really say:

> The server can handle exactly 13,534 RPS.

Performance is noisy.

You could have:

```text
13,500 → PASS
13,600 → FAIL
```

because of:

* GC
* database contention
* CPU scheduling
* network jitter
* cache state
* background processes
* load-generator noise
* connection pooling
* transient queueing

So I'd report:

```text
Sustainable capacity

13,500 RPS
±100 RPS search resolution
```

or:

```text
PASSING RATE     13,500
FAILING RATE     13,600
SEARCH RESOLUTION   100
```

That's much more scientifically defensible.

---

# 7. Your pass/fail system

This should be configurable.

For example:

```yaml
thresholds:
  error_rate:
    operator: "<="
    value: 0.01

  latency:
    p50:
      operator: "<="
      value: 100ms

    p95:
      operator: "<="
      value: 500ms

    p99:
      operator: "<="
      value: 1000ms
```

A test passes only if:

```text
error_rate <= 1%
AND
p50 <= 100ms
AND
p95 <= 500ms
AND
p99 <= 1000ms
```

k6 supports thresholds for exactly this kind of SLO-style pass/fail condition. ([Grafana Labs][3])

But I'd have **your tool evaluate the results too**, rather than relying exclusively on k6 thresholds.

Why?

Because you want to know *why* it failed.

For example:

```text
FAIL

Reason:
  p99 exceeded threshold

Observed:
  p99 = 1,482ms
  limit = 1,000ms

Other metrics:
  error rate = 0.03%
  p50 = 43ms
  p95 = 612ms
```

---

# 8. Get ALL the percentiles

I'd make these configurable:

```yaml
percentiles:
  - 50
  - 75
  - 90
  - 95
  - 99
  - 99.9
  - 99.99
```

Which produces:

```text
p50
p75
p90
p95
p99
p99.9
p99.99
```

k6 supports arbitrary Trend percentile statistics such as `p(95)`, `p(99)`, and `p(99.99)`. ([Grafana Labs][4])

I'd probably default to:

```text
p50
p75
p90
p95
p99
p99.9
max
```

---

# 9. Don't only measure aggregate latency

This is where your tool could become **much better than a basic k6 wrapper**.

Suppose your workload does:

```text
GET /feed
GET /posts/:id
POST /posts/:id/like
POST /posts
```

Your final report should show:

```text
OVERALL

p50     15.4ms
p95    689.5ms
p99    934.1ms


ENDPOINT

GET /feed
  p50     16.5ms
  p95    695.6ms
  p99    942.7ms

GET /posts/:id
  p50     14.9ms
  p95    685.3ms
  p99    922.6ms

POST /posts/:id/like
  p50     15.3ms
  p95    681.2ms
  p99    931.0ms

POST /posts
  p50     15.4ms
  p95    662.5ms
  p99    894.9ms
```

That's essentially what your screenshot is doing.

**Implemented, including over time.** V0.7 carries the same per-request breakdown
into every window, so the report can say which request caused a spike rather than
only that one happened.

---

# 10. Workload definition

I'd make the benchmark itself configurable.

Something like:

```yaml
name: social-api

target:
  base_url: http://165.227.86.235

workload:
  requests:
    - method: GET
      path: /feed
      weight: 50

    - method: GET
      path: /posts/:id
      weight: 25

    - method: POST
      path: /posts/:id/like
      weight: 15

    - method: POST
      path: /posts
      weight: 10
```

Then:

```text
5000 RPS
```

means approximately:

```text
2500 /feed
1250 /posts/:id
 750 /posts/:id/like
 500 /posts
```

This is much more useful than hammering one endpoint.

---

# 11. Very important: RPS vs iterations

**Implemented.** `workload.journey` makes an iteration perform every step, and
the report states both rates: `100 iterations/1s (= 400 requests/1s)`.

You need to define this carefully.

k6's arrival-rate executor actually controls **iterations per time unit**, not directly HTTP requests. ([Grafana Labs][2])

If one iteration does:

```javascript
http.get("/feed");
http.get("/posts/1");
http.post("/posts/1/like");
```

then:

```text
5,000 iterations/s
```

could generate:

```text
15,000 HTTP requests/s
```

Therefore I'd make your tool distinguish:

```text
iteration rate
```

and:

```text
HTTP request rate
```

For a simple benchmark:

```text
1 iteration = 1 HTTP request
```

Then:

```text
5000 iterations/s = 5000 HTTP RPS
```

For a realistic user journey:

```text
1 iteration = multiple HTTP requests
```

then your report says:

```text
Target iteration rate: 5,000/s
HTTP request rate:     ~11,800/s
```

This distinction is extremely important.

---

# 12. The probe duration

I'd use two durations.

### Search probe

Something like:

```yaml
probe:
  duration: 2m
```

But I'd actually split it internally:

```text
10s warmup
100s measurement
10s cooldown
```

or:

```text
15s warmup
90s measurement
15s cooldown
```

You don't want the first few seconds of connection establishment to dominate the result.

---

# 13. Then your 5-minute validation

Once you've found:

```text
~10,000 RPS
```

don't immediately call it successful.

Run:

```text
10,000 RPS
       │
       ├────────────── 5 minutes ──────────────┐
       │                                       │
       ▼                                       ▼
   collect metrics                        final verdict
```

And ideally make this configurable:

```yaml
validation:
  duration: 5m
  rate: discovered
```

Then your report:

```text
CAPACITY SEARCH

Passing: 10,078
Failing: 10,195


VALIDATION

Rate:       10,078 RPS
Duration:   5m

Requests:   3,023,400
Errors:     0

p50:        18.2ms
p95:        42.8ms
p99:        81.4ms
p99.9:      164.2ms

PASS
```

---

# 14. But I'd add a safety margin

This is one of the most useful features.

Suppose the search discovers:

```text
capacity = 10,000 RPS
```

Running your production-like test at exactly:

```text
10,000
```

isn't necessarily what you want.

I'd support:

```yaml
validation:
  rate: discovered
  safety_margin: 0.90
```

So:

```text
10,000 × 0.90
=
9,000 RPS
```

Then:

```text
Estimated capacity: 10,000 RPS

Validation rate:     9,000 RPS
Safety margin:          10%
```

That gives you:

> "We sustained 9k RPS for five minutes while the estimated failure boundary was ~10k."

Much more useful operationally.

---

# 15. Handle flaky tests

This is another area I'd improve over a naive binary search.

Imagine:

```text
10,000 PASS
10,500 FAIL
10,250 PASS
10,375 FAIL
10,312 PASS
```

Good.

But suppose:

```text
10,000 PASS
10,500 FAIL
10,250 FAIL
10,125 PASS
```

That could simply be noise.

So I'd introduce:

```yaml
search:
  confirmation_runs: 2
```

Meaning:

> A rate must pass twice before being considered reliably passing.

And perhaps:

```yaml
search:
  failure_confirmation: 2
```

So:

```text
10,500
  run #1 FAIL
  run #2 FAIL

=> definitively failed
```

Whereas:

```text
10,500
  run #1 FAIL
  run #2 PASS

=> inconclusive
=> retry
```

---

# 16. Even better: classify the failure

Don't just have:

```text
PASS
FAIL
```

Have:

```text
PASS
FAIL_LATENCY
FAIL_ERRORS
FAIL_GENERATOR
FAIL_TIMEOUT
INCONCLUSIVE
```

For example:

```text
10,500 RPS

FAIL_LATENCY

p99:
  1,482ms > 1,000ms

error rate:
  0.02% < 1%

generator:
  healthy
```

Or:

```text
FAIL_GENERATOR

Target:
  10,500 RPS

Achieved:
  8,942 RPS

Reason:
  maxVUs exhausted
```

That last one is **critical**.

---

# 17. k6 generator saturation

This is one of the biggest traps in what you're proposing.

Arrival-rate executors need enough VUs to actually sustain the requested arrival rate. k6 dynamically uses VUs to maintain the rate, but `preAllocatedVUs` and `maxVUs` constrain that ability. ([Grafana Labs][5])

Imagine you ask for:

```text
20,000 RPS
```

but your load generator can only produce:

```text
14,000 RPS
```

because you've run out of VUs.

You **must not conclude**:

> Server capacity = 14k RPS.

The benchmark itself became the bottleneck.

So your tool should detect:

```text
target rate
actual iteration rate
dropped iterations
VUs
max VUs
```

and say:

```text
⚠ GENERATOR SATURATION

Requested: 20,000 RPS
Achieved:  14,231 RPS

maxVUs reached.

Probe discarded.
```

**Implemented**, along with the sibling case this section does not name. A probe
that never reached the target rate is discarded because it says nothing about the
target; a run where *not one request received an HTTP response* is the same kind
of statement, and was added in V1.0 as `FAIL_NO_RESPONSE`.

It came from a real failure: with no error threshold configured, a capacity search
against a dead address grew to `max_probes` and reported `Capacity ≥ 1,600/s`,
`RESULT: PASS`, exit 0 -- while printing that 100% of responses had no HTTP
status and showing `p95 0.0ms` for a target that never answered. Two things were
wrong. A pass claimed a measurement that had not happened, which is a false green
in a pipeline. And `0ms` reads as instant, which is the opposite of the truth:
k6 records a duration of zero for a request that never got a response, and the
report now drops those samples rather than passing them on.

It outranks generator saturation, because a target that hangs produces both and
the cause is the more useful answer: telling someone to raise `max_vus` when
their service is not answering sends them the wrong way. The two diagnoses are
kept apart at the search level too -- `ErrNoResponses` rather than
`ErrNoPassingRate`, since "lower your start rate" is the wrong advice for a
target that was never reached.

---

# 18. Automatically calculate VUs

You could even have:

```yaml
load_generator:
  vus: auto
```

Then estimate:

```text
required VUs ≈ target RPS × expected iteration duration
```

For example:

```text
10,000 RPS
×
0.1 sec
=
1,000 concurrent VUs
```

Then add a safety factor:

```text
1,000 × 1.5
=
1,500 VUs
```

And let k6 use:

```javascript
preAllocatedVUs: 1500,
maxVUs: 3000
```

The actual required number depends on workload latency, so this should be an adaptive estimate rather than a hard mathematical guarantee. k6 itself notes that VU requirements depend on how long the iteration takes. ([Grafana Labs][5])

---

# 19. Your k6 script

You could keep the k6 side surprisingly small.

Something roughly like:

```javascript
import http from "k6/http";

export const options = {
    scenarios: {
        workload: {
            executor: "constant-arrival-rate",

            rate: Number(__ENV.RATE),
            timeUnit: "1s",

            duration: __ENV.DURATION,

            preAllocatedVUs: Number(__ENV.PRE_ALLOCATED_VUS),
            maxVUs: Number(__ENV.MAX_VUS),
        },
    },

    summaryTrendStats: [
        "avg",
        "min",
        "med",
        "max",
        "p(50)",
        "p(75)",
        "p(90)",
        "p(95)",
        "p(99)",
        "p(99.9)",
    ],
};
```

Then:

```javascript
export default function () {
    // execute configured workload
}
```

And:

```javascript
export function handleSummary(data) {
    return {
        stdout: JSON.stringify(data),
        "result.json": JSON.stringify(data),
    };
}
```

k6's `handleSummary()` gives you access to the aggregated summary object, which makes it suitable for producing machine-readable output for your controller. ([Grafana Labs][6])

---

# 20. Your controller

This is where the real project lives.

I'd probably use **Go**.

You'd have:

```text
autobench
│
├── cmd/
│   └── autobench/
│       └── main.go
│
├── internal/
│   ├── runner/
│   │   └── k6.go
│   │
│   ├── search/
│   │   ├── exponential.go
│   │   ├── binary.go
│   │   └── controller.go
│   │
│   ├── metrics/
│   │   ├── parser.go
│   │   ├── percentile.go
│   │   └── evaluator.go
│   │
│   ├── workload/
│   │   └── config.go
│   │
│   ├── results/
│   │   ├── model.go
│   │   └── writer.go
│   │
│   └── report/
│       ├── terminal.go
│       ├── json.go
│       └── html.go
│
├── k6/
│   └── runner.js
│
├── examples/
│   └── api.yaml
│
├── go.mod
└── README.md
```

---

# 21. The controller state machine

I'd model the search explicitly.

```text
                    ┌──────────────┐
                    │ INITIAL TEST │
                    └──────┬───────┘
                           │
                           ▼
                    ┌──────────────┐
                    │ EXPONENTIAL  │
                    │    SEARCH    │
                    └──────┬───────┘
                           │
                  failure found
                           │
                           ▼
                    ┌──────────────┐
                    │   BRACKET    │
                    │  low / high  │
                    └──────┬───────┘
                           │
                           ▼
                    ┌──────────────┐
                    │    BINARY    │
                    │    SEARCH    │
                    └──────┬───────┘
                           │
                    resolution reached
                           │
                           ▼
                    ┌──────────────┐
                    │  VALIDATION  │
                    │     5 min    │
                    └──────┬───────┘
                           │
                           ▼
                    ┌──────────────┐
                    │ FINAL REPORT │
                    └──────────────┘
```

---

# 22. Search controller pseudocode

Something like:

```go
func FindCapacity(cfg Config) Result {
    rate := cfg.Search.StartRate

    var low *Probe
    var high *Probe

    // Phase 1: exponential search
    for {
        result := runProbe(rate)

        if result.Pass {
            low = &result
            rate = nextRate(rate)
            continue
        }

        high = &result
        break
    }

    // Phase 2: binary search
    for high.Rate-low.Rate > cfg.Search.Resolution {
        mid := (low.Rate + high.Rate) / 2

        result := runProbe(mid)

        if result.Pass {
            low = &result
        } else {
            high = &result
        }
    }

    // Phase 3: validation
    validationRate := calculateValidationRate(
        low.Rate,
        cfg.Validation.SafetyMargin,
    )

    validation := runValidation(validationRate)

    return Result{
        PassingRate: low.Rate,
        FailingRate: high.Rate,
        Validation:  validation,
    }
}
```

That's basically the heart of the product.

---

# 23. But I'd improve that algorithm further

Have the search algorithm configurable.

### Conservative

```text
+10%
```

### Aggressive

```text
×1.5
```

### Doubling

```text
×2
```

And then:

```yaml
search:
  strategy: adaptive
```

Your adaptive algorithm could observe the shape of the latency curve.

For example:

```text
RPS       p99

2k        20ms
4k        23ms
6k        25ms
8k        31ms
10k       40ms
12k       91ms
14k       420ms
15k       1.8s
```

You can see the system is approaching a knee.

You could detect that.

---

# 24. The "knee" is actually very interesting

I'd eventually make the tool report:

```text
Capacity:
  ~14,200 RPS

Latency knee:
  ~12,500 RPS

Recommended operating range:
  < 11,000 RPS
```

But **I would not use "recommended" as a hidden judgment**. Make it configurable based on an explicit policy, e.g.:

```yaml
operating_policy:
  max_utilization_of_capacity: 0.80
```

Then the tool can mechanically say:

```text
Policy limit:
  80% of measured capacity

Calculated:
  11,360 RPS
```

That's much more defensible.

---

# 25. Track latency as a curve

This would make your reports awesome.

Store every probe:

```json
{
  "rate": 10000,
  "pass": true,
  "metrics": {
    "p50": 18.2,
    "p95": 42.8,
    "p99": 81.4
  }
}
```

Then:

```text
RPS → latency

p99

2000 ───── 20ms
4000 ───── 23ms
6000 ───── 25ms
8000 ───── 31ms
10000 ──── 42ms
12000 ──── 91ms
14000 ──── 420ms
15000 ──── 1.8s
```

You can generate an HTML report with graphs.

---

# 26. Result storage

I'd store each run as:

```text
results/
└── 2026-10-02T23-31-00/
    ├── config.yaml
    ├── manifest.json
    ├── probes/
    │   ├── 005000.json
    │   ├── 007500.json
    │   ├── 011250.json
    │   ├── 009375.json
    │   └── ...
    │
    ├── validation.json
    ├── summary.json
    └── report.html
```

That gives you reproducibility.

**Implemented (V1.0).** `--out-dir` writes exactly this, and `compare` reads it.

You can now say:

```bash
autobench compare baseline.json candidate.json
```

(pointed at the `--json` each run wrote, rather than at the directory, so a
comparison names two files rather than guessing which run inside a tree it
means)

And see:

```text
              BEFORE       AFTER

capacity      10,200       13,500
p50             22ms          18ms
p95             61ms          43ms
p99            143ms          81ms
errors        0.02%         0.00%
```

That's where this starts becoming a genuinely useful engineering tool rather than a wrapper around k6.

---

# 27. Configuration I'd aim for

Something like:

```yaml
name: social-api

target:
  base_url: http://165.227.86.235

workload:
  type: http

  requests:
    - name: feed
      method: GET
      path: /feed
      weight: 50

    - name: post
      method: GET
      path: /posts/123
      weight: 25

    - name: like
      method: POST
      path: /posts/123/like
      weight: 15

    - name: create_post
      method: POST
      path: /posts
      weight: 10

search:
  start_rate: 5000

  increase_factor: 1.5

  resolution: 100

  probe_duration: 2m

  warmup: 15s

  confirmation_runs: 2

thresholds:

  error_rate:
    max: 0.01

  latency:
    p50:
      max: 100ms

    p95:
      max: 500ms

    p99:
      max: 1000ms

percentiles:
  - 50
  - 75
  - 90
  - 95
  - 99
  - 99.9

validation:
  duration: 5m

  safety_margin: 0.9

load_generator:
  pre_allocated_vus: auto
  max_vus: auto
```

Then:

```bash
autobench run bench.yaml
```

---

# 28. I'd also support fixed tests

Don't make it *only* adaptive.

You want:

```bash
autobench run bench.yaml
```

adaptive search.

But also:

```bash
autobench run bench.yaml --rate 5000 --duration 10m
```

for a normal fixed benchmark.

And:

```bash
autobench run bench.yaml --rate 5000 --duration 1h
```

for soak testing.

And eventually:

```bash
autobench run bench.yaml --profile spike
```

or:

```yaml
profile:
  - rate: 1000
    duration: 1m

  - rate: 5000
    duration: 2m

  - rate: 10000
    duration: 2m

  - rate: 5000
    duration: 2m

  - rate: 1000
    duration: 1m
```

k6 already has ramping arrival-rate executors for varying iteration rates over stages. ([Grafana Labs][7])

---

# 29. Eventually I'd add multiple test modes

**Partly implemented.** `capacity` (alias `search`), `fixed` (alias `run`) and
`compare` ship. `ramp`, `spike` and `soak` do not: k6's ramping executors cover
them, and a fixed run at a known rate plus the windowed analysis answers the same
questions without a second mode to keep correct.

Your tool could expose:

```text
autobench
│
├── capacity
│   Find maximum sustainable throughput
│
├── fixed
│   Run at specified RPS
│
├── ramp
│   Gradually increase load
│
├── spike
│   Sudden load increase
│
├── soak
│   Long-duration constant load
│
└── compare
    Compare two benchmark runs
```

CLI:

```bash
autobench capacity bench.yaml
autobench fixed bench.yaml --rate 5000 --duration 10m
autobench soak bench.yaml --rate 8000 --duration 2h
autobench compare run1 run2
```

---

# 30. Metrics I'd collect

**Implemented.** Traffic, latency percentiles, generator state, the per-endpoint
breakdown and the error breakdown by status class all land in the report, and
§9's "everything above per endpoint" extends to the windowed view (V0.7).
Generator CPU and memory arrived in V0.9 as the `system` collector, which reads
the host autobench itself runs on: CPU, memory and load. It is the one collector
whose subject is the *generator* rather than the target, and the report says so.

At minimum:

### Traffic

```text
target RPS
actual RPS
iterations
HTTP requests
```

### Errors

```text
error count
error rate
HTTP 2xx
HTTP 3xx
HTTP 4xx
HTTP 5xx
timeouts
connection failures
```

### Latency

```text
min
avg
p50
p75
p90
p95
p99
p99.9
max
```

### Load generator

```text
VUs
max VUs
dropped iterations
CPU
memory
```

### Endpoint

Everything above per endpoint.

k6 already provides the core HTTP request, failure, and duration metrics, and can export time-series data when you want more granular analysis. ([Grafana k6][1])

---

# 31. One thing I'd add that the screenshot doesn't show

**Implemented.** `analysis.window: 10s`, reported as a latency-over-time table and
chart in both the terminal and HTML reports.

**Time-windowed percentiles.**

Overall:

```text
p99 = 800ms
```

can hide a serious problem.

Suppose:

```text
0-30s     p99 40ms
30-60s    p99 45ms
60-90s    p99 55ms
90-120s   p99 1,200ms
```

Overall you might still get:

```text
p99 = 70ms
```

depending on distribution.

So for each probe I'd collect:

```text
0-10s
10-20s
20-30s
...
```

Then report:

```text
LATENCY OVER TIME

p99

40ms ───────────────────
50ms ───────────────────
60ms ───────────────────
                       ╲
                        ╲
                         900ms
```

This is how you detect systems that **look fine initially but collapse under sustained load**.

---

# 32. The validation should therefore be stricter

**Implemented**, as `analysis.require_all_windows_pass` (see the note at the top
of this document for why it is not under `validation:`).

Instead of simply:

```text
5-minute average passes
```

I'd make validation require:

```text
overall thresholds pass
AND
every measurement window passes
AND
no generator saturation
```

For example:

```yaml
validation:
  duration: 5m

  window: 10s

  require_all_windows_pass: true
```

Then a 10-second latency spike isn't hidden by five minutes of averages.

---

# 33. Database/resource metrics later

This is where I'd make the architecture extensible.

Your benchmark currently sees:

```text
client → API
```

But eventually you want:

```text
                     ┌── CPU
                     ├── RAM
                     ├── DB connections
k6 → API → database ─┼── DB CPU
                     ├── Redis
                     └── queue depth
```

You could have collectors:

```text
metrics/
├── k6
├── prometheus
├── docker
├── kubernetes
└── system
```

Then your final report might say:

```text
CAPACITY FAILURE

Rate: 14,500 RPS

p99:
  1.82s

Likely correlated saturation:
  PostgreSQL CPU:       97%
  PostgreSQL connections: 498/500
  API CPU:               63%
  Redis CPU:             42%
```

You're no longer just measuring performance — you're finding **where the bottleneck appears**.

## What V0.8 built

The **Prometheus** collector, because it is a pull endpoint and therefore covers
the most ground for one implementation: whatever the system under test already
exports is reachable through it.

```text
internal/collect/
├── collect.go      Series, Sample, Report, Collector
├── sampler.go      the polling loop every collector shares
├── prometheus.go   instant-query polling on an interval
├── docker.go       the daemon's own stats endpoint
├── system.go       the Linux kernel's counters
└── kubernetes.go   the metrics API `kubectl top` reads
```

`Collector` is deliberately narrow — `Start(ctx)` returns a `stop()` that reports
— and V0.9 filled in the rest of the list above without touching the runner, the
result model or the renderers:

- **Start must not block.** A slow or unreachable endpoint must never delay the
  measurement. Stopping waits briefly for a poll already in flight, because that
  reading is the last one of the measurement.
- **Failures are advisory.** A target's monitoring being unreachable says nothing
  about its capacity, so a collector reports what it could not read and the run
  carries on. Static mistakes (a URL with no host, a duplicate name) are caught at
  configuration time instead.
- **Readings are relative to the measurement.** Each sample carries an offset from
  the start, on the same timeline the window labels use, so a resource peak can be
  read against the latency window it landed in. A remote server's own timestamp is
  ignored on purpose: a clock skew would otherwise shift every reading.

The report gained a `SYSTEM` section showing min, max, average and the peak
offset per series, in both the terminal and the HTML report.

## What V0.9 added

The other three kinds, and the shared machinery that made them cheap to add.

The three collectors each needed the same loop: read immediately so a measurement
shorter than the interval still collects something, then on every tick; stop
without discarding a reading already in flight; accumulate into one report in
configured order; report a broken series once rather than once per poll. That is
now one `sampler` driven by a small `poller` interface, and each collector is only
the part that knows how to read its own system.

Two of them are worth saying something about, because the obvious implementation
of each is wrong:

- **Docker** CPU is `(cpu_delta / system_cpu_delta) * online_cpus * 100`, the
  daemon's own documented formula, which is why the request says `stream=false`:
  that is what makes the daemon include the previous counters the delta needs.
  Memory is usage minus the reclaimable page cache, which is what the `docker` CLI
  shows — the kernel counts that cache as usage and the container does not really
  hold it.
- **Kubernetes** returns its quantities as strings with suffixes: `12345678n` is
  0.012345678 cores. There is a parser for the documented suffix set, and an
  unknown suffix is an error rather than a guess, because reading `5Z` as 5 would
  report a number that is not there.

`system` is the one to be careful about. It reads `/proc` on the host autobench
runs on, so it is measuring the **load generator**, not the target. If the system
under test is elsewhere the readings say nothing about it; if it is on the same
machine the two cannot be separated. What it is unambiguously good for is the
question a saturation verdict raises — was the generator, rather than the target,
the thing that ran out? That is §30, and this covers it.

## What V0.8 added to it

The "likely correlated saturation" line above, built honestly. Rather than
inventing a heuristic, the report computes the rank correlation between each
collected series and the per-window latency, and shows the series that survive a
permutation test:

```text
  LIKELY CORRELATED (resource vs p95, strongest first)

  SERIES                          r   WINDOWS
  postgres connections        +0.97         9
```

What makes that a claim worth reading:

- **Within one measurement.** The arrival rate is constant there, so a resource
  cannot correlate with latency merely by rising with load. Across rates, every
  resource would.
- **Rank correlation.** A resource that climbs 20% while latency triples is still
  moving together; one bad window would otherwise dominate a product-moment
  correlation.
- **A permutation test.** A handful of windows is where the normal approximation
  is worst, and shuffling assumes nothing about either series. It handles the
  ties a saturated pool produces, which the textbook rank-correlation tables do
  not. The shuffle is seeded from the data, so a report stays reproducible.
- **A bar that rises with the number of series compared**, at `0.05/n`. Testing a
  dozen resources is a dozen chances of a coincidence.

It is also deliberately hard to appear in: a series needs at least five windows
holding both readings, and the section is absent rather than padded when nothing
qualifies. Six windows carries little power, so a wide config may show nothing at
all — the honest answer for six data points, and a narrower `analysis.window`
buys more of them. The caveat that correlation is not cause is printed with the
table.

Docker, Kubernetes and system collectors are still to come, on this abstraction.

---

# 34. HTML dashboard

I'd definitely build this.

Something like:

```text
┌──────────────────────────────────────────────────────────────┐
│ API CAPACITY REPORT                                          │
│ 2026-10-02 23:31                                             │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  CAPACITY                                                   │
│                                                              │
│       13.5k RPS                                             │
│       sustainable                                           │
│                                                              │
│  ─────────────────────────────────────────────────────────  │
│                                                              │
│  RPS vs p99                                                 │
│                                                              │
│  2s │                                      ●                │
│     │                                  ●                    │
│  1s │                              ●                        │
│     │                          ●                            │
│500ms│                    ●                                  │
│     │               ●                                      │
│100ms│ ●──●──●──●──●                                        │
│     └──────────────────────────────────────────────         │
│       2k  4k  6k  8k  10k 12k 14k 16k                    │
│                                                              │
├──────────────────────────────────────────────────────────────┤
│ ENDPOINT LATENCY                                             │
│                                                              │
│ /feed             p50 18ms   p95 43ms   p99 81ms            │
│ /posts/:id        p50 16ms   p95 48ms   p99 92ms            │
│ /like             p50 14ms   p95 41ms   p99 76ms            │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

---

# 35. The MVP I'd actually build

Don't start with all of that.

I'd do this in **four iterations**.

## V0.1 — k6 runner

**Implemented.** `autobench run --target ... --rate ... --duration ...`.

Build:

```bash
autobench run --target http://localhost:3000 --rate 5000
```

Features:

* invoke k6
* constant arrival rate
* JSON output
* p50/p95/p99
* errors
* clean terminal output

---

## V0.2 — adaptive search

**Implemented.** `autobench capacity config.yaml`.

Add:

```bash
autobench capacity config.yaml
```

Implement:

```text
initial rate
      ↓
exponential search
      ↓
find PASS/FAIL bracket
      ↓
binary search
      ↓
capacity
```

This is the core of your idea.

---

## V0.3 — validation + reporting

**Implemented.** `autobench run --html report.html`, `autobench capacity
--html report.html`, and `report.html` written by `capacity --out-dir`.

Add:

```text
5-minute validation
JSON results
HTML report
endpoint breakdown
latency graph
```

At this point it's already a genuinely useful tool.

---

## V0.4 — production-grade benchmarker

**Implemented (V1.0).** Every item on this list now ships:

* repeated confirmation
* generator saturation detection
* automatic VU calculation
* time-window metrics
* Prometheus integration
* Docker support
* benchmark comparison
* CI integration
* GitHub Actions
* regression detection

`autobench compare` is the last of them, and the one that makes the rest usable
in a pipeline: a capacity search is a measurement, and only a comparison against
a known-good baseline says whether a change made things worse. It reads two
`--json` results — fixed runs or searches — and reports the change per
metric with the direction that matters, failing on anything past a configurable
tolerance. `examples/github-actions.yml` wires it into a pull request.

Three decisions are worth recording:

* **Direction is per metric, and the error rate is judged in absolute terms.** A
  latency rise is bad, a capacity drop is bad, an error rise is bad; a signed
  percentage without that invites reading a regression as good news. The error
  rate is the exception to the second half: 0.1% to 0.2% has doubled, but the
  doubling is not the alarming part, and a relative tolerance would treat a
  trivial rate as a crisis.
* **Only the intersection is compared.** A percentile one run did not collect has
  nothing to compare against, and inventing a zero reports a spectacular
  improvement. Capacity is compared only between two searches, because one
  measurement is not a capacity.
* **A mismatch is called out, not smoothed over.** Different targets, different
  rates, a fixed run against a search and a generator-saturated run each produce
  a note. A comparison of two different things is worse than no comparison,
  because it looks like an answer.

The tolerances default loose — 10% latency, 5% capacity, 0.1pp of errors —
because a load test is a noisy measurement and a gate that fires on run-to-run
variation is a gate people turn off. Both reports say so.

```bash
autobench compare main.json pr.json
```

```text
  CAPACITY

  METRIC            BASELINE   CANDIDATE      CHANGE
  capacity               675         621       -8.0%   REGRESSION

  LATENCY

  METRIC            BASELINE   CANDIDATE      CHANGE
  p95                  3.0ms       3.9ms      +30.1%   REGRESSION

  RESULT: FAIL_REGRESSION

  2 metrics regressed: capacity, p95
```

---

# 36. The architecture I'd settle on

Ultimately:

```text
                         autobench
                            │
             ┌──────────────┼───────────────┐
             │              │               │
             ▼              ▼               ▼
         Config          Search          Reporter
             │           Engine             │
             │              │               │
             └──────────────┼───────────────┘
                            │
                         Runner
                            │
                         ┌──▼──┐
                         │ k6  │
                         └──┬──┘
                            │
                         HTTP
                            │
                    ┌───────▼───────┐
                    │      SUT      │
                    └───────┬───────┘
                            │
                  ┌─────────┴─────────┐
                  ▼                   ▼
              Prometheus           Database
                  │
                  └─────────┬─────────┘
                            ▼
                       Correlation
```

And I'd use **Go for the orchestrator + k6's JS for the workload**.

That gives you a particularly nice project because you're not reinventing an HTTP load generator. You're building the **intelligent control plane around k6**.

The killer feature isn't actually "run k6 at 5k RPS."

It's:

> **"Automatically discover the maximum sustainable throughput under a user-defined SLO, prove it with a sustained validation run, and produce a reproducible report showing exactly where performance broke down."**

That's a very solid tool to build.

[1]: https://k6.io/docs/using-k6/metrics?utm_source=chatgpt.com "Metrics | Grafana k6 documentation"
[2]: https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/constant-arrival-rate/?utm_source=chatgpt.com "Constant arrival rate | Grafana k6 documentation"
[3]: https://grafana.com/docs/k6/latest/using-k6/thresholds/?pg=load-testing-your-api-with-postman&plcmt=in-text&utm_source=chatgpt.com "Thresholds | Grafana k6 documentation"
[4]: https://grafana.com/docs/k6/latest/using-k6/k6-options/reference/?utm_source=chatgpt.com "Options reference | Grafana k6 documentation"
[5]: https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/arrival-rate-vu-allocation/?utm_source=chatgpt.com "Arrival-rate VU allocation | Grafana k6 documentation"
[6]: https://grafana.com/docs/k6/latest/results-output/end-of-test/custom-summary/?utm_source=chatgpt.com "Custom summary | Grafana k6 documentation"
[7]: https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/ramping-arrival-rate/?pg=blog&plcmt=body-txt&utm_source=chatgpt.com "Ramping arrival rate | Grafana k6 documentation"

