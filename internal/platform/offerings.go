package platform

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type OfferingState string

const (
	OfferingStateDraft     OfferingState = "draft"
	OfferingStatePublished OfferingState = "published"
	OfferingStatePaused    OfferingState = "paused"
	OfferingStateSoldOut   OfferingState = "sold-out"
	OfferingStateRetired   OfferingState = "retired"
)

type LineOfferingInput struct {
	Code              string `json:"code"`
	Name              string `json:"name"`
	NodeID            string `json:"nodeId"`
	SourceMarket      string `json:"sourceMarket"`
	DestinationMarket string `json:"destinationMarket"`
	Notes             string `json:"notes"`
}

type OfferingStateInput struct {
	ExpectedRevision int64         `json:"expectedRevision"`
	State            OfferingState `json:"state"`
}

type LineOffering struct {
	ID string `json:"id"`
	LineOfferingInput
	ResourceMode ResourceMode  `json:"resourceMode"`
	State        OfferingState `json:"state"`
	Revision     int64         `json:"revision"`
	CreatedAt    time.Time     `json:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt"`
	Deletable    bool          `json:"deletable"`
}

const offeringColumns = "o.id,o.code,o.name,o.node_id,o.source_market,o.destination_market,o.notes,n.resource_mode,o.state,o.revision,o.created_at,o.updated_at,(o.state='draft' AND NOT EXISTS (SELECT 1 FROM plan_price_versions v WHERE v.offering_id=o.id))"

func nodeAllowsSales(status string) bool {
	return status == "unprovisioned" || status == "ready"
}

func optionalOfferingText(value string, max int) bool {
	return len(value) == 0 || billingText(value, max)
}

func normalizeOfferingInput(input LineOfferingInput) (LineOfferingInput, error) {
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.NodeID = strings.TrimSpace(input.NodeID)
	input.SourceMarket = strings.TrimSpace(input.SourceMarket)
	input.DestinationMarket = strings.TrimSpace(input.DestinationMarket)
	input.Notes = strings.TrimSpace(input.Notes)
	if !offeringCode.MatchString(input.Code) || !nodeIDPattern.MatchString(input.NodeID) || !billingText(input.Name, 120) || !optionalOfferingText(input.SourceMarket, 80) || !optionalOfferingText(input.DestinationMarket, 80) || !optionalOfferingText(input.Notes, 500) {
		return input, ErrInvalid
	}
	return input, nil
}

func normalizeOfferingState(state OfferingState) (OfferingState, error) {
	switch state {
	case OfferingStateDraft, OfferingStatePublished, OfferingStatePaused, OfferingStateSoldOut, OfferingStateRetired:
		return state, nil
	default:
		return "", ErrInvalid
	}
}

func scanOffering(row pgx.Row) (LineOffering, error) {
	var offering LineOffering
	err := row.Scan(&offering.ID, &offering.Code, &offering.Name, &offering.NodeID, &offering.SourceMarket, &offering.DestinationMarket, &offering.Notes, &offering.ResourceMode, &offering.State, &offering.Revision, &offering.CreatedAt, &offering.UpdatedAt, &offering.Deletable)
	if errors.Is(err, pgx.ErrNoRows) {
		return offering, ErrNotFound
	}
	return offering, err
}

func (s *Store) AdminCreateOffering(ctx context.Context, who Identity, input LineOfferingInput) (LineOffering, error) {
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return LineOffering{}, err
	}
	defer tx.Rollback(ctx)
	input, err = normalizeOfferingInput(input)
	if err != nil {
		return LineOffering{}, err
	}
	existing, err := scanOffering(tx.QueryRow(ctx, "SELECT "+offeringColumns+" FROM line_offerings o JOIN nodes n ON n.id=o.node_id WHERE o.code=$1", input.Code))
	if err == nil {
		if existing.LineOfferingInput != input {
			return LineOffering{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LineOffering{}, err
	}
	var node string
	err = tx.QueryRow(ctx, "SELECT id FROM nodes WHERE id=$1 FOR KEY SHARE", input.NodeID).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		return LineOffering{}, ErrNotFound
	}
	if err != nil {
		return LineOffering{}, err
	}
	id := newID()
	offering, err := scanOffering(tx.QueryRow(ctx, "INSERT INTO line_offerings(id,code,name,node_id,source_market,destination_market,notes) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id,code,name,node_id,source_market,destination_market,notes,(SELECT resource_mode FROM nodes WHERE id=$4),state,revision,created_at,updated_at,true", id, input.Code, input.Name, input.NodeID, input.SourceMarket, input.DestinationMarket, input.Notes))
	if err != nil {
		return LineOffering{}, nodeError(err)
	}
	if err = platformLog(ctx, tx, &who, "offering.create", offering.ID, map[string]any{"code": offering.Code, "nodeId": offering.NodeID, "revision": offering.Revision}); err != nil {
		return LineOffering{}, err
	}
	return offering, tx.Commit(ctx)
}

