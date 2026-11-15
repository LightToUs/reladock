-- Durable sequential cursor for automatic receive-only TRON USDT discovery.
-- The cursor stores no signing material and advances only across solidified blocks.
CREATE TABLE tron_payment_scan_cursors (
 id uuid PRIMARY KEY,
 network text NOT NULL CHECK (network='TRON'),
 contract_address text NOT NULL CHECK (contract_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
 collection_address text NOT NULL CHECK (collection_address ~ '^T[1-9A-HJ-NP-Za-km-z]{33}$'),
 next_block bigint NOT NULL CHECK (next_block>0),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_token uuid,
 lease_expires_at timestamptz,
 last_error text NOT NULL DEFAULT '' CHECK (length(last_error)<=120),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(network,contract_address,collection_address),
 CHECK ((lease_token IS NULL)=(lease_expires_at IS NULL))
);

CREATE INDEX tron_payment_scan_cursors_queue
 ON tron_payment_scan_cursors(next_attempt_at,next_block);
