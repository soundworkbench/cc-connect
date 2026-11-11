#!/bin/bash
# 列出各助理是否空闲：子进程数、最后一行日志距今秒数、最近一条回合事件。
EVRE='msg="(processing message|webhook: received|turn complete|unsolicited events detected|unsolicited turn complete)"'
for u in insights-cc-connect planning-cc-connect butler-cc-connect partner-cc-connect xiaoman-cc-connect; do
  main=$(systemctl show $u -p MainPID --value)
  kids=$(pgrep -P "$main" | wc -l)
  last=$(journalctl -u $u -n1 --no-pager -o short-unix | cut -d' ' -f1 | cut -d. -f1)
  ev=$(journalctl -u $u --since -6h --no-pager -o cat | grep -E "$EVRE" | tail -1 | grep -oE 'msg="[^"]+"')
  echo "$u kids=$kids age=$(( $(date +%s) - ${last:-0} ))s last_event=$ev"
done
for c in cc-connect-dashi-v4 cc-connect-xiaomangguo; do
  t=$(docker logs --since 30m -t $c 2>&1 | tail -1 | cut -d' ' -f1)
  ev=$(docker logs --since 6h $c 2>&1 | grep -E "$EVRE" | tail -1 | grep -oE 'msg="[^"]+"')
  echo "$c age=$(( $(date +%s) - $(date -d "$t" +%s 2>/dev/null || echo 0) ))s claude=$(docker top $c | grep -c -E "claude( |$)") last_event=$ev"
done
