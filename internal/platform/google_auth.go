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

func (s *Store) CompleteGoogleLogin(ctx context.Context, external GoogleIdentity) (Identity, string, error) {
	email, err := normalizeEmail(external.Email)
	if err != nil || !external.EmailVerified || !validExternalSubject(external.Subject) {
		return Identity{}, "", ErrUnauthorized
	}
	token := sessionToken()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, "", err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return Identity{}, "", err
	}

	var memberID string
	linked := false
	err = tx.QueryRow(ctx, `SELECT member_id FROM member_external_identities WHERE provider='google' AND subject=$1 FOR SHARE`, external.Subject).Scan(&memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT m.id FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.email=$1 AND t.status='active' FOR SHARE OF m,t`, email).Scan(&memberID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, "", ErrUnauthorized
		}
		if err != nil {
			return Identity{}, "", err
		}
		result, insertErr := tx.Exec(ctx, `INSERT INTO member_external_identities(provider,subject,member_id) VALUES('google',$1,$2) ON CONFLICT DO NOTHING`, external.Subject, memberID)
		if insertErr != nil {
			return Identity{}, "", insertErr
		}
		if result.RowsAffected() == 1 {
			linked = true
		} else {
			var existingMemberID string
			lookupErr := tx.QueryRow(ctx, `SELECT member_id FROM member_external_identities WHERE provider='google' AND subject=$1`, external.Subject).Scan(&existingMemberID)
			if lookupErr != nil || existingMemberID != memberID {
				if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
					return Identity{}, "", lookupErr
				}
				return Identity{}, "", ErrUnauthorized
			}
		}
	} else if err != nil {
		return Identity{}, "", err
	}

	var who Identity
	err = tx.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role,m.is_platform_admin
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
	if err = tx.Commit(ctx); err != nil {
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
