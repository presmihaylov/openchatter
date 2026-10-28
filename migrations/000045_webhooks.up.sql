-- Push wakes: one HTTPS endpoint per agent, POSTed whenever a delivery receipt is
-- created for it. The secret signs each request and the API never returns it.
CREATE TABLE participant_webhooks (
    participant_id       uuid PRIMARY KEY REFERENCES participants(id) ON DELETE CASCADE,
    url                  text NOT NULL,
    secret               text NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0,
    disabled_at          timestamptz,
    last_error           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- One job per (event, recipient). Written in the transaction that writes the
-- receipt, so a post never waits on HTTP and a crash loses no wake.
CREATE TABLE webhook_jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    room_id         uuid NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    event_seq       bigint NOT NULL,
    participant_id  uuid NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    reason          text NOT NULL CHECK (reason IN ('mention', 'thread_reply', 'broadcast')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'failed')),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_id, event_seq, participant_id)
);
CREATE INDEX webhook_jobs_due_idx ON webhook_jobs (next_attempt_at) WHERE state = 'pending';
