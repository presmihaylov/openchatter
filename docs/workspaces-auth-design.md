# Workspaces and login for OpenChatter

Design document. Synthesized from three candidate designs (mirror, compat, clean) and three
judge verdicts, verified against the code in this repo and in claudecontrol, then revised
after Maya answered the product questions (thread 2196ade6, 2026-09-04).
Status: approved model. Task 01 (auth provider, password provider, sessions) is implemented
(commit 2ddc377); everything else is tracked in tasks/README.md. Migration numbers below:
000024 users (task 01), 000025 room users (task 03), 000026 backfill (task 04, deploy N),
000027 null human tokens (task 08, deploy N+1).

## 1 Goal

1. Workspaces. A workspace is a room. A human belongs to several workspaces (rooms) and
   toggles between them in the UI. Channels, agents, tokens and invite codes are scoped to
   a room today and therefore to a workspace. Membership of a workspace is exactly the set
   of live participants of the room.
2. Auth. Real login and registration behind an auth-provider interface. First provider:
   username + password in Postgres, bcrypt. The interface lets a second provider (Clerk)
   serve a separate deployment from the same binary.
3. Migration. Every existing human gets a global username derived from their participant
   name and the default password `developer`. Nobody is locked out.
4. Fleet. Everything that works today keeps working for agents: `act_` tokens, invite
   codes, `POST /api/v1/rooms/join`, cli.sh, watchers, /skill, `POST /api/v1/invites` with
   its Cloudflare Access block, and the Cloudflare Access front door. No agent restarts.
   The one agent-visible change: nobody unregistered may create a room, so the
   unauthenticated `POST /api/v1/rooms` goes away. The brief also names a "models
   feature": the `models` storage package and every existing API built on it (rooms,
   channels, messages, threads, attachments, search, events) keep working unchanged.

## 2 Decision and why

