package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"grummans.finance.backend/internal/idempotency"
)

var (
	ErrNotFound             = errors.New("record not found")
	ErrReferenceNotFound    = errors.New("referenced record not found")
	ErrAlreadyExists        = errors.New("record already exists")
	ErrAlreadyLinked        = errors.New("payment or transaction is already linked")
	ErrInvalidChange        = errors.New("change violates a data rule")
	ErrIdempotencyKeyReused = errors.New("idempotency key was already used for a different request")
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const accountColumns = `id::text, name, kind, currency, opening_balance_vnd,
	COALESCE(credit_limit_vnd, 0), to_char(opened_on, 'YYYY-MM-DD')`

func (s *PostgresStore) CreateAccount(ctx context.Context, input AccountInput) (Account, error) {
	if input.IdempotencyKey == nil {
		return Account{}, fmt.Errorf("creating account: %w", errors.New("idempotency key is required"))
	}
	result, err := scanAccount(s.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, kind, opening_balance_vnd, credit_limit_vnd, idempotency_key, request_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+accountColumns,
		input.Name, input.Kind, input.OpeningBalanceVND, input.CreditLimitVND, input.IdempotencyKey,
		accountFingerprint(input)).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := scanAccount(s.pool.QueryRow(ctx, `
			SELECT `+accountColumns+` FROM accounts WHERE idempotency_key = $1`,
			*input.IdempotencyKey).Scan)
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrNotFound
		}
		if err != nil {
			return Account{}, fmt.Errorf("reading idempotent account: %w", err)
		}
		var stored string
		if err := s.pool.QueryRow(ctx, `
			SELECT request_fingerprint FROM accounts WHERE idempotency_key = $1`,
			*input.IdempotencyKey).Scan(&stored); err != nil {
			return Account{}, fmt.Errorf("reading idempotent account fingerprint: %w", err)
		}
		if stored != accountFingerprint(input) {
			return Account{}, fmt.Errorf("replaying idempotency key: %w", ErrIdempotencyKeyReused)
		}
		return existing, nil
	}
	if err != nil {
		return Account{}, mapWriteError(err, "creating account")
	}
	return result, nil
}

