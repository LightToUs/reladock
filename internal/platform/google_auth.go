package platform

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const googleLoginAttemptLifetime = 10 * time.Minute

type OAuthLoginAttempt struct {
	Verifier string
	Nonce    string
	ReturnTo string
}

type GoogleIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
}

type GoogleLoginResult struct {
	Identity     Identity
	SessionToken string
	LinkRequired bool
}

func (s *Store) StartGoogleLogin(ctx context.Context, state, verifier, nonce, returnTo string) error {
	if !validOAuthValue(state, 43, 43) || !validOAuthValue(verifier, 43, 128) ||
		!validOAuthValue(nonce, 43, 128) || returnTo != "/" && returnTo != "/admin" {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM oauth_login_attempts WHERE state_hash IN (
 SELECT state_hash FROM oauth_login_attempts
 WHERE expires_at<=now() OR consumed_at<=now()-interval '10 minutes'
 ORDER BY expires_at LIMIT 100
)`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO oauth_login_attempts(state_hash,verifier,nonce,return_to,expires_at) VALUES($1,$2,$3,$4,$5)`,
		tokenHash(state), verifier, nonce, returnTo, time.Now().Add(googleLoginAttemptLifetime))
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return ErrConflict
		}
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ConsumeGoogleLogin(ctx context.Context, state string) (OAuthLoginAttempt, error) {
	if !validOAuthValue(state, 43, 43) {
		return OAuthLoginAttempt{}, ErrUnauthorized
	}
	var attempt OAuthLoginAttempt
	err := s.pool.QueryRow(ctx, `UPDATE oauth_login_attempts SET consumed_at=now()
 WHERE state_hash=$1 AND consumed_at IS NULL AND expires_at>now()
 RETURNING verifier,nonce,return_to`, tokenHash(state)).Scan(&attempt.Verifier, &attempt.Nonce, &attempt.ReturnTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthLoginAttempt{}, ErrUnauthorized
	}
	return attempt, err
}

