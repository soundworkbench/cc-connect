#!/bin/bash
# sw.13：只升级空闲的洞察、总助与大师；规划有待答卡片，推迟；小满、伙伴、小芒果只放好程序不重启。
V=v1.5.2-beta.1-sw.167-sw.13
log=/srv/xiaoman/research/feishu-stream/upgrade-$V.log
REL=/srv/xiaoman/cc-connect-unified/releases/$V/cc-connect
echo "$(date +%T) $V installed, current -> $V; upgrading idle agents" >> $log
for u in ${UNITS:-insights-cc-connect butler-cc-connect}; do
  t0=$(date '+%Y-%m-%d %H:%M:%S')
  sudo -n systemctl restart $u || { echo "$(date +%T) restart $u failed" >> $log; continue; }
  ok=no
  for i in $(seq 1 20); do
    journalctl -u $u --since "$t0" --no-pager -o cat | grep -q 'connected to wss://' && { ok=yes; break; }
    /bin/sleep 2
  done
  echo "$(date +%T) restarted $u: active=$(systemctl is-active $u) connected=$ok exe=$(readlink /proc/$(systemctl show $u -p MainPID --value)/exe)" >> $log
done
# 大师：同 upgrade-agents.sh up_dashi
H=/srv/xiaoman/docker-data/cc-connect-dashi-v4/home/.local
sudo -n install -d -o 1001 -g 1001 $H/lib/cc-connect/$V && sudo -n install -m 755 -o 1001 -g 1001 $REL $H/lib/cc-connect/$V/cc-connect &&
sudo -n ln -sfn /home/claude/.local/lib/cc-connect/$V/cc-connect $H/bin/cc-connect &&
docker restart cc-connect-dashi-v4 >/dev/null && echo "$(date +%T) restarted cc-connect-dashi-v4: $(docker exec cc-connect-dashi-v4 cc-connect --version | head -1)" >> $log
# 小芒果：只换文件不重启（单文件绑定挂载，容器重启后才看到新文件）
F=/srv/xiaoman/docker-data/cc-connect-xiaomangguo/runtime/cc-connect
sudo -n cp -p $F $F.bak-pre-$V && sudo -n install -m 755 -o 1000 -g 1000 $REL $F &&
echo "$(date +%T) placed $V for cc-connect-xiaomangguo (not restarted): $($F --version | head -1)" >> $log
echo "$(date +%T) xiaoman-cc-connect, partner-cc-connect: current -> $V, not restarted" >> $log
echo "$(date +%T) done" >> $log
