package platform

import (
	"context"
	"github.com/jackc/pgx/v5"
	"regexp"
	"time"
)

var nodeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Node struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Region              string     `json:"region"`
	Status              string     `json:"status"`
	DesiredGeneration   int64      `json:"desiredGeneration"`
	ValidatedGeneration int64      `json:"validatedGeneration"`
	AppliedGeneration   int64      `json:"appliedGeneration"`
	LastSeenAt          *time.Time `json:"lastSeenAt"`
	AgentVersion        string     `json:"agentVersion"`
	DeliveryState       string     `json:"deliveryState"`
	DeliveryAttempts    int        `json:"deliveryAttempts"`
}

func lockNode(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "reladock-node:"+id)
	return err
}
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT n.id,n.name,n.region,n.status,n.desired_generation,n.validated_generation,n.last_seen_at,n.agent_version,COALESCE(o.state,'queued'),COALESCE(o.attempts,0),n.applied_generation FROM nodes n LEFT JOIN config_outbox o ON o.node_id=n.id AND o.generation=n.desired_generation ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []Node{}
	for rows.Next() {
		var n Node
		if err = rows.Scan(&n.ID, &n.Name, &n.Region, &n.Status, &n.DesiredGeneration, &n.ValidatedGeneration, &n.LastSeenAt, &n.AgentVersion, &n.DeliveryState, &n.DeliveryAttempts, &n.AppliedGeneration); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
