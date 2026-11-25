-- Listing every payment of an occurrence (reversed ones included) needs an index
-- that the partial sum index cannot serve.
-- CONCURRENTLY: cmd/migrate runs each file as a plain statement batch (no wrapping
-- transaction), so building this index does not block payment writes.
CREATE INDEX CONCURRENTLY payments_occurrence_paid_at_idx
    ON payments (occurrence_id, paid_at DESC, id DESC);
