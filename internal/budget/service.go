package budget

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidInput marks a request that breaks a money rule; handlers answer 422.
	ErrInvalidInput = errors.New("budget input is invalid")
	// ErrIdempotencyKeyReused means the key already belongs to another payload.
	ErrIdempotencyKeyReused = errors.New("idempotency key was already used for a different request")
)

// Store is the persistence the budget needs.
type Store interface {
	CreateSalaryReceipt(context.Context, int64, time.Time, string) (Receipt, error)
	ListSalaryReceipts(context.Context) ([]Receipt, error)
	GetSettings(context.Context) (Settings, error)
	UpdateSettings(context.Context, Settings, string) (Settings, error)
	// SnapshotData reads receipts, settings, cash, cycle spending and unpaid
	// obligations in one consistent snapshot.
	SnapshotData(context.Context, time.Time) (Data, []Receipt, Settings, error)
}

// Service reads the salary cycle and builds the budget snapshot.
type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// Snapshot is the single computation both budget endpoints serve.
func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	now := s.now()
	data, receipts, settings, err := s.store.SnapshotData(ctx, now)
	if err != nil {
		return Snapshot{}, fmt.Errorf("reading budget snapshot: %w", err)
	}
	return Build(CycleFor(now, receipts), settings, data, now)
}

func (s *Service) ListSalaryReceipts(ctx context.Context) ([]Receipt, error) {
	return s.store.ListSalaryReceipts(ctx)
}

// RecordSalaryReceipt stores one actual payday. A future date is refused: the
// next payday is a projection until the money has actually arrived.
func (s *Service) RecordSalaryReceipt(ctx context.Context, amountVND int64, receivedOn, idempotencyKey string) (Receipt, error) {
	if amountVND <= 0 {
		return Receipt{}, fmt.Errorf("%w: amount must be positive", ErrInvalidInput)
	}
	day, err := ParseBusinessDate(receivedOn)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: received_on must be YYYY-MM-DD", ErrInvalidInput)
	}
	if day.After(BusinessDate(s.now())) {
		return Receipt{}, fmt.Errorf("%w: received_on cannot be in the future", ErrInvalidInput)
	}
	return s.store.CreateSalaryReceipt(ctx, amountVND, day, idempotencyKey)
}

func (s *Service) GetSettings(ctx context.Context) (Settings, error) {
	return s.store.GetSettings(ctx)
}

// UpdateSettings replaces both knobs at once, so clearing the total budget is an
// explicit null rather than a forgotten field.
func (s *Service) UpdateSettings(ctx context.Context, bufferVND int64, budgetVND *int64, idempotencyKey string) (Settings, error) {
	if bufferVND < 0 {
		return Settings{}, fmt.Errorf("%w: global_buffer_vnd cannot be negative", ErrInvalidInput)
	}
	if budgetVND != nil && *budgetVND <= 0 {
		return Settings{}, fmt.Errorf("%w: global_budget_vnd must be positive or null", ErrInvalidInput)
	}
	return s.store.UpdateSettings(ctx, Settings{GlobalBufferVND: bufferVND, GlobalBudgetVND: budgetVND}, idempotencyKey)
}
