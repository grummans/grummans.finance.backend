package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"grummans.finance.backend/internal/budget"
	"grummans.finance.backend/internal/ledger"
	"grummans.finance.backend/internal/obligation"
)

type fakeBudgetStore struct {
	receipts   []budget.Receipt
	settings   budget.Settings
	data       budget.Data
	createErr  error
	createdKey string
}

func (f *fakeBudgetStore) CreateSalaryReceipt(_ context.Context, amountVND int64, receivedOn time.Time, idempotencyKey string) (budget.Receipt, error) {
	if f.createErr != nil {
		return budget.Receipt{}, f.createErr
	}
	f.createdKey = idempotencyKey
	return budget.Receipt{
		ID:         "10000000-0000-0000-0000-000000000001",
		AmountVND:  amountVND,
		ReceivedOn: receivedOn.Format("2006-01-02"),
		ReceivedAt: time.Now(),
	}, nil
}

func (f *fakeBudgetStore) ListSalaryReceipts(context.Context) ([]budget.Receipt, error) {
	return f.receipts, nil
}

func (f *fakeBudgetStore) GetSettings(context.Context) (budget.Settings, error) {
	return f.settings, nil
}

func (f *fakeBudgetStore) UpdateSettings(_ context.Context, settings budget.Settings, _ string) (budget.Settings, error) {
	f.settings = settings
	return settings, nil
}

func (f *fakeBudgetStore) SnapshotData(_ context.Context, now time.Time) (budget.Data, []budget.Receipt, budget.Settings, error) {
	return f.data, f.receipts, f.settings, nil
}

func newTestBudgetService() *budget.Service {
	return budget.NewService(&fakeBudgetStore{})
}

func newBudgetHandler(store *fakeBudgetStore) http.Handler {
	return NewHandler(obligation.NewService(&fakeStore{}), ledger.NewService(&fakeLedgerStore{}), budget.NewService(store))
}

func TestSalaryReceiptEndpoints(t *testing.T) {
	today := budget.BusinessDate(time.Now()).Format("2006-01-02")
	tomorrow := budget.BusinessDate(time.Now()).AddDate(0, 0, 1).Format("2006-01-02")

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		key        string
		want       int
		wantStored bool
	}{
		{
			name:       "records an actual payday",
			method:     http.MethodPost,
			path:       "/api/v1/salary-receipts",
			body:       `{"amount_vnd":13095124,"received_on":"` + today + `"}`,
			key:        "salary-1",
			want:       http.StatusCreated,
			wantStored: true,
		},
		{
			name:   "a payday that has not arrived is not a receipt",
			method: http.MethodPost,
			path:   "/api/v1/salary-receipts",
			body:   `{"amount_vnd":13095124,"received_on":"` + tomorrow + `"}`,
			key:    "salary-2",
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "zero amount is refused",
			method: http.MethodPost,
			path:   "/api/v1/salary-receipts",
			body:   `{"amount_vnd":0,"received_on":"` + today + `"}`,
			key:    "salary-3",
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "the idempotency key is mandatory",
			method: http.MethodPost,
			path:   "/api/v1/salary-receipts",
			body:   `{"amount_vnd":1,"received_on":"` + today + `"}`,
			want:   http.StatusBadRequest,
		},
		{
			name:   "a malformed business date is refused",
			method: http.MethodPost,
			path:   "/api/v1/salary-receipts",
			body:   `{"amount_vnd":1,"received_on":"11/2026"}`,
			key:    "salary-5",
			want:   http.StatusUnprocessableEntity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeBudgetStore{}
			handler := newBudgetHandler(store)
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.key != "" {
				request.Header.Set("Idempotency-Key", tt.key)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.want, response.Body.String())
			}
			if stored := store.createdKey != ""; stored != tt.wantStored {
				t.Fatalf("stored = %v, want %v", stored, tt.wantStored)
			}
		})
	}
}

func TestListSalaryReceipts(t *testing.T) {
	store := &fakeBudgetStore{receipts: []budget.Receipt{{
		ID: "10000000-0000-0000-0000-000000000001", AmountVND: 13_095_124, ReceivedOn: "2026-11-06",
	}}}
	response := httptest.NewRecorder()
	newBudgetHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/salary-receipts", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var body salaryReceiptsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.Receipts) != 1 || body.Receipts[0].ReceivedOn != "2026-11-06" {
		t.Fatalf("receipts = %+v, want one receipt on 2026-11-06", body.Receipts)
	}
}

func TestBudgetSettingsEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		key        string
		want       int
		wantBuffer int64
		wantBudget *int64
	}{
		{
			name:       "replaces both knobs",
			body:       `{"global_buffer_vnd":2000000,"global_budget_vnd":8000000}`,
			key:        "settings-1",
			want:       http.StatusOK,
			wantBuffer: 2_000_000,
			wantBudget: int64Ptr(8_000_000),
		},
		{
			name:       "a null total budget clears it",
			body:       `{"global_buffer_vnd":0,"global_budget_vnd":null}`,
			key:        "settings-2",
			want:       http.StatusOK,
			wantBuffer: 0,
		},
		{
			name: "a missing buffer is refused",
			body: `{"global_budget_vnd":8000000}`,
			key:  "settings-4",
			want: http.StatusBadRequest,
		},
		{
			name: "a null buffer is refused",
			body: `{"global_buffer_vnd":null,"global_budget_vnd":null}`,
			key:  "settings-5",
			want: http.StatusBadRequest,
		},
		{
			name: "a missing total budget is refused",
			body: `{"global_buffer_vnd":100000}`,
			key:  "settings-6",
			want: http.StatusBadRequest,
		},
		{
			name: "the idempotency key is mandatory",
			body: `{"global_buffer_vnd":0,"global_budget_vnd":null}`,
			want: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeBudgetStore{}
			request := httptest.NewRequest(http.MethodPut, "/api/v1/budget/settings", strings.NewReader(tt.body))
			if tt.key != "" {
				request.Header.Set("Idempotency-Key", tt.key)
			}
			response := httptest.NewRecorder()
			newBudgetHandler(store).ServeHTTP(response, request)

			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.want, response.Body.String())
			}
			if tt.want != http.StatusOK {
				return
			}
			if store.settings.GlobalBufferVND != tt.wantBuffer {
				t.Fatalf("buffer = %d, want %d", store.settings.GlobalBufferVND, tt.wantBuffer)
			}
			switch {
			case tt.wantBudget == nil && store.settings.GlobalBudgetVND != nil:
				t.Fatalf("budget = %d, want null", *store.settings.GlobalBudgetVND)
			case tt.wantBudget != nil && (store.settings.GlobalBudgetVND == nil || *store.settings.GlobalBudgetVND != *tt.wantBudget):
				t.Fatalf("budget = %v, want %d", store.settings.GlobalBudgetVND, *tt.wantBudget)
			}
		})
	}
}

func TestGetBudgetCurrent(t *testing.T) {
	store := &fakeBudgetStore{
		settings: budget.Settings{GlobalBufferVND: 1_000_000, GlobalBudgetVND: int64Ptr(5_000_000)},
		data: budget.Data{
			CashOnHandVND:   10_000_000,
			SpentInCycleVND: 1_000_000,
			Occurrences: []budget.Occurrence{
				{ID: "20000000-0000-0000-0000-000000000001", ObligationName: "Nha", DueDate: "2099-11-30", RemainingVND: 3_700_000},
				{ID: "20000000-0000-0000-0000-000000000002", ObligationName: "Dien", DueDate: "2099-11-05", RemainingVND: 500_000, IsEstimate: true},
			},
		},
	}
	response := httptest.NewRecorder()
	newBudgetHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/budget/current", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var snapshot budget.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}
	if snapshot.ReservedConfirmedVND != 3_700_000 || snapshot.ReservedExpectedVND != 500_000 {
		t.Fatalf("reserves = %d/%d, want 3700000/500000", snapshot.ReservedConfirmedVND, snapshot.ReservedExpectedVND)
	}
	if snapshot.AvailableVND != 10_000_000-4_200_000-1_000_000 {
		t.Fatalf("available = %d, want %d", snapshot.AvailableVND, 4_800_000)
	}
	// only the total budget can lower what is spendable: 5.000.000 - 1.000.000 spent
	if snapshot.SpendableVND != 4_000_000 {
		t.Fatalf("spendable = %d, want 4000000", snapshot.SpendableVND)
	}
	if snapshot.DailyAllowanceVND != snapshot.SpendableVND/int64(snapshot.Cycle.DaysRemaining) {
		t.Fatalf("daily allowance = %d, want spendable / %d days", snapshot.DailyAllowanceVND, snapshot.Cycle.DaysRemaining)
	}
	if len(snapshot.Forecast) != 2 || snapshot.Forecast[1].Certainty != "expected" {
		t.Fatalf("forecast = %+v, want a confirmed and an expected item", snapshot.Forecast)
	}
}

func TestGetCashflowForecast(t *testing.T) {
	store := &fakeBudgetStore{data: budget.Data{Occurrences: []budget.Occurrence{{
		ID: "20000000-0000-0000-0000-000000000001", ObligationName: "Nha", DueDate: "2099-11-30", RemainingVND: 3_700_000,
	}}}}
	response := httptest.NewRecorder()
	newBudgetHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/cashflow/forecast", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body cashflowForecastResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding forecast: %v", err)
	}
	if body.ConfirmedTotalVND != 3_700_000 || len(body.Items) != 1 {
		t.Fatalf("forecast = %+v, want one confirmed item of 3700000", body)
	}
}

func int64Ptr(value int64) *int64 { return &value }