func (s *Store) AdminSetOfferingState(ctx context.Context, who Identity, id string, input OfferingStateInput) (LineOffering, error) {
	if !billingUUID.MatchString(id) || input.ExpectedRevision < 1 {
		return LineOffering{}, ErrInvalid
	}
	state, err := normalizeOfferingState(input.State)
	if err != nil {
		return LineOffering{}, err
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return LineOffering{}, err
	}
	defer tx.Rollback(ctx)
	var nodeStatus string
	offering, err := scanOffering(tx.QueryRow(ctx, "SELECT "+offeringColumns+" FROM line_offerings o JOIN nodes n ON n.id=o.node_id WHERE o.id=$1 FOR UPDATE OF o FOR SHARE OF n", id))
	if err == nil {
		// scanOffering deliberately has a fixed shape; read the status separately
		// under the same transaction after the offering row is locked.
		err = tx.QueryRow(ctx, "SELECT status FROM nodes WHERE id=$1 FOR SHARE", offering.NodeID).Scan(&nodeStatus)
	}
	if err != nil {
		return LineOffering{}, err
	}
	if offering.Revision != input.ExpectedRevision {
		return LineOffering{}, ErrConflict
	}
	if offering.State == state {
		return offering, nil
	}
	if offering.State == OfferingStateRetired || state == OfferingStateDraft {
		return LineOffering{}, ErrConflict
	}
	if state == OfferingStatePublished && !nodeAllowsSales(nodeStatus) {
		return LineOffering{}, ErrNodeNotAllocatable
	}
	previous := offering.State
	offering, err = scanOffering(tx.QueryRow(ctx, "UPDATE line_offerings SET state=$2,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 RETURNING id,code,name,node_id,source_market,destination_market,notes,(SELECT resource_mode FROM nodes WHERE nodes.id=line_offerings.node_id),state,revision,created_at,updated_at,false", id, state))
	if err != nil {
		return LineOffering{}, err
	}
	if err = platformLog(ctx, tx, &who, "offering.state", offering.ID, map[string]any{"from": previous, "to": offering.State, "revision": offering.Revision}); err != nil {
		return LineOffering{}, err
	}
	return offering, tx.Commit(ctx)
}

func (s *Store) AdminDeleteOffering(ctx context.Context, who Identity, id string, expectedRevision int64) error {
	if !billingUUID.MatchString(id) || expectedRevision < 1 {
		return ErrInvalid
	}
	tx, err := s.adminTx(ctx, who)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	offering, err := scanOffering(tx.QueryRow(ctx, "SELECT "+offeringColumns+" FROM line_offerings o JOIN nodes n ON n.id=o.node_id WHERE o.id=$1 FOR UPDATE OF o FOR SHARE OF n", id))
	if err != nil {
		return err
	}
	if offering.Revision != expectedRevision || offering.State != OfferingStateDraft {
		return ErrConflict
	}
	var referenced bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM plan_price_versions WHERE offering_id=$1)", id).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, "DELETE FROM line_offerings WHERE id=$1", id); err != nil {
		return nodeError(err)
	}
	if err = platformLog(ctx, tx, &who, "offering.delete", offering.ID, map[string]any{"code": offering.Code, "nodeId": offering.NodeID, "revision": offering.Revision}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AdminOfferings(ctx context.Context, who Identity, page int) (Page[LineOffering], error) {
	result := Page[LineOffering]{Items: []LineOffering{}, Page: page, PageSize: 50}
	if page < 1 || page > 100000 {
		return result, ErrInvalid
	}
	tx, err := s.adminReadTx(ctx, who)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM line_offerings").Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, "SELECT "+offeringColumns+" FROM line_offerings o JOIN nodes n ON n.id=o.node_id ORDER BY o.code LIMIT 50 OFFSET $1", (page-1)*50)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		offering, scanErr := scanOffering(rows)
		if scanErr != nil {
			return result, scanErr
		}
		result.Items = append(result.Items, offering)
	}
	return result, rows.Err()
}
