package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"grummans.finance.backend/internal/obligation"
)

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set, skipping PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresStoreTransferDoesNotDoubleCount(t *testing.T) {
	pool := openPool(t)
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	wallet := mustAccount(t, service, fmt.Sprintf("wallet-%d", suffix), "CASH", 5_000_000, nil)
	bank := mustAccount(t, service, fmt.Sprintf("bank-%d", suffix), "BANK", 0, nil)
	occurredAt := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	transferKey := fmt.Sprintf("transfer-%d", suffix)

	postings, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      wallet.ID,
		ToAccountID:    bank.ID,
		Type:           TypeTransfer,
		AmountVND:      1_000_000,
		OccurredAt:     occurredAt,
		IdempotencyKey: &transferKey,
	})
	if err != nil {
		t.Fatalf("creating transfer: %v", err)
	}
	if len(postings) != 2 {
		t.Fatalf("transfer postings = %d, want 2", len(postings))
	}
	for _, posting := range postings {
		if posting.Type != string(TypeTransfer) || posting.AmountVND != 1_000_000 || posting.TransferID == nil {
			t.Fatalf("unexpected transfer posting: %+v", posting)
		}
	}
	if postings[0].Direction == postings[1].Direction {
		t.Fatalf("transfer postings share direction %s", postings[0].Direction)
	}

	retried, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      wallet.ID,
		ToAccountID:    bank.ID,
		Type:           TypeTransfer,
		AmountVND:      1_000_000,
		OccurredAt:     occurredAt,
		IdempotencyKey: &transferKey,
	})
	if err != nil {
		t.Fatalf("retrying transfer: %v", err)
	}
	if len(retried) != 2 || *retried[0].TransferID != *postings[0].TransferID {
		t.Fatalf("retry duplicated the transfer: %+v", retried)
	}

	expenseKey := fmt.Sprintf("expense-%d", suffix)
	expense, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      wallet.ID,
		Type:           TypeExpense,
		AmountVND:      250_000,
		OccurredAt:     occurredAt.Add(time.Hour),
		IdempotencyKey: &expenseKey,
	})
	if err != nil {
		t.Fatalf("creating expense: %v", err)
	}
	if len(expense) != 1 || expense[0].Direction != "OUT" {
		t.Fatalf("unexpected expense: %+v", expense)
	}
	if _, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      wallet.ID,
		Type:           TypeExpense,
		AmountVND:      250_000,
		OccurredAt:     occurredAt.Add(time.Hour),
		IdempotencyKey: &expenseKey,
	}); err != nil {
		t.Fatalf("retrying expense: %v", err)
	}

	// assert the stored postings themselves, not a synthetic sum: a transfer must
	// stay one OUT posting on the source account and one IN posting on the target
	type posting struct {
		typeName  string
		direction string
		amount    int64
		shareKey  bool
	}
	rows, err := pool.Query(ctx, `
		SELECT type, direction, amount_vnd, transfer_id IS NOT NULL
		FROM transactions WHERE account_id = $1 ORDER BY type, direction`, wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored := []posting{}
	for rows.Next() {
		var item posting
		if err := rows.Scan(&item.typeName, &item.direction, &item.amount, &item.shareKey); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, item)
	}
	rows.Close()
	if len(stored) != 2 {
		t.Fatalf("postings on the source account = %d, want 2 (one expense, one transfer OUT)", len(stored))
	}
	if stored[0].typeName != "EXPENSE" || stored[0].direction != "OUT" || stored[0].amount != 250_000 || stored[0].shareKey {
		t.Fatalf("expense posting = %+v, want OUT expense 250000 without transfer_id", stored[0])
	}
	if stored[1].typeName != "TRANSFER" || stored[1].direction != "OUT" || stored[1].amount != 1_000_000 || !stored[1].shareKey {
		t.Fatalf("transfer posting = %+v, want OUT transfer 1000000 with transfer_id", stored[1])
	}
	var transferRows, expenseRows int64
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM transactions WHERE idempotency_key = $1 AND type = 'EXPENSE'),
		       (SELECT COUNT(*) FROM transactions WHERE idempotency_key = $1)`,
		expenseKey).Scan(&expenseRows, &transferRows); err != nil {
		t.Fatal(err)
	}
	if transferRows != 1 {
		t.Fatalf("rows storing the expense idempotency key = %d, want 1 (retry must not duplicate)", transferRows)
	}
	if expenseRows != 1 {
		t.Fatalf("expense rows for the idempotency key = %d, want 1", expenseRows)
	}

	category, err := service.CreateCategory(ctx, fmt.Sprintf("food-%d", suffix), fmt.Sprintf("category-food-%d", suffix))
	if err != nil {
		t.Fatalf("creating category: %v", err)
	}
	updated, err := service.UpdateTransactionCategory(ctx, expense[0].ID, &category.ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("updating category: %v", err)
	}
	if updated.CategoryID == nil || *updated.CategoryID != category.ID {
		t.Fatalf("category not stored: %+v", updated)
	}

	filtered, err := service.ListTransactions(ctx, Filter{
		CategoryID: &category.ID,
		From:       occurredAt,
		HasFrom:    true,
		To:         occurredAt,
		HasTo:      true,
		Limit:      50,
	})
	if err != nil {
		t.Fatalf("listing transactions: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != expense[0].ID {
		t.Fatalf("category+date filter returned %+v", filtered)
	}
}

func TestPostgresStoreLinkPaymentTransactionIsManualAndUnique(t *testing.T) {
	pool := openPool(t)
	service := NewService(NewPostgresStore(pool))
	occurrences := obligation.NewService(obligation.NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	account := mustAccount(t, service, fmt.Sprintf("link-wallet-%d", suffix), "BANK", 0, nil)
	key := fmt.Sprintf("link-expense-%d", suffix)
	expense, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      account.ID,
		Type:           TypeExpense,
		AmountVND:      100_000,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("creating expense: %v", err)
	}
	otherKey := fmt.Sprintf("link-expense-other-%d", suffix)
	otherExpense, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      account.ID,
		Type:           TypeExpense,
		AmountVND:      100_000,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: &otherKey,
	})
	if err != nil {
		t.Fatalf("creating second expense: %v", err)
	}

	obligationRecord, err := occurrences.CreateObligation(ctx, fmt.Sprintf("link-rent-%d", suffix), fmt.Sprintf("obligation-key-link-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := occurrences.CreateOccurrence(ctx, obligationRecord.ID, time.Now().UTC(), 100_000, fmt.Sprintf("occurrence-key-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	paymentKey := fmt.Sprintf("link-payment-%d", suffix)
	if _, err := occurrences.RecordPayment(ctx, occurrence.ID, 100_000, time.Now().UTC(), paymentKey); err != nil {
		t.Fatal(err)
	}
	var paymentID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM payments WHERE idempotency_key = $1`, paymentKey).Scan(&paymentID); err != nil {
		t.Fatal(err)
	}

	replayKey := fmt.Sprintf("link-replay-%d", suffix)
	linked, err := service.LinkPaymentTransaction(ctx, paymentID, expense[0].ID, replayKey)
	if err != nil {
		t.Fatalf("linking payment: %v", err)
	}
	if linked.TransactionID == nil || *linked.TransactionID != expense[0].ID {
		t.Fatalf("unexpected link: %+v", linked)
	}

	// linking never changes the confirmed payment amount
	stored, err := occurrences.GetOccurrence(ctx, occurrence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RemainingAmountVND() != 0 {
		t.Fatalf("link must not change payment totals, remaining = %d", stored.RemainingAmountVND())
	}

	if _, err := service.LinkPaymentTransaction(ctx, paymentID, otherExpense[0].ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("relinking a payment error = %v, want ErrAlreadyLinked", err)
	}
	if _, err := service.LinkPaymentTransaction(ctx, "00000000-0000-0000-0000-000000000000", expense[0].ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("linking a missing payment error = %v, want ErrNotFound", err)
	}

	if repeated, err := service.LinkPaymentTransaction(ctx, paymentID, expense[0].ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); err != nil || repeated.TransactionID == nil || *repeated.TransactionID != expense[0].ID {
		t.Fatalf("repeating the same link must succeed, got %+v err %v", repeated, err)
	}

	// an uppercase spelling of the same uuid is the same link, not a conflict
	if upper, err := service.LinkPaymentTransaction(ctx, paymentID, strings.ToUpper(expense[0].ID), fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); err != nil || upper.TransactionID == nil {
		t.Fatalf("repeating the link with an uppercase uuid must succeed, got %+v err %v", upper, err)
	}

	// a retried link with the same key stays harmless, and the key cannot be reused elsewhere
	if replayed, err := service.LinkPaymentTransaction(ctx, paymentID, expense[0].ID, replayKey); err != nil || replayed.TransactionID == nil {
		t.Fatalf("retrying the identical link with the same key must succeed, got %+v err %v", replayed, err)
	}
	if _, err := service.LinkPaymentTransaction(ctx, paymentID, otherExpense[0].ID, replayKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the link key for another transaction error = %v, want ErrIdempotencyKeyReused", err)
	}

	// a second payment may not take over a transaction that is already linked
	secondKey := fmt.Sprintf("link-payment-second-%d", suffix)
	secondOccurrence, err := occurrences.CreateOccurrence(ctx, obligationRecord.ID, time.Now().UTC(), 100_000, fmt.Sprintf("occurrence-key-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := occurrences.RecordPayment(ctx, secondOccurrence.ID, 100_000, time.Now().UTC(), secondKey); err != nil {
		t.Fatal(err)
	}
	var secondPaymentID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM payments WHERE idempotency_key = $1`, secondKey).Scan(&secondPaymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.LinkPaymentTransaction(ctx, secondPaymentID, expense[0].ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("second payment taking over a linked transaction error = %v, want ErrAlreadyLinked", err)
	}
}

func mustAccount(t *testing.T, service *Service, name, kind string, openingBalance int64, creditLimit *int64) Account {
	t.Helper()
	key := fmt.Sprintf("account-%s-%d", name, time.Now().UnixNano())
	account, err := service.CreateAccount(context.Background(), AccountInput{
		Name:              name,
		Kind:              kind,
		OpeningBalanceVND: openingBalance,
		CreditLimitVND:    creditLimit,
		IdempotencyKey:    &key,
	})
	if err != nil {
		t.Fatalf("creating account: %v", err)
	}
	return account
}

func TestPostgresStoreTransferRollsBackOnUnknownAccount(t *testing.T) {
	pool := openPool(t)
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	wallet := mustAccount(t, service, fmt.Sprintf("rollback-wallet-%d", suffix), "CASH", 0, nil)
	key := fmt.Sprintf("rollback-transfer-%d", suffix)

	_, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      wallet.ID,
		ToAccountID:    "00000000-0000-0000-0000-000000000000",
		Type:           TypeTransfer,
		AmountVND:      500_000,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: &key,
	})
	if !errors.Is(err, ErrReferenceNotFound) {
		t.Fatalf("transfer to unknown account error = %v, want ErrReferenceNotFound", err)
	}

	var postings int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM transactions WHERE idempotency_key = $1`, key).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if postings != 0 {
		t.Fatalf("rollback left %d transfer posting(s)", postings)
	}
}