func (s *Store) CompleteGoogleLogin(ctx context.Context, external GoogleIdentity, linkToken, returnTo string) (GoogleLoginResult, error) {
	email, err := normalizeEmail(external.Email)
	if err != nil || !external.EmailVerified || !validExternalSubject(external.Subject) ||
		!validOAuthValue(linkToken, 43, 43) || returnTo != "/" && returnTo != "/admin" {
		return GoogleLoginResult{}, ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return GoogleLoginResult{}, err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return GoogleLoginResult{}, err
	}

	var memberID string
	err = tx.QueryRow(ctx, `SELECT member_id FROM member_external_identities WHERE provider='google' AND subject=$1 FOR SHARE`, external.Subject).Scan(&memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `DELETE FROM google_link_challenges WHERE token_hash IN (
 SELECT token_hash FROM google_link_challenges
 WHERE expires_at<=now() OR consumed_at<=now()-interval '10 minutes'
 ORDER BY expires_at LIMIT 100
)`); err != nil {
			return GoogleLoginResult{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO google_link_challenges(token_hash,subject,email,return_to,expires_at) VALUES($1,$2,$3,$4,$5)`,
			tokenHash(linkToken), external.Subject, email, returnTo, time.Now().Add(googleLoginAttemptLifetime))
		if err != nil {
			var pgerr *pgconn.PgError
			if errors.As(err, &pgerr) && pgerr.Code == "23505" {
				return GoogleLoginResult{}, ErrConflict
			}
			return GoogleLoginResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return GoogleLoginResult{}, err
		}
		return GoogleLoginResult{LinkRequired: true}, nil
	} else if err != nil {
		return GoogleLoginResult{}, err
	}

	who, token, err := completeGoogleSession(ctx, tx, memberID, false)
	if err != nil {
		return GoogleLoginResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return GoogleLoginResult{}, err
	}
	return GoogleLoginResult{Identity: who, SessionToken: token}, nil
}

func (s *Store) CompleteGoogleLink(ctx context.Context, linkToken, email, password string) (Identity, string, string, error) {
	normalizedEmail, emailErr := normalizeEmail(email)
	if !validOAuthValue(linkToken, 43, 43) || emailErr != nil || len(password) < 1 || len(password) > 128 {
		verifyPassword(password, dummyPasswordHash)
		return Identity{}, "", "", ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, "", "", err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return Identity{}, "", "", err
	}
	var subject, providerEmail, returnTo string
	err = tx.QueryRow(ctx, `SELECT subject,email,return_to FROM google_link_challenges
 WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now() FOR UPDATE`, tokenHash(linkToken)).
		Scan(&subject, &providerEmail, &returnTo)
	if errors.Is(err, pgx.ErrNoRows) {
		verifyPassword(password, dummyPasswordHash)
		return Identity{}, "", "", ErrUnauthorized
	}
	if err != nil {
		return Identity{}, "", "", err
	}
	if normalizedEmail != providerEmail {
		verifyPassword(password, dummyPasswordHash)
		return Identity{}, "", "", ErrUnauthorized
	}

	var memberID, passwordHash string
	err = tx.QueryRow(ctx, `SELECT m.id,m.password_hash FROM members m JOIN tenants t ON t.id=m.tenant_id
 WHERE m.email=$1 AND t.status='active' FOR SHARE OF m,t`, normalizedEmail).Scan(&memberID, &passwordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		verifyPassword(password, dummyPasswordHash)
		return Identity{}, "", "", ErrUnauthorized
	}
	if err != nil {
		return Identity{}, "", "", err
	}
	if !verifyPassword(password, passwordHash) {
		return Identity{}, "", "", ErrUnauthorized
	}

	linked := false
	var existingMemberID string
	err = tx.QueryRow(ctx, `SELECT member_id FROM member_external_identities WHERE provider='google' AND subject=$1 FOR SHARE`, subject).Scan(&existingMemberID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `INSERT INTO member_external_identities(provider,subject,member_id) VALUES('google',$1,$2)`, subject, memberID); err != nil {
			var pgerr *pgconn.PgError
			if errors.As(err, &pgerr) && pgerr.Code == "23505" {
				return Identity{}, "", "", ErrUnauthorized
			}
			return Identity{}, "", "", err
		}
		linked = true
	} else if err != nil {
		return Identity{}, "", "", err
	} else if existingMemberID != memberID {
		return Identity{}, "", "", ErrUnauthorized
	}

	who, token, err := completeGoogleSession(ctx, tx, memberID, linked)
	if err != nil {
		return Identity{}, "", "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE google_link_challenges SET consumed_at=now() WHERE token_hash=$1 AND consumed_at IS NULL`, tokenHash(linkToken)); err != nil {
		return Identity{}, "", "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return Identity{}, "", "", err
	}
	return who, token, returnTo, nil
}

const dummyPasswordHash = "pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func completeGoogleSession(ctx context.Context, tx pgx.Tx, memberID string, linked bool) (Identity, string, error) {
	token := sessionToken()
	var who Identity
	err := tx.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role,m.is_platform_admin
 FROM members m JOIN tenants t ON t.id=m.tenant_id
 WHERE m.id=$1 AND t.status='active' FOR SHARE OF m,t`, memberID).
		Scan(&who.MemberID, &who.TenantID, &who.Company, &who.Email, &who.Role, &who.PlatformAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, "", ErrUnauthorized
	}
	if err != nil {
		return Identity{}, "", err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM sessions WHERE member_id=$1 AND expires_at<=now()", who.MemberID); err != nil {
		return Identity{}, "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO sessions(token_hash,member_id,expires_at) VALUES($1,$2,$3)", tokenHash(token), who.MemberID, time.Now().Add(SessionLifetime)); err != nil {
		return Identity{}, "", err
	}
	if linked {
		if err = audit(ctx, tx, who, "account.google_link", who.MemberID); err != nil {
			return Identity{}, "", err
		}
	}
	if err = audit(ctx, tx, who, "account.login", who.MemberID); err != nil {
		return Identity{}, "", err
	}
	return who, token, nil
}

func validOAuthValue(value string, minLength, maxLength int) bool {
	if len(value) < minLength || len(value) > maxLength {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validExternalSubject(subject string) bool {
	if len(subject) < 1 || len(subject) > 255 || !utf8.ValidString(subject) || strings.TrimSpace(subject) != subject {
		return false
	}
	for _, char := range subject {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return false
		}
	}
	return true
}
