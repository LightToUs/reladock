package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func insertGoogleAuthMember(t *testing.T, store *Store, email string) (tenantID, memberID string) {
	t.Helper()
	ctx := context.Background()
	tenantID, memberID = newID(), newID()
	if _, err := store.pool.Exec(ctx, "INSERT INTO tenants(id,name) VALUES($1,$2)", tenantID, "Google auth fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "INSERT INTO members(id,tenant_id,email,password_hash,role) VALUES($1,$2,$3,'fixture-password-hash','owner')", memberID, tenantID, email); err != nil {
		t.Fatal(err)
	}
	return tenantID, memberID
}

func TestMigration41GoogleIdentityConstraints(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, memberA := insertGoogleAuthMember(t, store, "google-a@example.com")
	_, memberB := insertGoogleAuthMember(t, store, "google-b@example.com")

	if _, err := store.pool.Exec(ctx, "INSERT INTO member_external_identities(provider,subject,member_id) VALUES('google','subject-a',$1)", memberA); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"subject cannot move":           "INSERT INTO member_external_identities(provider,subject,member_id) VALUES('google','subject-a','" + memberB + "')",
		"member has one Google subject": "INSERT INTO member_external_identities(provider,subject,member_id) VALUES('google','subject-b','" + memberA + "')",
		"provider is fixed":             "INSERT INTO member_external_identities(provider,subject,member_id) VALUES('github','subject-c','" + memberB + "')",
	} {
		if _, err := store.pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s constraint was not enforced", name)
		}
	}

	validHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.pool.Exec(ctx, `INSERT INTO oauth_login_attempts(state_hash,verifier,nonce,return_to,expires_at) VALUES($1,$2,$3,'/',now()+interval '10 minutes')`, validHash, "verifier_abcdefghijklmnopqrstuvwxyz0123456789ABCDE", "nonce_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]any{
		"plaintext state": {"plaintext-state", "verifier_abcdefghijklmnopqrstuvwxyz0123456789ABCDE", "nonce_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH", "/", time.Now().Add(10 * time.Minute)},
		"short verifier":  {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "short", "nonce_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH", "/", time.Now().Add(10 * time.Minute)},
		"short nonce":     {"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "verifier_abcdefghijklmnopqrstuvwxyz0123456789ABCDE", "short", "/", time.Now().Add(10 * time.Minute)},
		"open redirect":   {"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "verifier_abcdefghijklmnopqrstuvwxyz0123456789ABCDE", "nonce_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH", "https://evil.example", time.Now().Add(10 * time.Minute)},
		"expired":         {"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "verifier_abcdefghijklmnopqrstuvwxyz0123456789ABCDE", "nonce_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH", "/admin", time.Now().Add(-time.Minute)},
	} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO oauth_login_attempts(state_hash,verifier,nonce,return_to,expires_at) VALUES($1,$2,$3,$4,$5)`, args...); err == nil {
			t.Fatalf("%s constraint was not enforced", name)
		}
	}

	rows, err := store.pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('member_external_identities','oauth_login_attempts') AND column_name IN ('access_token','refresh_token','id_token','authorization_code')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var column string
		_ = rows.Scan(&column)
		t.Fatalf("provider secret column exists: %s", column)
	}
}

func TestGoogleLoginAttemptIsHashedSingleUseAndExpires(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	state := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	verifier := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	nonce := "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	if err := store.StartGoogleLogin(ctx, state, verifier, nonce, "/admin"); err != nil {
		t.Fatal(err)
	}
	var storedHash string
	var expiresAt time.Time
	if err := store.pool.QueryRow(ctx, "SELECT state_hash,expires_at FROM oauth_login_attempts").Scan(&storedHash, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if storedHash == state || storedHash != tokenHash(state) {
		t.Fatalf("stored state hash = %q", storedHash)
	}
	if remaining := time.Until(expiresAt); remaining < 9*time.Minute || remaining > 10*time.Minute+time.Second {
		t.Fatalf("attempt lifetime = %v", remaining)
	}
	attempt, err := store.ConsumeGoogleLogin(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Verifier != verifier || attempt.Nonce != nonce || attempt.ReturnTo != "/admin" {
		t.Fatalf("attempt = %+v", attempt)
	}
	if _, err = store.ConsumeGoogleLogin(ctx, state); err != ErrUnauthorized {
		t.Fatalf("replay err = %v, want ErrUnauthorized", err)
	}

	expiredState := "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	if err = store.StartGoogleLogin(ctx, expiredState, verifier, nonce, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE oauth_login_attempts SET created_at=now()-interval '20 minutes',expires_at=now()-interval '10 minutes' WHERE state_hash=$1`, tokenHash(expiredState)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ConsumeGoogleLogin(ctx, expiredState); err != ErrUnauthorized {
		t.Fatalf("expired attempt err = %v, want ErrUnauthorized", err)
	}

	var before int
	if err = store.pool.QueryRow(ctx, "SELECT count(*) FROM oauth_login_attempts").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]struct{ state, verifier, nonce, returnTo string }{
		"short state":    {"short", verifier, nonce, "/"},
		"short verifier": {"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", "short", nonce, "/"},
		"short nonce":    {"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", verifier, "short", "/"},
		"open redirect":  {"GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG", verifier, nonce, "https://evil.example"},
	} {
		if err = store.StartGoogleLogin(ctx, input.state, input.verifier, input.nonce, input.returnTo); err != ErrInvalid {
			t.Fatalf("%s err = %v, want ErrInvalid", name, err)
		}
	}
	var after int
	if err = store.pool.QueryRow(ctx, "SELECT count(*) FROM oauth_login_attempts").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("invalid attempts wrote rows: before=%d after=%d", before, after)
	}
}

