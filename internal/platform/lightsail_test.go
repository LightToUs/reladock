package platform

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMigration40IsCurrentSchemaVersion(t *testing.T) {
	if SchemaVersion != 40 {
		t.Fatalf("SchemaVersion = %d, want 40", SchemaVersion)
	}
}

func validLightsailCreate(name, script string) LightsailCreateInput {
	return LightsailCreateInput{
		Name: name,
		Selection: LightsailCatalogSelection{
			Region: "us-east-1", AvailabilityZone: "us-east-1a", BlueprintID: "debian_12", BundleID: "small_3_0",
		},
		Script: script,
	}
}

func TestLightsailAdminCreateIsAuthorizedIdempotentAndSecretSafe(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, store)
	customer, _ := register(t, store, "lightsail-customer@example.com")
	input := validLightsailCreate("Deploy node", "#!/bin/bash\necho deploy\n")

	if _, err := store.AdminCreateLightsailInstance(ctx, customer, "customer-create", input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant create error = %v", err)
	}
	created, err := store.AdminCreateLightsailInstance(ctx, admin, "create-1", input)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !regexp.MustCompile(`^reladock-ls-[a-f0-9]{12}$`).MatchString(created.AWSInstanceName) || created.StaticIPName != "reladock-ip-"+strings.TrimPrefix(created.AWSInstanceName, "reladock-ls-") || created.ScriptSHA256 == "" || created.ScriptSize != len(input.Script) || created.DeploymentState != "pending" {
		t.Fatalf("created instance: %+v", created)
	}
	replayed, err := store.AdminCreateLightsailInstance(ctx, admin, "create-1", input)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("idempotent replay: %+v %v", replayed, err)
	}
	changed := input
	changed.Script = "echo changed\n"
	if _, err = store.AdminCreateLightsailInstance(ctx, admin, "create-1", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotency payload error = %v", err)
	}
	instances, err := store.AdminLightsailInstances(ctx, admin)
	if err != nil || len(instances) != 1 || instances[0].ID != created.ID {
		t.Fatalf("list: %+v %v", instances, err)
	}
	if _, err = store.AdminLightsailInstances(ctx, customer); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant list error = %v", err)
	}
	var leaked bool
	if err = store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_audit_logs WHERE details::text LIKE '%echo deploy%' OR details::text LIKE '%#!/bin/bash%')`).Scan(&leaked); err != nil || leaked {
		t.Fatalf("script leaked into audit: leaked=%t err=%v", leaked, err)
	}
}

func TestLightsailAdminCreateValidatesScriptAndRollsBackOnAuditFailure(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, store)
	for name, script := range map[string]string{
		"oversized":    strings.Repeat("x", 65537),
		"invalid utf8": string([]byte{0xff, 0xfe}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.AdminCreateLightsailInstance(ctx, admin, "invalid-"+name, validLightsailCreate("Invalid", script)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid script error = %v", err)
			}
		})
	}
	if _, err := store.pool.Exec(ctx, `CREATE FUNCTION reject_lightsail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='lightsail.create-requested' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_lightsail_audit BEFORE INSERT ON platform_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_lightsail_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdminCreateLightsailInstance(ctx, admin, "audit-failure", validLightsailCreate("Rollback", "echo private\n")); err == nil {
		t.Fatal("create succeeded while audit insert failed")
	}
	var count int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM lightsail_instances WHERE idempotency_key='audit-failure'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("audit failure left instance count=%d err=%v", count, err)
	}
}

