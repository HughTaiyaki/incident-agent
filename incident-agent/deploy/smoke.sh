#!/bin/sh
# Builds the images and proves the stack starts, serves and finishes a run. Uses throwaway
# credentials and an unreachable model, so it needs no secrets and sends nothing out.
set -eu
cd "$(dirname "$0")"
PROJECT=incident-agent-smoke
SECRETS_CREATED=
if [ -e secrets ] || [ -e .env ]; then
	echo "deploy/secrets or deploy/.env exists; move them aside so the smoke test cannot touch them" >&2
	exit 2
fi
cleanup() {
	docker compose -p "$PROJECT" --env-file smoke.env down -v --remove-orphans >/dev/null 2>&1 || true
	rm -f smoke.env
	[ -z "$SECRETS_CREATED" ] || rm -rf secrets
}
trap cleanup EXIT

umask 077
mkdir secrets
SECRETS_CREATED=1
TOKEN=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
printf '%s\n' "$TOKEN" >secrets/incident_api_token
printf 'unused-smoke-key\n' >secrets/incident_model_api_key
# The app's users must read the files Compose bind-mounts; they are throwaway here.
chmod 644 secrets/incident_api_token secrets/incident_model_api_key
chmod 755 secrets
cat >smoke.env <<ENV
INCIDENT_MAX_ACTIVE_RUNS=2
INCIDENT_REDIS_MAXMEMORY=64mb
INCIDENT_REDIS_CONTAINER_MEMORY=256m
INCIDENT_MODEL_ENDPOINT=https://model.invalid/chat/completions
INCIDENT_MODEL_NAME=smoke
INCIDENT_MODEL_CONTEXT_WINDOW=32000
INCIDENT_SYSTEM_PROFILE=goframeshop
INCIDENT_PROMETHEUS_URL=http://127.0.0.1:1
INCIDENT_LOKI_URL=http://127.0.0.1:1
INCIDENT_JAEGER_URL=http://127.0.0.1:1
INCIDENT_API_PORT=${SMOKE_PORT:-18080}
ENV
BASE="http://127.0.0.1:${SMOKE_PORT:-18080}"
docker compose -p "$PROJECT" --env-file smoke.env up --build --detach --wait --wait-timeout 180

ready=
for _ in $(seq 1 60); do
	if [ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/readyz")" = 200 ]; then
		ready=1
		break
	fi
	sleep 1
done
[ -n "$ready" ] || { echo "readyz never returned 200" >&2; curl -s "$BASE/readyz" >&2; docker compose -p "$PROJECT" --env-file smoke.env logs >&2; exit 1; }

[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/runs/none")" = 401 ] || { echo "unauthenticated request not refused" >&2; exit 1; }
BODY='{"alert_id":"smoke-1","service":"order-service","environment":"local","started_at":"2026-10-28T06:01:00Z","summary":"smoke"}'
RUN=$(curl -sf -X POST "$BASE/v1/incidents" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$BODY" | sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p')
[ -n "$RUN" ] || { echo "submission failed" >&2; exit 1; }
finished=
for _ in $(seq 1 90); do
	if curl -sf "$BASE/v1/runs/$RUN" -H "Authorization: Bearer $TOKEN" | grep -q '"terminal":true'; then
		finished=1
		break
	fi
	sleep 1
done
[ -n "$finished" ] || { echo "run did not finish" >&2; docker compose -p "$PROJECT" --env-file smoke.env logs >&2; exit 1; }
curl -sf "$BASE/v1/runs/$RUN/report" -H "Authorization: Bearer $TOKEN" | grep -q '"result":"INCONCLUSIVE"' || { echo "unreachable dependencies must not produce a diagnosis" >&2; exit 1; }
METRICS=$(curl -sf "$BASE/metrics" -H "Authorization: Bearer $TOKEN")
for want in 'incident_redis_up 1' 'incident_worker_alive 1' 'incident_runs_finished_total{status="COMPLETED"} 1'; do
	printf '%s\n' "$METRICS" | grep -qF "$want" || { echo "metrics missing: $want" >&2; printf '%s\n' "$METRICS" >&2; exit 1; }
done

# Restarting everything must keep the finished run: this is the AOF volume, not the container.
docker compose -p "$PROJECT" --env-file smoke.env restart redis worker api >/dev/null
for _ in $(seq 1 60); do
	[ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/readyz")" = 200 ] && break
	sleep 1
done
curl -sf "$BASE/v1/runs/$RUN/report" -H "Authorization: Bearer $TOKEN" | grep -q '"result":"INCONCLUSIVE"' || { echo "run lost across restart" >&2; exit 1; }
echo "smoke passed"
