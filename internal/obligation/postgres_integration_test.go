//go:build integration

package obligation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"grummans.finance.backend/internal/ledger"
	"sync"
)

func TestPostgresStoreRecordPayment(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer pool.Close()

	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	obligation, err := service.CreateObligation(ctx, fmt.Sprintf("integration-rent-%d", suffix), fmt.Sprintf("obligation-key-%d", suffix))
	if err != nil {
		t.Fatalf("creating obligation: %v", err)
	}
	occurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 100, fmt.Sprintf("occurrence-key-dec-%d", suffix))
	if err != nil {
		t.Fatalf("creating occurrence: %v", err)
	}

	firstKey := fmt.Sprintf("integration-%d-a", suffix)
	firstPaidAt := time.Now().UTC().Truncate(time.Microsecond)
	first, err := service.RecordPayment(ctx, occurrence.ID, 40, firstPaidAt, firstKey)
	if err != nil {
		t.Fatalf("recording first payment: %v", err)
	}
	if first.PaidAmountVND != 40 || first.RemainingAmountVND() != 60 {
		t.Fatalf("first payment result = paid %d remaining %d, want 40 and 60", first.PaidAmountVND, first.RemainingAmountVND())
	}

	second, err := service.RecordPayment(ctx, occurrence.ID, 60, time.Now().UTC(), fmt.Sprintf("integration-%d-b", suffix))
	if err != nil {
		t.Fatalf("recording second payment: %v", err)
	}
	if second.PaidAmountVND != 100 || second.RemainingAmountVND() != 0 {
		t.Fatalf("second payment result = paid %d remaining %d, want 100 and 0", second.PaidAmountVND, second.RemainingAmountVND())
	}

	retry, err := service.RecordPayment(ctx, occurrence.ID, 40, firstPaidAt, firstKey)
	if err != nil {
		t.Fatalf("retrying first payment: %v", err)
	}
	if retry.PaidAmountVND != first.PaidAmountVND || retry.RemainingAmountVND() != first.RemainingAmountVND() {
		t.Fatalf("idempotent retry = paid %d remaining %d, want original paid %d remaining %d", retry.PaidAmountVND, retry.RemainingAmountVND(), first.PaidAmountVND, first.RemainingAmountVND())
	}

	// a reused key may not hide a different payment
	if _, err := service.RecordPayment(ctx, occurrence.ID, 30, time.Now().UTC(), firstKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key with another amount error = %v, want ErrIdempotencyKeyReused", err)
	}
	if _, err := service.RecordPayment(ctx, occurrence.ID, 40, firstPaidAt.Add(time.Hour), firstKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key with another paid_at error = %v, want ErrIdempotencyKeyReused", err)
	}
	otherOccurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 100, fmt.Sprintf("occurrence-key-jan-%d", suffix))
	if err != nil {
		t.Fatalf("creating second occurrence: %v", err)
	}
	if _, err := service.RecordPayment(ctx, otherOccurrence.ID, 40, firstPaidAt, firstKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key on another occurrence error = %v, want ErrIdempotencyKeyReused", err)
	}
	_, err = service.RecordPayment(ctx, occurrence.ID, 1, time.Now().UTC(), fmt.Sprintf("integration-%d-c", suffix))
	if !errors.Is(err, ErrOverpayment) {
		t.Fatalf("overpayment error = %v, want %v", err, ErrOverpayment)
	}
}

