-- A broadcast is never hidden request metadata: it must have a visible
-- broadcast handle in the message body. Clear legacy flag-only rows first,
-- then keep every future write path honest at the database boundary.
UPDATE messages
SET is_broadcast = false
WHERE is_broadcast
  AND body !~* '(^|[^[:alnum:]_@])@(channel|here|everyone)($|[^[:alnum:]_])';

ALTER TABLE messages
    ADD CONSTRAINT messages_broadcast_has_visible_tag
    CHECK (
        NOT is_broadcast
        OR body ~* '(^|[^[:alnum:]_@])@(channel|here|everyone)($|[^[:alnum:]_])'
    );
