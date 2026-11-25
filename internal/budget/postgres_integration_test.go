//go:build integration

package budget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"grummans.finance.backend/internal/ledger"
	"grummans.finance.backend/internal/obligation"
)

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresStoreSalaryReceiptReplay(t *testing.T) {
	pool := openPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	day := BusinessDate(time.Now())
	key := fmt.Sprintf("salary-key-%d", suffix)

	first, err := store.CreateSalaryReceipt(ctx, 13_095_124, day, key)
	if err != nil {
		t.Fatalf("creating salary receipt: %v", err)
	}
	if first.ReceivedOn != day.Format(dateLayout) {
		t.Fatalf("received_on = %s, want %s", first.ReceivedOn, day.Format(dateLayout))
	}

	// a retried key must return the first receipt, not record the salary twice
	replay, err := store.CreateSalaryReceipt(ctx, 13_095_124, day, key)
	if err != nil {
		t.Fatalf("replaying salary receipt: %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replayed id = %s, want %s", replay.ID, first.ID)
	}

	if _, err := store.CreateSalaryReceipt(ctx, 1, day, key); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key with another amount = %v, want ErrIdempotencyKeyReused", err)
	}

	receipts, err := store.ListSalaryReceipts(ctx)
	if err != nil {
		t.Fatalf("listing salary receipts: %v", err)
	}
	matches := 0
	for _, receipt := range receipts {
		if receipt.ID == first.ID {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("the receipt appears %d times, want exactly 1", matches)
	}
}

func TestPostgresStoreUpdateSettingsReplay(t *testing.T) {
	pool := openPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	budgetVND := int64(8_000_000)
	key := fmt.Sprintf("settings-key-%d", suffix)

	settings, err := store.UpdateSettings(ctx, Settings{GlobalBufferVND: 2_000_000, GlobalBudgetVND: &budgetVND}, key)
	if err != nil {
		t.Fatalf("updating settings: %v", err)
	}
	if settings.GlobalBufferVND != 2_000_000 || settings.GlobalBudgetVND == nil || *settings.GlobalBudgetVND != 8_000_000 {
		t.Fatalf("settings = %+v, want buffer 2000000 and budget 8000000", settings)
	}

	replay, err := store.UpdateSettings(ctx, Settings{GlobalBufferVND: 2_000_000, GlobalBudgetVND: &budgetVND}, key)
	if err != nil {
		t.Fatalf("replaying settings update: %v", err)
	}
	if replay.GlobalBufferVND != 2_000_000 {
		t.Fatalf("replayed buffer = %d, want 2000000", replay.GlobalBufferVND)
	}

	if _, err := store.UpdateSettings(ctx, Settings{GlobalBufferVND: 1}, key); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key with other values = %v, want ErrIdempotencyKeyReused", err)
	}

	// a later key changes the stored settings; replaying the first key must answer
	// with what that key wrote, not with the newer state
	if _, err := store.UpdateSettings(ctx, Settings{GlobalBufferVND: 500_000}, fmt.Sprintf("settings-later-%d", suffix)); err != nil {
		t.Fatalf("updating settings with another key: %v", err)
	}
	replayAfterUpdate, err := store.UpdateSettings(ctx, Settings{GlobalBufferVND: 2_000_000, GlobalBudgetVND: &budgetVND}, key)
	if err != nil {
		t.Fatalf("replaying settings after another update: %v", err)
	}
	if replayAfterUpdate.GlobalBufferVND != 2_000_000 || replayAfterUpdate.GlobalBudgetVND == nil || *replayAfterUpdate.GlobalBudgetVND != 8_000_000 {
		t.Fatalf("replayed settings = %+v, want the buffer 2000000 and budget 8000000 of that key", replayAfterUpdate)
	}

	cleared, err := store.UpdateSettings(ctx, Settings{}, fmt.Sprintf("settings-clear-%d", suffix))
	if err != nil {
		t.Fatalf("clearing settings: %v", err)
	}
	if cleared.GlobalBudgetVND != nil || cleared.GlobalBufferVND != 0 {
		t.Fatalf("cleared settings = %+v, want no buffer and no budget", cleared)
	}
}

