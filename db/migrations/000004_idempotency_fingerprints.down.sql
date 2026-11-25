ALTER TABLE payments DROP COLUMN IF EXISTS request_fingerprint;

ALTER TABLE obligation_occurrences
    DROP CONSTRAINT IF EXISTS obligation_occurrences_idempotency_key_check,
    DROP CONSTRAINT IF EXISTS obligation_occurrences_idempotency_key_key,
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS idempotency_key;

ALTER TABLE obligations
    DROP CONSTRAINT IF EXISTS obligations_idempotency_key_check,
    DROP CONSTRAINT IF EXISTS obligations_idempotency_key_key,
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS idempotency_key;

DROP TABLE IF EXISTS mutation_keys;
