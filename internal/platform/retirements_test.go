package platform

import (
	"context"
	"strings"
	"testing"
)

func revertRetirementSchema(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version>=22;
DROP TABLE node_drain_attestations;
ALTER TABLE nodes DROP COLUMN agent_kernel_boot_id,DROP COLUMN agent_journal_sequence;
ALTER TABLE endpoint_allocations DROP COLUMN predecessor_retirement_id;
DROP TABLE endpoint_retirements;
DROP TRIGGER endpoint_allocation_identity_immutable ON endpoint_allocations;
DROP FUNCTION endpoint_allocation_identity_immutable();
ALTER TABLE endpoint_allocations DROP COLUMN allocation_id;
ALTER TABLE nodes DROP CONSTRAINT nodes_status_check,
 ADD CONSTRAINT nodes_status_check CHECK (status IN ('unprovisioned','ready','maintenance'))`)
	if err != nil {
		t.Fatal(err)
	}
}

func revertDrainSchemaTo22(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version=23;
DROP INDEX node_drain_attestations_unconsumed;
ALTER TABLE node_drain_attestations DROP COLUMN node_revision;
ALTER TABLE nodes DROP COLUMN agent_kernel_boot_id,DROP COLUMN agent_journal_sequence`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRetirementSchemaAddsImmutableAllocationIncarnations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "retirement-schema@example.com")
	rule, err := s.CreateRule(ctx, who, "retirement-schema-rule", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT allocation_id::text,protocol FROM endpoint_allocations WHERE rule_id=$1 ORDER BY protocol`, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id, protocol string
		if err = rows.Scan(&id, &protocol); err != nil {
			t.Fatal(err)
		}
		if !upgradeIDPattern.MatchString(id) || (protocol != "tcp" && protocol != "udp") || ids[id] {
			t.Fatalf("invalid allocation incarnation id=%q protocol=%q", id, protocol)
		}
		ids[id] = true
	}
	if err = rows.Err(); err != nil || len(ids) != 2 {
		t.Fatalf("allocation identities: %v err=%v", ids, err)
	}
	for id := range ids {
		if _, err = s.pool.Exec(ctx, "UPDATE endpoint_allocations SET allocation_id=$2 WHERE allocation_id=$1", id, newID()); err == nil {
			t.Fatal("database allowed allocation incarnation rewrite")
		}
		break
	}
}

func TestRetirementSchemaConstraintsAndHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "retirement-history@example.com")
	rule, err := s.CreateRule(ctx, who, "retirement-history-rule", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	var allocationID, address string
	var port int
	if err = s.pool.QueryRow(ctx, `SELECT allocation_id::text,host(listen_address),listen_port FROM endpoint_allocations WHERE rule_id=$1`, rule.ID).Scan(&allocationID, &address, &port); err != nil {
		t.Fatal(err)
	}
	secondRule, err := s.CreateRule(ctx, who, "retirement-history-second", ruleInput("tcp"))
	if err != nil {
		t.Fatal(err)
	}
	var secondAllocation string
	var secondPort int
	if err = s.pool.QueryRow(ctx, `SELECT allocation_id::text,listen_port FROM endpoint_allocations WHERE rule_id=$1`, secondRule.ID).Scan(&secondAllocation, &secondPort); err != nil {
		t.Fatal(err)
	}
	insert := func(id, incarnation, ruleID string, listenPort int, state string) error {
		_, e := s.pool.Exec(ctx, `INSERT INTO endpoint_retirements(id,allocation_id,node_id,rule_id,tenant_id,listen_address,protocol,listen_port,quarantine_generation,state,lease_token_hash,lease_expires_at) VALUES($1,$2,'hk-01',$3,$4,$5,'tcp',$6,1,$7,$8,now()+interval '2 minutes')`, id, incarnation, ruleID, who.TenantID, address, listenPort, state, strings.Repeat("e", 64))
		return e
	}
	first := newID()
	if err = insert(first, allocationID, rule.ID, port, "claimed"); err != nil {
		t.Fatal(err)
	}
	if err = insert(newID(), secondAllocation, secondRule.ID, secondPort, "purging"); err == nil {
		t.Fatal("database allowed two active purge executors on one node")
	}
	if _, err = s.pool.Exec(ctx, "UPDATE endpoint_retirements SET state='released',proof=$2::jsonb,proof_digest=$3,released_at=now() WHERE id=$1", first, `{"schemaVersion":1}`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "DELETE FROM endpoint_allocations WHERE allocation_id=$1", allocationID); err != nil {
		t.Fatal(err)
	}
	var retained string
	if err = s.pool.QueryRow(ctx, "SELECT allocation_id::text FROM endpoint_retirements WHERE id=$1", first).Scan(&retained); err != nil || retained != allocationID {
		t.Fatalf("released history lost: %q %v", retained, err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE endpoint_retirements SET state='unknown' WHERE id=$1", first); err == nil {
		t.Fatal("database accepted unknown retirement state")
	}
}

func TestRetirementSchemaAddsDrainPersistenceAndNodeStates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET status='draining' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET status='retired' WHERE id='hk-01'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE nodes SET status='destroyed' WHERE id='hk-01'"); err == nil {
		t.Fatal("database accepted unknown node lifecycle state")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO node_drain_attestations(id,node_id,generation,certificate_fingerprint,kernel_boot_id,agent_started_at,agent_version,journal_sequence,good_digest,quarantine_digest,observed_at,evidence) VALUES($1,'hk-01',1,$2,$3,now(),'v0.9.0',1,$4,$5,now(),'{}'::jsonb)`, newID(), strings.Repeat("b", 64), newID(), strings.Repeat("c", 64), strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var retirementTable, attestationTable bool
	if err = s.pool.QueryRow(ctx, "SELECT max(version),to_regclass('endpoint_retirements') IS NOT NULL,to_regclass('node_drain_attestations') IS NOT NULL FROM schema_migrations").Scan(&version, &retirementTable, &attestationTable); err != nil || version != SchemaVersion || !retirementTable || !attestationTable {
		t.Fatalf("schema version=%d retirement=%v attestation=%v err=%v", version, retirementTable, attestationTable, err)
	}
}

