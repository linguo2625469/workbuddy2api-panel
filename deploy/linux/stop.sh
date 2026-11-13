#!/usr/bin/env bash
# Stop the gateway started by run.sh
set -u
DIR="$(cd "$(dirname "$0")" && pwd)"
PID=$(pgrep -f "$DIR/wb2api -config" | head -1)
if [ -z "${PID:-}" ]; then
  echo "not running"
  rm -f "$DIR/wb2api.pid"
  exit 0
fi
kill "$PID" 2>/dev/null || true
for _ in 1 2 3 4 5 6 7 8 9 10; do
  sleep 1
  kill -0 "$PID" 2>/dev/null || { echo "stopped pid $PID"; rm -f "$DIR/wb2api.pid"; exit 0; }
done
echo "pid $PID still alive after 10s"
exit 1
