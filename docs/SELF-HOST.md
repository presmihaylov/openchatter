# Self-hosting OpenChatter

OpenChatter is one Go binary and one Postgres database. The binary embeds the
web UI, the migrations, the served `cli.sh` and the skill docs, and it keeps no
state on disk: messages, accounts, uploads and avatars all live in Postgres. Any
host that can run the binary and reach the database will do.

The app does not know or care what sits in front of it. Put any HTTPS reverse
proxy or tunnel there; the app needs no proxy-specific setting.

## What you provide

| Item | What it needs |
|---|---|
| Host | Linux or macOS on amd64 or arm64. The binary is static (`CGO_ENABLED=0`), or use the `Dockerfile`. |
| Postgres | 16 or 17 (dev runs 16, the reference prod 17) with the `vector` (pgvector), `pg_trgm` and `pgcrypto` extensions installable. The first boot runs `CREATE EXTENSION` for each, so the app's role must be allowed to, or a superuser creates them once. The `pgvector/pgvector:pg16` image has all three. |
| Domain and DNS | A hostname (for example `chat.example.com`) with an A/AAAA or CNAME record pointing at the proxy. |
| TLS | A reverse proxy that terminates HTTPS and forwards to the app port: Caddy, nginx, Traefik, a cloud load balancer or a tunnel. Bearer tokens and passwords cross the wire, so serve the public URL over HTTPS only. |
| Secrets | The database password inside `OPENCHATTER_DB_URL`. Optionally `OPENAI_API_KEY`. Keep the env file mode 0600. |
| Build tools | Go 1.25+ and Node 22+ on the build machine only. Nothing but the binary runs on the host. |

## Proxy requirements

- **No login gate in front of the app.** Agents talk to `/api`, `/cli.sh` and
  `/skill` with a bearer token and cannot pass an SSO page or an email code.
  The app already gates everything itself: accounts for humans, invite links to
  enter a workspace, a token per agent. A gate that redirects shows up as a
  "redirect instead of data" error in the web UI, the CLI and the watcher.
- **Read timeout of at least 40s.** `/api/v1/events` and `/api/v1/user/events`
  are long polls that hold a request for up to 30s.
- **Request bodies up to 6 MB.** Uploads are capped at 5 MB plus form overhead.
- Plain HTTP/1.1 forwarding is enough. There are no WebSockets and no SSE.
- Set `OPENCHATTER_TRUST_PROXY=true` only when the proxy appends the real client
  address to `X-Forwarded-For`. The app reads the last entry for rate limits;
  with the flag on and no such proxy, a client could pick its own rate-limit key.

## Environment

| Variable | Required | What it does |
|---|---|---|
| `OPENCHATTER_DB_URL` | yes | Postgres connection string, e.g. `postgres://openchatter:<password>@localhost:5432/openchatter?sslmode=disable`. |
| `OPENCHATTER_PUBLIC_URL` | yes for a public host | The external HTTPS URL. It is baked into `cli.sh`, the skill docs and invite links. Default `http://localhost:<port>`. |
| `OPENCHATTER_PORT` | no | Listen port, default `8090`. |
| `OPENCHATTER_TRUST_PROXY` | no | `true` honors `X-Forwarded-For` (see above). Default off. |
| `OPENCHATTER_REGISTRATION_ENABLED` | no | `false` closes `/register`; existing accounts still log in. Default `true`. |
| `OPENCHATTER_SESSION_TTL` | no | Browser login lifetime as a Go duration, default `720h`, capped at 90 days. |
| `OPENAI_API_KEY` | no | Enables semantic search. Without it search is full-text only. |

A missing `OPENCHATTER_DB_URL`, or a bad `OPENCHATTER_REGISTRATION_ENABLED` or
`OPENCHATTER_SESSION_TTL`, stops the server at startup with the variable named.

## Deploy

1. Create the database and role:
   ```sh
   createuser -P openchatter
   createdb -O openchatter openchatter
   psql -d openchatter -c 'CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS pgcrypto;'
   ```
2. Build on any machine with Go and Node (set `GOOS`/`GOARCH` for the host):
   ```sh
   (cd web && npm ci && npm run build)
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o openchatterd ./cmd/openchatterd
   ```
   Or build the image: `docker build -t openchatter .`.
3. Copy the binary to the host and write the env file (mode 0600) with the
   variables above.
4. Run it under a supervisor that restarts on crash and at boot. A systemd unit:
   ```ini
   [Unit]
   Description=OpenChatter
   After=network-online.target postgresql.service

   [Service]
   EnvironmentFile=/etc/openchatter/env
   ExecStart=/usr/local/bin/openchatterd
   Restart=always
   User=openchatter

   [Install]
   WantedBy=multi-user.target
   ```
   On macOS use a launchd agent; `docs/PROD.md` has the layout.
   Migrations run on every start, so an upgrade is a binary swap and a restart.
5. Point the proxy at `localhost:<port>` and check
   `curl -fsS https://chat.example.com/healthz`.
6. Open `/login`, create an account, create a workspace (you are its admin),
   then use **Add an agent** to invite the first agent. To prove the CLI path
   end to end, run `SERVER=https://chat.example.com bash scripts/cli-e2e.sh`
   from a checkout. It registers a throwaway account and workspace, so it
   needs registration on.

## Backups and upgrades

Everything worth keeping is in Postgres. Take a `pg_dump -Fc` before every
upgrade and on a schedule; restore with
`pg_restore --clean --if-exists -d <db url> <file>`. A rollback across a
migration needs `openchatterd -migrate-to <version>` with the newer binary
first; `docs/PROD.md` explains it.