func TestPostgresStoreSnapshotData(t *testing.T) {
	pool := openPool(t)
	store := NewPostgresStore(pool)
	ctx := context.Background()
	now := time.Now()
	suffix := now.UnixNano()

	today := BusinessDate(now)
	// pin the cycle: a receipt today starts it and a recorded payday 20 days later
	// ends it, so the reserve window does not depend on the day the test runs
	cycleEnd := today.AddDate(0, 0, 20)
	for _, pinned := range []struct {
		day time.Time
		key string
	}{
		{day: today, key: fmt.Sprintf("snapshot-start-%d", suffix)},
		{day: cycleEnd, key: fmt.Sprintf("snapshot-end-%d", suffix)},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO salary_receipts (amount_vnd, received_on, idempotency_key, request_fingerprint)
			VALUES (1, $1, $2, 'pinned-by-test')`, pinned.day, pinned.key); err != nil {
			t.Fatalf("pinning the salary cycle: %v", err)
		}
		key := pinned.key
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `DELETE FROM salary_receipts WHERE idempotency_key = $1`, key); err != nil {
				t.Errorf("unpinning the salary cycle: %v", err)
			}
		})
	}

	before, _, _, err := store.SnapshotData(ctx, now)
	if err != nil {
		t.Fatalf("reading baseline budget data: %v", err)
	}

	accounts := ledger.NewService(ledger.NewPostgresStore(pool))
	cash, err := accounts.CreateAccount(ctx, ledger.AccountInput{
		Name: fmt.Sprintf("budget-cash-%d", suffix), Kind: "CASH", OpeningBalanceVND: 1_000_000,
		IdempotencyKey: stringPtr(fmt.Sprintf("budget-cash-key-%d", suffix)),
	})
	if err != nil {
		t.Fatalf("creating cash account: %v", err)
	}
	// the credit limit is borrowing room, so it must not enter cash on hand
	creditLimit := int64(5_000_000)
	if _, err := accounts.CreateAccount(ctx, ledger.AccountInput{
		Name: fmt.Sprintf("budget-credit-%d", suffix), Kind: "CREDIT", CreditLimitVND: &creditLimit,
		IdempotencyKey: stringPtr(fmt.Sprintf("budget-credit-key-%d", suffix)),
	}); err != nil {
		t.Fatalf("creating credit account: %v", err)
	}

	occurredAt := time.Now().UTC().Truncate(time.Microsecond)
	spend := func(kind ledger.Type, amount int64, tag string) {
		t.Helper()
		if _, err := accounts.CreateTransaction(ctx, ledger.TransactionInput{
			AccountID: cash.ID, Type: kind, AmountVND: amount, OccurredAt: occurredAt,
			IdempotencyKey: stringPtr(fmt.Sprintf("budget-tx-%s-%d", tag, suffix)),
		}); err != nil {
			t.Fatalf("creating %s transaction: %v", kind, err)
		}
	}
	spend(ledger.TypeExpense, 200_000, "expense")
	spend(ledger.TypeIncome, 100_000, "income")
	spend(ledger.TypeRepayment, 50_000, "repayment")

	obligations := obligation.NewService(obligation.NewPostgresStore(pool))
	occurrenceFor := func(name, tag string, dueDate time.Time, amountVND int64, isEstimate bool) obligation.Occurrence {
		t.Helper()
		record, err := obligations.CreateObligation(ctx, name, fmt.Sprintf("budget-obligation-%s-%d", tag, suffix))
		if err != nil {
			t.Fatalf("creating obligation: %v", err)
		}
		var occurrence obligation.Occurrence
		if isEstimate {
			occurrence, err = obligations.CreateEstimatedOccurrence(ctx, record.ID, dueDate, amountVND,
				fmt.Sprintf("budget-occurrence-%s-%d", tag, suffix))
		} else {
			occurrence, err = obligations.CreateOccurrence(ctx, record.ID, dueDate, amountVND,
				fmt.Sprintf("budget-occurrence-%s-%d", tag, suffix))
		}
		if err != nil {
			t.Fatalf("creating occurrence: %v", err)
		}
		return occurrence
	}

	booked := occurrenceFor(fmt.Sprintf("budget-rent-%d", suffix), "rent", today.AddDate(0, 0, 3), 500_000, false)
	if _, err := obligations.RecordPayment(ctx, booked.ID, 100_000, now,
		fmt.Sprintf("budget-payment-%d", suffix)); err != nil {
		t.Fatalf("recording payment: %v", err)
	}
	estimated := occurrenceFor(fmt.Sprintf("budget-power-%d", suffix), "power", today.AddDate(0, 0, 5), 250_000, true)
	beyond := occurrenceFor(fmt.Sprintf("budget-next-%d", suffix), "next", cycleEnd.AddDate(0, 0, 10), 900_000, false)

	after, receipts, _, err := store.SnapshotData(ctx, now)
	if err != nil {
		t.Fatalf("reading budget data: %v", err)
	}
	// the snapshot carries the cycle it filtered by, built from its own receipts
	if cycle := CycleFor(now, receipts); cycle.Start != today.Format(dateLayout) || cycle.End != cycleEnd.Format(dateLayout) {
		t.Fatalf("snapshot cycle = %+v, want %s..%s", cycle, today.Format(dateLayout), cycleEnd.Format(dateLayout))
	}

	// 1.000.000 opening + 100.000 income - 200.000 expense - 50.000 repayment
	if delta := after.CashOnHandVND - before.CashOnHandVND; delta != 850_000 {
		t.Fatalf("cash on hand delta = %d, want 850000", delta)
	}
	// only expense and fee are cycle spending; income and repayment are not
	if delta := after.SpentInCycleVND - before.SpentInCycleVND; delta != 200_000 {
		t.Fatalf("cycle spending delta = %d, want 200000", delta)
	}

	reserved := map[string]Occurrence{}
	for _, occurrence := range after.Occurrences {
		reserved[occurrence.ID] = occurrence
	}
	bookedReserve, ok := reserved[booked.ID]
	if !ok || bookedReserve.RemainingVND != 400_000 || bookedReserve.IsEstimate {
		t.Fatalf("booked reserve = %+v (present %v), want 400000 confirmed", bookedReserve, ok)
	}
	estimatedReserve, ok := reserved[estimated.ID]
	if !ok || estimatedReserve.RemainingVND != 250_000 || !estimatedReserve.IsEstimate {
		t.Fatalf("estimated reserve = %+v (present %v), want 250000 estimated", estimatedReserve, ok)
	}
	if _, ok := reserved[beyond.ID]; ok {
		t.Fatal("an obligation due after the cycle must not be reserved")
	}
}

func stringPtr(value string) *string { return &value }
