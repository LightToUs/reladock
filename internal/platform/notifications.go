package platform

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/notify"
)

// NotificationMigrationSQL is registered as migration 15 by the parent.
//
//go:embed migration15.sql
var NotificationMigrationSQL string

const NotificationMaxAttempts = 8
const NotificationLease = 30 * time.Second

// Notification is a private worker lease. Send only Event to the webhook.
type Notification struct {
	Event      notify.Event `json:"event"`
	LeaseToken string       `json:"-"`
	LeaseUntil time.Time    `json:"-"`
	Attempts   int          `json:"attempts"`
}

// EnqueueIncidentNotifications repairs/backfills CURRENT states only. Reliable
// transitions, including short open->resolved, are captured by migration 15's
// operations_incidents trigger in the incident transaction, not by this poll.
func (s *Store) EnqueueIncidentNotifications(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalid
	}
	var inserted int
	err := s.pool.QueryRow(ctx, `WITH candidates AS (
 SELECT i.* FROM operations_incidents i WHERE NOT EXISTS(
 SELECT 1 FROM notification_outbox n WHERE n.incident_id=i.id AND n.occurrences=i.occurrences AND n.incident_state=i.state)
 ORDER BY i.opened_at,i.id LIMIT $1
 ), inserted AS (
 INSERT INTO notification_outbox(event_id,incident_id,occurrences,incident_state,payload)
 SELECT 'incident:'||id::text||':'||occurrences::text||':'||state,id,occurrences,state,
 notification_incident_payload(id,node_id,kind,state,occurrences,CASE WHEN state='open' THEN opened_at ELSE resolved_at END)
 FROM candidates ON CONFLICT(incident_id,occurrences,incident_state) DO NOTHING RETURNING 1)
 SELECT count(*) FROM inserted`, limit).Scan(&inserted)
	return inserted, err
}

