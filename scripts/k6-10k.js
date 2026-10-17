// Independent cross-check of the 10k req/s target with k6.
//
//   k6 run scripts/k6-10k.js
//   k6 run -e RATE=12000 -e DURATION=60s -e TARGET=http://127.0.0.1:8080 scripts/k6-10k.js
//
// Uses the constant-arrival-rate executor: k6 starts RATE requests every
// second no matter how slowly earlier ones finish. (k6's default executor
// waits for each reply before sending the next request, so it quietly
// sends less traffic when the system under test slows down.)
import http from "k6/http";
import { check } from "k6";

const RATE = Number(__ENV.RATE || 10000);
const TARGET = __ENV.TARGET || "http://127.0.0.1:8080";

export const options = {
  discardResponseBodies: true,
  scenarios: {
    target: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: __ENV.DURATION || "60s",
      preAllocatedVUs: 100, // ~60 are needed at 10k req/s x 6ms; opening hundreds of
      // connections in the same instant overflows the Windows listen backlog.
      maxVUs: 4000,
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.001"],
    http_req_duration: ["p(99)<50"],
    dropped_iterations: ["count<100"],
  },
};

export default function () {
  const res = http.get(`${TARGET}/api/trains`);
  check(res, { "status 200": (r) => r.status === 200 });
}
