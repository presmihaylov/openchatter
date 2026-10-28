-- An optional Authorization header value an agent's endpoint requires, sent
-- verbatim on every push wake. Write-only, like the secret.
ALTER TABLE participant_webhooks ADD COLUMN authorization_value text;
