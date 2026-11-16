CREATE TABLE member_external_identities (
 provider text NOT NULL CHECK (provider = 'google'),
 subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255 AND subject !~ '[[:cntrl:][:space:]]'),
 member_id uuid NOT NULL REFERENCES members(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(provider,subject),
 UNIQUE(provider,member_id)
);

CREATE TABLE oauth_login_attempts (
 state_hash text PRIMARY KEY CHECK (state_hash ~ '^[0-9a-f]{64}$'),
 verifier text NOT NULL CHECK (length(verifier) BETWEEN 43 AND 128 AND verifier ~ '^[A-Za-z0-9_-]+$'),
 nonce text NOT NULL CHECK (length(nonce) BETWEEN 43 AND 128 AND nonce ~ '^[A-Za-z0-9_-]+$'),
 return_to text NOT NULL CHECK (return_to IN ('/','/admin')),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK (expires_at > created_at),
 CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);

CREATE INDEX oauth_login_attempts_expiry ON oauth_login_attempts(expires_at);

CREATE TABLE google_link_challenges (
 token_hash text PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
 subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 255 AND subject !~ '[[:cntrl:][:space:]]'),
 email text NOT NULL CHECK (length(email) BETWEEN 3 AND 254 AND email = lower(email) AND email = btrim(email)),
 return_to text NOT NULL CHECK (return_to IN ('/','/admin')),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK (expires_at > created_at),
 CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);

CREATE INDEX google_link_challenges_expiry ON google_link_challenges(expires_at);
