package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"grummans.finance.backend/internal/budget"
)

type createSalaryReceiptRequest struct {
	AmountVND  int64  `json:"amount_vnd"`
	ReceivedOn string `json:"received_on"`
}
type optionalInt64 struct {
	Set   bool
	Value *int64
}

func (o *optionalInt64) UnmarshalJSON(data []byte) error {
	o.Set = true
	if string(data) == "null" {
		return nil
	}
	var value int64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

// updateBudgetSettingsRequest replaces both knobs at once; a null
// global_budget_vnd means "no total budget". Both fields must be present, because
// an omitted one would silently wipe a stored setting.
type updateBudgetSettingsRequest struct {
	GlobalBufferVND optionalInt64 `json:"global_buffer_vnd"`
	GlobalBudgetVND optionalInt64 `json:"global_budget_vnd"`
}

type salaryReceiptsResponse struct {
	Receipts []budget.Receipt `json:"receipts"`
}

type cashflowForecastResponse struct {
	Cycle             budget.Cycle          `json:"cycle"`
	ConfirmedTotalVND int64                 `json:"confirmed_total_vnd"`
	ExpectedTotalVND  int64                 `json:"expected_total_vnd"`
	Items             []budget.ForecastItem `json:"items"`
}

func (h *Handler) createSalaryReceipt(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request createSalaryReceiptRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	result, err := h.budget.RecordSalaryReceipt(r.Context(), request.AmountVND, request.ReceivedOn, idempotencyKey)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) listSalaryReceipts(w http.ResponseWriter, r *http.Request) {
	receipts, err := h.budget.ListSalaryReceipts(r.Context())
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, salaryReceiptsResponse{Receipts: receipts})
}

func (h *Handler) getBudgetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.budget.GetSettings(r.Context())
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *Handler) updateBudgetSettings(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request updateBudgetSettingsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !request.GlobalBufferVND.Set || request.GlobalBufferVND.Value == nil {
		writeError(w, http.StatusBadRequest, "global_buffer_vnd is required")
		return
	}
	if !request.GlobalBudgetVND.Set {
		writeError(w, http.StatusBadRequest, "global_budget_vnd is required; send null to clear it")
		return
	}
	settings, err := h.budget.UpdateSettings(r.Context(), *request.GlobalBufferVND.Value, request.GlobalBudgetVND.Value, idempotencyKey)
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (h *Handler) getBudgetCurrent(w http.ResponseWriter, r *http.Request) {
	snapshot, err := h.budget.Snapshot(r.Context())
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (h *Handler) getCashflowForecast(w http.ResponseWriter, r *http.Request) {
	snapshot, err := h.budget.Snapshot(r.Context())
	if err != nil {
		writeBudgetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cashflowForecastResponse{
		Cycle:             snapshot.Cycle,
		ConfirmedTotalVND: snapshot.ReservedConfirmedVND,
		ExpectedTotalVND:  snapshot.ReservedExpectedVND,
		Items:             snapshot.Forecast,
	})
}

func writeBudgetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, budget.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), budget.ErrInvalidInput.Error()+": "))
	case errors.Is(err, budget.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusConflict, map[string]string{
			"code":  "conflict",
			"error": "idempotency key was already used for a different request",
		})
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}
