package platform

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type NodeCostInput struct {
	NodeID      string    `json:"nodeId"`
	AmountCents int64     `json:"amountCents"`
	ServiceFrom time.Time `json:"serviceFrom"`
	ServiceTo   time.Time `json:"serviceTo"`
	InvoiceRef  string    `json:"invoiceRef"`
}
type NodeCostEntry struct {
	ID string `json:"id"`
	NodeCostInput
	Currency       string    `json:"currency"`
	IdempotencyKey string    `json:"idempotencyKey"`
	ActorID        string    `json:"actorId"`
	ActorEmail     string    `json:"actorEmail"`
	RecordedAt     time.Time `json:"recordedAt"`
}
type CashMarginAmounts struct {
	RevenueCents         int64  `json:"revenueCents"`
	CostCents            *int64 `json:"costCents"`
	MarginCents          *int64 `json:"marginCents"`
	HasCost              bool   `json:"hasCost"`
	CostCoverageComplete bool   `json:"costCoverageComplete"`
}
type NodeCashMargin struct {
	NodeID string `json:"nodeId"`
	Name   string `json:"name"`
	CashMarginAmounts
}
type CashMarginReport struct {
	From      time.Time            `json:"from"`
	To        time.Time            `json:"to"`
	Currency  string               `json:"currency"`
	Basis     string               `json:"basis"`
	TimeBasis string               `json:"timeBasis"`
	Totals    CashMarginAmounts    `json:"totals"`
	Nodes     Page[NodeCashMargin] `json:"nodes"`
}

func costTime(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }
func validCostTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 1970 && value.Year() <= 9999
}
func validCostWindow(from, to time.Time) bool {
	return validCostTime(from) && validCostTime(to) && to.After(from) && to.Sub(from) <= 366*24*time.Hour
}
func costPage[T any](page int) (Page[T], error) {
	result := Page[T]{Items: []T{}, Page: page, PageSize: 100}
	if page < 1 || page > 100000 {
		return result, ErrInvalid
	}
	return result, nil
}

const nodeCostColumns = `id,node_id,amount_cents,service_from,service_to,invoice_ref,currency,idempotency_key,actor_id,actor_email,recorded_at`

func scanNodeCost(row pgx.Row) (NodeCostEntry, error) {
	var v NodeCostEntry
	err := row.Scan(&v.ID, &v.NodeID, &v.AmountCents, &v.ServiceFrom, &v.ServiceTo, &v.InvoiceRef, &v.Currency, &v.IdempotencyKey, &v.ActorID, &v.ActorEmail, &v.RecordedAt)
	return v, err
}