func TestPostgresStoreRepaymentAndFeesAreNotExpense(t *testing.T) {
	pool := openPool(t)
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	card := mustAccount(t, service, fmt.Sprintf("card-%d", suffix), "CREDIT", 0, ptrInt64(20_000_000))
	bank := mustAccount(t, service, fmt.Sprintf("repay-bank-%d", suffix), "BANK", 0, nil)

	for index, kind := range []Type{TypeRepayment, TypeFee, TypeExpense} {
		key := fmt.Sprintf("repay-%d-%d", suffix, index)
		if _, err := service.CreateTransaction(ctx, TransactionInput{
			AccountID:      bank.ID,
			Type:           kind,
			AmountVND:      100_000,
			OccurredAt:     time.Now().UTC(),
			IdempotencyKey: &key,
		}); err != nil {
			t.Fatalf("creating %s: %v", kind, err)
		}
	}
	if _, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      card.ID,
		Type:           TypeExpense,
		AmountVND:      100_000,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: ptrString(fmt.Sprintf("repay-card-%d", suffix)),
	}); err != nil {
		t.Fatalf("creating card expense: %v", err)
	}

	var expenseTotal, repaymentRows int64
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_vnd) FILTER (WHERE type = 'EXPENSE'), 0)::bigint,
		       COUNT(*) FILTER (WHERE type = 'REPAYMENT')
		FROM transactions
		WHERE account_id = $1`, bank.ID).Scan(&expenseTotal, &repaymentRows)
	if err != nil {
		t.Fatal(err)
	}
	if expenseTotal != 100_000 {
		t.Fatalf("bank expense total = %d, want 100000 (repayment and fee excluded)", expenseTotal)
	}
	if repaymentRows != 1 {
		t.Fatalf("repayment rows = %d, want 1", repaymentRows)
	}
}

func TestPostgresStoreListTransactionsUsesHoChiMinhDayBoundary(t *testing.T) {
	pool := openPool(t)
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	wallet := mustAccount(t, service, fmt.Sprintf("tz-wallet-%d", suffix), "CASH", 0, nil)
	category, err := service.CreateCategory(ctx, fmt.Sprintf("tz-food-%d", suffix), fmt.Sprintf("category-tz-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}

	// 17:30Z on 24 Nov is 00:30 on 25 Nov in Asia/Ho_Chi_Minh; 16:59Z is 23:59 on 24 Nov.
	lateUTC := time.Date(2026, 11, 24, 17, 30, 0, 0, time.UTC)
	earlyUTC := time.Date(2026, 11, 24, 16, 59, 0, 0, time.UTC)
	for index, occurredAt := range []time.Time{lateUTC, earlyUTC} {
		key := fmt.Sprintf("tz-%d-%d", suffix, index)
		if _, err := service.CreateTransaction(ctx, TransactionInput{
			AccountID:      wallet.ID,
			Type:           TypeExpense,
			AmountVND:      1_000,
			OccurredAt:     occurredAt,
			CategoryID:     &category.ID,
			IdempotencyKey: &key,
		}); err != nil {
			t.Fatalf("creating timezone expense: %v", err)
		}
	}

	day := func(year int, month time.Month, date int) time.Time {
		return time.Date(year, month, date, 0, 0, 0, 0, time.UTC)
	}
	list := func(from, to time.Time) []Transaction {
		t.Helper()
		result, err := service.ListTransactions(ctx, Filter{
			CategoryID: &category.ID,
			From:       from,
			HasFrom:    true,
			To:         to,
			HasTo:      true,
			Limit:      50,
		})
		if err != nil {
			t.Fatalf("listing transactions: %v", err)
		}
		return result
	}

	if got := list(day(2026, 11, 24), day(2026, 11, 24)); len(got) != 1 || !got[0].OccurredAt.Equal(earlyUTC) {
		t.Fatalf("24 Nov local day returned %+v, want only the 16:59Z posting", got)
	}
	if got := list(day(2026, 11, 25), day(2026, 11, 25)); len(got) != 1 || !got[0].OccurredAt.Equal(lateUTC) {
		t.Fatalf("25 Nov local day returned %+v, want only the 17:30Z posting", got)
	}
}

func ptrInt64(value int64) *int64    { return &value }
func ptrString(value string) *string { return &value }

func TestPostgresStoreAccountCreateIsIdempotent(t *testing.T) {
	service := NewService(NewPostgresStore(openPool(t)))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	key := fmt.Sprintf("account-key-%d", suffix)
	input := AccountInput{
		Name:              fmt.Sprintf("idem-wallet-%d", suffix),
		Kind:              "CASH",
		OpeningBalanceVND: 2_000_000,
		IdempotencyKey:    &key,
	}

	first, err := service.CreateAccount(ctx, input)
	if err != nil {
		t.Fatalf("creating account: %v", err)
	}
	retried, err := service.CreateAccount(ctx, input)
	if err != nil {
		t.Fatalf("retrying account: %v", err)
	}
	if retried.ID != first.ID {
		t.Fatalf("retry created a second account: %s vs %s", retried.ID, first.ID)
	}

	changed := input
	changed.OpeningBalanceVND = 3_000_000
	if _, err := service.CreateAccount(ctx, changed); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("retry with a different opening balance error = %v, want ErrIdempotencyKeyReused", err)
	}
}

func TestPostgresStoreTransactionRetryRejectsChangedFingerprint(t *testing.T) {
	service := NewService(NewPostgresStore(openPool(t)))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	account := mustAccount(t, service, fmt.Sprintf("fingerprint-%d", suffix), "CASH", 0, nil)
	key := fmt.Sprintf("fingerprint-key-%d", suffix)
	occurredAt := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	note := "rent"
	input := TransactionInput{
		AccountID:      account.ID,
		Type:           TypeExpense,
		AmountVND:      100_000,
		OccurredAt:     occurredAt,
		Note:           &note,
		IdempotencyKey: &key,
	}
	if _, err := service.CreateTransaction(ctx, input); err != nil {
		t.Fatalf("creating transaction: %v", err)
	}
	if _, err := service.CreateTransaction(ctx, input); err != nil {
		t.Fatalf("identical retry must succeed: %v", err)
	}

	movedDate := input
	movedDate.OccurredAt = occurredAt.AddDate(0, 0, 1)
	if _, err := service.CreateTransaction(ctx, movedDate); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("retry with another date error = %v, want ErrIdempotencyKeyReused", err)
	}
	otherNote := "electricity"
	changedNote := input
	changedNote.Note = &otherNote
	if _, err := service.CreateTransaction(ctx, changedNote); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("retry with another note error = %v, want ErrIdempotencyKeyReused", err)
	}
}

func TestPostgresStoreTransactionRetrySurvivesCategoryCorrection(t *testing.T) {
	service := NewService(NewPostgresStore(openPool(t)))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	account := mustAccount(t, service, fmt.Sprintf("replay-%d", suffix), "CASH", 0, nil)
	category, err := service.CreateCategory(ctx, fmt.Sprintf("original-%d", suffix), fmt.Sprintf("category-original-%d", suffix))
	if err != nil {
		t.Fatalf("creating category: %v", err)
	}
	key := fmt.Sprintf("replay-key-%d", suffix)
	input := TransactionInput{
		AccountID:      account.ID,
		Type:           TypeExpense,
		AmountVND:      75_000,
		OccurredAt:     time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC),
		CategoryID:     &category.ID,
		IdempotencyKey: &key,
	}
	created, err := service.CreateTransaction(ctx, input)
	if err != nil {
		t.Fatalf("creating transaction: %v", err)
	}

	// correcting the category afterwards must not poison the retry: the fingerprint
	// is derived from the original request, not from the mutable stored row
	correction, err := service.CreateCategory(ctx, fmt.Sprintf("correction-%d", suffix), fmt.Sprintf("category-correction-%d", suffix))
	if err != nil {
		t.Fatalf("creating correction category: %v", err)
	}
	if _, err := service.UpdateTransactionCategory(ctx, created[0].ID, &correction.ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("correcting category: %v", err)
	}
	replayed, err := service.CreateTransaction(ctx, input)
	if err != nil {
		t.Fatalf("retry after category correction: %v", err)
	}
	if len(replayed) != 1 || replayed[0].ID != created[0].ID {
		t.Fatalf("retry after category correction returned %+v", replayed)
	}

	// a different request under the same key must still be refused
	changed := input
	changed.AmountVND = 80_000
	if _, err := service.CreateTransaction(ctx, changed); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("changed request error = %v, want ErrIdempotencyKeyReused", err)
	}
}

func TestPostgresStoreLinkRejectsIneligibleTransaction(t *testing.T) {
	pool := openPool(t)
	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	obligations := obligation.NewService(obligation.NewPostgresStore(pool))
	wallet := mustAccount(t, service, fmt.Sprintf("link-eligibility-%d", suffix), "CASH", 0, nil)
	card := mustAccount(t, service, fmt.Sprintf("link-card-%d", suffix), "CREDIT", 0, func() *int64 {
		limit := int64(5_000_000)
		return &limit
	}())

	obligationRecord, err := obligations.CreateObligation(ctx, fmt.Sprintf("link-eligibility-%d", suffix), fmt.Sprintf("obligation-key-eligibility-%d", suffix))
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := obligations.CreateOccurrence(ctx, obligationRecord.ID, time.Now().UTC(), 100_000, fmt.Sprintf("occurrence-key-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("link-eligibility-payment-%d", suffix)
	if _, err := obligations.RecordPayment(ctx, occurrence.ID, 100_000, time.Now().UTC(), key); err != nil {
		t.Fatal(err)
	}
	var paymentID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM payments WHERE idempotency_key = $1`, key).Scan(&paymentID); err != nil {
		t.Fatal(err)
	}

	incomeKey := fmt.Sprintf("link-eligibility-income-%d", suffix)
	income, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      wallet.ID,
		Type:           TypeIncome,
		AmountVND:      100_000,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: &incomeKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LinkPaymentTransaction(ctx, paymentID, income[0].ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("linking a payment to an income posting error = %v, want ErrInvalidChange", err)
	}

	cardKey := fmt.Sprintf("link-eligibility-card-%d", suffix)
	cardSpend, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      card.ID,
		Type:           TypeExpense,
		AmountVND:      100_000,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: &cardKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LinkPaymentTransaction(ctx, paymentID, cardSpend[0].ID, fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("linking a payment to a credit-card posting error = %v, want ErrInvalidChange", err)
	}
	if _, err := service.LinkPaymentTransaction(ctx, paymentID, "00000000-0000-0000-0000-000000000001", fmt.Sprintf("auto-key-%d", time.Now().UnixNano())); !errors.Is(err, ErrReferenceNotFound) {
		t.Fatalf("linking to a missing transaction error = %v, want ErrReferenceNotFound", err)
	}
}

