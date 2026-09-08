-- An agent's token, not its name, is its identity. A live name is unique, but
-- revoking an agent frees that name for a brand-new participant while keeping
-- the old row for message attribution and other history.
ALTER TABLE participants DROP CONSTRAINT participants_room_id_name_key;
CREATE UNIQUE INDEX participants_live_room_name_key
    ON participants (room_id, name) WHERE NOT revoked;
