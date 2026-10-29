package platform

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func namedTestStore(t *testing.T, s *Store, name string) *Store {
	t.Helper()
	cfg := s.pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return &Store{pool: p}
}
func waitTestLock(t *testing.T, s *Store, ctx context.Context, name string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", name).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not reach lock barrier")
}

func TestLoginExpiredSessionDoesNotDeadlockPrivilegeChange(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	who, _ := register(t, s, "login-lock@example.com")
	if _, err := s.pool.Exec(ctx, "UPDATE sessions SET expires_at=now()-interval '1 minute' WHERE member_id=$1", who.MemberID); err != nil {
		t.Fatal(err)
	}
	barrier := "login-barrier-" + newID()
	control, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback(context.Background())
	if _, err = control.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", barrier); err != nil {
		t.Fatal(err)
	}
	trigger := `CREATE FUNCTION login_grant_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(hashtextextended('` + barrier + `',0)); RETURN NEW; END; $$; CREATE TRIGGER login_grant_barrier AFTER UPDATE OF is_platform_admin ON members FOR EACH ROW EXECUTE FUNCTION login_grant_barrier()`
	if _, err = s.pool.Exec(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	grantName, loginName := "grant-"+newID(), "login-"+newID()
	grant, login := namedTestStore(t, s, grantName), namedTestStore(t, s, loginName)
	done := make(chan error, 2)
	go func() { done <- grant.SetPlatformAdmin(ctx, who.Email, true) }()
	waitTestLock(t, s, ctx, grantName)
	go func() { _, _, e := login.Login(ctx, who.Email, "long-test-password-42"); done <- e }()
	waitTestLock(t, s, ctx, loginName)
	if err = control.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case e := <-done:
			if e != nil {
				t.Fatalf("login/grant concurrency: %v", e)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestBlockedNodeWriteDoesNotBlockAnotherTenantNodeOrAdminRead(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	a, _ := register(t, s, "slow-node@example.com")
	b, _ := register(t, s, "fast-node@example.com")
	admin, _ := adminIdentity(t, s)
	node := "fast-" + newID()
	if err := s.AddNode(ctx, node, "Fast", "Test", "0.0.0.0"); err != nil {
		t.Fatal(err)
	}
	control, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback(context.Background())
	if err = lockNode(ctx, control, "hk-01"); err != nil {
		t.Fatal(err)
	}
	label := "slow-write-" + newID()
	slow := namedTestStore(t, s, label)
	done := make(chan error, 1)
	go func() { _, e := slow.CreateRule(ctx, a, "slow-node-request", ruleInput("tcp")); done <- e }()
	waitTestLock(t, s, ctx, label)
	fastCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	input := ruleInput("tcp")
	input.NodeID = node
	if _, err = s.CreateRule(fastCtx, b, "fast-node-request", input); err != nil {
		t.Fatalf("unrelated tenant blocked: %v", err)
	}
	if _, err = s.AdminOverview(fastCtx, admin); err != nil {
		t.Fatalf("unrelated admin read blocked: %v", err)
	}
	if err = control.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
