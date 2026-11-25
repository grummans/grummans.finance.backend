package obligation

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound          = errors.New("occurrence not found")
	ErrReferenceNotFound = errors.New("referenced record not found")
	ErrOverpayment       = errors.New("payment exceeds remaining amount")
	ErrAmountBelowPaid   = errors.New("confirmed amount is below paid amount")
	// ErrIdempotencyKeyReused guards a retried key against a silently different payment
	ErrIdempotencyKeyReused = errors.New("idempotency key was already used for a different request")
)

type Obligation struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Currency  string    `json:"currency"`
	CreatedAt time.Time `json:"created_at"`
}

type Occurrence struct {
	ID                 string    `json:"id"`
	ObligationID       string    `json:"obligation_id"`
	ObligationName     string    `json:"obligation_name"`
	DueDate            time.Time `json:"due_date"`
	ConfirmedAmountVND int64     `json:"confirmed_amount_vnd"`
	PaidAmountVND      int64     `json:"paid_amount_vnd"`
	// IsEstimate marks a variable bill booked with a reserve amount before its
	// real amount is known (D-012); updating the amount confirms it.
	IsEstimate bool `json:"is_estimate"`
}

func (o Occurrence) RemainingAmountVND() int64 {
	return o.ConfirmedAmountVND - o.PaidAmountVND
}

func (o Occurrence) Status(now time.Time) string {
	if o.RemainingAmountVND() == 0 {
		return "PAID"
	}
	location, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	today := now.In(location)
	dueDate := o.DueDate
	if time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, location).After(
		time.Date(dueDate.Year(), dueDate.Month(), dueDate.Day(), 0, 0, 0, 0, location),
	) {
		return "OVERDUE"
	}
	if o.PaidAmountVND > 0 {
		return "PARTIAL"
	}
	return "UNPAID"
}

// Payment is one recorded payment of an occurrence. It never counts as a
// transaction by itself; it can be linked to one manually and stays readable
// after a reversal.
type Payment struct {
	ID            string     `json:"id"`
	OccurrenceID  string     `json:"occurrence_id"`
	AmountVND     int64      `json:"amount_vnd"`
	PaidAt        time.Time  `json:"paid_at"`
	ReversedAt    *time.Time `json:"reversed_at"`
	TransactionID *string    `json:"transaction_id"`
}

type Store interface {
	CreateObligation(context.Context, string, string) (Obligation, error)
	CreateOccurrence(context.Context, string, time.Time, int64, bool, string) (Occurrence, error)
	UpdateOccurrenceAmount(context.Context, string, int64, string) (Occurrence, error)
	GetOccurrence(context.Context, string) (Occurrence, error)
	ListPayments(context.Context, string) ([]Payment, error)
	RecordPayment(context.Context, string, int64, time.Time, string) (Occurrence, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) CreateObligation(ctx context.Context, name, idempotencyKey string) (Obligation, error) {
	return s.store.CreateObligation(ctx, name, idempotencyKey)
}

func (s *Service) CreateOccurrence(ctx context.Context, obligationID string, dueDate time.Time, amountVND int64, idempotencyKey string) (Occurrence, error) {
	return s.store.CreateOccurrence(ctx, obligationID, dueDate, amountVND, false, idempotencyKey)
}

// CreateEstimatedOccurrence books a reserve amount for a variable bill whose
// real amount is not known yet; updating the amount later confirms it (D-012).
func (s *Service) CreateEstimatedOccurrence(ctx context.Context, obligationID string, dueDate time.Time, amountVND int64, idempotencyKey string) (Occurrence, error) {
	return s.store.CreateOccurrence(ctx, obligationID, dueDate, amountVND, true, idempotencyKey)
}

func (s *Service) UpdateOccurrenceAmount(ctx context.Context, occurrenceID string, amountVND int64, idempotencyKey string) (Occurrence, error) {
	return s.store.UpdateOccurrenceAmount(ctx, occurrenceID, amountVND, idempotencyKey)
}

func (s *Service) GetOccurrence(ctx context.Context, occurrenceID string) (Occurrence, error) {
	return s.store.GetOccurrence(ctx, occurrenceID)
}

func (s *Service) ListPayments(ctx context.Context, occurrenceID string) ([]Payment, error) {
	return s.store.ListPayments(ctx, occurrenceID)
}

func (s *Service) RecordPayment(ctx context.Context, occurrenceID string, amountVND int64, paidAt time.Time, idempotencyKey string) (Occurrence, error) {
	return s.store.RecordPayment(ctx, occurrenceID, amountVND, paidAt, idempotencyKey)
}
