#!/usr/bin/env bash
# Checks a running Compose stack from outside, over real HTTP: health,
# routing, prefix stripping, request ids, 404/405, 502, 504, and a clean
# SIGTERM shutdown.
set -euo pipefail
base_url="${BASE_URL:-http://localhost:8080}"

field() { python3 -c 'import json, sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }
status() { curl --silent --output /dev/null --write-out '%{http_code}' "$@"; }
expect() { # expect NAME GOT WANT
  if [ "$2" != "$3" ]; then echo "FAIL $1: got $2, want $3" >&2; exit 1; fi
  echo "ok   $1: $2"
}

body="$(curl --fail --silent --show-error "$base_url/healthz")"
expect "healthz" "$(field "$body" status)" "ok"

body="$(curl --fail --silent --show-error "$base_url/users/42")"
expect "GET /users/42 reaches" "$(field "$body" backend)" "users"
expect "GET /users/42 path" "$(field "$body" path)" "/users/42"

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

docker compose stop --timeout 15 gatekeeper >/dev/null
code="$(docker inspect --format '{{.State.ExitCode}}' "$(docker compose ps -a -q gatekeeper)")"
expect "SIGTERM exit code" "$code" "0"
docker compose logs --no-log-prefix gatekeeper | grep -q '"msg":"shut down cleanly"'
echo "smoke test passed"
