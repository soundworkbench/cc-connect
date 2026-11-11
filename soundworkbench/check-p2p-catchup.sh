#!/bin/bash
# Prove the restart regression fails on the original source, then run the
# current catchup tests. Uses only disposable Docker containers and mock APIs.
set -euo pipefail
BASE_REF=${1:-4a245e70}
CPUS=${CPUS:-4}
GO_IMAGE=${GO_IMAGE:-golang:1.25}
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
[[ "$CPUS" =~ ^[1-9][0-9]*$ ]] || { echo 'CPUS must be a positive integer' >&2; exit 1; }
REGRESSION="$PWD/platform/feishu/p2p_catchup_regression_test.go"
BEFORE_LOG=$(mktemp)
trap 'rm -f "$BEFORE_LOG"' EXIT
RUN=(docker run --rm -i --cpus "$CPUS" -v cc-connect-gocache:/go -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e GOMAXPROCS="$CPUS")
if git archive "$BASE_REF" | "${RUN[@]}" -v "$REGRESSION:/regression_test.go:ro" "$GO_IMAGE" sh -c '
  mkdir /src && tar -x -C /src && cd /src || exit 1
  cp /regression_test.go platform/feishu/p2p_catchup_regression_test.go
  go test -tags no_web,goolm -p "$1" -count=1 ./platform/feishu -run "^TestP2PCatchup_RecentOldPrivateMessage$" -v
' sh "$CPUS" > "$BEFORE_LOG" 2>&1; then
  cat "$BEFORE_LOG"
  echo 'Expected the original source to lose the recent private message' >&2
  exit 1
fi
cat "$BEFORE_LOG"
rg -q 'recent old private message was lost during restart' "$BEFORE_LOG"
"${RUN[@]}" -v "$PWD:/src:ro" -w /src "$GO_IMAGE" go test -tags no_web,goolm -p "$CPUS" -count=1 ./platform/feishu -run TestP2PCatchup -v
