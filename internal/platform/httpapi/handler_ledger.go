package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"grummans.finance.backend/internal/ledger"
)

const (
	defaultTransactionPageSize = 50
	maxTransactionPageSize     = 200
)

type createAccountRequest struct {
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	OpeningBalanceVND int64  `json:"opening_balance_vnd"`
	CreditLimitVND    *int64 `json:"credit_limit_vnd"`
}

type createCategoryRequest struct {
	Name string `json:"name"`
}

type createTransactionRequest struct {
	AccountID   string  `json:"account_id"`
	ToAccountID string  `json:"to_account_id"`
	Type        string  `json:"type"`
	AmountVND   int64   `json:"amount_vnd"`
	OccurredAt  string  `json:"occurred_at"`
	CategoryID  *string `json:"category_id"`
	Note        *string `json:"note"`
}

type updateTransactionRequest struct {
	// RawMessage distinguishes an absent field (nil) from an explicit null.
	CategoryID json.RawMessage `json:"category_id"`
}

type linkPaymentRequest struct {
	TransactionID string `json:"transaction_id"`
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request createAccountRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || utf8.RuneCountInString(request.Name) > 120 || hasNUL(request.Name) {
		writeError(w, http.StatusBadRequest, "name must contain 1 to 120 characters and no NUL characters")
		return
	}
	kind := strings.ToUpper(strings.TrimSpace(request.Kind))
	switch kind {
	case "CASH", "BANK", "CREDIT":
	default:
		writeError(w, http.StatusBadRequest, "kind must be CASH, BANK or CREDIT")
		return
	}
	if request.OpeningBalanceVND < 0 {
		writeError(w, http.StatusBadRequest, "opening_balance_vnd must not be negative")
		return
	}
	if kind == "CREDIT" {
		if request.CreditLimitVND == nil || *request.CreditLimitVND <= 0 {
			writeError(w, http.StatusBadRequest, "credit_limit_vnd must be positive for a CREDIT account")
			return
		}
		if request.OpeningBalanceVND != 0 {
			writeError(w, http.StatusBadRequest, "a CREDIT account carries no opening balance")
			return
		}
	} else if request.CreditLimitVND != nil {
		writeError(w, http.StatusBadRequest, "only a CREDIT account has a credit limit")
		return
	}
	result, err := h.ledger.CreateAccount(r.Context(), ledger.AccountInput{
		Name:              request.Name,
		Kind:              kind,
		OpeningBalanceVND: request.OpeningBalanceVND,
		CreditLimitVND:    request.CreditLimitVND,
		IdempotencyKey:    &idempotencyKey,
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.ledger.ListAccounts(r.Context())
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]ledger.Account{"accounts": accounts})
}

func (h *Handler) createCategory(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request createCategoryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || utf8.RuneCountInString(request.Name) > 120 || hasNUL(request.Name) {
		writeError(w, http.StatusBadRequest, "name must contain 1 to 120 characters and no NUL characters")
		return
	}
	result, err := h.ledger.CreateCategory(r.Context(), request.Name, idempotencyKey)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) listCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := h.ledger.ListCategories(r.Context())
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]ledger.Category{"categories": categories})
}

func (h *Handler) createTransaction(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request createTransactionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	transactionType := strings.ToUpper(strings.TrimSpace(request.Type))
	switch ledger.Type(transactionType) {
	case ledger.TypeExpense, ledger.TypeIncome, ledger.TypeRepayment, ledger.TypeFee, ledger.TypeRefund, ledger.TypeTransfer:
	default:
		writeError(w, http.StatusBadRequest, "type must be EXPENSE, INCOME, TRANSFER, REPAYMENT, FEE or REFUND")
		return
	}
	if !validUUID(request.AccountID) {
		writeError(w, http.StatusBadRequest, "account_id must be a UUID")
		return
	}
	if request.AmountVND <= 0 {
		writeError(w, http.StatusBadRequest, "amount_vnd must be positive")
		return
	}
	occurredAt, err := time.Parse(time.RFC3339, request.OccurredAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "occurred_at must be RFC3339")
		return
	}
	if request.CategoryID != nil {
		if !validUUID(*request.CategoryID) {
			writeError(w, http.StatusBadRequest, "category_id must be a UUID")
			return
		}
	}
	note, ok := normalizedNote(w, request.Note)
	if !ok {
		return
	}
	if transactionType == string(ledger.TypeTransfer) {
		if !validUUID(request.ToAccountID) {
			writeError(w, http.StatusBadRequest, "to_account_id must be a UUID for a transfer")
			return
		}
		if strings.EqualFold(request.ToAccountID, request.AccountID) {
			writeError(w, http.StatusBadRequest, "a transfer needs two different accounts")
			return
		}
		if request.CategoryID != nil {
			writeError(w, http.StatusBadRequest, "a transfer has no category")
			return
		}
	} else if request.ToAccountID != "" {
		writeError(w, http.StatusBadRequest, "to_account_id is only valid for a transfer")
		return
	}

	result, err := h.ledger.CreateTransaction(r.Context(), ledger.TransactionInput{
		AccountID:      request.AccountID,
		ToAccountID:    request.ToAccountID,
		Type:           ledger.Type(transactionType),
		AmountVND:      request.AmountVND,
		OccurredAt:     occurredAt,
		CategoryID:     request.CategoryID,
		Note:           note,
		IdempotencyKey: &idempotencyKey,
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string][]ledger.Transaction{"transactions": result})
}

