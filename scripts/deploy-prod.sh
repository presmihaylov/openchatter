#!/usr/bin/env bash
# Deploy OpenChatter to prod. See docs/PROD.md.
# Usage: scripts/deploy-prod.sh [commit]   (defaults to HEAD)
# The ssh host and the health URL come from scripts/deploy-prod.env (gitignored):
#   OPENCHATTER_PROD_HOST=<ssh host>
#   OPENCHATTER_PROD_HEALTH_URL=http://<prod host>:<port>/healthz
#   VITE_FLEET_SLUG=<slug of the workspace whose delete asks twice>   (optional)
set -euo pipefail

ENV_FILE="$(dirname "$0")/deploy-prod.env"
[ -f "$ENV_FILE" ] && source "$ENV_FILE"
HOST="${OPENCHATTER_PROD_HOST:?set OPENCHATTER_PROD_HOST in scripts/deploy-prod.env}"
HEALTH_URL="${OPENCHATTER_PROD_HEALTH_URL:?set OPENCHATTER_PROD_HEALTH_URL in scripts/deploy-prod.env}"
COMMIT=$(git rev-parse --short "${1:-HEAD}")
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# build from a clean checkout so uncommitted local changes never reach prod
git archive "$COMMIT" | tar -x -C "$WORK"
echo "building openchatterd @ $COMMIT..."
(cd "$WORK/web" && npm ci --silent && VITE_FLEET_SLUG="${VITE_FLEET_SLUG:-}" npm run build --silent)
(cd "$WORK" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "openchatterd-$COMMIT" ./cmd/openchatterd)

# dump the prod db before anything moves, so a bad migration has a restore
# point; a failed dump aborts the deploy instead of deploying blind
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
DUMP="openchatter-prod/backups/openchatter-$STAMP-pre-$COMMIT.dump"
echo "dumping prod db to $HOST:$DUMP..."
ssh "$HOST" "set -euo pipefail
  set -a && source ~/openchatter-prod/env && set +a
  mkdir -p ~/openchatter-prod/backups
  /opt/homebrew/opt/postgresql@17/bin/pg_dump -Fc --dbname=\"\$OPENCHATTER_DB_URL\" --file=\"$DUMP\"
  ls -t ~/openchatter-prod/backups/*.dump | tail -n +11 | xargs rm -f" || {
  echo "DEPLOY ABORTED: pg_dump on $HOST failed, nothing was changed" >&2
  exit 1
}

scp "$WORK/openchatterd-$COMMIT" "$HOST:openchatter-prod/bin/"
ssh "$HOST" "ln -sf openchatterd-$COMMIT ~/openchatter-prod/bin/openchatterd \
  && launchctl kickstart -k gui/\$(id -u)/com.openchatter.prod"

sleep 3
curl -sf --max-time 5 "$HEALTH_URL"
echo " deployed $COMMIT"

# post-migrate verification (000026 user backfill, docs/PROD.md): the four
# counts must all be 0. Opt-in, for the one deploy that carries the backfill:
# humans who join with an invite code later are unlinked by design, and a
# database rolled back to 25 has no users_backfill_000026 table.
if [ "${OPENCHATTER_DEPLOY_VERIFY_BACKFILL:-0}" != "1" ]; then
  exit 0
fi
echo "verifying user backfill on $HOST..."
VERIFY_SQL="SELECT 'unlinked_live_humans', count(*) FROM participants WHERE is_human AND NOT revoked AND user_id IS NULL
UNION ALL SELECT 'backfilled_users_without_identity', count(*) FROM users_backfill_000026 b LEFT JOIN user_identities i ON i.user_id = b.user_id WHERE i.user_id IS NULL
UNION ALL SELECT 'duplicate_usernames', count(*) FROM (SELECT username FROM users GROUP BY username HAVING count(*) > 1) d
UNION ALL SELECT 'links_to_missing_user', count(*) FROM participants p WHERE p.user_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = p.user_id)"
COUNTS=$(ssh "$HOST" "set -euo pipefail
  set -a && source ~/openchatter-prod/env && set +a
  /opt/homebrew/opt/postgresql@17/bin/psql \"\$OPENCHATTER_DB_URL\" -At -F ' ' -c \"$VERIFY_SQL\"") || {
  echo "VERIFY FAILED: could not run the verification queries on $HOST" >&2
  exit 1
}
echo "$COUNTS"
if [ "$(echo "$COUNTS" | wc -l | tr -d ' ')" != "4" ] || echo "$COUNTS" | awk '$2 != 0 { bad = 1 } END { exit !bad }'; then
  echo "VERIFY FAILED: a backfill count is not 0; see docs/PROD.md (rollback target 25)" >&2
  exit 1
fi
echo "verification passed: all four counts are 0"
