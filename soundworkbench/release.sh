#!/bin/bash
# One GitHub release entry point. Never installs or restarts a running bridge.
# Usage: soundworkbench/release.sh [--dry-run [--allow-unpushed]] [commit]
set -euo pipefail
REPO=soundworkbench/cc-connect
UPSTREAM_BASE=v1.5.2-beta.1
DRY_RUN=0
ALLOW_UNPUSHED=0
REF=HEAD
REF_SET=0
fail() { echo "release: $*" >&2; exit 1; }
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    --allow-unpushed) ALLOW_UNPUSHED=1 ;;
    --help|-h) echo 'Usage: soundworkbench/release.sh [--dry-run [--allow-unpushed]] [commit]'; exit 0 ;;
    -*) fail "unknown option: $arg" ;;
    *) [ "$REF_SET" = 0 ] || fail 'expected at most one commit'; REF=$arg; REF_SET=1 ;;
  esac
done
[ "$ALLOW_UNPUSHED" = 0 ] || [ "$DRY_RUN" = 1 ] || fail '--allow-unpushed requires --dry-run; it can never publish'
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
[ -z "$(git status --porcelain)" ] || fail 'working tree must be clean (commit changes first)'
SHA=$(git rev-parse --verify "$REF^{commit}")
# Use the scripts belonging to the selected source, rather than a different checkout.
[ "$SHA" = "$(git rev-parse HEAD)" ] || fail 'check out the selected commit before running release.sh'
case "$(git remote get-url origin)" in
  https://github.com/soundworkbench/cc-connect|https://github.com/soundworkbench/cc-connect.git|git@github.com:soundworkbench/cc-connect|git@github.com:soundworkbench/cc-connect.git|ssh://git@github.com/soundworkbench/cc-connect.git) ;;
  *) fail "origin must be $REPO" ;;
esac
# This reads GitHub and updates only the local tracking ref. It never pushes.
git fetch --no-tags origin +refs/heads/local:refs/remotes/origin/local
if ! git merge-base --is-ancestor "$SHA" refs/remotes/origin/local; then
  if [ "$ALLOW_UNPUSHED" = 1 ]; then
    echo 'release: WARNING: dry-run only; bypassing unpushed-commit check' >&2
  else
    fail "$SHA is not pushed to origin/local"
  fi
fi
VERSION=$(git show "$SHA:soundworkbench/VERSION")
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail 'soundworkbench/VERSION must contain vX.Y.Z'
if git show-ref --verify --quiet "refs/tags/$VERSION"; then
  fail "tag $VERSION already exists locally"
fi
REMOTE_TAG=$(git ls-remote --tags origin "refs/tags/$VERSION")
[ -z "$REMOTE_TAG" ] || fail "tag $VERSION already exists on origin"
# Include drafts when detecting collisions; match Bridge bump's newest published release.
PREVIOUS=$(gh api --paginate --slurp "repos/$REPO/releases?per_page=100" | jq -r --arg version "$VERSION" '
  [.[][]] | if any(.tag_name == $version) then error("release " + $version + " already exists")
  else [.[] | select(.draft == false)] | sort_by(.published_at) | last | .tag_name // empty end')
[ -n "$PREVIOUS" ] || fail 'no previous published release found'
git fetch --no-tags origin "refs/tags/$PREVIOUS:refs/tags/$PREVIOUS"
git merge-base --is-ancestor "$UPSTREAM_BASE" "$SHA" || fail "commit is not based on $UPSTREAM_BASE"
git merge-base --is-ancestor "$PREVIOUS^{commit}" "$SHA" || fail "previous release $PREVIOUS is not an ancestor of $SHA"
OUT=${OUT:-dist}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
NAME="cc-connect-$VERSION-linux-amd64"
BIN="$OUT/$NAME"
NOTES="$OUT/$VERSION-notes.md"
RECEIPT="$OUT/$VERSION-receipt.json"
# A release always tests the source and always builds linux-amd64.
GOOS=linux GOARCH=amd64 SKIP_TESTS=0 OUT="$OUT" soundworkbench/build.sh "$SHA"
ACTUAL=$("$BIN" --version)
[ "$ACTUAL" = "cc-connect $VERSION" ] || fail "unexpected --version: $ACTUAL"
(cd "$OUT" && sha256sum "$NAME" > "$NAME.sha256" && sha256sum -c "$NAME.sha256")
DIGEST=$(cut -d ' ' -f 1 "$BIN.sha256")
BUILT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '{"version":"%s","commit":"%s","base":"%s","previous":"%s","tags":"no_web,goolm","sha256":"%s","built":"%s"}\n' \
  "$VERSION" "$SHA" "$UPSTREAM_BASE" "$PREVIOUS" "$DIGEST" "$BUILT" > "$RECEIPT"
{
  printf '# %s\n\nUpstream baseline: `%s`.\n\nFork commit: `%s` (`origin/local`).\n\n' "$VERSION" "$UPSTREAM_BASE" "$SHA"
  printf '## Changes since %s\n\n' "$PREVIOUS"
  git log --reverse --format='- %h %s' "$PREVIOUS..$SHA"
  printf '\nCompare: https://github.com/%s/compare/%s...%s\n\n' "$REPO" "$PREVIOUS" "$SHA"
  printf 'Linux amd64; build tags: `no_web,goolm`; SHA-256: `%s`.\n\nVerify: `sha256sum -c %s.sha256`\n' "$DIGEST" "$NAME"
} > "$NOTES"
printf 'Release: %s\nCommit: %s\nPrevious: %s\nAssets: %s, %s.sha256\nNotes: %s\nReceipt: %s\n' \
  "$VERSION" "$SHA" "$PREVIOUS" "$NAME" "$NAME" "$NOTES" "$RECEIPT"
if [ "$DRY_RUN" = 1 ]; then
  echo 'Dry run passed: all remaining checks and build passed; no tag or Release created.'
  exit 0
fi
# Recheck immediately before the only remote mutation.
git fetch --no-tags origin +refs/heads/local:refs/remotes/origin/local
git merge-base --is-ancestor "$SHA" refs/remotes/origin/local || fail 'commit is no longer on origin/local'
REMOTE_TAG=$(git ls-remote --tags origin "refs/tags/$VERSION")
[ -z "$REMOTE_TAG" ] || fail "tag $VERSION appeared during the build"
gh release create "$VERSION" -R "$REPO" --target "$SHA" --title "$VERSION" --notes-file "$NOTES" "$BIN" "$BIN.sha256"
VERIFIED=$(gh release view "$VERSION" -R "$REPO" --json tagName,name,isDraft,assets | jq -r --arg version "$VERSION" --arg name "$NAME" '
  .tagName == $version and .name == $version and .isDraft == false and
  ([.assets[].name] | sort) == ([$name, ($name + ".sha256")] | sort)')
[ "$VERIFIED" = true ] || fail 'published Release title, tag or asset names do not match VERSION; inspect GitHub'
echo "Verified Release: https://github.com/$REPO/releases/tag/$VERSION"
