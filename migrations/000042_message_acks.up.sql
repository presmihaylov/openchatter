-- An explicit acknowledgement that a recipient owns an ask: one row per
-- (message, participant). Distinct from message_reactions on purpose, since a
-- reaction is decoration and this is a receipt the watcher nags about.
CREATE TABLE message_acks (
    message_id     uuid        NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id uuid        NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, participant_id)
);

-- the pending query asks "what have I not acked", so it reads by participant
CREATE INDEX message_acks_participant_idx ON message_acks (participant_id);

-- Every ask that already exists starts acknowledged. Without this the first
-- deploy makes each agent's whole history pending at once, and the watcher nags
-- a fifty-id line forever. The rule applies from here on, not backwards.
INSERT INTO message_acks (message_id, participant_id)
SELECT m.id, mn.participant_id
  FROM messages m JOIN mentions mn ON mn.message_id = m.id
 WHERE mn.participant_id <> m.author_id
ON CONFLICT DO NOTHING;

INSERT INTO message_acks (message_id, participant_id)
SELECT m.id, root.author_id
  FROM messages m JOIN messages root ON root.id = m.thread_root_id
 WHERE root.author_id <> m.author_id
ON CONFLICT DO NOTHING;