func (s *PostgresStore) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+accountColumns+`
		FROM accounts
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	defer rows.Close()

	accounts := []Account{}
	for rows.Next() {
		account, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	return accounts, nil
}

// CreateCategory returns the existing category when the name is already taken,
// so a retried create stays harmless.
func (s *PostgresStore) CreateCategory(ctx context.Context, name, idempotencyKey string) (Category, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Category{}, fmt.Errorf("starting category create: %w", err)
	}
	defer tx.Rollback(ctx)
	replayed, err := idempotency.Claim(ctx, tx, idempotencyKey, requestFingerprint("category", name))
	if err != nil {
		return Category{}, mapIdempotencyError(err, "creating category")
	}
	if replayed {
		// the original claim already created this category; never write again
		var existing Category
		if err := tx.QueryRow(ctx, `SELECT id::text, name FROM categories WHERE name = $1`, name).
			Scan(&existing.ID, &existing.Name); err != nil {
			return Category{}, fmt.Errorf("reading replayed category: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Category{}, fmt.Errorf("committing category create: %w", err)
		}
		return existing, nil
	}
	var result Category
	err = tx.QueryRow(ctx, `
		INSERT INTO categories (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text, name`, name).Scan(&result.ID, &result.Name)
	if err != nil {
		return Category{}, fmt.Errorf("creating category: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Category{}, fmt.Errorf("committing category create: %w", err)
	}
	_ = replayed
	return result, nil
}

func (s *PostgresStore) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, name FROM categories ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing categories: %w", err)
	}
	defer rows.Close()

	categories := []Category{}
	for rows.Next() {
		var category Category
		if err := rows.Scan(&category.ID, &category.Name); err != nil {
			return nil, fmt.Errorf("scanning category: %w", err)
		}
		categories = append(categories, category)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing categories: %w", err)
	}
	return categories, nil
}

func (s *PostgresStore) CreateTransaction(ctx context.Context, input TransactionInput) ([]Transaction, error) {
	if input.Type == TypeTransfer {
		return s.createTransfer(ctx, input)
	}
	if input.IdempotencyKey == nil {
		return nil, fmt.Errorf("creating transaction: %w", errors.New("idempotency key is required"))
	}

	var id string
	// a retried key returns the original posting instead of a duplicate
	err := s.pool.QueryRow(ctx, `
		INSERT INTO transactions (account_id, direction, type, amount_vnd, occurred_at, category_id, note, idempotency_key, request_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text`,
		input.AccountID, DirectionFor(input.Type), string(input.Type), input.AmountVND, input.OccurredAt,
		input.CategoryID, input.Note, input.IdempotencyKey, transactionFingerprint(input)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.replayedTransaction(ctx, s.pool, input)
	}
	if err != nil {
		return nil, mapWriteError(err, "creating transaction")
	}
	created, err := getTransaction(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	return []Transaction{created}, nil
}

// createTransfer posts the linked OUT/IN pair atomically, so a transfer can never
// appear as a single expense or income row.
// shortcut: the "exactly two postings per transfer_id" invariant is enforced only by
// this function, not by the schema; add a deferred constraint trigger if other writers appear.
func (s *PostgresStore) createTransfer(ctx context.Context, input TransactionInput) ([]Transaction, error) {
	if input.IdempotencyKey == nil {
		return nil, fmt.Errorf("creating transfer: %w", errors.New("idempotency key is required"))
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting transfer: %w", err)
	}
	defer tx.Rollback(ctx)

	var outID, transferID string
	err = tx.QueryRow(ctx, `
		INSERT INTO transactions (account_id, direction, type, amount_vnd, occurred_at, note, transfer_id, idempotency_key, request_fingerprint)
		VALUES ($1, 'OUT', 'TRANSFER', $2, $3, $4, gen_random_uuid(), $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text, transfer_id::text`,
		input.AccountID, input.AmountVND, input.OccurredAt, input.Note, input.IdempotencyKey,
		transactionFingerprint(input)).Scan(&outID, &transferID)
	if errors.Is(err, pgx.ErrNoRows) {
		postings, err := s.replayedTransaction(ctx, tx, input)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("committing transfer retry: %w", err)
		}
		return postings, nil
	}
	if err != nil {
		return nil, mapWriteError(err, "creating transfer posting")
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO transactions (account_id, direction, type, amount_vnd, occurred_at, note, transfer_id)
		VALUES ($1, 'IN', 'TRANSFER', $2, $3, $4, $5)`,
		input.ToAccountID, input.AmountVND, input.OccurredAt, input.Note, transferID)
	if err != nil {
		return nil, mapWriteError(err, "creating transfer posting")
	}
	postings, err := transactionsByTransfer(ctx, tx, transferID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing transfer: %w", err)
	}
	return postings, nil
}

func (s *PostgresStore) GetTransaction(ctx context.Context, id string) (Transaction, error) {
	return getTransaction(ctx, s.pool, id)
}

func (s *PostgresStore) ListTransactions(ctx context.Context, filter Filter) ([]Transaction, error) {
	var from, to *string
	if filter.HasFrom {
		value := filter.From.Format("2006-01-02")
		from = &value
	}
	if filter.HasTo {
		value := filter.To.Format("2006-01-02")
		to = &value
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+transactionColumns+`
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN categories c ON c.id = t.category_id
		WHERE ($1::date IS NULL OR t.occurred_at >= ($1::date::timestamp AT TIME ZONE 'Asia/Ho_Chi_Minh'))
		  AND ($2::date IS NULL OR t.occurred_at < ((($2::date + 1)::timestamp) AT TIME ZONE 'Asia/Ho_Chi_Minh'))
		  AND ($3::uuid IS NULL OR t.category_id = $3::uuid)
		ORDER BY t.occurred_at DESC, t.created_at DESC, t.id DESC
		LIMIT $4 OFFSET $5`,
		from, to, filter.CategoryID, limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("listing transactions: %w", err)
	}
	defer rows.Close()

	transactions := []Transaction{}
	for rows.Next() {
		transaction, err := scanTransaction(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scanning transaction: %w", err)
		}
		transactions = append(transactions, transaction)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing transactions: %w", err)
	}
	return transactions, nil
}

func (s *PostgresStore) UpdateTransactionCategory(ctx context.Context, id string, categoryID *string, idempotencyKey string) (Transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Transaction{}, fmt.Errorf("starting transaction update: %w", err)
	}
	defer tx.Rollback(ctx)
	category := ""
	if categoryID != nil {
		category = strings.ToLower(*categoryID)
	}
	// the fingerprint covers the intent (this category), not the previous value
	replayed, err := idempotency.Claim(ctx, tx, idempotencyKey,
		requestFingerprint("transaction-category", strings.ToLower(id), category))
	if err != nil {
		return Transaction{}, mapIdempotencyError(err, "updating transaction category")
	}
	if replayed {
		// the original claim already applied this category; never overwrite a later edit
		replayedTransaction, err := getTransaction(ctx, tx, id)
		if err != nil {
			return Transaction{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Transaction{}, fmt.Errorf("committing transaction update: %w", err)
		}
		return replayedTransaction, nil
	}
	command, err := tx.Exec(ctx, `UPDATE transactions SET category_id = $2 WHERE id = $1`, id, categoryID)
	if err != nil {
		return Transaction{}, mapWriteError(err, "updating transaction category")
	}
	if command.RowsAffected() == 0 {
		return Transaction{}, ErrNotFound
	}
	// update and read share one transaction, so a concurrent update cannot leak in
	updated, err := getTransaction(ctx, tx, id)
	if err != nil {
		return Transaction{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transaction{}, fmt.Errorf("committing transaction update: %w", err)
	}
	return updated, nil
}

func (s *PostgresStore) LinkPaymentTransaction(ctx context.Context, paymentID, transactionID, idempotencyKey string) (PaymentLink, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentLink{}, fmt.Errorf("starting payment link: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := idempotency.Claim(ctx, tx, idempotencyKey,
		requestFingerprint("payment-link", strings.ToLower(paymentID), strings.ToLower(transactionID))); err != nil {
		return PaymentLink{}, mapIdempotencyError(err, "linking payment")
	}
	command, err := tx.Exec(ctx, `
		UPDATE payments p SET transaction_id = $2
		WHERE p.id = $1 AND p.reversed_at IS NULL AND p.transaction_id IS NULL
		  AND EXISTS (
		    SELECT 1 FROM transactions t
		    JOIN accounts a ON a.id = t.account_id
		    WHERE t.id = $2 AND t.direction = 'OUT' AND a.kind <> 'CREDIT'
		  )`, paymentID, transactionID)
	if err != nil {
		return PaymentLink{}, mapWriteError(err, "linking payment")
	}
	if command.RowsAffected() == 0 {
		var existing string
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(transaction_id::text, '') FROM payments
			WHERE id = $1 AND reversed_at IS NULL`, paymentID).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentLink{}, ErrNotFound
		}
		if err != nil {
			return PaymentLink{}, fmt.Errorf("checking payment before linking: %w", err)
		}
		if strings.EqualFold(existing, transactionID) {
			// repeating the same link is harmless; commit so the key stays claimed
			if err := tx.Commit(ctx); err != nil {
				return PaymentLink{}, fmt.Errorf("committing payment link: %w", err)
			}
			return PaymentLink{PaymentID: paymentID, TransactionID: &existing}, nil
		}
		// a payment may only be confirmed against money leaving a real account:
		// not an income/refund posting, not a credit-card liability
		var eligible, transactionExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM transactions t JOIN accounts a ON a.id = t.account_id
				WHERE t.id = $1 AND t.direction = 'OUT' AND a.kind <> 'CREDIT'
			), EXISTS (SELECT 1 FROM transactions WHERE id = $1)`,
			transactionID).Scan(&eligible, &transactionExists); err != nil {
			return PaymentLink{}, fmt.Errorf("checking transaction eligibility: %w", err)
		}
		if !transactionExists {
			return PaymentLink{}, ErrReferenceNotFound
		}
		if !eligible {
			return PaymentLink{}, fmt.Errorf("linking a payment: %w", ErrInvalidChange)
		}
		return PaymentLink{}, ErrAlreadyLinked
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentLink{}, fmt.Errorf("committing payment link: %w", err)
	}
	return PaymentLink{PaymentID: paymentID, TransactionID: &transactionID}, nil
}

const transactionColumns = `t.id::text, t.account_id::text, a.name, t.direction, t.type, t.amount_vnd,
	t.occurred_at, COALESCE(t.category_id::text, ''), COALESCE(c.name, ''), COALESCE(t.note, ''),
	COALESCE(t.transfer_id::text, '')`

func getTransaction(ctx context.Context, querier rowQuerier, id string) (Transaction, error) {
	transaction, err := scanTransaction(querier.QueryRow(ctx, `
		SELECT `+transactionColumns+`
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN categories c ON c.id = t.category_id
		WHERE t.id = $1`, id).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transaction{}, ErrNotFound
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("getting transaction: %w", err)
	}
	return transaction, nil
}

// replayedTransaction returns the original posting for a retried key, but refuses
// a key whose money fields no longer match the stored posting.
func (s *PostgresStore) replayedTransaction(ctx context.Context, querier rowQuerier, input TransactionInput) ([]Transaction, error) {
	postings, err := s.transactionsByIdempotencyKey(ctx, querier, *input.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	var stored string
	if err := querier.QueryRow(ctx, `
		SELECT request_fingerprint FROM transactions WHERE idempotency_key = $1`,
		*input.IdempotencyKey).Scan(&stored); err != nil {
		return nil, fmt.Errorf("reading idempotent fingerprint: %w", err)
	}
	if stored != transactionFingerprint(input) {
		return nil, fmt.Errorf("replaying idempotency key: %w", ErrIdempotencyKeyReused)
	}
	return postings, nil
}

func (s *PostgresStore) transactionsByIdempotencyKey(ctx context.Context, querier rowQuerier, key string) ([]Transaction, error) {
	var transferID string
	err := querier.QueryRow(ctx, `
		SELECT COALESCE(transfer_id::text, '') FROM transactions WHERE idempotency_key = $1`, key).Scan(&transferID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading idempotent transaction: %w", err)
	}
	if transferID == "" {
		existing, err := scanTransaction(querier.QueryRow(ctx, `
			SELECT `+transactionColumns+`
			FROM transactions t
			JOIN accounts a ON a.id = t.account_id
			LEFT JOIN categories c ON c.id = t.category_id
			WHERE t.idempotency_key = $1`, key).Scan)
		if err != nil {
			return nil, fmt.Errorf("reading idempotent transaction: %w", err)
		}
		return []Transaction{existing}, nil
	}
	return transactionsByTransfer(ctx, querier, transferID)
}

func transactionsByTransfer(ctx context.Context, querier rowQuerier, transferID string) ([]Transaction, error) {
	rows, err := querier.Query(ctx, `
		SELECT `+transactionColumns+`
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN categories c ON c.id = t.category_id
		WHERE t.transfer_id = $1
		ORDER BY t.direction`, transferID)
	if err != nil {
		return nil, fmt.Errorf("reading transfer postings: %w", err)
	}
	defer rows.Close()

	postings := []Transaction{}
	for rows.Next() {
		posting, err := scanTransaction(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scanning transfer posting: %w", err)
		}
		postings = append(postings, posting)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading transfer postings: %w", err)
	}
	return postings, nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func scanAccount(scan func(...any) error) (Account, error) {
	var result Account
	var creditLimit int64
	if err := scan(&result.ID, &result.Name, &result.Kind, &result.Currency,
		&result.OpeningBalanceVND, &creditLimit, &result.OpenedOn); err != nil {
		return Account{}, err
	}
	if creditLimit > 0 {
		result.CreditLimitVND = &creditLimit
	}
	return result, nil
}

func scanTransaction(scan func(...any) error) (Transaction, error) {
	var result Transaction

	var categoryID, categoryName, note, transferID string
	err := scan(&result.ID, &result.AccountID, &result.AccountName, &result.Direction, &result.Type,
		&result.AmountVND, &result.OccurredAt, &categoryID, &categoryName, &note, &transferID)
	if err != nil {
		return Transaction{}, err
	}
	result.CategoryID = nilIfEmpty(categoryID)
	result.CategoryName = nilIfEmpty(categoryName)
	result.Note = nilIfEmpty(note)
	result.TransferID = nilIfEmpty(transferID)
	return result, nil
}

func nilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func mapWriteError(err error, action string) error {
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		switch postgresErr.Code {
		case "23503": // foreign key violation
			return fmt.Errorf("%s: %w", action, ErrReferenceNotFound)
		case "23505": // unique violation
			if postgresErr.ConstraintName == "payments_transaction_id_unique" {
				return fmt.Errorf("%s: %w", action, ErrAlreadyLinked)
			}
			return fmt.Errorf("%s: %w", action, ErrAlreadyExists)
		case "23514": // check constraint violation
			return fmt.Errorf("%s: %w", action, ErrInvalidChange)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

// mapIdempotencyError keeps the ledger sentinel the handlers already map to 409.
func mapIdempotencyError(err error, action string) error {
	if errors.Is(err, idempotency.ErrKeyReused) {
		return fmt.Errorf("%s: %w", action, ErrIdempotencyKeyReused)
	}
	return fmt.Errorf("%s: %w", action, err)
}
