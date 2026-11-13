#!/usr/bin/env bash
# Daily credit collection for every account in the pool.
# Runs the same chain as the panel buttons: checkin -> activity -> travel -> growth tasks -> balance.
set -u
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR" || exit 1
export TZ=Asia/Shanghai
KEY="sk-VjX5IHXIQuDgx-RYQTWLyiKY"
BASE="http://127.0.0.1:7863"
LOG="$DIR/logs/daily-$(date +%F).log"
mkdir -p "$DIR/logs"

log() { echo "[$(date '+%F %T')] $*" >> "$LOG"; }

api() { # api METHOD PATH [BODY]
  local m="$1" p="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS --noproxy '*' --max-time 240 -X "$m" "$BASE$p" \
      -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS --noproxy '*' --max-time 240 -X "$m" "$BASE$p" \
      -H "Authorization: Bearer $KEY"
  fi
}

log "=== daily run start ==="

if ! "$DIR/run.sh" >> "$LOG" 2>&1; then
  log "FATAL: service unavailable, abort"
  exit 1
fi

log "checkin : $(api POST /panel/api/checkin_all)"
sleep 20
log "activity: $(api POST /panel/api/activity_all)"
sleep 30
log "travel  : $(api POST /panel/api/travel_all)"
sleep 30
log "scan    : $(api POST /panel/api/tasks/scan_all)"
sleep 5
log "queue   : $(api POST /panel/api/tasks/run_queue '{"concurrency":2}')"

for i in $(seq 1 40); do
  sleep 60
  q=$(api GET /panel/api/tasks/queue)
  pending=$(printf '%s' "$q" | jq -r '[.items[]? | select(.status != "done")] | length' 2>/dev/null)
  log "poll $i: pending=${pending:-unknown}"
  [ "${pending:-1}" = "0" ] && break
done

log "balance : $(api POST /panel/api/balance_all)"
sleep 5

st=$(api GET /status)
printf '%s' "$st" | jq -c '{healthy,total,cooling,disabled}' 2>/dev/null | while read -r line; do
  log "pool    : $line"
done
printf '%s' "$st" | jq -r '.accounts[]? | "\(.nickname)\t\(.realm)\t\(.credits)"' 2>/dev/null |
while IFS=$'\t' read -r nick realm credits; do
  log "acct    : $nick | realm=$realm | credits=$credits"
done

log "=== daily run done ==="
exit 0
