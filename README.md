# autobench

An adaptive load-testing orchestrator around [k6](https://k6.io/).

autobench is the **controller**; k6 is the **traffic generator**. The controller
decides the load, spawns k6, parses the machine-readable summary, evaluates your
SLO thresholds and renders the report. k6 stays a dumb, precisely controlled
arrival-rate generator.

> **Status: V0.1** — a single fixed-rate run. The adaptive capacity search
> (exponential bracket → binary search → validation) and HTML reporting are
> planned for V0.2/V0.3. See `PLAN-OUTLINE.md`.

## Install

Requires Go 1.24+ and `k6` on your `PATH` (or set `AUTOBENCH_K6`).

```bash
go build -o bin/autobench ./cmd/autobench
# or
make build
```

## Usage

```bash
# Pure flags
autobench run --target http://localhost:3000 --rate 500 --duration 30s

# From a config file
autobench run examples/api.yaml

# Config file with flag overrides
autobench run examples/api.yaml --rate 2000 --json result.json
```

The config path is a positional argument and flags may appear before or after it.

Exit codes: `0` all thresholds passed · `1` threshold failed or the generator
saturated · `2` usage/runtime error.

Verdicts: `PASS`, `FAIL_LATENCY`, `FAIL_ERRORS`, `FAIL_GENERATOR`. A saturated
generator outranks threshold breaches, because a probe that never reached the
target rate says nothing about the system under test.

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
| `--no-color` | Disable coloured output |

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

Omit `load_generator` and autobench sizes the pool from `run.rate` (assuming
~100ms per iteration). If k6 runs out of VUs or drops iterations, autobench
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
