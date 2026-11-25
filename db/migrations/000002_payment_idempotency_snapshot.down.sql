ALTER TABLE payments
    DROP CONSTRAINT payments_paid_amount_vnd_after_positive,
    DROP COLUMN paid_amount_vnd_after;
