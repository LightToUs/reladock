-- Old unbound reports remain unbound. Never infer a period from a receipt time
-- or silently erase previously consumed paid quota during this upgrade.
DO $$
BEGIN
 IF EXISTS (
  SELECT 1 FROM rule_subscriptions p LEFT JOIN billing_usage_totals u ON u.rule_id=p.rule_id
  WHERE COALESCE(p.final_upload_bytes,u.upload_bytes,0)>p.base_upload_bytes
     OR COALESCE(p.final_download_bytes,u.download_bytes,0)>p.base_download_bytes
 ) THEN
  RAISE EXCEPTION 'unattributed paid usage must be reconciled before migration 10';
 END IF;
END;
$$;

ALTER TABLE rule_subscriptions ADD CONSTRAINT subscription_identity UNIQUE(id,rule_id,tenant_id);
CREATE TABLE subscription_usage (
 subscription_id uuid PRIMARY KEY,
 rule_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 node_id text NOT NULL REFERENCES nodes(id),
 upload_bytes bigint NOT NULL DEFAULT 0 CHECK (upload_bytes>=0),
 download_bytes bigint NOT NULL DEFAULT 0 CHECK (download_bytes>=0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (upload_bytes::numeric+download_bytes::numeric<=9223372036854775807),
 UNIQUE(subscription_id,rule_id,tenant_id,node_id),
 FOREIGN KEY(subscription_id,rule_id,tenant_id) REFERENCES rule_subscriptions(id,rule_id,tenant_id)
);
INSERT INTO subscription_usage(subscription_id,rule_id,tenant_id,node_id)
 SELECT p.id,p.rule_id,p.tenant_id,r.node_id FROM rule_subscriptions p JOIN relay_rules r ON r.id=p.rule_id;

ALTER TABLE billing_usage_epochs ADD COLUMN subscription_id uuid;
ALTER TABLE billing_usage_reports ADD COLUMN subscription_id uuid;
ALTER TABLE billing_usage_epochs ADD CONSTRAINT epoch_subscription_identity
 FOREIGN KEY(subscription_id,rule_id,tenant_id,node_id) REFERENCES subscription_usage(subscription_id,rule_id,tenant_id,node_id);
ALTER TABLE billing_usage_reports ADD CONSTRAINT report_subscription_identity
 FOREIGN KEY(subscription_id,rule_id,tenant_id,node_id) REFERENCES subscription_usage(subscription_id,rule_id,tenant_id,node_id);
CREATE INDEX subscription_reports ON billing_usage_reports(subscription_id,received_at);

CREATE FUNCTION billing_preserve_usage_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.subscription_id IS DISTINCT FROM OLD.subscription_id OR NEW.rule_id<>OLD.rule_id
    OR NEW.tenant_id<>OLD.tenant_id OR NEW.node_id<>OLD.node_id THEN
  RAISE EXCEPTION 'usage period binding is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_TABLE_NAME='billing_usage_epochs' AND to_jsonb(NEW)->>'epoch' IS DISTINCT FROM to_jsonb(OLD)->>'epoch' THEN
  RAISE EXCEPTION 'usage epoch is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER epoch_subscription_immutable BEFORE UPDATE ON billing_usage_epochs
 FOR EACH ROW EXECUTE FUNCTION billing_preserve_usage_identity();
CREATE TRIGGER subscription_usage_identity_immutable BEFORE UPDATE ON subscription_usage
 FOR EACH ROW EXECUTE FUNCTION billing_preserve_usage_identity();

CREATE FUNCTION billing_verify_report_period() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bound_period uuid;
BEGIN
 SELECT subscription_id INTO bound_period FROM billing_usage_epochs WHERE rule_id=NEW.rule_id AND epoch=NEW.epoch;
 IF NOT FOUND OR bound_period IS DISTINCT FROM NEW.subscription_id THEN
  RAISE EXCEPTION 'report does not match epoch period' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER report_matches_epoch_period BEFORE INSERT ON billing_usage_reports
 FOR EACH ROW EXECUTE FUNCTION billing_verify_report_period();
