// Package idempotency claims a mutation key before an update runs, so a retried
// key replays the original outcome and a reused key with another payload is refused.
package idempotency

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrKeyReused means the key was already used for a different request.
var ErrKeyReused = errors.New("idempotency key was already used for a different request")

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Claim returns replayed=true when this exact request already claimed the key.
func Claim(ctx context.Context, querier querier, key, fingerprint string) (bool, error) {
	if key == "" {
		return false, errors.New("idempotency key is required")
	}
	var claimed int
	err := querier.QueryRow(ctx, `
		INSERT INTO mutation_keys (idempotency_key, request_fingerprint)
		VALUES ($1, $2)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING 1`, key, fingerprint).Scan(&claimed)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("claiming idempotency key: %w", err)
	}
	var stored string
	if err := querier.QueryRow(ctx, `
		SELECT request_fingerprint FROM mutation_keys WHERE idempotency_key = $1`,
		key).Scan(&stored); err != nil {
		return false, fmt.Errorf("reading idempotency key: %w", err)
	}
	if stored != fingerprint {
		return false, ErrKeyReused
	}
	return true, nil
}
