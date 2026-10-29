package platform

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file provides v4 services; the parent registers migration4.sql and wires
// the HTTP hooks. No mail delivery is implied: invitation secrets are returned
// once to the inviting administrator for delivery through a trusted channel.
type Member struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}
type Invitation struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	AcceptedAt *time.Time `json:"acceptedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}
type APIKeyInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	ExpiresAt time.Time  `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt"`
}

type apiKeyContextKey struct{}

// WithAPIKey binds the bearer credential to the request for transaction rechecks.
// The parent MUST pass this context to every store operation authenticated by a
// key, and route tenantWriteAccess through AuthorizeWrite (or equivalent).
// Identity alone cannot distinguish a session from a key without changing auth.go.
func WithAPIKey(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, apiKeyContextKey{}, tokenHash(token))
}

// AuthorizeWrite rechecks current member and key permissions while holding row
// locks until commit. Call this from the parent's tenant write authorization.
func (s *Store) AuthorizeWrite(ctx context.Context, tx pgx.Tx, who Identity) error {
	if !canWrite(who) {
		return ErrForbidden
	}
	if err := tenantWriteAccess(ctx, tx, who); err != nil {
		return err
	}
	return CheckAPIKeyWrite(ctx, tx, who)
}

// CheckAPIKeyWrite supplements the existing tenant/member transaction check.
// It does nothing for session contexts. This split avoids recursive integration
// when tenantWriteAccess itself is changed to invoke this helper.
// The caller holds tenant/member row shares before this key share. Role changes
// and credential revocation use the same row order; unrelated tenants need no
// global authorization mutex.
func CheckAPIKeyWrite(ctx context.Context, tx pgx.Tx, who Identity) error {
	if !canWrite(who) {
		return ErrForbidden
	}
	digest, ok := ctx.Value(apiKeyContextKey{}).(string)
	if !ok {
		return nil
	}
	var scope string
	err := tx.QueryRow(ctx, `SELECT scope FROM api_keys WHERE token_hash=$1 AND member_id=$2 AND tenant_id=$3 AND revoked_at IS NULL AND expires_at>clock_timestamp() FOR SHARE`, digest, who.MemberID, who.TenantID).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if scope != "write" {
		return ErrForbidden
	}
	return nil
}

