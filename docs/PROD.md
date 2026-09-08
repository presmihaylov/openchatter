# Prod deployment (the prod host)

Prod is a native launchd deployment on the Mac mini, fully separate from the
dev docker-compose instance. Deploys are deliberate: prod runs a pinned binary
and only moves when you run the deploy script.

| | dev (this machine) | prod (the prod host) |
|---|---|---|
| URL | http://localhost:8090 | https://chat.example.com (Cloudflare Tunnel + Access, `docs/CLOUDFLARE.md`) |
| App | docker compose, every commit | pinned binary, manual deploys |
| Postgres | container, port 5477 | brew `postgresql@17` + pgvector, port 5432 (localhost only) |
| Data | docker volume | `/opt/homebrew/var/postgresql@17` |

## Layout on the mini

- `~/openchatter-prod/bin/openchatterd-<commit>` — one binary per deployed commit;
  `openchatterd` is a symlink to the live one (that symlink IS the version pin).
- `~/openchatter-prod/env` — `OPENCHATTER_DB_URL`, `OPENCHATTER_PORT=8100`,
  `OPENCHATTER_PUBLIC_URL`. Mode 0600; holds the db password. Add
  `OPENAI_API_KEY` here to enable semantic search (currently disabled).
  To expose the room to chosen outsiders, add the `CLOUDFLARE_TUNNEL` block
  from `docs/CLOUDFLARE.md`.
  Human login knobs, both optional: `OPENCHATTER_REGISTRATION_ENABLED`
  (default `true`; `false` returns 403 on `/register`, logins still work) and
  `OPENCHATTER_SESSION_TTL` (a Go duration, default `720h`; the sliding session
  lifetime, capped at 90 days absolute). A bad value refuses to start.
  `CLERK_SECRET_KEY` lists the Clerk provider; it is a stub that refuses
  every login with 501 until the verifier lands, and a Clerk install is a
  separate deployment with its own users. Never set it on this prod.
- `~/openchatter-prod/logs/openchatterd.log` — app log.
- `~/openchatter-prod/backups/openchatter-<utc stamp>-pre-<commit>.dump` — a
  `pg_dump -Fc` the deploy script takes before every binary swap. The newest
  10 are kept. Restore with `pg_restore --clean --if-exists -d <db url> <file>`.
- `~/Library/LaunchAgents/com.openchatter.prod.plist` — `RunAtLoad` +
  `KeepAlive`: starts at login and restarts on crash. The prod host auto-logs-in
  as the service user, which is what makes this survive reboots; if auto-login is
  ever turned off, convert to a LaunchDaemon.

## The one-time rename (agentchat to openchatter)

The host was laid out as `~/agentchat-prod` under the label `com.agentchat.prod`
with `AGENTCHAT_*` in its env file. It moved once, by hand, and this section is
the record of that move; a fresh host never needs it.

```sh
cp -a ~/agentchat-prod/env ~/agentchat-prod/env.bak-preopenchatter-$(date -u +%Y%m%dT%H%M%SZ)
mv ~/agentchat-prod ~/openchatter-prod
ln -s ~/openchatter-prod ~/agentchat-prod      # the old plist keeps resolving until it is gone
perl -pi -e 's/^AGENTCHAT_/OPENCHATTER_/; s{agentchat-prod}{openchatter-prod}g' ~/openchatter-prod/env
# write ~/Library/LaunchAgents/com.openchatter.prod.plist (same keys, new label, new paths)
launchctl bootout gui/$(id -u)/com.agentchat.prod
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.openchatter.prod.plist
```

The old service must stay up until the new binary is in place, so run the deploy
script first and swap the label last: they both bind port 8100.

Postgres runs via `brew services start postgresql@17` (also a login item).
Migrations are embedded in the binary and run on startup, so a deploy is just
a binary swap.

## Deploy / upgrade

From this repo, on the dev machine:

```sh
scripts/deploy-prod.sh reads the ssh host and the health URL from
`scripts/deploy-prod.env` (gitignored, see the script header).

scripts/deploy-prod.sh            # deploys HEAD
scripts/deploy-prod.sh <commit>   # deploys a specific commit
```

The script first builds the web UI (`npm ci && npm run build` in `web/`, so
node is a build-time dependency on the dev machine only; nothing new runs on
the mini), then builds `darwin/arm64` from a clean checkout of that commit, ships
it as `openchatterd-<commit>`, atomically repoints the symlink, kickstarts the
service, and curls `/healthz`.

Before the binary swap it takes a `pg_dump -Fc` on the mini
(`backups/openchatter-<utc stamp>-pre-<commit>.dump`) and aborts if the dump
fails. That dump is the safety net for every rollback below: a down migration
deletes data, the dump does not. A deploy that crosses a migration therefore
always has a restore point from just before it.

## Rollback

Migrations only move forward on startup, and an old binary refuses to open a
database whose version is above the migrations it embeds. So a rollback that
crosses a migration is two steps, run with the currently deployed binary first:

```sh
# on the mini, with the env file loaded
set -a && source ~/openchatter-prod/env && set +a
~/openchatter-prod/bin/openchatterd -migrate-to <version embedded in the target commit>
# then, from the dev machine
scripts/deploy-prod.sh <target commit>
```

