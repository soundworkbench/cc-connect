#!/bin/bash
export PATH=/home/xiaoman/.local/bin:/usr/local/bin:/usr/bin:/bin
S=/srv/xiaoman/research/swcc/sessfix/src-git; D=/srv/xiaoman/research/cc-connect-release; log=$D/release.log
cd $S || exit 1
git fetch -q https://github.com/soundworkbench/cc-connect local && git checkout -q --detach FETCH_HEAD
REF=${1:?ref}
git merge-base --is-ancestor "$REF" FETCH_HEAD || { echo "refused $REF: not pushed to soundworkbench/cc-connect local" | tee -a $log; exit 1; }
OUT=$D/dist $S/soundworkbench/build.sh $REF > $D/build.out 2>&1 || { echo "build failed $(tail -3 $D/build.out)" >> $log; exit 1; }
BIN=$(ls -t $D/dist/cc-connect-*-linux-amd64 | head -1); NAME=$(basename $BIN); VER=${NAME#cc-connect-}; VER=${VER%-linux-amd64}
(cd $D/dist && sha256sum $NAME > $NAME.sha256)
echo "built $NAME $(cut -c1-64 $BIN.sha256) $($BIN --version | head -1)" >> $log
NOTES="Linux amd64 build of the \`local\` branch at ${REF:0:8}.

Includes the Feishu typing reaction, streaming card preview, and per-thread isolation of private chats. Steered messages are no longer lost when they arrive as a turn ends, and commands or stale card answers never steer into a running turn.

Verify: \`sha256sum -c $NAME.sha256\`"
gh release create "$VER" -R soundworkbench/cc-connect --target $(git -C $S rev-parse $REF) --title "$VER" --notes "$NOTES" $BIN $BIN.sha256 >> $log 2>&1 && echo "released $VER" >> $log
gh release view "$VER" -R soundworkbench/cc-connect --json assets -q '.assets[]|.url' >> $log 2>&1
