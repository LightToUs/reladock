package platform

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Serialize privilege changes and platform mutations, so revocation cannot race
// an already-authorized write. Tenant operations retain their node-level locks.
func platformLock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7263520190)")
	return err
}
func authorizePlatform(ctx context.Context, tx pgx.Tx, who Identity) error {
	if ctx.Value(apiKeyContextKey{}) != nil {
		return ErrForbidden
	}
	var allowed bool
	err := tx.QueryRow(ctx, "SELECT m.is_platform_admin AND t.status='active' FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.id=$1 AND m.tenant_id=$2 FOR SHARE OF m,t", who.MemberID, who.TenantID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
		return ErrForbidden
	}
	return err
}
func platformLog(ctx context.Context, tx pgx.Tx, who *Identity, action, resource string, details any) error {
	var actor any
	email := "local-operator"
	if who != nil {
		actor = who.MemberID
		email = who.Email
	}
	if details == nil {
		details = map[string]any{}
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO platform_audit_logs(actor_id,actor_email,action,resource_id,details) VALUES($1,$2,$3,$4,$5)", actor, email, action, resource, payload)
	return err
}
func (s *Store) adminTx(ctx context.Context, who Identity) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err = platformLock(ctx, tx); err == nil {
		err = authorizePlatform(ctx, tx, who)
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// Read-only platform queries never wait for unrelated node mutations' global
// authorization lock. Row sharing still serializes revocation of this actor.
func (s *Store) adminReadTx(ctx context.Context, who Identity) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err = authorizePlatform(ctx, tx, who); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// Only the local operator CLI exposes this function; never self-register admins.
func (s *Store) SetPlatformAdmin(ctx context.Context, email string, enabled bool) error {
	email, err := normalizeEmail(email)
	if err != nil {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = platformLock(ctx, tx); err != nil {
		return err
	}
	var id, status string
	var current bool
	err = tx.QueryRow(ctx, "SELECT m.id,m.is_platform_admin,t.status FROM members m JOIN tenants t ON t.id=m.tenant_id WHERE m.email=$1 FOR UPDATE OF m", email).Scan(&id, &current, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if enabled && status != "active" {
		return ErrConflict
	}
	if current == enabled {
		return nil
	}
	if _, err = tx.Exec(ctx, "UPDATE members SET is_platform_admin=$1 WHERE id=$2", enabled, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM sessions WHERE member_id=$1", id); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, nil, "platform.admin-access", id, map[string]any{"email": email, "enabled": enabled}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type PlatformSummary struct {
	Tenants          int `json:"tenants"`
	SuspendedTenants int `json:"suspendedTenants"`
	Nodes            int `json:"nodes"`
	MaintenanceNodes int `json:"maintenanceNodes"`
	Rules            int `json:"rules"`
	PausedRules      int `json:"pausedRules"`
	FailedDeliveries int `json:"failedDeliveries"`
}

func (s *Store) AdminOverview(ctx context.Context, who Identity) (PlatformSummary, error) {
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return PlatformSummary{}, err
	}
	defer tx.Rollback(ctx)
	var sum PlatformSummary
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM tenants),(SELECT count(*) FROM tenants WHERE status='suspended'),(SELECT count(*) FROM nodes),(SELECT count(*) FROM nodes WHERE status='maintenance'),(SELECT count(*) FROM relay_rules WHERE status<>'deleted'),(SELECT count(*) FROM relay_rules WHERE status='paused'),(SELECT count(*) FROM config_outbox o JOIN nodes n ON n.id=o.node_id AND n.desired_generation=o.generation WHERE o.state='failed')`).Scan(&sum.Tenants, &sum.SuspendedTenants, &sum.Nodes, &sum.MaintenanceNodes, &sum.Rules, &sum.PausedRules, &sum.FailedDeliveries)
	return sum, err
}

type Page[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}
type TenantSummary struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	Revision         int64     `json:"revision"`
	CreatedAt        time.Time `json:"createdAt"`
	Members          int       `json:"members"`
	Rules            int       `json:"rules"`
	OwnerEmail       string    `json:"ownerEmail"`
	HasPlatformAdmin bool      `json:"hasPlatformAdmin"`
}

func (s *Store) AdminTenants(ctx context.Context, who Identity, page int) (Page[TenantSummary], error) {
	result := Page[TenantSummary]{Items: []TenantSummary{}, Page: page, PageSize: 50}
	if page < 1 || page > 100000 {
		return result, ErrInvalid
	}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT t.id,t.name,t.status,t.revision,t.created_at,(SELECT count(*) FROM members m WHERE m.tenant_id=t.id),(SELECT count(*) FROM relay_rules r WHERE r.tenant_id=t.id AND r.status<>'deleted'),COALESCE((SELECT email FROM members m WHERE m.tenant_id=t.id AND role='owner' ORDER BY created_at LIMIT 1),''),EXISTS(SELECT 1 FROM members m WHERE m.tenant_id=t.id AND m.is_platform_admin) FROM tenants t ORDER BY t.created_at DESC,t.id LIMIT 50 OFFSET $1`, (page-1)*50)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var t TenantSummary
		if err = rows.Scan(&t.ID, &t.Name, &t.Status, &t.Revision, &t.CreatedAt, &t.Members, &t.Rules, &t.OwnerEmail, &t.HasPlatformAdmin); err != nil {
			return result, err
		}
		result.Items = append(result.Items, t)
	}
	return result, rows.Err()
}
func (s *Store) AdminTenantAccess(ctx context.Context, who Identity, id string, enabled bool, revision int64) error {
	if revision < 1 {
		return ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current int64
	err = tx.QueryRow(ctx, "SELECT revision FROM tenants WHERE id=$1 FOR UPDATE", id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != revision {
		return ErrConflict
	}
	if !enabled {
		var contains bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM members WHERE tenant_id=$1 AND is_platform_admin)", id).Scan(&contains); err != nil {
			return err
		}
		if contains {
			return ErrConflict
		}
	}
	status := "suspended"
	if enabled {
		status = "active"
	}
	if _, err = tx.Exec(ctx, "UPDATE tenants SET status=$1,revision=revision+1 WHERE id=$2", status, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM sessions WHERE member_id IN (SELECT id FROM members WHERE tenant_id=$1)", id); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "tenant.access", id, map[string]any{"status": status}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type PlatformAudit struct {
	ID         string          `json:"id"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	ResourceID string          `json:"resourceId"`
	Details    json.RawMessage `json:"details"`
	CreatedAt  time.Time       `json:"createdAt"`
}

func (s *Store) AdminAudit(ctx context.Context, who Identity) ([]PlatformAudit, error) {
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,actor_email,action,resource_id,details,created_at FROM (SELECT 'p-'||id::text AS id,actor_email,action,resource_id,details,created_at FROM platform_audit_logs UNION ALL SELECT 'n-'||id::text,'local-operator',action,node_id,'{}'::jsonb,created_at FROM node_operator_events) e ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []PlatformAudit{}
	for rows.Next() {
		var item PlatformAudit
		if err = rows.Scan(&item.ID, &item.Actor, &item.Action, &item.ResourceID, &item.Details, &item.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, item)
	}
	return logs, rows.Err()
}
