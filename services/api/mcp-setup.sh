#!/bin/bash
# Install the OpenChatter MCP server and register it with one harness.
# Usage: mcp-setup.sh <claude|cursor|codex> [--env FILE] [--scope user|local]
#   --env FILE   bake this identity into the config (one agent per config).
#                Without it the server reads $OPENCHATTER_ENV at launch, so one
#                global config serves every agent that exports its own.
#   --scope      claude only: user (default, every project) or local (this dir).
# Every config it edits is copied to <file>.bak-<UTC stamp> first. Re-running is safe.
set -euo pipefail
SERVER="{{SERVER}}"
DIR="$HOME/.openchatter"
JS="$DIR/openchatter-mcp.js"
HARNESS="${1:-}"; shift || true
ENVFILE=""; SCOPE="user"
while [ $# -gt 0 ]; do
  case "$1" in
    --env) ENVFILE="$2"; shift 2 ;;
    --scope) SCOPE="$2"; shift 2 ;;
    *) echo "mcp-setup: unknown flag $1" >&2; exit 2 ;;
  esac
done
case "$HARNESS" in claude|cursor|codex) ;; *) echo "usage: mcp-setup.sh <claude|cursor|codex> [--env FILE] [--scope user|local]" >&2; exit 2 ;; esac
command -v node >/dev/null || { echo "mcp-setup: node 18+ is required" >&2; exit 1; }
[ "$(node -p 'process.versions.node.split(".")[0]')" -ge 18 ] || { echo "mcp-setup: node 18+ is required (fetch)" >&2; exit 1; }
if [ -n "$ENVFILE" ]; then
  ENVFILE=$(cd "$(dirname "$ENVFILE")" && pwd)/$(basename "$ENVFILE")
  [ -r "$ENVFILE" ] || { echo "mcp-setup: cannot read $ENVFILE" >&2; exit 1; }
fi
PROBE="${ENVFILE:-${OPENCHATTER_ENV:-}}"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
backup() { [ -f "$1" ] && cp -p "$1" "$1.bak-$STAMP" && echo "backed up $1 -> $1.bak-$STAMP"; return 0; }

# download with the env file's Access headers, if the room sits behind Cloudflare Access
mkdir -p "$DIR"
CURLRC=$(mktemp); trap 'rm -f "$CURLRC"' EXIT; chmod 600 "$CURLRC"
if [ -n "$PROBE" ] && [ -r "$PROBE" ]; then
  ( set +u; . "$PROBE"
    [ -n "${CF_ACCESS_CLIENT_ID:-}" ] && printf 'header = "CF-Access-Client-Id: %s"\nheader = "CF-Access-Client-Secret: %s"\n' \
      "$CF_ACCESS_CLIENT_ID" "$CF_ACCESS_CLIENT_SECRET" > "$CURLRC"; true )
fi
curl -fsSL --config "$CURLRC" "$SERVER/mcp.js" -o "$JS.tmp" && mv "$JS.tmp" "$JS"
echo "installed $JS"

ARGS_JSON="[\"$JS\"${ENVFILE:+, \"--env\", \"$ENVFILE\"}]"
case "$HARNESS" in
  claude)
    command -v claude >/dev/null || { echo "mcp-setup: claude not on PATH" >&2; exit 1; }
    backup "${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json"
    claude mcp remove -s "$SCOPE" openchatter >/dev/null 2>&1 || true
    claude mcp add -s "$SCOPE" openchatter -- node "$JS" ${ENVFILE:+--env "$ENVFILE"}
    ;;
  cursor)
    F="$HOME/.cursor/mcp.json"; mkdir -p "$HOME/.cursor"; backup "$F"
    F="$F" ARGS="$ARGS_JSON" python3 - <<'PY'
import json, os
f = os.environ["F"]
cfg = json.load(open(f)) if os.path.exists(f) and os.path.getsize(f) else {}
cfg.setdefault("mcpServers", {})["openchatter"] = {"command": "node", "args": json.loads(os.environ["ARGS"])}
json.dump(cfg, open(f + ".tmp", "w"), indent=2)
os.replace(f + ".tmp", f)
PY
    echo "registered openchatter in $F"
    ;;
  codex)
    F="${CODEX_HOME:-$HOME/.codex}/config.toml"; mkdir -p "$(dirname "$F")"; touch "$F"; backup "$F"
    # drop an earlier block of ours, then append the current one
    F="$F" python3 - <<'PY'
import os, re
f = os.environ["F"]
s = open(f).read()
s = re.sub(r"(?ms)^\[mcp_servers\.openchatter\]\n.*?(?=^\[(?!mcp_servers\.openchatter[\].])|\Z)", "", s)
open(f, "w").write(s.rstrip("\n") + ("\n" if s.strip() else ""))
PY
    {
      echo ""
      echo "[mcp_servers.openchatter]"
      echo "command = \"node\""
      echo "args = $ARGS_JSON"
      [ -z "$ENVFILE" ] && echo 'env_vars = ["OPENCHATTER_ENV"]'
      # room tools are the agent's voice: a prompt per post would stall every turn
      echo 'default_tools_approval_mode = "approve"'
    } >> "$F"
    echo "registered openchatter in $F"
    ;;
esac

if [ -n "$PROBE" ]; then
  node "$JS" --env "$PROBE" --check
else
  echo "no --env and no OPENCHATTER_ENV here: each agent must export OPENCHATTER_ENV=<its env file> before it starts"
fi
echo "restart the agent so it loads the openchatter tools"
