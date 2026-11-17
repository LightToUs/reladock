package platform

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"reladock/internal/sourceflow"
)

// SourceFlowEvent is connection metadata observed from a conntrack NEW event.
// It deliberately has no payload, DNS, SNI, URL, or packet-capture fields.
type SourceFlowEvent struct {
	EventID          string    `json:"eventId"`
	TenantID         string    `json:"tenantId"`
	RuleID           string    `json:"ruleId"`
	NodeID           string    `json:"nodeId"`
	AllocationID     string    `json:"allocationId"`
	ConfigGeneration int64     `json:"configGeneration"`
	RuleVersion      int64     `json:"ruleVersion"`
	Protocol         string    `json:"protocol"`
	SourceIP         string    `json:"sourceIp"`
	SourcePort       int       `json:"sourcePort"`
	RelayIP          string    `json:"relayIp"`
	RelayPort        int       `json:"relayPort"`
	TargetIP         string    `json:"targetIp"`
	TargetPort       int       `json:"targetPort"`
	ObservedAt       time.Time `json:"observedAt"`
	ReceivedAt       time.Time `json:"receivedAt,omitempty"`
	KernelBootID     string    `json:"kernelBootId"`
	ConntrackZone    uint16    `json:"conntrackZone"`
	ConntrackID      uint32    `json:"conntrackId"`
	KnownGap         bool      `json:"knownGap"`
	Company          string    `json:"company"`
	RuleName         string    `json:"ruleName"`
}

type SourceFlowQuery struct {
	From, To           time.Time
	RuleID, NodeID     string
	Protocol, SourceIP string
	Page               int
}

type SourceFlowPage struct {
	Items         []SourceFlowEvent `json:"items"`
	Gaps          []SourceFlowGap   `json:"gaps"`
	Total         int64             `json:"total"`
	Page          int               `json:"page"`
	PageSize      int               `json:"pageSize"`
	HasMore       bool              `json:"hasMore"`
	TotalCapped   bool              `json:"totalCapped"`
	GapsTruncated bool              `json:"gapsTruncated"`
	RetentionDays int               `json:"retentionDays"`
}

