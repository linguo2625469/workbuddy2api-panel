#!/usr/bin/env bash
# workbuddy2api launcher (idempotent: skips when the service already answers)
set -u
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR" || exit 1
export TZ=Asia/Shanghai
PORT=7863
mkdir -p "$DIR/logs" "$DIR/data"

probe() { curl -s --noproxy '*' -o /dev/null --max-time 3 "http://127.0.0.1:$PORT/healthz"; }

if probe; then
  echo "[$(date '+%F %T')] already running"
  exit 0
fi

# stale pid handling
if [ -f "$DIR/wb2api.pid" ]; then
  PID=$(cat "$DIR/wb2api.pid" 2>/dev/null || true)
  if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then
    for _ in 1 2 3 4 5 6; do
      sleep 2
      if probe; then echo "[$(date '+%F %T')] already running (pid $PID)"; exit 0; fi
    done
    echo "[$(date '+%F %T')] pid $PID alive but not serving, restarting"
    kill "$PID" 2>/dev/null || true
    sleep 2
  fi
fi

setsid nohup "$DIR/wb2api" -config "$DIR/config.json" >> "$DIR/logs/wb2api.log" 2>&1 < /dev/null &
sleep 1
PID=$(pgrep -f "$DIR/wb2api -config" | head -1)
[ -n "${PID:-}" ] && echo "$PID" > "$DIR/wb2api.pid"

for _ in 1 2 3 4 5 6 7 8 9 10; do
  sleep 2
  if probe; then
    echo "[$(date '+%F %T')] started pid=${PID:-?}"
    exit 0
  fi
done

echo "[$(date '+%F %T')] FAILED to start within 20s, check $DIR/logs/wb2api.log"
exit 1
