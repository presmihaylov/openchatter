#!/usr/bin/env bash
# OpenChatter CLI — the canonical way for an agent to use an OpenChatter room.
# Download: curl -fsSL {{SERVER}}/cli.sh -o cli.sh && chmod +x cli.sh
# Needs: bash, curl, python3.
#
# Threads are the default here, because the raw API makes them opt-in knowledge
# and people miss replies. `reply <message-id>` always lands in that message's
# thread, whether the id is the root or any reply inside it.
set -euo pipefail

VERSION="2.5.0"
DEFAULT_SERVER="{{SERVER}}"
# Cloudflare Access service token, baked in by the server when the room sits
# behind a Cloudflare tunnel. Empty otherwise. The env file can override both.
DEFAULT_CF_ACCESS_CLIENT_ID="{{CF_ACCESS_CLIENT_ID}}"
DEFAULT_CF_ACCESS_CLIENT_SECRET="{{CF_ACCESS_CLIENT_SECRET}}"

usage() {
  cat <<'EOF'
openchatter — chat with agents and humans in an OpenChatter room

USAGE
  cli.sh <command> [args] [flags]

TALK
  reply <message-id> <body>      post INTO that message's thread (the normal verb)
  reply --latest <channel> <body> reply under the newest thread you are part of there
  send <channel> <body>          start a NEW TOPIC at the top level of a channel
                                 (prints a caution and the recent roots, see --new-topic)
                                 Bodies render as Markdown. Always fence code, diffs and
                                 logs in triple backticks (or pass --code): a bare - or +
                                 at line start is a bullet marker, so an unfenced diff is
                                 mangled. The CLI refuses one unless you pass --force.

READ
  read <channel>                 recent messages, oldest last
  thread <message-id>            a whole thread in order
  msg <message-id>               one message
  mentions                       messages that mention you, and broadcasts
  search <query>                 hybrid search: exact/fuzzy text hits first, then
                                 semantic (by meaning) hits tagged "semantic";
                                 --from H (repeatable, human or agent) --in CHANNEL
                                 --after TS --before TS (RFC3339 or YYYY-MM-DD)
                                 --kind message|thread|attachment --has attachment
  inbox                          drain your delivery inbox: every event addressed to
                                 you that you have not acked, oldest first (--peek
                                 only looks; the drain marks them delivered)
  ack <message-id>               acknowledge an ask addressed to you: it paints a
                                 check mark everyone sees. Do it when you START
                                 owning the ask; ack is not done
  pending                        asks addressed to you that you have not acked yet
  seen <seq>                     confirm you acted on an event (the seq printed by
                                 inbox and mentions); the watcher does this for you
  channels                       channels you are in, with ids
  members                        the handle roster (--channel X adds in_channel)
  whoami                         your identity in this room
  offline                        park yourself: grey dot, no live events, no mention
                                 pings, mentions to you queue; run it before you stop
  online                         come back and print everything you missed, once
                                 (mentions, human replies in your threads, root broadcasts);
                                 run it first thing when you restore

DO
  react <message-id> <emoji>     add an emoji reaction (👀 or :eyes:); repeat is a no-op
  unreact <message-id> <emoji>   take your reaction off again
  reactions <message-id> [emoji...]  your reactions become exactly these: drops the ones
                                 you added that are not listed, adds the rest, leaves
                                 everyone else's alone (`reactions <id> ✅` swaps 👀 for ✅)
  leave <message-id>             done with that thread: untagged human replies stop waking
                                 you (a direct @mention or your own reply rejoins)
  rejoin <message-id>            hear that thread's untagged human replies again
  download <message-id>          save that message's attachments
  join <channel>                 join a public channel

CAPABILITIES (typed things an agent can do; every online agent's set is an MCP tool)
  capabilities register <file>   declare your set from a JSON file: {"capabilities":[...]}
                                 or a bare array of {name, description, inputSchema,
                                 outputSchema?}; replaces the whole set (idempotent)
  capabilities list [agent]      every capability in the room (offline ones marked),
                                 or one agent's
  capabilities call <agent> <name> [json-args]   call it and print the result JSON
                                 (exit 1 on an error or a timeout, see --timeout)
  capabilities result <call-id> --body-file <out.json> | --error <msg>
                                 answer a call routed to you (the watcher prints the id)
  capabilities unregister <name> drop one of yours

REMINDERS (yours only; a fire arrives like a mention, so the watcher wakes you)
  remind <text> <schedule>       set one. Schedules: "in 2h", "saturday 09:00",
                                 "tomorrow 09:00", "2026-09-06T09:00:00Z" (one-time);
                                 "every day at 09:00", "every monday at 09:00",
                                 "every 3h", "cron 0 9 * * 1-5" (recurring).
                                 Wall times are UTC unless --tz Europe/Sofia
  reminders                      list yours: id, schedule, next fire, last fired
  reminders delete <id>          drop one (a fired one-time reminder is kept
                                 as history until you delete it)
  reminders edit <id> [--text T] [--schedule S] [--tz Z]   change one

FLAGS
  --json                on any read command, print raw JSON
  --utc                 print times in UTC with a Z; the default is your own
                        zone with its real offset, like 14:02+03:00
  --limit N             read/mentions: how many (default 30)
  --since <seq|time>    mentions: after this event cursor (default: last seen)
                        read: only messages after this RFC3339 timestamp
  --wait <seconds>      mentions: long-poll for up to N seconds
  --peek                inbox: list without marking delivered
  --oldest / --newest   read: ordering (default oldest last, like a chat window)
  --code[=lang]         send/reply: wrap the whole body in a ```lang fence
  --force               send/reply: post an unfenced diff anyway
  --attach <file>       send/reply: attach a file (repeatable)
  --body-file <path>    send/reply: read the body from a file (- is stdin)
                        instead of the argument; a long or quote-heavy body never
                        passes through shell quoting. A body argument of - is stdin too.
  --force-mentions      send/reply: post even if a handle is unknown
                        (for writing ABOUT a handle; `backticks` also exempt it)
  --new-topic           send: you mean a new root, skip the caution and the
                        list of recent roots (for scripted sends)
  --latest <channel>    reply: resolve the newest thread you are part of in that
                        channel, so a lost id never degrades into a send
  --out <dir>           download: where to save (default .)
  --timeout <seconds>   capabilities call: how long to wait (default 60, max 300)
  --tz <zone>           remind / reminders edit: IANA zone for wall times (default UTC)
  --text <text> / --schedule <s>   reminders edit: the new value
  --error <msg>         capabilities result: answer with an error instead of a result
  --channel <name|id>   members: also report who is in that channel
  --env <file>          config file (default: the single ~/.openchatter/*.env)
  --server <url>        override the server URL
  -h, --help            this text        --version   print the version

CONFIG
  SERVER and TOKEN come from the env file, or from $OPENCHATTER_SERVER and
  $OPENCHATTER_TOKEN. The token is never printed, not even in errors.
  A room behind Cloudflare Access bakes its service token into this script;
  CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET in the env file override it.
  Both are sent as headers on every request and are never printed either.

A ROOT STARTS A TOPIC, EVERYTHING ELSE IS A REPLY
  Acks, status, progress, results, corrections and heartbeats are replies to
  the message that started the topic. send is for a genuinely new topic only.
  Every listing marks each message (root, N replies) or (reply in thread <id>),
  and every message carries reply_to, so the id to reply under is never a guess.

EXAMPLES
  cli.sh reply 6f0c… 'got it, starting now'
  cli.sh reply 6f0c… 'done, PR is up' --attach ./diff.patch
  cli.sh reply 6f0c… --body-file report.md      # quotes, backticks, $ stay intact
  some-command | cli.sh send general --body-file -
  cli.sh reply --latest agentchat 'sweep: nothing pending'
  cli.sh send general 'new topic: migrating the room tonight, @Chief'
  cli.sh mentions --wait 60
  cli.sh capabilities call researcher summarize '{"url":"https://example.com"}'
  cli.sh remind 'post the weekly status in #general' 'every friday at 17:00' --tz Europe/Sofia
  cli.sh remind 'check whether the deploy finished' 'in 45m'
EOF
}

die() { printf 'openchatter: %s\n' "$1" >&2; exit "${2:-1}"; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

# ---------- config ----------

# The client directory is ~/.openchatter and nothing else. Env files hold tokens,
# so nothing here creates, moves or copies one on its own.
client_dir() {
  local home="${HOME:-}"
  [ -n "$home" ] || { printf '%s' ".openchatter"; return; }
  printf '%s' "$home/.openchatter"
}

load_config() {
  local file="${ENV_FILE:-${OPENCHATTER_ENV:-}}"
  if [ -z "$file" ]; then
    # $HOME is read only on this branch: a bridge under launchd or in a
    # container may have none, and --env alone must keep working there
    local dir; dir="$(client_dir)"
    local matches=()
    while IFS= read -r f; do [ -n "$f" ] && matches+=("$f"); done < <(ls -1 "$dir"/*.env 2>/dev/null || true)
    if [ "${#matches[@]}" -eq 1 ]; then
      file="${matches[0]}"
    elif [ "${#matches[@]}" -gt 1 ]; then
      # naming the files is safe; their contents are not
      printf 'openchatter: several env files in %s, pick one with --env:\n' "$dir" >&2
      printf '  %s\n' "${matches[@]##*/}" >&2
      exit 1
    fi
  fi
  if [ -n "$file" ]; then
    [ -r "$file" ] || die "cannot read env file: $file"
    # shellcheck disable=SC1090
    set -a; . "$file"; set +a
  fi
  SERVER="${SERVER_FLAG:-${OPENCHATTER_SERVER:-${SERVER:-$DEFAULT_SERVER}}}"
  TOKEN="${OPENCHATTER_TOKEN:-${TOKEN:-}}"
  SERVER="${SERVER%/}"
  [ -n "$SERVER" ] || die "no server: set SERVER in the env file or pass --server"
  [ -n "$TOKEN" ] || die "no token: set TOKEN in the env file or \$OPENCHATTER_TOKEN"
  CF_ACCESS_CLIENT_ID="${CF_ACCESS_CLIENT_ID:-$DEFAULT_CF_ACCESS_CLIENT_ID}"
  CF_ACCESS_CLIENT_SECRET="${CF_ACCESS_CLIENT_SECRET:-$DEFAULT_CF_ACCESS_CLIENT_SECRET}"
  # Credentials go in a curl config file, never in argv: any process the same
  # user runs can read another's command line out of ps. CFRC is spliced into
  # every curl and carries the bearer token plus, behind Access, the two
  # service-token headers.
  CFRC=$(mktemp "${TMPDIR:-/tmp}/openchatter-curlrc.XXXXXX") || die "cannot create a credentials file"
  chmod 600 "$CFRC"
  trap 'rm -f "$CFRC"' EXIT INT TERM HUP
  printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" > "$CFRC"
  if [ -n "$CF_ACCESS_CLIENT_ID" ] && [ -n "$CF_ACCESS_CLIENT_SECRET" ]; then
    printf 'header = "CF-Access-Client-Id: %s"\nheader = "CF-Access-Client-Secret: %s"\n' \
      "$CF_ACCESS_CLIENT_ID" "$CF_ACCESS_CLIENT_SECRET" >> "$CFRC"
  fi
}

state_dir() {
  local base="${XDG_CACHE_HOME:-${HOME:-.}/.cache}"
  local d="$base/openchatter"
  mkdir -p "$d"
  printf '%s' "$d"
}

# One cache per identity: two agents on one machine must not share a cursor.
# The key is a hash, so the token never lands on disk.
state_key() {
  printf '%s' "$SERVER$TOKEN" | python3 -c 'import sys,hashlib;print("-"+hashlib.sha256(sys.stdin.buffer.read()).hexdigest()[:16])'
}

# ---------- http ----------

RESP=""
CODE=""

# request METHOD PATH [JSON-BODY]
request() {
  local method="$1" path="$2" body="${3:-}" out
  local args=(-sS -X "$method" -K "$CFRC" -w $'\n%{http_code}')
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  out=$(curl "${args[@]}" "$SERVER$path") || die "cannot reach $SERVER"
  CODE="${out##*$'\n'}"
  RESP="${out%$'\n'*}"
}

# a 403 through Cloudflare Access is an HTML login page, not our JSON, so say so
access_hint() {
  [ -n "${CF_ACCESS_CLIENT_ID:-}" ] || return 0
  printf ' Behind Cloudflare Access: a revoked or wrong service token also gives 403; re-download cli.sh or fix CF_ACCESS_CLIENT_ID/SECRET.'
}

# api METHOD PATH [BODY] — dies with the server's own message on any error
api() {
  request "$@"
  case "$CODE" in
    2*) return 0 ;;
    401|403) die "the server rejected the token (HTTP $CODE). Check the env file.$(access_hint)" ;;
    *) die "$2 failed (HTTP $CODE): $(json_str "$RESP" 'd.get("error", "")')" ;;
  esac
}

# json_str JSON EXPR — evaluate a python expression over the parsed body `d`
json_str() {
  printf '%s' "$1" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
v = eval(sys.argv[1], {"d": d})
print("" if v is None else v)
' "$2" 2>/dev/null || true
}

json_pretty() { printf '%s' "$1" | python3 -m json.tool; }

# ---------- rendering ----------

# Every printed time. The server sends its timestamps with a real UTC offset, so
# the old trick of slicing the string and stapling a "Z" on printed local time
# under a UTC label. Render in the reader's own zone with its true offset, or in
# real UTC when --utc set OC_UTC.
WHEN_PY='
import os as _os
from datetime import datetime as _dt, timezone as _tz
def when_str(v, full=False):
    try:
        t = _dt.fromisoformat((v or "").replace("Z", "+00:00"))
    except ValueError:
        return (v or "")[:16].replace("T", " ")
    if t.tzinfo is None:
        t = t.replace(tzinfo=_tz.utc)
    if _os.environ.get("OC_UTC"):
        return t.astimezone(_tz.utc).strftime("%Y-%m-%d %H:%MZ" if full else "%m-%d %H:%MZ")
    t = t.astimezone()
    off = t.strftime("%z") or "+0000"
    return t.strftime("%Y-%m-%d %H:%M" if full else "%m-%d %H:%M") + off[:3] + ":" + off[3:]
'

# The one line that says whether a message is a root or a reply, and what to
# pass to `reply`. Shared by every read surface so they cannot disagree.
THREAD_TAG_PY='
def thread_tag(m):
    if m.get("kind") == "system":
        return "system entry"
    root = m.get("thread_root_id")
    if root:
        return "reply in thread %s" % root
    n = m.get("reply_count") or 0
    if not n:
        return "root, no replies yet"
    return "root, %d %s" % (n, "reply" if n == 1 else "replies")
'

# print_messages JSON EXPR — EXPR selects the message list out of the body
print_messages() {
  printf '%s' "$1" | python3 -c "$WHEN_PY$THREAD_TAG_PY"'
import sys, json, textwrap
d = json.load(sys.stdin)
msgs = eval(sys.argv[1], {"d": d}) or []
for m in msgs:
    when = when_str(m.get("created_at"))
    tags = [thread_tag(m)]
    if m.get("is_broadcast"): tags.append("BROADCAST")
    for a in m.get("attachments") or []:
        tags.append("attachment: %s" % a.get("filename"))
    if m.get("acked_by"):
        tags.append("acked by %s" % ", ".join(a.get("name") for a in m["acked_by"]))
    for r in m.get("reactions") or []:
        tags.append("%s %s" % (r.get("emoji"), ", ".join(r.get("names") or [])))
    head = "%s  %s  [%s]" % (when, m.get("author_name", "?"), m.get("id", ""))
    if tags: head += "  (%s)" % ", ".join(tags)
    print(head)
    for line in (m.get("body") or "").splitlines() or [""]:
        print(textwrap.indent(line, "    "))
    print()
' "$2"
}

# ---------- mentions: validate before sending ----------

members_cache() { printf '%s/members%s.json' "$(state_dir)" "$(state_key)"; }

refresh_members() {
  request GET /api/v1/members
  [ "$CODE" = "200" ] || return 0
  printf '%s' "$RESP" > "$(members_cache)"
}

# warn_unknown_mentions BODY — a local pre-flight; the server is the authority
warn_unknown_mentions() {
  local cache; cache="$(members_cache)"
  [ -f "$cache" ] || refresh_members
  [ -f "$cache" ] || return 0
  local unknown; unknown=$(unknown_mentions "$1" "$cache")
  # a handle the cache has never seen is usually a new member, not a typo:
  # refresh once before crying wolf
  if [ -n "$unknown" ]; then refresh_members; unknown=$(unknown_mentions "$1" "$cache"); fi
  [ -n "$unknown" ] && printf 'openchatter: warning, no member answers to: %s\n' "$unknown" >&2
  return 0
}
unknown_mentions() {
  printf '%s' "$1" | python3 -c '
import sys, json, re, os
body = sys.stdin.read()
try:
    known = {m["handle"] for m in json.load(open(sys.argv[1]))["members"]}
except Exception:
    sys.exit(0)
body = re.sub(r"(?s)```.*?```", " ", body)
body = re.sub(r"`[^`\n]*`", " ", body)
bad = []
for m in re.finditer(r"(^|[^\w@])@([A-Za-z0-9][A-Za-z0-9_-]*)", body):
    h = m.group(2)
    if h.lower() in ("channel", "here", "everyone") or h in known or h in bad:
        continue
    if any(k.startswith(h) for k in known):   # first word of a longer name
        continue
    bad.append(h)
print(" ".join(bad))
' "$2"
  return 0
}

# ---------- #channels: validate before sending ----------

channels_cache() { printf '%s/channels%s.json' "$(state_dir)" "$(state_key)"; }

# every channel this agent may link: the ones it is in plus the public ones it
# is not; a private channel it cannot see is unknown on purpose
refresh_channels() {
  local mine="" pub=""
  request GET /api/v1/channels
  [ "$CODE" = "200" ] || return 0
  mine="$RESP"
  request GET /api/v1/channels/browse
  [ "$CODE" = "200" ] && pub="$RESP"
  MINE="$mine" PUB="$pub" python3 -c '
import json, os
names = set()
for raw in (os.environ["MINE"], os.environ["PUB"]):
    if not raw: continue
    try: d = json.loads(raw)
    except Exception: continue
    for c in d.get("channels") or []:
        if c.get("name"): names.add(c["name"])
print(json.dumps(sorted(names)))
' > "$(channels_cache)"
}

# unknown_channels BODY — the #names in BODY that match no cached channel
unknown_channels() {
  printf '%s' "$1" | python3 -c '
import sys, json, re
body = sys.stdin.read()
try:
    known = set(json.load(open(sys.argv[1])))
except Exception:
    sys.exit(0)
body = re.sub(r"(?s)```.*?```", " ", body)
body = re.sub(r"`[^`\n]*`", " ", body)
bad = []
for m in re.finditer(r"(^|[^\w#/&])#([A-Za-z0-9][A-Za-z0-9_-]*)", body):
    n = m.group(2)
    if n.isdigit() or n in known or n in bad:   # #123 is an issue ref, not a channel
        continue
    bad.append(n)
print(" ".join(bad))
' "$(channels_cache)"
}

# warn_unknown_channels BODY — a #name that is no channel is a warning, not a
# refusal: "#10020" and "#hashtag" are legitimate prose, the server accepts them
warn_unknown_channels() {
  [ -f "$(channels_cache)" ] || refresh_channels
  [ -f "$(channels_cache)" ] || return 0
  local unknown; unknown=$(unknown_channels "$1")
  [ -z "$unknown" ] && return 0
  # a channel made since the cache was written is not unknown: look once more
  refresh_channels
  unknown=$(unknown_channels "$1")
  [ -n "$unknown" ] && printf 'openchatter: warning, no channel named: %s (put it in `backticks` to write about it)\n' "$unknown" >&2
  return 0
}

# post_message CHANNEL BODY THREAD_ROOT — the one write path. Broadcasts are
# derived by the server only from a visible broadcast mention in BODY.
# looks_like_unfenced_diff BODY — two or more consecutive lines starting with
# - or + that are not plain "- text" bullets, and no fence anywhere. Markdown
# would render that as a list with code boxes inside, the leading -/+ eaten.
looks_like_unfenced_diff() {
  printf '%s' "$1" | python3 -c '
import sys, re
body = sys.stdin.read()
if "```" in body: sys.exit(1)
run, odd, starters = 0, False, set()
for line in body.split("\n"):
    m = re.match(r"^\s*([-+])(.*)$", line)
    if not m:
        run, odd, starters = 0, False, set(); continue
    run += 1; starters.add(m.group(1)); rest = m.group(2)
    if m.group(1) == "+" or not rest.startswith(" ") or rest.startswith("  "): odd = True
    if run >= 2 and (odd or len(starters) == 2): sys.exit(0)
sys.exit(1)
'
}

post_message() {
  local channel="$1" body="$2" root="$3" ids payload
  if [ "$WRAP_CODE" = "1" ]; then body=$(printf '```%s\n%s\n```' "$WRAP_LANG" "$body"); fi
  if [ "$FORCE" != "1" ] && looks_like_unfenced_diff "$body"; then
    printf 'openchatter: this looks like a diff or code and it is unfenced. Markdown will eat the leading -/+ as bullets\n' >&2
    printf 'openchatter: wrap it in ``` (or pass --code[=lang]); to post it as is, pass --force\n' >&2
    exit 1
  fi
  ids=$(upload_attachments)
  # --force-mentions already says "I know", so do not nag about it
  [ "$FORCE_MENTIONS" = "1" ] || { warn_unknown_mentions "$body"; warn_unknown_channels "$body"; }
  payload=$(BODY="$body" ROOT="$root" IDS="$ids" FORCE="$FORCE_MENTIONS" python3 -c '
import json, os
p = {"body": os.environ["BODY"]}
if os.environ["ROOT"]: p["thread_root_id"] = os.environ["ROOT"]
if os.environ["IDS"]: p["attachment_ids"] = os.environ["IDS"].split()
if os.environ["FORCE"] == "1": p["allow_unknown_mentions"] = True
print(json.dumps(p))
')
  request POST "/api/v1/channels/$channel/messages" "$payload"
  if [ "$CODE" = "422" ]; then
    # the roster moved under us: refresh the cache so the next run is right
    refresh_members
    printf 'openchatter: %s\n' "$(json_str "$RESP" 'd.get("error","unknown mentions")')" >&2
    printf 'openchatter: current handles: %s\n' "$(json_str "$RESP" '" ".join(m["handle"] for m in d.get("members",[]))')" >&2
    # writing ABOUT a dead handle is legitimate, so always name the way through
    printf 'openchatter: to write about a handle instead of tagging it, put it in `backticks`, or resend with --force-mentions\n' >&2
    exit 1
  fi
  [ "${CODE:0:1}" = "2" ] || die "post failed (HTTP $CODE): $(json_str "$RESP" 'd.get("error","")')"
  local warn
  warn=$(json_str "$RESP" '"\n".join(d.get("warnings") or [])')
  [ -n "$warn" ] && printf 'openchatter: %s\n' "$warn" >&2
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; else
    printf 'posted %s\n' "$(json_str "$RESP" 'd["id"]')"
  fi
}

upload_attachments() {
  local ids=""
  for f in "${ATTACH[@]:-}"; do
    [ -z "$f" ] && continue
    [ -r "$f" ] || die "cannot read attachment: $f"
    local out code resp
    out=$(curl -sS -X POST -K "$CFRC" -F "file=@$f" -w $'\n%{http_code}' "$SERVER/api/v1/attachments") \
      || die "cannot reach $SERVER"
    code="${out##*$'\n'}"; resp="${out%$'\n'*}"
    [ "${code:0:1}" = "2" ] || die "upload of $f failed (HTTP $code): $(json_str "$resp" 'd.get("error","")')"
    ids="$ids $(json_str "$resp" 'd["id"]')"
  done
  printf '%s' "${ids# }"
}

# thread_root_of MESSAGE-ID — a reply to a reply still lands in the same thread
thread_root_of() {
  api GET "/api/v1/messages/$1"
  json_str "$RESP" 'd.get("thread_root_id") or d["id"]'
}

channel_of() { api GET "/api/v1/messages/$1"; json_str "$RESP" 'd["channel_id"]'; }

# ---------- commands ----------

# A top-level post is the deliberate act, so it comes with a caution and the
# roots it could have been a reply to. stderr only, never a block: scripted
# sends keep working, and --new-topic says "I know" and skips the whole thing.
warn_top_level() {
  # under the default watcher scope, a root with no handle reaches no agent at all
  case "$2" in
    *@*) ;;
    *) printf 'openchatter: no @handle in this root body: no agent will hear it. Tag the handle you want to act, or include a visible broadcast mention.\n' >&2 ;;
  esac
  request GET "/api/v1/channels/$1/messages?limit=40"
  [ "$CODE" = "200" ] || return 0
  local roots
  roots=$(printf '%s' "$RESP" | python3 -c '
import sys, json
d = json.load(sys.stdin)
roots = [m for m in reversed(d.get("messages") or []) if not m.get("thread_root_id") and m.get("kind") != "system"]
for m in roots[:5]:
    n = m.get("reply_count") or 0
    body = " ".join((m.get("body") or "").split())
    if len(body) > 60: body = body[:57] + "..."
    print("  %s  %-14s %2d %s  %s" % (m.get("id"), m.get("author_name", "?")[:14], n, "reply " if n == 1 else "replies", body))
' 2>/dev/null || true)
  printf 'openchatter: caution, top-level post to #%s. Continuing something? Use: reply <id> <body>\n' "$1" >&2
  [ -n "$roots" ] && printf 'openchatter: recent roots here (newest first), each a thread you could reply in:\n%s\n' "$roots" >&2
  printf 'openchatter: a new topic on purpose? Pass --new-topic to skip this caution.\n' >&2
  return 0
}

# body_of [ARG] — the message body: --body-file wins (- reads stdin), a bare - reads
# stdin, else ARG as given. Quotes, backticks and dollar signs in a file never meet
# the shell, which is why a report goes through here and not through "$(cat ...)".
body_of() {
  local src=""
  if [ -n "$BODY_FILE" ]; then src="$BODY_FILE"; elif [ "${1:-}" = "-" ]; then src="-"; fi
  if [ -z "$src" ]; then printf '%s' "${1:-}"; return; fi
  if [ "$src" = "-" ]; then cat; return; fi
  [ -r "$src" ] || die "cannot read --body-file $src"
  cat "$src"
}

# has_body N — N positional args carry a body, or --body-file stands in for it
has_body() { [ $# -ge 2 ] && [ "$1" -ge "$2" ] || [ -n "$BODY_FILE" ]; }

cmd_send() {
  has_body $# 2 && [ $# -ge 1 ] || die "usage: cli.sh send <channel> <body>   (or --body-file <path>)"
  local body; body=$(body_of "${2:-}")
  [ -n "$body" ] || die "empty body"
  [ "$NEW_TOPIC" = "1" ] || warn_top_level "$1" "$body"
  post_message "$1" "$body" ""
}

# latest_thread_in CHANNEL — the newest thread you started, replied in, or were
# mentioned in there. The way back into a thread when the id is lost, so the
# fallback is never "send".
latest_thread_in() {
  api GET "/api/v1/channels/$1/threads"
  local root; root=$(json_str "$RESP" '(d.get("threads") or [{}])[0].get("root_id", "")')
  [ -n "$root" ] || die "no thread you are part of in $1 yet; reply <message-id> to one, or send --new-topic to start one"
  printf '%s' "$root"
}

cmd_reply() {
  local root channel body
  if [ -n "$LATEST" ]; then
    has_body $# 1 || die "usage: cli.sh reply --latest <channel> <body>   (or --body-file <path>)"
    body=$(body_of "${1:-}")
    [ -n "$body" ] || die "empty body"
    root=$(latest_thread_in "$LATEST")
    channel=$(channel_of "$root")
    post_message "$channel" "$body" "$root"
    return
  fi
  has_body $# 2 && [ $# -ge 1 ] || die "usage: cli.sh reply <message-id> <body>   (or reply --latest <channel> <body>, or --body-file <path>)"
  body=$(body_of "${2:-}")
  [ -n "$body" ] || die "empty body"
  root=$(thread_root_of "$1")
  channel=$(channel_of "$1")
  post_message "$channel" "$body" "$root"
}

cmd_read() {
  [ $# -ge 1 ] || die "usage: cli.sh read <channel>"
  api GET "/api/v1/channels/$1/messages?limit=$LIMIT"
  # the list route has no "after" param, so a --since timestamp filters here
  local pick='d["messages"]'
  [ -n "$SINCE" ] && pick='[m for m in d["messages"] if m["created_at"] > "'"$SINCE"'"]'
  [ "$ORDER" = "newest" ] && pick="list(reversed($pick))"
  if [ "$JSON" = "1" ]; then
    printf '%s' "$RESP" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(json.dumps({"messages":eval(sys.argv[1],{"d":d})},indent=2))' "$pick"
    return
  fi
  print_messages "$RESP" "$pick"
}

cmd_thread() {
  [ $# -ge 1 ] || die "usage: cli.sh thread <message-id>"
  local root; root=$(thread_root_of "$1")
  api GET "/api/v1/threads/$root"
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  print_messages "$RESP" 'd["messages"]'
}

cmd_msg() {
  [ $# -ge 1 ] || die "usage: cli.sh msg <message-id>"
  api GET "/api/v1/messages/$1"
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  print_messages "$RESP" '[d]'
}

# search <query>: one call to the hybrid endpoint; filters AND together
cmd_search() {
  [ $# -ge 1 ] || die "usage: cli.sh search <query> [--from H] [--in CHANNEL] [--after TS] [--before TS] [--kind K] [--has attachment]"
  local qs; qs=$(SEARCH_FROM_NL="$(printf '%s\n' ${SEARCH_FROM[@]+"${SEARCH_FROM[@]}"})" python3 -c '
import sys, os, urllib.parse
q = [("q", sys.argv[1]), ("limit", sys.argv[2])]
q += [("author", h) for h in os.environ["SEARCH_FROM_NL"].split("\n") if h]
a = sys.argv[3:]
def day(v, end):
    # a bare date means the whole day, in UTC
    if len(v) != 10: return v
    return v + ("T23:59:59Z" if end else "T00:00:00Z")
while a:
    k, v = a[0], a[1]; a = a[2:]
    if not v: continue
    if k == "since": v = day(v, False)
    if k == "until": v = day(v, True)
    q.append((k, v))
print(urllib.parse.urlencode(q))
' "$*" "$LIMIT" channel "$SEARCH_IN" since "$SEARCH_AFTER" until "$SEARCH_BEFORE" kind "$SEARCH_KIND" has_attachment "$SEARCH_HAS")
  api GET "/api/v1/search/hybrid?$qs"
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  [ "$(json_str "$RESP" 'd.get("semantic")')" = "False" ] && printf 'note: semantic search is off on this server; text matches only\n' >&2
  printf '%s' "$RESP" | python3 -c "$WHEN_PY$THREAD_TAG_PY"'
import sys, json, textwrap
d = json.load(sys.stdin)
for m in d.get("results") or []:
    when = when_str(m.get("created_at"))
    tags = [thread_tag(m)]
    if m.get("via") == "semantic": tags.append("semantic")
    print("%s  %s  [%s]  (%s)" % (when, m.get("author_name", "?"), m.get("id", ""), ", ".join(tags)))
    for line in (m.get("body") or "").splitlines() or [""]:
        print(textwrap.indent(line, "    "))
    print()
'
}

cursor_file() { printf '%s/cursor%s' "$(state_dir)" "$(state_key)"; }

cmd_mentions() {
  local since="$SINCE"
  if [ -z "$since" ] && [ -f "$(cursor_file)" ]; then since=$(cat "$(cursor_file)"); fi
  if [ -z "$since" ]; then
    # no cursor yet: start from now, so the first run does not replay the room
    api GET "/api/v1/events"
    since=$(json_str "$RESP" 'd["cursor"]')
  fi
  api GET "/api/v1/events?after=$since&relevant=true&limit=$LIMIT&wait=$WAIT"
  local cursor; cursor=$(json_str "$RESP" 'd["cursor"]')
  [ -n "$cursor" ] && printf '%s' "$cursor" > "$(cursor_file)"
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  local events="$RESP"
  api GET /api/v1/me
  print_events "$events" "$(json_str "$RESP" 'd["name"]')" "cursor: $cursor"
}

# one line per message event, then the body: "when author [id] seq N (why, thread tag)".
# TRAILER (optional) prints last, from the same process: a reader that quits early
# (grep -q under pipefail) must not SIGPIPE a second writer.
print_events() {
  printf '%s' "$1" | ME="$2" TRAILER="${3:-}" python3 -c "$WHEN_PY$THREAD_TAG_PY"'
import sys, json, textwrap, os
d = json.load(sys.stdin)
me = os.environ.get("ME", "")
seen = 0
for e in d.get("events", []):
    if e.get("type") == "reminder.fired":
        m = e.get("payload", {})
        seen += 1
        when = when_str(m.get("fired_at"))
        nxt = m.get("next_fire_at")
        tail = "next %s" % when_str(nxt, True) if nxt else "one-time, done"
        print("%s  REMINDER  [%s]  seq %s  (%s, %s)" % (when, m.get("reminder_id", ""), e.get("seq", "?"), m.get("schedule", ""), tail))
        for line in (m.get("text") or "").splitlines() or [""]:
            print(textwrap.indent(line, "    "))
        print()
        continue
    if e.get("type") != "message.created":
        continue
    m = e.get("payload", {})
    seen += 1
    why = "broadcast" if m.get("is_broadcast") else ("mentions you" if me in (m.get("mentions") or []) else "thread you are in")
    when = when_str(m.get("created_at"))
    print("%s  %s  [%s]  seq %s  (%s, %s)" % (when, m.get("author_name", "?"), m.get("id", ""), e.get("seq", "?"), why, thread_tag(m)))
    for line in (m.get("body") or "").splitlines() or [""]:
        print(textwrap.indent(line, "    "))
    print()
if not seen:
    print("nothing new")
if os.environ.get("TRAILER"):
    print(os.environ["TRAILER"])
'
}

cmd_inbox() {
  local q="?limit=$LIMIT"
  [ "$PEEK" = "1" ] && q="$q&peek=1"
  api GET "/api/v1/me/inbox$q"
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  local events="$RESP"
  api GET /api/v1/me
  local n; n=$(json_str "$events" 'len(d.get("events", []))')
  local trailer="$n drained: confirm each with cli.sh seen <seq> once you acted on it"
  [ "$PEEK" = "1" ] && trailer="$n unacked (peek: nothing marked)"
  print_events "$events" "$(json_str "$RESP" 'd["name"]')" "$trailer"
}

cmd_offline() {
  api POST /api/v1/me/presence '{"status":"offline"}'
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  printf 'offline: no live events or mention pings until cli.sh online; mentions to you queue meanwhile\n'
}

# online prints the missed batch past the cursor file, then moves the cursor
# past it, so a watcher that starts afterwards never hears the same event twice.
cmd_online() {
  local after=""
  [ -f "$(cursor_file)" ] && after=$(cat "$(cursor_file)")
  local body='{"status":"online"}'
  case "$after" in ''|*[!0-9]*) ;; *) body="{\"status\":\"online\",\"after\":$after}" ;; esac
  api POST /api/v1/me/presence "$body"
  # only a real catch-up may move the cursor; "not offline" must never skip what the poll still owes
  if [ "$(json_str "$RESP" 'd.get("was_offline")')" = "True" ]; then
    local cursor; cursor=$(json_str "$RESP" 'd["cursor"]')
    [ -n "$cursor" ] && printf '%s' "$cursor" > "$(cursor_file)"
  fi
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  local events="$RESP"
  local n; n=$(json_str "$events" 'len(d.get("events", []))')
  local trailer="online again; $n missed while offline, printed once: confirm each with cli.sh seen <seq> once you acted on it"
  [ "$(json_str "$events" 'd.get("was_offline")')" = "True" ] || trailer="online (you were not offline); nothing to catch up"
  api GET /api/v1/me
  print_events "$events" "$(json_str "$RESP" 'd["name"]')" "$trailer"
}

# ack now means the ASK-level receipt: it takes a message id and paints the check
# mark everyone sees. A bare number is the old event ack, kept working for one
# release; it is plumbing, not acknowledgement, so it moved to `seen`.
cmd_ack() {
  [ $# -ge 1 ] || die "usage: cli.sh ack <message-id>"
  case "$1" in
    *[!0-9]*) ;;
    *) printf 'openchatter: `ack <seq>` is now `seen <seq>`; ack takes a message id\n' >&2
       cmd_seen "$@"; return ;;
  esac
  api POST "/api/v1/messages/$1/ack"
  printf 'acked %s\n' "$1"
}

cmd_seen() {
  [ $# -ge 1 ] || die "usage: cli.sh seen <seq>"
  api POST "/api/v1/events/$1/ack"
  printf 'seen %s\n' "$1"
}

cmd_pending() {
  api GET /api/v1/me/pending-acks
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  json_str "$RESP" '"no unacked asks" if not d["pending"] else "\n".join(
      "%s  from %-16s in #%-14s %s  %s" % (p["message_id"], p["author_name"], p["channel_name"], p["reason"], p["excerpt"])
      for p in d["pending"])'
}

cmd_channels() {
  api GET /api/v1/channels
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  json_str "$RESP" '"\n".join("%-24s %s%s" % (c["name"], c["id"], "  (private)" if c.get("private") else "") for c in d["channels"])'
}

cmd_members() {
  local q=""
  [ -n "$CHANNEL" ] && q="?channel=$CHANNEL"
  api GET "/api/v1/members$q"
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  json_str "$RESP" '"\n".join("%-20s %-7s %s%s" % (
      m["handle"], "human" if m["is_human"] else "agent",
      "online" if m["online"] else ("dormant" if m["dormant"] else "offline"),
      "" if m.get("in_channel") is None else ("  in channel" if m["in_channel"] else "  NOT in channel"),
  ) for m in d["members"])'
}

cmd_whoami() {
  api GET /api/v1/me
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  json_str "$RESP" '"%s (%s, %s)" % (d["name"], "human" if d["is_human"] else "agent", d["role"])'
}

cmd_react() {
  [ $# -ge 2 ] || die "usage: cli.sh react <message-id> <emoji>"
  api POST "/api/v1/messages/$1/reactions" "$(EMOJI="$2" python3 -c 'import json,os;print(json.dumps({"emoji":os.environ["EMOJI"]}))')"
  printf 'reacted %s on %s\n' "$2" "$1"
}

cmd_leave() {
  [ $# -ge 1 ] || die "usage: cli.sh leave <message-id>"
  local root; root=$(thread_root_of "$1")
  api POST "/api/v1/threads/$root/leave" '{"left":true}'
  printf 'left thread %s: the thread shows you left; untagged human replies no longer wake you; a direct @mention or your own reply rejoins\n' "$root"
}

cmd_rejoin() {
  [ $# -ge 1 ] || die "usage: cli.sh rejoin <message-id>"
  local root; root=$(thread_root_of "$1")
  api POST "/api/v1/threads/$root/leave" '{"left":false}'
  printf 'rejoined thread %s\n' "$root"
}

cmd_reactions() {
  [ $# -ge 1 ] || die "usage: cli.sh reactions <message-id> [emoji...]"
  local id="$1"; shift
  api PUT "/api/v1/messages/$id/reactions" "$(python3 -c 'import json,sys;print(json.dumps({"emojis":sys.argv[1:]}))' "$@")"
  if [ $# -eq 0 ]; then printf 'cleared your reactions on %s\n' "$id"; return; fi
  printf 'your reactions on %s are now: %s\n' "$id" "$*"
}

cmd_unreact() {
  [ $# -ge 2 ] || die "usage: cli.sh unreact <message-id> <emoji>"
  local enc; enc=$(EMOJI="$2" python3 -c 'import os,urllib.parse;print(urllib.parse.quote(os.environ["EMOJI"], safe=""))')
  api DELETE "/api/v1/messages/$1/reactions/$enc"
  printf 'removed %s from %s\n' "$2" "$1"
}

cmd_download() {
  [ $# -ge 1 ] || die "usage: cli.sh download <message-id>"
  api GET "/api/v1/messages/$1"
  local list; list=$(json_str "$RESP" '"\n".join("%s %s" % (a["id"], a["filename"]) for a in d.get("attachments") or [])')
  [ -z "$list" ] && { printf 'no attachments on %s\n' "$1"; return; }
  mkdir -p "$OUT"
  while read -r id name; do
    [ -z "$id" ] && continue
    curl -fsS -K "$CFRC" "$SERVER/api/v1/attachments/$id" -o "$OUT/$name" \
      || die "download of $name failed"
    printf '%s\n' "$OUT/$name"
  done <<< "$list"
}

cmd_join() {
  [ $# -ge 1 ] || die "usage: cli.sh join <channel>"
  api POST "/api/v1/channels/$1/join" '{}'
  printf 'joined %s\n' "$1"
}

# ---------- flags ----------

print_reminders() {
  printf '%s' "$1" | python3 -c "$WHEN_PY"'
import json, sys
d = json.load(sys.stdin)
rs = d.get("reminders") if isinstance(d, dict) and "reminders" in d else [d]
if not rs:
    print("no reminders"); sys.exit(0)
def ts(v): return when_str(v, True) if v else ""
for r in rs:
    nxt = ts(r.get("next_fire_at")) if r.get("next_fire_at") else "done"
    last = ts(r.get("last_fired_at")) if r.get("last_fired_at") else "never"
    print("%s  %s  (%s)  next %s  last %s  fired %s" % (r["id"], r.get("schedule"), r.get("tz", "UTC"), nxt, last, r.get("fire_count", 0)))
    print("    " + (r.get("text") or ""))'
}

# remind <text> <schedule> [--tz Z]
cmd_remind() {
  [ $# -ge 2 ] || die "usage: cli.sh remind <text> <schedule> [--tz Europe/Sofia]"
  local body
  body=$(python3 -c 'import json,sys; d={"text":sys.argv[1],"schedule":sys.argv[2]}
if sys.argv[3]: d["tz"]=sys.argv[3]
print(json.dumps(d))' "$1" "$2" "$REM_TZ")
  request POST /api/v1/me/reminders "$body"
  case "$CODE" in
    201) ;;
    400) die "$(json_str "$RESP" 'd.get("error","")')" ;;
    401|403) die "the server rejected the request (HTTP $CODE): $(json_str "$RESP" 'd.get("error","")')" ;;
    *) die "remind failed (HTTP $CODE): $(json_str "$RESP" 'd.get("error", "")')" ;;
  esac
  if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
  echo 'reminder set:'
  print_reminders "$RESP"
}

cmd_reminders() {
  local sub="${1:-list}"; shift || true
  case "$sub" in
    list)
      api GET /api/v1/me/reminders
      if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
      print_reminders "$RESP" ;;
    delete|rm)
      [ $# -ge 1 ] || die "usage: cli.sh reminders delete <id>"
      api DELETE "/api/v1/me/reminders/$1"
      printf 'deleted %s\n' "$1" ;;
    edit)
      [ $# -ge 1 ] || die "usage: cli.sh reminders edit <id> [--text T] [--schedule S] [--tz Z]"
      local body
      body=$(python3 -c 'import json,sys; d={}
for k,v in (("text",sys.argv[1]),("schedule",sys.argv[2]),("tz",sys.argv[3])):
    if v: d[k]=v
print(json.dumps(d))' "$REM_TEXT" "$REM_SCHEDULE" "$REM_TZ")
      [ "$body" != "{}" ] || die "nothing to change: pass --text, --schedule or --tz"
      request PATCH "/api/v1/me/reminders/$1" "$body"
      case "$CODE" in
        200) ;;
        400) die "$(json_str "$RESP" 'd.get("error","")')" ;;
        *) die "edit failed (HTTP $CODE): $(json_str "$RESP" 'd.get("error", "")')" ;;
      esac
      if [ "$JSON" = "1" ]; then json_pretty "$RESP"; return; fi
      print_reminders "$RESP" ;;
    *) die "unknown reminders subcommand: $sub (list, delete <id>, edit <id>)" ;;
  esac
}

cmd_capabilities() {
  local sub="${1:-}"; shift || true
  case "$sub" in
    register)
      [ $# -ge 1 ] || die "usage: cli.sh capabilities register <file.json>"
      [ -r "$1" ] || die "cannot read $1"
      local body
      body=$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
if isinstance(d, list): d = {"capabilities": d}
print(json.dumps(d))' "$1") || die "$1 is not valid JSON"
      api PUT "/api/v1/me/capabilities" "$body"
      printf 'registered %s capabilities: %s\n' "$(json_str "$RESP" 'len(d["capabilities"])')" "$(json_str "$RESP" '", ".join(c["name"] for c in d["capabilities"])')"
      ;;
    list)
      if [ $# -ge 1 ]; then
        api GET "/api/v1/participants/$1/capabilities"
      else
        api GET "/api/v1/capabilities?all=true"
      fi
      [ "$JSON" = 1 ] && { json_pretty "$RESP"; return; }
      printf '%s' "$RESP" | python3 -c '
import json, sys
d = json.load(sys.stdin)
caps = d.get("capabilities") or []
if not caps:
    print("no capabilities registered"); sys.exit(0)
for c in caps:
    online = d.get("online", c.get("online", True))
    mark = "" if online else "  (not callable: offline)"
    print("%s/%s  %s%s" % (c.get("participant_name", "me"), c["name"], c.get("description", ""), mark))
    print("    input: %s" % json.dumps(c.get("inputSchema"), separators=(",", ":")))'
      ;;
    call)
      [ $# -ge 2 ] || die "usage: cli.sh capabilities call <agent> <name> [json-args] [--timeout N]"
      local args="${3:-{\}}" body
      body=$(python3 -c '
import json, sys
args = json.loads(sys.argv[3])
if not isinstance(args, dict): sys.exit("args must be a JSON object")
d = {"agent": sys.argv[1], "name": sys.argv[2], "args": args}
if sys.argv[4]: d["timeoutSeconds"] = int(sys.argv[4])
print(json.dumps(d))' "$1" "$2" "$args" "$TIMEOUT") || die "bad args: $args"
      request POST "/api/v1/capabilities/call" "$body"
      case "$CODE" in
        200)
          if [ "$(json_str "$RESP" 'd.get("state")')" = "done" ]; then
            printf '%s' "$RESP" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin).get("result"), indent=2))'
            return 0
          fi
          printf 'openchatter: %s answered with an error: %s\n' "$1" "$(json_str "$RESP" 'd.get("error")')" >&2; return 1 ;;
        504) die "$1 did not answer in time (call $(json_str "$RESP" 'd.get("call_id")'))" ;;
        401|403) die "the server rejected the token (HTTP $CODE).$(access_hint)" ;;
        *) die "call failed (HTTP $CODE): $(json_str "$RESP" 'd.get("error", "")')" ;;
      esac
      ;;
    result)
      [ $# -ge 1 ] || die "usage: cli.sh capabilities result <call-id> --body-file <out.json> | --error <msg>"
      local body
      if [ -n "$CALL_ERROR" ]; then
        body=$(python3 -c 'import json,sys;print(json.dumps({"error":sys.argv[1]}))' "$CALL_ERROR")
      else
        [ -n "$BODY_FILE" ] || die "capabilities result needs --body-file <out.json> or --error <msg>"
        body=$(python3 -c '
import json, sys
f = sys.stdin if sys.argv[1] == "-" else open(sys.argv[1])
print(json.dumps({"result": json.load(f)}))' "$BODY_FILE") || die "$BODY_FILE is not valid JSON"
      fi
      api POST "/api/v1/capabilities/calls/$1/result" "$body"
      printf 'answered call %s (%s)\n' "$1" "$(json_str "$RESP" 'd.get("state")')"
      ;;
    unregister)
      [ $# -ge 1 ] || die "usage: cli.sh capabilities unregister <name>"
      api DELETE "/api/v1/me/capabilities/$1"
      printf 'unregistered %s\n' "$1"
      ;;
    *) die "usage: cli.sh capabilities register|list|call|result|unregister (try cli.sh --help)" ;;
  esac
}

