package platform

import (
	"context"
	"errors"
	"strings"
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
	passwordHash, err := hashPassword("long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "INSERT INTO members(id,tenant_id,email,password_hash,role) VALUES($1,$2,$3,$4,'owner')", memberID, tenantID, email, passwordHash); err != nil {
		t.Fatal(err)
	}
	return tenantID, memberID
}

func linkGoogleAuthMember(t *testing.T, store *Store, identity GoogleIdentity, linkToken, returnTo string) (Identity, string) {
	t.Helper()
	result, err := store.CompleteGoogleLogin(context.Background(), identity, linkToken, returnTo)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LinkRequired {
		t.Fatalf("first Google login did not require a password link: %+v", result)
	}
	who, token, linkedReturnTo, err := store.CompleteGoogleLink(context.Background(), linkToken, identity.Email, "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if linkedReturnTo != returnTo {
		t.Fatalf("link return target = %q, want %q", linkedReturnTo, returnTo)
	}
	return who, token
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
	if _, err := store.pool.Exec(ctx, `INSERT INTO google_link_challenges(token_hash,subject,email,return_to,expires_at) VALUES($1,'link-subject','google-a@example.com','/',now()+interval '10 minutes')`, validHash); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]any{
		"plaintext token": {"plaintext-token", "link-subject", "google-a@example.com", "/", time.Now().Add(10 * time.Minute)},
		"open redirect":   {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "link-subject", "google-a@example.com", "https://evil.example", time.Now().Add(10 * time.Minute)},
		"expired":         {"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "link-subject", "google-a@example.com", "/admin", time.Now().Add(-time.Minute)},
	} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO google_link_challenges(token_hash,subject,email,return_to,expires_at) VALUES($1,$2,$3,$4,$5)`, args...); err == nil {
			t.Fatalf("%s link challenge constraint was not enforced", name)
		}
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

func TestCompleteGoogleLoginRequiresPasswordLinkBeforeCreatingSession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, memberID := insertGoogleAuthMember(t, store, "member@example.com")
	linkToken := strings.Repeat("L", 43)
	result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "google-subject-a", Email: "Member@Example.com", EmailVerified: true}, linkToken, "/")
	if err != nil {
		t.Fatal(err)
	}
	if !result.LinkRequired || result.SessionToken != "" || result.Identity.MemberID != "" {
		t.Fatalf("unbound completion = %+v", result)
	}
	var challengeHash, subject, email string
	var bindings, sessions, audits int
	if err := store.pool.QueryRow(ctx, `SELECT token_hash,subject,email,
 (SELECT count(*) FROM member_external_identities),
 (SELECT count(*) FROM sessions),
 (SELECT count(*) FROM audit_logs)
 FROM google_link_challenges`).Scan(&challengeHash, &subject, &email, &bindings, &sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if challengeHash != tokenHash(linkToken) || challengeHash == linkToken || subject != "google-subject-a" || email != "member@example.com" || bindings != 0 || sessions != 0 || audits != 0 {
		t.Fatalf("challenge hash=%q subject=%q email=%q bindings=%d sessions=%d audits=%d", challengeHash, subject, email, bindings, sessions, audits)
	}
	who, token, returnTo, err := store.CompleteGoogleLink(ctx, linkToken, "MEMBER@example.com", "long-test-password-42")
	if err != nil {
		t.Fatal(err)
	}
	if who.MemberID != memberID || len(token) != 43 || returnTo != "/" {
		t.Fatalf("linked identity=%+v tokenLength=%d returnTo=%q", who, len(token), returnTo)
	}
	if loaded, err := store.Session(ctx, token); err != nil || loaded.MemberID != memberID {
		t.Fatalf("session identity = %+v, err = %v", loaded, err)
	}
	var links, logins, consumed int
	if err := store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM member_external_identities WHERE provider='google' AND subject='google-subject-a' AND member_id=$1),
 (SELECT count(*) FROM audit_logs WHERE action='account.google_link' AND actor_id=$1),
	 (SELECT count(*) FROM audit_logs WHERE action='account.login' AND actor_id=$1),
	 (SELECT count(*) FROM google_link_challenges WHERE consumed_at IS NOT NULL)`, memberID).Scan(&bindings, &links, &logins, &consumed); err != nil {
		t.Fatal(err)
	}
	if bindings != 1 || links != 1 || logins != 1 || consumed != 1 {
		t.Fatalf("bindings=%d links=%d logins=%d consumed=%d", bindings, links, logins, consumed)
	}
}

