package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"sync"
	"testing"
)

func TestRuleMutationsEnqueueCompleteVersionedSnapshots(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "delivery@example.com")
	r, err := s.CreateRule(ctx, who, "delivery-create", ruleInput("both"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	var config NodeConfig
	if err = json.Unmarshal(job.Payload, &config); err != nil {
		t.Fatal(err)
	}
	if config.NodeID != "hk-01" || len(config.Rules) != 1 || config.Rules[0].ID != r.ID || config.Rules[0].TenantID != who.TenantID {
		t.Fatalf("snapshot: %+v", config)
	}
	hash := sha256.Sum256(job.Payload)
	if job.Checksum != hex.EncodeToString(hash[:]) {
		t.Fatal("checksum mismatch")
	}
	if err = s.ReportConfig(ctx, "hk-01", job.ID, ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "validated"}); err != nil {
		t.Fatal(err)
	}
	rules, _ := s.ListRules(ctx, who)
	if rules[0].AppliedVersion != 0 || rules[0].Status != "pending-agent" {
		t.Fatal("validation claimed application")
	}
	if _, err = s.PauseRule(ctx, who, r.ID); err != nil {
		t.Fatal(err)
	}
	next, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if next == nil || next.Generation <= job.Generation {
		t.Fatal("no next version")
	}
	json.Unmarshal(next.Payload, &config)
	if config.Rules[0].Status != "paused" {
		t.Fatal("pause not in desired config")
	}
	if err = s.DeleteRule(ctx, who, r.ID); err != nil {
		t.Fatal(err)
	}
	last, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if last == nil {
		t.Fatal("delete not enqueued")
	}
	json.Unmarshal(last.Payload, &config)
	if len(config.Rules) != 0 {
		t.Fatal("deleted rule remained in full desired snapshot")
	}
}

func TestConfigLeaseExpiryReplayAndStaleReport(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "lease@example.com")
	r, _ := s.CreateRule(ctx, who, "lease-create", ruleInput("tcp"))
	first, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if first == nil {
		t.Fatal("missing job")
	}
	duplicate, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if duplicate != nil {
		t.Fatal("same generation concurrently leased")
	}
	report := ConfigReport{LeaseToken: first.LeaseToken, Checksum: first.Checksum, Outcome: "validated"}
	if err := s.ReportConfig(ctx, "other-node", first.ID, report); err != ErrNotFound {
		t.Fatalf("node escape: %v", err)
	}
	s.pool.Exec(ctx, "UPDATE config_outbox SET lease_until=now()-interval '1 second' WHERE id=$1", first.ID)
	second, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if second == nil || second.LeaseToken == first.LeaseToken {
		t.Fatal("lease not reclaimed")
	}
	if err := s.ReportConfig(ctx, "hk-01", first.ID, report); err != ErrConflict {
		t.Fatalf("old token: %v", err)
	}
	report.LeaseToken = second.LeaseToken
	if err := s.ReportConfig(ctx, "hk-01", second.ID, report); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportConfig(ctx, "hk-01", second.ID, report); err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	report.Outcome = "failed"
	report.ErrorCode = "invalid-config"
	if err := s.ReportConfig(ctx, "hk-01", second.ID, report); err != ErrConflict {
		t.Fatalf("changed replay: %v", err)
	}
	s.PauseRule(ctx, who, r.ID)
	third, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	s.ResumeRule(ctx, who, r.ID)
	if err := s.ReportConfig(ctx, "hk-01", third.ID, ConfigReport{LeaseToken: third.LeaseToken, Checksum: third.Checksum, Outcome: "validated"}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.ListNodes(ctx)
	if nodes[0].ValidatedGeneration != second.Generation {
		t.Fatal("stale report advanced latest validation")
	}
}