func TestPostgresStoreRecordPaymentConcurrentIdempotencyKey(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer pool.Close()
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	obligation, err := service.CreateObligation(ctx, fmt.Sprintf("concurrent-rent-%d", suffix), fmt.Sprintf("obligation-key-concurrent-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Now().UTC(), 100, fmt.Sprintf("occurrence-key-%d-%d", suffix, time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("concurrent-%d", suffix)
	sharedPaidAt := time.Now().UTC().Truncate(time.Microsecond)
	start := make(chan struct{})
	results := make(chan Occurrence, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := service.RecordPayment(ctx, occurrence.ID, 40, sharedPaidAt, key)
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent payment: %v", err)
		}
	}
	for result := range results {
		if result.PaidAmountVND != 40 || result.RemainingAmountVND() != 60 {
			t.Fatalf("result = paid %d remaining %d", result.PaidAmountVND, result.RemainingAmountVND())
		}
	}
}

func TestPostgresStoreUpdateAmountDoesNotUndercutConcurrentPayment(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer pool.Close()
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	obligation, err := service.CreateObligation(ctx, fmt.Sprintf("race-rent-%d", suffix), fmt.Sprintf("obligation-key-race-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Now().UTC(), 100, fmt.Sprintf("occurrence-key-%d-%d", suffix, time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var updateErr, paymentErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, updateErr = service.UpdateOccurrenceAmount(ctx, occurrence.ID, 50, fmt.Sprintf("amount-key-%d", suffix))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, paymentErr = service.RecordPayment(ctx, occurrence.ID, 80, time.Now().UTC(), fmt.Sprintf("race-%d", suffix))
	}()
	close(start)
	wg.Wait()
	if updateErr == nil && paymentErr == nil {
		t.Fatal("update and payment both succeeded")
	}
	stored, err := service.GetOccurrence(ctx, occurrence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConfirmedAmountVND < stored.PaidAmountVND {
		t.Fatalf("confirmed %d below paid %d", stored.ConfirmedAmountVND, stored.PaidAmountVND)
	}
}

func TestPostgresStoreObligationAndOccurrenceCreateAreIdempotent(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer pool.Close()

	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	obligationKey := fmt.Sprintf("obligation-idem-%d", suffix)
	first, err := service.CreateObligation(ctx, fmt.Sprintf("idem-rent-%d", suffix), obligationKey)
	if err != nil {
		t.Fatalf("creating obligation: %v", err)
	}
	replayed, err := service.CreateObligation(ctx, fmt.Sprintf("idem-rent-%d", suffix), obligationKey)
	if err != nil {
		t.Fatalf("retrying obligation: %v", err)
	}
	if replayed.ID != first.ID {
		t.Fatalf("retry created a second obligation: %s vs %s", replayed.ID, first.ID)
	}
	if _, err := service.CreateObligation(ctx, fmt.Sprintf("other-rent-%d", suffix), obligationKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("retry with another name error = %v, want ErrIdempotencyKeyReused", err)
	}

	dueDate := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	occurrenceKey := fmt.Sprintf("occurrence-idem-%d", suffix)
	occurrence, err := service.CreateOccurrence(ctx, first.ID, dueDate, 100, occurrenceKey)
	if err != nil {
		t.Fatalf("creating occurrence: %v", err)
	}
	replayedOccurrence, err := service.CreateOccurrence(ctx, first.ID, dueDate, 100, occurrenceKey)
	if err != nil {
		t.Fatalf("retrying occurrence: %v", err)
	}
	if replayedOccurrence.ID != occurrence.ID {
		t.Fatalf("retry created a second occurrence: %s vs %s", replayedOccurrence.ID, occurrence.ID)
	}
	if _, err := service.CreateOccurrence(ctx, first.ID, dueDate, 200, occurrenceKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("retry with another amount error = %v, want ErrIdempotencyKeyReused", err)
	}
	if _, err := service.CreateOccurrence(ctx, "00000000-0000-0000-0000-000000000000", dueDate, 100, fmt.Sprintf("occurrence-missing-%d", suffix)); !errors.Is(err, ErrReferenceNotFound) {
		t.Fatalf("occurrence for a missing obligation error = %v, want ErrReferenceNotFound", err)
	}
}

func TestPostgresStoreReplayedAmountUpdateDoesNotOverwriteLaterEdit(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer pool.Close()
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	obligation, err := service.CreateObligation(ctx, fmt.Sprintf("replay-rent-%d", suffix), fmt.Sprintf("obligation-key-replay-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Now().UTC(), 100_000, fmt.Sprintf("occurrence-key-replay-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}

	keyA := fmt.Sprintf("amount-replay-a-%d", suffix)
	if _, err := service.UpdateOccurrenceAmount(ctx, occurrence.ID, 80_000, keyA); err != nil {
		t.Fatalf("applying the first amount: %v", err)
	}
	if _, err := service.UpdateOccurrenceAmount(ctx, occurrence.ID, 60_000, fmt.Sprintf("amount-replay-b-%d", suffix)); err != nil {
		t.Fatalf("applying the second amount: %v", err)
	}
	// retrying the first request must not resurrect its amount
	if _, err := service.UpdateOccurrenceAmount(ctx, occurrence.ID, 80_000, keyA); err != nil {
		t.Fatalf("replaying the first amount: %v", err)
	}
	stored, err := service.GetOccurrence(ctx, occurrence.ID)
	if err != nil {
		t.Fatalf("reading occurrence: %v", err)
	}
	if stored.ConfirmedAmountVND != 60_000 {
		t.Fatalf("replayed amount = %d, want the later edit 60000", stored.ConfirmedAmountVND)
	}
}

func TestPostgresStoreListPayments(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer pool.Close()
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	obligation, err := service.CreateObligation(ctx, fmt.Sprintf("payments-rent-%d", suffix), fmt.Sprintf("obligation-key-payments-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Now().UTC(), 100_000, fmt.Sprintf("occurrence-key-payments-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	firstAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if _, err := service.RecordPayment(ctx, occurrence.ID, 40_000, firstAt, fmt.Sprintf("payment-key-a-%d", suffix)); err != nil {
		t.Fatalf("recording the first payment: %v", err)
	}
	secondAt := time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC)
	if _, err := service.RecordPayment(ctx, occurrence.ID, 25_000, secondAt, fmt.Sprintf("payment-key-b-%d", suffix)); err != nil {
		t.Fatalf("recording the second payment: %v", err)
	}

	payments, err := service.ListPayments(ctx, occurrence.ID)
	if err != nil {
		t.Fatalf("listing payments: %v", err)
	}
	if len(payments) != 2 {
		t.Fatalf("payments = %d, want 2", len(payments))
	}
	// newest first, so the UI shows the latest payment on top
	if payments[0].AmountVND != 25_000 || payments[1].AmountVND != 40_000 {
		t.Fatalf("order = %d then %d, want 25000 then 40000", payments[0].AmountVND, payments[1].AmountVND)
	}
	if payments[0].TransactionID != nil || payments[0].ReversedAt != nil {
		t.Fatalf("a fresh payment must be neither linked nor reversed: %+v", payments[0])
	}
	if !payments[1].PaidAt.Equal(firstAt) {
		t.Fatalf("paid_at = %s, want %s", payments[1].PaidAt, firstAt)
	}

	// a reversed payment stays readable so the history is never rewritten
	reversedAt := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE payments SET reversed_at = $2 WHERE id = $1`, payments[1].ID, reversedAt); err != nil {
		t.Fatalf("reversing a payment: %v", err)
	}
	afterReverse, err := service.ListPayments(ctx, occurrence.ID)
	if err != nil {
		t.Fatalf("listing payments after a reversal: %v", err)
	}
	if len(afterReverse) != 2 || afterReverse[1].ReversedAt == nil {
		t.Fatalf("a reversed payment must stay listed, got %+v", afterReverse)
	}
	if afterReverse[1].TransactionID != nil {
		t.Fatalf("a reversed payment must still report its link state: %+v", afterReverse[1])
	}

	// a linked payment must report the transaction it was matched to
	accounts := ledger.NewService(ledger.NewPostgresStore(pool))
	cash, err := accounts.CreateAccount(ctx, ledger.AccountInput{
		Name:              fmt.Sprintf("payments-account-%d", suffix),
		Kind:              "CASH",
		OpeningBalanceVND: 500_000,
		IdempotencyKey:    ptrString(fmt.Sprintf("account-key-payments-%d", suffix)),
	})
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	spend, err := accounts.CreateTransaction(ctx, ledger.TransactionInput{
		AccountID:      cash.ID,
		Type:           ledger.TypeExpense,
		AmountVND:      25_000,
		OccurredAt:     secondAt,
		IdempotencyKey: ptrString(fmt.Sprintf("transaction-key-payments-%d", suffix)),
	})
	if err != nil {
		t.Fatalf("creating the transaction: %v", err)
	}
	if _, err := accounts.LinkPaymentTransaction(ctx, payments[0].ID, spend[0].ID, fmt.Sprintf("link-key-payments-%d", suffix)); err != nil {
		t.Fatalf("linking the payment: %v", err)
	}
	linked, err := service.ListPayments(ctx, occurrence.ID)
	if err != nil {
		t.Fatalf("listing payments after linking: %v", err)
	}
	if linked[0].TransactionID == nil || *linked[0].TransactionID != spend[0].ID {
		t.Fatalf("linked payment transaction = %v, want %s", linked[0].TransactionID, spend[0].ID)
	}

	// an occurrence with no payments is an empty list, not a 404
	emptyOccurrence, err := service.CreateOccurrence(ctx, obligation.ID, time.Now().UTC(), 10_000, fmt.Sprintf("occurrence-key-empty-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := service.ListPayments(ctx, emptyOccurrence.ID)
	if err != nil || len(empty) != 0 {
		t.Fatalf("payments of an unpaid occurrence = %d, %v; want 0, nil", len(empty), err)
	}

	// a missing occurrence must not look like an occurrence without payments
	if _, err := service.ListPayments(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("listing payments of a missing occurrence error = %v, want ErrNotFound", err)
	}
}

func ptrString(value string) *string { return &value }
