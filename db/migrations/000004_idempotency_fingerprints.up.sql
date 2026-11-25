-- A retried mutation key must replay the original request, so every keyed table
-- stores an immutable fingerprint of that request. Legacy rows get a fingerprint
-- derived from what they already hold, which keeps them replayable.

ALTER TABLE obligations
    ADD COLUMN idempotency_key text,
    ADD COLUMN request_fingerprint text;

UPDATE obligations
SET idempotency_key = 'legacy-obligation-' || id::text,
    request_fingerprint = encode(digest('obligation' || chr(31) || name, 'sha256'), 'hex');

ALTER TABLE obligations
    ALTER COLUMN idempotency_key SET NOT NULL,
    ALTER COLUMN request_fingerprint SET NOT NULL,
    ADD CONSTRAINT obligations_idempotency_key_key UNIQUE (idempotency_key),
    ADD CONSTRAINT obligations_idempotency_key_check
        CHECK (length(idempotency_key) BETWEEN 1 AND 255);

ALTER TABLE obligation_occurrences
    ADD COLUMN idempotency_key text,
    ADD COLUMN request_fingerprint text;

UPDATE obligation_occurrences
SET idempotency_key = 'legacy-occurrence-' || id::text,
    request_fingerprint = encode(digest(
        'occurrence' || chr(31) || lower(obligation_id::text) || chr(31) ||
        to_char(due_date::timestamp, 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') || chr(31) ||
        confirmed_amount_vnd::text,
        'sha256'), 'hex');

ALTER TABLE obligation_occurrences
    ALTER COLUMN idempotency_key SET NOT NULL,
    ALTER COLUMN request_fingerprint SET NOT NULL,
    ADD CONSTRAINT obligation_occurrences_idempotency_key_key UNIQUE (idempotency_key),
    ADD CONSTRAINT obligation_occurrences_idempotency_key_check
        CHECK (length(idempotency_key) BETWEEN 1 AND 255);

ALTER TABLE payments
    ADD COLUMN request_fingerprint text;

UPDATE payments
SET request_fingerprint = encode(digest(
    lower(occurrence_id::text) || chr(31) || amount_vnd::text || chr(31) ||
    to_char(paid_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'sha256'), 'hex');

ALTER TABLE payments
    ALTER COLUMN request_fingerprint SET NOT NULL;

-- Updates cannot carry a natural unique key, so a retried key is claimed here
-- before the mutation runs; the same key with another payload is refused.
CREATE TABLE mutation_keys (
    idempotency_key text PRIMARY KEY CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
