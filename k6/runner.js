// autobench k6 workload runner.
//
// All configuration is passed in through AUTOBENCH_* environment variables so
// that the Go controller stays the single source of truth. This script only
// generates traffic at a fixed iteration rate and emits the aggregated summary
// as JSON for the controller to parse and evaluate.
//
// The controller owns pass/fail decisions; this script deliberately sets no k6
// threshold that could abort the run. The thresholds below exist only to make k6
// emit a per-endpoint submetric for every request in the workload: k6 includes a
// tagged submetric in the summary when (and only when) a threshold selects it.
// Each one is written as an always-true condition with abortOnFail disabled, so
// it can never change the outcome of a run.

import { check } from 'k6';
import { Counter } from 'k6/metrics';
import http from 'k6/http';

const RATE = Number(__ENV.AUTOBENCH_RATE || '0');
const TIME_UNIT = __ENV.AUTOBENCH_TIME_UNIT || '1s';
const DURATION = __ENV.AUTOBENCH_DURATION || '30s';
const BASE_URL = (__ENV.AUTOBENCH_BASE_URL || '').replace(/\/+$/, '');
const PRE_ALLOCATED_VUS = Number(__ENV.AUTOBENCH_PRE_ALLOCATED_VUS || '10');
const MAX_VUS = Number(__ENV.AUTOBENCH_MAX_VUS || '100');
const PERCENTILES = JSON.parse(__ENV.AUTOBENCH_PERCENTILES || '[50,75,90,95,99,99.9]');
const WORKLOAD_MODE = __ENV.AUTOBENCH_WORKLOAD_MODE || 'weighted';
const WORKLOAD = JSON.parse(__ENV.AUTOBENCH_WORKLOAD || '[]');
const SUMMARY_PATH = __ENV.AUTOBENCH_SUMMARY_PATH || '';
// A staging target commonly presents a certificate the system roots do not know,
// and k6 verifies by default, so the switch is passed through rather than assumed.
const INSECURE_TLS = (__ENV.AUTOBENCH_INSECURE_TLS || 'false') === 'true';
const REQUEST_TIMEOUT = __ENV.AUTOBENCH_REQUEST_TIMEOUT || '';
// k6 follows up to 10 redirects by default, which is a decision a benchmark may
// want to make differently: following a chain measures the whole chain, and not
// following measures the redirect itself.
const MAX_REDIRECTS = __ENV.AUTOBENCH_MAX_REDIRECTS || '';
// A target that authenticates the caller needs a client certificate, which k6
// takes as PEM contents rather than as a path.
const CLIENT_CERT = __ENV.AUTOBENCH_CLIENT_CERT || '';
const CLIENT_KEY = __ENV.AUTOBENCH_CLIENT_KEY || '';
// k6 maps a client certificate to a set of domains, and its runtime has no URL
// parser, so the host is derived from the base URL on the Go side.
const TARGET_HOST = __ENV.AUTOBENCH_TARGET_HOST || '';

// tlsAuthOptions is a client certificate mapped to the target's host, or nothing
// when none is configured. It is a global option in k6 rather than a request
// parameter, and an entry without domains is not applied at all.
function applyTLSAuth(o) {
  if (CLIENT_CERT && CLIENT_KEY && TARGET_HOST) {
    o.tlsAuth = [{ domains: [TARGET_HOST], cert: CLIENT_CERT, key: CLIENT_KEY }];
  }
}

// k6 emits one value per requested stat; "med" is the p50 alias.
const trendStats = ['avg', 'min', 'max', 'med'].concat(
  PERCENTILES.map((p) => `p(${p})`),
);

// Always-satisfied thresholds, used purely to request a submetric. k6 validates
// the aggregation method against the metric type, so each kind gets its own.
const ALWAYS_TREND = [{ threshold: 'p(95)>=0', abortOnFail: false }];
const ALWAYS_RATE = [{ threshold: 'rate>=0', abortOnFail: false }];
const ALWAYS_COUNT = [{ threshold: 'count>=0', abortOnFail: false }];

