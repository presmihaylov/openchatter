#!/usr/bin/env bash
# End-to-end for cli.sh: drive a real conversation with the CLI itself.
# Every command an agent needs is exercised here — send, thread reply, read,
# attachments both ways, mentions, reactions, membership.
# Run: SERVER=http://localhost:8095 bash scripts/cli-e2e.sh
set -euo pipefail

SERVER="${SERVER:-http://localhost:8095}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
CLI="$WORK/cli.sh"
curl -fsS "$SERVER/cli.sh" -o "$CLI"
chmod +x "$CLI"

ok() { printf '  ok  %s\n' "$1"; }
fail() { printf 'CLI_E2E_FAIL: %s\n' "$1" >&2; exit 1; }
jq_() { python3 -c 'import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1],{"d":d}))' "$1"; }

# only a logged-in human creates a room: register a throwaway user, create with the session
reg=$(curl -fsS -X POST "$SERVER/api/v1/auth/password/register" -H 'Content-Type: application/json' \
  -d "{\"username\":\"cli-$(date +%s)-$RANDOM\",\"password\":\"cli-throwaway-pw\"}")
session=$(printf '%s' "$reg" | jq_ 'd["token"]')
created=$(curl -fsS -X POST "$SERVER/api/v1/rooms" -H "Authorization: Bearer $session" -H 'Content-Type: application/json' -d "{\"name\":\"cli check\",\"slug\":\"cli-check-$(date +%s)-$RANDOM\"}")
invite=$(printf '%s' "$created" | jq_ 'd["invite"]')
join() {
  curl -fsS -X POST "$SERVER/api/v1/rooms/join" -H 'Content-Type: application/json' \
    -d "{\"invite\":\"$invite\",\"name\":\"$1\",\"description\":\"t\"}" | jq_ 'd["token"]'
}
alice=$(join alice)
bob=$(join bob)

env_for() { printf 'SERVER=%s\nTOKEN=%s\n' "$SERVER" "$1" > "$WORK/$2.env"; }
env_for "$alice" alice
env_for "$bob" bob
A=("$CLI" --env "$WORK/alice.env")
B=("$CLI" --env "$WORK/bob.env")

# 1. identity and channels
"${A[@]}" whoami | grep -q '^alice ' || fail "whoami"
"${A[@]}" channels | grep -q '^general ' || fail "channels does not list general"
ok "whoami + channels"

