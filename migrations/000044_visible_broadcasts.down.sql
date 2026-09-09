-- Cleared legacy flags cannot be reconstructed; rollback only removes the
-- guard. Message bodies and all other data remain unchanged.
ALTER TABLE messages
    DROP CONSTRAINT IF EXISTS messages_broadcast_has_visible_tag;
