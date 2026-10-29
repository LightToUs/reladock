CREATE TABLE request_limits (
 key_hash text NOT NULL,
 category text NOT NULL CHECK (category IN ('api','auth')),
 requests integer NOT NULL CHECK (requests>0),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(key_hash,category)
);
CREATE INDEX request_limits_expiry ON request_limits(expires_at);
