ALTER TABLE node_source_flow_latest
 ADD COLUMN degraded_since timestamptz,
 ADD COLUMN normal_since timestamptz;

UPDATE node_source_flow_latest
 SET degraded_since=CASE WHEN collector_status IN ('degraded','unavailable') THEN received_at END,
     normal_since=CASE WHEN collector_status='normal' THEN received_at END;

ALTER TABLE node_source_flow_latest ADD CONSTRAINT node_source_flow_latest_sustained_state CHECK (
 (collector_status='normal' AND normal_since IS NOT NULL AND degraded_since IS NULL)
 OR
 (collector_status IN ('degraded','unavailable') AND degraded_since IS NOT NULL AND normal_since IS NULL)
);

ALTER TABLE operations_incidents
 DROP CONSTRAINT operations_incidents_kind_check,
 ADD CONSTRAINT operations_incidents_kind_check CHECK (kind IN (
  'contact-stale','config-failed','certificate-expiring','certificate-expired',
  'certificate-rotation-failed','resource-cpu-high','resource-memory-high',
  'resource-disk-high','resource-conntrack-high','resource-clock-skew',
  'dataplane-drift','source-flow-collector-degraded'
 ));

ALTER TABLE applied_source_flow_rules
 ADD COLUMN superseded_at timestamptz;

CREATE INDEX source_flow_events_observed_idx
 ON source_flow_events(observed_at DESC, event_id DESC);
