package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func offeringInput(code string) LineOfferingInput {
	return LineOfferingInput{
		Code:              code,
		Name:              "  香港大陆优化线路  ",
		NodeID:            "hk-01",
		SourceMarket:      "  中国大陆  ",
		DestinationMarket: "  香港 / 亚洲  ",
		Notes:             "  晚高峰需人工复核  ",
	}
}

func TestAdminOfferingCreateReplayAuthorizationAndValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "offering-owner@example.com")

	if _, err := s.AdminCreateOffering(ctx, owner, offeringInput("hk-mainland")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant owner created offering: %v", err)
	}
	created, err := s.AdminCreateOffering(ctx, admin, offeringInput("hk-mainland"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != "hk-mainland" || created.Name != "香港大陆优化线路" || created.NodeID != "hk-01" || created.SourceMarket != "中国大陆" || created.DestinationMarket != "香港 / 亚洲" || created.Notes != "晚高峰需人工复核" || created.State != OfferingStateDraft || created.Revision != 1 || created.ResourceMode != ResourceModeShared || !created.Deletable {
		t.Fatalf("created offering: %+v", created)
	}
	replayed, err := s.AdminCreateOffering(ctx, admin, offeringInput("hk-mainland"))
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("create replay: %+v %v", replayed, err)
	}
	changed := offeringInput("hk-mainland")
	changed.Name = "Different"
	if _, err = s.AdminCreateOffering(ctx, admin, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting code replay: %v", err)
	}

	page, err := s.AdminOfferings(ctx, admin, 1)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("offering page: %+v %v", page, err)
	}
	if _, err = s.AdminOfferings(ctx, owner, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant owner listed offerings: %v", err)
	}

	for _, bad := range []LineOfferingInput{
		{},
		{Code: "Bad Code", Name: "Bad", NodeID: "hk-01", SourceMarket: "A", DestinationMarket: "B"},
		{Code: "missing-node", Name: "Bad", NodeID: "missing", SourceMarket: "A", DestinationMarket: "B"},
		{Code: "notes-too-long", Name: "Bad", NodeID: "hk-01", Notes: strings.Repeat("界", 501)},
	} {
		if _, err = s.AdminCreateOffering(ctx, admin, bad); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid offering accepted: %+v %v", bad, err)
		}
	}
}

func TestAdminOfferingAllowsBlankMarketsAndNotes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	input := offeringInput("hk-draft")
	input.SourceMarket = "  "
	input.DestinationMarket = ""
	input.Notes = "  等待线路资料  "
	created, err := s.AdminCreateOffering(ctx, admin, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.SourceMarket != "" || created.DestinationMarket != "" || created.Notes != "等待线路资料" {
		t.Fatalf("created offering: %+v", created)
	}
}

