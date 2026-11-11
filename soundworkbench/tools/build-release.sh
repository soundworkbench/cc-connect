#!/bin/bash
# usage: build-release.sh <version> <change text> <base text> ; builds $SRC (default wt-stream) HEAD and installs under cc-connect-unified/releases/<version>
set -e
V=$1; CH=$2; BASE=$3
W=${SRC:-/srv/xiaoman/research/swcc/sessfix/wt-stream}
SHA=$(git -C $W rev-parse --short=8 HEAD); TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
OUT=/srv/xiaoman/research/swcc/sessfix/out/$V; mkdir -p $OUT
/srv/xiaoman/research/swcc/sessfix/gotest.sh $W sh -c "CGO_ENABLED=1 go build -trimpath -tags no_web,goolm -ldflags '-s -w -X main.version=$V -X main.commit=$SHA -X main.buildTime=$TS' -o /go/cc-connect-$V ./cmd/cc-connect"
mv /srv/xiaoman/research/swcc/sessfix/gocache/cc-connect-$V $OUT/cc-connect
H=$(sha256sum $OUT/cc-connect | cut -d' ' -f1)
printf '{"version":"%s","commit":"%s","base":"%s","tags":"no_web,goolm","sha256":"%s","built":"%s","change":"%s"}\n' "$V" "$SHA" "$BASE" "$H" "$TS" "$CH" > $OUT/receipt.json
D=/srv/xiaoman/cc-connect-unified/releases/$V
sudo -n install -d -m 755 $D
sudo -n install -m 755 $OUT/cc-connect $D/cc-connect
sudo -n install -m 600 $OUT/receipt.json $D/receipt.json
echo installed $D $SHA
