CREATE TABLE node_resource_latest (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 sampled_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 cpu_percent smallint CHECK (cpu_percent BETWEEN 0 AND 100),
 memory_percent smallint CHECK (memory_percent BETWEEN 0 AND 100),
 disk_percent smallint CHECK (disk_percent BETWEEN 0 AND 100),
 conntrack_percent smallint CHECK (conntrack_percent BETWEEN 0 AND 100),
 clock_skew_seconds integer NOT NULL CHECK (clock_skew_seconds BETWEEN 0 AND 604860),
 dataplane_state text NOT NULL CHECK (dataplane_state IN ('not-applicable','in-sync','drifted','unknown')),
 cpu_high_since timestamptz,
 memory_high_since timestamptz,
 disk_high_since timestamptz,
 conntrack_high_since timestamptz,
 clock_skew_since timestamptz,
 dataplane_drift_since timestamptz,
 CHECK ((cpu_percent IS NOT NULL AND cpu_percent>=90)=(cpu_high_since IS NOT NULL)),
 CHECK ((memory_percent IS NOT NULL AND memory_percent>=90)=(memory_high_since IS NOT NULL)),
 CHECK ((disk_percent IS NOT NULL AND disk_percent>=85)=(disk_high_since IS NOT NULL)),
 CHECK ((conntrack_percent IS NOT NULL AND conntrack_percent>=80)=(conntrack_high_since IS NOT NULL)),
 CHECK ((clock_skew_seconds>120)=(clock_skew_since IS NOT NULL)),
 CHECK ((dataplane_state='drifted')=(dataplane_drift_since IS NOT NULL))
);

CREATE INDEX node_resource_latest_received ON node_resource_latest(received_at);
