CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE obligations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (length(trim(name)) > 0),
    currency char(3) NOT NULL DEFAULT 'VND' CHECK (currency = 'VND'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE obligation_occurrences (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    obligation_id uuid NOT NULL REFERENCES obligations(id),
    due_date date NOT NULL,
    confirmed_amount_vnd bigint NOT NULL CHECK (confirmed_amount_vnd > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX obligation_occurrences_obligation_due_idx
    ON obligation_occurrences (obligation_id, due_date);

CREATE TABLE payments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    occurrence_id uuid NOT NULL REFERENCES obligation_occurrences(id),
    amount_vnd bigint NOT NULL CHECK (amount_vnd > 0),
    paid_at timestamptz NOT NULL,
    idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    reversed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payments_occurrence_active_idx
    ON payments (occurrence_id) WHERE reversed_at IS NULL;