func TestCompleteGoogleLoginLinksExistingVerifiedEmailAndCreatesSession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, memberID := insertGoogleAuthMember(t, store, "member@example.com")
	who, token, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "google-subject-a", Email: "Member@Example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if who.MemberID != memberID || who.Email != "member@example.com" || who.Role != "owner" || who.PlatformAdmin {
		t.Fatalf("identity = %+v", who)
	}
	if len(token) != 43 {
		t.Fatalf("session token length = %d", len(token))
	}
	if loaded, err := store.Session(ctx, token); err != nil || loaded.MemberID != memberID {
		t.Fatalf("session identity = %+v, err = %v", loaded, err)
	}
	var bindings, links, logins int
	if err := store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM member_external_identities WHERE provider='google' AND subject='google-subject-a' AND member_id=$1),
 (SELECT count(*) FROM audit_logs WHERE action='account.google_link' AND actor_id=$1),
 (SELECT count(*) FROM audit_logs WHERE action='account.login' AND actor_id=$1)`, memberID).Scan(&bindings, &links, &logins); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 || links != 1 || logins != 1 {
		t.Fatalf("bindings=%d links=%d logins=%d", bindings, links, logins)
	}
}

func TestCompleteGoogleLoginRejectsUnknownOrUnverifiedEmailWithoutWrites(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	insertGoogleAuthMember(t, store, "known@example.com")
	for name, identity := range map[string]GoogleIdentity{
		"unknown":    {Subject: "unknown-subject", Email: "unknown@example.com", EmailVerified: true},
		"unverified": {Subject: "unverified-subject", Email: "known@example.com", EmailVerified: false},
	} {
		if _, _, err := store.CompleteGoogleLogin(ctx, identity); err != ErrUnauthorized {
			t.Fatalf("%s err = %v, want ErrUnauthorized", name, err)
		}
	}
	var bindings, sessions, audits int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_external_identities),(SELECT count(*) FROM sessions),(SELECT count(*) FROM audit_logs)`).Scan(&bindings, &sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || sessions != 0 || audits != 0 {
		t.Fatalf("failed login wrote bindings=%d sessions=%d audits=%d", bindings, sessions, audits)
	}
}