// ClaimNotification leases at most one event, fencing expired workers with a
// fresh token. No event means (nil,nil). Only call when the sender is enabled.
func (s *Store) ClaimNotification(ctx context.Context) (*Notification, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// Bounded reclamation also marks last-attempt process crashes as exhausted.
	if _, err = tx.Exec(ctx, `UPDATE notification_outbox SET state='exhausted',lease_until=NULL,last_error_code='lease-expired'
 WHERE event_id IN (SELECT event_id FROM notification_outbox WHERE state='claimed' AND lease_until<=clock_timestamp() AND attempts>=8 ORDER BY sequence LIMIT 100 FOR UPDATE SKIP LOCKED)`); err != nil {
		return nil, err
	}
	var eventID string
	err = tx.QueryRow(ctx, `SELECT event_id FROM notification_outbox WHERE attempts<8 AND
 ((state='pending' AND next_attempt_at<=clock_timestamp()) OR (state='claimed' AND lease_until<=clock_timestamp()))
 ORDER BY sequence LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	n := &Notification{LeaseToken: sessionToken()}
	var raw []byte
	err = tx.QueryRow(ctx, `UPDATE notification_outbox SET state='claimed',attempts=attempts+1,lease_token=$2,lease_until=clock_timestamp()+interval '30 seconds'
 WHERE event_id=$1 RETURNING payload,attempts,lease_until`, eventID, n.LeaseToken).Scan(&raw, &n.Attempts, &n.LeaseUntil)
	if err != nil {
		return nil, err
	}
	if len(raw) > notify.MaxEventBytes || json.Unmarshal(raw, &n.Event) != nil || !n.Event.Valid() {
		return nil, ErrInvalid
	}
	return n, tx.Commit(ctx)
}

// ReportNotification records only a real 2xx delivery acknowledgement as
// delivered. A failed/uncertain request is retried with the same event ID and
// content. The receiver MUST deduplicate that ID: delivery is at least once.
// These methods are worker-internal and must not be exposed as tenant APIs.
func (s *Store) ReportNotification(ctx context.Context, eventID, leaseToken string, delivered bool, errorCode string) error {
	if len(eventID) > 128 || !strings.HasPrefix(eventID, "incident:") || len(leaseToken) != 43 || delivered && errorCode != "" || !delivered && errorCode != "delivery-failed" {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state, token, lastCode string
	var attempts int
	var leaseValid bool
	err = tx.QueryRow(ctx, `SELECT state,COALESCE(lease_token,''),last_error_code,attempts,COALESCE(lease_until>clock_timestamp(),false)
 FROM notification_outbox WHERE event_id=$1 FOR UPDATE`, eventID).Scan(&state, &token, &lastCode, &attempts, &leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if token != leaseToken {
		return ErrConflict
	}
	if delivered && state == "delivered" || !delivered && (state == "pending" || state == "exhausted") && lastCode == errorCode {
		return tx.Commit(ctx)
	}
	if state != "claimed" || !leaseValid {
		return ErrConflict
	}
	if delivered {
		_, err = tx.Exec(ctx, `UPDATE notification_outbox SET state='delivered',lease_until=NULL,last_error_code='',delivered_at=clock_timestamp() WHERE event_id=$1`, eventID)
	} else {
		delay := 5 * (1 << uint(attempts-1))
		if delay > 300 {
			delay = 300
		}
		next := "pending"
		if attempts >= NotificationMaxAttempts {
			next = "exhausted"
		}
		_, err = tx.Exec(ctx, `UPDATE notification_outbox SET state=$2,lease_until=NULL,last_error_code=$3,next_attempt_at=clock_timestamp()+$4*interval '1 second' WHERE event_id=$1`, eventID, next, errorCode, delay)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// NotificationDelivery deliberately excludes the payload, endpoint and tokens.
type NotificationDelivery struct {
	EventID       string     `json:"eventId"`
	NodeID        string     `json:"nodeId"`
	Kind          string     `json:"kind"`
	IncidentState string     `json:"incidentState"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	ErrorCode     string     `json:"errorCode"`
	CreatedAt     time.Time  `json:"createdAt"`
	DeliveredAt   *time.Time `json:"deliveredAt"`
}

func (s *Store) AdminNotificationDeliveries(ctx context.Context, who Identity, page int) (Page[NotificationDelivery], error) {
	result, err := operationsPage[NotificationDelivery](page)
	if err != nil {
		return result, err
	}
	tx, err := s.operationsTx(ctx, who, true, false)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM notification_outbox").Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT event_id,payload->>'nodeId',payload->>'kind',incident_state,state,attempts,last_error_code,created_at,delivered_at
 FROM notification_outbox ORDER BY sequence DESC LIMIT 100 OFFSET $1`, (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var n NotificationDelivery
		if err = rows.Scan(&n.EventID, &n.NodeID, &n.Kind, &n.IncidentState, &n.State, &n.Attempts, &n.ErrorCode, &n.CreatedAt, &n.DeliveredAt); err != nil {
			return result, err
		}
		result.Items = append(result.Items, n)
	}
	return result, rows.Err()
}

// AdminRetryNotification only requeues exhausted events. The administrator's
// permission, row transition and audit are one transaction. Keeping event ID
// and payload preserves receiver idempotency even when a previous ACK was lost.
func (s *Store) AdminRetryNotification(ctx context.Context, who Identity, eventID string) error {
	if !validNotificationEventID(eventID) {
		return ErrInvalid
	}
	tx, err := s.operationsTx(ctx, who, true, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	err = tx.QueryRow(ctx, "SELECT state FROM notification_outbox WHERE event_id=$1 FOR UPDATE", eventID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "pending" {
		return tx.Commit(ctx)
	}
	if state != "exhausted" {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE notification_outbox SET state='pending',attempts=0,lease_token=NULL,lease_until=NULL,
 last_error_code='',next_attempt_at=clock_timestamp() WHERE event_id=$1`, eventID); err != nil {
		return err
	}
	if err = platformLog(ctx, tx, &who, "notification.retry", eventID, map[string]any{"previousState": "exhausted"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validNotificationEventID(id string) bool {
	parts := strings.Split(id, ":")
	if len(parts) != 4 || parts[0] != "incident" || !billingUUID.MatchString(parts[1]) || (parts[3] != "open" && parts[3] != "resolved") {
		return false
	}
	occurrence, err := strconv.ParseInt(parts[2], 10, 64)
	return err == nil && occurrence > 0 && strconv.FormatInt(occurrence, 10) == parts[2]
}
