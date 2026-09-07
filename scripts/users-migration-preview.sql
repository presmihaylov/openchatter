-- Read-only preview of migration 000026 (docs/workspaces-auth-design.md section 7).
-- Runs on schema 25, before the task 04 deploy:
--   psql "$OPENCHATTER_DB_URL" -f scripts/users-migration-preview.sql
-- Only temp objects are created; nothing persists past the psql session.
-- The derivation below is a verbatim copy of the human_rows CTE in
-- migrations/000026_backfill_users.up.sql; keep the two in sync.

\set ON_ERROR_STOP on
\pset footer off

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
    -- rank 2+ takes the (rn-1)th free suffix: not a registered username, not
    -- a plain name another row derives
    SELECT r.*, CASE WHEN r.rn = 1 THEN r.uname2 ELSE
        (SELECT left(r.uname2, 29) || '-' || k FROM generate_series(2, 99) k
         WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.username = left(r.uname2, 29) || '-' || k)
           AND NOT EXISTS (SELECT 1 FROM shaped s WHERE s.uname2 = left(r.uname2, 29) || '-' || k)
         ORDER BY k OFFSET r.rn - 2 LIMIT 1) END AS username0
    FROM ranked r
)
SELECT id AS participant_id, room_id, name, created_at, last_seen_at, revoked, role, username0,
       CASE
           WHEN NOT EXISTS (SELECT 1 FROM users u WHERE u.username = n.username0) THEN n.username0
           WHEN EXISTS (SELECT 1 FROM users u JOIN participants l ON l.user_id = u.id
                        WHERE u.username = n.username0)
                AND NOT EXISTS (SELECT 1 FROM users u JOIN participants l ON l.user_id = u.id
                                WHERE u.username = n.username0 AND l.room_id = n.room_id)
               THEN n.username0
           ELSE (SELECT left(n.username0, 28) || '-' || k FROM generate_series(2, 99) k
                 WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.username = left(n.username0, 28) || '-' || k)
                   AND NOT EXISTS (SELECT 1 FROM named n2 WHERE n2.username0 = left(n.username0, 28) || '-' || k)
                 ORDER BY k LIMIT 1)
       END AS username
FROM named n;

\echo '== merge report: one username, several participant rows =='
SELECT h.username, array_agg(DISTINCT h.name) AS names, array_agg(DISTINCT r.slug) AS rooms, count(*)
FROM human_rows h JOIN rooms r ON r.id = h.room_id
GROUP BY h.username HAVING count(*) > 1
ORDER BY h.username;

\echo '== same-room clash report: two rows in one room end with the same username (the migration would abort; rename one first) =='
SELECT r.slug AS room, h.username, array_agg(h.name) AS names
FROM human_rows h JOIN rooms r ON r.id = h.room_id
GROUP BY r.slug, h.username HAVING count(*) > 1
ORDER BY r.slug, h.username;

\echo '== collision report: derived username already registered with zero links (legacy row gets a suffix) =='
SELECT h.username0 AS derived, h.username AS becomes, h.name, r.slug AS room, h.revoked
FROM human_rows h JOIN rooms r ON r.id = h.room_id
JOIN users u ON u.username = h.username0
WHERE NOT EXISTS (SELECT 1 FROM participants l WHERE l.user_id = u.id)
ORDER BY h.username0, r.slug;

\echo '== pre-linked report: users that already hold participant links (merge targets) =='
SELECT u.username, array_agg(DISTINCT r.slug) AS rooms, count(DISTINCT l.id) AS links,
       array_agg(DISTINCT h.name) FILTER (WHERE h.participant_id IS NOT NULL) AS will_merge
FROM users u JOIN participants l ON l.user_id = u.id JOIN rooms r ON r.id = l.room_id
LEFT JOIN human_rows h ON h.username = u.username
GROUP BY u.username ORDER BY u.username;

\echo '== usernames the migration will create (password "developer", must change) =='
SELECT h.username, min(h.name) AS display_name, count(*) AS rows, array_agg(DISTINCT r.slug) AS rooms
FROM human_rows h JOIN rooms r ON r.id = h.room_id
WHERE NOT h.revoked
  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.username = h.username)
GROUP BY h.username ORDER BY h.username;

\echo '== humans with only revoked rows: no account, stay unlinked =='
SELECT h.username, array_agg(DISTINCT h.name) AS names, array_agg(DISTINCT r.slug) AS rooms
FROM human_rows h JOIN rooms r ON r.id = h.room_id
WHERE NOT EXISTS (SELECT 1 FROM human_rows l WHERE l.username = h.username AND NOT l.revoked)
  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.username = h.username)
GROUP BY h.username ORDER BY h.username;

\echo '== verification counts, current state (all four must be 0 after the deploy) =='
SELECT
  (SELECT count(*) FROM participants WHERE is_human AND NOT revoked AND user_id IS NULL) AS unlinked_live_humans,
  (SELECT count(*) FROM users u LEFT JOIN user_identities i ON i.user_id = u.id WHERE i.user_id IS NULL) AS users_without_identity,
  (SELECT count(*) FROM (SELECT username FROM users GROUP BY username HAVING count(*) > 1) d) AS duplicate_usernames,
  (SELECT count(*) FROM participants p WHERE p.user_id IS NOT NULL
     AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = p.user_id)) AS links_to_missing_user;

DROP TABLE human_rows;
