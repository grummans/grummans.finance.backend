-- No data change. Adding is_estimate to an occurrence request could have rewritten
-- every stored request fingerprint, but the store now reads a row keyed before the
-- flag existed as well, so nothing here has to be touched: no digest is rebuilt, no
-- key changes meaning, and a downgrade has no data to undo.
SELECT 1;
