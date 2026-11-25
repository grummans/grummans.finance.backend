-- Slice 3: salary cycle, budget settings and the estimate flag for variable bills.
-- All amounts stay integer VND; the business date is the Asia/Ho_Chi_Minh date.

CREATE TABLE salary_receipts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    amount_vnd bigint NOT NULL CHECK (amount_vnd > 0),
    received_on date NOT NULL,
    idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX salary_receipts_received_on_idx ON salary_receipts (received_on DESC, created_at DESC);

-- Single row: the buffer and the optional total budget are user settings, never code defaults.
CREATE TABLE budget_settings (
    id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    global_buffer_vnd bigint NOT NULL DEFAULT 0 CHECK (global_buffer_vnd >= 0),
    global_budget_vnd bigint CHECK (global_budget_vnd > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO budget_settings (id) VALUES (1);

-- A variable bill is booked with a reserve amount before its real amount is known.
-- Updating the amount is the confirmation, so the flag is cleared there.
ALTER TABLE obligation_occurrences
    ADD COLUMN is_estimate boolean NOT NULL DEFAULT false;