func TestNodeMaintenanceAndFailedValidationRetry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "maintenance@example.com")
	if err := s.MaintainNode(ctx, "hk-01", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule(ctx, who, "maintenance-rule", ruleInput("udp")); err != ErrConflict {
		t.Fatalf("maintenance accepted create: %v", err)
	}
	if err := s.MaintainNode(ctx, "hk-01", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule(ctx, who, "maintenance-rule", ruleInput("udp")); err != nil {
		t.Fatal(err)
	}
	job, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if err := s.ReportConfig(ctx, "hk-01", job.ID, ConfigReport{LeaseToken: job.LeaseToken, Checksum: job.Checksum, Outcome: "failed", ErrorCode: "invalid-config"}); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.ClaimConfig(ctx, "hk-01", "validator-1"); next != nil {
		t.Fatal("failed job retried before backoff")
	}
	s.pool.Exec(ctx, "UPDATE config_outbox SET next_attempt_at=now()-interval '1 second' WHERE id=$1", job.ID)
	retry, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if retry == nil || retry.Attempts != 2 {
		t.Fatal("failure was not retried")
	}
	if err := s.ReportConfig(ctx, "hk-01", job.ID, ConfigReport{LeaseToken: retry.LeaseToken, Checksum: retry.Checksum, Outcome: "applied"}); err != ErrConflict {
		t.Fatalf("validation agent claimed applied: %v", err)
	}
}

func TestConcurrentNodeSnapshotsAndOutboxRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	who, _ := register(t, s, "outbox@example.com")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateRule(ctx, who, fmt.Sprintf("outbox-%d", i), ruleInput("udp"))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	job, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	var config NodeConfig
	json.Unmarshal(job.Payload, &config)
	if len(config.Rules) != 8 {
		t.Fatal("snapshot lost concurrent rules")
	}
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_config() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END; $$; CREATE TRIGGER reject_config BEFORE INSERT ON config_outbox FOR EACH ROW EXECUTE FUNCTION reject_config()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRule(ctx, who, "outbox-reject", ruleInput("both")); err == nil {
		t.Fatal("rule committed without outbox")
	}
	rules, _ := s.ListRules(ctx, who)
	if len(rules) != 8 {
		t.Fatal("failed outbox leaked rule")
	}
	var endpoints int
	s.pool.QueryRow(ctx, "SELECT count(*) FROM endpoint_allocations").Scan(&endpoints)
	if endpoints != 8 {
		t.Fatal("failed outbox leaked ports")
	}
}

func TestCertificateRotationAndOperatorRetryReconcileLatestSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, _ := s.ClaimConfig(ctx, "hk-01", "validator-1")
	if first == nil {
		t.Fatal("migration did not backfill config")
	}
	if err := s.ReportConfig(ctx, "hk-01", first.ID, ConfigReport{LeaseToken: first.LeaseToken, Checksum: first.Checksum, Outcome: "validated"}); err != nil {
		t.Fatal(err)
	}
	fp := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := s.EnrollNode(ctx, "hk-01", fp); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ClaimConfig(ctx, "hk-01", "validator-1", fp)
	if err != nil || fresh == nil {
		t.Fatalf("new agent did not receive latest desired state: %v", err)
	}
	s.pool.Exec(ctx, "UPDATE config_outbox SET attempts=5,lease_until=now()-interval '1 second' WHERE id=$1", fresh.ID)
	if job, _ := s.ClaimConfig(ctx, "hk-01", "validator-1", fp); job != nil {
		t.Fatal("exceeded retry limit")
	}
	nodes, _ := s.ListNodes(ctx)
	if nodes[0].DeliveryState != "failed" {
		t.Fatal("exhausted lease still shows as processing")
	}
	if err = s.RetryNodeConfig(ctx, "hk-01"); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ClaimConfig(ctx, "hk-01", "validator-1", fp)
	if err != nil || retry == nil || retry.Attempts != 1 {
		t.Fatalf("operator retry: %v", err)
	}
	if _, err = s.ClaimConfig(ctx, "hk-01", "validator-1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != ErrUnauthorized {
		t.Fatalf("certificate bypass: %v", err)
	}
}

func TestVersionOneUpgradePreservesRulesAndBackfillsDelivery(t *testing.T) {
	base := testStore(t)
	ctx := context.Background()
	schema := "v1_" + strings.ReplaceAll(newID(), "-", "")
	if _, err := base.pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config := base.pool.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { pool.Close(); base.pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	if _, err = pool.Exec(ctx, schemaSQL+";CREATE TABLE schema_migrations(version integer PRIMARY KEY);INSERT INTO schema_migrations VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	old := &Store{pool: pool}
	who, token := register(t, old, "upgrade@example.com")
	id := newID()
	if _, err = pool.Exec(ctx, `INSERT INTO relay_rules(id,tenant_id,node_id,name,protocol,target_host,target_port,bandwidth_mbps,listen_port,status) VALUES($1,$2,'hk-01','Existing relay','udp','192.0.2.10',443,1000,20000,'pending-agent')`, id, who.TenantID); err != nil {
		t.Fatal(err)
	}
	if err = old.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := old.Session(ctx, token)
	if err != nil || loaded.TenantID != who.TenantID {
		t.Fatal("upgrade lost account/session")
	}
	job, err := old.ClaimConfig(ctx, "hk-01", "validator-1")
	if err != nil || job == nil {
		t.Fatalf("backfill missing: %v", err)
	}
	var desired NodeConfig
	json.Unmarshal(job.Payload, &desired)
	if len(desired.Rules) != 1 || desired.Rules[0].ID != id {
		t.Fatal("upgrade lost existing rule")
	}
	var version int
	pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version)
	if version != SchemaVersion {
		t.Fatal("migration version not recorded")
	}
}
