package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grummans.finance.backend/internal/ledger"
	"grummans.finance.backend/internal/obligation"
)

type fakeStore struct {
	getOccurrenceErr    error
	updateOccurrenceErr error
	listPaymentsErr     error
	payments            []obligation.Payment
	calls               int
}

func (f *fakeStore) CreateObligation(context.Context, string, string) (obligation.Obligation, error) {
	f.calls++
	return obligation.Obligation{}, nil
}

func (f *fakeStore) CreateOccurrence(context.Context, string, time.Time, int64, bool, string) (obligation.Occurrence, error) {
	f.calls++
	return obligation.Occurrence{}, nil
}

func (f *fakeStore) UpdateOccurrenceAmount(context.Context, string, int64, string) (obligation.Occurrence, error) {
	f.calls++
	return obligation.Occurrence{}, f.updateOccurrenceErr
}

func (f *fakeStore) ListPayments(context.Context, string) ([]obligation.Payment, error) {
	f.calls++
	return f.payments, f.listPaymentsErr
}

func (f *fakeStore) GetOccurrence(context.Context, string) (obligation.Occurrence, error) {
	return obligation.Occurrence{}, f.getOccurrenceErr
}

func (f *fakeStore) RecordPayment(context.Context, string, int64, time.Time, string) (obligation.Occurrence, error) {
	f.calls++
	return obligation.Occurrence{}, nil
}

func TestHandlerRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
		want    int
	}{
		{
			name:    "blank obligation name",
			method:  http.MethodPost,
			path:    "/api/v1/obligations",
			body:    `{"name":" "}`,
			headers: map[string]string{"Idempotency-Key": "obl-1"},
			want:    http.StatusBadRequest,
		},
		{
			name:   "obligation without idempotency key",
			method: http.MethodPost,
			path:   "/api/v1/obligations",
			body:   `{"name":"rent"}`,
			want:   http.StatusBadRequest,
		},
		{
			name:    "occurrence with a non-uuid obligation",
			method:  http.MethodPost,
			path:    "/api/v1/obligations/not-a-uuid/occurrences",
			body:    `{"due_date":"2026-12-01","amount_vnd":100}`,
			headers: map[string]string{"Idempotency-Key": "occ-1"},
			want:    http.StatusBadRequest,
		},
		{
			name:   "missing idempotency key",
			method: http.MethodPost,
			path:   "/api/v1/occurrences/22222222-2222-2222-2222-222222222222/payments",
			body:   `{"amount_vnd":1,"paid_at":"2026-01-01T00:00:00Z"}`,
			want:   http.StatusBadRequest,
		},
		{
			name:   "unknown occurrence",
			method: http.MethodGet,
			path:   "/api/v1/occurrences/22222222-2222-2222-2222-222222222222",
			want:   http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			if tt.name == "unknown occurrence" {
				store.getOccurrenceErr = obligation.ErrNotFound
			}
			handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			for key, value := range tt.headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d", response.Code, tt.want)
			}
			if response.Code == http.StatusBadRequest && !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
				t.Fatalf("error response missing stable code: %s", response.Body.String())
			}
			// one JSON body only, and a rejected request never reaches the store
			if response.Code == http.StatusBadRequest && (store.calls != 0 || strings.Count(response.Body.String(), `"code"`) != 1) {
				t.Fatalf("rejected request reached the store %d times or wrote %d bodies: %s",
					store.calls, strings.Count(response.Body.String(), `"code"`), response.Body.String())
			}
		})
	}
}

type fakeLedgerStore struct {
	calls                int
	getTransactionErr    error
	listTransactionsErr  error
	linkPaymentErr       error
	updateTransactionErr error
	createTransactionErr error
	listAccountsErr      error
	createdTransactions  []ledger.Transaction
}

func (f *fakeLedgerStore) CreateAccount(context.Context, ledger.AccountInput) (ledger.Account, error) {
	f.calls++
	return ledger.Account{}, nil
}

func (f *fakeLedgerStore) ListAccounts(context.Context) ([]ledger.Account, error) {
	return nil, f.listAccountsErr
}

func (f *fakeLedgerStore) CreateCategory(context.Context, string, string) (ledger.Category, error) {
	return ledger.Category{}, nil
}

func (f *fakeLedgerStore) ListCategories(context.Context) ([]ledger.Category, error) {
	return nil, nil
}

func (f *fakeLedgerStore) CreateTransaction(context.Context, ledger.TransactionInput) ([]ledger.Transaction, error) {
	f.calls++
	if f.createTransactionErr != nil {
		return nil, f.createTransactionErr
	}
	return f.createdTransactions, nil
}

func (f *fakeLedgerStore) GetTransaction(context.Context, string) (ledger.Transaction, error) {
	return ledger.Transaction{}, f.getTransactionErr
}

func (f *fakeLedgerStore) ListTransactions(context.Context, ledger.Filter) ([]ledger.Transaction, error) {
	return nil, f.listTransactionsErr
}

