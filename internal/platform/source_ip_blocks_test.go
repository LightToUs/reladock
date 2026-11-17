package platform

import (
	"context"
	"errors"
	"testing"
)

func sourceIPBlockFixture(t *testing.T) (*Store, Identity, Identity, string) {
	t.Helper()
	s, customer, ruleID, _ := sourceFlowFixture(t)
	admin, _ := adminIdentity(t, s)
	return s, admin, customer, ruleID
}

func TestAdminSourceIPBlocksAreVersionedAuditedAndIdempotent(t *testing.T) {
	s, admin, customer, ruleID := sourceIPBlockFixture(t)
	ctx := context.Background()
	var outboxBefore int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}

	block, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{
		CIDR: "198.51.100.23", Reason: "abuse report", DesiredVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if block.CIDR != "198.51.100.23/32" || block.RequestedVersion != 2 || block.Applied {
		t.Fatalf("added block: %+v", block)
	}
	var version int64
	var outboxAfter int
	var action string
	if err = s.pool.QueryRow(ctx, `SELECT desired_version FROM relay_rules WHERE id=$1`, ruleID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT action FROM platform_audit_logs WHERE resource_id=$1 ORDER BY id DESC LIMIT 1`, block.ID).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if version != 2 || outboxAfter != outboxBefore+1 || action != "rule.source-ip-block.add" {
		t.Fatalf("version=%d outbox=%d/%d action=%q", version, outboxAfter, outboxBefore, action)
	}

	replayed, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{
		CIDR: "198.51.100.23/32", Reason: "ignored on exact replay", DesiredVersion: 2,
	})
	if err != nil || replayed.ID != block.ID || replayed.RequestedVersion != 2 {
		t.Fatalf("replay: %+v err=%v", replayed, err)
	}
	var outboxReplay int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM config_outbox WHERE node_id='hk-01'`).Scan(&outboxReplay); err != nil || outboxReplay != outboxAfter {
		t.Fatalf("replay outbox=%d want=%d err=%v", outboxReplay, outboxAfter, err)
	}

	items, err := s.AdminSourceIPBlocks(ctx, admin, customer.TenantID, ruleID)
	if err != nil || len(items.Items) != 1 || items.Items[0].ID != block.ID || items.DesiredVersion != 2 {
		t.Fatalf("list: %+v err=%v", items, err)
	}
	removed, err := s.AdminRemoveSourceIPBlock(ctx, admin, customer.TenantID, ruleID, block.ID, 2)
	if err != nil || removed.State != "removed" || removed.RemovedVersion != 3 {
		t.Fatalf("removed: %+v err=%v", removed, err)
	}
	items, err = s.AdminSourceIPBlocks(ctx, admin, customer.TenantID, ruleID)
	if err != nil || len(items.Items) != 1 || items.Items[0].State != "removed" || items.Items[0].Applied || items.DesiredVersion != 3 {
		t.Fatalf("pending removal list: %+v err=%v", items, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE relay_rules SET applied_version=3 WHERE id=$1`, ruleID); err != nil {
		t.Fatal(err)
	}
	items, err = s.AdminSourceIPBlocks(ctx, admin, customer.TenantID, ruleID)
	if err != nil || len(items.Items) != 0 || items.AppliedVersion != 3 {
		t.Fatalf("applied removal list: %+v err=%v", items, err)
	}
}

func TestAdminSourceIPBlocksRejectInvalidScopeAndCIDR(t *testing.T) {
	s, admin, customer, ruleID := sourceIPBlockFixture(t)
	ctx := context.Background()
	valid := SourceIPBlockInput{CIDR: "198.51.100.0/24", Reason: "abuse", DesiredVersion: 1}
	if _, err := s.AdminAddSourceIPBlock(ctx, customer, customer.TenantID, ruleID, valid); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant member added platform block: %v", err)
	}
	if _, err := s.AdminAddSourceIPBlock(ctx, admin, newID(), ruleID, valid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant rule: %v", err)
	}
	for _, cidr := range []string{"", "not-an-ip", "2001:db8::/32", "0.0.0.0/0", "198.51.100.1/24", "127.0.0.1", "224.0.0.0/4", "0.0.0.0/32"} {
		input := valid
		input.CIDR = cidr
		if _, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("CIDR %q accepted: %v", cidr, err)
		}
	}
	if _, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{CIDR: "198.51.100.0/24", Reason: "", DesiredVersion: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty reason accepted: %v", err)
	}
	if _, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{CIDR: "198.51.100.0/24", Reason: "abuse", DesiredVersion: 2}); !errors.Is(err, ErrConflict) {
		t.Fatalf("future desired version accepted: %v", err)
	}
}

func TestAdminAddSourceIPBlockRollsBackWhenAuditFails(t *testing.T) {
	s, admin, customer, ruleID := sourceIPBlockFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_source_ip_block_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'rule.source-ip-block.%' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_source_ip_block_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_source_ip_block_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{CIDR: "198.51.100.44", Reason: "abuse", DesiredVersion: 1}); err == nil {
		t.Fatal("audit failure did not fail mutation")
	}
	var blocks int
	var version int64
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM rule_source_ip_blocks),(SELECT desired_version FROM relay_rules WHERE id=$1)`, ruleID).Scan(&blocks, &version); err != nil {
		t.Fatal(err)
	}
	if blocks != 0 || version != 1 {
		t.Fatalf("rollback blocks=%d version=%d", blocks, version)
	}
}

func TestAdminAddSourceIPBlockEnforcesPerRuleCapacity(t *testing.T) {
	s, admin, customer, ruleID := sourceIPBlockFixture(t)
	ctx := context.Background()
	for i := 0; i < maxActiveSourceIPBlocksPerRule; i++ {
		// Use inet arithmetic to avoid formatting fixture addresses in Go.
		if _, err := s.pool.Exec(ctx, `INSERT INTO rule_source_ip_blocks(id,tenant_id,rule_id,cidr,reason,requested_version,created_by) VALUES($1,$2,$3,(inet '10.0.0.0'+$4)::cidr,'fixture',1,$5)`, newID(), customer.TenantID, ruleID, i*256, admin.MemberID); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.AdminAddSourceIPBlock(ctx, admin, customer.TenantID, ruleID, SourceIPBlockInput{CIDR: "198.51.100.1", Reason: "over capacity", DesiredVersion: 1})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity error: %v", err)
	}
}