func memberConflict(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return ErrConflict
	}
	return err
}
func (s *Store) memberTx(ctx context.Context, who Identity, manager bool) (pgx.Tx, Identity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, Identity{}, err
	}
	if manager {
		if err = platformLock(ctx, tx); err != nil {
			tx.Rollback(ctx)
			return nil, Identity{}, err
		}
	}
	current := Identity{}
	err = tx.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.id=$1 AND m.tenant_id=$2 AND t.status='active' FOR SHARE OF t,m`, who.MemberID, who.TenantID).Scan(&current.MemberID, &current.TenantID, &current.Company, &current.Email, &current.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrUnauthorized
	}
	if err == nil && manager && (!canWrite(current) || !canWrite(who)) {
		err = ErrForbidden
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, Identity{}, err
	}
	return tx, current, nil
}
func memberSessionOnly(ctx context.Context) error {
	if ctx.Value(apiKeyContextKey{}) != nil {
		return ErrForbidden
	}
	return nil
}
func validMemberRole(role string) bool { return role == "admin" || role == "viewer" }
func validMemberExpiry(expires time.Time, max time.Duration) bool {
	now := time.Now()
	return expires.After(now) && !expires.After(now.Add(max))
}

func (s *Store) ListMembers(ctx context.Context, who Identity) ([]Member, error) {
	tx, _, err := s.memberTx(ctx, who, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,email,role,created_at FROM members WHERE tenant_id=$1 ORDER BY created_at,id`, who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Member{}
	for rows.Next() {
		var v Member
		if err = rows.Scan(&v.ID, &v.Email, &v.Role, &v.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// ChangeMemberRole never creates owners, modifies owners, or changes any
// platform administrator. Revokes sessions, keys and pending invitations issued
// by the target so a downgraded administrator cannot retain delegated authority.
func (s *Store) ChangeMemberRole(ctx context.Context, who Identity, id, role string) error {
	if !validMemberRole(role) {
		return ErrInvalid
	}
	if err := memberSessionOnly(ctx); err != nil {
		return err
	}
	tx, current, err := s.memberTx(ctx, who, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var previous string
	var platform bool
	err = tx.QueryRow(ctx, `SELECT role,is_platform_admin FROM members WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, id, current.TenantID).Scan(&previous, &platform)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if previous == "owner" || platform {
		return ErrForbidden
	}
	if previous == role {
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE members SET role=$1 WHERE id=$2 AND tenant_id=$3`, role, id, current.TenantID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE member_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE member_id=$1 AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE member_invitations SET revoked_at=now() WHERE invited_by=$1 AND accepted_at IS NULL AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, current, "member.role", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateInvitation(ctx context.Context, who Identity, email, role string, expires time.Time) (Invitation, string, error) {
	email, err := normalizeEmail(email)
	if err != nil || !validMemberRole(role) || !validMemberExpiry(expires, 7*24*time.Hour) {
		return Invitation{}, "", ErrInvalid
	}
	if err = memberSessionOnly(ctx); err != nil {
		return Invitation{}, "", err
	}
	tx, current, err := s.memberTx(ctx, who, true)
	if err != nil {
		return Invitation{}, "", err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM members WHERE email=$1)`, email).Scan(&exists); err != nil {
		return Invitation{}, "", err
	}
	if exists {
		return Invitation{}, "", ErrConflict
	}
	// Expired invitations must not permanently reserve the email.
	if _, err = tx.Exec(ctx, `UPDATE member_invitations SET revoked_at=now() WHERE tenant_id=$1 AND email=$2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at<=clock_timestamp()`, current.TenantID, email); err != nil {
		return Invitation{}, "", err
	}
	raw := sessionToken()
	v := Invitation{ID: newID(), Email: email, Role: role, ExpiresAt: expires}
	err = tx.QueryRow(ctx, `INSERT INTO member_invitations(id,tenant_id,email,role,token_hash,invited_by,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, v.ID, current.TenantID, email, role, tokenHash(raw), current.MemberID, expires).Scan(&v.CreatedAt)
	if err != nil {
		return Invitation{}, "", memberConflict(err)
	}
	if err = audit(ctx, tx, current, "invitation.create", v.ID); err != nil {
		return Invitation{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return Invitation{}, "", err
	}
	return v, raw, nil
}

func (s *Store) ListInvitations(ctx context.Context, who Identity) ([]Invitation, error) {
	tx, _, err := s.memberTx(ctx, who, true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,email,role,expires_at,created_at,accepted_at,revoked_at FROM member_invitations WHERE tenant_id=$1 ORDER BY created_at DESC,id LIMIT 100`, who.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Invitation{}
	for rows.Next() {
		var v Invitation
		if err = rows.Scan(&v.ID, &v.Email, &v.Role, &v.ExpiresAt, &v.CreatedAt, &v.AcceptedAt, &v.RevokedAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) RevokeInvitation(ctx context.Context, who Identity, id string) error {
	if err := memberSessionOnly(ctx); err != nil {
		return err
	}
	tx, current, err := s.memberTx(ctx, who, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var accepted, revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT accepted_at,revoked_at FROM member_invitations WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, id, current.TenantID).Scan(&accepted, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revoked != nil {
		return nil
	}
	if accepted != nil {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE member_invitations SET revoked_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, current, "invitation.revoke", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AcceptInvitation creates a new member only; it never resets an existing
// account's password or moves an account between tenants. Email is normalized
// and must match the invitation. The returned session is new and secret.
func (s *Store) AcceptInvitation(ctx context.Context, token, email, password string) (Identity, string, error) {
	email, err := normalizeEmail(email)
	if err != nil || len(token) != 43 {
		return Identity{}, "", ErrUnauthorized
	}
	if len(password) < 12 || len(password) > 128 {
		return Identity{}, "", ErrInvalid
	}
	// Hash outside the authorization lock, then recheck token/expiry under lock.
	hash, err := hashPassword(password)
	if err != nil {
		return Identity{}, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, "", err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return Identity{}, "", err
	}
	var id string
	who := Identity{MemberID: newID(), Email: email}
	err = tx.QueryRow(ctx, `SELECT i.id,i.tenant_id,t.name,i.role FROM member_invitations i JOIN tenants t ON t.id=i.tenant_id JOIN members m ON m.id=i.invited_by AND m.tenant_id=i.tenant_id WHERE i.token_hash=$1 AND i.email=$2 AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at>clock_timestamp() AND t.status='active' AND m.role IN ('owner','admin') FOR UPDATE OF i FOR SHARE OF t,m`, tokenHash(token), email).Scan(&id, &who.TenantID, &who.Company, &who.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, "", ErrUnauthorized
	}
	if err != nil {
		return Identity{}, "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO members(id,tenant_id,email,password_hash,role,is_platform_admin) VALUES($1,$2,$3,$4,$5,false)`, who.MemberID, who.TenantID, email, hash, who.Role); err != nil {
		return Identity{}, "", memberConflict(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE member_invitations SET accepted_at=now() WHERE id=$1`, id); err != nil {
		return Identity{}, "", err
	}
	session := sessionToken()
	if _, err = tx.Exec(ctx, `INSERT INTO sessions(token_hash,member_id,expires_at) VALUES($1,$2,$3)`, tokenHash(session), who.MemberID, time.Now().Add(SessionLifetime)); err != nil {
		return Identity{}, "", err
	}
	if err = audit(ctx, tx, who, "invitation.accept", id); err != nil {
		return Identity{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return Identity{}, "", err
	}
	return who, session, nil
}

// APIKey returns the current member identity capped by the credential scope.
// Keys NEVER carry platform privileges, including keys owned by a platform admin.
func (s *Store) APIKey(ctx context.Context, token string) (Identity, error) {
	if len(token) != 47 || !strings.HasPrefix(token, "rdk_") {
		return Identity{}, ErrUnauthorized
	}
	var who Identity
	var scope string
	err := s.pool.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role,k.scope FROM api_keys k JOIN members m ON m.id=k.member_id AND m.tenant_id=k.tenant_id JOIN tenants t ON t.id=m.tenant_id WHERE k.token_hash=$1 AND k.revoked_at IS NULL AND k.expires_at>clock_timestamp() AND t.status='active'`, tokenHash(token)).Scan(&who.MemberID, &who.TenantID, &who.Company, &who.Email, &who.Role, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthorized
	}
	if err != nil {
		return Identity{}, err
	}
	if scope == "read" || who.Role == "viewer" {
		who.Role = "viewer"
	}
	return who, nil
}
func (s *Store) CreateAPIKey(ctx context.Context, who Identity, name, scope string, expires time.Time) (APIKeyInfo, string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 80 || (scope != "read" && scope != "write") || !validMemberExpiry(expires, 366*24*time.Hour) {
		return APIKeyInfo{}, "", ErrInvalid
	}
	if err := memberSessionOnly(ctx); err != nil {
		return APIKeyInfo{}, "", err
	}
	tx, current, err := s.memberTx(ctx, who, false)
	if err != nil {
		return APIKeyInfo{}, "", err
	}
	defer tx.Rollback(ctx)
	if scope == "write" && (!canWrite(current) || !canWrite(who)) {
		return APIKeyInfo{}, "", ErrForbidden
	}
	raw := "rdk_" + sessionToken()
	v := APIKeyInfo{ID: newID(), Name: name, Scope: scope, ExpiresAt: expires}
	err = tx.QueryRow(ctx, `INSERT INTO api_keys(id,tenant_id,member_id,name,token_hash,scope,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`, v.ID, current.TenantID, current.MemberID, name, tokenHash(raw), scope, expires).Scan(&v.CreatedAt)
	if err != nil {
		return APIKeyInfo{}, "", err
	}
	if err = audit(ctx, tx, current, "api-key.create", v.ID); err != nil {
		return APIKeyInfo{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return APIKeyInfo{}, "", err
	}
	return v, raw, nil
}
func (s *Store) ListAPIKeys(ctx context.Context, who Identity) ([]APIKeyInfo, error) {
	tx, _, err := s.memberTx(ctx, who, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,name,scope,expires_at,created_at,revoked_at FROM api_keys WHERE tenant_id=$1 AND member_id=$2 ORDER BY created_at DESC,id LIMIT 100`, who.TenantID, who.MemberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []APIKeyInfo{}
	for rows.Next() {
		var v APIKeyInfo
		if err = rows.Scan(&v.ID, &v.Name, &v.Scope, &v.ExpiresAt, &v.CreatedAt, &v.RevokedAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) RevokeAPIKey(ctx context.Context, who Identity, id string) error {
	if err := memberSessionOnly(ctx); err != nil {
		return err
	}
	tx, current, err := s.memberTx(ctx, who, false)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT revoked_at FROM api_keys WHERE id=$1 AND tenant_id=$2 AND member_id=$3 FOR UPDATE`, id, current.TenantID, current.MemberID).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revoked != nil {
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, current, "api-key.revoke", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