func TestCompleteGoogleLinkRejectsWrongCredentialsWithoutWritesAndAllowsRetry(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	insertGoogleAuthMember(t, store, "known@example.com")
	linkToken := strings.Repeat("M", 43)
	if _, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "credential-subject", Email: "known@example.com", EmailVerified: true}, linkToken, "/admin"); err != nil {
		t.Fatal(err)
	}
	for name, credentials := range map[string][2]string{
		"wrong email":    {"other@example.com", "long-test-password-42"},
		"wrong password": {"known@example.com", "wrong-password"},
	} {
		if _, _, _, err := store.CompleteGoogleLink(ctx, linkToken, credentials[0], credentials[1]); err != ErrUnauthorized {
			t.Fatalf("%s err = %v, want ErrUnauthorized", name, err)
		}
	}
	var bindings, sessions, audits, consumed int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_external_identities),(SELECT count(*) FROM sessions),(SELECT count(*) FROM audit_logs),(SELECT count(*) FROM google_link_challenges WHERE consumed_at IS NOT NULL)`).Scan(&bindings, &sessions, &audits, &consumed); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || sessions != 0 || audits != 0 || consumed != 0 {
		t.Fatalf("failed link wrote bindings=%d sessions=%d audits=%d consumed=%d", bindings, sessions, audits, consumed)
	}
	if _, _, _, err := store.CompleteGoogleLink(ctx, linkToken, "known@example.com", "long-test-password-42"); err != nil {
		t.Fatalf("valid retry failed: %v", err)
	}
}

func TestCompleteGoogleLinkRejectsExpiredAndReplayedChallenge(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	insertGoogleAuthMember(t, store, "expiry@example.com")
	expiredToken := strings.Repeat("N", 43)
	if result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "expiry-subject", Email: "expiry@example.com", EmailVerified: true}, expiredToken, "/"); err != nil || !result.LinkRequired {
		t.Fatalf("expired challenge setup = %+v err=%v", result, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE google_link_challenges SET created_at=now()-interval '20 minutes',expires_at=now()-interval '10 minutes' WHERE token_hash=$1`, tokenHash(expiredToken)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.CompleteGoogleLink(ctx, expiredToken, "expiry@example.com", "long-test-password-42"); err != ErrUnauthorized {
		t.Fatalf("expired challenge err=%v", err)
	}

	validToken := strings.Repeat("O", 43)
	if result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "replay-link-subject", Email: "expiry@example.com", EmailVerified: true}, validToken, "/admin"); err != nil || !result.LinkRequired {
		t.Fatalf("replay challenge setup = %+v err=%v", result, err)
	}
	if _, _, _, err := store.CompleteGoogleLink(ctx, validToken, "expiry@example.com", "long-test-password-42"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.CompleteGoogleLink(ctx, validToken, "expiry@example.com", "long-test-password-42"); err != ErrUnauthorized {
		t.Fatalf("replayed challenge err=%v", err)
	}
}

func TestCompleteGoogleLoginUsesSubjectAfterBindingAndRejectsConflicts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	_, memberA := insertGoogleAuthMember(t, store, "subject-a@example.com")
	_, memberB := insertGoogleAuthMember(t, store, "subject-b@example.com")
	linkGoogleAuthMember(t, store, GoogleIdentity{Subject: "stable-subject", Email: "subject-a@example.com", EmailVerified: true}, strings.Repeat("A", 43), "/")
	result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "stable-subject", Email: "subject-b@example.com", EmailVerified: true}, strings.Repeat("B", 43), "/admin")
	if err != nil {
		t.Fatal(err)
	}
	who := result.Identity
	if result.LinkRequired || len(result.SessionToken) != 43 {
		t.Fatalf("bound subject result = %+v", result)
	}
	if who.MemberID != memberA || who.MemberID == memberB {
		t.Fatalf("stable subject moved to %+v", who)
	}
	secondToken := strings.Repeat("C", 43)
	if pending, pendingErr := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "second-subject", Email: "subject-a@example.com", EmailVerified: true}, secondToken, "/"); pendingErr != nil || !pending.LinkRequired {
		t.Fatalf("second subject challenge = %+v err=%v", pending, pendingErr)
	}
	if _, _, _, err = store.CompleteGoogleLink(ctx, secondToken, "subject-a@example.com", "long-test-password-42"); err != ErrUnauthorized {
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
	linkTokens := []string{strings.Repeat("D", 43), strings.Repeat("E", 43)}
	for _, linkToken := range linkTokens {
		result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "concurrent-subject", Email: "concurrent@example.com", EmailVerified: true}, linkToken, "/")
		if err != nil || !result.LinkRequired {
			t.Fatalf("challenge result=%+v err=%v", result, err)
		}
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, linkToken := range linkTokens {
		wait.Add(1)
		go func(linkToken string) {
			defer wait.Done()
			<-start
			who, token, _, err := store.CompleteGoogleLink(ctx, linkToken, "concurrent@example.com", "long-test-password-42")
			results <- result{who: who, token: token, err: err}
		}(linkToken)
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
	linkGoogleAuthMember(t, store, identity, strings.Repeat("F", 43), "/")
	if _, err := store.pool.Exec(ctx, "UPDATE members SET role='viewer',is_platform_admin=true WHERE id=$1", memberID); err != nil {
		t.Fatal(err)
	}
	result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: identity.Subject, Email: "changed@example.com", EmailVerified: true}, strings.Repeat("G", 43), "/")
	if err != nil {
		t.Fatal(err)
	}
	who, token := result.Identity, result.SessionToken
	if who.Role != "viewer" || !who.PlatformAdmin || who.Email != "privilege@example.com" {
		t.Fatalf("current database identity not re-read: %+v", who)
	}
	if _, err = store.pool.Exec(ctx, "UPDATE tenants SET status='suspended' WHERE id=$1", tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CompleteGoogleLogin(ctx, identity, strings.Repeat("H", 43), "/"); err != ErrUnauthorized {
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
	linkToken := strings.Repeat("I", 43)
	result, err := store.CompleteGoogleLogin(ctx, GoogleIdentity{Subject: "audit-subject", Email: "audit-failure@example.com", EmailVerified: true}, linkToken, "/")
	if err != nil || !result.LinkRequired {
		t.Fatalf("challenge result=%+v err=%v", result, err)
	}
	_, _, _, err = store.CompleteGoogleLink(ctx, linkToken, "audit-failure@example.com", "long-test-password-42")
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("audit failure result = %v", err)
	}
	var bindings, sessions, audits, consumed int
	if err = store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_external_identities),(SELECT count(*) FROM sessions),(SELECT count(*) FROM audit_logs),(SELECT count(*) FROM google_link_challenges WHERE consumed_at IS NOT NULL)`).Scan(&bindings, &sessions, &audits, &consumed); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 || sessions != 0 || audits != 0 || consumed != 0 {
		t.Fatalf("audit failure committed bindings=%d sessions=%d audits=%d consumed=%d", bindings, sessions, audits, consumed)
	}
}