# 2. send, and the id comes back for scripting
sent=$("${A[@]}" send general 'hello from the CLI, @bob')
root=${sent#posted }
[ ${#root} -eq 36 ] || fail "send did not print a message id: $sent"
ok "send with a mention"

# 3. reply lands IN the thread, and a reply to the reply stays in the same one
r1=$("${B[@]}" reply "$root" 'got it')
r1=${r1#posted }
"${A[@]}" reply "$r1" 'thanks' >/dev/null
count=$("${A[@]}" thread "$root" --json | jq_ 'len(d["messages"])')
[ "$count" = "3" ] || fail "thread has $count messages, want 3"
kids=$("${A[@]}" thread "$root" --json | jq_ 'len([m for m in d["messages"] if m.get("thread_root_id")])')
[ "$kids" = "2" ] || fail "replies are not in the thread: $kids"
ok "reply resolves the thread root from any id"

# 4. read back, newest-first and oldest-last both work, bodies are untruncated
"${A[@]}" read general | grep -q 'hello from the CLI' || fail "read lost the message"
"${A[@]}" read general --json | grep -q '"messages"' || fail "read --json"
# a multi-line body comes back whole, not clipped to its first line
"${A[@]}" send general $'line one\nline two' >/dev/null
"${A[@]}" read general | grep -q 'line two' || fail "read truncated a body"
# --newest flips the order: the message just posted comes first
[ "$("${A[@]}" read general --newest --json | jq_ 'd["messages"][0]["body"].splitlines()[0]')" = "line one" ] \
  || fail "--newest did not reverse the order"
[ "$("${A[@]}" read general --json | jq_ 'd["messages"][0]["body"].splitlines()[0]')" = "hello from the CLI, @bob" ] \
  || fail "the default order is not oldest-first"
# --since drops everything older
n=$("${A[@]}" read general --since 2099-01-01T00:00:00Z --json | jq_ 'len(d["messages"])')
[ "$n" = "0" ] || fail "--since kept $n older messages"
n=$("${A[@]}" read general --since 2000-01-01T00:00:00Z --json | jq_ 'len(d["messages"])')
[ "$n" -gt 0 ] || fail "--since dropped everything"
ok "read (ordering, --since, full bodies)"

# 5. one message by id
"${A[@]}" msg "$root" | grep -q 'hello from the CLI' || fail "msg"
ok "msg"

# 6. attachment round-trip
printf 'attached payload\n' > "$WORK/note.txt"
att=$("${A[@]}" send general 'here is the file' --attach "$WORK/note.txt")
att=${att#posted }
mkdir -p "$WORK/dl"
"${B[@]}" download "$att" --out "$WORK/dl" >/dev/null
grep -q 'attached payload' "$WORK/dl/note.txt" || fail "attachment round-trip"
ok "attachment upload + download"

# 7. an unknown handle fails loudly, and the roster cache refreshes
if "${A[@]}" send general 'ping @nobody-here' >"$WORK/out" 2>"$WORK/err"; then
  fail "an unknown mention must exit non-zero"
fi
grep -q 'nobody-here' "$WORK/err" || fail "the 422 message did not reach stderr: $(cat "$WORK/err")"
grep -q 'alice' "$WORK/err" || fail "the 422 did not list the current handles"
grep -q 'force-mentions' "$WORK/err" || fail "the 422 did not name the way through"
ok "unknown mention exits non-zero with the roster"

# 7a. writing ABOUT a dead handle must stay possible: a post-mortem needs it
"${A[@]}" send general 'post-mortem: @nobody-here is gone' --force-mentions >/dev/null \
  || fail "--force-mentions did not get the message through"
"${A[@]}" send general 'post-mortem: `@nobody-here` is gone' >/dev/null \
  || fail "a backticked handle must not be treated as a mention"
ok "a dead handle can still be written about"

# 7b. a real handle who cannot read the channel is a loud warning, not silence
curl -fsS -X POST "$SERVER/api/v1/channels" -H "Authorization: Bearer $alice" \
  -H 'Content-Type: application/json' -d '{"name":"alice-only"}' >/dev/null
"${A[@]}" send alice-only 'are you there @bob' 2>"$WORK/err" >/dev/null
grep -q 'bob' "$WORK/err" || fail "no out-of-channel warning: $(cat "$WORK/err")"
ok "out-of-channel mention warns the sender"

# 7c. there is no --token flag, so a token can never land in the process list
if grep -q -- '--token' "$CLI"; then fail "cli.sh takes a token on the command line"; fi
ok "the token is env-file only"

# 7d. reply never degrades to a channel-root post: an id it cannot resolve fails
if "${A[@]}" reply 00000000-0000-0000-0000-000000000000 'orphan' >/dev/null 2>"$WORK/err"; then
  fail "reply with an unresolvable id must exit non-zero"
fi
ok "reply fails loudly rather than posting to the channel root"

# 7e. a top-level send warns on stderr and names the roots it could have continued
"${A[@]}" send general 'another root' >"$WORK/out" 2>"$WORK/err" || fail "send failed"
grep -q 'caution, top-level post to #general' "$WORK/err" || fail "send did not caution: $(cat "$WORK/err")"
grep -q "$root" "$WORK/err" || fail "the caution did not list the earlier root $root"
grep -q -- '--new-topic' "$WORK/err" || fail "the caution did not name --new-topic"
grep -q '^posted ' "$WORK/out" || fail "the caution blocked the post"
"${A[@]}" send general 'meant as a root' --new-topic >/dev/null 2>"$WORK/err"
if grep -q 'caution' "$WORK/err"; then fail "--new-topic did not silence the caution"; fi
ok "send cautions about a top-level post, --new-topic silences it"

# 7g. a #name that is no channel warns but still posts; real, backticked and
#     numeric ones stay quiet
"${A[@]}" send general 'see #no-such-room for details' 2>"$WORK/err" >"$WORK/out" \
  || fail "an unknown #channel must still post"
grep -q 'posted' "$WORK/out" || fail "unknown #channel did not post: $(cat "$WORK/out")"
grep -q 'no channel named: no-such-room' "$WORK/err" || fail "no unknown-channel warning: $(cat "$WORK/err")"
"${A[@]}" send general 'see #general, `#no-such-room`, PR #10020' 2>"$WORK/err" >/dev/null \
  || fail "known #channel post failed"
if grep -q 'no channel named' "$WORK/err"; then fail "false channel warning: $(cat "$WORK/err")"; fi
ok "unknown #channel warns, known/backticked/numeric stay quiet"

# 7f. every read surface says root or reply, and names the root to reply under
"${A[@]}" read general | grep -F "[$root]" | grep -q 'root, 2 replies' || fail "read did not tag the root: $("${A[@]}" read general | grep -F "[$root]")"
"${A[@]}" thread "$root" | grep -F "[$r1]" | grep -q "reply in thread $root" || fail "thread did not tag the reply"
"${A[@]}" msg "$root" | grep -q 'root, 2 replies' || fail "msg did not tag the root"
[ "$("${A[@]}" msg "$r1" --json | jq_ 'd["reply_to"]')" = "$root" ] || fail "reply_to missing on a reply"
[ "$("${A[@]}" msg "$root" --json | jq_ 'd["reply_to"]')" = "$root" ] || fail "reply_to missing on a root"
ok "root/reply tags and reply_to on read, thread, msg"

# 7g. reply --latest lands in the newest thread you are part of, never at the root
"${B[@]}" mentions --limit 50 >/dev/null
"${A[@]}" reply "$root" 'one more for @bob' >/dev/null
lt=$("${B[@]}" reply --latest general 'found you without the id')
lt=${lt#posted }
[ "$("${B[@]}" msg "$lt" --json | jq_ 'd["thread_root_id"]')" = "$root" ] || fail "reply --latest did not land in $root"
out=$("${B[@]}" mentions --limit 50)
grep -q "one more for @bob" <<<"$out" || fail "mentions missed the reply"
grep -q "mentions you, reply in thread $root" <<<"$out" || fail "mentions did not tag the thread: $out"
"${A[@]}" reply "$root" 'no tag this time' >/dev/null
out=$("${B[@]}" mentions --limit 50)
grep -q "thread you are in, reply in thread $root" <<<"$out" || fail "an untagged follow-up was not labelled as a thread hit: $out"
ok "reply --latest and mentions tag the thread"

# 7h. a member who joined after the roster cache was warm is not a false alarm
carol=$(join carol)
"${A[@]}" reply "$root" 'welcome @carol' >"$WORK/out" 2>"$WORK/err" || fail "mentioning a new member failed: $(cat "$WORK/err")"
if grep -q 'no member answers' "$WORK/err"; then fail "a stale roster cache cried wolf on a new member: $(cat "$WORK/err")"; fi
ok "a new member does not trip the stale-cache warning"

# 8. mentions catch-up sees what was addressed to bob, and broadcasts
"${A[@]}" mentions --limit 50 >/dev/null   # each side starts its cursor at "now"
"${B[@]}" mentions --limit 50 >/dev/null
"${A[@]}" send general 'ping @bob again' >/dev/null
"${B[@]}" broadcast general 'everybody read this' >/dev/null
out=$("${B[@]}" mentions --limit 50)
grep -q 'ping @bob again' <<<"$out" || fail "mentions missed a direct mention"
# the cursor advanced, so a second run is quiet
"${B[@]}" mentions | grep -q 'nothing new' || fail "the mentions cursor did not advance"
# alice keeps her own cursor, so bob's catch-up did not consume hers
"${A[@]}" mentions --limit 50 | grep -q 'everybody read this' || fail "alice inherited bob's cursor, or missed a broadcast"
ok "mentions --since with a per-identity cursor"

# 9b. reactions: add, see who, the same emoji twice stays one, remove
"${B[@]}" react "$root" '👀' >/dev/null
"${B[@]}" react "$root" '👀' >/dev/null
"${A[@]}" react "$root" ':tada:' >/dev/null
"${A[@]}" msg "$root" | grep -q '👀 bob' || fail "reaction tag not visible"
"${A[@]}" msg "$root" --json | python3 -c '
import sys,json; d=json.load(sys.stdin); r=d["reactions"]
assert [x["emoji"] for x in r]==["👀",":tada:"], r
assert r[0]["count"]==1 and r[0]["names"]==["bob"], r' || fail "reaction json wrong"
"${B[@]}" unreact "$root" '👀' >/dev/null
"${A[@]}" unreact "$root" ':tada:' >/dev/null
"${A[@]}" msg "$root" | grep -q '👀' && fail "reaction survived unreact"
ok "reactions"

# 9b2. reactions <id> ✅ swaps bob's 👀 for ✅ in one call and leaves alice's alone
"${B[@]}" react "$root" '👀' >/dev/null
"${A[@]}" react "$root" '👀' >/dev/null
"${B[@]}" reactions "$root" '✅' | grep -q 'are now: ✅' || fail "reactions output"
"${A[@]}" msg "$root" --json | python3 -c '
import sys,json; d=json.load(sys.stdin); r={x["emoji"]:x["names"] for x in d["reactions"]}
assert r=={"👀":["alice"],"✅":["bob"]}, r' || fail "reactions swap wrong"
"${B[@]}" reactions "$root" | grep -q 'cleared' || fail "reactions clear output"
"${A[@]}" reactions "$root" >/dev/null
"${A[@]}" msg "$root" --json | python3 -c '
import sys,json; d=json.load(sys.stdin); assert d["reactions"]==[], d["reactions"]' || fail "reactions clear wrong"
ok "reactions swap"

# 9c. leave a thread: bob's untagged replies stop naming alice, a mention rejoins
c0=$(curl -fsS "$SERVER/api/v1/events?after=0" -H "Authorization: Bearer $alice" | python3 -c 'import sys,json;print(json.load(sys.stdin)["cursor"])')
"${A[@]}" leave "$root" | grep -q "left thread $root" || fail "leave output"
"${B[@]}" reply "$root" "carrying on without alice" >/dev/null
"${B[@]}" reply "$root" "@alice one more thing" >/dev/null
curl -fsS "$SERVER/api/v1/events?after=$c0" -H "Authorization: Bearer $alice" | python3 -c '
import sys,json
evs=[e["payload"] for e in json.load(sys.stdin)["events"] if e["type"]=="message.created"]
assert any(m["kind"]=="system" and m["body"]=="left this thread" and m["author_name"]=="alice" and m["mentions"]==[] for m in evs), evs
p={m["body"]:m["thread_participants"] for m in evs if m["kind"]!="system"}
assert "alice" not in p["carrying on without alice"], p
assert "alice" in p["@alice one more thing"], p' || fail "leave did not drop alice from thread_participants"
# leaving a thread you were pulled back into by a mention: the timeline says so
"${A[@]}" leave "$root" >/dev/null
"${A[@]}" mentions | grep -q "carrying on without alice" && fail "ac mentions still lists a thread alice left"
"${A[@]}" rejoin "$root" | grep -q "rejoined thread $root" || fail "rejoin output"
"${A[@]}" thread "$root" > "$WORK/thread"
grep -q "left this thread" "$WORK/thread" || fail "no 'left this thread' entry in the thread"
grep -q "rejoined this thread" "$WORK/thread" || fail "no 'rejoined this thread' entry in the thread"
ok "leave + rejoin"

# 10. membership: join a channel, and members reports who is in it
curl -fsS -X POST "$SERVER/api/v1/channels" -H "Authorization: Bearer $alice" \
  -H 'Content-Type: application/json' -d '{"name":"side"}' >/dev/null
"${B[@]}" join side >/dev/null
"${A[@]}" members --channel side | grep -E '^bob .*in channel' >/dev/null || fail "members --channel"
ok "join + members"

# 11. a bad token fails loudly and never echoes the token
printf 'SERVER=%s\nTOKEN=%s\n' "$SERVER" "not-a-real-token" > "$WORK/bad.env"
if "$CLI" --env "$WORK/bad.env" channels >"$WORK/out" 2>"$WORK/err"; then
  fail "a bad token must exit non-zero"
fi
grep -q 'rejected the token' "$WORK/err" || fail "unclear auth error: $(cat "$WORK/err")"
if grep -q 'not-a-real-token' "$WORK/out" "$WORK/err"; then fail "the CLI printed the token"; fi
ok "errors are loud and the token stays secret"

# 12. --body-file and stdin, on a fresh root so earlier reply counts stay put
bfroot=$("${A[@]}" send general "@bob body-file root" --new-topic); bfroot=${bfroot#posted }
printf '%s\n' 'report "one"' '`code` costs $5 and '"'"'more'"'"'' > "$WORK/report.md"
bf=$("${A[@]}" reply "$bfroot" --body-file "$WORK/report.md"); bf=${bf#posted }
[ "$("${A[@]}" msg "$bf" --json | jq_ 'd["body"]')" = "$(cat "$WORK/report.md")" ] || fail "--body-file body mangled"
sf=$(cat "$WORK/report.md" | "${A[@]}" reply "$bfroot" --body-file -); sf=${sf#posted }
[ "$("${A[@]}" msg "$sf" --json | jq_ 'd["body"]')" = "$(cat "$WORK/report.md")" ] || fail "stdin body mangled"
df=$(cat "$WORK/report.md" | "${A[@]}" reply "$bfroot" -); df=${df#posted }
[ "$("${A[@]}" msg "$df" --json | jq_ 'd["body"]')" = "$(cat "$WORK/report.md")" ] || fail "dash body mangled"
"${A[@]}" reply "$bfroot" --body-file "$WORK/missing.md" 2>/dev/null && fail "a missing --body-file must exit non-zero"
ok "--body-file and stdin bodies"

# 13. inbox drain and ack (task 25): bob goes offline, misses a mention, drains it, acks it.
# Earlier steps left bob unacked rows too, so the counts are relative.
unacked() { "${B[@]}" inbox --peek | sed -n 's/^\([0-9]*\) unacked.*/\1/p'; }
before=$(unacked)
curl -fsS -X POST "$SERVER/api/v1/me/offline" -H "Authorization: Bearer $bob" >/dev/null
"${A[@]}" send general "@bob inbox test" --new-topic >/dev/null
[ "$(unacked)" = "$((before + 1))" ] || fail "peek should show $((before + 1)) unacked, got $(unacked)"
iseq=$("${B[@]}" inbox --peek --json | jq_ '[e["seq"] for e in d["events"] if e["payload"]["body"] == "@bob inbox test"][0]')
[ -n "$iseq" ] || fail "inbox --json gave no seq"
[ "$(unacked)" = "$((before + 1))" ] || fail "peek must not mark anything"
"${B[@]}" inbox | grep -q "seq $iseq" || fail "inbox drain did not print seq $iseq"
"${B[@]}" seen "$iseq" | grep -q "seen $iseq" || fail "seen did not confirm"
[ "$(unacked)" = "$before" ] || fail "peek should show $before unacked after the ack, got $(unacked)"
# the old spelling keeps working for a release, with a warning on stderr
"${A[@]}" send general "@bob second inbox test" --new-topic >/dev/null
iseq2=$("${B[@]}" inbox --peek --json | jq_ '[e["seq"] for e in d["events"] if e["payload"]["body"] == "@bob second inbox test"][0]')
"${B[@]}" ack "$iseq2" 2>"$WORK/ackwarn" | grep -q "seen $iseq2" || fail "ack <seq> no longer confirms an event"
grep -q 'now `seen <seq>`' "$WORK/ackwarn" || fail "ack <seq> did not warn about the rename"
ok "inbox drain, seen, and ack <seq> still works with a warning"

# 14. capabilities (task 27): bob registers, alice lists and calls, bob answers from
# the inbox, the result comes back; an error answer exits 1; unregister empties the list.
cat > "$WORK/caps.json" <<'JSON'
[{"name":"echo","description":"echoes the question","inputSchema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"outputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]
JSON
"${B[@]}" capabilities register "$WORK/caps.json" | grep -q "registered 1 capabilities: echo" || fail "capabilities register output"
"${A[@]}" capabilities list | grep -q "bob/echo" || fail "capabilities list did not show bob/echo"
"${A[@]}" capabilities list bob --json | jq_ 'd["online"] and len(d["capabilities"]) == 1' | grep -q True || fail "capabilities list bob"
# bob answers in the background: poll its relevant firehose for the call
(
  cur=$(curl -fsS "$SERVER/api/v1/events" -H "Authorization: Bearer $bob" | jq_ 'd["cursor"]')
  for _ in $(seq 1 40); do
    ev=$(curl -fsS "$SERVER/api/v1/events?after=$cur&relevant=true&timeout=1" -H "Authorization: Bearer $bob")
    id=$(printf '%s' "$ev" | jq_ '([e["payload"]["call_id"] for e in d["events"] if e["type"] == "capability.call"] or [""])[0]')
    if [ -n "$id" ]; then
      printf '{"text":"pong"}' > "$WORK/out.json"
      "${B[@]}" capabilities result "$id" --body-file "$WORK/out.json" > "$WORK/answer.log"
      exit 0
    fi
    cur=$(printf '%s' "$ev" | jq_ 'd["cursor"]')
  done
  exit 1
) &
answerer=$!
res=$("${A[@]}" capabilities call bob echo '{"q":"ping"}' --timeout 20) || fail "capabilities call failed: $res"
printf '%s' "$res" | grep -q '"text": "pong"' || fail "call result: $res"
wait "$answerer" || fail "bob's answerer never saw the call"
grep -q "answered call" "$WORK/answer.log" || fail "result output"
# an error answer surfaces as exit 1 with the message
(
  cur=$(curl -fsS "$SERVER/api/v1/events" -H "Authorization: Bearer $bob" | jq_ 'd["cursor"]')
  for _ in $(seq 1 40); do
    ev=$(curl -fsS "$SERVER/api/v1/events?after=$cur&relevant=true&timeout=1" -H "Authorization: Bearer $bob")
    id=$(printf '%s' "$ev" | jq_ '([e["payload"]["call_id"] for e in d["events"] if e["type"] == "capability.call"] or [""])[0]')
    if [ -n "$id" ]; then "${B[@]}" capabilities result "$id" --error "no can do" >/dev/null; exit 0; fi
    cur=$(printf '%s' "$ev" | jq_ 'd["cursor"]')
  done
  exit 1
) &
answerer=$!
if "${A[@]}" capabilities call bob echo '{"q":"ping"}' --timeout 20 2> "$WORK/callerr"; then fail "error answer should exit 1"; fi
grep -q "no can do" "$WORK/callerr" || fail "error message missing: $(cat "$WORK/callerr")"
wait "$answerer" || fail "bob's error answerer never saw the call"
"${B[@]}" capabilities unregister echo | grep -q "unregistered echo" || fail "unregister output"
"${A[@]}" capabilities list | grep -q "no capabilities registered" || fail "list after unregister"
ok "capabilities register, call, result, unregister"

# 15. presence (task 21): bob parks himself, a mention lands while he is offline and
# his poll stays quiet, online prints it exactly once, a second online prints nothing.
"${B[@]}" mentions --limit 50 >/dev/null   # cursor at "now"
"${B[@]}" offline | grep -q '^offline:' || fail "offline did not confirm"
curl -fsS "$SERVER/api/v1/participants" -H "Authorization: Bearer $alice" \
  | jq_ '[p["presence"] for p in d["participants"] if p["name"] == "bob"][0]' | grep -qx offline || fail "roster should say bob is offline"
"${A[@]}" send general '@bob parked mention' --new-topic >/dev/null
"${B[@]}" mentions | grep -q 'nothing new' || fail "an offline poll must hand out nothing"
out=$("${B[@]}" online)
grep -q 'parked mention' <<<"$out" || fail "online did not print the missed mention: $out"
grep -q '1 missed while offline' <<<"$out" || fail "online trailer: $out"
curl -fsS "$SERVER/api/v1/participants" -H "Authorization: Bearer $alice" \
  | jq_ '[p["presence"] for p in d["participants"] if p["name"] == "bob"][0]' | grep -qx online || fail "roster should say bob is online again"
"${B[@]}" online | grep -q 'you were not offline' || fail "a second online should print nothing"
"${B[@]}" mentions | grep -q 'nothing new' || fail "the online batch must move the cursor: mentions replayed it"
# a stale cursor survives an online that was not offline: mentions still replays the gap
"${A[@]}" send general '@bob after a crash' --new-topic >/dev/null
"${B[@]}" online | grep -q 'you were not offline' || fail "online while online should say so"
"${B[@]}" mentions | grep -q 'after a crash' || fail "online while online must not move the cursor past an unread mention"
ok "offline, queued mention, online prints it once, stale cursor kept"

# 16. reminders (task 22): bob sets, lists, edits and deletes his own; a fire
# (backdated through the db when AGENTCHAT_DB_URL and psql are here, else skipped)
# shows in mentions as a REMINDER line and, while offline, in the online batch.
out=$("${B[@]}" remind 'check the build' 'in 2h')
grep -q '^reminder set:' <<<"$out" || fail "remind did not confirm: $out"
rid=$(sed -n '2p' <<<"$out" | awk '{print $1}')
[ ${#rid} -eq 36 ] || fail "remind printed no id: $out"
"${B[@]}" remind 'standup' 'every day at 09:00' --tz Europe/Sofia | grep -q 'every day at 09:00  (Europe/Sofia)' || fail "recurring remind"
# the cli exits 1 here, so capture instead of piping (pipefail)
bad=$("${B[@]}" remind 'never' 'whenever' 2>&1 || true)
grep -qi 'invalid schedule' <<<"$bad" && ok "bad schedule refused" || fail "bad schedule accepted: $bad"
"${B[@]}" reminders | grep -c 'next ' | grep -qx 2 || fail "reminders list should show 2"
"${B[@]}" reminders edit "$rid" --text 'check the build again' | grep -q 'check the build again' || fail "reminders edit"
"${A[@]}" reminders | grep -q 'no reminders' || fail "alice must not see bob's reminders"
notmine=$("${A[@]}" reminders delete "$rid" 2>&1 || true)
grep -q 'HTTP 404' <<<"$notmine" || fail "alice deleting bob's reminder should 404: $notmine"
if command -v psql >/dev/null 2>&1 && [ -n "${AGENTCHAT_DB_URL:-}" ]; then
  "${B[@]}" mentions --limit 50 >/dev/null
  psql "$AGENTCHAT_DB_URL" -q -v ON_ERROR_STOP=1 -c "UPDATE reminders SET next_fire_at = now() WHERE id = '$rid'"
  fired=""
  for _ in 1 2 3 4 5 6; do
    sleep 3
    fired=$("${B[@]}" mentions --limit 50) && grep -q "REMINDER  \[$rid\]" <<<"$fired" && break
  done
  grep -q "REMINDER  \[$rid\]" <<<"$fired" || fail "fired reminder missing from mentions: $fired"
  grep -q 'check the build again' <<<"$fired" || fail "fired reminder text: $fired"
  grep -q 'one-time, done' <<<"$fired" || fail "one-time fire should say done: $fired"
  "${B[@]}" reminders | grep "$rid" | grep -q 'next done' || fail "fired one-time should show next done"
  # offline: the fire queues and online prints it
  sid=$("${B[@]}" reminders | grep 'every day' | awk '{print $1}')
  "${B[@]}" offline >/dev/null
  psql "$AGENTCHAT_DB_URL" -q -v ON_ERROR_STOP=1 -c "UPDATE reminders SET next_fire_at = now() WHERE id = '$sid'"
  sleep 8
  "${B[@]}" mentions | grep -q 'nothing new' || fail "offline poll leaked the fire"
  out=$("${B[@]}" online)
  grep -q "REMINDER  \[$sid\]" <<<"$out" || fail "online batch missing the fired reminder: $out"
  grep -q 'standup' <<<"$out" || fail "online batch reminder text: $out"
  ok "one-time fires once into mentions, recurring fire waits for online"
else
  echo "  skip fire check (needs psql and AGENTCHAT_DB_URL)"
fi
"${B[@]}" reminders delete "$rid" | grep -q "deleted $rid" || fail "reminders delete"
"${B[@]}" reminders | grep -c 'next ' | grep -qx 1 || fail "one reminder should remain"
ok "remind, reminders list/edit/delete, other agents locked out"

# 17. search (task F): hybrid endpoint, filters AND, --from repeats, dates, --json
"${A[@]}" send general 'zebra migration budget alpha' --new-topic >/dev/null 2>&1
"${B[@]}" send general 'zebra migration budget bravo' --new-topic >/dev/null 2>&1
"${A[@]}" search zebra migration | grep -q 'zebra migration budget alpha' || fail "search missed alice's row"
"${A[@]}" search zebra migration | grep -q 'zebra migration budget bravo' || fail "search missed bob's row"
"${A[@]}" search zebra migration --from bob >"$WORK/out" || fail "search --from"
grep -q 'bravo' "$WORK/out" || fail "--from bob dropped bob's row"
if grep -q 'alpha' "$WORK/out"; then fail "--from bob kept alice's row"; fi
"${A[@]}" search zebra migration --from bob --from alice --json | jq_ 'len(d["results"])' | grep -q '^2$' || fail "two --from should OR to 2 rows"
"${A[@]}" search zebra migration --in alice-only --json | jq_ 'len(d["results"])' | grep -q '^0$' || fail "--in alice-only should be empty"
"${A[@]}" search zebra migration --before 2020-01-01 --json | jq_ 'len(d["results"])' | grep -q '^0$' || fail "--before 2020 should be empty"
"${A[@]}" search zebra migration --after 2020-01-01 --kind message --json | jq_ 'len(d["results"])' | grep -q '^2$' || fail "--after 2020 --kind message should keep 2"
"${A[@]}" search zebra migration --has attachment --json | jq_ 'len(d["results"])' | grep -q '^0$' || fail "--has attachment should be empty"
if "${A[@]}" search zebra --has photos >/dev/null 2>&1; then fail "--has photos must be rejected"; fi
"${A[@]}" search zebra migration --json | jq_ 'd["results"][0]["via"]' | grep -q '^text$' || fail "text hit should say via=text"
ok "search: hybrid endpoint, --from/--in/--after/--before/--kind/--has"

# 18. rename compatibility (OpenFlock phase 2, step 1). An agent that has not
# migrated must keep working, and a migrated one must be found without --env.
FAKE="$WORK/fakehome"
run_home() { HOME="$FAKE" "$CLI" "$@"; }

mkdir -p "$FAKE/.agentchat"
cp "$WORK/alice.env" "$FAKE/.agentchat/room.alice.agentchat.env"
run_home whoami | grep -q '^alice ' || fail "an un-migrated ~/.agentchat must still be found"
ok "un-migrated client directory still works"

# an empty new directory must not hide a working old one: the join docs tell an
# agent to mkdir ~/.openflock for cli.sh long before it moves its env file over
mkdir -p "$FAKE/.openflock"
run_home whoami | grep -q '^alice ' || fail "an empty ~/.openflock must not hide ~/.agentchat"
ok "an ~/.openflock with no env file does not hide the old directory"

cp "$WORK/bob.env" "$FAKE/.openflock/room.bob.openflock.env"
run_home whoami | grep -q '^bob ' || fail "~/.openflock must win over ~/.agentchat"
ok "migrated client directory wins, and the new env-file suffix is found"

# the variables dual-read: the new name wins, the old one still carries. HOME is
# empty in each, or the directory search would answer and prove nothing.
mkdir -p "$WORK/empty"
HOME="$WORK/empty" OPENFLOCK_ENV="$WORK/alice.env" "$CLI" whoami | grep -q '^alice ' || fail "OPENFLOCK_ENV ignored"
HOME="$WORK/empty" AGENTCHAT_ENV="$WORK/alice.env" "$CLI" whoami | grep -q '^alice ' || fail "AGENTCHAT_ENV no longer works"
HOME="$FAKE" OPENFLOCK_ENV="$WORK/alice.env" "$CLI" whoami | grep -q '^alice ' || fail "OPENFLOCK_ENV must beat the client directory"
OPENFLOCK_SERVER="$SERVER" OPENFLOCK_TOKEN="$alice" HOME="$WORK/empty" "$CLI" whoami | grep -q '^alice ' || fail "OPENFLOCK_SERVER/TOKEN ignored"
AGENTCHAT_SERVER="$SERVER" AGENTCHAT_TOKEN="$alice" HOME="$WORK/empty" "$CLI" whoami | grep -q '^alice ' || fail "AGENTCHAT_SERVER/TOKEN no longer work"
ok "env vars read the OPENFLOCK_ name first and the AGENTCHAT_ one second"

# --env alone must work with no HOME at all: a bridge under launchd or in a
# container has none, and the client directory search must never be reached
(unset HOME; "$CLI" --env "$WORK/alice.env" whoami) | grep -q '^alice ' || fail "--env must work with no HOME"
ok "--env works with no HOME set"

# the cursor cache follows the same rule as the client directory
CACHE_HOME="$WORK/cachehome"
mkdir -p "$CACHE_HOME/.cache/agentchat"
HOME="$CACHE_HOME" XDG_CACHE_HOME= "$CLI" --env "$WORK/alice.env" mentions --since 0 >/dev/null || fail "mentions failed under the legacy cache"
[ -n "$(ls -A "$CACHE_HOME/.cache/agentchat")" ] || fail "an existing ~/.cache/agentchat must keep being used"
[ ! -d "$CACHE_HOME/.cache/openflock" ] || fail "a legacy cache must not be abandoned for a new one"
ok "cursor cache stays in ~/.cache/agentchat when that is where it already is"

# 19. explicit acknowledgements (task 32): an ask addressed to bob is pending
# until bob acks it by message id, and the check mark rides on the message.
"${A[@]}" send general "@bob please own this" --new-topic >/dev/null
askid=$("${B[@]}" mentions --json | jq_ '[e["payload"]["id"] for e in d["events"] if e["payload"]["body"] == "@bob please own this"][0]')
[ -n "$askid" ] || fail "mentions gave no id for the ask"
"${B[@]}" pending | grep -q "$askid" || fail "the ask is not in bob's pending list"
"${B[@]}" pending | grep -q "from alice" || fail "pending does not name who asked"
! "${A[@]}" pending | grep -q "$askid" || fail "alice's own ask must not be pending for her"
"${B[@]}" ack "$askid" | grep -q "acked $askid" || fail "ack <message-id> did not confirm"
! "${B[@]}" pending | grep -q "$askid" || fail "the acked ask is still pending"
"${B[@]}" msg "$askid" --json | grep -q '"acked_by"' || fail "acked_by is missing from the message"
"${B[@]}" ack "$askid" | grep -q "acked $askid" || fail "acking twice must be a no-op, not an error"
ok "ack <message-id>, pending goes to zero, acked_by rides the message"

echo CLI_E2E_OK
