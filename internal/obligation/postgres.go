package obligation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"grummans.finance.backend/internal/idempotency"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) CreateObligation(ctx context.Context, name, idempotencyKey string) (Obligation, error) {
	var result Obligation
	err := s.pool.QueryRow(ctx, `
		INSERT INTO obligations (name, idempotency_key, request_fingerprint)
		VALUES ($1, $2, $3)
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id::text, name, currency, created_at`,
		name, idempotencyKey, obligationFingerprint(name)).Scan(
		&result.ID, &result.Name, &result.Currency, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.replayedObligation(ctx, name, idempotencyKey)
	}
	if err != nil {
		return Obligation{}, fmt.Errorf("creating obligation: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) CreateOccurrence(ctx context.Context, obligationID string, dueDate time.Time, amountVND int64, idempotencyKey string) (Occurrence, error) {
	var occurrenceID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO obligation_occurrences (obligation_id, due_date, confirmed_amount_vnd, idempotency_key, request_fingerprint)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text`, obligationID, dueDate, amountVND, idempotencyKey,
		occurrenceFingerprint(obligationID, dueDate, amountVND)).Scan(&occurrenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.replayedOccurrence(ctx, obligationID, dueDate, amountVND, idempotencyKey)
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			return Occurrence{}, ErrReferenceNotFound
		}
		return Occurrence{}, fmt.Errorf("creating occurrence: %w", err)
	}
	return getOccurrence(ctx, s.pool, occurrenceID)
}

func (s *PostgresStore) UpdateOccurrenceAmount(ctx context.Context, occurrenceID string, amountVND int64, idempotencyKey string) (Occurrence, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Occurrence{}, fmt.Errorf("starting occurrence update: %w", err)
	}
	defer tx.Rollback(ctx)
	replayed, err := idempotency.Claim(ctx, tx, idempotencyKey,
		requestFingerprint("occurrence-amount", strings.ToLower(occurrenceID), strconv.FormatInt(amountVND, 10)))
	if err != nil {
		return Occurrence{}, mapIdempotencyError(err, "updating occurrence amount")
	}
	if replayed {
		// the original claim already applied this amount; never write it twice
		result, err := getOccurrence(ctx, tx, occurrenceID)
		if err != nil {
			return Occurrence{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Occurrence{}, fmt.Errorf("committing occurrence update: %w", err)
		}
		return result, nil
	}
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM obligation_occurrences WHERE id = $1 FOR UPDATE`, occurrenceID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Occurrence{}, ErrNotFound
		}
		return Occurrence{}, fmt.Errorf("locking occurrence: %w", err)
	}
	var paidAmount int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_vnd) FILTER (WHERE reversed_at IS NULL), 0)::bigint FROM payments WHERE occurrence_id = $1`, occurrenceID).Scan(&paidAmount)
	if err != nil {
		return Occurrence{}, fmt.Errorf("summing paid amount: %w", err)
	}
	if amountVND < paidAmount {
		return Occurrence{}, ErrAmountBelowPaid
	}
	command, err := tx.Exec(ctx, `UPDATE obligation_occurrences SET confirmed_amount_vnd = $2 WHERE id = $1`, occurrenceID, amountVND)
	if err != nil {
		return Occurrence{}, fmt.Errorf("updating occurrence amount: %w", err)
	}
	if command.RowsAffected() == 0 {
		return Occurrence{}, ErrNotFound
	}
	result, err := getOccurrence(ctx, tx, occurrenceID)
	if err != nil {
		return Occurrence{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Occurrence{}, fmt.Errorf("committing occurrence update: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) GetOccurrence(ctx context.Context, occurrenceID string) (Occurrence, error) {
	return getOccurrence(ctx, s.pool, occurrenceID)
}

func (s *PostgresStore) ListPayments(ctx context.Context, occurrenceID string) ([]Payment, error) {
	// one snapshot: the LEFT JOIN keeps a paymentless occurrence visible, so a
	// missing occurrence (no rows at all) stays distinguishable from an empty list
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.occurrence_id::text, p.amount_vnd, p.paid_at, p.reversed_at,
		       p.transaction_id::text
		FROM obligation_occurrences o
		LEFT JOIN payments p ON p.occurrence_id = o.id
		WHERE o.id = $1
		ORDER BY p.paid_at DESC NULLS LAST, p.id DESC`, occurrenceID)
	if err != nil {
		return nil, fmt.Errorf("listing payments: %w", err)
	}
	defer rows.Close()
	payments := make([]Payment, 0, 4)
	sawOccurrence := false
	for rows.Next() {
		sawOccurrence = true
		var payment Payment
		var id, paidOccurrenceID, transactionID *string
		var amountVND *int64
		var paidAt, reversedAt *time.Time
		if err := rows.Scan(&id, &paidOccurrenceID, &amountVND, &paidAt, &reversedAt, &transactionID); err != nil {
			return nil, fmt.Errorf("scanning payment: %w", err)
		}
		if id == nil || paidOccurrenceID == nil || amountVND == nil || paidAt == nil {
			continue // the occurrence exists but has no payment yet
		}
		payment = Payment{
			ID:            *id,
			OccurrenceID:  *paidOccurrenceID,
			AmountVND:     *amountVND,
			PaidAt:        *paidAt,
			ReversedAt:    reversedAt,
			TransactionID: transactionID,
		}
		payments = append(payments, payment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading payments: %w", err)
	}
	if !sawOccurrence {
		return nil, ErrNotFound
	}
	return payments, nil
}

