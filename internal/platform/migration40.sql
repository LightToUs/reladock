-- An empty Lightsail script means provisioning only: no SSH access is requested.
ALTER TABLE lightsail_instances
 DROP CONSTRAINT lightsail_instances_deployment_state_check,
 DROP CONSTRAINT lightsail_instances_script_size_check,
 DROP CONSTRAINT lightsail_instances_check,
 DROP CONSTRAINT lightsail_instances_check1,
 DROP CONSTRAINT lightsail_instances_check2;

ALTER TABLE lightsail_instances
 ADD CONSTRAINT lightsail_instances_deployment_state_check
  CHECK (deployment_state IN ('not-requested','pending','running','succeeded','failed','unknown')),
 ADD CONSTRAINT lightsail_instances_script_size_check
  CHECK (script_size BETWEEN 0 AND 65536),
 ADD CONSTRAINT lightsail_instances_execution_payload_check
  CHECK (
   (script_size=0 AND script_body IS NULL AND execution_started_at IS NULL AND deployment_state='not-requested')
   OR
   (script_size>0 AND deployment_state<>'not-requested' AND
    ((execution_started_at IS NULL AND script_body IS NOT NULL) OR
     (execution_started_at IS NOT NULL AND script_body IS NULL)))
  ),
 ADD CONSTRAINT lightsail_instances_script_body_size_check
  CHECK (script_body IS NULL OR octet_length(script_body)=script_size),
 ADD CONSTRAINT lightsail_instances_running_execution_check
  CHECK (deployment_state<>'running' OR execution_started_at IS NOT NULL);