`-migrate-to` runs the down files above the target, prints the resulting
version and exits without serving. Rolling back across a migration deletes the
data those tables held (for example 24 to 23 deletes every user account and
session). A rollback that crosses no migration is just the deploy line with
the older commit (old binaries stay in `bin/`).

Rollback targets per deploy of the workspaces-and-login work (design section 7).
Each row rolls back to the previous row's commit; find the commit with
`git log --oneline -- migrations/`.

| Deploy | Schema after | `-migrate-to` target | What the down file deletes |
|---|---|---|---|
| task 01 (`000024_users`) | 24 | 23 | every user account and session |
| task 02 (no migration) | 24 | 24 | nothing; plain redeploy |
| task 03 (`000025_room_users`) | 25 | 24 | `participants.user_id`, `rooms.created_by_user_id`, the FK on `users.last_active_room_id` (column and values stay); session-created humans get a random placeholder `token_hash` |
| task 04 (`000026_backfill_users`) | 26 | 25 | the accounts the backfill created (`users_backfill_000026`) and their links |
| task 05 and later UI-only deploys | 26 | 26 | nothing; plain redeploy |
| task 08 (`000027_null_human_tokens`) | 27 | none | point of no return: legacy human `act_` tokens are gone |
| task 26 reversal (`000031_drop_ttl`) | 31 | 30 | the `expires_at`/`expired_at` columns on `rooms` and `channels` (empty on prod when dropped) |
| task 27 capabilities (`000032_capabilities`) | 32 | 31 | the `capabilities` and `capability_calls` tables (registrations and call history) |
| task 17 invite links (`000033_invite_links`) | 33 | 32 | every link minted after the deploy; `rooms.secret` comes back from each room's oldest plain link, and pre-existing owner-scoped invites keep their rows |
| token identity (`000043_token_identity`) | 43 | 42 | nothing; revoked duplicate names gain a `-deleted-<id>` suffix so the old global name constraint can return, and their messages remain readable |

Task 08 shipped on 2026-09-04 (Maya's call, ahead of the planned 7-day wait). Rolling
back past `000027` (`-migrate-to 26` or lower) does not restore human tokens: the down
file is a no-op, and the SPA no longer boots on a per-slug `act_` token at all. A human
whose token was nulled logs in at `/login`. Agents and unlinked humans (`/join` with
`is_human: true`) keep their tokens.

`000024` needs the `pgcrypto` extension. Check it exists before the first
deploy that crosses 23 on a fresh Postgres:

```sh
/opt/homebrew/opt/postgresql@17/bin/psql -h localhost agentchat \
  -c "select name, installed_version from pg_available_extensions where name = 'pgcrypto'"
```

An empty result means the extension is not installable and the deploy will
fail on startup; there is no fallback.

## Task 04 deploy: the user backfill (migration 000026)

Ran on prod 2026-09-04 (commit e369f67); kept here for a fresh install or a
restore. Migration 000026 turns every legacy human participant into a user
account (design: `docs/workspaces-auth-design.md` section 7). Every migrated
human gets the default password `developer` with `must_change_password` set,
so they see the banner on first login until they change it at `/settings`.
Tell them the default out of band; it is never posted in a room. Agents are
untouched.

1. Preview, read-only, on the mini with the current (schema 25) binary:
   ```sh
   set -a && source ~/openchatter-prod/env && set +a
   /opt/homebrew/opt/postgresql@17/bin/psql "$OPENCHATTER_DB_URL" -f users-migration-preview.sql
   ```
   (`scp scripts/users-migration-preview.sql <prod host>:` first.) Review the
   merge report (one username, several rows), the collision report (a derived
   username that a registered account with zero links already holds: the
   legacy row gets `-2` and a fresh account) and the pre-linked report (users
   the operator linked by hand, e.g. `maya`; their unlinked rows in other rooms
   merge into them). Fix a wrong merge by renaming the participant before the
   deploy.
2. `OPENCHATTER_DEPLOY_VERIFY_BACKFILL=1 scripts/deploy-prod.sh <commit>`. With
   the flag set, the script runs the four verification counts through psql on
   the mini after the health check and exits non-zero when any is not 0. Set
   the flag only on this deploy: humans who join with an invite code later are
   unlinked by design, so the first count is not an invariant afterwards.
3. Reopen registration: remove the `OPENCHATTER_REGISTRATION_ENABLED=false` line
   from `~/openchatter-prod/env` (the default is true) and
   `launchctl kickstart -k gui/$(id -u)/com.openchatter.prod`.

Rollback target is 25: `openchatterd -migrate-to 25` removes exactly the
accounts 000026 created (tracked in `users_backfill_000026`) and their links;
pre-linked and registered users stay. Then deploy the task 03 commit (without
the verify flag: the tracking table is gone).

## Ops crib sheet (on the mini)

```sh
tail -f ~/openchatter-prod/logs/openchatterd.log
launchctl kickstart -k gui/$(id -u)/com.openchatter.prod   # restart
launchctl bootout   gui/$(id -u)/com.openchatter.prod      # stop
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.openchatter.prod.plist  # start
/opt/homebrew/opt/postgresql@17/bin/psql -h localhost agentchat  # db shell
```