func TestAdminOfferingDeleteRequiresUnreferencedDraftAndRevision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	owner, _ := register(t, s, "offering-delete-owner@example.com")

	unreferenced, err := s.AdminCreateOffering(ctx, admin, offeringInput("delete-me"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteOffering(ctx, owner, unreferenced.ID, unreferenced.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant owner deleted offering: %v", err)
	}
	if err = s.AdminDeleteOffering(ctx, admin, unreferenced.ID, unreferenced.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if err = s.AdminDeleteOffering(ctx, admin, unreferenced.ID, unreferenced.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteOffering(ctx, admin, unreferenced.ID, unreferenced.Revision); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted offering still exists: %v", err)
	}

	linked, err := s.AdminCreateOffering(ctx, admin, offeringInput("linked-draft"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdminCreatePlan(ctx, admin, PlanInput{OfferingID: linked.ID, Code: "linked-plan", Version: 1, Name: "Linked", PriceCents: 100, QuotaBytes: 1000, BandwidthMbps: 10, PeriodSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteOffering(ctx, admin, linked.ID, linked.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("linked draft deleted: %v", err)
	}
	page, err := s.AdminOfferings(ctx, admin, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == linked.ID && item.Deletable {
			t.Fatal("linked draft advertised as deletable")
		}
	}

	nonDraft, err := s.AdminCreateOffering(ctx, admin, offeringInput("paused-offer"))
	if err != nil {
		t.Fatal(err)
	}
	nonDraft, err = s.AdminSetOfferingState(ctx, admin, nonDraft.ID, OfferingStateInput{ExpectedRevision: nonDraft.Revision, State: OfferingStatePaused})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteOffering(ctx, admin, nonDraft.ID, nonDraft.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-draft deleted: %v", err)
	}

	var deletes int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM platform_audit_logs WHERE resource_id=$1 AND action='offering.delete'", unreferenced.ID).Scan(&deletes); err != nil || deletes != 1 {
		t.Fatalf("delete audit count=%d err=%v", deletes, err)
	}
}

func TestAdminOfferingDeleteAuditFailureRollsBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	created, err := s.AdminCreateOffering(ctx, admin, offeringInput("delete-rollback"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_offering_delete_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='offering.delete' THEN RAISE EXCEPTION 'reject offering delete audit'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_offering_delete_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_offering_delete_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = s.AdminDeleteOffering(ctx, admin, created.ID, created.Revision); err == nil {
		t.Fatal("delete succeeded without audit")
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM line_offerings WHERE id=$1", created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delete rollback count=%d err=%v", count, err)
	}
}

func TestAdminOfferingDeleteRechecksReferencesAfterWaitingForPlanCreator(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	created, err := s.AdminCreateOffering(ctx, admin, offeringInput("delete-race"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM line_offerings WHERE id=$1 FOR SHARE", created.ID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		deleted <- s.AdminDeleteOffering(ctx, admin, created.ID, created.Revision)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE pid<>pg_backend_pid()
			 AND datname=current_database()
			 AND state='active'
			 AND wait_event_type='Lock'
			 AND query LIKE 'SELECT o.id,o.code,o.name,o.node_id%FOR UPDATE OF o FOR SHARE OF n'
		)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delete did not reach the offering lock wait")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO plan_price_versions(id,offering_id,code,version,name,price_cents,quota_bytes,bandwidth_mbps,period_seconds,resource_mode) VALUES($1,$2,'race-plan',1,'Race',100,1000,10,86400,'shared')", newID(), created.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-deleted:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("delete after concurrent plan commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete remained blocked after plan transaction committed")
	}
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM line_offerings WHERE id=$1", created.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("offering count=%d err=%v", count, err)
	}
}

func TestAdminOfferingCreateNormalizesMobileCapitalization(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)

	created, err := s.AdminCreateOffering(ctx, admin, offeringInput("HK-Mainland"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != "hk-mainland" {
		t.Fatalf("code=%q want hk-mainland", created.Code)
	}
}

func TestAdminOfferingStateTransitionsAllowUnprovisionedNodeAndUseRevision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	created, err := s.AdminCreateOffering(ctx, admin, offeringInput("hk-state"))
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 1, State: OfferingStatePublished})
	if err != nil || published.State != OfferingStatePublished || published.Revision != 2 {
		t.Fatalf("publish unprovisioned offering: %+v %v", published, err)
	}
	var nodeStatus string
	if err = s.pool.QueryRow(ctx, "SELECT status FROM nodes WHERE id='hk-01'").Scan(&nodeStatus); err != nil || nodeStatus != "unprovisioned" {
		t.Fatalf("node status=%q err=%v", nodeStatus, err)
	}
	if _, err = s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 1, State: OfferingStatePaused}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale state update: %v", err)
	}
	paused, err := s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 2, State: OfferingStatePaused})
	if err != nil || paused.Revision != 3 {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	soldOut, err := s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 3, State: OfferingStateSoldOut})
	if err != nil || soldOut.Revision != 4 {
		t.Fatalf("sold out: %+v %v", soldOut, err)
	}
	republished, err := s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 4, State: OfferingStatePublished})
	if err != nil || republished.Revision != 5 {
		t.Fatalf("republish: %+v %v", republished, err)
	}
	retired, err := s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 5, State: OfferingStateRetired})
	if err != nil || retired.Revision != 6 {
		t.Fatalf("retire: %+v %v", retired, err)
	}
	if _, err = s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 6, State: OfferingStatePublished}); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired offering reopened: %v", err)
	}
	if _, err = s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 6, State: "online"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid state accepted: %v", err)
	}

	var creates, changes int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action='offering.create'),count(*) FILTER (WHERE action='offering.state') FROM platform_audit_logs WHERE resource_id=$1", created.ID).Scan(&creates, &changes); err != nil {
		t.Fatal(err)
	}
	if creates != 1 || changes != 5 {
		t.Fatalf("audit creates=%d changes=%d", creates, changes)
	}
}

func TestAdminOfferingPublishRejectsUnavailableNodeStatuses(t *testing.T) {
	for _, status := range []string{"maintenance", "draining", "retired"} {
		t.Run(status, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			admin, _ := adminIdentity(t, s)
			created, err := s.AdminCreateOffering(ctx, admin, offeringInput("hk-"+status+"-sale"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(ctx, "UPDATE nodes SET status=$1 WHERE id='hk-01'", status); err != nil {
				t.Fatal(err)
			}
			if _, err = s.AdminSetOfferingState(ctx, admin, created.ID, OfferingStateInput{ExpectedRevision: 1, State: OfferingStatePublished}); !errors.Is(err, ErrNodeNotAllocatable) {
				t.Fatalf("%s node offering published: %v", status, err)
			}
		})
	}
}

func TestAdminOfferingAuditFailureRollsBackCreate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, s)
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_offering_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='offering.create' THEN RAISE EXCEPTION 'reject offering audit'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_offering_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_offering_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminCreateOffering(ctx, admin, offeringInput("hk-rollback")); err == nil {
		t.Fatal("create succeeded without audit")
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM line_offerings WHERE code='hk-rollback'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back offerings=%d err=%v", count, err)
	}
}