func TestCompleteGoogleLoginUsesSubjectAfterBindingAndRejectsConflicts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, memberA := insertGoogleAuthMember(t, store, "subject-a@example.com")
	_, memberB := insertGoogleAuthMember(t, store, "subject-b@example.com")
	if _, _, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "stable-subject", Email: "subject-a@example.com", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	who, _, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "stable-subject", Email: "subject-b@example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if who.MemberID != memberA || who.MemberID == memberB {
		t.Fatalf("stable subject moved to %+v", who)
	}
	if _, _, err = store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "second-subject", Email: "subject-a@example.com", EmailVerified: true}); err != ErrUnauthorized {
		t.Fatalf("second subject for one member err = %v", err)
	}
	var bindings int
	if err = store.pool.QueryRow(ctx, "SELECT count(*) FROM member_external_identities").Scan(&bindings); err != nil || bindings != 1 {
		t.Fatalf("bindings = %d, err = %v", bindings, err)
	}
}

func TestCompleteGoogleLoginConcurrentFirstBinding(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, memberID := insertGoogleAuthMember(t, store, "concurrent@example.com")
	type result struct {
		who   Identity
		token string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			who, token, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "concurrent-subject", Email: "concurrent@example.com", EmailVerified: true})
			results <- result{who: who, token: token, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	tokens := map[string]bool{}
	for result := range results {
		if result.err != nil || result.who.MemberID != memberID {
			t.Fatalf("concurrent result = %+v", result)
		}
		if _, err := store.Session(ctx, result.token); err != nil {
			t.Fatalf("concurrent session: %v", err)
		}
		tokens[result.token] = true
	}
	if len(tokens) != 2 {
		t.Fatalf("distinct session tokens = %d", len(tokens))
	}
	var bindings, links, logins int
	if err := store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM member_external_identities),
 (SELECT count(*) FROM audit_logs WHERE action='account.google_link'),
 (SELECT count(*) FROM audit_logs WHERE action='account.login')`).Scan(&bindings, &links, &logins); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 || links != 1 || logins != 2 {
		t.Fatalf("bindings=%d links=%d logins=%d", bindings, links, logins)
	}
}

func TestCompleteGoogleLoginRechecksTenantAndPrivileges(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	tenantID, memberID := insertGoogleAuthMember(t, store, "privilege@example.com")
	identity := GoogleIdentity{Subject: "privilege-subject", Email: "privilege@example.com", EmailVerified: true}
	if _, _, err := store.CompleteGoogleLogin(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE members SET role='viewer',is_platform_admin=true WHERE id=$1", memberID); err != nil {
		t.Fatal(err)
	}
	who, token, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: identity.Subject, Email: "changed@example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if who.Role != "viewer" || !who.PlatformAdmin || who.Email != "privilege@example.com" {
		t.Fatalf("current database identity not re-read: %+v", who)
	}
	if _, err = store.pool.Exec(ctx, "UPDATE tenants SET status='suspended' WHERE id=$1", tenantID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.CompleteGoogleLogin(ctx, identity); err != ErrUnauthorized {
		t.Fatalf("suspended tenant login err = %v", err)
	}
	if _, err = store.Session(ctx, token); err != ErrUnauthorized {
		t.Fatalf("session remained authorized after suspension: %v", err)
	}
}

func TestCompleteGoogleLoginAuditFailureRollsBackBindingAndSession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	insertGoogleAuthMember(t, store, "audit-failure@example.com")
	if _, err := store.pool.Exec(ctx, `
CREATE FUNCTION reject_google_auth_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.action IN ('account.google_link','account.login') THEN RAISE EXCEPTION 'test Google audit failure'; END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER reject_google_auth_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_google_auth_audit();`); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "audit-subject", Email: "audit-failure@example.com", EmailVerified: true})
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("audit failure result = %v", err)
	}
	var bindings, sessions, audits int
	if err = store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_external_identities),(SELECT count(*) FROM sessions),(SELECT count(*) FROM audit_logs)`).Scan(&bindings, &sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || sessions != 0 || audits != 0 {
		t.Fatalf("audit failure committed bindings=%d sessions=%d audits=%d", bindings, sessions, audits)
	}
}
