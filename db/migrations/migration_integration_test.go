//go:build integration

package migrations

import (
	"context"
	_ "embed"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed 000002_payment_idempotency_snapshot.up.sql
var paymentSnapshotMigration string

func TestPaymentSnapshotMigrationUsesPaymentTime(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for integration tests")
	}
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = tx.Exec(context.Background(), `CREATE TEMP TABLE payments (
		id uuid PRIMARY KEY, occurrence_id uuid NOT NULL, amount_vnd bigint NOT NULL,
		paid_at timestamptz NOT NULL, idempotency_key text NOT NULL, reversed_at timestamptz, created_at timestamptz NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = tx.Exec(context.Background(), `INSERT INTO payments (id, occurrence_id, amount_vnd, paid_at, idempotency_key, reversed_at, created_at) VALUES
		('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000010', 40, $1, 'a', $2, $1),
		('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000010', 20, $3, 'b', NULL, $3)`, created, created.Add(2*time.Hour), created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), paymentSnapshotMigration); err != nil {
		t.Fatal(err)
	}
	var snapshot int64
	if err := tx.QueryRow(context.Background(), `SELECT paid_amount_vnd_after FROM payments WHERE id = '00000000-0000-0000-0000-000000000002'`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot != 60 {
		t.Fatalf("second payment snapshot = %d, want 60", snapshot)
	}
}
