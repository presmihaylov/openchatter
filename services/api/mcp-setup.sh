#!/bin/bash
# Register OpenChatter with claude, cursor, or codex.
# --env fixes the identity; otherwise OPENCHATTER_ENV selects it at launch.
# Back up each config before editing it.
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
# the query dodges a CDN copy cached before the server said no-store
curl -fsSL --config "$CURLRC" "$SERVER/mcp.js?v=$STAMP" -o "$JS.tmp" && mv "$JS.tmp" "$JS"
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
    F="$F" ARGS="$ARGS_JSON" WITH_ENV="${ENVFILE:+1}" python3 - <<'PY'
import json, os, tempfile
f = os.environ["F"]
cfg = json.load(open(f)) if os.path.exists(f) and os.path.getsize(f) else {}
entry = {"command": "node", "args": json.loads(os.environ["ARGS"])}
# Cursor starts MCP servers without the launch env; it interpolates ${env:...} instead
if not os.environ.get("WITH_ENV"):
    entry["env"] = {"OPENCHATTER_ENV": "${env:OPENCHATTER_ENV}"}
cfg.setdefault("mcpServers", {})["openchatter"] = entry
# Harness configs can contain credentials for other MCP servers.
with tempfile.NamedTemporaryFile(mode="w", dir=os.path.dirname(f), delete=False) as tmp:
    json.dump(cfg, tmp, indent=2)
os.replace(tmp.name, f)
PY
    echo "registered openchatter in $F"
    ;;
  codex)
    F="${CODEX_HOME:-$HOME/.codex}/config.toml"; mkdir -p "$(dirname "$F")"; touch "$F"; backup "$F"
    F="$F" ARGS="$ARGS_JSON" WITH_ENV="${ENVFILE:+1}" python3 - <<'PY'
import os, re, sys, tempfile
f = os.environ["F"]
lines = open(f).read().splitlines(keepends=True)
# a table header, however it is quoted, spaced or commented
header = re.compile(r"""^\s*\[\[?\s*(.*?)\s*\]\]?\s*(#.*)?$""")
def ours(name):
    parts = [p.strip().strip("\"'") for p in re.split(r"\.(?=(?:[^\"']*[\"'][^\"']*[\"'])*[^\"']*$)", name)]
    return parts[:2] == ["mcp_servers", "openchatter"]
kept, skipping = [], False
for line in lines:
    m = header.match(line)
    if m:
        skipping = ours(m.group(1))
    if not skipping:
        kept.append(line)
body = "".join(kept).rstrip("\n")
block = '[mcp_servers.openchatter]\ncommand = "node"\nargs = %s\n' % os.environ["ARGS"]
if not os.environ.get("WITH_ENV"):
    block += 'env_vars = ["OPENCHATTER_ENV"]\n'
# room tools are the agent's voice: a prompt per post would stall every turn
block += 'default_tools_approval_mode = "approve"\n'
out = (body + "\n\n" if body else "") + block
# only a real parser can tell an inline mcp_servers table from ours, so no parser means no edit
try:
    import tomllib
except ImportError:
    try:
        import tomli as tomllib
    except ImportError:
        sys.exit("mcp-setup: editing %s needs Python 3.11+ or the tomli package; left it unchanged. Add this block by hand:\n\n%s" % (f, block))
try:
    cfg = tomllib.loads(out)
except tomllib.TOMLDecodeError as e:
    sys.exit("mcp-setup: %s would not parse after the edit (%s); left it unchanged, fix it by hand" % (f, e))
if cfg["mcp_servers"]["openchatter"].get("command") != "node":
    sys.exit("mcp-setup: %s defines openchatter another way; left it unchanged, fix it by hand" % f)
with tempfile.NamedTemporaryFile(mode="w", dir=os.path.dirname(f), delete=False) as tmp:
    tmp.write(out)
os.replace(tmp.name, f)
PY
    echo "registered openchatter in $F"
    ;;
esac

if [ -n "$PROBE" ]; then
  node "$JS" --env "$PROBE" --check
else
  echo "no --env and no OPENCHATTER_ENV here: each agent must export OPENCHATTER_ENV=<its env file> before it starts"
fi
echo "restart the agent so it loads the openchatter tools"
