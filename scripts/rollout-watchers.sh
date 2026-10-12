#!/bin/sh
# Regenerate every agent's watcher from the template the server serves now.
#
#   sh scripts/rollout-watchers.sh <room-slug> [--dry-run] [--template FILE] [--skill PATH]...
#
# For each ~/.openchatter/<room-slug>.<name>.watch.sh it keeps that file's ME,
# WATCH and BASE lines, backs the old file up next to it (.bak-<time>) and writes
# the new template with those three lines. The template comes from SERVER/skill/watch.sh,
# SERVER read from the first agent's env file, unless --template names a local copy.
# The new file replaces the old one by rename, never in place: a running sh reads
# its script as it goes, so an in-place write would corrupt a live watcher.
# Running watchers keep the old code until their agent restarts them.
#
# --skill PATH refreshes a local OpenChatter SKILL.md from SERVER/skill: its YAML
# front matter stays, the body is replaced. It is written in place on purpose, so
# a SKILL.md hardlinked into several homes stays one file. Backed up first.
set -eu

usage() { echo "usage: $0 <room-slug> [--dry-run] [--template FILE] [--skill PATH]..." >&2; exit 2; }
[ $# -ge 1 ] || usage
ROOM="$1"; shift
case "$ROOM" in ''|-*|*/*) usage;; esac
DRY=0; TEMPLATE=""; SKILLS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1;;
    --template) [ $# -ge 2 ] || usage; TEMPLATE="$2"; shift;;
    --skill) [ $# -ge 2 ] || usage; SKILLS="$SKILLS
$2"; shift;;
    *) usage;;
  esac
  shift
done

DIR="${OPENCHATTER_DIR:-$HOME/.openchatter}"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FILES=$(ls "$DIR/$ROOM".*.watch.sh 2>/dev/null || true)
[ -n "$FILES" ] || { echo "no $DIR/$ROOM.*.watch.sh to regenerate" >&2; exit 1; }

# fetch GETs a served page with the credentials of the first agent env file. A
# curl config file carries them, never argv: ps shows argv to every local user.
fetch() {
  ENVF=$(ls "$DIR/$ROOM".*.env 2>/dev/null | head -1)
  [ -n "$ENVF" ] || { echo "no $DIR/$ROOM.*.env to read SERVER from" >&2; exit 1; }
  SERVER=$(sed -n 's/^SERVER=//p' "$ENVF" | head -1 | tr -d '"'"'")
  [ -n "$SERVER" ] || { echo "no SERVER= in $ENVF" >&2; exit 1; }
  CFG="$TMP/curlrc"
  (umask 077; : > "$CFG"
   ID=$(sed -n 's/^CF_ACCESS_CLIENT_ID=//p' "$ENVF" | head -1 | tr -d '"'"'")
   SECRET=$(sed -n 's/^CF_ACCESS_CLIENT_SECRET=//p' "$ENVF" | head -1 | tr -d '"'"'")
   if [ -n "$ID" ] && [ -n "$SECRET" ]; then
     printf 'header = "CF-Access-Client-Id: %s"\nheader = "CF-Access-Client-Secret: %s"\n' "$ID" "$SECRET" > "$CFG"
   fi)
  curl -fsS --max-time 30 -K "$CFG" "$SERVER$1" -o "$2"
}

NEWT="$TMP/watch.sh"
if [ -n "$TEMPLATE" ]; then
  cp "$TEMPLATE" "$NEWT"
else
  fetch /skill/watch.sh "$NEWT"
fi
# refuse anything that is not the template: a proxy page would replace every watcher
head -1 "$NEWT" | grep -q '^#!/bin/sh' || { echo "the template is not a shell script, refusing" >&2; exit 1; }
VERSION=$(sed -n 's/^WATCHER_VERSION="\(.*\)"$/\1/p' "$NEWT" | head -1)
for key in ME WATCH BASE; do
  [ "$(grep -c "^$key=" "$NEWT")" = "1" ] || { echo "the template has no single $key= line, refusing" >&2; exit 1; }
done
[ -n "$VERSION" ] || { echo "the template has no WATCHER_VERSION, refusing" >&2; exit 1; }
sh -n "$NEWT" || { echo "the template does not parse, refusing" >&2; exit 1; }

N=0
for f in $FILES; do
  OLD=$(sed -n 's/^WATCHER_VERSION="\(.*\)"$/\1/p' "$f" | head -1)
  ok=1
  for key in ME WATCH BASE; do
    grep -q "^$key=" "$f" || { echo "SKIP $f: no $key= line, regenerate it by hand" >&2; ok=0; break; }
  done
  [ "$ok" = 1 ] || continue
  out="$TMP/$(basename "$f")"
  # awk swaps the three template lines for the agent's own, verbatim (ENVIRON,
  # not -v: -v would expand backslashes)
  RO_ME=$(grep -m1 '^ME=' "$f") RO_WATCH=$(grep -m1 '^WATCH=' "$f") RO_BASE=$(grep -m1 '^BASE=' "$f") awk '
    /^ME=/ && !m { print ENVIRON["RO_ME"]; m = 1; next }
    /^WATCH=/ && !w { print ENVIRON["RO_WATCH"]; w = 1; next }
    /^BASE=/ && !b { print ENVIRON["RO_BASE"]; b = 1; next }
    { print }' "$NEWT" > "$out"
  sh -n "$out" || { echo "SKIP $f: the regenerated file does not parse" >&2; continue; }
  if [ "$DRY" = 1 ]; then
    echo "would regenerate $f (${OLD:-unknown} -> $VERSION) keeping $(grep -m1 '^ME=' "$f" | cut -d'#' -f1)"
    continue
  fi
  cp -p "$f" "$f.bak-$STAMP"
  chmod 755 "$out"
  mv "$out" "$f.new.$$" && mv "$f.new.$$" "$f"
  echo "regenerated $f (${OLD:-unknown} -> $VERSION), backup $f.bak-$STAMP"
  N=$((N + 1))
done

if [ -n "$SKILLS" ]; then
  BODY="$TMP/skill.md"
  fetch /skill "$BODY"
  head -1 "$BODY" | grep -q '^# OpenChatter' || { echo "SERVER/skill is not the OpenChatter skill, refusing" >&2; exit 1; }
  printf '%s\n' "$SKILLS" | while IFS= read -r s; do
    [ -n "$s" ] || continue
    [ -f "$s" ] || { echo "SKIP $s: no such file" >&2; continue; }
    new="$TMP/SKILL.md"
    # keep a leading YAML front matter block, then the served body
    awk 'NR == 1 && $0 == "---" { fm = 1; print; next } fm { print; if ($0 == "---") exit }' "$s" > "$new"
    cat "$BODY" >> "$new"
    if [ "$DRY" = 1 ]; then
      echo "would refresh $s ($(wc -l < "$s" | tr -d ' ') -> $(wc -l < "$new" | tr -d ' ') lines, links: $(ls -l "$s" | awk '{print $2}'))"
      continue
    fi
    cp -p "$s" "$s.bak-$STAMP"
    cat "$new" > "$s"
    echo "refreshed $s in place, backup $s.bak-$STAMP"
  done
fi

[ "$DRY" = 1 ] || echo "done: $N watcher(s) on $VERSION. Each agent restarts its watcher to pick it up."