func (h *Handler) listTransactions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := ledger.Filter{Limit: defaultTransactionPageSize}
	if raw := query.Get("from"); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
			return
		}
		filter.From, filter.HasFrom = parsed, true
	}
	if raw := query.Get("to"); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to must be YYYY-MM-DD")
			return
		}
		filter.To, filter.HasTo = parsed, true
	}
	if filter.HasFrom && filter.HasTo && filter.To.Before(filter.From) {
		writeError(w, http.StatusBadRequest, "to must not be before from")
		return
	}
	if raw := query.Get("category_id"); raw != "" {
		if !validUUID(raw) {
			writeError(w, http.StatusBadRequest, "category_id must be a UUID")
			return
		}
		filter.CategoryID = &raw
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxTransactionPageSize {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		filter.Limit = limit
	}
	if raw := query.Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			writeError(w, http.StatusBadRequest, "offset must not be negative")
			return
		}
		filter.Offset = offset
	}

	transactions, err := h.ledger.ListTransactions(r.Context(), filter)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]ledger.Transaction{"transactions": transactions})
}

func (h *Handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	transactionID := r.PathValue("transactionID")
	if !validUUID(transactionID) {
		writeError(w, http.StatusBadRequest, "transactionID must be a UUID")
		return
	}
	result, err := h.ledger.GetTransaction(r.Context(), transactionID)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) updateTransaction(w http.ResponseWriter, r *http.Request) {
	transactionID := r.PathValue("transactionID")
	if !validUUID(transactionID) {
		writeError(w, http.StatusBadRequest, "transactionID must be a UUID")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request updateTransactionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.CategoryID) == 0 {
		writeError(w, http.StatusBadRequest, "category_id is required; send null to clear it")
		return
	}
	var categoryID *string
	if string(request.CategoryID) != "null" {
		var value string
		if err := json.Unmarshal(request.CategoryID, &value); err != nil || !validUUID(value) {
			writeError(w, http.StatusBadRequest, "category_id must be a UUID or null")
			return
		}
		categoryID = &value
	}
	result, err := h.ledger.UpdateTransactionCategory(r.Context(), transactionID, categoryID, idempotencyKey)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) linkPaymentTransaction(w http.ResponseWriter, r *http.Request) {
	paymentID := r.PathValue("paymentID")
	if !validUUID(paymentID) {
		writeError(w, http.StatusBadRequest, "paymentID must be a UUID")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(w, idempotencyKey) {
		return
	}
	var request linkPaymentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !validUUID(request.TransactionID) {
		writeError(w, http.StatusBadRequest, "transaction_id must be a UUID")
		return
	}
	result, err := h.ledger.LinkPaymentTransaction(r.Context(), paymentID, request.TransactionID, idempotencyKey)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// PostgreSQL text columns reject NUL bytes, so they must be refused at the boundary.
func hasNUL(value string) bool {
	return strings.ContainsRune(value, 0)
}

func validIdempotencyKey(w http.ResponseWriter, key string) bool {
	if key == "" || utf8.RuneCountInString(key) > 255 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must contain 1 to 255 characters")
		return false
	}
	if hasNUL(key) {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must not contain NUL characters")
		return false
	}
	return true
}

func normalizedNote(w http.ResponseWriter, note *string) (*string, bool) {
	if note == nil {
		return nil, true
	}
	trimmed := strings.TrimSpace(*note)
	if utf8.RuneCountInString(trimmed) > 500 || hasNUL(trimmed) {
		writeError(w, http.StatusBadRequest, "note must be at most 500 characters and no NUL characters")
		return nil, false
	}
	if trimmed == "" {
		return nil, true
	}
	return &trimmed, true
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func writeLedgerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		writeError(w, http.StatusNotFound, "record not found")
	case errors.Is(err, ledger.ErrReferenceNotFound):
		writeError(w, http.StatusUnprocessableEntity, "referenced account, category or transaction does not exist")
	case errors.Is(err, ledger.ErrInvalidChange):
		writeError(w, http.StatusUnprocessableEntity, "request violates a financial rule")
	case errors.Is(err, ledger.ErrAlreadyLinked):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "conflict", "error": "payment or transaction is already linked"})
	case errors.Is(err, ledger.ErrAlreadyExists):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "conflict", "error": "record already exists"})
	case errors.Is(err, ledger.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "conflict", "error": "idempotency key was already used for a different request"})
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}
