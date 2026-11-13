#!/usr/bin/env bash
# Cron entry point. Runs the daily credit job once per day (Asia/Shanghai date),
# and catches up automatically if the machine was off / the run failed.
set -u
DIR="$(cd "$(dirname "$0")" && pwd)"
export TZ=Asia/Shanghai
TODAY=$(date +%F)
MARK="$DIR/data/.daily-$TODAY.done"
ATT="$DIR/data/.daily-$TODAY.attempts"

[ -f "$MARK" ] && exit 0

# housekeeping: drop markers/logs older than 30 days
find "$DIR/data" -maxdepth 1 -name '.daily-*' -mtime +30 -delete 2>/dev/null
find "$DIR/logs" -maxdepth 1 -name 'daily-*.log' -mtime +30 -delete 2>/dev/null

# single instance
if ! mkdir "$DIR/data/.daily.lock" 2>/dev/null; then
  exit 0
fi
trap 'rmdir "$DIR/data/.daily.lock" 2>/dev/null' EXIT

n=$(cat "$ATT" 2>/dev/null || echo 0)
if [ "$n" -ge 3 ]; then
  echo "[$(date '+%F %T')] attempt limit reached for $TODAY, skip" >> "$DIR/logs/tick.log"
  touch "$MARK"
  exit 0
fi
echo $((n + 1)) > "$ATT"

"$DIR/daily.sh"
rc=$?
if [ $rc -eq 0 ]; then
  touch "$MARK"
  echo "[$(date '+%F %T')] daily job finished ok" >> "$DIR/logs/tick.log"
else
  echo "[$(date '+%F %T')] daily job failed rc=$rc (attempt $((n + 1)))" >> "$DIR/logs/tick.log"
fi
exit 0
