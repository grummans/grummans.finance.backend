# Terminal Finance backend

Go API for personal finance: obligations, occurrences, idempotent payments, accounts,
categories and a transaction timeline.

Implemented so far:

- **Obligations & payments** — a recurring obligation template, a manually created monthly
  occurrence, manual confirmation of a variable amount, and idempotent partial payments.
- **Accounts** — cash / bank / credit with an opening balance; a credit limit is never cash.
- **Categories & transactions** — typed postings (EXPENSE, INCOME, TRANSFER, REPAYMENT, FEE,
  REFUND) with an int64 VND amount and a direction, never a float and never a signed amount.
  A transfer writes two postings under one `transfer_id`, so it can never count twice.
- **Timeline** — filters by business date range (`Asia/Ho_Chi_Minh`) and category, paginated.
- **Manual reconciliation** — list an occurrence's payments and link one to the bank
  transaction that produced it. Linking never changes the confirmed amount.

Every mutation requires `Idempotency-Key`. A retry with the same key and the same payload
replays the original outcome; the same key with a different payload returns 409.

## Local development

1. Start PostgreSQL: `docker compose up -d`.
2. Apply pending migrations: `DATABASE_URL=postgres://terminal_finance:terminal_finance@localhost:5432/terminal_finance?sslmode=disable make migrate`.
3. Run the API with the same `DATABASE_URL`: `DATABASE_URL=postgres://terminal_finance:terminal_finance@localhost:5432/terminal_finance?sslmode=disable make run`.

`MIGRATIONS_PATH` overrides the default `file://db/migrations` location for deployments.

## Checks

```bash
go vet ./... && go test ./...
go vet -tags integration ./... && go test -tags integration ./...   # needs DATABASE_URL
make lint-openapi                                                   # redocly
```

Integration tests skip themselves when `DATABASE_URL` is unset.

## Layout

- `api/openapi.yaml` — the HTTP contract.
- `cmd/api`, `cmd/migrate` — the server and the migration runner.
- `internal/obligation` — obligations, occurrences, payments.
- `internal/ledger` — accounts, categories, transactions, payment links.
- `internal/idempotency` — the shared mutation-key claim used by update-style endpoints.
- `internal/platform/httpapi` — routing, JSON decoding, error mapping.
- `db/migrations` — forward-only migrations; released files are never edited.
