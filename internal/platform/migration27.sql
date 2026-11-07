CREATE TABLE line_offerings (
 id uuid PRIMARY KEY,
 code text NOT NULL UNIQUE CHECK (code ~ '^[a-z][a-z0-9-]{0,63}$'),
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
 node_id text NOT NULL REFERENCES nodes(id),
 source_market text NOT NULL CHECK (length(source_market) BETWEEN 1 AND 80),
 destination_market text NOT NULL CHECK (length(destination_market) BETWEEN 1 AND 80),
 state text NOT NULL DEFAULT 'draft'
  CHECK (state IN ('draft','published','paused','sold-out','retired')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>=1),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX line_offerings_node_state ON line_offerings(node_id,state,code);

-- Nullable only for immutable versions created before concrete line offerings
-- existed. New application writes require an offering and never guess one.
ALTER TABLE plan_price_versions
 ADD COLUMN offering_id uuid REFERENCES line_offerings(id);
CREATE INDEX plan_price_versions_offering ON plan_price_versions(offering_id,code,version DESC);
