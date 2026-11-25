package obligation

import (
	"strings"
	"testing"
	"time"
)

func TestOccurrenceStatus(t *testing.T) {
	location, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		occurrence Occurrence
		now        time.Time
		want       string
	}{
		{
			name:       "unpaid on due date",
			occurrence: Occurrence{DueDate: time.Date(2026, 1, 10, 0, 0, 0, 0, location), ConfirmedAmountVND: 1},
			now:        time.Date(2026, 1, 10, 23, 59, 0, 0, location),
			want:       "UNPAID",
		},
		{
			name:       "overdue from next day",
			occurrence: Occurrence{DueDate: time.Date(2026, 1, 10, 0, 0, 0, 0, location), ConfirmedAmountVND: 1},
			now:        time.Date(2026, 1, 11, 0, 0, 0, 0, location),
			want:       "OVERDUE",
		},
		{
			name:       "partial is overdue after due date",
			occurrence: Occurrence{DueDate: time.Date(2026, 1, 10, 0, 0, 0, 0, location), ConfirmedAmountVND: 10, PaidAmountVND: 5},
			now:        time.Date(2026, 1, 11, 0, 0, 0, 0, location),
			want:       "OVERDUE",
		},
		{
			name:       "paid",
			occurrence: Occurrence{ConfirmedAmountVND: 10, PaidAmountVND: 10},
			now:        time.Now(),
			want:       "PAID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.occurrence.Status(tt.now); got != tt.want {
				t.Fatalf("Status() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestOccurrenceRemainingAmountVND(t *testing.T) {
	occurrence := Occurrence{ConfirmedAmountVND: 2_000_000, PaidAmountVND: 500_000}
	if got := occurrence.RemainingAmountVND(); got != 1_500_000 {
		t.Fatalf("RemainingAmountVND() = %d, want 1500000", got)
	}
}

func TestPaymentFingerprint(t *testing.T) {
	occurrenceID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	paidAt := time.Date(2026, 11, 24, 10, 0, 0, 123456789, time.UTC)
	base := paymentFingerprint(occurrenceID, 100_000, paidAt)

	// PostgreSQL stores uuids lowercased and timestamps at microsecond precision,
	// so an identical retry spelled differently must still match
	if paymentFingerprint(strings.ToUpper(occurrenceID), 100_000, paidAt) != base {
		t.Fatal("uppercase uuid or extra nanosecond precision must not change the fingerprint")
	}
	if paymentFingerprint(occurrenceID, 200_000, paidAt) == base {
		t.Fatal("a different amount must produce a different fingerprint")
	}
	otherOccurrence := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if paymentFingerprint(otherOccurrence, 100_000, paidAt) == base {
		t.Fatal("a different occurrence must produce a different fingerprint")
	}
	if paymentFingerprint(occurrenceID, 100_000, paidAt.Add(time.Hour)) == base {
		t.Fatal("a different paid_at must produce a different fingerprint")
	}
}
