-- Metadata-only, transactionally captured incident transitions. No endpoint or
-- credential is stored here, and no external request is made by the database.
CREATE TABLE notification_outbox (
 sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
 event_id text PRIMARY KEY,
 incident_id uuid NOT NULL REFERENCES operations_incidents(id),
 occurrences bigint NOT NULL CHECK(occurrences>0),
 incident_state text NOT NULL CHECK(incident_state IN ('open','resolved')),
 payload jsonb NOT NULL CHECK(octet_length(payload::text)<=4096),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','claimed','delivered','exhausted')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 8),
 lease_token text,
 lease_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 last_error_code text NOT NULL DEFAULT '' CHECK(last_error_code IN ('','delivery-failed','lease-expired')),
 delivered_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(incident_id,occurrences,incident_state),
 CHECK(event_id='incident:'||incident_id::text||':'||occurrences::text||':'||incident_state),
 CHECK((state='claimed')=(lease_until IS NOT NULL)),
 CHECK(state<>'claimed' OR lease_token IS NOT NULL),
 CHECK((state='delivered')=(delivered_at IS NOT NULL))
);
CREATE INDEX notification_outbox_ready ON notification_outbox(state,next_attempt_at,sequence);
CREATE INDEX notification_outbox_expiry ON notification_outbox(lease_until) WHERE state='claimed';

CREATE FUNCTION notification_incident_payload(i uuid,n text,k text,s text,o bigint,at_time timestamptz)
 RETURNS jsonb LANGUAGE sql AS $$
 SELECT jsonb_build_object('eventId','incident:'||i::text||':'||o::text||':'||s,
 'incidentId',i,'nodeId',n,'kind',k,'state',s,'occurrences',o,'occurredAt',at_time)
$$;

CREATE FUNCTION notification_capture_incident() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW.state IS NOT DISTINCT FROM OLD.state
    AND NEW.occurrences IS NOT DISTINCT FROM OLD.occurrences THEN
  RETURN NEW;
 END IF;
 INSERT INTO notification_outbox(event_id,incident_id,occurrences,incident_state,payload)
 VALUES('incident:'||NEW.id::text||':'||NEW.occurrences::text||':'||NEW.state,
 NEW.id,NEW.occurrences,NEW.state,
 notification_incident_payload(NEW.id,NEW.node_id,NEW.kind,NEW.state,NEW.occurrences,
 CASE WHEN NEW.state='open' THEN NEW.opened_at ELSE NEW.resolved_at END))
 ON CONFLICT(incident_id,occurrences,incident_state) DO NOTHING;
 RETURN NEW;
END;
$$;
CREATE TRIGGER notification_incident_transition AFTER INSERT OR UPDATE ON operations_incidents
 FOR EACH ROW EXECUTE FUNCTION notification_capture_incident();

-- All future writers, including probe incidents, use this table-level trigger.
-- Backfill only current old state; no historical transition is invented.
INSERT INTO notification_outbox(event_id,incident_id,occurrences,incident_state,payload)
 SELECT 'incident:'||id::text||':'||occurrences::text||':'||state,id,occurrences,state,
 notification_incident_payload(id,node_id,kind,state,occurrences,
 CASE WHEN state='open' THEN opened_at ELSE resolved_at END)
 FROM operations_incidents ORDER BY opened_at,id
 ON CONFLICT(incident_id,occurrences,incident_state) DO NOTHING;

CREATE FUNCTION notification_preserve_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.sequence,NEW.event_id,NEW.incident_id,NEW.occurrences,NEW.incident_state,NEW.payload,NEW.created_at)
 IS DISTINCT FROM
 (OLD.sequence,OLD.event_id,OLD.incident_id,OLD.occurrences,OLD.incident_state,OLD.payload,OLD.created_at) THEN
  RAISE EXCEPTION 'notification event is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER notification_event_immutable BEFORE UPDATE ON notification_outbox
 FOR EACH ROW EXECUTE FUNCTION notification_preserve_event();
