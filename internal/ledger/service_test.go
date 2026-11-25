package ledger

import (
	"strings"
	"testing"
	"time"
)

func TestDirectionFor(t *testing.T) {
	tests := []struct {
		kind Type
		want string
	}{
		{TypeExpense, "OUT"},
		{TypeFee, "OUT"},
		{TypeRepayment, "OUT"},
		{TypeIncome, "IN"},
		{TypeRefund, "IN"},
		{TypeTransfer, ""},
	}
	for _, tt := range tests {
		if got := DirectionFor(tt.kind); got != tt.want {
			t.Fatalf("DirectionFor(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestNilIfEmpty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Fatal("empty string must map to nil")
	}
	value := nilIfEmpty("food")
	if value == nil || *value != "food" {
		t.Fatalf("nilIfEmpty(food) = %v", value)
	}
}

func TestTransactionFingerprint(t *testing.T) {
	key := "k"
	occurredAt := time.Date(2026, 11, 24, 10, 0, 0, 123456789, time.UTC)
	categoryID := "66666666-6666-6666-6666-666666666666"
	note := "rent"
	expense := TransactionInput{
		AccountID: "aaa", Type: TypeExpense, AmountVND: 100, OccurredAt: occurredAt,
		CategoryID: &categoryID, Note: &note, IdempotencyKey: &key,
	}
	base := transactionFingerprint(expense)

	// the request is compared with itself, not with the mutable stored row:
	// changing the category later must not make the original retry conflict.
	edited := expense
	other := "77777777-7777-7777-7777-777777777777"
	edited.CategoryID = &other
	if transactionFingerprint(edited) == base {
		t.Fatal("a different category must produce a different fingerprint")
	}

	// PostgreSQL stores uuids lowercased and timestamps at microsecond precision,
	// so an identical retry spelled differently must still match.
	restyled := expense
	restyled.AccountID = "AAA"
	restyled.CategoryID = &categoryID
	upper := strings.ToUpper(categoryID)
	restyled.CategoryID = &upper
	restyled.OccurredAt = time.Date(2026, 11, 24, 10, 0, 0, 123456000, time.UTC)
	if transactionFingerprint(restyled) != base {
		t.Fatal("uppercase uuid or extra nanosecond precision must not change the fingerprint")
	}

	changedAmount := expense
	changedAmount.AmountVND = 200
	if transactionFingerprint(changedAmount) == base {
		t.Fatal("a different amount must produce a different fingerprint")
	}
	changedAccount := expense
	changedAccount.AccountID = "bbb"
	if transactionFingerprint(changedAccount) == base {
		t.Fatal("a different account must produce a different fingerprint")
	}
	changedNote := expense
	otherNote := "electricity"
	changedNote.Note = &otherNote
	if transactionFingerprint(changedNote) == base {
		t.Fatal("a different note must produce a different fingerprint")
	}

	transfer := TransactionInput{AccountID: "a", ToAccountID: "b", Type: TypeTransfer, AmountVND: 500, OccurredAt: occurredAt, IdempotencyKey: &key}
	swapped := TransactionInput{AccountID: "b", ToAccountID: "a", Type: TypeTransfer, AmountVND: 500, OccurredAt: occurredAt, IdempotencyKey: &key}
	if transactionFingerprint(transfer) == transactionFingerprint(swapped) {
		t.Fatal("a swapped transfer must not match the original request")
	}
}

func TestAccountFingerprint(t *testing.T) {
	key := "k"
	account := AccountInput{Name: "wallet", Kind: "CASH", OpeningBalanceVND: 1000, IdempotencyKey: &key}
	if accountFingerprint(account) != accountFingerprint(account) {
		t.Fatal("the same account request must produce the same fingerprint")
	}
	changed := account
	changed.OpeningBalanceVND = 2000
	if accountFingerprint(changed) == accountFingerprint(account) {
		t.Fatal("a different opening balance must produce a different fingerprint")
	}
	limit := int64(5000000)
	card := AccountInput{Name: "card", Kind: "CREDIT", CreditLimitVND: &limit, IdempotencyKey: &key}
	otherLimit := int64(6000000)
	otherCard := card
	otherCard.CreditLimitVND = &otherLimit
	if accountFingerprint(card) == accountFingerprint(otherCard) {
		t.Fatal("a different credit limit must produce a different fingerprint")
	}
}
