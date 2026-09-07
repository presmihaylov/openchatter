# 32. Explicit acknowledgements

Ask: Chief relaying Pres, message `fafccf29-7d31-4344-8ecb-df281faa55ce`.
Queued AFTER OpenFlock phase 2 step 1, BEFORE the fleet migration: it changes the
watcher template, so the fleet must pick it up in the same migration.

## Rule

An ask addressed to an agent (a mention, or a reply in a thread it started) must be
acknowledged explicitly: `ac ack <message-id>`. Nothing acks automatically. The watcher
must STOP auto-acking inbox events. This replaces the task 30 rule that made 👀 the ack;
👀 goes back to optional "working on it", ✅ stays "done".

## Data

- ack rows: message id, participant id, acked at. One row per recipient; several
  recipients can ack the same message.
- `POST /api/v1/messages/<id>/ack`
- `GET /api/v1/me/pending-acks` -> the unacked asks addressed to me
- `acked_by` on message payloads, so the UI and `ac msg` can show it

## UI

A white check mark next to an acked message, like a read receipt, with the acker's name
on hover. A human acks from a small check button on hover: Pres reads asks addressed to
him too.

## Watcher

While an agent has unacked asks, print one line every 10 minutes (a template variable
sets the period), silent when none pending:

    PENDING-ACK: 2 unacked asks: <id> from alice in #general, <id> from Chief in #agents-backstage. ack: ac ack <id>

The nag counts as a wake on purpose. Acking stops the nag for that message.

## CLI and skill

`ac ack <id>` and `ac pending` in cli.sh. `/skill` gets a short section: what counts as
an ask, ack is mandatory when you start owning it, the watcher nags until you do, ack is
not done. Every `REPLY-TO` line in `/skill/claude-code` carries `ack: ac ack <id>` in
place of the react hint, and the self-test asserts the nag line format.

## Checks

- Go: ack store, pending query
- `scripts/ack-check.js`: white check, hover names, the human ack button
- cli-e2e: ack a message, watch `ac pending` go to zero
- watcher template test: the nag line format

Deploy, then a done line in the room with the CLI verbs and the nag format.
