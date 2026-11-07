package platform

import (
	"context"
	"testing"
)

func TestAdminWithdrawsUncontactedNodeAfterAdministrativeChanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	input := NodeInput{ID: "unused-history", Name: "Unused", Region: "HK", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}
	if err := s.AdminAddNode(ctx, admin, input); err != nil {
		t.Fatal(err)
	}
	revision := int64(1)
	check := func() {
		t.Helper()
		nodes, err := s.AdminNodes(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.ID == input.ID {
				if !n.Withdrawable {
					t.Fatalf("unused node at revision %d status %s is not withdrawable", revision, n.Status)
				}
				return
			}
		}
		t.Fatal("node missing")
	}
	input.Name = "Edited unused node"
	if err := s.AdminUpdateNode(ctx, admin, input, revision); err != nil {
		t.Fatal(err)
	}
	revision++
	check()
	for _, action := range []string{"retry", "retry", "maintenance"} {
		if err := s.AdminNodeAction(ctx, admin, input.ID, revision, action, ""); err != nil {
			t.Fatal(err)
		}
		revision++
		check()
	}
	if err := s.StartNodeDraining(ctx, admin, input.ID, revision); err != nil {
		t.Fatal(err)
	}
	revision++
	check()
	if err := s.AdminNodeAction(ctx, admin, input.ID, revision, "retry", ""); err != nil {
		t.Fatal(err)
	}
	revision++
	check()
	if err := s.AdminWithdrawNodeRegistration(ctx, admin, input.ID, revision-1); err != ErrConflict {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if err := s.AdminWithdrawNodeRegistration(ctx, admin, input.ID, revision); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"nodes", "config_outbox", "node_bootstrap"} {
		column := "node_id"
		if table == "nodes" {
			column = "id"
		}
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+column+"=$1", input.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	var audits int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE resource_id=$1 AND action='node.withdraw-registration'", input.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit=%d err=%v", audits, err)
	}
}

func TestAdminWithdrawRejectsHiddenContactHistoryAndOffering(t *testing.T) {
	for name, statement := range map[string]string{
		"superseded attempted config": `UPDATE config_outbox SET attempts=1 WHERE node_id=$1 AND generation=1`,
		"superseded lease":            `UPDATE config_outbox SET lease_token='past-lease' WHERE node_id=$1 AND generation=1`,
		"superseded report":           `UPDATE config_outbox SET reported_at=now() WHERE node_id=$1 AND generation=1`,
		"superseded failure":          `UPDATE config_outbox SET error_code='past-failure' WHERE node_id=$1 AND generation=1`,
		"superseded applied":          `UPDATE config_outbox SET state='applied' WHERE node_id=$1 AND generation=1`,
		"revoked identity history":    `INSERT INTO platform_audit_logs(actor_email,action,resource_id) VALUES('operator','node.enroll',$1)`,
		"retired node":                `UPDATE nodes SET status='retired' WHERE id=$1`,
		"offering":                    `INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market) VALUES(gen_random_uuid(),'blocked-offer','Bound offer',$1,'','')`,
	} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			admin, _ := adminIdentity(t, s)
			id := "blocked-history"
			if err := s.AdminAddNode(ctx, admin, NodeInput{ID: id, Name: "Unused", Region: "HK", ListenAddress: "0.0.0.0", CapacityMbps: 1000, ResourceMode: ResourceModeShared}); err != nil {
				t.Fatal(err)
			}
			if err := s.AdminNodeAction(ctx, admin, id, 1, "retry", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, statement, id); err != nil {
				t.Fatal(err)
			}
			nodes, err := s.AdminNodes(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range nodes {
				if n.ID == id && n.Withdrawable {
					t.Fatal("unsafe node marked withdrawable")
				}
			}
			if err := s.AdminWithdrawNodeRegistration(ctx, admin, id, 2); err != ErrConflict {
				t.Fatalf("unsafe withdrawal: %v", err)
			}
		})
	}
}