function submetricThresholds() {
  const thresholds = {};
  for (const req of WORKLOAD) {
    const name = req.name || req.path;
    thresholds[`http_req_duration{name:${name}}`] = ALWAYS_TREND;
    thresholds[`http_req_failed{name:${name}}`] = ALWAYS_RATE;
    thresholds[`http_reqs{name:${name}}`] = ALWAYS_COUNT;
  }
  return thresholds;
}

export const options = {
  scenarios: {
    workload: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: TIME_UNIT,
      duration: DURATION,
      preAllocatedVUs: PRE_ALLOCATED_VUS,
      maxVUs: MAX_VUS,
    },
  },
  summaryTrendStats: trendStats,
  thresholds: submetricThresholds(),
  insecureSkipTLSVerify: INSECURE_TLS,
};

// The optional settings are applied after the literal rather than folded into it
// with object spread. k6's older JavaScript engines do not compile `...expr`
// inside an object literal: v0.50.0 rejects the whole script with a SyntaxError,
// and this script has to run on whatever k6 the user has.
if (MAX_REDIRECTS !== '') {
  options.maxRedirects = Number(MAX_REDIRECTS);
}
applyTLSAuth(options);

// Pre-compute a cumulative-weight table so each iteration picks a request with
// a single O(n) scan over the (small) configured workload.
const cumulative = [];
let totalWeight = 0;
for (const req of WORKLOAD) {
  totalWeight += req.weight && req.weight > 0 ? req.weight : 1;
  cumulative.push(totalWeight);
}

function pickRequest() {
  if (WORKLOAD.length === 0) {
    return null;
  }
  const x = Math.random() * totalWeight;
  for (let i = 0; i < cumulative.length; i++) {
    if (x < cumulative[i]) {
      return WORKLOAD[i];
    }
  }
  return WORKLOAD[WORKLOAD.length - 1];
}

function doRequest(req) {
  const method = (req.method || 'GET').toUpperCase();
  const params = { tags: { name: req.name || req.path } };
  if (req.headers) {
    params.headers = req.headers;
  }
  // k6 has no global HTTP timeout: it is a per-request parameter, and it defaults
  // to 60s. A target that hangs would otherwise make the probe last a minute
  // rather than the duration it was configured for, and the run would measure the
  // hang rather than the load.
  if (REQUEST_TIMEOUT) {
    params.timeout = REQUEST_TIMEOUT;
  }

  if (method === 'GET' || method === 'HEAD' || method === 'DELETE' || method === 'OPTIONS') {
    return http.request(method, BASE_URL + req.path, null, params);
  }
  return http.request(method, BASE_URL + req.path, req.body || null, params);
}

// Response classes, declared up front because k6 metrics must exist before the
// test starts. k6's own http_req_failed tells you that requests failed but not
// how: a 4xx storm means the workload is wrong, a 5xx storm means the system
// under test is. The controller reports the split (PLAN-OUTLINE.md section 30).
const statusClasses = {
  2: new Counter('ab_status_2xx'),
  3: new Counter('ab_status_3xx'),
  4: new Counter('ab_status_4xx'),
  5: new Counter('ab_status_5xx'),
};
const statusOther = new Counter('ab_status_other');

function recordStatus(res) {
  const bucket = statusClasses[Math.floor(res.status / 100)];
  if (bucket) {
    bucket.add(1);
    return;
  }
  // A non-HTTP status, or a transport error reported as 0.
  statusOther.add(1);
}

// One iteration is either a single weighted request, or every step of a journey
// in order. The controller reports the two rates separately because they differ
// by a factor of the journey length (PLAN-OUTLINE.md section 11).
export default function () {
  if (WORKLOAD_MODE === 'journey') {
    for (const req of WORKLOAD) {
      const res = doRequest(req);
      recordStatus(res);
      check(res, { 'status < 400': (r) => r.status < 400 });
    }
    return;
  }

  const req = pickRequest();
  if (!req) {
    return;
  }
  const res = doRequest(req);
  recordStatus(res);
  check(res, { 'status < 400': (r) => r.status < 400 });
}

// Emit the aggregated summary to the controller-provided path. Returning a map
// without a "stdout" key suppresses k6's own end-of-test summary.
export function handleSummary(data) {
  if (!SUMMARY_PATH) {
    return {};
  }
  return { [SUMMARY_PATH]: JSON.stringify(data) };
}
