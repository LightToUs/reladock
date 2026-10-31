ALTER TABLE operations_incidents
 DROP CONSTRAINT operations_incidents_kind_check,
 ADD CONSTRAINT operations_incidents_kind_check CHECK (kind IN (
  'contact-stale',
  'config-failed',
  'certificate-expiring',
  'certificate-expired',
  'certificate-rotation-failed'
 ));