// AdminRecordNodeCost appends an explicitly entered USD cost and audit together.
// Key uniqueness is platform-wide; invoice uniqueness is per node. Exact key
// replays return the original actor/time and never add another cost or audit.
// There are deliberately no update/delete APIs, tenant allocations or defaults.
func (s *Store) AdminRecordNodeCost(ctx context.Context, who Identity, key string, input NodeCostInput) (NodeCostEntry, error) {
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return NodeCostEntry{}, err
	}
	defer tx.Rollback(ctx)
	input.InvoiceRef = strings.TrimSpace(input.InvoiceRef)
	input.ServiceFrom = costTime(input.ServiceFrom)
	input.ServiceTo = costTime(input.ServiceTo)
	if !nodeIDPattern.MatchString(input.NodeID) || input.AmountCents <= 0 || !idempotencyKey.MatchString(key) || !billingText(input.InvoiceRef, 160) || !validCostTime(input.ServiceFrom) || !validCostTime(input.ServiceTo) || !input.ServiceTo.After(input.ServiceFrom) {
		return NodeCostEntry{}, ErrInvalid
	}
	for _, c := range input.InvoiceRef {
		if unicode.IsControl(c) {
			return NodeCostEntry{}, ErrInvalid
		}
	}
	old, err := scanNodeCost(tx.QueryRow(ctx, `SELECT `+nodeCostColumns+` FROM node_cost_entries WHERE idempotency_key=$1`, key))
	if err == nil {
		if old.NodeID != input.NodeID || old.AmountCents != input.AmountCents || old.InvoiceRef != input.InvoiceRef || !old.ServiceFrom.Equal(input.ServiceFrom) || !old.ServiceTo.Equal(input.ServiceTo) {
			return NodeCostEntry{}, ErrConflict
		}
		return old, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return NodeCostEntry{}, err
	}
	var node string
	err = tx.QueryRow(ctx, `SELECT id FROM nodes WHERE id=$1 FOR KEY SHARE`, input.NodeID).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeCostEntry{}, ErrNotFound
	}
	if err != nil {
		return NodeCostEntry{}, err
	}
	// Actor email comes from the locked DB identity, never from a supplied label.
	if err = tx.QueryRow(ctx, `SELECT email FROM members WHERE id=$1 AND tenant_id=$2`, who.MemberID, who.TenantID).Scan(&who.Email); err != nil {
		return NodeCostEntry{}, err
	}
	entry, err := scanNodeCost(tx.QueryRow(ctx, `INSERT INTO node_cost_entries(id,node_id,amount_cents,service_from,service_to,invoice_ref,idempotency_key,actor_id,actor_email) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+nodeCostColumns, newID(), input.NodeID, input.AmountCents, input.ServiceFrom, input.ServiceTo, input.InvoiceRef, key, who.MemberID, who.Email))
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			err = ErrConflict
		}
		return NodeCostEntry{}, err
	}
	if err = platformLog(ctx, tx, &who, "node-cost.record", entry.ID, map[string]any{"nodeId": input.NodeID, "amountCents": input.AmountCents, "currency": "USD", "invoiceRef": input.InvoiceRef, "serviceFrom": input.ServiceFrom, "serviceTo": input.ServiceTo}); err != nil {
		return NodeCostEntry{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return NodeCostEntry{}, err
	}
	return entry, nil
}

// AdminNodeCosts lists immutable entries by server recorded_at in [from,to),
// not by overlap of the descriptive service period. Explicit UTC-normalized
// windows up to 366 days and pages of 100 keep retrieval bounded.
func (s *Store) AdminNodeCosts(ctx context.Context, who Identity, from, to time.Time, page int) (Page[NodeCostEntry], error) {
	result, err := costPage[NodeCostEntry](page)
	if err != nil {
		return result, err
	}
	from, to = costTime(from), costTime(to)
	if !validCostWindow(from, to) {
		return result, ErrInvalid
	}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM node_cost_entries WHERE recorded_at>=$1 AND recorded_at<$2`, from, to).Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT `+nodeCostColumns+` FROM node_cost_entries WHERE recorded_at>=$1 AND recorded_at<$2 ORDER BY recorded_at DESC,id LIMIT 100 OFFSET $3`, from, to, (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		v, e := scanNodeCost(rows)
		if e != nil {
			return result, e
		}
		result.Items = append(result.Items, v)
	}
	return result, rows.Err()
}

func cashAmounts(revenue, cost string, hasCost, complete bool) (CashMarginAmounts, error) {
	result := CashMarginAmounts{HasCost: hasCost, CostCoverageComplete: complete}
	r, err := strconv.ParseInt(revenue, 10, 64)
	if err != nil || r < 0 {
		return result, ErrInvalid
	}
	c, err := strconv.ParseInt(cost, 10, 64)
	if err != nil || c < 0 {
		return result, ErrInvalid
	}
	result.RevenueCents = r
	if hasCost {
		result.CostCents = &c
	}
	// Both independently bounded nonnegative values fit int64; their difference
	// therefore lies in [-MaxInt64,MaxInt64] without intermediate overflow.
	if complete {
		margin := r - c
		result.MarginCents = &margin
	}
	return result, nil
}

// AdminCashMargin is a recorded cash-basis operational view, NOT accrual gross
// profit or a revenue-recognition policy. Income is actual subscription-debit
// ledger entries joined to purchases, never top-ups or configured list prices.
// Cost periods are descriptive only: whole costs enter by recorded_at [from,to).
// No usage/tenant allocation is inferred. Missing costs produce nil margin.
func (s *Store) AdminCashMargin(ctx context.Context, who Identity, from, to time.Time, page int) (CashMarginReport, error) {
	nodes, err := costPage[NodeCashMargin](page)
	result := CashMarginReport{From: costTime(from), To: costTime(to), Currency: "USD", Basis: "cash-basis", TimeBasis: "recorded-at", Nodes: nodes}
	if err != nil || !validCostWindow(result.From, result.To) {
		return result, ErrInvalid
	}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	// One statement gives totals and the page the SAME MVCC snapshot. Every sum
	// and negation uses numeric BEFORE Go range checks, including platform totals
	// across nodes not present on the current page. Deleted rules stay joined.
	rows, err := tx.Query(ctx, `WITH revenue AS (
 SELECT r.node_id,SUM(-l.amount_cents::numeric) AS cents
 FROM billing_ledger l JOIN rule_subscriptions p ON p.ledger_id=l.id AND p.tenant_id=l.tenant_id AND p.rule_id=l.rule_id AND p.price_version_id=l.price_version_id
 JOIN relay_rules r ON r.id=l.rule_id AND r.tenant_id=l.tenant_id
 WHERE l.kind='subscription-debit' AND l.currency='USD' AND l.created_at>=$1 AND l.created_at<$2 GROUP BY r.node_id
 ), costs AS (
 SELECT node_id,SUM(amount_cents::numeric) AS cents,COUNT(*) AS entries FROM node_cost_entries WHERE recorded_at>=$1 AND recorded_at<$2 GROUP BY node_id
 ), values_by_node AS (
 SELECT n.id,n.name,COALESCE(r.cents,0::numeric) AS revenue,COALESCE(c.cents,0::numeric) AS cost,COALESCE(c.entries,0)>0 AS has_cost FROM nodes n LEFT JOIN revenue r ON r.node_id=n.id LEFT JOIN costs c ON c.node_id=n.id
 ), summary AS (
 SELECT 'total' AS kind,''::text AS node_id,''::text AS name,COALESCE(SUM(revenue),0)::text AS revenue,COALESCE(SUM(cost),0)::text AS cost,COUNT(*)::bigint AS node_count,COUNT(*) FILTER(WHERE has_cost)::bigint AS cost_nodes FROM values_by_node
 UNION ALL
 SELECT 'node',id,name,revenue::text,cost::text,1::bigint,CASE WHEN has_cost THEN 1::bigint ELSE 0::bigint END FROM (SELECT * FROM values_by_node ORDER BY id LIMIT 100 OFFSET $3) page
 ) SELECT kind,node_id,name,revenue,cost,node_count,cost_nodes FROM summary ORDER BY kind DESC,node_id`, result.From, result.To, (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, node, name, revenue, cost string
		var count, known int64
		if err = rows.Scan(&kind, &node, &name, &revenue, &cost, &count, &known); err != nil {
			return result, err
		}
		amounts, e := cashAmounts(revenue, cost, known > 0, count > 0 && known == count)
		if e != nil {
			return CashMarginReport{}, e
		}
		if kind == "total" {
			result.Totals = amounts
			result.Nodes.Total = int(count)
		} else {
			result.Nodes.Items = append(result.Nodes.Items, NodeCashMargin{NodeID: node, Name: name, CashMarginAmounts: amounts})
		}
	}
	return result, rows.Err()
}
