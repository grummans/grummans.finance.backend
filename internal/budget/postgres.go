package budget

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
	"github.com/jackc/pgx/v5/pgxpool"

	"grummans.finance.backend/internal/idempotency"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const receiptColumns = `id::text, amount_vnd, to_char(received_on, 'YYYY-MM-DD'), created_at`

func (s *PostgresStore) CreateSalaryReceipt(ctx context.Context, amountVND int64, receivedOn time.Time, idempotencyKey string) (Receipt, error) {
	var result Receipt
	err := s.pool.QueryRow(ctx, `
		INSERT INTO salary_receipts (amount_vnd, received_on, idempotency_key, request_fingerprint)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+receiptColumns,
		amountVND, receivedOn, idempotencyKey, receiptFingerprint(amountVND, receivedOn)).Scan(
		&result.ID, &result.AmountVND, &result.ReceivedOn, &result.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.replayedReceipt(ctx, amountVND, receivedOn, idempotencyKey)
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("creating salary receipt: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) ListSalaryReceipts(ctx context.Context) ([]Receipt, error) {
	return listReceiptsFrom(ctx, s.pool)
}

func listReceiptsFrom(ctx context.Context, querier rowQuerier) ([]Receipt, error) {
	rows, err := querier.Query(ctx, `
		SELECT `+receiptColumns+` FROM salary_receipts
		ORDER BY received_on DESC, created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing salary receipts: %w", err)
	}
	defer rows.Close()

	receipts := make([]Receipt, 0, 12)
	for rows.Next() {
		var receipt Receipt
		if err := rows.Scan(&receipt.ID, &receipt.AmountVND, &receipt.ReceivedOn, &receipt.ReceivedAt); err != nil {
			return nil, fmt.Errorf("scanning salary receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading salary receipts: %w", err)
	}
	return receipts, nil
}

// replayedReceipt returns the original receipt for a retried key, and refuses a
// key that is now carrying a different amount or date.
func (s *PostgresStore) replayedReceipt(ctx context.Context, amountVND int64, receivedOn time.Time, idempotencyKey string) (Receipt, error) {
	var result Receipt
	var stored string
	err := s.pool.QueryRow(ctx, `
		SELECT `+receiptColumns+`, request_fingerprint
		FROM salary_receipts WHERE idempotency_key = $1`, idempotencyKey).Scan(
		&result.ID, &result.AmountVND, &result.ReceivedOn, &result.ReceivedAt, &stored)
	if err != nil {
		return Receipt{}, fmt.Errorf("reading idempotent salary receipt: %w", err)
	}
	if stored != receiptFingerprint(amountVND, receivedOn) {
		return Receipt{}, fmt.Errorf("replaying salary receipt: %w", ErrIdempotencyKeyReused)
	}
	return result, nil
}

func (s *PostgresStore) GetSettings(ctx context.Context) (Settings, error) {
	var settings Settings
	err := s.pool.QueryRow(ctx, `
		SELECT global_buffer_vnd, global_budget_vnd FROM budget_settings WHERE id = 1`).Scan(
		&settings.GlobalBufferVND, &settings.GlobalBudgetVND)
	if err != nil {
		return Settings{}, fmt.Errorf("reading budget settings: %w", err)
	}
	return settings, nil
}

// UpdateSettings claims the mutation key before writing, so a retry replays the
// stored settings and a reused key with other values is refused.
func (s *PostgresStore) UpdateSettings(ctx context.Context, settings Settings, idempotencyKey string) (Settings, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("starting settings update: %w", err)
	}
	defer tx.Rollback(ctx)

	replayed, err := idempotency.Claim(ctx, tx, idempotencyKey, settingsFingerprint(settings))
	if err != nil {
		if errors.Is(err, idempotency.ErrKeyReused) {
			return Settings{}, fmt.Errorf("updating budget settings: %w", ErrIdempotencyKeyReused)
		}
		return Settings{}, fmt.Errorf("claiming settings update: %w", err)
	}
	if replayed {
		// the claim proved this key carried exactly this payload, so the original
		// response is the request itself and not whatever a later key wrote
		if err := tx.Commit(ctx); err != nil {
			return Settings{}, fmt.Errorf("committing settings replay: %w", err)
		}
		return settings, nil
	}

	var result Settings
	err = tx.QueryRow(ctx, `
		UPDATE budget_settings
		SET global_buffer_vnd = $1, global_budget_vnd = $2, updated_at = now()
		WHERE id = 1
		RETURNING global_buffer_vnd, global_budget_vnd`,
		settings.GlobalBufferVND, settings.GlobalBudgetVND).Scan(
		&result.GlobalBufferVND, &result.GlobalBudgetVND)
	if err != nil {
		return Settings{}, fmt.Errorf("updating budget settings: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Settings{}, fmt.Errorf("committing settings update: %w", err)
	}
	return result, nil
}

// rowQuerier is the part of a pool or a transaction the reads here need.
type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getSettingsFrom(ctx context.Context, querier rowQuerier) (Settings, error) {
	var settings Settings
	err := querier.QueryRow(ctx, `
		SELECT global_buffer_vnd, global_budget_vnd FROM budget_settings WHERE id = 1`).Scan(
		&settings.GlobalBufferVND, &settings.GlobalBudgetVND)
	if err != nil {
		return Settings{}, fmt.Errorf("reading budget settings: %w", err)
	}
	return settings, nil
}

// SnapshotData reads the receipts, the settings, cash, cycle spending and the
// unpaid obligations in one repeatable-read transaction, so every number in a
// snapshot — including the cycle those numbers were filtered by — belongs to the
// same moment.
func (s *PostgresStore) SnapshotData(ctx context.Context, now time.Time) (Data, []Receipt, Settings, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return Data{}, nil, Settings{}, fmt.Errorf("starting budget snapshot: %w", err)
	}
	defer tx.Rollback(ctx)

	receipts, err := listReceiptsFrom(ctx, tx)
	if err != nil {
		return Data{}, nil, Settings{}, err
	}
	settings, err := getSettingsFrom(ctx, tx)
	if err != nil {
		return Data{}, nil, Settings{}, err
	}
	cycle := CycleFor(now, receipts)
	start, err := ParseBusinessDate(cycle.Start)
	if err != nil {
		return Data{}, nil, Settings{}, fmt.Errorf("parsing cycle start: %w", err)
	}
	end, err := ParseBusinessDate(cycle.End)
	if err != nil {
		return Data{}, nil, Settings{}, fmt.Errorf("parsing cycle end: %w", err)
	}
	data, err := budgetDataFrom(ctx, tx, start, end)
	if err != nil {
		return Data{}, nil, Settings{}, err
	}
	return data, receipts, settings, nil
}

