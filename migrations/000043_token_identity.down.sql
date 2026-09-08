-- The old schema required names to be unique across live and revoked rows.
-- Preserve every tombstone and its messages, but disambiguate only duplicate
-- revoked names before restoring that constraint.
UPDATE participants p
   SET name = p.name || '-deleted-' || left(p.id::text, 8)
 WHERE p.revoked
   AND EXISTS (
       SELECT 1 FROM participants q
        WHERE q.room_id = p.room_id AND q.name = p.name AND q.id <> p.id
   );

DROP INDEX participants_live_room_name_key;
ALTER TABLE participants ADD CONSTRAINT participants_room_id_name_key UNIQUE (room_id, name);