func TestPostgresStoreCategoryAndPatchMutationsAreIdempotent(t *testing.T) {
	service := NewService(NewPostgresStore(openPool(t)))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	categoryKey := fmt.Sprintf("category-key-%d", suffix)

	name := fmt.Sprintf("idem-category-%d", suffix)
	first, err := service.CreateCategory(ctx, name, categoryKey)
	if err != nil {
		t.Fatalf("creating category: %v", err)
	}
	retried, err := service.CreateCategory(ctx, name, categoryKey)
	if err != nil || retried.ID != first.ID {
		t.Fatalf("retrying category = %+v, %v; want the original category", retried, err)
	}
	if _, err := service.CreateCategory(ctx, fmt.Sprintf("other-category-%d", suffix), categoryKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reused category key error = %v, want ErrIdempotencyKeyReused", err)
	}

	account := mustAccount(t, service, fmt.Sprintf("idem-account-%d", suffix), "CASH", 1_000_000, nil)
	createdList, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      account.ID,
		Type:           TypeExpense,
		AmountVND:      10_000,
		OccurredAt:     time.Now(),
		IdempotencyKey: ptrString(fmt.Sprintf("txn-key-%d", suffix)),
	})
	if err != nil {
		t.Fatalf("creating transaction: %v", err)
	}
	created := createdList[0]

	patchKey := fmt.Sprintf("patch-key-%d", suffix)
	updated, err := service.UpdateTransactionCategory(ctx, created.ID, &first.ID, patchKey)
	if err != nil || updated.CategoryID == nil || *updated.CategoryID != first.ID {
		t.Fatalf("patching category = %+v, %v", updated, err)
	}
	replayed, err := service.UpdateTransactionCategory(ctx, created.ID, &first.ID, patchKey)
	if err != nil || replayed.CategoryID == nil || *replayed.CategoryID != first.ID {
		t.Fatalf("replaying category patch = %+v, %v", replayed, err)
	}
	second, err := service.CreateCategory(ctx, fmt.Sprintf("idem-second-%d", suffix), fmt.Sprintf("category-second-%d", suffix))
	if err != nil {
		t.Fatalf("creating second category: %v", err)
	}
	if _, err := service.UpdateTransactionCategory(ctx, created.ID, &second.ID, patchKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reused patch key error = %v, want ErrIdempotencyKeyReused", err)
	}
}

