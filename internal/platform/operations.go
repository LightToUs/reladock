package platform

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const DefaultContactStaleAfter = 5 * time.Minute

type OperationNode struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	ContactStatus       string     `json:"contactStatus"`
	LastSeenAt          *time.Time `json:"lastSeenAt"`
	DesiredGeneration   int64      `json:"desiredGeneration"`
	ValidatedGeneration int64      `json:"validatedGeneration"`
	DeliveryState       string     `json:"deliveryState"`
	DataPlaneStatus     string     `json:"dataPlaneStatus"`
	RuleIDs             []string   `json:"ruleIds"`
}
type Incident struct {
	ID          string     `json:"id"`
	NodeID      string     `json:"nodeId"`
	Kind        string     `json:"kind"`
	State       string     `json:"state"`
	OpenedAt    time.Time  `json:"openedAt"`
	ObservedAt  time.Time  `json:"observedAt"`
	ResolvedAt  *time.Time `json:"resolvedAt"`
	Occurrences int64      `json:"occurrences"`
	RuleIDs     []string   `json:"ruleIds"`
}
type SupportTicket struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenantId"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type SupportMessage struct {
	ID         int64     `json:"id"`
	AuthorKind string    `json:"authorKind"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}
type SupportTicketThread struct {
	Ticket   SupportTicket    `json:"ticket"`
	Messages []SupportMessage `json:"messages"`
}

func validStaleAfter(value time.Duration) bool {
	return value >= time.Second && value <= 30*24*time.Hour
}
func operationsPage[T any](page int) (Page[T], error) {
	v := Page[T]{Items: []T{}, Page: page, PageSize: 100}
	if page < 1 || page > 100000 {
		return v, ErrInvalid
	}
	return v, nil
}

// Keys cannot enter platform operations even if their member has a platform
// flag in the DB. Tenant writes recheck current roles/key expiry under row locks
// protecting revocation; read scopes may only use tenant queries. Platform reads
// use actor/tenant row sharing without the global mutation lock.
func (s *Store) operationsTx(ctx context.Context, who Identity, admin, write bool) (pgx.Tx, error) {
	if admin {
		if !who.PlatformAdmin || ctx.Value(apiKeyContextKey{}) != nil {
			return nil, ErrForbidden
		}
		if !write {
			return s.adminReadTx(ctx, who)
		}
		return s.adminTx(ctx, who)
	}
	tx, _, err := s.memberTx(ctx, who, write)
	if err != nil {
		return nil, err
	}
	if write {
		if err = s.AuthorizeWrite(ctx, tx, who); err != nil {
			tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

// ReconcileIncidents uses the database clock after each node lock. It monitors
// enrolled, non-maintenance nodes only. A NULL contact is unknown, not stale.
// A resolved incident means the condition stopped being actionable, never that
// forwarding works. Events and transitions commit together; unchanged conditions
// produce no duplicate audit events, and recurrence reuses the same incident ID.
// Each node has a separate bounded transaction, with no global authorization
// lock. Busy node locks are skipped and retried by a subsequent reconciliation.
func (s *Store) ReconcileIncidents(ctx context.Context, staleAfter time.Duration) error {
	if !validStaleAfter(staleAfter) {
		return ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM nodes ORDER BY id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures error
	for _, nodeID := range ids {
		if err = ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err = s.reconcileNodeIncidents(ctx, nodeID, staleAfter); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Store) reconcileNodeIncidents(ctx context.Context, nodeID string, staleAfter time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var acquired bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "reladock-node:"+nodeID).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	var enrolled, stale, failed bool
	err = tx.QueryRow(ctx, `SELECT n.certificate_fingerprint IS NOT NULL AND n.status<>'maintenance',COALESCE(n.last_seen_at<clock_timestamp()-$2*interval '1 second',false),COALESCE(o.state='failed',false) FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation WHERE n.id=$1`, nodeID, staleAfter.Seconds()).Scan(&enrolled, &stale, &failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // Inventory may change between its read and this transaction.
	}
	if err != nil {
		return err
	}
	for _, condition := range []struct {
		kind       string
		actionable bool
	}{{"contact-stale", enrolled && stale}, {"config-failed", enrolled && failed}} {
		var id, state string
		err = tx.QueryRow(ctx, `SELECT id,state FROM operations_incidents WHERE node_id=$1 AND kind=$2 FOR UPDATE`, nodeID, condition.kind).Scan(&id, &state)
		absent := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !absent {
			return err
		}
		err = nil
		action := ""
		if condition.actionable {
			if absent {
				id = newID()
				action = "incident.open"
				_, err = tx.Exec(ctx, `INSERT INTO operations_incidents(id,node_id,kind,state) VALUES($1,$2,$3,'open') ON CONFLICT(node_id,kind) DO NOTHING`, id, nodeID, condition.kind)
			} else if state == "resolved" {
				action = "incident.open"
				_, err = tx.Exec(ctx, `UPDATE operations_incidents SET state='open',opened_at=clock_timestamp(),observed_at=clock_timestamp(),resolved_at=NULL,occurrences=occurrences+1 WHERE id=$1`, id)
			} else {
				_, err = tx.Exec(ctx, `UPDATE operations_incidents SET observed_at=clock_timestamp() WHERE id=$1`, id)
			}
		} else if !absent && state == "open" {
			action = "incident.resolve"
			_, err = tx.Exec(ctx, `UPDATE operations_incidents SET state='resolved',observed_at=clock_timestamp(),resolved_at=clock_timestamp() WHERE id=$1`, id)
		}
		if err != nil {
			return err
		}
		if action != "" {
			_, err = tx.Exec(ctx, `INSERT INTO platform_audit_logs(actor_email,action,resource_id,details) VALUES('operations-worker',$1,$2,jsonb_build_object('nodeId',$3::text,'kind',$4::text))`, action, id, nodeID, condition.kind)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ListOperationsNodes(ctx context.Context, who Identity, admin bool, staleAfter time.Duration, page int) (Page[OperationNode], error) {
	result, err := operationsPage[OperationNode](page)
	if err != nil || !validStaleAfter(staleAfter) {
		return result, ErrInvalid
	}
	tx, err := s.operationsTx(ctx, who, admin, false)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	where := ` WHERE ($1 OR EXISTS(SELECT 1 FROM relay_rules r WHERE r.node_id=n.id AND r.tenant_id=$2 AND r.status<>'deleted'))`
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM nodes n`+where, admin, who.TenantID).Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT n.id,n.name,CASE WHEN n.status='maintenance' THEN 'maintenance' WHEN n.certificate_fingerprint IS NULL THEN 'unknown' WHEN n.last_seen_at IS NULL THEN 'never-seen' WHEN n.last_seen_at<clock_timestamp()-$3*interval '1 second' THEN 'stale' ELSE 'recent' END,n.last_seen_at,n.desired_generation,n.validated_generation,COALESCE(o.state,'unknown'),ARRAY(SELECT r.id::text FROM relay_rules r WHERE r.node_id=n.id AND r.status<>'deleted' AND ($1 OR r.tenant_id=$2) ORDER BY r.id LIMIT 100) FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation`+where+` ORDER BY n.id LIMIT 100 OFFSET $4`, admin, who.TenantID, staleAfter.Seconds(), (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		v := OperationNode{DataPlaneStatus: "unknown"}
		if err = rows.Scan(&v.ID, &v.Name, &v.ContactStatus, &v.LastSeenAt, &v.DesiredGeneration, &v.ValidatedGeneration, &v.DeliveryState, &v.RuleIDs); err != nil {
			return result, err
		}
		result.Items = append(result.Items, v)
	}
	return result, rows.Err()
}

func (s *Store) ListIncidents(ctx context.Context, who Identity, admin bool, page int) (Page[Incident], error) {
	result, err := operationsPage[Incident](page)
	if err != nil {
		return result, err
	}
	tx, err := s.operationsTx(ctx, who, admin, false)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	where := ` WHERE ($1 OR EXISTS(SELECT 1 FROM relay_rules r WHERE r.node_id=i.node_id AND r.tenant_id=$2 AND r.status<>'deleted'))`
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM operations_incidents i`+where, admin, who.TenantID).Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT i.id,i.node_id,i.kind,i.state,i.opened_at,i.observed_at,i.resolved_at,i.occurrences,ARRAY(SELECT r.id::text FROM relay_rules r WHERE r.node_id=i.node_id AND r.status<>'deleted' AND ($1 OR r.tenant_id=$2) ORDER BY r.id LIMIT 100) FROM operations_incidents i`+where+` ORDER BY i.opened_at DESC,i.id LIMIT 100 OFFSET $3`, admin, who.TenantID, (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var v Incident
		if err = rows.Scan(&v.ID, &v.NodeID, &v.Kind, &v.State, &v.OpenedAt, &v.ObservedAt, &v.ResolvedAt, &v.Occurrences, &v.RuleIDs); err != nil {
			return result, err
		}
		result.Items = append(result.Items, v)
	}
	return result, rows.Err()
}

func supportText(value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > max {
		return "", ErrInvalid
	}
	return value, nil
}

const supportTicketColumns = `id,tenant_id,subject,status,revision,created_at,updated_at`

func scanSupportTicket(row pgx.Row) (SupportTicket, error) {
	var v SupportTicket
	err := row.Scan(&v.ID, &v.TenantID, &v.Subject, &v.Status, &v.Revision, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}
func (s *Store) CreateSupportTicket(ctx context.Context, who Identity, subject, body string) (SupportTicket, error) {
	subject, err := supportText(subject, 160)
	if err != nil {
		return SupportTicket{}, err
	}
	body, err = supportText(body, 4000)
	if err != nil {
		return SupportTicket{}, err
	}
	tx, err := s.operationsTx(ctx, who, false, true)
	if err != nil {
		return SupportTicket{}, err
	}
	defer tx.Rollback(ctx)
	ticket, err := scanSupportTicket(tx.QueryRow(ctx, `INSERT INTO support_tickets(id,tenant_id,created_by,subject) VALUES($1,$2,$3,$4) RETURNING `+supportTicketColumns, newID(), who.TenantID, who.MemberID, subject))
	if err != nil {
		return SupportTicket{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO support_messages(ticket_id,tenant_id,author_id,author_kind,body) VALUES($1,$2,$3,'tenant',$4)`, ticket.ID, who.TenantID, who.MemberID, body); err != nil {
		return SupportTicket{}, err
	}
	if err = audit(ctx, tx, who, "support.create", ticket.ID); err != nil {
		return SupportTicket{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SupportTicket{}, err
	}
	return ticket, nil
}
func (s *Store) ListSupportTickets(ctx context.Context, who Identity, admin bool, page int) (Page[SupportTicket], error) {
	result, err := operationsPage[SupportTicket](page)
	if err != nil {
		return result, err
	}
	tx, err := s.operationsTx(ctx, who, admin, false)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM support_tickets WHERE $1 OR tenant_id=$2`, admin, who.TenantID).Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT `+supportTicketColumns+` FROM support_tickets WHERE $1 OR tenant_id=$2 ORDER BY updated_at DESC,id LIMIT 100 OFFSET $3`, admin, who.TenantID, (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanSupportTicket(rows)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, v)
	}
	return result, rows.Err()
}
func (s *Store) SupportThread(ctx context.Context, who Identity, id string, admin bool) (SupportTicketThread, error) {
	result := SupportTicketThread{Messages: []SupportMessage{}}
	tx, err := s.operationsTx(ctx, who, admin, false)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	result.Ticket, err = scanSupportTicket(tx.QueryRow(ctx, `SELECT `+supportTicketColumns+` FROM support_tickets WHERE id=$1 AND ($2 OR tenant_id=$3) FOR SHARE`, id, admin, who.TenantID))
	if err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT id,author_kind,body,created_at FROM (SELECT id,author_kind,body,created_at FROM support_messages WHERE ticket_id=$1 ORDER BY id DESC LIMIT 100) recent ORDER BY id`, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var v SupportMessage
		if err = rows.Scan(&v.ID, &v.AuthorKind, &v.Body, &v.CreatedAt); err != nil {
			return result, err
		}
		result.Messages = append(result.Messages, v)
	}
	return result, rows.Err()
}
func (s *Store) ReplySupportTicket(ctx context.Context, who Identity, id, body, status string, revision int64, admin bool) (SupportTicket, error) {
	body, err := supportText(body, 4000)
	if err != nil || revision < 1 {
		return SupportTicket{}, ErrInvalid
	}
	if admin {
		if status != "open" && status != "waiting-customer" && status != "resolved" {
			return SupportTicket{}, ErrInvalid
		}
	} else {
		if status != "" {
			return SupportTicket{}, ErrInvalid
		}
		status = "open"
	}
	tx, err := s.operationsTx(ctx, who, admin, true)
	if err != nil {
		return SupportTicket{}, err
	}
	defer tx.Rollback(ctx)
	ticket, err := scanSupportTicket(tx.QueryRow(ctx, `SELECT `+supportTicketColumns+` FROM support_tickets WHERE id=$1 AND ($2 OR tenant_id=$3) FOR UPDATE`, id, admin, who.TenantID))
	if err != nil {
		return SupportTicket{}, err
	}
	if ticket.Revision != revision {
		return SupportTicket{}, ErrConflict
	}
	kind := "tenant"
	if admin {
		kind = "platform"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO support_messages(ticket_id,tenant_id,author_id,author_kind,body) VALUES($1,$2,$3,$4,$5)`, id, ticket.TenantID, who.MemberID, kind, body); err != nil {
		return SupportTicket{}, err
	}
	ticket, err = scanSupportTicket(tx.QueryRow(ctx, `UPDATE support_tickets SET status=$2,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 RETURNING `+supportTicketColumns, id, status))
	if err != nil {
		return SupportTicket{}, err
	}
	if admin {
		err = platformLog(ctx, tx, &who, "support.reply", id, map[string]any{"tenantId": ticket.TenantID, "status": status, "revision": ticket.Revision})
	} else {
		err = audit(ctx, tx, who, "support.reply", id)
	}
	if err != nil {
		return SupportTicket{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SupportTicket{}, err
	}
	return ticket, nil
}
