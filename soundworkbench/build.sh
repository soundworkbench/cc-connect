#!/bin/bash
# Build a committed snapshot (default local); VERSION comes from that snapshot.
# GOOS/GOARCH=linux/amd64, CPUS=4, GO_IMAGE=golang:1.25, OUT=dist.
# SKIP_TESTS=1 is for local rebuilds only; release.sh always runs tests.
set -euo pipefail
REF=${1:-local}
GOOS=${GOOS:-linux}
GOARCH=${GOARCH:-amd64}
CPUS=${CPUS:-4}
GO_IMAGE=${GO_IMAGE:-golang:1.25}
OUT=${OUT:-dist}
TAGS=no_web,goolm
PKGS=(./agent/claudecode/ ./agent/codex/ ./agent/opencode/ ./core/ ./platform/feishu/ ./config/ ./cmd/cc-connect/)
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
[[ "$CPUS" =~ ^[1-9][0-9]*$ ]] || { echo 'CPUS must be a positive integer' >&2; exit 1; }
SOURCE=$(git rev-parse --verify "$REF^{object}")
SHA=$(git rev-parse --short=8 "$SOURCE")
VERSION=$(git show "$SOURCE:soundworkbench/VERSION")
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'soundworkbench/VERSION must contain vX.Y.Z' >&2; exit 1; }
NAME="cc-connect-$VERSION-$GOOS-$GOARCH"
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
RUN=(docker run --rm -i --cpus "$CPUS" -v cc-connect-gocache:/go -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e GOMAXPROCS="$CPUS")
if [ "${SKIP_TESTS:-0}" != 1 ]; then
  git archive "$SOURCE" | "${RUN[@]}" "$GO_IMAGE" sh -c '
    mkdir /src && tar -x -C /src && cd /src || exit 1
    tags=$1; cpus=$2; shift 2
    go test -tags "$tags" -p "$cpus" -count=1 -timeout 30m "$@"
  ' sh "$TAGS" "$CPUS" "${PKGS[@]}" || exit 3
fi
LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$SHA -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PART=$(mktemp "$OUT/$NAME.part.XXXXXX")
trap 'rm -f "$PART"' EXIT
git archive "$SOURCE" | "${RUN[@]}" -e GOOS="$GOOS" -e GOARCH="$GOARCH" "$GO_IMAGE" sh -c '
  mkdir /src && tar -x -C /src && cd /src || exit 1
  go build -p "$1" -tags "$2" -ldflags "$3" -o /tmp/cc-connect ./cmd/cc-connect >&2 && cat /tmp/cc-connect
' sh "$CPUS" "$TAGS" "$LDFLAGS" > "$PART"
chmod +x "$PART"
mv "$PART" "$OUT/$NAME"
sha256sum "$OUT/$NAME"
if [ "$GOOS" = "$(uname -s | tr '[:upper:]' '[:lower:]')" ] && [ "$GOARCH" = "$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" ]; then
  ACTUAL=$("$OUT/$NAME" --version)
  [ "$ACTUAL" = "cc-connect $VERSION" ] || { echo "unexpected --version: $ACTUAL" >&2; exit 1; }
  printf '%s\n' "$ACTUAL"
fi
