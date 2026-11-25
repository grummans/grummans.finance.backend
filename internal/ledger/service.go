package ledger

import (
	"context"
	"time"
)

// Type determines the posting direction; TRANSFER creates an OUT/IN pair.
type Type string

const (
	TypeExpense   Type = "EXPENSE"
	TypeIncome    Type = "INCOME"
	TypeTransfer  Type = "TRANSFER"
	TypeRepayment Type = "REPAYMENT"
	TypeFee       Type = "FEE"
	TypeRefund    Type = "REFUND"
)

// DirectionFor returns the posting direction implied by the transaction type.
// Only TRANSFER has no implied direction because it posts both sides.
func DirectionFor(kind Type) string {
	switch kind {
	case TypeIncome, TypeRefund:
		return "IN"
	case TypeTransfer:
		return ""
	default:
		return "OUT"
	}
}

type Account struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Currency          string `json:"currency"`
	OpeningBalanceVND int64  `json:"opening_balance_vnd"`
	CreditLimitVND    *int64 `json:"credit_limit_vnd"`
	OpenedOn          string `json:"opened_on"`
}

type AccountInput struct {
	Name              string
	Kind              string
	OpeningBalanceVND int64
	CreditLimitVND    *int64
	IdempotencyKey    *string
}

type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Transaction struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	AccountName  string    `json:"account_name"`
	Direction    string    `json:"direction"`
	Type         string    `json:"type"`
	AmountVND    int64     `json:"amount_vnd"`
	OccurredAt   time.Time `json:"occurred_at"`
	CategoryID   *string   `json:"category_id"`
	CategoryName *string   `json:"category_name"`
	Note         *string   `json:"note"`
	TransferID   *string   `json:"transfer_id"`
}

type TransactionInput struct {
	AccountID      string
	ToAccountID    string
	Type           Type
	AmountVND      int64
	OccurredAt     time.Time
	CategoryID     *string
	Note           *string
	IdempotencyKey *string
}

// Filter narrows the timeline. From and To are inclusive business dates in Asia/Ho_Chi_Minh.
type Filter struct {
	From       time.Time
	HasFrom    bool
	To         time.Time
	HasTo      bool
	CategoryID *string
	Limit      int
	Offset     int
}

type PaymentLink struct {
	PaymentID     string  `json:"payment_id"`
	TransactionID *string `json:"transaction_id"`
}

type Store interface {
	CreateAccount(context.Context, AccountInput) (Account, error)
	ListAccounts(context.Context) ([]Account, error)
	CreateCategory(context.Context, string, string) (Category, error)
	ListCategories(context.Context) ([]Category, error)
	CreateTransaction(context.Context, TransactionInput) ([]Transaction, error)
	GetTransaction(context.Context, string) (Transaction, error)
	ListTransactions(context.Context, Filter) ([]Transaction, error)
	UpdateTransactionCategory(context.Context, string, *string, string) (Transaction, error)
	LinkPaymentTransaction(context.Context, string, string, string) (PaymentLink, error)
}

// Service is a thin pass-through: financial validation lives at the HTTP boundary
// and the invariants lives in the schema.
type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) CreateAccount(ctx context.Context, input AccountInput) (Account, error) {
	return s.store.CreateAccount(ctx, input)
}

func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	return s.store.ListAccounts(ctx)
}

func (s *Service) CreateCategory(ctx context.Context, name, idempotencyKey string) (Category, error) {
	return s.store.CreateCategory(ctx, name, idempotencyKey)
}

func (s *Service) ListCategories(ctx context.Context) ([]Category, error) {
	return s.store.ListCategories(ctx)
}

func (s *Service) CreateTransaction(ctx context.Context, input TransactionInput) ([]Transaction, error) {
	return s.store.CreateTransaction(ctx, input)
}

func (s *Service) GetTransaction(ctx context.Context, id string) (Transaction, error) {
	return s.store.GetTransaction(ctx, id)
}

func (s *Service) ListTransactions(ctx context.Context, filter Filter) ([]Transaction, error) {
	return s.store.ListTransactions(ctx, filter)
}

func (s *Service) UpdateTransactionCategory(ctx context.Context, id string, categoryID *string, idempotencyKey string) (Transaction, error) {
	return s.store.UpdateTransactionCategory(ctx, id, categoryID, idempotencyKey)
}

func (s *Service) LinkPaymentTransaction(ctx context.Context, paymentID, transactionID, idempotencyKey string) (PaymentLink, error) {
	return s.store.LinkPaymentTransaction(ctx, paymentID, transactionID, idempotencyKey)
}