func TestMigration22BackfillsDeletedHeldAllocationsWithoutReleasingThem(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "retirement-upgrade@example.com")
	rule, err := s.CreateRule(ctx, who, "retirement-upgrade-rule", ruleInput("udp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, "UPDATE relay_rules SET ever_dispatched=true WHERE id=$1", rule.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteRule(ctx, who, rule.ID); err != nil {
		t.Fatal(err)
	}
	revertRetirementSchema(t, s)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var allocationID, state string
	var generation int64
	if err = s.pool.QueryRow(ctx, `SELECT allocation_id::text,state,quarantine_generation FROM endpoint_retirements WHERE rule_id=$1`, rule.ID).Scan(&allocationID, &state, &generation); err != nil {
		t.Fatal(err)
	}
	if !upgradeIDPattern.MatchString(allocationID) || state != "waiting-removal" || generation < 1 {
		t.Fatalf("unsafe historical backfill id=%q state=%q generation=%d", allocationID, state, generation)
	}
	var held int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations WHERE rule_id=$1", rule.ID).Scan(&held); err != nil || held != 1 {
		t.Fatalf("historical allocation released held=%d err=%v", held, err)
	}
}

func TestMigration22RollsBackOnVersionRecordFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	revertRetirementSchema(t, s)
	if _, err := s.pool.Exec(ctx, "ALTER TABLE schema_migrations ADD CONSTRAINT reject_v22 CHECK (version<>22)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("migration committed despite version record failure")
	}
	var version int
	var allocationColumn, retirementTable bool
	if err := s.pool.QueryRow(ctx, `SELECT max(version),EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='endpoint_allocations' AND column_name='allocation_id'),to_regclass('endpoint_retirements') IS NOT NULL FROM schema_migrations`).Scan(&version, &allocationColumn, &retirementTable); err != nil {
		t.Fatal(err)
	}
	if version != 21 || allocationColumn || retirementTable {
		t.Fatalf("failed migration leaked state version=%d allocation=%v retirement=%v", version, allocationColumn, retirementTable)
	}
}

func TestMigration23AddsMonotonicRuntimeAndConsumesOldAttestations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO node_drain_attestations(id,node_id,generation,certificate_fingerprint,kernel_boot_id,agent_started_at,agent_version,journal_sequence,good_digest,quarantine_digest,observed_at,evidence) VALUES($1,'hk-01',1,$2,$3,now(),'v0.9.0',1,$4,$5,now(),'{}')`, newID(), strings.Repeat("a", 64), newID(), strings.Repeat("b", 64), strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	revertDrainSchemaTo22(t, s)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version, consumed, runtimeColumns int
	if err := s.pool.QueryRow(ctx, `SELECT max(version),(SELECT count(*) FROM node_drain_attestations WHERE consumed_at IS NOT NULL),(SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND ((table_name='nodes' AND column_name IN ('agent_kernel_boot_id','agent_journal_sequence')) OR (table_name='node_drain_attestations' AND column_name='node_revision'))) FROM schema_migrations`).Scan(&version, &consumed, &runtimeColumns); err != nil {
		t.Fatal(err)
	}
	if version != 23 || consumed != 1 || runtimeColumns != 3 {
		t.Fatalf("migration23 incomplete version=%d consumed=%d columns=%d", version, consumed, runtimeColumns)
	}
}

func TestMigration23RollsBackOnVersionRecordFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	revertDrainSchemaTo22(t, s)
	if _, err := s.pool.Exec(ctx, "ALTER TABLE schema_migrations ADD CONSTRAINT reject_v23 CHECK (version<>23)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("migration committed despite version record failure")
	}
	var version, runtimeColumns int
	if err := s.pool.QueryRow(ctx, `SELECT max(version),(SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND ((table_name='nodes' AND column_name IN ('agent_kernel_boot_id','agent_journal_sequence')) OR (table_name='node_drain_attestations' AND column_name='node_revision'))) FROM schema_migrations`).Scan(&version, &runtimeColumns); err != nil {
		t.Fatal(err)
	}
	if version != 22 || runtimeColumns != 0 {
		t.Fatalf("failed migration leaked state version=%d columns=%d", version, runtimeColumns)
	}
}
