# OpenChatter — working rules

Single Go binary + Postgres (pgvector). Server code in `services/api`, storage in
`models`, web UI in `web/` (Vite app: `web/index.html`, `web/src`, vendor libs in
`web/public/vendor`; `npm run build` emits `web/dist`, embedded into the binary),
entrypoint `cmd/openchatterd`.

## Always write tests

Every new feature and every bug fix ships with tests in the same change. No exceptions.

- A bug fix gets a **regression test that fails without the fix** — write it, watch it
  fail on the unpatched code, then confirm the fix turns it green. If a fix is not
  observable in a test (e.g. crash-atomicity, temp-file cleanup), say so in the change
  and add the closest guarding test instead.
- Server/storage logic: Go tests in `services/api/*_test.go` (they hit a real Postgres).
- User-facing web behavior: a headless-Chrome e2e in `scripts/*-check.js` when reachable.
- "Done" means the whole suite is green, not just the new test.

## Build and test

```sh
go build ./...
# Go tests need a live Postgres (docker compose db, port 5477):
OPENCHATTER_DB_URL="postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable" \
  go test ./services/... ./models/... ./pkg/... -count=1
# Rebuild the dev app container (builds and serves the web UI too):
set -a && source .env && set +a && docker compose up -d --build app
bash scripts/e2e.sh                        # REST end-to-end (needs sourced .env)
SERVER=http://localhost:8095 bash scripts/cli-e2e.sh   # cli.sh end-to-end (CLI_E2E_OK)
# Browser e2e (needs puppeteer-core on NODE_PATH; every check makes its room through
# scripts/lib/login.js, whose newRoom() needs psql on PATH and the dev db, OPENCHATTER_DB_URL):
node scripts/ui-smoke.js                    # UI_SMOKE_OK
node scripts/url-check.js                   # URL_CHECK_OK
node scripts/replybar-check.js              # REPLYBAR_CHECK_OK
node scripts/msgsync-check.js               # MSGSYNC_CHECK_OK
node scripts/channelnav-check.js            # CHANNELNAV_CHECK_OK (stale page/thread snapshots cannot erase live data; opens at latest)
node scripts/coldchannel-check.js            # COLDCHANNEL_CHECK_OK (first-load isolation and live-event reconciliation)
node scripts/uistaterace-check.js            # UISTATERACE_CHECK_OK (profile/member requests stay bound to their view)
node scripts/pendingnav-check.js              # PENDINGNAV_CHECK_OK (optimistic sends settle across navigation)
node scripts/attachmentnav-check.js           # ATTACHMENTNAV_CHECK_OK (staged uploads stay bound to their composer and release previews)
node scripts/search-check.js                # SEARCH_CHECK_OK (hybrid list, avatar rows, semantic tag, degrade)
node scripts/searchfilters-check.js         # SEARCHFILTERS_CHECK_OK (From/In/Date/Kind/Has, chips, inline tokens)
node scripts/copy-check.js                  # COPY_CHECK_OK
node scripts/mention-check.js               # MENTION_CHECK_OK
node scripts/dnd-check.js                   # DND_CHECK_OK
node scripts/channelsections-check.js       # CHANNELSECTIONS_CHECK_OK (default section, drag between and inside sections, task 28)
node scripts/replycount-check.js            # REPLYCOUNT_CHECK_OK (root reply footer updates live, cached page, task 29)
node scripts/list-check.js                  # LIST_CHECK_OK
node scripts/attach-check.js                # ATTACH_CHECK_OK
node scripts/avatar-check.js                # AVATAR_CHECK_OK (image-only avatars: uploaded, none, legacy emoji row, Remove)
node scripts/avatarloading-check.js         # AVATARLOADING_CHECK_OK (slow image skeleton, real error, all surfaces, cache)
node scripts/invite-check.js               # INVITE_CHECK_OK (ACCESS_ID/ACCESS_SECRET for a gated server)
node scripts/invitemenu-check.js           # INVITEMENU_CHECK_OK
node scripts/invitelink-check.js           # INVITELINK_CHECK_OK (/join/<token> page, revoke, expiry)
node scripts/addagent-check.js             # ADDAGENT_CHECK_OK (+ Add an agent row and modal)
node scripts/agentidentity-check.js         # AGENTIDENTITY_CHECK_OK (token identity, owner/admin delete, name reuse, history)
node scripts/channeladd-check.js            # CHANNELADD_CHECK_OK
node scripts/moreactions-check.js           # MOREACTIONS_CHECK_OK
node scripts/chanlink-check.js              # CHANLINK_CHECK_OK
node scripts/notify-check.js                # NOTIFY_CHECK_OK
node scripts/threadnotify-check.js          # THREADNOTIFY_CHECK_OK (every involved-thread reply sounds while focused)
node scripts/timestamps-check.js            # TIMESTAMPS_CHECK_OK (relative message times, hover, live tick)
node scripts/archive-check.js               # ARCHIVE_CHECK_OK
node scripts/emoji-check.js                 # EMOJI_CHECK_OK
node scripts/theme-check.js                 # THEME_CHECK_OK
node scripts/settings-check.js              # SETTINGS_CHECK_OK (same)
node scripts/icons-check.js                # ICONS_CHECK_OK (every chrome icon is an inline Lucide svg, stroke 2, 16/20/24, no glyphs, task 24)
node scripts/openchatter-check.js            # OPENCHATTER_CHECK_OK (brand: name, mark, favicon set, splash, skill and cli.sh)
node scripts/wsavatar-check.js              # WSAVATAR_CHECK_OK
node scripts/rail-check.js                  # RAIL_CHECK_OK
node scripts/railbadge-check.js             # RAILBADGE_CHECK_OK (count pills, 99+, title, favicon)
node scripts/railorder-check.js             # RAILORDER_CHECK_OK (rail drag order, workspace mute, task 18)
node scripts/wsdelete-check.js             # WSDELETE_CHECK_OK
node scripts/fleetroom-check.js            # FLEETROOM_CHECK_OK (no browser, no db)
node scripts/kick-check.js                 # KICK_CHECK_OK
node scripts/owners-check.js               # OWNERS_CHECK_OK (member tree, rebind, cascade, 409s, task 19)
node scripts/slug-check.js                 # SLUG_CHECK_OK
node scripts/reactions-check.js             # REACTIONS_CHECK_OK
node scripts/codeblock-check.js             # CODEBLOCK_CHECK_OK
node scripts/docpreview-check.js           # DOCPREVIEW_CHECK_OK
node scripts/login-check.js                # LOGIN_CHECK_OK (needs OPENCHATTER_DB_URL for openchatter-passwd)
node scripts/settings-nav-check.js         # SETTINGS_NAV_CHECK_OK (same)
node scripts/enter-check.js                # ENTER_CHECK_OK
node scripts/dateseps-check.js             # DATESEPS_CHECK_OK
node scripts/switcher-check.js             # SWITCHER_CHECK_OK (same)
node scripts/composer-check.js             # COMPOSER_CHECK_OK
node scripts/chanrename-check.js           # CHANRENAME_CHECK_OK
node scripts/boot-check.js                 # BOOT_CHECK_OK (throttled boot: no layout flash)
node scripts/deliverystats-check.js        # DELIVERYSTATS_CHECK_OK (profile delivery row, task 25)
node scripts/caps-check.js                 # CAPS_CHECK_OK (profile capabilities + settings MCP URL, task 27)
node scripts/presence-check.js             # PRESENCE_CHECK_OK (declared offline: grey dot, flat agent list, top-level offline section, online batch, task 21)
node scripts/switch-check.js               # SWITCH_CHECK_OK (atomic cold switch, zero-request warm switch under Slow 3G, live store, task 23)
node scripts/ack-check.js                   # ACK_CHECK_OK (explicit acks: check mark, hover names, human ack button, task 32)
node scripts/reminders-check.js            # REMINDERS_CHECK_OK (owner profile reminders, live fire, delete, 403, task 22; needs psql)
```

## Conventions

- Every event-writing transaction takes the room advisory lock **first**
  (`lockRoomEvents`), before any FK row lock, or it deadlocks AB-BA against
  `RotateSecret`. The UPDATE/INSERT and its `appendEventTx` go in one transaction.
- The invite code is a secret; the room slug is not. Only admins ever see the code.
- Never log or embed a secret (invite code, token) in an event payload.

## Prod

Native launchd deploy on a Mac mini; see `docs/PROD.md`. Deploys are deliberate:
`scripts/deploy-prod.sh <commit>`.
