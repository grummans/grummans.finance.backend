ALTER TABLE payments ADD COLUMN paid_amount_vnd_after bigint;

WITH payment_totals AS (
    SELECT p.id,
           COALESCE(
               SUM(previous.amount_vnd) FILTER (
                   WHERE previous.reversed_at IS NULL OR previous.reversed_at > p.created_at
               ),
               p.amount_vnd
           ) AS paid_amount_vnd_after
    FROM payments p
    JOIN payments previous
      ON previous.occurrence_id = p.occurrence_id
     AND (previous.created_at, previous.id) <= (p.created_at, p.id)
    GROUP BY p.id, p.created_at, p.amount_vnd
)
UPDATE payments p
SET paid_amount_vnd_after = payment_totals.paid_amount_vnd_after
FROM payment_totals
WHERE p.id = payment_totals.id;

ALTER TABLE payments
    ALTER COLUMN paid_amount_vnd_after SET NOT NULL,
    ADD CONSTRAINT payments_paid_amount_vnd_after_positive CHECK (paid_amount_vnd_after > 0);
