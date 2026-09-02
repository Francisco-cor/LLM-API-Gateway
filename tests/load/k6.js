import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export const options = {
  stages: [
    { duration: '10s', target: 100 },   // ramp up to 100 VUs
    { duration: '30s', target: 1000 },  // ramp to 1000 RPS equivalent (~100 VUs with max ~10 rps per VU)
    { duration: '30s', target: 1000 },  // sustain 1k RPS
    { duration: '10s', target: 0 },     // ramp down
  ],
  thresholds: {
    http_req_failed: ['rate<0.01'],           // 1% error budget
    http_req_duration: ['p(95)<30', 'p(99)<50'], // p95 <30ms overhead (mock), p99 <50ms
    errors: ['rate<0.01'],
  },
};

const GATEWAY_URL = __ENV.GATEWAY_URL || 'http://localhost:8080';
const API_KEY = __ENV.GATEWAY_API_KEY || 'test-key-bench';

export default function () {
  const payload = JSON.stringify({
    model: 'gpt-4o',
    messages: [{ role: 'user', content: 'What is a token bucket? Answer in one sentence.' }],
    max_tokens: 32,
  });

  const params = {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${API_KEY}`,
      'X-Request-ID': `k6-${__VU}-${__ITER}`,
    },
    timeout: '5s',
  };

  const res = http.post(`${GATEWAY_URL}/v1/chat/completions`, payload, params);

  const ok = check(res, {
    'status is 200 or 429': (r) => r.status === 200 || r.status === 429,
    'has X-Gateway-Provider or rate limited': (r) => r.headers['X-Gateway-Provider'] !== undefined || r.status === 429,
    'p95 overhead <30ms or rate limited': (r) => r.timings.duration < 30 || r.status === 429,
  });

  errorRate.add(!ok);

  // Optional: also hit /v1/models every 20th iter to test discovery path
  if (__ITER % 20 === 0) {
    const m = http.get(`${GATEWAY_URL}/v1/models`, { headers: { Authorization: `Bearer ${API_KEY}` } });
    check(m, { 'models 200': (r) => r.status === 200 });
  }

  sleep(0.1);
}

export function handleSummary(data) {
  return {
    'summary.json': JSON.stringify(data, null, 2),
    stdout: textSummary(data, { indent: ' ', enableColors: true }),
  };
}

function textSummary(data, opts) {
  // minimal pretty printer (k6's textSummary is built-in but we avoid import)
  let s = `\n=== k6 load summary ===\n`;
  s += `checks: ${JSON.stringify(data.metrics.checks)}\n`;
  s += `http_req_duration p95: ${data.metrics.http_req_duration ? data.metrics.http_req_duration.values['p(95)'] : 'n/a'} ms\n`;
  s += `http_req_failed: ${data.metrics.http_req_failed ? data.metrics.http_req_failed.values.rate : 0}\n`;
  s += `errors rate: ${data.metrics.errors ? data.metrics.errors.values.rate : 0}\n`;
  return s;
}