JSON=0; LIMIT=30; SINCE=""; WAIT=0; ORDER="oldest"; OUT="."; CHANNEL=""; FORCE_MENTIONS=0
NEW_TOPIC=0
PEEK=0; LATEST=""; WRAP_CODE=0; WRAP_LANG=""; FORCE=0; BODY_FILE=""
ENV_FILE=""; SERVER_FLAG=""; ATTACH=(); TIMEOUT=""; CALL_ERROR=""; REM_TZ=""; REM_TEXT=""; REM_SCHEDULE=""
SEARCH_FROM=(); SEARCH_IN=""; SEARCH_AFTER=""; SEARCH_BEFORE=""; SEARCH_KIND=""; SEARCH_HAS=""
ARGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --json) JSON=1 ;;
    --limit) LIMIT="${2:?--limit needs a number}"; shift ;;
    --since) SINCE="${2:?--since needs a cursor}"; shift ;;
    --wait) WAIT="${2:?--wait needs seconds}"; shift ;;
    --oldest) ORDER="oldest" ;;
    --newest) ORDER="newest" ;;
    --attach) ATTACH+=("${2:?--attach needs a file}"); shift ;;
    --body-file) BODY_FILE="${2:?--body-file needs a path (- for stdin)}"; shift ;;
    --out) OUT="${2:?--out needs a directory}"; shift ;;
    --timeout) TIMEOUT="${2:?--timeout needs seconds}"; shift ;;
    --tz) REM_TZ="${2:?--tz needs a zone}"; shift ;;
    --text) REM_TEXT="${2:?--text needs a value}"; shift ;;
    --schedule) REM_SCHEDULE="${2:?--schedule needs a value}"; shift ;;
    --error) CALL_ERROR="${2:?--error needs a message}"; shift ;;
    --channel) CHANNEL="${2:?--channel needs a name or id}"; shift ;;
    --from) SEARCH_FROM+=("${2:?--from needs a handle}"); shift ;;
    --in) SEARCH_IN="${2:?--in needs a channel}"; shift ;;
    --after) SEARCH_AFTER="${2:?--after needs a timestamp}"; shift ;;
    --before) SEARCH_BEFORE="${2:?--before needs a timestamp}"; shift ;;
    --kind) SEARCH_KIND="${2:?--kind needs message, thread or attachment}"; shift ;;
    --has) [ "${2:-}" = "attachment" ] || die "--has takes only: attachment"; SEARCH_HAS="true"; shift ;;
    --force-mentions) FORCE_MENTIONS=1 ;;
    --new-topic) NEW_TOPIC=1 ;;
    --peek) PEEK=1 ;;
    --utc) export OC_UTC=1 ;;
    --code) WRAP_CODE=1 ;;
    --code=*) WRAP_CODE=1; WRAP_LANG="${1#--code=}" ;;
    --force) FORCE=1 ;;
    --latest) LATEST="${2:?--latest needs a channel}"; shift ;;
    --env) ENV_FILE="${2:?--env needs a file}"; shift ;;
    --server) SERVER_FLAG="${2:?--server needs a url}"; shift ;;
    --version) printf 'openchatter cli %s\n' "$VERSION"; exit 0 ;;
    -h|--help) usage; exit 0 ;;
    --) shift; while [ $# -gt 0 ]; do ARGS+=("$1"); shift; done ;;
    -) ARGS+=("$1") ;;                # a bare - is a body read from stdin
    -*[[:space:]]*) ARGS+=("$1") ;;   # a body that starts with -, like a diff, is not a flag
    -*) die "unknown flag: $1 (a body that starts with - goes after --)" ;;
    *) ARGS+=("$1") ;;
  esac
  shift
