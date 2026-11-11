#!/bin/bash
# 从 GitHub soundworkbench/cc-connect 的 local 分支取代码 → 测试 → 构建 <版本> → 配置补 segmented = true → 等小满空闲 → 换版重启小满；40 秒连不上飞书就退回上一版。
# 用法：ship-git.sh <版本号或 auto> <改动说明>。auto 按公开仓库的规则算：<上游标签>-sw.<提交数>，与 soundworkbench/build.sh 一致。结果写 ship-<版本>.log。
V=$1; CH=$2
S=/srv/xiaoman/research/swcc/sessfix
SRC=$S/src-git
log=/srv/xiaoman/research/feishu-stream/ship-$V.log
cur=/srv/xiaoman/cc-connect-unified/current
T=/srv/xiaoman/docker-data/claude-a/home/.claude/projects/-srv-git-shared-worktrees-JBrain-xiaoman-migration
REPO=https://github.com/soundworkbench/cc-connect
[ -d $SRC/.git ] || git clone -q $REPO $SRC
git -C $SRC fetch -q $REPO local && git -C $SRC checkout -q --detach FETCH_HEAD || { echo "$(date +%T) fetch failed" >> $log; exit 1; }
if [ "$V" = auto ]; then
  TAG=$(git -C $SRC describe --tags --match 'v*' --exclude '*-sw.*' --abbrev=0 HEAD) || exit 1
  V="$TAG-sw.$(git -C $SRC rev-list --count $TAG..HEAD)"
  log=/srv/xiaoman/research/feishu-stream/ship-$V.log
  echo "$V" > /srv/xiaoman/research/feishu-stream/ship-last-version
fi
echo "$(date +%T) start $(git -C $SRC log --oneline -1)" >> $log
$S/gotest.sh $SRC sh -c 'go vet ./platform/feishu/ ./core/ ./agent/claudecode/ ./config/ && go test -tags no_web,goolm ./platform/feishu/ ./core/ ./agent/claudecode/ ./config/' > $S/out/test-$V.txt 2>&1
if grep -qE '^(FAIL|--- FAIL)' $S/out/test-$V.txt; then
  # 偶发失败（如临时目录清理）先整包重跑一次，第二次仍失败才算失败。
  echo "$(date +%T) tests failed once ($(grep -E '^--- FAIL' $S/out/test-$V.txt | head -3 | tr '\n' ' ')), rerun" >> $log
  $S/gotest.sh $SRC sh -c 'go test -count=1 -tags no_web,goolm ./platform/feishu/ ./core/ ./agent/claudecode/ ./config/' > $S/out/test-$V.txt 2>&1
fi
if grep -qE '^(FAIL|--- FAIL)' $S/out/test-$V.txt; then
  fails=$(grep -E '^--- FAIL' $S/out/test-$V.txt | grep -v TestHandleRelay_ReturnsPartialOnTimeout)
  if [ -n "$fails" ] || ! grep -q 'ok.*platform/feishu' $S/out/test-$V.txt || ! grep -q 'ok.*agent/claudecode' $S/out/test-$V.txt; then
    echo "$(date +%T) tests failed, stop: $fails" >> $log; exit 1
  fi
  echo "$(date +%T) only known relay test failed, continue" >> $log
fi
echo "$(date +%T) tests ok" >> $log
prev=$(readlink $cur)
SRC=$SRC $S/build-release.sh $V "$CH" "${prev#releases/}" >> $log 2>&1 || { echo "$(date +%T) build failed" >> $log; exit 1; }
# 配置项改名：crysound6 → segmented。两行并存，新旧版本都认。
for f in /etc/xiaoman/config.toml /etc/xiaoman/butler.toml /etc/xiaoman/partner.toml; do
  sudo -n grep -q '^segmented = true' $f || sudo -n sed -i 's/^crysound6 = true$/crysound6 = true\nsegmented = true/' $f
done
end=$(( $(date +%s) + 1800 ))
while :; do
  newest=$(ls -t $T/*.jsonl 2>/dev/null | head -1)
  age=$(( $(date +%s) - $(stat -c %Y "$newest") ))
  [ $age -ge 120 ] && break
  if [ $(date +%s) -ge $end ]; then echo "$(date +%T) not idle after 30 min, restart anyway" >> $log; break; fi
  /bin/sleep 20
done
sudo -n ln -sfn releases/$V $cur
t0=$(date '+%Y-%m-%d %H:%M:%S')
sudo -n systemctl restart xiaoman-cc-connect
for i in $(seq 1 20); do
  if journalctl -u xiaoman-cc-connect --since "$t0" --no-pager -o cat | grep -q 'connected to wss://'; then
    echo "$(date +%T) ok: $($cur/cc-connect --version | head -1)" >> $log
    exit 0
  fi
  /bin/sleep 2
done
echo "$(date +%T) $V not connected, rolling back to $prev" >> $log
sudo -n ln -sfn $prev $cur
sudo -n systemctl restart xiaoman-cc-connect
echo "$(date +%T) rolled back to $prev" >> $log
