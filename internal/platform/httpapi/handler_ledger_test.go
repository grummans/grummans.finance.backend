package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grummans.finance.backend/internal/ledger"
	"grummans.finance.backend/internal/obligation"
)

// rune limits must be counted in characters, not bytes, matching the SQL length() checks.
func TestHandlerLedgerRuneLimits(t *testing.T) {
	const accountID = "11111111-1111-1111-1111-111111111111"

	tests := []struct {
		name           string
		method         string
		path           string
		body           string
		idempotencyKey string
		want           int
	}{
		{
			name:   "vietnamese note of 500 runes is accepted",
			method: http.MethodPost,
			path:   "/api/v1/transactions",
			body: `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1000,` +
				`"occurred_at":"2026-11-24T10:00:00+07:00","note":"` + strings.Repeat("ă", 500) + `"}`,
			idempotencyKey: "note-500",
			want:           http.StatusCreated,
		},
		{
			name:   "vietnamese note of 501 runes is rejected",
			method: http.MethodPost,
			path:   "/api/v1/transactions",
			body: `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1000,` +
				`"occurred_at":"2026-11-24T10:00:00+07:00","note":"` + strings.Repeat("ă", 501) + `"}`,
			idempotencyKey: "note-501",
			want:           http.StatusBadRequest,
		},
		{
			name:           "account name of 120 runes is accepted",
			method:         http.MethodPost,
			path:           "/api/v1/accounts",
			body:           `{"name":"` + strings.Repeat("ă", 120) + `","kind":"CASH"}`,
			idempotencyKey: "name-120",
			want:           http.StatusCreated,
		},
		{
			name:           "account name of 121 runes is rejected",
			method:         http.MethodPost,
			path:           "/api/v1/accounts",
			body:           `{"name":"` + strings.Repeat("ă", 121) + `","kind":"CASH"}`,
			idempotencyKey: "name-121",
			want:           http.StatusBadRequest,
		},
		{
			name:           "idempotency key of 256 characters is rejected",
			method:         http.MethodPost,
			path:           "/api/v1/transactions",
			body:           `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1000,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			idempotencyKey: strings.Repeat("ă", 256),
			want:           http.StatusBadRequest,
		},
		{
			name:           "idempotency key of 255 vietnamese characters is accepted",
			method:         http.MethodPost,
			path:           "/api/v1/transactions",
			body:           `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1000,"occurred_at":"2026-11-24T10:00:00+07:00"}`,
			idempotencyKey: strings.Repeat("ă", 255),
			want:           http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeLedgerStore{}
			handler := NewHandler(obligation.NewService(&fakeStore{}), ledger.NewService(store))
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Idempotency-Key", tt.idempotencyKey)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, tt.want, response.Body.String())
			}
			if response.Code == http.StatusBadRequest && (store.calls != 0 || strings.Count(response.Body.String(), `"code"`) != 1) {
				t.Fatalf("rejected request reached the store %d times or wrote %d bodies: %s",
					store.calls, strings.Count(response.Body.String(), `"code"`), response.Body.String())
			}
		})
	}
}

func TestHandlerUpdateTransactionCategoryShape(t *testing.T) {
	const transactionID = "44444444-4444-4444-4444-444444444444"
	const categoryID = "55555555-5555-5555-5555-555555555555"

	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "empty body is rejected instead of clearing", body: `{}`, want: http.StatusBadRequest},
		{name: "explicit null clears", body: `{"category_id":null}`, want: http.StatusOK},
		{name: "uuid sets", body: `{"category_id":"` + categoryID + `"}`, want: http.StatusOK},
		{name: "garbage is rejected", body: `{"category_id":123}`, want: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(obligation.NewService(&fakeStore{}), ledger.NewService(&fakeLedgerStore{}))
			request := httptest.NewRequest(http.MethodPatch, "/api/v1/transactions/"+transactionID, strings.NewReader(tt.body))
			request.Header.Set("Idempotency-Key", "shape-"+tt.name)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, tt.want, response.Body.String())
			}
		})
	}
}

func TestHandlerRejectsNULCharacters(t *testing.T) {
	const accountID = "11111111-1111-1111-1111-111111111111"

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "note",
			path: "/api/v1/transactions",
			body: `{"account_id":"` + accountID + `","type":"EXPENSE","amount_vnd":1000,` +
				`"occurred_at":"2026-11-24T10:00:00+07:00","note":"rent\u0000paid"}`,
		},
		{
			name: "account name",
			path: "/api/v1/accounts",
			body: `{"name":"wal\u0000let","kind":"CASH"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(obligation.NewService(&fakeStore{}), ledger.NewService(&fakeLedgerStore{}))
			request := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Idempotency-Key", "nul-key")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", response.Code, response.Body.String())
			}
		})
	}
}
