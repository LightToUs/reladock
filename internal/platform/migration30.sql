-- Package-first flow: a paid subscription may wait for the customer to bind it
-- to a new rule. Existing bound periods remain unchanged.
DO $$
BEGIN
 IF to_regclass('rule_subscriptions') IS NOT NULL
    AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='rule_subscriptions' AND column_name='rule_id') THEN
  ALTER TABLE rule_subscriptions ALTER COLUMN rule_id DROP NOT NULL;
 END IF;
 IF to_regclass('billing_ledger') IS NOT NULL THEN
  ALTER TABLE billing_ledger DROP CONSTRAINT billing_ledger_kind_check;
  ALTER TABLE billing_ledger ADD CONSTRAINT billing_ledger_kind_check CHECK (
   (kind='local-adjustment' AND price_version_id IS NULL) OR
   (kind='subscription-debit' AND amount_cents<0 AND price_version_id IS NOT NULL) OR
   (kind='payment-credit' AND amount_cents>0 AND rule_id IS NULL AND price_version_id IS NULL) OR
   (kind='payment-refund' AND amount_cents<0 AND rule_id IS NULL AND price_version_id IS NULL)
  );
 END IF;
END;
$$;
