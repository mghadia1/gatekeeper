#!/usr/bin/env bash
# Checks a running Compose stack from outside, over real HTTP.
set -euo pipefail
base_url="${BASE_URL:-http://localhost:8080}"

echo "GET /healthz"
body="$(curl --fail --silent --show-error "$base_url/healthz")"
echo "$body"
python3 -c 'import json, sys; d = json.loads(sys.argv[1]); assert d["status"] == "ok", d' "$body"

echo "graceful shutdown: SIGTERM, then the exit code"
docker compose stop --timeout 15 gatekeeper >/dev/null
code="$(docker inspect --format '{{.State.ExitCode}}' "$(docker compose ps -a -q gatekeeper)")"
echo "exit code: $code"
[ "$code" = "0" ]
docker compose logs --no-log-prefix gatekeeper | grep -q '"msg":"shut down cleanly"'
echo "smoke test passed"