func TestLightsailAdminCreateAllowsEmptyScriptWithoutExecutionPayload(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, store)
	created, err := store.AdminCreateLightsailInstance(ctx, admin, "create-without-script", validLightsailCreate("Create only", ""))
	if err != nil {
		t.Fatalf("create without script: %v", err)
	}
	if created.ScriptSize != 0 || created.ScriptSHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || created.DeploymentState != "not-requested" || created.ExecutionStartedAt != nil {
		t.Fatalf("empty script state: %+v", created)
	}
	var body *string
	if err = store.pool.QueryRow(ctx, "SELECT script_body FROM lightsail_instances WHERE id=$1", created.ID).Scan(&body); err != nil || body != nil {
		t.Fatalf("empty script persisted as executable payload: body=%v err=%v", body, err)
	}
	job, err := store.ClaimLightsailJob(ctx, time.Minute)
	if err != nil || job == nil || job.InstanceID != created.ID {
		t.Fatalf("claim empty-script create: job=%+v err=%v", job, err)
	}
	if err = store.CompleteLightsailJob(ctx, job.ID, job.LeaseToken, "succeeded", ""); err != nil {
		t.Fatalf("complete empty-script create: %v", err)
	}
	var lifecycle, deployment string
	var revision int64
	if err = store.pool.QueryRow(ctx, "SELECT lifecycle_state,deployment_state,revision FROM lightsail_instances WHERE id=$1", created.ID).Scan(&lifecycle, &deployment, &revision); err != nil || lifecycle != "running" || deployment != "not-requested" || revision != created.Revision+1 {
		t.Fatalf("completed empty-script state lifecycle=%q deployment=%q revision=%d err=%v", lifecycle, deployment, revision, err)
	}
}

