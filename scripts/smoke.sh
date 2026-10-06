#!/usr/bin/env bash
# Checks a running Compose stack from outside, over real HTTP: health,
# routing, prefix stripping, request ids, 404/405, 502, 504, load balancing
# across two instances, losing one with no failed requests (retries) and
# regaining it, a rate limit shared by two gateway replicas, failing open
# when Redis is down, the circuit breaker opening and closing, the body
# size limit, Prometheus metrics on the admin port only, and a clean SIGTERM
# shutdown.
set -euo pipefail
base_url="${BASE_URL:-http://localhost:8080}"
second_url="${SECOND_URL:-http://localhost:8081}"

# Every routing check sends its own API key, so the rate limit (per key)
# never gets in the way of checks that are not about it.
curl() { command curl -H "X-API-Key: smoke-$RANDOM$RANDOM" "$@"; }

field() { python3 -c 'import json, sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }
status() { curl --silent --output /dev/null --write-out '%{http_code}' "$@"; }
expect() { # expect NAME GOT WANT
  if [ "$2" != "$3" ]; then echo "FAIL $1: got $2, want $3" >&2; exit 1; fi
  echo "ok   $1: $2"
}

body="$(curl --fail --silent --show-error "$base_url/healthz")"
expect "healthz" "$(field "$body" status)" "ok"

body="$(curl --fail --silent --show-error "$base_url/users/42")"
expect "GET /users/42 reaches a users instance" "$(field "$body" backend | cut -d- -f1)" "users"
expect "GET /users/42 path" "$(field "$body" path)" "/users/42"

instances() { # instances N: which instance answered each of N requests, counted
  for _ in $(seq "$1"); do
    curl --silent "$base_url/users/1" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("backend", "error"))' 2>/dev/null || echo error
  done | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}'
}
expect "round robin over 2 instances" "$(instances 10)" "users-1=5 users-2=5 "

body="$(curl --fail --silent --show-error -X DELETE "$base_url/api/orders/7")"
expect "DELETE /api/orders/7 reaches" "$(field "$body" backend)" "orders"
expect "prefix stripped" "$(field "$body" path)" "/orders/7"

headers="$(mktemp)"; trap 'rm -f "$headers"' EXIT
body="$(curl --fail --silent --show-error -D "$headers" -H 'X-Request-ID: smoke-1' "$base_url/users/1")"
expect "request id reaches the backend" "$(field "$body" request_id)" "smoke-1"
expect "request id returned once" "$(grep -ci '^x-request-id: smoke-1' "$headers")" "1"

expect "unknown path" "$(status "$base_url/nope")" "404"
expect "wrong method" "$(status -X PUT "$base_url/users/42")" "405"
expect "backend down" "$(status "$base_url/offline/x")" "502"
expect "backend too slow" "$(status "$base_url/slow?sleep=3s")" "504"

# One API key, 60 requests as fast as possible, alternating between the two
# replicas. The limit is 5/s with a burst of 10, shared through Redis, so
# about 10 pass in total, not 10 per replica.
limited="$(python3 - "$base_url" "$second_url" <<'PY'
import sys, time, urllib.request, urllib.error
urls, key, counts = sys.argv[1:3], "smoke-shared-key", {200: 0, 429: 0}
retry_after = None
start = time.monotonic()
for i in range(60):
    req = urllib.request.Request(urls[i % 2] + "/users/1", headers={"X-API-Key": key})
    try:
        urllib.request.urlopen(req).read(); counts[200] += 1
    except urllib.error.HTTPError as e:
        counts[e.code] = counts.get(e.code, 0) + 1
        retry_after = retry_after or e.headers.get("Retry-After")