func (s *Store) AdminSourceFlows(ctx context.Context, who Identity, query SourceFlowQuery) (SourceFlowPage, error) {
	result := SourceFlowPage{Items: []SourceFlowEvent{}, Gaps: []SourceFlowGap{}, Page: query.Page, PageSize: 50, RetentionDays: s.sourceFlowRetentionDays}
	query.From = query.From.UTC()
	query.To = query.To.UTC()
	if query.Page < 1 || query.Page > 200 || query.From.IsZero() || !query.To.After(query.From) || query.To.Sub(query.From) > 24*time.Hour || query.To.After(time.Now().UTC().Add(5*time.Minute)) || query.Protocol != "" && query.Protocol != "tcp" && query.Protocol != "udp" || query.RuleID != "" && !billingUUID.MatchString(query.RuleID) || query.NodeID != "" && !nodeIDPattern.MatchString(query.NodeID) {
		return result, ErrInvalid
	}
	if query.SourceIP != "" {
		ip, err := netip.ParseAddr(query.SourceIP)
		if err != nil || !ip.Is4() || ip.IsUnspecified() {
			return result, ErrInvalid
		}
		query.SourceIP = ip.Unmap().String()
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	where := ` WHERE e.observed_at>=$1 AND e.observed_at<$2
	 AND ($3='' OR e.rule_id=NULLIF($3,'')::uuid) AND ($4='' OR e.node_id=$4)
	AND ($5='' OR e.protocol=$5) AND ($6='' OR e.source_ip=NULLIF($6,'')::inet)`
	args := []any{query.From, query.To, query.RuleID, query.NodeID, query.Protocol, query.SourceIP}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM source_flow_events e`+where+` LIMIT 10001) bounded`, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	result.TotalCapped = result.Total > 10000
	result.HasMore = query.Page < 200 && int64(query.Page*result.PageSize) < result.Total
	rows, err := tx.Query(ctx, `SELECT e.event_id::text,e.tenant_id::text,e.rule_id::text,e.node_id,e.allocation_id::text,e.config_generation,e.rule_version,e.protocol,host(e.source_ip),e.source_port,host(e.relay_ip),e.relay_port,host(e.target_ip),e.target_port,e.observed_at,e.received_at,e.kernel_boot_id::text,e.conntrack_zone,e.conntrack_id,
EXISTS(SELECT 1 FROM source_flow_gaps g WHERE g.node_id=e.node_id AND g.opened_at<=e.observed_at AND (g.closed_at IS NULL OR g.closed_at>=e.observed_at))
,t.name,r.name FROM source_flow_events e JOIN tenants t ON t.id=e.tenant_id JOIN relay_rules r ON r.id=e.rule_id`+where+` ORDER BY e.observed_at DESC,e.event_id DESC LIMIT 50 OFFSET $7`, append(args, (query.Page-1)*50)...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var event SourceFlowEvent
		if err = rows.Scan(&event.EventID, &event.TenantID, &event.RuleID, &event.NodeID, &event.AllocationID, &event.ConfigGeneration, &event.RuleVersion, &event.Protocol, &event.SourceIP, &event.SourcePort, &event.RelayIP, &event.RelayPort, &event.TargetIP, &event.TargetPort, &event.ObservedAt, &event.ReceivedAt, &event.KernelBootID, &event.ConntrackZone, &event.ConntrackID, &event.KnownGap, &event.Company, &event.RuleName); err != nil {
			return result, err
		}
		result.Items = append(result.Items, event)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	gapRows, err := tx.Query(ctx, `SELECT g.node_id,g.collector_epoch::text,g.sequence,g.reason,g.dropped_count,g.opened_at,g.closed_at,g.received_at
FROM source_flow_gaps g
WHERE g.opened_at<$2 AND (g.closed_at IS NULL OR g.closed_at>=$1)
  AND ($4='' OR g.node_id=$4)
  AND ($3='' OR EXISTS(SELECT 1 FROM relay_rules r WHERE r.id=NULLIF($3,'')::uuid AND r.node_id=g.node_id))
ORDER BY g.opened_at DESC,g.node_id,g.collector_epoch,g.sequence LIMIT 201`, args[:4]...)
	if err != nil {
		return result, err
	}
	defer gapRows.Close()
	for gapRows.Next() {
		var gap SourceFlowGap
		if err = gapRows.Scan(&gap.NodeID, &gap.CollectorEpoch, &gap.Sequence, &gap.Reason, &gap.DroppedCount, &gap.OpenedAt, &gap.ClosedAt, &gap.ReceivedAt); err != nil {
			return result, err
		}
		result.Gaps = append(result.Gaps, gap)
	}
	if err = gapRows.Err(); err != nil {
		return result, err
	}
	if len(result.Gaps) > 200 {
		result.Gaps = result.Gaps[:200]
		result.GapsTruncated = true
	}
	if err = platformLog(ctx, tx, &who, "source-flow.query", "source-flows", map[string]any{"from": query.From, "to": query.To, "ruleId": query.RuleID, "nodeId": query.NodeID, "protocol": query.Protocol, "page": query.Page, "resultCount": len(result.Items)}); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

// PruneSourceFlows deletes only optional source metadata in bounded batches.
// Billing usage, wallet, and audit facts are deliberately outside this worker.
func (s *Store) PruneSourceFlows(ctx context.Context, limit int) (int, error) {
	if s.sourceFlowRetentionDays == 0 || limit < 1 || limit > 10000 {
		return 0, ErrInvalid
	}
	cutoff := time.Now().UTC().Add(-time.Duration(s.sourceFlowRetentionDays) * 24 * time.Hour)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	deleted := 0
	for _, statement := range []string{
		`WITH doomed AS (SELECT node_id,event_id FROM source_flow_events WHERE received_at<$1 ORDER BY received_at,node_id,event_id LIMIT $2 FOR UPDATE SKIP LOCKED) DELETE FROM source_flow_events e USING doomed d WHERE e.node_id=d.node_id AND e.event_id=d.event_id`,
		`WITH doomed AS (SELECT node_id,collector_epoch,sequence FROM source_flow_gaps WHERE closed_at IS NOT NULL AND received_at<$1 ORDER BY received_at,node_id,collector_epoch,sequence LIMIT $2 FOR UPDATE SKIP LOCKED) DELETE FROM source_flow_gaps g USING doomed d WHERE g.node_id=d.node_id AND g.collector_epoch=d.collector_epoch AND g.sequence=d.sequence`,
	} {
		tag, execErr := tx.Exec(ctx, statement, cutoff, limit-deleted)
		if execErr != nil {
			return 0, execErr
		}
		deleted += int(tag.RowsAffected())
		if deleted >= limit {
			break
		}
	}
	if deleted < limit {
		tag, execErr := tx.Exec(ctx, `WITH doomed AS (
SELECT a.node_id,a.config_generation,a.protocol,a.relay_ip,a.relay_port FROM applied_source_flow_rules a
WHERE a.superseded_at<$1
  AND NOT EXISTS(SELECT 1 FROM nodes n WHERE n.id=a.node_id AND n.applied_generation=a.config_generation)
  AND NOT EXISTS(SELECT 1 FROM source_flow_events e WHERE e.node_id=a.node_id AND e.config_generation=a.config_generation)
ORDER BY a.superseded_at,a.node_id,a.config_generation LIMIT $2 FOR UPDATE SKIP LOCKED)
DELETE FROM applied_source_flow_rules a USING doomed d WHERE a.node_id=d.node_id AND a.config_generation=d.config_generation AND a.protocol=d.protocol AND a.relay_ip=d.relay_ip AND a.relay_port=d.relay_port`, cutoff, limit-deleted)
		if execErr != nil {
			return 0, execErr
		}
		deleted += int(tag.RowsAffected())
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deleted, nil
}

type SourceFlowGap struct {
	NodeID         string     `json:"nodeId"`
	CollectorEpoch string     `json:"collectorEpoch"`
	Sequence       int64      `json:"sequence"`
	Reason         string     `json:"reason"`
	DroppedCount   int64      `json:"droppedCount"`
	OpenedAt       time.Time  `json:"openedAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
	ReceivedAt     time.Time  `json:"receivedAt,omitempty"`
}