func TestPostgresStoreReplayedPatchDoesNotOverwriteLaterEdit(t *testing.T) {
	service := NewService(NewPostgresStore(openPool(t)))
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	first, err := service.CreateCategory(ctx, fmt.Sprintf("seq-first-%d", suffix), fmt.Sprintf("seq-first-key-%d", suffix))
	if err != nil {
		t.Fatalf("creating first category: %v", err)
	}
	second, err := service.CreateCategory(ctx, fmt.Sprintf("seq-second-%d", suffix), fmt.Sprintf("seq-second-key-%d", suffix))
	if err != nil {
		t.Fatalf("creating second category: %v", err)
	}
	account := mustAccount(t, service, fmt.Sprintf("seq-account-%d", suffix), "CASH", 500_000, nil)
	createdList, err := service.CreateTransaction(ctx, TransactionInput{
		AccountID:      account.ID,
		Type:           TypeExpense,
		AmountVND:      20_000,
		OccurredAt:     time.Now(),
		IdempotencyKey: ptrString(fmt.Sprintf("seq-txn-key-%d", suffix)),
	})
	if err != nil {
		t.Fatalf("creating transaction: %v", err)
	}
	transaction := createdList[0]

	keyA := fmt.Sprintf("seq-patch-a-%d", suffix)
	if _, err := service.UpdateTransactionCategory(ctx, transaction.ID, &first.ID, keyA); err != nil {
		t.Fatalf("applying the first patch: %v", err)
	}
	if _, err := service.UpdateTransactionCategory(ctx, transaction.ID, &second.ID, fmt.Sprintf("seq-patch-b-%d", suffix)); err != nil {
		t.Fatalf("applying the second patch: %v", err)
	}
	// retrying the first request must not resurrect the first category
	if _, err := service.UpdateTransactionCategory(ctx, transaction.ID, &first.ID, keyA); err != nil {
		t.Fatalf("replaying the first patch: %v", err)
	}
	stored, err := service.GetTransaction(ctx, transaction.ID)
	if err != nil {
		t.Fatalf("reading transaction: %v", err)
	}
	if stored.CategoryID == nil || *stored.CategoryID != second.ID {
		t.Fatalf("replayed patch overwrote the later edit: %+v, want category %s", stored.CategoryID, second.ID)
	}
}
