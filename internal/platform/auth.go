package platform

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const SessionLifetime = 24 * time.Hour
const passwordIterations = 600000

type Identity struct {
	MemberID      string `json:"memberId"`
	TenantID      string `json:"tenantId"`
	Company       string `json:"company"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	PlatformAdmin bool   `json:"platformAdmin"`
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 254 {
		return "", ErrInvalid
	}
	return email, nil
}
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds != passwordIterations {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
func tokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
func sessionToken() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func (s *Store) Register(ctx context.Context, company, email, password string) (Identity, string, error) {
	company = strings.TrimSpace(company)
	email, err := normalizeEmail(email)
	if err != nil || !utf8.ValidString(company) || utf8.RuneCountInString(company) < 1 || utf8.RuneCountInString(company) > 120 || len(password) < 12 || len(password) > 128 {
		return Identity{}, "", ErrInvalid
	}
	hash, err := hashPassword(password)
	if err != nil {
		return Identity{}, "", err
	}
	who := Identity{MemberID: newID(), TenantID: newID(), Company: company, Email: email, Role: "owner"}
	token := sessionToken()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO tenants(id,name) VALUES($1,$2)", who.TenantID, company); err != nil {
		return Identity{}, "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO members(id,tenant_id,email,password_hash,role) VALUES($1,$2,$3,$4,'owner')", who.MemberID, who.TenantID, email, hash); err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			err = ErrConflict
		}
		return Identity{}, "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO sessions(token_hash,member_id,expires_at) VALUES($1,$2,$3)", tokenHash(token), who.MemberID, time.Now().Add(SessionLifetime)); err != nil {
		return Identity{}, "", err
	}
	if err = audit(ctx, tx, who, "account.register", who.TenantID); err != nil {
		return Identity{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return Identity{}, "", err
	}
	return who, token, nil
}

func (s *Store) Login(ctx context.Context, email, password string) (Identity, string, error) {
	email, err := normalizeEmail(email)
	if err != nil || len(password) > 128 {
		return Identity{}, "", ErrUnauthorized
	}
	var who Identity
	var hash string
	err = s.pool.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role,m.password_hash,m.is_platform_admin FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.email=$1 AND t.status='active'`, email).Scan(&who.MemberID, &who.TenantID, &who.Company, &who.Email, &who.Role, &hash, &who.PlatformAdmin)
	if err != nil {
		// Spend the same password work on an unknown email; no account enumeration by timing.
		verifyPassword(password, "pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, "", ErrUnauthorized
		}
		return Identity{}, "", err
	}
	if !verifyPassword(password, hash) {
		return Identity{}, "", ErrUnauthorized
	}
	token := sessionToken()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, "", err
	}
	defer tx.Rollback(ctx)
	// Match revocation before touching sessions. Refresh the identity after the
	// password check so suspended tenants or changed privileges cannot race login.
	if err = platformLock(ctx, tx); err != nil {
		return Identity{}, "", err
	}
	var currentHash string
	err = tx.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role,m.password_hash,m.is_platform_admin FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.email=$1 AND t.status='active' FOR SHARE OF m,t`, email).Scan(&who.MemberID, &who.TenantID, &who.Company, &who.Email, &who.Role, &currentHash, &who.PlatformAdmin)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && currentHash != hash {
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
	if err = audit(ctx, tx, who, "account.login", who.MemberID); err != nil {
		return Identity{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return Identity{}, "", err
	}
	return who, token, nil
}

func (s *Store) Session(ctx context.Context, token string) (Identity, error) {
	if len(token) != 43 {
		return Identity{}, ErrUnauthorized
	}
	var who Identity
	err := s.pool.QueryRow(ctx, `SELECT m.id,m.tenant_id,t.name,m.email,m.role,m.is_platform_admin FROM sessions s JOIN members m ON m.id=s.member_id JOIN tenants t ON t.id=m.tenant_id WHERE s.token_hash=$1 AND s.expires_at>now() AND t.status='active'`, tokenHash(token)).Scan(&who.MemberID, &who.TenantID, &who.Company, &who.Email, &who.Role, &who.PlatformAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthorized
	}
	return who, err
}
func (s *Store) Logout(ctx context.Context, token string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Match administrative access changes: acquire the authorization lock before
	// locking a session. Audit foreign keys may then lock tenant/member rows safely.
	if err = platformLock(ctx, tx); err != nil {
		return err
	}
	var who Identity
	err = tx.QueryRow(ctx, `DELETE FROM sessions s USING members m WHERE s.member_id=m.id AND token_hash=$1 RETURNING m.id,m.tenant_id`, tokenHash(token)).Scan(&who.MemberID, &who.TenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = audit(ctx, tx, who, "account.logout", who.MemberID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
