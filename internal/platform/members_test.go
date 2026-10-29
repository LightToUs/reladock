package platform

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// Apply v4 only inside the isolated test schema until the parent registers it.
func membersStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	sql, err := os.ReadFile("migration4.sql")
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = s.pool.QueryRow(context.Background(), "SELECT to_regclass('member_invitations') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err = s.pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestInvitationsAreEmailBoundOneTimeAndTenantScoped(t *testing.T) {
	s := membersStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "invite-owner@example.com")
	other, _ := register(t, s, "other-owner@example.com")
	inv, raw, err := s.CreateInvitation(ctx, owner, " Member@Example.com ", "admin", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err = s.pool.QueryRow(ctx, "SELECT token_hash FROM member_invitations WHERE id=$1", inv.ID).Scan(&hash); err != nil || hash == raw || len(hash) != 64 {
		t.Fatalf("unsafe token storage: %v", err)
	}
	if _, _, err = s.AcceptInvitation(ctx, raw, "wrong@example.com", "long-invited-password"); err != ErrUnauthorized {
		t.Fatalf("email binding: %v", err)
	}
	if err = s.RevokeInvitation(ctx, other, inv.ID); err != ErrNotFound {
		t.Fatalf("tenant boundary: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, e := s.AcceptInvitation(ctx, raw, "member@example.com", "long-invited-password")
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		} else if e != ErrUnauthorized {
			t.Fatal(e)
		}
	}
	if successes != 1 {
		t.Fatalf("one-time acceptance: %d", successes)
	}
	member, memberSession, err := s.Login(ctx, "member@example.com", "long-invited-password")
	if err != nil || member.Role != "admin" || member.PlatformAdmin || member.TenantID != owner.TenantID {
		t.Fatalf("member: %+v %v", member, err)
	}
	if _, _, err = s.CreateInvitation(ctx, owner, "owner2@example.com", "owner", time.Now().Add(time.Hour)); err != ErrInvalid {
		t.Fatalf("owner creation: %v", err)
	}
	if err = s.ChangeMemberRole(ctx, owner, owner.MemberID, "viewer"); err != ErrForbidden {
		t.Fatalf("owner modification: %v", err)
	}
	_, memberKey, err := s.CreateAPIKey(ctx, member, "old-admin", "write", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, delegated, err := s.CreateInvitation(ctx, member, "delegated@example.com", "admin", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeMemberRole(ctx, owner, member.MemberID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Session(ctx, memberSession); err != ErrUnauthorized {
		t.Fatalf("role change retained session: %v", err)
	}
	if _, err = s.APIKey(ctx, memberKey); err != ErrUnauthorized {
		t.Fatalf("role change retained key: %v", err)
	}
	if _, _, err = s.AcceptInvitation(ctx, delegated, "delegated@example.com", "long-invited-password"); err != ErrUnauthorized {
		t.Fatalf("downgrade retained invitation: %v", err)
	}
	if _, err = s.CreateRule(ctx, member, "stale-member-rule", ruleInput("tcp")); err != ErrForbidden {
		t.Fatalf("stale rule writer: %v", err)
	}
	if _, _, err = s.CreateInvitation(ctx, member, "stale@example.com", "viewer", time.Now().Add(time.Hour)); err != ErrForbidden {
		t.Fatalf("stale role: %v", err)
	}
}

func TestAPIKeyScopesRevocationAndStaleWriteAuthorization(t *testing.T) {
	s := membersStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "key-owner@example.com")
	if err := s.SetPlatformAdmin(ctx, owner.Email, true); err != nil {
		t.Fatal(err)
	}
	key, raw, err := s.CreateAPIKey(ctx, owner, "automation", "read", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	who, err := s.APIKey(ctx, raw)
	if err != nil || who.Role != "viewer" || who.PlatformAdmin {
		t.Fatalf("read ceiling/platform: %+v %v", who, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(WithAPIKey(ctx, raw), tx, who); err != ErrForbidden {
		t.Fatalf("read key write: %v", err)
	}
	tx.Rollback(ctx)
	if err = s.RevokeAPIKey(ctx, owner, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.APIKey(ctx, raw); err != ErrUnauthorized {
		t.Fatalf("revoked authentication: %v", err)
	}
	_, raw, err = s.CreateAPIKey(ctx, owner, "writer", "write", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	who, err = s.APIKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE api_keys SET revoked_at=now() WHERE token_hash=$1", tokenHash(raw)); err != nil {
		t.Fatal(err)
	}
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(WithAPIKey(ctx, raw), tx, who); err != ErrUnauthorized {
		t.Fatalf("stale key write: %v", err)
	}
}

func TestMembersAuditFailureRollsBackInvitationAndRole(t *testing.T) {
	s := membersStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "audit-owner@example.com")
	_, raw, err := s.CreateInvitation(ctx, owner, "audit-member@example.com", "admin", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	member, _, err := s.AcceptInvitation(ctx, raw, "audit-member@example.com", "long-invited-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT fail_members_audit CHECK (action NOT IN ('member.role','invitation.create','api-key.create')) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeMemberRole(ctx, owner, member.MemberID, "viewer"); err == nil {
		t.Fatal("role survived failed audit")
	}
	if _, _, err = s.CreateInvitation(ctx, owner, "rollback@example.com", "viewer", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("invitation survived failed audit")
	}
	if _, _, err = s.CreateAPIKey(ctx, owner, "rollback", "write", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("key survived failed audit")
	}
	var role string
	var invitations, keys int
	if err = s.pool.QueryRow(ctx, "SELECT role FROM members WHERE id=$1", member.MemberID).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM member_invitations WHERE email='rollback@example.com'").Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM api_keys").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || invitations != 0 || keys != 0 {
		t.Fatalf("rollback: role=%s invitations=%d keys=%d", role, invitations, keys)
	}
}

func TestInvitationExpiryRevocationAndExistingAccountCannotResetPassword(t *testing.T) {
	s := membersStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "expiry-owner@example.com")
	for _, mode := range []string{"expired", "revoked", "existing"} {
		t.Run(mode, func(t *testing.T) {
			email := mode + "@example.com"
			inv, raw, err := s.CreateInvitation(ctx, owner, email, "viewer", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			want := ErrUnauthorized
			switch mode {
			case "expired":
				_, err = s.pool.Exec(ctx, `UPDATE member_invitations SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE id=$1`, inv.ID)
			case "revoked":
				err = s.RevokeInvitation(ctx, owner, inv.ID)
			case "existing":
				register(t, s, email)
				want = ErrConflict
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.AcceptInvitation(ctx, raw, email, "different-password-42"); err != want {
				t.Fatalf("accept: %v want %v", err, want)
			}
			if mode == "existing" {
				if _, _, err = s.Login(ctx, email, "long-test-password-42"); err != nil {
					t.Fatalf("existing account password changed: %v", err)
				}
			}
		})
	}
}

func TestAPIKeyViewerCeilingExpiryTenantBoundaryAndGlobalSerialization(t *testing.T) {
	s := membersStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "serialization-owner@example.com")
	other, _ := register(t, s, "key-other@example.com")
	_, raw, err := s.CreateInvitation(ctx, owner, "viewer-key@example.com", "viewer", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	viewer, _, err := s.AcceptInvitation(ctx, raw, "viewer-key@example.com", "long-invited-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateAPIKey(ctx, viewer, "escalate", "write", time.Now().Add(time.Hour)); err != ErrForbidden {
		t.Fatalf("viewer escalated: %v", err)
	}
	key, raw, err := s.CreateAPIKey(ctx, viewer, "viewer", "read", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeAPIKey(ctx, other, key.ID); err != ErrNotFound {
		t.Fatalf("other tenant revoked key: %v", err)
	}
	keys, err := s.ListAPIKeys(ctx, other)
	if err != nil || len(keys) != 0 {
		t.Fatalf("key list tenant isolation: %v %v", keys, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE api_keys SET scope='write' WHERE id=$1`, key.ID); err != nil {
		t.Fatal(err)
	}
	who, err := s.APIKey(ctx, raw)
	if err != nil || who.Role != "viewer" {
		t.Fatalf("member ceiling: %+v %v", who, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE api_keys SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE id=$1`, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.APIKey(ctx, raw); err != ErrUnauthorized {
		t.Fatalf("expiry: %v", err)
	}
	key, raw, err = s.CreateAPIKey(ctx, owner, "writer", "write", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	if err = s.pool.QueryRow(ctx, `SELECT token_hash FROM api_keys WHERE id=$1`, key.ID).Scan(&digest); err != nil || digest == raw || len(digest) != 64 {
		t.Fatalf("key stored raw: %v", err)
	}
	who, err = s.APIKey(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(WithAPIKey(ctx, raw), tx, who); err != nil {
		t.Fatal(err)
	}
	// A revoker cannot finish while an authorized transaction owns the lock.
	limited, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	err = s.RevokeAPIKey(limited, owner, key.ID)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("revocation did not wait for in-flight authorization lock: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeAPIKey(ctx, owner, key.ID); err != nil {
		t.Fatal(err)
	}
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.AuthorizeWrite(WithAPIKey(ctx, raw), tx, who); err != ErrUnauthorized {
		t.Fatalf("cached identity survived revocation: %v", err)
	}
}

func TestInvitationAcceptanceAuditFailureLeavesTokenUsable(t *testing.T) {
	s := membersStore(t)
	ctx := context.Background()
	owner, _ := register(t, s, "accept-audit@example.com")
	inv, raw, err := s.CreateInvitation(ctx, owner, "accept-rollback@example.com", "viewer", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE audit_logs ADD CONSTRAINT fail_accept CHECK(action<>'invitation.accept') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptInvitation(ctx, raw, inv.Email, "long-invited-password"); err == nil {
		t.Fatal("acceptance ignored failed audit")
	}
	var accepted *time.Time
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT accepted_at FROM member_invitations WHERE id=$1`, inv.ID).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM members WHERE email=$1`, inv.Email).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if accepted != nil || count != 0 {
		t.Fatal("failed audit consumed invitation or created member")
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE audit_logs DROP CONSTRAINT fail_accept`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptInvitation(ctx, raw, inv.Email, "long-invited-password"); err != nil {
		t.Fatalf("rolled-back token unusable: %v", err)
	}
}
