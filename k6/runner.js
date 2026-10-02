// autobench k6 workload runner.
//
// All configuration is passed in through AUTOBENCH_* environment variables so
// that the Go controller stays the single source of truth. This script only
// generates traffic at a fixed iteration rate and emits the aggregated summary
// as JSON for the controller to parse and evaluate.
//
// The controller owns pass/fail decisions; this script deliberately sets no k6
// thresholds so that a failing probe does not abort the k6 process.

import { check } from 'k6';
import http from 'k6/http';

const RATE = Number(__ENV.AUTOBENCH_RATE || '0');
const TIME_UNIT = __ENV.AUTOBENCH_TIME_UNIT || '1s';
const DURATION = __ENV.AUTOBENCH_DURATION || '30s';
const BASE_URL = (__ENV.AUTOBENCH_BASE_URL || '').replace(/\/+$/, '');
const PRE_ALLOCATED_VUS = Number(__ENV.AUTOBENCH_PRE_ALLOCATED_VUS || '10');
const MAX_VUS = Number(__ENV.AUTOBENCH_MAX_VUS || '100');
const PERCENTILES = JSON.parse(__ENV.AUTOBENCH_PERCENTILES || '[50,75,90,95,99,99.9]');
const WORKLOAD = JSON.parse(__ENV.AUTOBENCH_WORKLOAD || '[]');
const SUMMARY_PATH = __ENV.AUTOBENCH_SUMMARY_PATH || '';

// k6 emits one value per requested stat; "med" is the p50 alias.
const trendStats = ['avg', 'min', 'max', 'med'].concat(
  PERCENTILES.map((p) => `p(${p})`),
);

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
};

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

export default function () {
  const req = pickRequest();
  if (!req) {
    return;
  }

  const method = (req.method || 'GET').toUpperCase();
  const params = { tags: { name: req.name || req.path } };
  if (req.headers) {
    params.headers = req.headers;
  }

  let res;
  if (method === 'GET' || method === 'HEAD' || method === 'DELETE' || method === 'OPTIONS') {
    res = http.request(method, BASE_URL + req.path, null, params);
  } else {
    res = http.request(method, BASE_URL + req.path, req.body || null, params);
  }

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