func TestLightsailAdminStopAndDestroyRequireRevisionStateAndOwnership(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, store)
	created, err := store.AdminCreateLightsailInstance(ctx, admin, "lifecycle-1", validLightsailCreate("Lifecycle node", "echo once\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_jobs SET state='succeeded',completed_at=clock_timestamp() WHERE instance_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET aws_arn='arn:aws:lightsail:us-east-1:123:Instance/test',aws_state='running',lifecycle_state='running',static_ip_address='203.0.113.8' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdminRequestLightsailStop(ctx, admin, created.ID, created.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale stop error = %v", err)
	}
	stop, err := store.AdminRequestLightsailStop(ctx, admin, created.ID, created.Revision)
	if err != nil || stop.Kind != "stop" {
		t.Fatalf("stop: %+v %v", stop, err)
	}
	replayed, err := store.AdminRequestLightsailStop(ctx, admin, created.ID, created.Revision)
	if err != nil || replayed.ID != stop.ID {
		t.Fatalf("stop replay: %+v %v", replayed, err)
	}
	if _, err = store.AdminRequestLightsailDestroy(ctx, admin, created.ID, created.Revision+1, created.AWSInstanceName); !errors.Is(err, ErrConflict) {
		t.Fatalf("destroy while running/active stop error = %v", err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_jobs SET state='succeeded',lease_token=NULL,lease_expires_at=NULL,completed_at=clock_timestamp() WHERE id=$1`, stop.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET aws_state='stopped',lifecycle_state='stopped' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := store.AdminLightsailInstances(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	current := instances[0]
	if _, err = store.AdminRequestLightsailDestroy(ctx, admin, current.ID, current.Revision, "wrong-name"); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong confirmation error = %v", err)
	}
	destroy, err := store.AdminRequestLightsailDestroy(ctx, admin, current.ID, current.Revision, current.AWSInstanceName)
	if err != nil || destroy.Kind != "destroy" {
		t.Fatalf("destroy: %+v %v", destroy, err)
	}

	missing, err := store.AdminCreateLightsailInstance(ctx, admin, "lifecycle-missing", validLightsailCreate("Missing evidence", "echo once\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_jobs SET state='succeeded',completed_at=clock_timestamp() WHERE instance_id=$1`, missing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET aws_state='stopped',lifecycle_state='stopped' WHERE id=$1`, missing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdminRequestLightsailDestroy(ctx, admin, missing.ID, missing.Revision, missing.AWSInstanceName); !errors.Is(err, ErrConflict) {
		t.Fatalf("destroy without ownership evidence error = %v", err)
	}
}

func TestLightsailExecutionBoundaryIsAtMostOnceAndLeaseFenced(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin, _ := adminIdentity(t, store)
	created, err := store.AdminCreateLightsailInstance(ctx, admin, "execution-1", validLightsailCreate("Execute once", "echo one-time\n"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimLightsailJob(ctx, time.Minute)
	if err != nil || job == nil || job.InstanceID != created.ID || job.LeaseToken == "" {
		t.Fatalf("claim: %+v %v", job, err)
	}
	if err = store.AdvanceLightsailJob(ctx, job.ID, "00000000-0000-4000-8000-000000000001", "reconcile-instance", "execute-script", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong lease advanced job: %v", err)
	}
	if err = store.AdvanceLightsailJob(ctx, job.ID, job.LeaseToken, "reconcile-instance", "execute-script", ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	execution, err := store.BeginLightsailExecution(ctx, job.ID, job.LeaseToken)
	if err != nil || execution.Script != "echo one-time\n" || execution.Instance.ID != created.ID {
		t.Fatalf("begin: %+v %v", execution, err)
	}
	var body *string
	var started *time.Time
	if err = store.pool.QueryRow(ctx, "SELECT script_body,execution_started_at FROM lightsail_instances WHERE id=$1", created.ID).Scan(&body, &started); err != nil || body != nil || started == nil {
		t.Fatalf("execution boundary body=%v started=%v err=%v", body, started, err)
	}
	if _, err = store.pool.Exec(ctx, "UPDATE lightsail_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimLightsailJob(ctx, time.Minute)
	if err != nil || reclaimed != nil {
		t.Fatalf("crossed boundary was reclaimed: %+v %v", reclaimed, err)
	}
	var jobState, deploymentState string
	if err = store.pool.QueryRow(ctx, `SELECT j.state,i.deployment_state FROM lightsail_jobs j JOIN lightsail_instances i ON i.id=j.instance_id WHERE j.id=$1`, job.ID).Scan(&jobState, &deploymentState); err != nil || jobState != "unknown" || deploymentState != "unknown" {
		t.Fatalf("crash recovery job=%q deployment=%q err=%v", jobState, deploymentState, err)
	}
}

func TestMigration40EnforcesLightsailLifecycleConstraints(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	who, _ := register(t, store, "lightsail-schema@example.com")
	instanceID := newID()
	requestHash := strings.Repeat("a", 64)
	scriptHash := strings.Repeat("b", 64)
	_, err := store.pool.Exec(ctx, `INSERT INTO lightsail_instances(
 id,created_by,idempotency_key,request_hash,name,region,availability_zone,blueprint_id,bundle_id,
 aws_instance_name,static_ip_name,lifecycle_state,deployment_state,script_sha256,script_size,script_body
) VALUES($1,$2,'schema-key',$3,'Schema node','us-east-1','us-east-1a','debian','small',$4,$5,'requested','pending',$6,7,'echo ok')`,
		instanceID, who.MemberID, requestHash, "reladock-ls-"+strings.ReplaceAll(instanceID, "-", "")[:12], "reladock-ip-"+strings.ReplaceAll(instanceID, "-", "")[:12], scriptHash)
	if err != nil {
		t.Fatalf("valid Lightsail row rejected: %v", err)
	}
	jobID := newID()
	if _, err = store.pool.Exec(ctx, `INSERT INTO lightsail_jobs(id,instance_id,kind,state,stage) VALUES($1,$2,'create','queued','reconcile-instance')`, jobID, instanceID); err != nil {
		t.Fatalf("valid job rejected: %v", err)
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO lightsail_jobs(id,instance_id,kind,state,stage) VALUES($1,$2,'stop','queued','reconcile-stop')`, newID(), instanceID); err == nil {
		t.Fatal("second active job accepted")
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET region='eu-west-1' WHERE id=$1`, instanceID); err == nil {
		t.Fatal("immutable create identity changed")
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET script_body=NULL WHERE id=$1`, instanceID); err == nil {
		t.Fatal("script cleared without execution boundary")
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET execution_started_at=clock_timestamp(),script_body=NULL,deployment_state='running' WHERE id=$1`, instanceID); err != nil {
		t.Fatalf("execution boundary rejected: %v", err)
	}
	if _, err = store.pool.Exec(ctx, `UPDATE lightsail_instances SET script_body='echo replay' WHERE id=$1`, instanceID); err == nil {
		t.Fatal("script reintroduced after execution boundary")
	}
	if _, err = store.pool.Exec(ctx, `INSERT INTO lightsail_instances(
 id,created_by,idempotency_key,request_hash,name,region,availability_zone,blueprint_id,bundle_id,
 aws_instance_name,static_ip_name,lifecycle_state,deployment_state,script_sha256,script_size,script_body
) SELECT $1,created_by,'schema-key-2',$2,'Duplicate','us-east-1','us-east-1a','debian','small',aws_instance_name,'another-ip','requested','pending',$3,7,'echo ok'
 FROM lightsail_instances WHERE id=$4`, newID(), strings.Repeat("c", 64), strings.Repeat("d", 64), instanceID); err == nil {
		t.Fatal("duplicate AWS instance name accepted")
	}
}