type AppliedSourceFlowRule struct {
	NodeID               string    `json:"nodeId"`
	ConfigGeneration     int64     `json:"configGeneration"`
	TenantID             string    `json:"tenantId"`
	RuleID               string    `json:"ruleId"`
	AllocationID         string    `json:"allocationId"`
	RuleVersion          int64     `json:"ruleVersion"`
	SourceLoggingEnabled bool      `json:"sourceLoggingEnabled"`
	Protocol             string    `json:"protocol"`
	RelayIP              string    `json:"relayIp"`
	RelayPort            int       `json:"relayPort"`
	TargetIP             string    `json:"targetIp"`
	TargetPort           int       `json:"targetPort"`
	ManifestDigest       string    `json:"manifestDigest"`
	AppliedAt            time.Time `json:"appliedAt,omitempty"`
}

type SourceFlowStatus struct {
	NodeID            string     `json:"nodeId"`
	CollectorStatus   string     `json:"collectorStatus"`
	SubscriptionAlive bool       `json:"subscriptionAlive"`
	BacklogCount      int64      `json:"backlogCount"`
	OldestBacklogAt   *time.Time `json:"oldestBacklogAt,omitempty"`
	WALBytesUsed      int64      `json:"walBytesUsed"`
	WALBytesLimit     int64      `json:"walBytesLimit"`
	DroppedTotal      int64      `json:"droppedTotal"`
	OpenGap           bool       `json:"openGap"`
	CollectorEpoch    string     `json:"collectorEpoch"`
	CollectorSequence int64      `json:"collectorSequence"`
	ObservedAt        time.Time  `json:"observedAt"`
	ReceivedAt        time.Time  `json:"receivedAt"`
}