elapsed = time.monotonic() - start
ceiling = 10 + int(5 * elapsed) + 1   # the burst plus what refilled meanwhile
ok = 10 <= counts[200] <= ceiling and counts[429] == 60 - counts[200] and retry_after
print(f"{'ok' if ok else 'FAIL'} {counts[200]} allowed, {counts[429]} refused (429) across 2 replicas in {elapsed:.2f}s; ceiling {ceiling}; Retry-After {retry_after}")
PY
)"
echo "     $limited"
[ "${limited%% *}" = "ok" ] || { echo "FAIL shared rate limit" >&2; exit 1; }
expect "a different key has its own limit" "$(command curl -s -o /dev/null -w '%{http_code}' -H 'X-API-Key: another-key' "$base_url/users/1")" "200"

# Redis down: the limiter fails open (on_store_error "allow").
docker compose stop --timeout 5 redis >/dev/null
expect "Redis down: fails open" "$(command curl -s -o /dev/null -w '%{http_code}' -H 'X-API-Key: smoke-shared-key' "$base_url/users/1")" "200"
docker compose start redis >/dev/null

# Lose an instance. Straight away, before any health check notices, a
# request sent to users-2 fails to connect and is retried on users-1, so no
# client sees an error. After that, health checks keep it out of rotation.
docker compose stop --timeout 5 users-2 >/dev/null
expect "users-2 just stopped: no failures" "$(instances 10)" "users-1=10 "
sleep 3
expect "with users-2 stopped" "$(instances 10)" "users-1=10 "
docker compose start users-2 >/dev/null
sleep 4
expect "users-2 back in rotation" "$(instances 10)" "users-1=5 users-2=5 "

# The body size limit (1 MiB in config/gatekeeper.json).
big="$(mktemp)"; head -c 2000000 /dev/zero > "$big"
expect "2 MB upload" "$(status -X POST --data-binary "@$big" "$base_url/users")" "413"
rm -f "$big"

# The circuit breaker on orders: stop it, and after 5 failed requests (at a
# 50% failure ratio) the gateway stops trying and answers 503 at once.
docker compose stop --timeout 5 orders >/dev/null
codes="$(for _ in $(seq 8); do status "$base_url/api/orders/1"; echo; done | tr '\n' ' ')"
expect "orders down: 502 x5, then circuit open" "$codes" "502 502 502 502 502 503 503 503 "
expect "open circuit sends Retry-After" "$(curl -s -D - -o /dev/null "$base_url/api/orders/1" | grep -ci '^retry-after:')" "1"
docker compose start orders >/dev/null
sleep 4   # open_for is 3 s; then one probe, which succeeds and closes it
expect "orders back: circuit closes" "$(status "$base_url/api/orders/1")" "200"
expect "and stays closed" "$(status "$base_url/api/orders/2")" "200"

# Metrics: on the admin port, not the public one; and Prometheus has them.
metrics="$(command curl -s http://127.0.0.1:9091/metrics)"
expect "metrics labelled by route pattern" "$(grep -c 'gatekeeper_requests_total{backend="users",code="200",route="GET /users/{id}"}' <<<"$metrics")" "1"
expect "no metrics on the public port" "$(status "$base_url/metrics")" "404"
sleep 3   # at least one scrape after the traffic above
query() { command curl -s --get http://127.0.0.1:9090/api/v1/query --data-urlencode "query=$1" \
  | python3 -c 'import json, sys; r = json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else "none")'; }
expect "Prometheus scrapes both replicas" "$(query 'sum(up{job="gatekeeper"})')" "2"
expect "Prometheus sees the 429s" "$(query 'sum(gatekeeper_requests_total{code="429"}) > bool 0')" "1"
expect "Prometheus sees the circuit closed again" "$(query 'max(gatekeeper_circuit_state{backend="orders"})')" "0"

docker compose stop --timeout 15 gatekeeper >/dev/null
code="$(docker inspect --format '{{.State.ExitCode}}' "$(docker compose ps -a -q gatekeeper)")"
expect "SIGTERM exit code" "$code" "0"
docker compose logs --no-log-prefix gatekeeper | grep -q '"msg":"shut down cleanly"'
echo "smoke test passed"