func (f *fakeLedgerStore) UpdateTransactionCategory(context.Context, string, *string, string) (ledger.Transaction, error) {
	return ledger.Transaction{}, f.updateTransactionErr
}

func (f *fakeLedgerStore) LinkPaymentTransaction(context.Context, string, string, string) (ledger.PaymentLink, error) {
	return ledger.PaymentLink{}, f.linkPaymentErr
}

func TestHandlerLedgerValidation(t *testing.T) {
	const (
		accountID   = "11111111-1111-1111-1111-111111111111"
		otherID     = "22222222-2222-2222-2222-222222222222"
		paymentID   = "33333333-3333-3333-3333-333333333333"
		transaction = "44444444-4444-4444-4444-444444444444"
	)
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
		store   *fakeLedgerStore
		want    int
	}{
		{
			name:    "credit account without limit",
			method:  http.MethodPost,
			path:    "/api/v1/accounts",
			body:    `{"name":"card","kind":"CREDIT"}`,
			headers: map[string]string{"Idempotency-Key": "acct-1"},
			want:    http.StatusBadRequest,
		},
		{
			name:    "cash account with credit limit",
			method:  http.MethodPost,
			path:    "/api/v1/accounts",
			body:    `{"name":"wallet","kind":"CASH","credit_limit_vnd":5000000}`,
			headers: map[string]string{"Idempotency-Key": "acct-2"},
			want:    http.StatusBadRequest,
		},
		{
			name:   "transaction without idempotency key",
			method: http.MethodPost,
			path:   "/api/v1/transactions",
			body:   `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			want:   http.StatusBadRequest,
		},
		{
			name:    "unknown transaction type",
			method:  http.MethodPost,
			path:    "/api/v1/transactions",
			body:    `{"account_id":"` + accountID + `","type":"GIFT","amount_vnd":1,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			headers: map[string]string{"Idempotency-Key": "key-1"},
			want:    http.StatusBadRequest,
		},
		{
			name:    "transfer to the same account",
			method:  http.MethodPost,
			path:    "/api/v1/transactions",
			body:    `{"account_id":"` + accountID + `","to_account_id":"` + accountID + `","type":"TRANSFER","amount_vnd":1,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			headers: map[string]string{"Idempotency-Key": "key-2"},
			want:    http.StatusBadRequest,
		},
		{
			name:    "transfer with a category",
			method:  http.MethodPost,
			path:    "/api/v1/transactions",
			body:    `{"account_id":"` + accountID + `","to_account_id":"` + otherID + `","type":"TRANSFER","amount_vnd":1,"occurred_at":"2026-11-24T10:00:00+07:00","category_id":"` + otherID + `"}`,
			headers: map[string]string{"Idempotency-Key": "key-3"},
			want:    http.StatusBadRequest,
		},
		{
			name:    "expense with a destination account",
			method:  http.MethodPost,
			path:    "/api/v1/transactions",
			body:    `{"account_id":"` + accountID + `","to_account_id":"` + otherID + `","type":"EXPENSE","amount_vnd":1,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			headers: map[string]string{"Idempotency-Key": "key-4"},
			want:    http.StatusBadRequest,
		},
		{
			name:    "transfer accepted at the boundary",
			method:  http.MethodPost,
			path:    "/api/v1/transactions",
			body:    `{"account_id":"` + accountID + `","to_account_id":"` + otherID + `","type":"TRANSFER","amount_vnd":500000,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			headers: map[string]string{"Idempotency-Key": "key-5"},
			want:    http.StatusCreated,
		},
		{
			name:   "transaction filter limit out of range",
			method: http.MethodGet,
			path:   "/api/v1/transactions?limit=500",
			want:   http.StatusBadRequest,
		},
		{
			name:   "transaction filter with invalid category",
			method: http.MethodGet,
			path:   "/api/v1/transactions?category_id=not-a-uuid",
			want:   http.StatusBadRequest,
		},
		{
			name:   "date range reversed",
			method: http.MethodGet,
			path:   "/api/v1/transactions?from=2026-11-30&to=2026-11-01",
			want:   http.StatusBadRequest,
		},
		{
			name:   "unknown transaction",
			method: http.MethodGet,
			path:   "/api/v1/transactions/" + transaction,
			store:  &fakeLedgerStore{getTransactionErr: ledger.ErrNotFound},
			want:   http.StatusNotFound,
		},
		{
			name:    "payment already linked",
			method:  http.MethodPatch,
			path:    "/api/v1/payments/" + paymentID + "/transaction",
			body:    `{"transaction_id":"` + transaction + `"}`,
			headers: map[string]string{"Idempotency-Key": "link-1"},
			store:   &fakeLedgerStore{linkPaymentErr: ledger.ErrAlreadyLinked},
			want:    http.StatusConflict,
		},
		{
			name:    "category on a transfer is a rule violation",
			method:  http.MethodPatch,
			path:    "/api/v1/transactions/" + transaction,
			body:    `{"category_id":"` + otherID + `"}`,
			headers: map[string]string{"Idempotency-Key": "patch-1"},
			store:   &fakeLedgerStore{updateTransactionErr: ledger.ErrInvalidChange},
			want:    http.StatusUnprocessableEntity,
		},
		{
			name:    "idempotency key reused for a different request",
			method:  http.MethodPost,
			path:    "/api/v1/transactions",
			body:    `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			headers: map[string]string{"Idempotency-Key": "reused"},
			store:   &fakeLedgerStore{createTransactionErr: ledger.ErrIdempotencyKeyReused},
			want:    http.StatusConflict,
		},
		{
			name:   "unknown account or category",
			method: http.MethodGet,
			path:   "/api/v1/accounts",
			store:  &fakeLedgerStore{listAccountsErr: ledger.ErrReferenceNotFound},
			want:   http.StatusUnprocessableEntity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.store == nil {
				tt.store = &fakeLedgerStore{}
			}
			handler := NewHandler(obligation.NewService(&fakeStore{}), ledger.NewService(tt.store), newTestBudgetService())
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			for key, value := range tt.headers {
				request.Header.Set(key, value)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, tt.want, response.Body.String())
			}
		})
	}
}

