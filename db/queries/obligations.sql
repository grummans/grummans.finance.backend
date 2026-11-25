-- name: CreateObligation :one
INSERT INTO obligations (name, currency)
VALUES ($1, $2)
RETURNING id, name, currency, created_at;

-- name: CreateOccurrence :one
INSERT INTO obligation_occurrences (obligation_id, due_date, confirmed_amount_vnd)
VALUES ($1, $2, $3)
RETURNING id, obligation_id, due_date, confirmed_amount_vnd, created_at;

-- name: GetOccurrence :one
SELECT o.id,
       o.obligation_id,
       b.name AS obligation_name,
       o.due_date,
       o.confirmed_amount_vnd,
       COALESCE(SUM(p.amount_vnd) FILTER (WHERE p.reversed_at IS NULL), 0)::bigint AS paid_amount_vnd
FROM obligation_occurrences o
JOIN obligations b ON b.id = o.obligation_id
LEFT JOIN payments p ON p.occurrence_id = o.id
WHERE o.id = $1
GROUP BY o.id, b.name;

-- name: GetIdempotentPaymentOccurrence :one
SELECT o.id,
       o.obligation_id,
       b.name AS obligation_name,
       o.due_date,
       o.confirmed_amount_vnd,
       COALESCE(SUM(p2.amount_vnd) FILTER (WHERE p2.reversed_at IS NULL), 0)::bigint AS paid_amount_vnd
FROM payments p
JOIN obligation_occurrences o ON o.id = p.occurrence_id
JOIN obligations b ON b.id = o.obligation_id
LEFT JOIN payments p2 ON p2.occurrence_id = o.id
WHERE p.idempotency_key = $1
GROUP BY o.id, b.name;
