package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// requestFingerprint canonicalizes a create request so a retried idempotency key
// is compared with the original request, never with the mutable stored row.
// UUIDs are lowercased and times truncated to microseconds because that is what
// PostgreSQL stores; anything else would make an identical retry look different.
func requestFingerprint(fields ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func accountFingerprint(input AccountInput) string {
	creditLimit := ""
	if input.CreditLimitVND != nil {
		creditLimit = strconv.FormatInt(*input.CreditLimitVND, 10)
	}
	return requestFingerprint("account", input.Name, input.Kind,
		strconv.FormatInt(input.OpeningBalanceVND, 10), creditLimit)
}

func transactionFingerprint(input TransactionInput) string {
	categoryID := ""
	if input.CategoryID != nil {
		categoryID = strings.ToLower(*input.CategoryID)
	}
	note := ""
	if input.Note != nil {
		note = *input.Note
	}
	toAccountID := strings.ToLower(input.ToAccountID)
	return requestFingerprint(string(input.Type), DirectionFor(input.Type),
		strings.ToLower(input.AccountID), toAccountID,
		strconv.FormatInt(input.AmountVND, 10),
		input.OccurredAt.UTC().Truncate(1e3).Format("2006-01-02T15:04:05.000000Z07:00"),
		categoryID, note)
}
