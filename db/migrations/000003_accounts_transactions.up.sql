CREATE TABLE accounts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (length(trim(name)) > 0),
    kind text NOT NULL CHECK (kind IN ('CASH', 'BANK', 'CREDIT')),
    currency char(3) NOT NULL DEFAULT 'VND' CHECK (currency = 'VND'),
    opening_balance_vnd bigint NOT NULL DEFAULT 0 CHECK (opening_balance_vnd >= 0),
    credit_limit_vnd bigint CHECK (credit_limit_vnd > 0),
    opened_on date NOT NULL DEFAULT (now() AT TIME ZONE 'Asia/Ho_Chi_Minh')::date,
    -- the opening balance must never be booked twice by a retried create
    idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    -- immutable fingerprint of the original request, compared on a retried key
    request_fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- credit limit is never part of cash on hand, and cash accounts have no limit
    CONSTRAINT accounts_credit_check CHECK (
        (kind = 'CREDIT' AND credit_limit_vnd IS NOT NULL AND opening_balance_vnd = 0)
        OR (kind <> 'CREDIT' AND credit_limit_vnd IS NULL)
    )
);

CREATE TABLE categories (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transactions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts(id),
    direction text NOT NULL CHECK (direction IN ('IN', 'OUT')),
    type text NOT NULL CHECK (type IN ('EXPENSE', 'INCOME', 'TRANSFER', 'REPAYMENT', 'FEE', 'REFUND')),
    amount_vnd bigint NOT NULL CHECK (amount_vnd > 0),
    occurred_at timestamptz NOT NULL,
    category_id uuid REFERENCES categories(id),
    note text CHECK (note IS NULL OR length(note) <= 500),
    transfer_id uuid,
    idempotency_key text UNIQUE CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 1 AND 255),
    -- set on the OUT posting only, so a transfer retry is compared once
    request_fingerprint text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transactions_type_direction_check CHECK (
        (type = 'EXPENSE' AND direction = 'OUT')
        OR (type = 'FEE' AND direction = 'OUT')
        OR (type = 'REPAYMENT' AND direction = 'OUT')
        OR (type = 'INCOME' AND direction = 'IN')
        OR (type = 'REFUND' AND direction = 'IN')
        OR type = 'TRANSFER'
    ),
    -- a transfer is exactly a linked OUT/IN posting pair sharing transfer_id
    CONSTRAINT transactions_transfer_link_check CHECK ((type = 'TRANSFER') = (transfer_id IS NOT NULL)),
    CONSTRAINT transactions_transfer_category_check CHECK (type <> 'TRANSFER' OR category_id IS NULL),
    CONSTRAINT transactions_idempotency_check CHECK (type = 'TRANSFER' OR idempotency_key IS NOT NULL),
    CONSTRAINT transactions_fingerprint_check CHECK ((idempotency_key IS NULL) = (request_fingerprint IS NULL))
);

CREATE INDEX transactions_timeline_idx ON transactions (occurred_at DESC, created_at DESC, id DESC);
CREATE INDEX transactions_account_idx ON transactions (account_id);
CREATE INDEX transactions_category_idx ON transactions (category_id) WHERE category_id IS NOT NULL;
CREATE INDEX transactions_transfer_idx ON transactions (transfer_id) WHERE transfer_id IS NOT NULL;

-- manual confirmation only: linking a bank transaction never changes payment amounts
ALTER TABLE payments
    ADD COLUMN transaction_id uuid REFERENCES transactions(id),
    ADD CONSTRAINT payments_transaction_id_unique UNIQUE (transaction_id);