func TestHandlerUpdateOccurrenceIsAmountOnlyAndMapsMissingObligation(t *testing.T) {
	const occurrenceID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

	t.Run("due_date is not part of the amount update", func(t *testing.T) {
		store := &fakeStore{}
		handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
		request := httptest.NewRequest(http.MethodPatch, "/api/v1/occurrences/"+occurrenceID, strings.NewReader(`{"amount_vnd":5000,"due_date":"2026-01-01"}`))
		request.Header.Set("Idempotency-Key", "occ-patch-1")
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest || store.calls != 0 {
			t.Fatalf("status = %d, store calls = %d, body = %s", response.Code, store.calls, response.Body.String())
		}
	})

	t.Run("missing obligation is a reference error", func(t *testing.T) {
		store := &fakeStore{updateOccurrenceErr: obligation.ErrReferenceNotFound}
		handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
		request := httptest.NewRequest(http.MethodPatch, "/api/v1/occurrences/"+occurrenceID, strings.NewReader(`{"amount_vnd":5000}`))
		request.Header.Set("Idempotency-Key", "occ-patch-2")
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422, body = %s", response.Code, response.Body.String())
		}
	})
}

func TestHandlerListPayments(t *testing.T) {
	const occurrenceID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	linked := "3f2b8c1a-1c2d-4e5f-8a9b-0c1d2e3f4a5b"

	t.Run("returns the payments of an occurrence", func(t *testing.T) {
		store := &fakeStore{payments: []obligation.Payment{{
			ID:            "11111111-1111-4111-8111-111111111111",
			OccurrenceID:  occurrenceID,
			AmountVND:     2_500_000,
			PaidAt:        time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC),
			TransactionID: &linked,
		}}}
		handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
		request := httptest.NewRequest(http.MethodGet, "/api/v1/occurrences/"+occurrenceID+"/payments", nil)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var body struct {
			Payments []obligation.Payment `json:"payments"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding payments: %v", err)
		}
		want := obligation.Payment{
			ID:            "11111111-1111-4111-8111-111111111111",
			OccurrenceID:  occurrenceID,
			AmountVND:     2_500_000,
			PaidAt:        time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC),
			TransactionID: &linked,
		}
		if len(body.Payments) != 1 || body.Payments[0].ID != want.ID || !body.Payments[0].PaidAt.Equal(want.PaidAt) ||
			body.Payments[0].AmountVND != want.AmountVND || body.Payments[0].OccurrenceID != occurrenceID ||
			body.Payments[0].TransactionID == nil || *body.Payments[0].TransactionID != linked ||
			body.Payments[0].ReversedAt != nil {
			t.Fatalf("payments = %+v, want %+v", body.Payments, want)
		}
	})

	t.Run("an occurrence without payments is an empty list", func(t *testing.T) {
		store := &fakeStore{}
		handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
		request := httptest.NewRequest(http.MethodGet, "/api/v1/occurrences/"+occurrenceID+"/payments", nil)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var body struct {
			Payments []obligation.Payment `json:"payments"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding payments: %v", err)
		}
		if body.Payments == nil || len(body.Payments) != 0 {
			t.Fatalf("payments = %+v, want an empty array", body.Payments)
		}
	})

	t.Run("a missing occurrence is a 404", func(t *testing.T) {
		store := &fakeStore{listPaymentsErr: obligation.ErrNotFound}
		handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
		request := httptest.NewRequest(http.MethodGet, "/api/v1/occurrences/"+occurrenceID+"/payments", nil)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("a non-uuid occurrence id is rejected before the store", func(t *testing.T) {
		store := &fakeStore{}
		handler := NewHandler(obligation.NewService(store), ledger.NewService(&fakeLedgerStore{}), newTestBudgetService())
		request := httptest.NewRequest(http.MethodGet, "/api/v1/occurrences/not-a-uuid/payments", nil)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest || store.calls != 0 {
			t.Fatalf("status = %d, store calls = %d", response.Code, store.calls)
		}
	})
}
