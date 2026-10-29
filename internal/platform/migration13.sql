ALTER TABLE config_outbox DROP CONSTRAINT config_outbox_state_check;
ALTER TABLE config_outbox ADD CONSTRAINT config_outbox_state_check CHECK (state IN ('queued','leased','applying','applied','validated','failed','superseded'));
CREATE UNIQUE INDEX one_application_per_node ON config_outbox(node_id) WHERE state='applying';
ALTER TABLE nodes ADD COLUMN applied_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE relay_rules ADD COLUMN ever_dispatched boolean NOT NULL DEFAULT false;
ALTER TABLE relay_rules ADD COLUMN applied_status text NOT NULL DEFAULT 'unknown';