func (s *PostgresStore) RecordPayment(ctx context.Context, occurrenceID string, amountVND int64, paidAt time.Time, idempotencyKey string) (Occurrence, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		result, attemptErr := s.recordPaymentOnce(ctx, occurrenceID, amountVND, paidAt, idempotencyKey)
		if !isSerializationFailure(attemptErr) && !isUniqueViolation(attemptErr) {
			return result, attemptErr
		}
		err = attemptErr
	}
	return Occurrence{}, fmt.Errorf("retrying payment transaction: %w", err)
}

func isSerializationFailure(err error) bool {
	var postgresErr *pgconn.PgError
	return errors.As(err, &postgresErr) && postgresErr.Code == "40001"
}
func (s *PostgresStore) recordPaymentOnce(ctx context.Context, occurrenceID string, amountVND int64, paidAt time.Time, idempotencyKey string) (Occurrence, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Occurrence{}, fmt.Errorf("starting payment transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	result, stored, err := getPaymentSnapshot(ctx, tx, idempotencyKey)
	if err == nil {
		if stored != paymentFingerprint(occurrenceID, amountVND, paidAt) {
			return Occurrence{}, fmt.Errorf("replaying payment: %w", ErrIdempotencyKeyReused)
		}
		if err := tx.Commit(ctx); err != nil {
			return Occurrence{}, fmt.Errorf("committing idempotent payment read: %w", err)
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Occurrence{}, fmt.Errorf("checking payment idempotency: %w", err)
	}

	var confirmedAmount, paidAmount int64
	err = tx.QueryRow(ctx, `
		SELECT confirmed_amount_vnd
		FROM obligation_occurrences
		WHERE id = $1
		FOR UPDATE`, occurrenceID).Scan(&confirmedAmount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Occurrence{}, ErrNotFound
	}
	if err != nil {
		return Occurrence{}, fmt.Errorf("locking occurrence for payment: %w", err)
	}
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_vnd) FILTER (WHERE reversed_at IS NULL), 0)::bigint
		FROM payments
		WHERE occurrence_id = $1`, occurrenceID).Scan(&paidAmount)
	if err != nil {
		return Occurrence{}, fmt.Errorf("summing paid amount: %w", err)
	}
	if amountVND > confirmedAmount-paidAmount {
		return Occurrence{}, ErrOverpayment
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO payments (occurrence_id, amount_vnd, paid_at, idempotency_key, paid_amount_vnd_after, request_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6)`, occurrenceID, amountVND, paidAt, idempotencyKey, paidAmount+amountVND,
		paymentFingerprint(occurrenceID, amountVND, paidAt))
	if err != nil {
		return Occurrence{}, fmt.Errorf("recording payment: %w", err)
	}
	result, err = getOccurrence(ctx, tx, occurrenceID)
	if err != nil {
		return Occurrence{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Occurrence{}, fmt.Errorf("committing payment: %w", err)
	}
	return result, nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getOccurrence(ctx context.Context, querier rowQuerier, occurrenceID string) (Occurrence, error) {
	var result Occurrence
	err := querier.QueryRow(ctx, `
		SELECT o.id::text, o.obligation_id::text, b.name, o.due_date, o.confirmed_amount_vnd,
		       COALESCE(SUM(p.amount_vnd) FILTER (WHERE p.reversed_at IS NULL), 0)::bigint
		FROM obligation_occurrences o
		JOIN obligations b ON b.id = o.obligation_id
		LEFT JOIN payments p ON p.occurrence_id = o.id
		WHERE o.id = $1
		GROUP BY o.id, b.name`, occurrenceID).Scan(
		&result.ID, &result.ObligationID, &result.ObligationName, &result.DueDate,
		&result.ConfirmedAmountVND, &result.PaidAmountVND)
	if errors.Is(err, pgx.ErrNoRows) {
		return Occurrence{}, ErrNotFound
	}
	if err != nil {
		return Occurrence{}, fmt.Errorf("getting occurrence: %w", err)
	}
	return result, nil
}

// paymentFingerprint pins the original payment request, so a reused key cannot
// silently replay a different occurrence, amount or time.
func paymentFingerprint(occurrenceID string, amountVND int64, paidAt time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(occurrenceID),
		strconv.FormatInt(amountVND, 10),
		paidAt.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z"),
	}, "")))
	return hex.EncodeToString(sum[:])
}