Winner: the compat design ("users, sessions and workspaces as a layer above rooms,
fleet-safe, reversible").

Judge totals (sum of six criteria per judge, three judges):

| Design | Judge 1 | Judge 2 | Judge 3 | Total | fleet_compat sum | migration_safety sum |
|---|---|---|---|---|---|---|
| compat | 45 | 44 | 49 | 138 | 27 | 24 |
| clean | 39 | 38 | 43 | 120 | 24 | 16 |
| mirror | 35 | 34 | 35 | 104 | 16 | 9 |

All three judges picked compat. The angle that won: rooms, participants, `act_` tokens,
invite codes and cli.sh do not change on the wire. Users and sessions are a new layer.
`authed()` gains one branch keyed on the `ses_` token prefix. The provider is consulted
once at login and mints the same opaque session for every provider, so handlers and
`authed()` never see a provider. It was the only design that read golang-migrate correctly
(an old binary refuses to start when the DB is past its embedded files) and planned a real
rollback step.

Revised after Maya decisions. The compat design put a `workspaces` table above rooms with
`workspace_members`, `workspace_invites`, `workspace_audit_events`, `rooms.workspace_id`,
an `X-Workspace-Id` resolver and a lazy projection of members into rooms. Maya decided
that a workspace IS a room (decisions 3 and 12). All of that layer is gone. Membership is
`participants.user_id`, one row per (room, user). The switcher lists the rooms a user is a
live participant of. Room routes keep `X-Workspace-Slug`. Room creation requires a logged-in
human, capped at 5 rooms per creator, and the unauthenticated create is removed together
with every script that used it. The default password is `developer`, changed on a settings
page. Clerk becomes a separate deployment with its own users, no linking. Legacy human
`act_` tokens retire at deploy N+1 (the task 08 deploy). Deploy N is the task 04 deploy, the
one that runs the backfill; tasks 01, 02, 03 and 05 each ship on their own earlier or later
deploy (section 7).

Grafts from the runner-ups that survive the revision:

- From clean: a `user_identities` table (one user, several provider rows) instead of
  `auth_provider` columns on `users`. Implemented in 000024.
- From clean: an absolute session cap on top of the sliding expiry, `last_used_at`
  refreshed at most every 5 minutes, and `DeleteUserSessions(keepHash)` on password
  change so the current tab survives. Implemented in task 01.
- From clean: `cmd/openchatter-passwd`, a mirror of claudecontrol `cmd/resetpassword`.
  Implemented in task 01.
- From mirror: legacy human `act_` tokens retired at deploy N+1 (000027).
- From mirror: `scripts/users-migration-preview.sql`, a merge report Maya reviews before
  deploy N, and a migration test that seeds two rooms with the same human.
- From mirror: `writeErrCode(w, status, code, msg)`. Implemented in task 01.

Flaws the judges verified in compat, fixed here:

- `POST /api/v1/workspaces/{slug}/enter` never adopts an unlinked participant by display name.
  Any logged-in human could otherwise take over an unlinked human's identity. Adoption is
  done only by the migration.
- `/enter` and the session branch of `authed()` both check `revoked`. A room-kicked human
  gets `403 workspace_forbidden` with `reason: "revoked"` from both, so the SPA cannot loop.
- Login never overwrites `display_name`.
- Sessions have an absolute lifetime (90 days from `created_at`) in addition to the sliding
  30 day idle window.
- pgcrypto is verified on prod before the task 01 deploy, because 000024 (already written)
  opens with `CREATE EXTENSION IF NOT EXISTS pgcrypto`. There is no fallback: a missing
  extension fails the task 01 deploy at startup, before any backfill.
- The backfill in 000026 never links a legacy participant to a pre-registered account
  (section 6), and registration stays closed on prod until 000026 has run (section 7).
- `ReclaimParticipant` refuses linked rows (`user_id IS NOT NULL`), so `/join` cannot take
  over a migrated human by name (section 10).

## 3 Identity and auth provider interface

Package `services/auth` (implemented). It imports `models`; `services/api` imports it;
`models` imports neither.

```go
// services/auth/provider.go
type Identity struct {
    Provider    string // "password" | "clerk"
    Subject     string // provider-unique id: the username for password, the Clerk user id for clerk
    Username    string // proposed login handle
    DisplayName string // "" when the provider has none; never overwrites an existing display_name
    Email       string // "" for password
}

// Provider verifies a credential once, at login. Everything after that is a ses_ session.
type Provider interface {
    Name() string
    Authenticate(ctx context.Context, body json.RawMessage) (Identity, error)
}

// Registrar is implemented by providers that create accounts locally. Only password does.
type Registrar interface {
    Register(ctx context.Context, body json.RawMessage) (Identity, error)
}

var (
    ErrBadCredentials, ErrLockedOut, ErrRegistrationDisabled, ErrWeakPassword,
    ErrPasswordTooLong, ErrBadUsername, ErrUsernameTaken error
)

type Registry struct{ byName map[string]Provider; order []string }
func NewRegistry(ps ...Provider) *Registry
func (r *Registry) Get(name string) (Provider, bool)
func (r *Registry) Names() []string // registration order; drives the login page
```

Password provider (`services/auth/password.go`, implemented): bcrypt cost 12, password
8 to 72 bytes, username `^[a-z0-9][a-z0-9_-]{1,31}$` (same shape as the
`users_username_shape` CHECK and `nameRe` in helpers.go), 5 failures per username in any
60 s window lock the username out (429), an unknown username still runs one bcrypt compare
against a fixed dummy hash. Store contract satisfied by `*models.Store`:

```go
type PasswordStore interface {
    PasswordIdentity(ctx, username) (userID string, hash []byte, err error)
    CreatePasswordUser(ctx, username, displayName string, hash []byte) (models.User, error)
    SetPasswordHash(ctx, userID string, hash, keepSessionHash []byte) (revoked int64, err error)
}
func (p *PasswordProvider) ChangePassword(ctx, username, current, next string, keepSessionHash []byte) error
```

Clerk provider (later, same interface, separate deployment; section 11):

```go
// services/auth/clerk.go
type ClerkProvider struct{ jwks *jwks.Client }
func NewClerkProvider(secretKey string) *ClerkProvider
func (c *ClerkProvider) Name() string { return "clerk" }
// body {"token":"<Clerk session JWT>"}: verify with JWKS, user.Get for the primary email,
// a port of claudecontrol ccbackend/middleware/clerk_verifier.go. Subject = claims.Subject.
```

Wiring (`cmd/openchatterd/main.go`, implemented for password):

```go
providers := []auth.Provider{auth.NewPasswordProvider(store, registration)}
if k := os.Getenv("CLERK_SECRET_KEY"); k != "" {  // Clerk deployment only
    providers = append(providers, auth.NewClerkProvider(k))
}
server := api.New(store, api.Config{..., Providers: auth.NewRegistry(providers...),
    SessionTTL: sessionTTL, RegistrationEnabled: registration})
```

Login exchange (`services/api/handlers_auth.go`, implemented): `POST
/api/v1/auth/{provider}/login` looks the provider up (404 `unknown_provider`), calls
`Authenticate` (401 `invalid_credentials`, 429 `locked_out`), then `issueSession`:
`store.UserByIdentity(ctx, provider, subject)`, `secrets.NewSessionToken()`,
`store.CreateSession(...)`, response `{token, expires_at, user}`. A miss in
`UserByIdentity` for the password provider is 401, because `Register` created the identity.
A Clerk deployment creates the user on first login inside `UserByIdentity` (username from
the email local part, deduped `-2`, `-3`).

`api.New` panics when `cfg.Providers` is nil. New dependency: `golang.org/x/crypto`
(bcrypt). Clerk adds `github.com/clerk/clerk-sdk-go/v2` only when that file lands.

## 4 Sessions

Format: `ses_` + 32 base58 chars from `pkg/secrets.NewSessionToken()`, the same generator
as `NewToken` with a different prefix. Only `sha256(token)` is stored
(`sessions.token_hash bytea UNIQUE`). The plaintext is returned once in the login or
register response.

Lifetime (implemented in `models/sessions.go`): sliding idle window `OPENCHATTER_SESSION_TTL`
(default 720h) and an absolute cap `models.SessionMaxAge` of 90 days from `created_at`.
`SessionByTokenHash` refreshes `last_used_at` and `expires_at = LEAST(now() + ttl,
created_at + 90d)` at most once per `SessionTouchEvery` (5 minutes). An hourly
`SweepSessions` runs on the existing ticker next to `DeleteOrphanAttachments`. Logout
deletes the row. Password change deletes the user's other sessions in the same transaction
as the new hash.

Transport: `Authorization: Bearer ses_...`, the same header agents use. The SPA keeps it in
`localStorage['agentchat:session']`. No cookies anywhere, so CSRF does not apply. The
Cloudflare Access cookie belongs to Cloudflare and never reaches this code.

Dispatch (`services/api/server.go authed()`): task 01 inserted a `strings.HasPrefix(token,
"ses_")` branch before the untouched `act_` path. Today that branch validates the session
and answers `403 no_room`. Task 03 replaces the body of the branch with
`participantForSession`; the `act_` path below stays byte for byte:

```go
if strings.HasPrefix(token, "ses_") {
    p, ok := s.participantForSession(w, r, token) // writes its own error
    if !ok { return }
    _ = s.store.TouchPresence(r.Context(), p.RoomID, p.ID)
    h(w, r, p)
    return
}
```

`participantForSession` reads the room slug from `X-Workspace-Slug` and runs one query,
`models.SessionScope(ctx, hash, slug, ttl)`:

```sql
SELECT u.id, u.username, u.display_name, u.must_change_password, u.last_active_room_id,
       r.id,
       p.id, p.name, ..., p.revoked, o.name   -- the columns ParticipantByTokenHash scans, plus revoked
FROM sessions s
JOIN users u ON u.id = s.user_id
LEFT JOIN rooms r ON r.slug = $2
LEFT JOIN participants p ON p.room_id = r.id AND p.user_id = u.id
LEFT JOIN participants o ON o.id = p.owner_id
WHERE s.token_hash = $1 AND s.expires_at > now() AND s.created_at > now() - $3::interval
```

Outcomes:

| State | Response |
|---|---|
| no session row | 401 `{"error":"session expired","code":"session_invalid"}` |
| task 01 and 02 binaries only | 403 `no_room` for every session on a room route; the SPA sends the per-slug `act_` token instead until task 03 (section 9) |
| `X-Workspace-Slug` missing | 400 `workspace_required` |
| room slug unknown | 404 |
| no participant row for (room, user) | 403 `workspace_forbidden`, `reason: "not_member"` |
| participant exists and `revoked` | 403 `workspace_forbidden`, `reason: "revoked"` |
| live participant | continue; one query on the hot path, same cost as agents |

There is no lazy projection. The participant row is the membership. When the resolved
room differs from `users.last_active_room_id` (JSON key `last_active_workspace_id`), the
query path updates the pointer
(one extra UPDATE, at most once per room switch). The `models.User` rides on the request
context; `handleGetMe` adds `user_id` and `username` to its response when present
(omitempty). Nothing else reads it.

Wrapper for non-room routes (`withSession`, implemented): `Bearer ses_` only; an `act_`
token gets 401 `session_required`. Agents have no user.

Legacy human `act_` tokens keep working through the default branch until migration 000027
retires them at deploy N+1. The backfill links those participant rows to the new users, so
a login lands on the same identity, role, history and tags.

## 5 Workspace model and request scoping

A workspace is a row of `rooms`. There is no grouping table above rooms. The UI says
"workspace"; the API keeps `/api/v1/rooms/*` and `/api/v1/room` paths and the `room`
JSON key, because agents, cli.sh and watchers speak them.

Scoping: every domain table either carries `room_id NOT NULL ON DELETE CASCADE`
(rooms, participants, channels, messages, attachments, events, invites) or hangs off a row
that does through a cascading foreign key (channel_reads, thread_states, message_reactions,
message_embeddings, channel_groups, channel_group_items, mentions, participant_tags,
message_attachments, channel_members), and every room handler scopes by `p.RoomID`.
Channels, agents, `act_` tokens, room invite codes and owner-scoped invites are workspace
scoped for free. No scope column is added to any table agents write to.

Membership: a user is a member of a workspace when a row exists in `participants` with
`room_id = room`, `user_id = user` and `revoked = false`. `participants.user_id` is
nullable (agents and cli humans have none). The partial unique index
`participants_room_user_key (room_id, user_id) WHERE user_id IS NOT NULL` makes one user
at most one participant per room and makes a room kick sticky.

Roles: room roles `admin|member`, `ErrLastAdmin`, `SetRole`, `Revoke` stay as they are.
Kicking a human (`DELETE /api/v1/participants/{id}`) sets `revoked` and closes both the
session door (`reason: revoked`) and the legacy `act_` door (`ParticipantByTokenHash`
already refuses revoked rows). No separate member-removal endpoint.

Membership entry points, both require a logged-in human:

1. Creator on `POST /api/v1/rooms`: `CreateRoomAs` inserts the room, the creator's admin
   participant (`is_human = true`, `user_id = creator`, `token_hash = NULL`), joins
   `#general` and emits `participant.joined` with a
   `user_id` field, in one transaction, room advisory lock first. `rooms.created_by_user_id`
   records the creator. Quota: at most 5 rooms per creator, counted under
   `pg_advisory_xact_lock(hashtext('room-create:' || user_id))`, 409 `workspace_quota`.
   The creator's participant name is derived the way `/enter` derives it: `display_name`
   when `validParticipantName` (helpers.go, 2 to 32 chars, letters, digits, single spaces,
   `-` or `_`) accepts it, else `username`; then `-2`, `-3` against existing rows of the
   room. Register only trims `display_name`, so a name like a 40 char string or one with an
   emoji must not reach `participants.name`.
2. `POST /api/v1/workspaces/{slug}/enter` with a valid room invite code (`RoomByAnySecret`,
   unchanged). The code must resolve to the room named by `{slug}`, else 400
   `invite_invalid`. A new participant is created with `user_id` set; an owner-scoped
   code sets `owner_id` exactly as a `/join` would. Name conflicts with an existing
   participant get `-2`, `-3`; `/enter` never adopts an existing row.

Who cannot create a room: agents (`act_` tokens) and anyone without a session; both get
401 `session_required` from `withSession`, which writes that one code for every non `ses_`
bearer, an empty one included. Agents ask their human to create a workspace in
the web UI. Registration does not create a personal workspace; a fresh account lands on
"You are in no workspace" with a create form and an invite-code form.

Error codes (JSON `{"error","code"}` via `writeErrCode`; `writeStoreErr` maps
`models.ErrRoomQuota`, `ErrInviteInvalid`):

| Code | Status | Meaning |
|---|---|---|
| `session_invalid` | 401 | session missing, expired or unknown; SPA clears the token and goes to /login |
| `session_required` | 401 | an `act_` token or no token hit a user route or `POST /api/v1/rooms`; the one code `withSession` writes |
| `workspace_required` | 400 | session request to a room route without `X-Workspace-Slug` |
| `workspace_forbidden` | 403 | `reason: not_member` (SPA shows the enter view) or `reason: revoked` (SPA shows "removed from this workspace") |
| `workspace_quota` | 409 | 5 rooms already created by this user |
| `invite_invalid` | 400 | the code on `/enter` does not open this room |
| `no_room` | 403 | task 01 placeholder for a session on a room route; replaced by the table in section 4 in task 03 |

Models: `models.User{ID, Username, DisplayName, Email *string, MustChangePassword,
CreatedAt}` (implemented, models/models.go) gains `LastActiveWorkspaceID *string
json:"last_active_workspace_id,omitempty"` in task 03, scanned from the column
`users.last_active_room_id`. `models.Room` gains `CreatedByUserID *string
json:"created_by_user_id,omitempty"`. `models.Participant` gains `UserID *string
json:"user_id,omitempty"`. `CreateParticipant` gains a trailing `userID *string` and
accepts `tokenHash == nil`; `handleJoinRoom` passes `nil`, so an agent join writes the same
row as today plus `user_id NULL`. New `CreateRoomAs(ctx, name, slug, secret string,
creator models.User) (Room, Participant, error)`. `CreateRoom(ctx, name, slug, secret)`
stays for tests and the backfill. New `RoomsByUser(ctx, userID) ([]UserRoom, error)` for
the switcher.

## 6 Schema (SQL, ordered)

Migration 000024 is implemented (commit 2ddc377) and stays as it is. Its
`users.last_active_room_id` column is the "last opened room" hint; 000025 adds the FK to
`rooms`. The listing below is a condensed transcript of the file, not the file itself.

```sql
-- migrations/000024_users.up.sql  (implemented; condensed)
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- crypt()/gen_salt('bf') for the bcrypt backfill in 000026
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    email text,
    must_change_password boolean NOT NULL DEFAULT false,
    last_active_room_id uuid,                 -- FK to rooms added in 000025; hint only, re-validated per request
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_username_key UNIQUE (username),
    CONSTRAINT users_username_shape CHECK (username ~ '^[a-z0-9][a-z0-9_-]{1,31}$')
);
CREATE INDEX users_email_idx ON users (lower(email)) WHERE email IS NOT NULL;
CREATE TABLE user_identities (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider text NOT NULL, subject text NOT NULL,
    password_hash text, password_changed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject),
    CONSTRAINT user_identities_password_present CHECK (provider <> 'password' OR password_hash IS NOT NULL)
);
CREATE INDEX user_identities_user_idx ON user_identities (user_id);
CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE, provider text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
-- 000024 down: DROP TABLE sessions, user_identities, users
```

```sql
-- migrations/000025_room_users.up.sql
-- membership is the participant row; one user is at most one participant per room
ALTER TABLE participants ADD COLUMN user_id uuid REFERENCES users(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX participants_room_user_key ON participants (room_id, user_id) WHERE user_id IS NOT NULL;
CREATE INDEX participants_user_idx ON participants (user_id) WHERE user_id IS NOT NULL;

-- who created the room; drives the 5-per-user quota
ALTER TABLE rooms ADD COLUMN created_by_user_id uuid REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX rooms_created_by_idx ON rooms (created_by_user_id) WHERE created_by_user_id IS NOT NULL;

-- a workspace is a room, so the sticky pointer points at a room
ALTER TABLE users ADD CONSTRAINT users_last_active_room_fkey
    FOREIGN KEY (last_active_room_id) REFERENCES rooms(id) ON DELETE SET NULL;

-- a human created from a session holds no participant token; CreateRoomAs (task 03) needs this
ALTER TABLE participants ALTER COLUMN token_hash DROP NOT NULL;

-- migrations/000025_room_users.down.sql
UPDATE participants SET token_hash = sha256(('retired:' || id::text)::bytea) WHERE token_hash IS NULL;
ALTER TABLE participants ALTER COLUMN token_hash SET NOT NULL;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_last_active_room_fkey;
DROP INDEX IF EXISTS rooms_created_by_idx;
ALTER TABLE rooms DROP COLUMN IF EXISTS created_by_user_id;
DROP INDEX IF EXISTS participants_user_idx;
DROP INDEX IF EXISTS participants_room_user_key;
ALTER TABLE participants DROP COLUMN IF EXISTS user_id;
```

```sql
-- migrations/000026_backfill_users.up.sql
-- username = lower(btrim(name)), whitespace runs -> '-', anything outside [a-z0-9_-] dropped;
-- a result that fails the username shape falls back to 'user-' + first 8 hex of the participant id.
-- The same derived username across rooms is ONE user (usernames are global, decision 1).
-- Two humans in the SAME room that derive the same username: the live, most recently seen one
-- keeps the plain username, the others get '-2', '-3'.
-- A username that a registered account already holds is a collision, never a link: the legacy
-- row takes the next free '-2', '-3' instead. Registration is closed on prod until this file
-- has run (section 7), so this branch is a guard, not the expected path.
CREATE TEMP TABLE human_rows AS
WITH base AS (
    SELECT p.id, p.room_id, p.name, p.created_at, p.last_seen_at, p.revoked, p.role,
           regexp_replace(lower(regexp_replace(btrim(p.name), '\s+', '-', 'g')), '[^a-z0-9_-]', '', 'g') AS uname
    FROM participants p
    WHERE p.is_human AND p.user_id IS NULL
), shaped AS (
    SELECT *, CASE WHEN uname ~ '^[a-z0-9][a-z0-9_-]{1,31}$' THEN uname
                   ELSE 'user-' || left(replace(id::text, '-', ''), 8) END AS uname2
    FROM base
), ranked AS (
    SELECT *, row_number() OVER (PARTITION BY room_id, uname2 ORDER BY revoked, last_seen_at DESC, id) AS rn
    FROM shaped
), named AS (
    SELECT *, CASE WHEN rn = 1 THEN uname2 ELSE left(uname2, 29) || '-' || rn END AS username0
    FROM ranked
)
SELECT id AS participant_id, room_id, name, created_at, last_seen_at, revoked, role,
       CASE WHEN NOT EXISTS (SELECT 1 FROM users u WHERE u.username = n.username0) THEN n.username0
            ELSE (SELECT left(n.username0, 28) || '-' || k FROM generate_series(2, 99) k
                  WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.username = left(n.username0, 28) || '-' || k)
                  ORDER BY k LIMIT 1) END AS username
FROM named n;

-- accounts only for humans who hold at least one live participant row; revoked-only humans get no login.
-- No ON CONFLICT: a residual collision aborts the transaction and the deploy fails loudly
-- (the preview script reports collisions before the deploy).
CREATE TEMP TABLE new_users (id uuid, username text);
WITH ins AS (
    INSERT INTO users (username, display_name, must_change_password, created_at)
    SELECT DISTINCT ON (h.username) h.username, h.name, true, h.created_at
    FROM human_rows h
    WHERE NOT h.revoked
    ORDER BY h.username, h.last_seen_at DESC
    RETURNING id, username
)
INSERT INTO new_users SELECT id, username FROM ins;

-- password "developer", bcrypt cost 12, fresh salt per row ($2a$, verified by golang.org/x/crypto/bcrypt);
-- only for the accounts this file created, never for a registered user
INSERT INTO user_identities (user_id, provider, subject, password_hash)
SELECT u.id, 'password', u.username, crypt('developer', gen_salt('bf', 12))
FROM new_users u;

-- link every human row (revoked ones too, so a room kick stays sticky) to the user this file created
UPDATE participants p SET user_id = u.id
FROM human_rows h JOIN new_users u ON u.username = h.username
WHERE p.id = h.participant_id AND p.user_id IS NULL;

-- creator = the earliest live human admin; NULL when a room has none (agent-only rooms)
UPDATE rooms r SET created_by_user_id = (
    SELECT p.user_id FROM participants p
    WHERE p.room_id = r.id AND p.user_id IS NOT NULL AND p.role = 'admin' AND NOT p.revoked
    ORDER BY p.created_at LIMIT 1)
WHERE r.created_by_user_id IS NULL;

-- last opened workspace = the room where the human was seen most recently
UPDATE users u SET last_active_room_id = (
    SELECT p.room_id FROM participants p WHERE p.user_id = u.id AND NOT p.revoked
    ORDER BY p.last_seen_at DESC LIMIT 1)
WHERE u.last_active_room_id IS NULL;

DROP TABLE new_users;
DROP TABLE human_rows;
-- verification (each must be 0):
-- SELECT count(*) FROM participants WHERE is_human AND NOT revoked AND user_id IS NULL;
-- SELECT count(*) FROM users u LEFT JOIN user_identities i ON i.user_id = u.id WHERE i.user_id IS NULL;
-- SELECT count(*) FROM (SELECT room_id, user_id FROM participants WHERE user_id IS NOT NULL
--   GROUP BY 1, 2 HAVING count(*) > 1) d;

-- migrations/000026_backfill_users.down.sql  (rollback window only: accounts registered after deploy N are lost too)
UPDATE participants SET user_id = NULL;
UPDATE rooms SET created_by_user_id = NULL;
UPDATE users SET last_active_room_id = NULL;
DELETE FROM users;
```

Deploy N+1 (task 08):

```sql
-- migrations/000027_null_human_tokens.up.sql
-- retires legacy human browser act_ tokens; humans log in (decision 8)
UPDATE participants SET token_hash = NULL WHERE is_human AND user_id IS NOT NULL;
-- migrations/000027_null_human_tokens.down.sql: no-op (tokens cannot be restored; humans log in)
```

Indexes reused unchanged: `participants.token_hash UNIQUE` (NULLs do not collide),
`rooms_slug_key`, `events_room_seq_idx`.

## 7 Migration plan

Files are embedded through `migrations/embed.go` and applied at startup by `models.Open`
(models/store.go:31). Numbering is strictly sequential and every file that ships in a
deploy has a higher number than every file already applied. golang-migrate applies only
versions above the current one, so a lower-numbered file shipped later would never run.

Deploys, one per task (tasks/README.md), each with its own rollback target. "Deploy N" in
this document is the task 04 deploy, the one that runs the backfill:

| Deploy | Files | Rollback target (`-migrate-to`) |
|---|---|---|
| task 01 (on prod) | `000024_users`: pgcrypto, `users`, `user_identities`, `sessions`. Additive. Implemented | 23 |
| task 02 | none | 24 |
| task 03 | `000025_room_users`: `participants.user_id` plus the partial unique index and the user index, `rooms.created_by_user_id` plus its index, the `users.last_active_room_id` FK to rooms, `participants.token_hash` DROP NOT NULL. Additive | 24 |
| task 04 = deploy N | `000026_backfill_users`: humans to users, identities, links, creator, sticky pointer. Idempotent through `p.user_id IS NULL` (a second run finds no unlinked human) | 25 |
| task 05 | none (switcher UI, `/w/{slug}`, `GET /api/v1/user` workspaces) | 26 |
| task 08 = deploy N+1, at least 7 days after deploy N and after a full e2e pass on prod | `000027_null_human_tokens`: retires legacy human `act_` tokens | none (point of no return for human tokens) |

Registration on prod: `OPENCHATTER_REGISTRATION_ENABLED=false` in the mini's env file from the
task 01 deploy until 000026 has run at deploy N. Otherwise anyone could register `maya` (or
any existing human's derived name) in the window and the backfill would have to treat it
as a collision. 000026 does treat it as a collision (section 6), but closed registration
keeps the merge report exact. The task 04 deploy flips the toggle to true after the
verification queries pass.

Backfill rules:
Backfill rules:

- Username derivation: `lower(btrim(name))`, whitespace runs to `-`, strip everything
  outside `[a-z0-9_-]`; if the result fails `^[a-z0-9][a-z0-9_-]{1,31}$`, use `user-` +
  first 8 hex of the participant id. Examples: `Maria Chen` to `maria-chen`, `Maya` to
  `maya`, an emoji-only name to `user-1a2b3c4d`.
- Cross-room merge: the same derived username across rooms is one user with one participant
  row per room (decision 1). Inside one room, rows that derive the same username are ranked
  live first, then most recently seen; the first keeps the plain username, the rest get
  `-2`, `-3` and become separate users. `display_name` comes from the most recently seen
  live row.
- Revoked humans: linked to their user when one exists (so the room kick stays sticky
  through the `(room_id, user_id)` unique index), but a human with only revoked rows gets
  no account.
- Pre-registered usernames: a derived username that already exists in `users` gets the next
  free `-2`, `-3` and a fresh account; the registered account is never linked and never
  receives a default password. The preview script lists these collisions.
- Default password: `crypt('developer', gen_salt('bf', 12))` from pgcrypto, a `$2a$`
  bcrypt hash with a fresh salt per row, cost 12. `must_change_password = true` drives the
  banner. pgcrypto is created by 000024, so the check `SELECT 1 FROM
  pg_available_extensions WHERE name = 'pgcrypto'` runs on the mini before the task 01
  deploy (present on the dev pgvector pg16 image; prod is brew postgresql@17, which ships
  contrib, verify anyway). There is no fallback.
- Verification queries at the bottom of 000026 must return 0. `scripts/deploy-prod.sh`
  gains a post-migrate step that runs them through psql on the mini and fails loudly.

Pre-deploy: `scripts/users-migration-preview.sql` runs the `human_rows` CTE read-only and
prints the merge report (`username, array_agg(DISTINCT name), array_agg(DISTINCT room
slug), count(*)` where count > 1), the collision report (derived usernames that already
exist in `users`) plus the verification counts. Maya reviews the merges. A wrong merge is
fixed by renaming the participant before deploy N. `scripts/deploy-prod.sh` gains
`pg_dump -Fc` on the mini before the binary swap in task 01 (today it takes none;
docs/PROD.md has no backup section), so every deploy from task 01 on has a dump.

Rollback: golang-migrate v4.19.1 `readUp` calls `versionExists(from)` (migrate.go:537), so
an old binary whose embedded files stop below the DB version fails `models.Open` with `no
migration found for version <n>`. Rolling back any deploy is therefore two steps: with the
current binary, `openchatterd -migrate-to <version embedded in the target commit>` (flag and
`models.MigrateTo(dbURL, v)`, which calls `m.Migrate(v)`, ship in task 01), then
`scripts/deploy-prod.sh <target commit>`. Targets: 23 to go back before task 01, 24 before
task 03, 25 before task 04. Cost: rolling back task 01 deletes every account and session;
rolling back task 04 unlinks participants and deletes the accounts the backfill created plus
any registered since; rolling back task 03 drops `created_by_user_id` and `user_id` and
gives session-created participants a placeholder token hash. Participants, rooms, agent
tokens, messages and events are untouched by every down file, so every agent stays online
through a rollback. The 7 day window counts from deploy N (task 04). After deploy N+1 the
down files still work technically, but 000027 cannot restore human tokens; that deploy is
the point of no return for human browser tokens.

NOT NULL timing: `participants.user_id` stays nullable forever (agents, cli humans).
`participants.token_hash` stays nullable forever (session humans).
`rooms.created_by_user_id` stays nullable forever (agent-only rooms from before deploy N).
`user_identities.password_hash` stays nullable (Clerk rows) with the CHECK for the
password provider.

Also in deploy N: `web/dist` rebuilt as on every deploy (the login page shipped with task
02, the switcher ships with task 05), `OPENCHATTER_SESSION_TTL` unchanged (defaults 720h),
`OPENCHATTER_REGISTRATION_ENABLED` flipped from false to true after the verification queries
pass. No agent restart, no env file change on any agent machine, no cli.sh re-download.

Tests: `models/store_test.go TestBackfillUsers` migrates a dedicated database to 23 with
golang-migrate (`OPENCHATTER_MIGRATE_TEST_DB_URL`, skipped when unset), seeds two rooms with
`Maya` in both, `Maria Chen` in one, a revoked `Eve`, `maria chen` as a second row in
the same room, and a pre-registered user `sam` (registered through the API at version 25)
next to a legacy participant `Sam`, runs `Up()`, and asserts: one user `maya` linked to two
participant rows, `maria-chen` linked to the live row and `maria-chen-2` for the other, no
user `eve`, the legacy `Sam` linked to a new user `sam-2` while the registered `sam` keeps
its password hash and zero participant links,
`bcrypt.CompareHashAndPassword(hash, "developer") == nil`, `created_by_user_id` set on
both rooms, and `POST /auth/password/login` plus `X-Workspace-Slug` resolves to the original
participant id. `api_test.go` gains `TestSessionAuthResolvesParticipant`,
`TestSessionWithoutParticipantIs403`, `TestRevokedHumanIs403FromEnterAndRoom`,
`TestEnterWithInviteCodeCreatesLinkedParticipant`, `TestEnterDoesNotAdoptByName`,
`TestEnterWrongCodeIs400`, `TestActTokenIgnoresRoomHeader`, `TestRoomCreateRequiresSession`,
`TestRoomQuota`, `TestUserRoomsListsLiveParticipations`, `TestSessionAbsoluteCap`,
`TestAgentJoinRowUnchanged`, `TestJoinCannotReclaimLinkedHuman`,
`TestRoomCreateInvalidDisplayNameUsesUsername` (a 40-char or emoji `display_name` yields
a participant named after the username).

## 8 API

Auth column: none | act_ | ses_ | room (ses_ plus `X-Workspace-Slug`, or act_). Bodies go
through `readJSON` (`DisallowUnknownFields`). Errors are `{"error","code"}`.

| Method and path | Auth | Body | Response | Notes |
|---|---|---|---|---|
| GET /api/v1/auth/providers | none | | `{providers:["password"], registration_enabled}` | drives the login page; implemented. `clerk_publishable_key?` is added in task 07 (Clerk deployment) |
| POST /api/v1/auth/password/register | none, joinLimit | `{username, password, display_name}` | 201 `{token:"ses_...", expires_at, user}` | 403 `registration_disabled`; 409 `username_taken`; 400 `weak_password`, `password_too_long`, `bad_username`; 429 `rate_limited` (joinLimit per IP); implemented. No personal workspace is created |
| POST /api/v1/auth/{provider}/login | none, joinLimit | provider body | 200 same shape | 401 `invalid_credentials`; 404 `unknown_provider`; 429 `locked_out` (per username) or `rate_limited` (joinLimit per IP); implemented |
| POST /api/v1/auth/logout | ses_ | | 204 | deletes the session row; implemented |
| POST /api/v1/auth/password/change | ses_ | `{current_password, new_password}` | 204 | clears `must_change_password`; deletes the user's other sessions; 400 `password_too_long`, `weak_password`; 401 `invalid_credentials`; 429 `rate_limited`; implemented |
| GET /api/v1/user | ses_ | | `{user, last_active_workspace_id, workspaces:[{id, slug, name, role, joined_at}]}` | one call for the switcher: the rooms the user is a live participant of, `ORDER BY p.created_at`. Task 01 returns `{user}` only; the rest arrives in task 05 |
| POST /api/v1/rooms | ses_ only | `{name}` | 201 `{room, join_url, invite_code}` | `CreateRoomAs`; creator admin; 409 `workspace_quota` over 5; 401 `session_required` without a token and for `act_` (one code, from `withSession`). The one existing handler that changes; task 03 |
| POST /api/v1/workspaces/{slug}/enter | ses_ | `{invite_code?}` | 200 `{participant, room}` | task 03; live participant: idempotent, code ignored; revoked: 403 `workspace_forbidden` `reason: revoked`; no participant: code required, must open this slug, else 400 `invite_invalid`; 404 unknown slug. Never adopts by name |
| GET /api/v1/me | room | | Participant plus `user_id`, `username` when linked | omitempty; agents see no change |
| all other /api/v1/* room routes | room | unchanged | unchanged plus `participant.user_id` on linked humans, `room.created_by_user_id` | paths and handlers untouched |
| POST /api/v1/rooms/join, GET /api/v1/rooms/peek, POST /api/v1/invites, GET /cli.sh, GET /api/v1/events | as today | | unchanged | agents keep joining with a code; `/join` refuses to reclaim a linked human row (section 10); owner-scoped invites keep their Cloudflare Access block |
| GET /skill, /skill/claude-code, /skill/hermes | none | | markdown | skill text changes in the create section only (task 03); a humans-and-workspaces section and the harness guides follow in task 06 |

Humans do not mint `act_` tokens (decision 9). There is no `POST /api/v1/me/token`.
Humans are web-only; `cli.sh` stays an agent tool.

Naming of the new surface follows decision 3: new paths, headers and codes say workspace
(`/api/v1/workspaces/{slug}/enter`, `X-Workspace-Slug`, `workspace_required`,
`workspace_forbidden`, `workspace_quota`); every existing `/api/v1/rooms/*` path, the
`room` JSON key and `POST /api/v1/rooms` itself keep their names because agents, cli.sh
and watchers speak them.

Routes served by `serveApp`: add `GET /login`, `/register`, `/settings`, `/w/{slug}`.
`/w/{slug}` is the workspace route the switcher navigates to; the SPA treats it exactly as
`/r/{slug}`, which stays canonical for `join_url` and old links. `GET /{$}` redirects to
`/login` instead of `/create`. `/create` stays and means "create a workspace" (session
required, else `/login?next=/create`).

Removing the unauthenticated room create touches these files in the same change (task 03),
found with `grep -lE "api/v1/rooms['\"\`,) ]" scripts/*.js` and `grep -rn 'api/v1/rooms"'`:

- `services/api/server.go` route table (`POST /api/v1/rooms` wrapped in `withSession`) and
  `services/api/handlers_rooms.go` (`handleCreateRoom` takes a `models.User`).
- `services/api/skill.go`: the "## Creating a new room" section is rewritten (section 10),
  with `TestSkillDoc` and `TestSkillHarnessGuides`.
- `services/api/api_test.go`: `setupRoom` and the three other `POST /api/v1/rooms` sites
  (`grep -n '"/api/v1/rooms"' services/api/api_test.go`) register a user and send the
  session.
- `scripts/cli-e2e.sh` room setup: registers a throwaway user with curl and creates the
  room with the `ses_` token. `services/api/cli.sh` itself has zero diff.
- `scripts/e2e.sh` creates its room through `$CLI create-room`, the `create-room` case in
  `cmd/agentchat/main.go`. `create-room` gains a `--session <ses_ token>` flag (or
  `OPENCHATTER_SESSION` in the env); e2e.sh registers a user with curl first.
- `scripts/lib/login.js`: new helper, `registerAndLogin(base, username)` returns a `ses_`
  token and `createRoom(base, session, name)` returns `{room, invite_code}`.
- The 48 browser scripts that call `api('/api/v1/rooms', {method: 'POST'})` switch to
  the helper in one mechanical sweep: archive-check.js, attach-check.js, browse-check.js,
  channeladd-check.js, chanlink-check.js, codeblock-check.js, composer-check.js,
  copy-check.js, density-check.js, dnd-check.js, docpreview-check.js, emoji-check.js,
  groups-check.js, invite-check.js, list-check.js, liveupdate-check.js, mdrender-check.js,
  members-check.js, membership-check.js, mention-check.js, mentionbadge-check.js,
  moreactions-check.js, msgsync-check.js, notify-check.js, offline-sort-check.js,
  optimistic-check.js, ownerbadge-check.js, participanttree-check.js, permalink-check.js,
  privacy-check.js, private-check.js, reactions-check.js, replybar-check.js,
  roster-check.js, scrollbar-check.js, search-check.js, slashcmd-check.js,
  subscribe-check.js, systementry-check.js, theme-check.js, thread-active-check.js,
  threadmenu-check.js, threadresize-check.js, threadswitch-check.js, threadtree-check.js,
  threadwidth-check.js, ui-smoke.js, url-check.js.

Config and env (`cmd/openchatterd/main.go`, env files only): `OPENCHATTER_REGISTRATION_ENABLED`
(default true, implemented), `OPENCHATTER_SESSION_TTL` (default 720h, implemented),
`CLERK_SECRET_KEY` and `CLERK_PUBLISHABLE_KEY` (Clerk deployment only). `openchatterd
-migrate-to <version>` flag (task 01). `cmd/openchatter-passwd <username> <password>` for manual resets
(implemented). There is no `OPENCHATTER_LEGACY_ROOM_CREATE` flag: the unauthenticated create
is removed, not gated.

## 9 UI

Vocabulary: the web UI says "workspace" everywhere a person sees a room. The existing
`#create-view` ("Create a workspace", "Workspace name") already matches and stays. The
`#join-view` copy ("This link does not point to a workspace.") matches too. The API and the
skill keep saying "room".

`web/index.html`:

- `#login-view` at `/login`: username, password, submit, "Create account" link, one button
  per extra provider from `GET /api/v1/auth/providers`.
- `#register-view` at `/register`: username, password, display name; hidden when
  `registration_enabled` is false.
- `#settings-view` at `/settings`: a Change password form (`current_password`,
  `new_password`, confirm) for the password provider, and Sign out. This is the settings
  page (decision 7).
- `#pw-banner`: shown on every page while `user.must_change_password` is true, one line
  with a link to `/settings`. It disappears after a successful change.
- `#no-ws-view`: "You are in no workspace", a create form (name only) and an invite-code
  form (`slug` from the pasted link plus the code).
- `#enter-view` replaces `#join-view`: workspace name from `/rooms/peek`, one field, the
  invite code. The name, avatar and about fields go; the account supplies them. Shown on
  `workspace_forbidden` with `reason: not_member`. A `reason: revoked` shows "You were removed
  from this workspace" instead. `#join-view` is dropped; humans never see the old form
  again.
- `#ws-switcher` button and `#ws-menu` inside `header#room-header` above `#room-name`:
  the current workspace name, the other workspaces from `GET /api/v1/user`, then "Create
  workspace", "Settings", "Sign out". Switch = navigate to `/w/<slug>` (full load).
- `#create-view`: name only; the "Your name" field goes.

`web/src/app.js`:

- `api()` attaches `Authorization: Bearer <session>` from
  `localStorage['agentchat:session']` and `X-Workspace-Slug: <slug>` on room pages. The three
  raw `fetch('/api/v1/attachments/...')` calls go through the same header builder. Task 02
  ships the builder with one rule: on `/r/{slug}` a per-slug `act_` token wins over the
  session, and the session header goes only to `/login`, `/register`, `/settings` and
  `/create`, because the task 02 binary still answers `403 no_room` to a session on a room
  route. Task 03 lifts the rule for room pages.
- Error routing in `api()`: 401 `session_invalid` clears the session and goes to
  `/login?next=<path>`; 403 `workspace_forbidden` shows `#enter-view` or the removed notice by
  `reason`. The two `e.status === 401` reload sites (eventLoop line 1664, boot line 2500)
  route through the same function.
- Boot: session present and path `/r/{slug}` or `/w/{slug}`: `enterChat()` as today
  (`GET /api/v1/me` with the session), on `not_member` show `#enter-view`, then delete the
  legacy `localStorage['agentchat:'+slug]` key. Session present and path `/`:
  `GET /api/v1/user`, then `/w/<last_active or first workspace>` or `#no-ws-view`. No
  session but a legacy per-slug `act_` token: boot exactly as today and show a one-line
  banner "Sign in with your username (<derived>) to use this identity everywhere".
  Neither: `/login?next=`.
- The `/create` block (`isCreatePage`) becomes `POST /api/v1/rooms` with the session,
  then `location.href = '/r/' + room.slug` (task 03; `/w/` is an alias from task 05). It no
  longer joins with the code, because the creator is already the admin participant.
- New `web/src/auth.js` holds login, register, settings and switcher code; `main.js`
  imports it.

Tests: `scripts/lib/login.js` also exports `loginPage(page, base, username)` (register
through the API, seed `localStorage['agentchat:session']`, open the room) and
`enterWithCode(page, code)`. The 11 scripts that type into `#join-form` switch to it in one
mechanical sweep (task 05; `#enter-view` itself ships in task 03 for session users, `#join-view` stays for the rest until 05): codeblock-check.js, copy-check.js, docpreview-check.js,
invite-check.js, liveupdate-check.js, msgsync-check.js, ownerbadge-check.js,
participanttree-check.js, replybar-check.js, ui-smoke.js, url-check.js. New
`scripts/login-check.js` (LOGIN_CHECK_OK: register, logout, login, wrong password 401,
lockout 429, banner, change password on `/settings` clears the banner and revokes the
other tab) and `scripts/switcher-check.js` (SWITCHER_CHECK_OK: two workspaces, switcher,
enter a third with a code, post a message, revoked user sees the removed notice, quota
409 on the sixth create).

## 10 Fleet compatibility guarantees

By file, what does not change:

- `services/api/server.go authed()`: the `act_` path is today's code verbatim. Only the
  `strings.HasPrefix(token, "ses_")` branch sits before it. `pkg/secrets.NewToken` always
  prefixes `act_`, so no participant token can start with `ses_`.
- `models/participants.go`: `ParticipantByTokenHash`, `Revoke`, `SetRole`,
  `TouchPresence`, `SweepPresence` untouched. `CreateParticipant` gains a trailing
  `userID *string`; `handleJoinRoom` passes `nil`. `ReclaimParticipant` gains one refusal:
  a row with `user_id IS NOT NULL` returns `ErrConflict` like a revoked row. Agents keep
  `user_id NULL`, so the agent path is byte-identical; without it anyone with the room code
  could `/join` as an offline linked human, post as that person and drop them to member.
  `TestJoinCannotReclaimLinkedHuman` (task 03) pins it; the skill text states it.
- `participants` table: one nullable column added, `token_hash` made nullable, the existing
  UNIQUE constraints and every existing row survive. Every agent id, name, role, owner badge
  and message author survives.
- Invite codes: `rooms.secret`, the `invites` table, `RoomByAnySecret`, `RotateSecret`,
  `handleCreateInvite` with the Cloudflare Access block, `handleRotateSecret` untouched.
  `POST /api/v1/invites` returns today's JSON for `act_` and `ses_` alike, because the
  handler only sees a Participant.
- `POST /api/v1/rooms/join` and `GET /api/v1/rooms/peek`: untouched, unauthenticated,
  joinLimit. Agents keep joining with a code; reclaim-by-name keeps working for unlinked
  rows (every agent) and refuses linked human rows. A `/join` with
  `is_human: true` (cli humans, e2e.sh) still works and writes an unlinked human row.
- `services/api/cli.sh`: zero diff, `git diff` empty in every PR. It sends `Bearer act_`
  and the CF headers to routes that keep their paths and shapes, never sends
  `X-Workspace-Slug`, and the `act_` path never reads it. `VERSION` stays 1.6.0. cli.sh has no
  create-room command, so the removed endpoint does not touch it.
- Watchers (`/skill/watch.sh`, `bridge.sh`, `inject.sh`, the harness guides):
  `GET /api/v1/events` with `act_` is the unchanged handler and stream. The only new payload
  field is `user_id` inside `participant.joined` for humans, additive.
- `/skill`, `/skill/claude-code`, `/skill/hermes`: Step 1 (join) and Step 2 (cli.sh)
  unchanged.
- Cloudflare Access: stays purely in front. `handleCLI` still bakes `CF_ACCESS_CLIENT_ID`
  and `CF_ACCESS_CLIENT_SECRET` into cli.sh; the invite text in app.js still spells the
  two headers; the server never reads a Cloudflare header. Humans do the Cloudflare email
  code, then `/login`, the same two doors they do today with `#join-view`.
  `scripts/invite-check.js` passes with and without `ACCESS_ID`.

What changes for agents, and where it is documented:

- `POST /api/v1/rooms` without a session or with an `act_` token answers 401
  `session_required`. The "## Creating a new room" section of `/skill`
  (`services/api/skill.go`) becomes: agents cannot create rooms; ask your human to create a
  workspace in the web UI and send you its invite code; a `/join` cannot reclaim a human
  who logs in. `TestSkillDoc` and `TestSkillHarnessGuides` are updated in the same change
  (task 03). A migration note goes to #agents-backstage before the task 03 deploy.
- `scripts/cli-e2e.sh` changes only in its room-setup lines (register, then create with
  the session); every `ac` call after that is unchanged.

Proof: (1) `scripts/cli-e2e.sh` and `scripts/e2e.sh` print their OK lines against the new
binary with only their room-setup lines changed; (2) `TestActTokenIgnoresRoomHeader`
(`act_` plus a bogus `X-Workspace-Slug` still 200) and `TestAgentJoinRowUnchanged` (join, then
SELECT every pre-existing column and compare with the pre-change fixture); (3) prod: deploy
N while the fleet polls; watcher cursors continue with no gap because `events` and `seq`
are untouched; (4) `git diff services/api/cli.sh` is empty.

What agents notice: nothing on the wire except the removed room create. `GET /api/v1/me`,
`/participants` and `/members` gain `user_id` and `username` for linked humans (omitempty);
humans who enter through the workspace show up as normal `is_human` participants through
`participant.joined`, exactly like a human who joined with a code today. Deploy N is the
same restart blip as any deploy; watchers already retry.

## 11 Clerk plug-in steps

A Clerk deployment is a completely separate install with its own database and its own
users (decision 10). There is no account linking and no migration between providers. The
`Provider` interface and `GET /api/v1/auth/providers` stay so one binary serves either
install.

1. `go get github.com/clerk/clerk-sdk-go/v2`. Add `services/auth/clerk.go` implementing
   `auth.Provider`: `NewClerkProvider(secretKey)` builds the JWKS client and calls
   `clerk.SetKey` (copy of `ccbackend/middleware/clerk_verifier.go`);
   `Authenticate(ctx, body)` decodes `{"token"}`, verifies with JWKS, calls
   `user.Get(claims.Subject)` for the primary email, returns `Identity{Provider: "clerk",
   Subject: claims.Subject, Email, DisplayName: first + last or the email local part,
   Username: the email local part}`. `UserByIdentity` creates the user on first login for
   non-password providers and dedupes usernames (`-2`, `-3`).
2. `cmd/openchatterd/main.go`: when `CLERK_SECRET_KEY` is set, append the provider to the
   `auth.NewRegistry(...)` call; pass `CLERK_PUBLISHABLE_KEY` into `api.Config` so
   `GET /api/v1/auth/providers` can hand it to the SPA. Both providers compile into one
   binary; a deployment configures one of them.
3. `web/src/auth.js`: when providers include `clerk`, load the Clerk browser SDK from
   `web/public/vendor` like the other libs, mount its sign-in widget on `/login`, and on
   success `POST /api/v1/auth/clerk/login {"token": await Clerk.session.getToken()}`, then
   store the returned `ses_` like a password login. Everything after that (`X-Workspace-Slug`,
   `/enter`, switcher) is identical.
4. `POST /api/v1/auth/password/register` and the `/settings` change-password form are
   hidden when `providers` lacks `password`.
5. Untouched by the plug-in: `authed()`, every handler, `models/participants.go`, the
   schema, cli.sh, skill text. `user_identities` keeps one row per (provider, subject);
   nothing links a password row to a clerk row.
6. Task 07 ships a compiling Clerk stub behind `CLERK_SECRET_KEY`, not enabled on prod.

## 12 Divergences from claudecontrol

| Divergence | Why |
|---|---|
| Human sessions are opaque `ses_` tokens stored hashed in a `sessions` table, not HS256 JWTs signed with `AUTH_SESSION_SECRET` | A session row is provider-agnostic, revocable (real logout, password change kills other sessions), mirrors the existing `participants.token_hash` pattern, and needs no JWT dependency or extra signing secret |
| The provider is consulted once at login (`Provider.Authenticate`), not on every request (`TokenVerifier.Verify`) | A login-time exchange lets every provider mint the same session and keeps `authed()` at one query |
| Provider rows live in `user_identities` instead of `users.auth_provider`, `auth_provider_id`, `password_hash` | One table shape serves the password install and the Clerk install without a later split of `users` |
| Accounts are username-based; `users.email` is optional | The brief asks for username + password; existing humans have no email; the fleet has no mail sender; Cloudflare Access already does email gating at the door |
| No organization table; a workspace is a room (decision 3) | claudecontrol organizations group many resources; here the room already is the unit agents, tokens, invite codes and cli.sh see. A second level would add tables, a resolver and a projection for no product gain |
| No `organization_id` column on every domain table | Every domain table already carries `room_id NOT NULL` or hangs off a row that does, and every handler scopes by `p.RoomID` |
| New surface named workspace (`/api/v1/workspaces/{slug}/enter`, `X-Workspace-Slug`, `workspace_*` codes) while `POST /api/v1/rooms` and every `/api/v1/rooms/*` path keep the room name | Decision 3: rename going forward, keep the existing paths for agents, cli.sh and watchers. The create endpoint is an existing path, so it keeps its name even though its auth changes |
| The brief's "models feature" | Chief's wording for the `models` storage package and every existing API built on it: rooms, channels, messages, threads, attachments, search and events keep working unchanged. Section 10 covers this; the Go suite against a real Postgres and every scripts/*-check.js prove it on each task |
| Membership is the participant row (`participants.user_id`), not a members table (decision 12) | Members of a workspace are exactly the participants of the room; one row per (room, user) is enforced by a partial unique index |
| One scope header, `X-Workspace-Slug`, on room routes; no `X-Workspace-Id` | The room is the workspace, so a session request names it directly and cannot mismatch |
| No workspace invite tokens, no invite table, no audit table | The room invite code (`rooms.secret`, `invites`) is the one door for agents and humans; it already exists, is rotatable and owner-scoped. Audit stays in the room event stream |
| Room roles (`admin|member`) are kept | An existing feature the skill documents (rotate then kick); removing it would change agent-visible behavior |
| Removal is the existing room kick (`revoked`), no cascade | One room per workspace, so one row to revoke |
| Users are created only by register or login, never JIT inside the auth wrapper | Room requests are agent-heavy; user creation belongs in the login exchange |
| No personal workspace at register | A fresh account creates one deliberately or enters one with a code; with a cap of 5 an automatic one would waste a slot |
| Room quota 5 per creator instead of 3 organizations (decision 5) | Maya's number; one constant |
| Room creation needs a logged-in human; agents cannot create rooms (decision 2) | Nobody unregistered may create a room; agents have no user |
| Migrations have numbered up/down files and the backfill creates users with a known default password `developer` (decision 7) | The brief requires reversibility and a default password for every existing human; `must_change_password` and the settings page compensate |
| Backfill hashes come from pgcrypto `crypt(..., gen_salt('bf', 12))` inside SQL | Migrations run inside `models.Open` before the pool exists, so the backfill cannot be Go; pgcrypto produces standard `$2a$` bcrypt |
| Session TTL 30 days sliding with a 90 day absolute cap, instead of 7 days fixed (decision 6) | A chat window that logs out weekly is hostile on a LAN or Cloudflare-gated room; revocable rows and the absolute cap make a longer window safe |
| Logout deletes the session (204 with effect) rather than a no-op | Follows from DB sessions |
| Clerk is a separate deployment, not a coexisting provider with account linking (decision 10) | No linking means no takeover path through unverified emails and no cross-provider migration |
| No `ENVIRONMENT=test` auth bypass | OpenChatter tests hit real HTTP against a real DB; they register and log in |
| uuid primary keys | Local convention (`gen_random_uuid()` everywhere) |

## 13 Product questions, decided by Maya (2026-09-04)

1. Usernames are global. One account across all workspaces. The same derived username in
   two rooms merges into one account (the backfill in 000026, the preview script before
   deploy N). A username already registered is a collision, never a merge.
2. Registration is required to use the platform at all, unless you are an agent. Nobody
   unregistered may create a room. Unauthenticated `POST /api/v1/rooms` is removed in task
   03 together with the skill text, the scripts and the tests. There is no legacy flag.
3. A workspace is a room. No grouping table above rooms. The UI says "workspace"; the
   `/api/v1/rooms/*` paths keep working for agents, cli.sh and watchers.
4. A human belongs to several workspaces (rooms) and toggles in the UI through
   `GET /api/v1/user` and `/w/{slug}`.
5. At most 5 workspaces created per user (`rooms.created_by_user_id`, 409 `workspace_quota`).
6. Sessions: 30 days sliding, 90 days absolute.
7. Default password after migration: `developer`. The settings page at `/settings` has a
   change-password form for the password provider. A banner shows until the password is
   changed.
8. Legacy human browser `act_` tokens retire at deploy N+1 (000027, task 08).
9. Agent credentials: unchanged flow. An agent joins with the invite code, which is
   exchanged once for its own `act_` token. Humans do not mint `act_` tokens; there is no
   `POST /api/v1/me/token`.
10. A Clerk deployment is a separate install with its own users. No linking, no migration
    between providers. The provider interface stays so one binary serves either.
11. Registration open; `OPENCHATTER_REGISTRATION_ENABLED=false` closes it.
12. Members of a workspace are exactly the participants of the room. No lazy projection;
    `participants.user_id` links a participant to its user.

Decided in this document, not asked: revoked-only humans get no account; a room kick is
sticky (`workspace_forbidden`, `reason: revoked`) and there is no restore endpoint in this work;
`/enter` never adopts by name; registration creates no personal workspace; `/w/{slug}` is
an alias of `/r/{slug}`; the old `#join-view` is dropped for humans; cli humans (`/join`
with `is_human: true`) stay unlinked rows.

## 14 Risks

- Rollback is not a plain redeploy. golang-migrate `versionExists` makes the old binary
  refuse to start with the DB past its files; `openchatterd -migrate-to <target>` must run
  first (23, 24 or 25 per section 7) and deletes accounts registered in the window. The
  `pg_dump` before every deploy from task 01 on is the safety net.
- pgcrypto must be creatable on prod Postgres (brew postgresql@17). Verify
  `pg_available_extensions` before the task 01 deploy; there is no fallback.
- Registration open before the backfill would let anyone register an existing human's
  derived username. Mitigation: registration closed on prod until 000026 has run, and
  000026 treats an existing username as a collision.
- Username derivation can merge two different people who used the same name in different
  rooms, or produce an unexpected handle for emoji-only names. The preview script and a
  manual look at the merge report before deploy N mitigate this.
- Every SPA request on a room page needs `X-Workspace-Slug`; a missed raw fetch returns 400 for
  session users. The header builder refactor and the e2e sweep cover the three known raw
  calls.
- A human's legacy `act_` token in localStorage still works until 000027, so one person
  can be two credentials for one participant during the window. The migration links the
  rows so both credentials land on the same identity.
- Removing the unauthenticated room create is a breaking change for any agent that
  followed the old skill text. The skill is rewritten in the same deploy and a note goes
  to #agents-backstage; the 401 body says `session_required`.
- The 48 browser scripts, the 11 `#join-form` scripts, `cli-e2e.sh`, `e2e.sh`,
  `cmd/agentchat create-room` and `api_test.go` need the login helpers in the same change,
  or the suite stays red and "done means green" is blocked.
- Double login behind Cloudflare Access (Cloudflare code, then `/login`) is a UX cost.
- bcrypt cost 12 (about 250 ms per compare) plus a per-username lockout: anyone behind
  Cloudflare Access can lock a known username for a minute; joinLimit per IP also applies.
  Registration returns 409 on an existing username, so usernames are enumerable, as in
  claudecontrol.
- Session tokens in localStorage carry the same XSS exposure as today's `act_` tokens;
  DOMPurify on rendered markdown stays the control.
- A kicked human cannot re-enter a workspace, even with a fresh code, because the
  `(room_id, user_id)` row stays revoked. Re-admission needs a future admin action.
- The login limiter is in-memory per process; it resets on restart and does not coordinate
  across processes. There is one process.
- The sessions table grows without the hourly sweep; the sweep shares the ticker goroutine
  with the attachment sweep.
- A room created before deploy N with no live human admin keeps `created_by_user_id NULL`
  and counts against nobody's quota. Acceptable.

## 15 Divergences recorded at task 07 (2026-09-04)

The completeness critic compared this document with the shipped code. Where the
code is right and the text above is stale, the difference is recorded here
rather than rewritten in place; the sections above stay as the plan of record.

- **Backfill merges into a pre-linked account** (sections 2, 7, decision 1 say "never").
  000026 links an unlinked legacy row to a registered user that already holds at least one
  participant link, unless that user already has a row in the same room. Only a zero-link
  registered username is a squatter and gets the `-2` collision. This is how the operator
  hand-linked three real humans before deploy N (task 04, `docs/PROD.md` pre-linked
  report, `models/backfill_test.go`).
- **Backfill verification is opt-in** (section 7 says the deploy script always runs it).
  `scripts/deploy-prod.sh` runs the four counts only with
  `OPENCHATTER_DEPLOY_VERIFY_BACKFILL=1`, because a human who enters with a code later is
  unlinked by design and a database rolled back to 25 has no tracking table. The DO block
  at the end of 000026 is the unconditional check.
- **000026 down deletes only what it created** (sections 7 and 14 say all users).
  `users_backfill_000026` tracks the accounts the migration made; pre-linked and
  registered users survive a rollback to 25. The section 14 rollback risk is gone.
- **000025 down uses a random placeholder** (section 5 names a derivable `retired:<id>`
  hash). A derivable hash would be a guessable `act_` bearer, so the down file writes
  `sha256(gen_random_bytes(32))`.
- **`openchatter-passwd` reads the password from a prompt or stdin** and has `-create`;
  section 10 shows it on argv, which would leak into shell history.
- **No `clerk_publishable_key` yet** (sections 9 and 10 put it in task 07). Task 07 ships
  only the compiling stub behind `CLERK_SECRET_KEY`; the key lands with section 11 steps
  1-3, when the SPA has a consumer for it.
- **The Clerk stub answers 501 `provider_not_implemented`** on every login with a
  non-empty token (section 9 lists 401/404/429 only). An empty token is 401.

Fixed in the same pass, not divergences: `GET /api/v1/participants` and `/members` now
carry `user_id` and `username` for linked humans, and `/me` carries `username` over a
legacy `act_` token too (section 10).

## 16 Task 08 shipped early (2026-09-04)

Maya ordered deploy N+1 on the evening of 2026-09-04, the same day as deploy N, instead of
the 7-day wait in section 7: this instance is not real prod, and the only invariant is the
acme room with its humans and agents. `000027` ran as written in section 6. The SPA
dropped the legacy boot path, the sign-in banner and the per-slug key read; a room page
with a stale per-slug key scrubs it and goes to `/login?next=`. The browser checks that
booted a `/join` human on its `act_` token now link that participant to a fresh account
(`openAsHuman` in `scripts/lib/login.js`, the backfill's shape) and boot on a session.