func budgetDataFrom(ctx context.Context, tx rowQuerier, cycleStart, cycleEnd time.Time) (Data, error) {
	var data Data
	// credit accounts are borrowing room, never cash on hand
	err := tx.QueryRow(ctx, `
		SELECT
			(SELECT COALESCE(SUM(opening_balance_vnd), 0) FROM accounts WHERE kind <> 'CREDIT')
			+ (SELECT COALESCE(SUM(CASE WHEN t.direction = 'IN' THEN t.amount_vnd ELSE -t.amount_vnd END), 0)
			   FROM transactions t JOIN accounts a ON a.id = t.account_id
			   WHERE a.kind <> 'CREDIT')`).Scan(&data.CashOnHandVND)
	if err != nil {
		return Data{}, fmt.Errorf("summing cash on hand: %w", err)
	}

	// transfers and repayments move money between accounts, so they are not spend
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_vnd), 0) FROM transactions
		WHERE type IN ('EXPENSE', 'FEE') AND occurred_at >= $1 AND occurred_at < $2`,
		cycleStart, cycleEnd).Scan(&data.SpentInCycleVND)
	if err != nil {
		return Data{}, fmt.Errorf("summing cycle spending: %w", err)
	}

	// everything still unpaid and due before the next payday is reserved, overdue included
	rows, err := tx.Query(ctx, `
		SELECT o.id::text, b.name, to_char(o.due_date, 'YYYY-MM-DD'), o.is_estimate,
		       o.confirmed_amount_vnd - COALESCE(SUM(p.amount_vnd) FILTER (WHERE p.reversed_at IS NULL), 0)
		FROM obligation_occurrences o
		JOIN obligations b ON b.id = o.obligation_id
		LEFT JOIN payments p ON p.occurrence_id = o.id
		WHERE o.due_date < $1::date
		GROUP BY o.id, b.name
		HAVING o.confirmed_amount_vnd > COALESCE(SUM(p.amount_vnd) FILTER (WHERE p.reversed_at IS NULL), 0)
		ORDER BY o.due_date, b.name`, cycleEnd.Format(dateLayout))
	if err != nil {
		return Data{}, fmt.Errorf("listing reserved occurrences: %w", err)
	}
	defer rows.Close()

	data.Occurrences = make([]Occurrence, 0, 8)
	for rows.Next() {
		var occurrence Occurrence
		if err := rows.Scan(&occurrence.ID, &occurrence.ObligationName, &occurrence.DueDate,
			&occurrence.IsEstimate, &occurrence.RemainingVND); err != nil {
			return Data{}, fmt.Errorf("scanning reserved occurrence: %w", err)
		}
		data.Occurrences = append(data.Occurrences, occurrence)
	}
	if err := rows.Err(); err != nil {
		return Data{}, fmt.Errorf("reading reserved occurrences: %w", err)
	}
	return data, nil
}

// receiptFingerprint pins the original request: a retried key replays it, a
// different amount or date under the same key is a conflict.
func receiptFingerprint(amountVND int64, receivedOn time.Time) string {
	return digest("salary-receipt", strconv.FormatInt(amountVND, 10), receivedOn.Format(dateLayout))
}

func settingsFingerprint(settings Settings) string {
	budget := "null"
	if settings.GlobalBudgetVND != nil {
		budget = strconv.FormatInt(*settings.GlobalBudgetVND, 10)
	}
	return digest("budget-settings", strconv.FormatInt(settings.GlobalBufferVND, 10), budget)
}

func digest(fields ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x1f")))
	return hex.EncodeToString(sum[:])
}