func getPaymentSnapshot(ctx context.Context, querier rowQuerier, idempotencyKey string) (Occurrence, string, error) {
	var result Occurrence
	var fingerprint string
	err := querier.QueryRow(ctx, `
		SELECT o.id::text, o.obligation_id::text, b.name, o.due_date, o.confirmed_amount_vnd,
		       p.paid_amount_vnd_after, p.request_fingerprint
		FROM payments p
		JOIN obligation_occurrences o ON o.id = p.occurrence_id
		JOIN obligations b ON b.id = o.obligation_id
		WHERE p.idempotency_key = $1`, idempotencyKey).Scan(
		&result.ID, &result.ObligationID, &result.ObligationName, &result.DueDate,
		&result.ConfirmedAmountVND, &result.PaidAmountVND, &fingerprint)
	if err != nil {
		return Occurrence{}, "", err
	}
	return result, fingerprint, nil
}

// requestFingerprint canonicalizes a create request: uuids lowercased and times
// truncated to microseconds, because that is what PostgreSQL stores.
func requestFingerprint(fields ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func obligationFingerprint(name string) string {
	return requestFingerprint("obligation", name)
}

func occurrenceFingerprint(obligationID string, dueDate time.Time, amountVND int64) string {
	return requestFingerprint("occurrence", strings.ToLower(obligationID),
		dueDate.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z"), strconv.FormatInt(amountVND, 10))
}

func isForeignKeyViolation(err error) bool {
	var postgresErr *pgconn.PgError
	return errors.As(err, &postgresErr) && postgresErr.Code == "23503"
}

func (s *PostgresStore) replayedObligation(ctx context.Context, name, idempotencyKey string) (Obligation, error) {
	var result Obligation
	var stored string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, currency, created_at, request_fingerprint
		FROM obligations WHERE idempotency_key = $1`, idempotencyKey).Scan(
		&result.ID, &result.Name, &result.Currency, &result.CreatedAt, &stored)
	if err != nil {
		return Obligation{}, fmt.Errorf("reading idempotent obligation: %w", err)
	}
	if stored != obligationFingerprint(name) {
		return Obligation{}, fmt.Errorf("replaying obligation: %w", ErrIdempotencyKeyReused)
	}
	return result, nil
}

func (s *PostgresStore) replayedOccurrence(ctx context.Context, obligationID string, dueDate time.Time, amountVND int64, idempotencyKey string) (Occurrence, error) {
	var occurrenceID string
	var stored string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, request_fingerprint FROM obligation_occurrences
		WHERE idempotency_key = $1`, idempotencyKey).Scan(&occurrenceID, &stored)
	if err != nil {
		return Occurrence{}, fmt.Errorf("reading idempotent occurrence: %w", err)
	}
	if stored != occurrenceFingerprint(obligationID, dueDate, amountVND) {
		return Occurrence{}, fmt.Errorf("replaying occurrence: %w", ErrIdempotencyKeyReused)
	}
	return getOccurrence(ctx, s.pool, occurrenceID)
}

func isUniqueViolation(err error) bool {
	var postgresErr *pgconn.PgError
	return errors.As(err, &postgresErr) && postgresErr.Code == "23505"
}

func mapIdempotencyError(err error, action string) error {
	if errors.Is(err, idempotency.ErrKeyReused) {
		return fmt.Errorf("%s: %w", action, ErrIdempotencyKeyReused)
	}
	return fmt.Errorf("%s: %w", action, err)
}