func (s *Store) IngestSourceFlows(ctx context.Context, authenticatedNodeID string, batch sourceflow.Batch) (sourceflow.Receipt, error) {
	receipt := batch.Receipt()
	if s.sourceFlowRetentionDays == 0 {
		return sourceflow.Receipt{}, ErrSourceFlowRetentionUnavailable
	}
	if batch.NodeID != authenticatedNodeID || len(batch.Events)+len(batch.Gaps) < 1 || len(batch.Events)+len(batch.Gaps) > 256 {
		return sourceflow.Receipt{}, ErrInvalid
	}
	now := time.Now().UTC()
	oldest := now.Add(-time.Duration(s.sourceFlowRetentionDays) * 24 * time.Hour)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return sourceflow.Receipt{}, err
	}
	defer tx.Rollback(ctx)
	events := append([]sourceflow.Event(nil), batch.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].EventID < events[j].EventID })
	for _, event := range events {
		if err = sourceflow.ValidateEvent(event, authenticatedNodeID); err != nil || event.ObservedAt.After(now.Add(5*time.Minute)) || event.ObservedAt.Before(oldest) {
			return sourceflow.Receipt{}, ErrInvalid
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "source-flow-event:"+authenticatedNodeID+":"+event.EventID); err != nil {
			return sourceflow.Receipt{}, err
		}
		var applied sourceflow.AppliedRule
		err = tx.QueryRow(ctx, `SELECT allocation_id::text,tenant_id::text,rule_id::text,rule_version,source_logging_enabled,protocol,host(relay_ip),relay_port,host(target_ip),target_port
FROM applied_source_flow_rules WHERE node_id=$1 AND config_generation=$2 AND allocation_id=$3`, authenticatedNodeID, event.ConfigGeneration, event.AllocationID).Scan(
			&applied.AllocationID, &applied.TenantID, &applied.RuleID, &applied.RuleVersion, &applied.SourceLoggingEnabled, &applied.Protocol, &applied.RelayIP, &applied.RelayPort, &applied.TargetIP, &applied.TargetPort)
		if err != nil || !applied.SourceLoggingEnabled || applied.TenantID != event.TenantID || applied.RuleID != event.RuleID || applied.RuleVersion != event.RuleVersion || applied.Protocol != event.Protocol || applied.RelayIP != event.RelayIP || applied.RelayPort != event.RelayPort || applied.TargetIP != event.TargetIP || applied.TargetPort != event.TargetPort {
			return sourceflow.Receipt{}, ErrInvalid
		}
		normalized := event
		normalized.ObservedAt = normalized.ObservedAt.UTC().Truncate(time.Microsecond)
		if ip, parseErr := netip.ParseAddr(normalized.SourceIP); parseErr == nil {
			normalized.SourceIP = ip.Unmap().String()
		}
		var existing sourceflow.Event
		err = tx.QueryRow(ctx, `SELECT event_id::text,tenant_id::text,rule_id::text,node_id,allocation_id::text,config_generation,rule_version,protocol,host(source_ip),source_port,host(relay_ip),relay_port,host(target_ip),target_port,observed_at,kernel_boot_id::text,conntrack_zone,conntrack_id
FROM source_flow_events WHERE node_id=$1 AND event_id=$2`, authenticatedNodeID, event.EventID).Scan(
			&existing.EventID, &existing.TenantID, &existing.RuleID, &existing.NodeID, &existing.AllocationID, &existing.ConfigGeneration, &existing.RuleVersion, &existing.Protocol, &existing.SourceIP, &existing.SourcePort, &existing.RelayIP, &existing.RelayPort, &existing.TargetIP, &existing.TargetPort, &existing.ObservedAt, &existing.KernelBootID, &existing.ConntrackZone, &existing.ConntrackID)
		if err == nil {
			existing.ObservedAt = existing.ObservedAt.UTC().Truncate(time.Microsecond)
			if !reflect.DeepEqual(existing, normalized) {
				return sourceflow.Receipt{}, ErrInvalid
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return sourceflow.Receipt{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO source_flow_events(node_id,event_id,tenant_id,rule_id,allocation_id,config_generation,rule_version,protocol,source_ip,source_port,relay_ip,relay_port,target_ip,target_port,observed_at,kernel_boot_id,conntrack_zone,conntrack_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, authenticatedNodeID, event.EventID, event.TenantID, event.RuleID, event.AllocationID, event.ConfigGeneration, event.RuleVersion, event.Protocol, event.SourceIP, event.SourcePort, event.RelayIP, event.RelayPort, event.TargetIP, event.TargetPort, normalized.ObservedAt, event.KernelBootID, event.ConntrackZone, event.ConntrackID)
		if err != nil {
			return sourceflow.Receipt{}, err
		}
	}
	gaps := append([]sourceflow.Gap(nil), batch.Gaps...)
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].CollectorEpoch == gaps[j].CollectorEpoch {
			return gaps[i].Sequence < gaps[j].Sequence
		}
		return gaps[i].CollectorEpoch < gaps[j].CollectorEpoch
	})
	for _, gap := range gaps {
		if err = sourceflow.ValidateGap(gap, authenticatedNodeID); err != nil || gap.OpenedAt.After(now.Add(5*time.Minute)) || gap.ClosedAt != nil && gap.ClosedAt.After(now.Add(5*time.Minute)) {
			return sourceflow.Receipt{}, ErrInvalid
		}
		lockKey := "source-flow-gap:" + authenticatedNodeID + ":" + gap.CollectorEpoch + ":" + strconv.FormatInt(gap.Sequence, 10)
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return sourceflow.Receipt{}, err
		}
		openedAt := gap.OpenedAt.UTC().Truncate(time.Microsecond)
		var closedAt *time.Time
		if gap.ClosedAt != nil {
			value := gap.ClosedAt.UTC().Truncate(time.Microsecond)
			closedAt = &value
		}
		var existing sourceflow.Gap
		err = tx.QueryRow(ctx, `SELECT node_id,collector_epoch::text,sequence,reason,dropped_count,opened_at,closed_at FROM source_flow_gaps WHERE node_id=$1 AND collector_epoch=$2 AND sequence=$3 FOR UPDATE`, authenticatedNodeID, gap.CollectorEpoch, gap.Sequence).Scan(&existing.NodeID, &existing.CollectorEpoch, &existing.Sequence, &existing.Reason, &existing.DroppedCount, &existing.OpenedAt, &existing.ClosedAt)
		if err == nil {
			existing.OpenedAt = existing.OpenedAt.UTC().Truncate(time.Microsecond)
			if existing.ClosedAt != nil {
				value := existing.ClosedAt.UTC().Truncate(time.Microsecond)
				existing.ClosedAt = &value
			}
			if existing.NodeID != gap.NodeID || existing.CollectorEpoch != gap.CollectorEpoch || existing.Sequence != gap.Sequence || existing.Reason != gap.Reason || !existing.OpenedAt.Equal(openedAt) || gap.DroppedCount < existing.DroppedCount || existing.ClosedAt != nil && (gap.DroppedCount != existing.DroppedCount || closedAt == nil || !existing.ClosedAt.Equal(*closedAt)) {
				return sourceflow.Receipt{}, ErrInvalid
			}
			if existing.ClosedAt == nil && (gap.DroppedCount != existing.DroppedCount || closedAt != nil) {
				if _, err = tx.Exec(ctx, `UPDATE source_flow_gaps SET dropped_count=$4,closed_at=$5,received_at=clock_timestamp() WHERE node_id=$1 AND collector_epoch=$2 AND sequence=$3`, authenticatedNodeID, gap.CollectorEpoch, gap.Sequence, gap.DroppedCount, closedAt); err != nil {
					return sourceflow.Receipt{}, err
				}
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return sourceflow.Receipt{}, err
		}
		if gap.OpenedAt.Before(oldest) {
			return sourceflow.Receipt{}, ErrInvalid
		}
		_, err = tx.Exec(ctx, `INSERT INTO source_flow_gaps(node_id,collector_epoch,sequence,reason,dropped_count,opened_at,closed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, authenticatedNodeID, gap.CollectorEpoch, gap.Sequence, gap.Reason, gap.DroppedCount, openedAt, closedAt)
		if err != nil {
			return sourceflow.Receipt{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return sourceflow.Receipt{}, err
	}
	return receipt, nil
}

func validateSourceFlowApplication(cfg NodeConfig, manifest *sourceflow.Manifest, digest string) ([]sourceflow.AppliedRule, error) {
	loggingRules := 0
	for _, rule := range cfg.Rules {
		if rule.SourceLoggingEnabled {
			loggingRules++
		}
	}
	if loggingRules == 0 {
		if manifest != nil || digest != "" {
			return nil, ErrInvalid
		}
		return nil, nil
	}
	if cfg.SchemaVersion < 3 || cfg.SchemaVersion > 5 {
		return nil, ErrInvalid
	}
	if manifest == nil || sourceflow.ValidateManifest(*manifest, digest) != nil || manifest.NodeID != cfg.NodeID || manifest.Generation != cfg.Generation {
		return nil, ErrInvalid
	}
	rules := make(map[string]ConfigRule, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		if rule.SourceLoggingEnabled {
			rules[rule.ID] = rule
		}
	}
	allocations := make(map[string]ConfigAllocation, len(cfg.Allocations))
	for _, allocation := range cfg.Allocations {
		if rules[allocation.RuleID].ID != "" {
			allocations[allocation.ID] = allocation
		}
	}
	if len(allocations) != len(manifest.Rules) || len(allocations) == 0 {
		return nil, ErrInvalid
	}
	for _, applied := range manifest.Rules {
		allocation, ok := allocations[applied.AllocationID]
		if !ok {
			return nil, ErrInvalid
		}
		rule := rules[allocation.RuleID]
		if applied.TenantID != rule.TenantID || applied.RuleID != rule.ID || applied.RuleVersion != rule.Version || !applied.SourceLoggingEnabled || applied.Protocol != allocation.Protocol || applied.RelayIP != allocation.Address || applied.RelayPort != allocation.Port || applied.TargetPort != rule.TargetPort {
			return nil, ErrInvalid
		}
		if target, err := netip.ParseAddr(rule.TargetHost); err == nil && target.Unmap().String() != applied.TargetIP {
			return nil, ErrInvalid
		}
		delete(allocations, applied.AllocationID)
	}
	if len(allocations) != 0 {
		return nil, ErrInvalid
	}
	return append([]sourceflow.AppliedRule(nil), manifest.Rules...), nil
}
