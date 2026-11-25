package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"grummans.finance.backend/internal/ledger"
	"grummans.finance.backend/internal/obligation"
)

type Handler struct {
	service *obligation.Service
	ledger  *ledger.Service
}

func NewHandler(service *obligation.Service, ledgerService *ledger.Service) http.Handler {
	h := &Handler{service: service, ledger: ledgerService}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/obligations", h.createObligation)
	mux.HandleFunc("POST /api/v1/obligations/{obligationID}/occurrences", h.createOccurrence)
	mux.HandleFunc("PATCH /api/v1/occurrences/{occurrenceID}", h.updateOccurrence)
	mux.HandleFunc("GET /api/v1/occurrences/{occurrenceID}", h.getOccurrence)
	mux.HandleFunc("GET /api/v1/occurrences/{occurrenceID}/payments", h.listPayments)
	mux.HandleFunc("POST /api/v1/occurrences/{occurrenceID}/payments", h.recordPayment)
	mux.HandleFunc("POST /api/v1/accounts", h.createAccount)
	mux.HandleFunc("GET /api/v1/accounts", h.listAccounts)
	mux.HandleFunc("POST /api/v1/categories", h.createCategory)
	mux.HandleFunc("GET /api/v1/categories", h.listCategories)
	mux.HandleFunc("POST /api/v1/transactions", h.createTransaction)
	mux.HandleFunc("GET /api/v1/transactions", h.listTransactions)
	mux.HandleFunc("GET /api/v1/transactions/{transactionID}", h.getTransaction)
	mux.HandleFunc("PATCH /api/v1/transactions/{transactionID}", h.updateTransaction)
	mux.HandleFunc("PATCH /api/v1/payments/{paymentID}/transaction", h.linkPaymentTransaction)
	return mux
}

type createObligationRequest struct {
	Name string `json:"name"`
}

type createOccurrenceRequest struct {
	DueDate   string `json:"due_date"`
	AmountVND int64  `json:"amount_vnd"`
}

// updateOccurrenceRequest is amount-only: a silent due_date would be a lie
type updateOccurrenceRequest struct {
	AmountVND int64 `json:"amount_vnd"`
}

type recordPaymentRequest struct {
	AmountVND int64  `json:"amount_vnd"`
	PaidAt    string `json:"paid_at"`
}

type occurrenceResponse struct {
	ID                 string `json:"id"`
	ObligationID       string `json:"obligation_id"`
	ObligationName     string `json:"obligation_name"`
	DueDate            string `json:"due_date"`
	ConfirmedAmountVND int64  `json:"confirmed_amount_vnd"`
	PaidAmountVND      int64  `json:"paid_amount_vnd"`
	RemainingAmountVND int64  `json:"remaining_amount_vnd"`
	Status             string `json:"status"`
}

func (h *Handler) createObligation(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request createObligationRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || utf8.RuneCountInString(request.Name) > 120 || hasNUL(request.Name) {
		writeError(w, http.StatusBadRequest, "name must contain 1 to 120 characters and no NUL characters")
		return
	}
	result, err := h.service.CreateObligation(r.Context(), request.Name, idempotencyKey)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) createOccurrence(w http.ResponseWriter, r *http.Request) {
	obligationID := r.PathValue("obligationID")
	if !validUUID(obligationID) {
		writeError(w, http.StatusBadRequest, "obligationID must be a UUID")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request createOccurrenceRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.AmountVND <= 0 {
		writeError(w, http.StatusBadRequest, "amount_vnd must be positive")
		return
	}
	dueDate, err := time.Parse("2006-01-02", request.DueDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "due_date must be YYYY-MM-DD")
		return
	}
	result, err := h.service.CreateOccurrence(r.Context(), obligationID, dueDate, request.AmountVND, idempotencyKey)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, responseFor(result))
}

func (h *Handler) updateOccurrence(w http.ResponseWriter, r *http.Request) {
	occurrenceID := r.PathValue("occurrenceID")
	if !validUUID(occurrenceID) {
		writeError(w, http.StatusBadRequest, "occurrenceID must be a UUID")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request updateOccurrenceRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.AmountVND <= 0 {
		writeError(w, http.StatusBadRequest, "amount_vnd must be positive")
		return
	}
	result, err := h.service.UpdateOccurrenceAmount(r.Context(), occurrenceID, request.AmountVND, idempotencyKey)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, responseFor(result))
}

func (h *Handler) getOccurrence(w http.ResponseWriter, r *http.Request) {
	occurrenceID := r.PathValue("occurrenceID")
	if !validUUID(occurrenceID) {
		writeError(w, http.StatusBadRequest, "occurrenceID must be a UUID")
		return
	}
	result, err := h.service.GetOccurrence(r.Context(), occurrenceID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, responseFor(result))
}

func (h *Handler) recordPayment(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	occurrenceID := r.PathValue("occurrenceID")
	if !validUUID(occurrenceID) {
		writeError(w, http.StatusBadRequest, "occurrenceID must be a UUID")
		return
	}
	var request recordPaymentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.AmountVND <= 0 {
		writeError(w, http.StatusBadRequest, "amount_vnd must be positive")
		return
	}
	paidAt, err := time.Parse(time.RFC3339, request.PaidAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "paid_at must be RFC3339")
		return
	}
	result, err := h.service.RecordPayment(r.Context(), occurrenceID, request.AmountVND, paidAt, idempotencyKey)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, responseFor(result))
}

func (h *Handler) listPayments(w http.ResponseWriter, r *http.Request) {
	occurrenceID := r.PathValue("occurrenceID")
	if !validUUID(occurrenceID) {
		writeError(w, http.StatusBadRequest, "occurrenceID must be a UUID")
		return
	}
	payments, err := h.service.ListPayments(r.Context(), occurrenceID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if payments == nil {
		payments = []obligation.Payment{} // the contract promises an array, never null
	}
	writeJSON(w, http.StatusOK, map[string][]obligation.Payment{"payments": payments})
}
func responseFor(occurrence obligation.Occurrence) occurrenceResponse {
	return occurrenceResponse{
		ID: occurrence.ID, ObligationID: occurrence.ObligationID, ObligationName: occurrence.ObligationName,
		DueDate: occurrence.DueDate.Format("2006-01-02"), ConfirmedAmountVND: occurrence.ConfirmedAmountVND,
		PaidAmountVND: occurrence.PaidAmountVND, RemainingAmountVND: occurrence.RemainingAmountVND(), Status: occurrence.Status(time.Now()),
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return false
	}
	return true
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, obligation.ErrNotFound):
		writeError(w, http.StatusNotFound, "occurrence not found")
	case errors.Is(err, obligation.ErrReferenceNotFound):
		writeError(w, http.StatusUnprocessableEntity, "referenced record does not exist")
	case errors.Is(err, obligation.ErrOverpayment):
		writeError(w, http.StatusUnprocessableEntity, "payment exceeds remaining amount")
	case errors.Is(err, obligation.ErrAmountBelowPaid):
		writeError(w, http.StatusUnprocessableEntity, "confirmed amount is below paid amount")
	case errors.Is(err, obligation.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "conflict", "error": "idempotency key was already used for a different request"})
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	code := "internal_error"
	if status == http.StatusBadRequest {
		code = "invalid_request"
	}
	if status == http.StatusNotFound {
		code = "not_found"
	}
	if status == http.StatusUnprocessableEntity {
		code = "unprocessable"
	}
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}
