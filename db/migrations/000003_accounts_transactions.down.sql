ALTER TABLE payments DROP CONSTRAINT payments_transaction_id_unique;
ALTER TABLE payments DROP COLUMN transaction_id;

DROP TABLE transactions;
DROP TABLE categories;
DROP TABLE accounts;
