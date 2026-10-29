CREATE TABLE operations_incidents (
 id uuid PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 kind text NOT NULL CHECK (kind IN ('contact-stale','config-failed')),
 state text NOT NULL CHECK (state IN ('open','resolved')),
 opened_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 resolved_at timestamptz,
 occurrences bigint NOT NULL DEFAULT 1 CHECK (occurrences>0),
 UNIQUE(node_id,kind),
 CHECK ((state='resolved')=(resolved_at IS NOT NULL))
);
CREATE INDEX operations_incidents_recent ON operations_incidents(opened_at DESC,id);
CREATE TABLE support_tickets (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 created_by uuid NOT NULL,
 subject text NOT NULL CHECK (length(subject) BETWEEN 1 AND 160),
 status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','waiting-customer','resolved')),
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(id,tenant_id),
 FOREIGN KEY(created_by,tenant_id) REFERENCES members(id,tenant_id)
);
CREATE INDEX support_tickets_tenant_recent ON support_tickets(tenant_id,updated_at DESC,id);
CREATE TABLE support_messages (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 ticket_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 author_id uuid NOT NULL REFERENCES members(id),
 author_kind text NOT NULL CHECK (author_kind IN ('tenant','platform')),
 body text NOT NULL CHECK (length(body) BETWEEN 1 AND 4000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(ticket_id,tenant_id) REFERENCES support_tickets(id,tenant_id)
);
CREATE INDEX support_messages_thread ON support_messages(ticket_id,id DESC);
