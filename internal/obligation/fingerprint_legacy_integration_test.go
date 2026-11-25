//go:build integration

package obligation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Adding is_estimate to a create request could have rewritten every stored request
// fingerprint. It does not: this slice leaves the digests alone and the store reads
// both forms, so a key recorded before the flag still replays and a retry that only
// flips the flag is still the different request it is.
func TestOccurrenceFingerprintKeepsLegacyReplayable(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	// the migrations are unreadable once this binary runs inside the database container
	for _, name := range []string{
		"000007_occurrence_estimate_fingerprint.up.sql",
		"000007_occurrence_estimate_fingerprint.down.sql",
	} {
		shipped, err := os.ReadFile("../../db/migrations/" + name)
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToUpper(string(shipped)), "UPDATE OBLIGATION_OCCURRENCES") {
			t.Fatalf("%s rewrites fingerprints again; keys recorded before is_estimate would stop replaying", name)
		}
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(pool.Close) // after the row cleanup registered below

	service := NewService(NewPostgresStore(pool))
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	obligation, err := service.CreateObligation(ctx, fmt.Sprintf("legacy-rent-%d", suffix), fmt.Sprintf("legacy-obligation-key-%d", suffix))
	if err != nil {
		t.Fatalf("creating obligation: %v", err)
	}

	dueDate := time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC)
	key := fmt.Sprintf("legacy-occurrence-key-%d", suffix)
	// the fingerprint the pre-slice code wrote: the same fields without is_estimate
	legacyFingerprint := requestFingerprint("occurrence", obligation.ID,
		dueDate.Format("2006-01-02T15:04:05.000000Z"), "250000")
	var occurrenceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO obligation_occurrences
			(obligation_id, due_date, confirmed_amount_vnd, idempotency_key, request_fingerprint)
		VALUES ($1, $2, 250000, $3, $4)
		RETURNING id::text`, obligation.ID, dueDate, key, legacyFingerprint).Scan(&occurrenceID); err != nil {
		t.Fatalf("inserting a pre-slice occurrence: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM obligation_occurrences WHERE id = $1`, occurrenceID); err != nil {
			t.Errorf("cleaning up occurrence: %v", err)
		}
	})

	replayed, err := service.CreateOccurrence(ctx, obligation.ID, dueDate, 250000, key)
	if err != nil {
		t.Fatalf("replaying a pre-slice occurrence: %v", err)
	}
	if replayed.ID != occurrenceID {
		t.Fatalf("replayed occurrence = %s, want %s", replayed.ID, occurrenceID)
	}
	// the flag moves a reserve from expected to confirmed, so it is part of the request
	if _, err := service.CreateEstimatedOccurrence(ctx, obligation.ID, dueDate, 250000, key); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("replaying with the other estimate flag = %v, want ErrIdempotencyKeyReused", err)
	}

	// an occurrence whose amount was edited keeps the fingerprint of its original
	// create request, which no formula over the current row can rebuild — the key
	// still has to replay
	editedDate := dueDate.AddDate(0, 0, 1)
	editedKey := fmt.Sprintf("legacy-edited-key-%d", suffix)
	editedFingerprint := requestFingerprint("occurrence", obligation.ID,
		editedDate.Format("2006-01-02T15:04:05.000000Z"), "250000")
	var editedID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO obligation_occurrences
			(obligation_id, due_date, confirmed_amount_vnd, idempotency_key, request_fingerprint)
		VALUES ($1, $2, 300000, $3, $4)
		RETURNING id::text`, obligation.ID, editedDate, editedKey, editedFingerprint).Scan(&editedID); err != nil {
		t.Fatalf("inserting an edited pre-slice occurrence: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM obligation_occurrences WHERE id = $1`, editedID); err != nil {
			t.Errorf("cleaning up the edited occurrence: %v", err)
		}
	})
	replayedEdited, err := service.CreateOccurrence(ctx, obligation.ID, editedDate, 250000, editedKey)
	if err != nil {
		t.Fatalf("replaying an edited pre-slice occurrence: %v", err)
	}
	if replayedEdited.ID != editedID {
		t.Fatalf("replayed edited occurrence = %s, want %s", replayedEdited.ID, editedID)
	}
	// the edited amount is still what the row holds; a retry with it is a new request
	if _, err := service.CreateOccurrence(ctx, obligation.ID, editedDate, 300000, editedKey); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key with another amount = %v, want ErrIdempotencyKeyReused", err)
	}
}
