ALTER TABLE nodes ADD COLUMN agent_kernel_boot_id uuid;
ALTER TABLE nodes ADD COLUMN agent_journal_sequence bigint NOT NULL DEFAULT 0 CHECK (agent_journal_sequence>=0);

ALTER TABLE node_drain_attestations
 ADD COLUMN node_revision bigint NOT NULL DEFAULT 0 CHECK (node_revision>=0);

UPDATE node_drain_attestations
 SET consumed_at=COALESCE(consumed_at,clock_timestamp());

CREATE INDEX node_drain_attestations_unconsumed
 ON node_drain_attestations(node_id,received_at DESC,id DESC)
 WHERE consumed_at IS NULL;