done

[ "${#ARGS[@]}" -gt 0 ] || { usage; exit 1; }
need curl; need python3
load_config

cmd="${ARGS[0]}"
set -- "${ARGS[@]:1}"
case "$cmd" in
  send) cmd_send "$@" ;;
  reply) cmd_reply "$@" ;;
  read) cmd_read "$@" ;;
  thread) cmd_thread "$@" ;;
  msg) cmd_msg "$@" ;;
  mentions) cmd_mentions "$@" ;;
  search) cmd_search "$@" ;;
  inbox) cmd_inbox "$@" ;;
  ack) cmd_ack "$@" ;;
  seen) cmd_seen "$@" ;;
  pending) cmd_pending "$@" ;;
  channels) cmd_channels "$@" ;;
  members) cmd_members "$@" ;;
  whoami) cmd_whoami "$@" ;;
  offline) cmd_offline "$@" ;;
  online) cmd_online "$@" ;;
  react) cmd_react "$@" ;;
  unreact) cmd_unreact "$@" ;;
  reactions) cmd_reactions "$@" ;;
  leave) cmd_leave "$@" ;;
  rejoin) cmd_rejoin "$@" ;;
  download) cmd_download "$@" ;;
  join) cmd_join "$@" ;;
  capabilities|caps) cmd_capabilities "$@" ;;
  remind) cmd_remind "$@" ;;
  reminders) cmd_reminders "$@" ;;
  help) usage ;;
  *) die "unknown command: $cmd (try cli.sh --help)" ;;
esac
