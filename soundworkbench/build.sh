#!/bin/bash
# 从 local 分支构建单个可执行文件 dist/cc-connect-<版本>-<系统>-<架构>。
# 用法：soundworkbench/build.sh [提交或分支，默认 local]
# 先跑改动相关包的测试，不过就停，不编译。
# 版本号 <上游版本>-sw.<序号>：上游版本是 local 下面最近的上游标签，
# 序号是从该标签到这个提交的提交数。我们自己发布的 *-sw.* 标签不算上游标签，否则下一版会变成 …-sw.12-sw.1。
# 源码经 git archive 从标准输入送进 docker，不读工作树里未提交的改动。
# 环境变量：GOOS、GOARCH（默认 linux/amd64），CPUS（默认 4），
# GO_IMAGE（默认 golang:1.25），OUT（默认 dist），SKIP_TESTS=1 只编译。
set -u
REF=${1:-local}
GOOS=${GOOS:-linux}
GOARCH=${GOARCH:-amd64}
CPUS=${CPUS:-4}
GO_IMAGE=${GO_IMAGE:-golang:1.25}
OUT=${OUT:-dist}
TAGS=no_web,goolm
PKGS="./agent/claudecode/ ./agent/codex/ ./agent/opencode/ ./core/ ./platform/feishu/ ./config/ ./cmd/cc-connect/"
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)" || exit 1
SHA=$(git rev-parse --short=8 "$REF^{commit}") || exit 1
TAG=$(git describe --tags --match 'v*' --exclude '*-sw.*' --abbrev=0 "$REF") || { echo "no upstream tag below $REF: git fetch upstream --tags first"; exit 1; }
SEQ=$(git rev-list --count "$TAG..$REF") || exit 1
VERSION="$TAG-sw.$SEQ"
NAME="cc-connect-$VERSION-$GOOS-$GOARCH"
mkdir -p "$OUT" || exit 1
OUT=$(cd "$OUT" && pwd)
RUN=(docker run --rm -i --cpus "$CPUS" -v cc-connect-gocache:/go -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0)
if [ "${SKIP_TESTS:-0}" != 1 ]; then
  git archive "$REF" | "${RUN[@]}" "$GO_IMAGE" sh -c "mkdir /src && tar -x -C /src && cd /src && go test -tags $TAGS -p $CPUS -count=1 -timeout 30m $PKGS" || exit 3
fi
LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$SHA -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
git archive "$REF" | "${RUN[@]}" -e GOOS="$GOOS" -e GOARCH="$GOARCH" "$GO_IMAGE" sh -c "mkdir /src && tar -x -C /src && cd /src && go build -tags $TAGS -ldflags '$LDFLAGS' -o /tmp/cc-connect ./cmd/cc-connect >&2 && cat /tmp/cc-connect" > "$OUT/$NAME.part" || { rm -f "$OUT/$NAME.part"; exit 1; }
chmod +x "$OUT/$NAME.part" && mv "$OUT/$NAME.part" "$OUT/$NAME" || exit 1
sha256sum "$OUT/$NAME"
if [ "$GOOS" = linux ] && [ "$GOARCH" = "$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" ]; then "$OUT/$NAME" --version | head -1; fi
